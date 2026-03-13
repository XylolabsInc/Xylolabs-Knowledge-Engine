package bot

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
	"github.com/xylolabsinc/xylolabs-kb/internal/kbrepo"
	"github.com/xylolabsinc/xylolabs-kb/internal/tools"
)

const (
	maxReplyLength    = 3000
	maxFileDownload   = 10 * 1024 * 1024 // 10 MB
	maxThreadHistory  = 20               // max prior messages to include as context
	maxToolIterations = 5                // max function calling round-trips

	maxTrackedThreads     = 1000
	maxNotBotThreads      = 500
	threadCleanupInterval = 10 * time.Minute
	notBotCacheTTL        = 5 * time.Minute

	// Token budget estimates (chars-based, ~3 chars per token)
	maxContextChars      = 300000 // ~100k tokens for total context
	maxKBContextChars    = 200000 // ~67k tokens for KB context
	maxFileChars         = 24000  // ~8k tokens per attached file
	maxSystemPromptChars = 50000
)

//go:embed system_prompt.txt
var systemPromptTemplate string

// Bot handles Slack mentions and DMs with Gemini-powered responses.
type Bot struct {
	slackClient  *slack.Client
	gemini       *gemini.Client
	kbReader     *kbrepo.Reader
	botUserID    string
	botToken     string
	proModel     string
	systemPrompt string
	location     *time.Location
	logger       *slog.Logger
	httpClient   *http.Client
	toolExecutor *tools.ToolExecutor
	extractor    *extractor.Extractor

	// Track threads where the bot has responded, so follow-up replies are handled.
	// Key: "channel:thread_ts"
	trackedThreads   map[string]bool
	trackedThreadsMu sync.RWMutex

	// Negative cache for threads confirmed not involving the bot.
	// Key: "channel:thread_ts", Value: when checked.
	notBotThreads   map[string]time.Time
	notBotThreadsMu sync.RWMutex

	done chan struct{}
}

// New creates a Bot handler.
func New(slackClient *slack.Client, geminiClient *gemini.Client, kbReader *kbrepo.Reader, botUserID, botToken, proModel, systemPromptFile string, location *time.Location, logger *slog.Logger) *Bot {
	return &Bot{
		slackClient:    slackClient,
		gemini:         geminiClient,
		kbReader:       kbReader,
		botUserID:      botUserID,
		botToken:       botToken,
		proModel:       proModel,
		systemPrompt:   loadSystemPrompt(systemPromptFile),
		location:       location,
		logger:         logger.With("component", "bot"),
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		trackedThreads: make(map[string]bool),
		notBotThreads:  make(map[string]time.Time),
		done:           make(chan struct{}),
	}
}

// loadSystemPrompt returns the system prompt template from filePath if provided,
// falling back to the embedded default if the path is empty or the file cannot be read.
func loadSystemPrompt(filePath string) string {
	if filePath == "" {
		return systemPromptTemplate
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		slog.Warn("failed to load custom system prompt, using default", "path", filePath, "error", err)
		return systemPromptTemplate
	}
	slog.Info("loaded custom system prompt", "path", filePath)
	return string(data)
}

// SetToolExecutor sets the tool executor for function calling support.
func (b *Bot) SetToolExecutor(executor *tools.ToolExecutor) {
	b.toolExecutor = executor
}

// SetExtractor sets the content extractor for URL fetching.
func (b *Bot) SetExtractor(ext *extractor.Extractor) {
	b.extractor = ext
}

// StartCleanup begins periodic cleanup of thread caches.
func (b *Bot) StartCleanup() {
	go func() {
		ticker := time.NewTicker(threadCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-b.done:
				return
			case <-ticker.C:
				b.cleanupCaches()
			}
		}
	}()
}

// StopCleanup stops the periodic cleanup goroutine.
func (b *Bot) StopCleanup() {
	close(b.done)
}

func (b *Bot) cleanupCaches() {
	// Evict oldest tracked threads if over limit
	b.trackedThreadsMu.Lock()
	if len(b.trackedThreads) > maxTrackedThreads {
		// Simple eviction: clear half the map when it exceeds limit
		count := 0
		for k := range b.trackedThreads {
			delete(b.trackedThreads, k)
			count++
			if count >= len(b.trackedThreads)/2 {
				break
			}
		}
	}
	b.trackedThreadsMu.Unlock()

	// Evict expired entries from negative cache
	now := time.Now()
	b.notBotThreadsMu.Lock()
	for k, checkedAt := range b.notBotThreads {
		if now.Sub(checkedAt) > notBotCacheTTL {
			delete(b.notBotThreads, k)
		}
	}
	// Hard cap
	if len(b.notBotThreads) > maxNotBotThreads {
		count := 0
		for k := range b.notBotThreads {
			delete(b.notBotThreads, k)
			count++
			if count >= len(b.notBotThreads)/2 {
				break
			}
		}
	}
	b.notBotThreadsMu.Unlock()
}

// IsTrackedThread returns true if the bot has previously replied in this thread.
// Checks in-memory cache first, then falls back to Slack API with negative caching.
func (b *Bot) IsTrackedThread(channel, threadTS string) bool {
	key := channel + ":" + threadTS

	// Check positive cache.
	b.trackedThreadsMu.RLock()
	cached := b.trackedThreads[key]
	b.trackedThreadsMu.RUnlock()
	if cached {
		return true
	}

	// Check negative cache (skip API call if recently checked).
	b.notBotThreadsMu.RLock()
	if checkedAt, ok := b.notBotThreads[key]; ok && time.Since(checkedAt) < 5*time.Minute {
		b.notBotThreadsMu.RUnlock()
		return false
	}
	b.notBotThreadsMu.RUnlock()

	// Check Slack API for bot messages in the thread.
	msgs, _, _, err := b.slackClient.GetConversationRepliesContext(
		context.Background(),
		&slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadTS,
			Limit:     50,
		},
	)
	if err != nil {
		b.logger.Debug("failed to check thread for bot replies", "error", err)
		return false
	}

	for _, msg := range msgs {
		if msg.User == b.botUserID {
			b.trackThread(channel, threadTS)
			return true
		}
	}

	// Cache negative result.
	b.notBotThreadsMu.Lock()
	b.notBotThreads[key] = time.Now()
	b.notBotThreadsMu.Unlock()
	return false
}

func (b *Bot) trackThread(channel, threadTS string) {
	b.trackedThreadsMu.Lock()
	defer b.trackedThreadsMu.Unlock()
	b.trackedThreads[channel+":"+threadTS] = true
}

// HandleMention processes a message where the bot was @mentioned.
func (b *Bot) HandleMention(ctx context.Context, ev *slackevents.MessageEvent) {
	// Strip bot mention from text.
	query := strings.ReplaceAll(ev.Text, "<@"+b.botUserID+">", "")
	query = strings.TrimSpace(query)
	if query == "" {
		query = "Hello! How can I help you?"
	}

	b.respond(ctx, ev, query)
}

// HandleDirectMessage processes a DM to the bot.
func (b *Bot) HandleDirectMessage(ctx context.Context, ev *slackevents.MessageEvent) {
	query := strings.TrimSpace(ev.Text)
	if query == "" {
		return
	}
	b.respond(ctx, ev, query)
}

// respond loads KB context, builds Gemini prompt, and posts the reply to Slack.
func (b *Bot) respond(ctx context.Context, ev *slackevents.MessageEvent, query string) {
	threadTS := ev.ThreadTimeStamp
	if threadTS == "" {
		threadTS = ev.TimeStamp
	}

	// Resolve user mentions in the query text.
	query = b.resolveUserMentions(ctx, query)

	// 1. Load knowledge base context from the markdown repo.
	var kbContext string
	if b.kbReader != nil {
		var err error
		kbContext, err = b.kbReader.BuildContext(query)
		if err != nil {
			b.logger.Warn("failed to load kb context", "error", err)
			kbContext = ""
		}
	}

	b.logger.Info("kb context loaded", "length", len(kbContext), "query", query)

	// Truncate KB context if it exceeds budget
	if len(kbContext) > maxKBContextChars {
		kbContext = kbContext[:maxKBContextChars] + "\n\n[... KB context truncated due to size ...]"
		b.logger.Warn("kb context truncated", "original_length", len(kbContext), "max", maxKBContextChars)
	}

	// 2. Download any attached files from the message.
	var images []gemini.Image
	fileAttachments := make(map[string][]byte)
	var fileNames []string
	if ev.Message != nil {
		for _, f := range ev.Message.Files {
			if f.URLPrivateDownload == "" {
				continue
			}
			data, err := b.downloadSlackFile(ctx, f.URLPrivateDownload)
			if err != nil {
				b.logger.Warn("failed to download slack file",
					"file_id", f.ID,
					"error", err,
				)
				continue
			}
			mimeType := f.Mimetype
			if mimeType == "" {
				mimeType = "application/octet-stream"
			}
			fileName := f.Name
			if fileName == "" {
				fileName = f.ID
			}
			// Send supported MIME types as inline data to Gemini
			if strings.HasPrefix(mimeType, "image/") || mimeType == "application/pdf" {
				images = append(images, gemini.Image{
					MimeType: mimeType,
					Data:     data,
				})
			} else if b.extractor != nil {
				// Extract text from document files (HWP, DOCX, XLSX, etc.)
				result, err := b.extractor.ExtractFromBytes(ctx, data, mimeType, fileName)
				if err == nil && result.Text != "" && !strings.HasPrefix(result.Text, "[") {
					content := result.Text
					if len(content) > 8000 {
						content = content[:8000] + "\n..."
					}
					query += fmt.Sprintf("\n\n---\nFile: %s\n%s\n---", fileName, content)
				}
			}
			// Store for potential Drive upload
			fileAttachments[fileName] = data
			fileNames = append(fileNames, fileName)
		}
	}

	// Also fetch files from previous messages in the thread
	if ev.ThreadTimeStamp != "" {
		threadFiles := b.fetchThreadFiles(ctx, ev.Channel, threadTS, ev.TimeStamp)
		for name, data := range threadFiles {
			if _, exists := fileAttachments[name]; !exists {
				fileAttachments[name] = data
				fileNames = append(fileNames, name)
				// Extract text from thread files too
				if b.extractor != nil {
					result, err := b.extractor.ExtractFromBytes(ctx, data, "application/octet-stream", name)
					if err == nil && result.Text != "" && !strings.HasPrefix(result.Text, "[") {
						content := result.Text
						if len(content) > 8000 {
							content = content[:8000] + "\n..."
						}
						query += fmt.Sprintf("\n\n---\nFile: %s\n%s\n---", name, content)
					}
				}
			}
		}
	}

	// Add attached file names to query so the model knows they're available for upload
	if len(fileNames) > 0 {
		query += "\n\n[Attached files: " + strings.Join(fileNames, ", ") + "]"
	}

	// Store file attachments for tool executor (upload_to_drive)
	if b.toolExecutor != nil && len(fileAttachments) > 0 {
		b.toolExecutor.SetAttachments(fileAttachments)
		defer b.toolExecutor.ClearAttachments()
	}

	// 2.5. Extract and fetch URLs from message text for context.
	if b.extractor != nil {
		urls := extractURLs(query)
		for _, u := range urls {
			result, err := b.extractor.ExtractFromURL(ctx, u)
			if err != nil {
				b.logger.Warn("failed to fetch URL", "url", u, "error", err)
				continue
			}
			if result.Text != "" {
				// Truncate long pages
				content := result.Text
				if len(content) > 5000 {
					content = content[:5000] + "..."
				}
				query += fmt.Sprintf("\n\n---\nLink: %s\n%s\n---", u, content)
			}
		}
	}

	// 3. Build system prompt with full KB context.
	// Resolve current user's display name for context.
	userName := b.resolveUserName(ctx, ev.User)
	now := time.Now().In(b.location)
	currentTime := now.Format("2006-01-02 (Monday) 15:04 MST")
	systemPrompt := fmt.Sprintf(b.systemPrompt, userName) + "\n\nCurrent date and time: " + currentTime + "\n\n--- Reference Materials ---\n" + kbContext + "\n---"

	// Truncate system prompt if it exceeds budget
	if len(systemPrompt) > maxSystemPromptChars {
		systemPrompt = systemPrompt[:maxSystemPromptChars] + "\n[... truncated ...]"
		b.logger.Warn("system prompt truncated", "length", len(systemPrompt), "max", maxSystemPromptChars)
	}

	// 4. Build conversation messages with thread history for context continuity.
	var messages []gemini.Message
	if ev.ThreadTimeStamp != "" {
		messages = b.fetchThreadHistory(ctx, ev.Channel, threadTS, ev.TimeStamp)
	}

	// 2.6. Extract and fetch URLs from thread history messages (before adding current message).
	if b.extractor != nil {
		for _, msg := range messages {
			if msg.Role != "user" {
				continue
			}
			for _, u := range extractURLs(msg.Content) {
				result, err := b.extractor.ExtractFromURL(ctx, u)
				if err != nil {
					b.logger.Debug("failed to fetch thread URL", "url", u, "error", err)
					continue
				}
				if result.Text != "" {
					content := result.Text
					if len(content) > 5000 {
						content = content[:5000] + "..."
					}
					query += fmt.Sprintf("\n\n---\nLink: %s\n%s\n---", u, content)
				}
			}
		}
	}

	messages = append(messages, gemini.Message{
		Role:    "user",
		Content: "[" + userName + "] " + query,
		Images:  images,
	})
	messages = mergeConsecutiveRoles(messages)

	// Add available tools if executor is configured
	genReq := gemini.GenerateRequest{
		SystemPrompt:  systemPrompt,
		ThinkingLevel: "low",
		Messages:      messages,
		GoogleSearch:  true,
	}
	if b.toolExecutor != nil {
		genReq.Tools = b.toolExecutor.Declarations()
	}

	// Smart model + thinking routing
	if b.proModel != "" && isCreationTask(query) {
		genReq.Model = b.proModel
		genReq.ThinkingLevel = "high"
	} else if isComplexQuery(query) {
		genReq.ThinkingLevel = "high"
	}

	// Tool calling loop: call Gemini, execute any tools, re-call with results
	var genResp *gemini.GenerateResponse
	for i := 0; i <= maxToolIterations; i++ {
		var err error
		genResp, err = b.gemini.Generate(ctx, genReq)
		if err != nil {
			b.logger.Error("gemini generate failed", "error", err, "iteration", i)
			b.postReply(ctx, ev.Channel, threadTS, fmt.Sprintf("An error occurred: %v", err))
			return
		}

		// If no function calls, we have the final text response
		if len(genResp.FunctionCalls) == 0 {
			break
		}

		if b.toolExecutor == nil {
			b.logger.Warn("model returned function calls but no executor configured")
			break
		}

		b.logger.Info("executing tool calls", "count", len(genResp.FunctionCalls), "iteration", i)

		// Record the model's function call message
		genReq.Messages = append(genReq.Messages, gemini.Message{
			Role:          "model",
			FunctionCalls: genResp.FunctionCalls,
		})

		// Execute each function call and collect responses
		var responses []gemini.FunctionResponse
		for _, fc := range genResp.FunctionCalls {
			if fc.Name == "google_search" {
				// google_search is handled natively via GoogleSearch:true in the request.
				// If the model emits it as a function call, return a helpful message instead.
				responses = append(responses, gemini.FunctionResponse{
					Name: fc.Name,
					Response: map[string]any{
						"result": "Google Search is not available as a function call. URL content from messages is automatically fetched and provided in the conversation context.",
					},
				})
				continue
			}
			resp := b.toolExecutor.Execute(ctx, fc)
			responses = append(responses, resp)
		}

		// Add tool results as a user message for the next round
		genReq.Messages = append(genReq.Messages, gemini.Message{
			Role:              "user",
			FunctionResponses: responses,
		})
	}

	// 5.5. Upload screenshot attachments if any were produced by tools.
	if b.toolExecutor != nil {
		if screenshotData, ok := b.toolExecutor.PopScreenshot(); ok {
			go func(data []byte, channel, ts string) {
				uploadCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				_, err := b.slackClient.UploadFileContext(uploadCtx, slack.UploadFileParameters{
					Channel:         channel,
					Filename:        "screenshot.png",
					FileSize:        len(data),
					Reader:          bytes.NewReader(data),
					Title:           "Web Page Screenshot",
					ThreadTimestamp: ts,
				})
				if err != nil {
					if strings.Contains(err.Error(), "missing_scope") {
						b.logger.Error("screenshot upload failed: Slack app missing 'files:write' scope — add it at https://api.slack.com/apps", "error", err)
					} else {
						b.logger.Warn("failed to upload screenshot", "error", err, "size_bytes", len(data))
					}
				} else {
					b.logger.Info("screenshot uploaded to Slack", "channel", channel, "size_bytes", len(data))
				}
			}(screenshotData, ev.Channel, threadTS)
		}
	}

	// 5. Check if the model decided to skip this message.
	responseText := genResp.Text
	if strings.TrimSpace(responseText) == "===SKIP===" {
		b.logger.Debug("skipping trivial message", "query", query)
		return
	}
	// Strip any ===SKIP=== fragments embedded in a longer response.
	responseText = strings.ReplaceAll(responseText, "===SKIP===", "")
	responseText = strings.TrimSpace(responseText)

	// 7. Extract and save any LEARN blocks, then strip them from the reply.
	learnBlocks := reLearnBlock.FindAllStringSubmatch(responseText, -1)
	if strings.Contains(responseText, "===LEARN") {
		b.logger.Info("LEARN block detection", "matched", len(learnBlocks), "kb_reader", b.kbReader != nil)
	}
	if len(learnBlocks) > 0 && b.kbReader != nil {
		// Determine the author from the Slack event.
		author := b.resolveUserName(ctx, ev.User)
		for _, lb := range learnBlocks {
			topic := strings.TrimSpace(lb[1])
			content := strings.TrimSpace(lb[2])
			go func(topic, content, author string) {
				if err := b.kbReader.SaveFact(topic, content, author); err != nil {
					b.logger.Warn("failed to save learned fact", "topic", topic, "error", err)
				}
			}(topic, content, author)
		}
		responseText = reLearnBlock.ReplaceAllString(responseText, "")
		responseText = strings.TrimSpace(responseText)
	}

	// 7.5. Extract emoji reaction name, then strip REACT blocks from the reply.
	var reactEmoji string
	if m := reReactBlock.FindStringSubmatch(responseText); len(m) > 1 {
		reactEmoji = strings.TrimSpace(m[1])
		reactEmoji = strings.Trim(reactEmoji, ":")
	}
	responseText = reReactBlock.ReplaceAllString(responseText, "")
	responseText = strings.TrimSpace(responseText)

	// 8. Convert Markdown → Slack mrkdwn, then post.
	replyText := convertToSlackMrkdwn(responseText)
	if len(replyText) > maxReplyLength {
		replyText = replyText[:maxReplyLength-3] + "..."
	}

	b.postReply(ctx, ev.Channel, threadTS, replyText)

	// Add emoji reaction to the user's original message.
	if reactEmoji != "" {
		msgRef := slack.NewRefToMessage(ev.Channel, ev.TimeStamp)
		go func(emoji string) {
			reactionCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := b.slackClient.AddReactionContext(reactionCtx, emoji, msgRef); err != nil {
				b.logger.Warn("failed to add reaction", "emoji", emoji, "error", err)
			}
		}(reactEmoji)
	}
}

// postReply posts a message to the given channel and thread, and tracks the thread.
// Uses Block Kit with explicit mrkdwn type for reliable formatting.
func (b *Bot) postReply(ctx context.Context, channel, threadTS, text string) {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false), // fallback for notifications
		slack.MsgOptionTS(threadTS),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil,
			),
		),
	}
	_, _, err := b.slackClient.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		b.logger.Error("failed to post reply to slack", "channel", channel, "error", err)
		return
	}
	b.trackThread(channel, threadTS)
}

// resolveUserName looks up the display name for a Slack user ID.
func (b *Bot) resolveUserName(ctx context.Context, userID string) string {
	info, err := b.slackClient.GetUserInfoContext(ctx, userID)
	if err != nil {
		b.logger.Debug("failed to resolve user name", "user_id", userID, "error", err)
		return userID
	}
	if info.Profile.DisplayName != "" {
		return info.Profile.DisplayName
	}
	if info.RealName != "" {
		return info.RealName
	}
	return userID
}

// resolveUserMentions replaces Slack user mentions (<@USERID>) with display names.
func (b *Bot) resolveUserMentions(ctx context.Context, text string) string {
	return reSlackUserMention.ReplaceAllStringFunc(text, func(match string) string {
		sub := reSlackUserMention.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		name := b.resolveUserName(ctx, sub[1])
		if name == sub[1] {
			return match // couldn't resolve, keep original
		}
		return name
	})
}

// fetchThreadHistory retrieves prior messages from a Slack thread
// and returns them as Gemini messages for conversation continuity.
func (b *Bot) fetchThreadHistory(ctx context.Context, channel, threadTS, currentMsgTS string) []gemini.Message {
	msgs, _, _, err := b.slackClient.GetConversationRepliesContext(ctx,
		&slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadTS,
			Limit:     maxThreadHistory + 5,
		},
	)
	if err != nil {
		b.logger.Warn("failed to fetch thread history", "error", err)
		return nil
	}

	var history []gemini.Message
	for _, msg := range msgs {
		// Skip the current message — caller will add it.
		if msg.Timestamp == currentMsgTS {
			continue
		}
		text := msg.Text
		if text == "" {
			continue
		}

		if msg.User == b.botUserID {
			history = append(history, gemini.Message{
				Role:    "model",
				Content: text,
			})
		} else {
			// Strip bot mention.
			text = strings.ReplaceAll(text, "<@"+b.botUserID+">", "")
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			// Resolve user mentions in thread history messages.
			text = b.resolveUserMentions(ctx, text)
			// Prefix with sender's name so the model can distinguish between users.
			senderName := b.resolveUserName(ctx, msg.User)
			history = append(history, gemini.Message{
				Role:    "user",
				Content: "[" + senderName + "] " + text,
			})
		}
	}

	// Keep only the most recent messages.
	if len(history) > maxThreadHistory {
		history = history[len(history)-maxThreadHistory:]
	}

	return history
}

// mergeConsecutiveRoles combines adjacent messages with the same role,
// as the Gemini API requires strictly alternating user/model turns.
func mergeConsecutiveRoles(messages []gemini.Message) []gemini.Message {
	if len(messages) == 0 {
		return messages
	}
	merged := []gemini.Message{messages[0]}
	for _, msg := range messages[1:] {
		last := &merged[len(merged)-1]
		if msg.Role == last.Role {
			last.Content += "\n\n" + msg.Content
			last.Images = append(last.Images, msg.Images...)
		} else {
			merged = append(merged, msg)
		}
	}
	return merged
}

// extractURLs finds URLs in message text (both Slack-formatted and plain).
func extractURLs(text string) []string {
	seen := make(map[string]bool)
	var urls []string

	// Slack-formatted URLs: <https://example.com> or <https://example.com|label>
	for _, match := range reSlackURL.FindAllStringSubmatch(text, -1) {
		u := match[1]
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}

	// Plain URLs (fallback)
	for _, u := range rePlainURL.FindAllString(text, -1) {
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}

	return urls
}

var (
	// Matches URLs in message text (including Slack-formatted <url> and <url|label>)
	reSlackURL  = regexp.MustCompile(`<(https?://[^|>]+)(?:\|[^>]*)?>`)
	rePlainURL  = regexp.MustCompile(`https?://[^\s<>]+`)

	reSlackUserMention = regexp.MustCompile(`<@(U[A-Z0-9]+)>`)

	reMarkdownBold   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reMarkdownLink   = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reMarkdownHeader = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reMarkdownStrike = regexp.MustCompile(`~~(.+?)~~`)
	// Matches a closing formatting char (*_~) — preceded by non-space, followed by a letter.
	// Inserts a zero-width space so Slack's mrkdwn parser sees a word boundary.
	reMrkdwnBoundary = regexp.MustCompile(`(\S)([*_~])(\pL)`)
	// Matches Slack-style links to internal file paths: <../path/file.md|label> or <path/file.md|label>
	reInternalLink = regexp.MustCompile(`<[^>]*?(?:indexes/|slack/|google/|notion/|user-provided/|_meta/)[^>]*?\|([^>]+)>`)
	// Matches bare internal paths like ../indexes/people.md or notion/pages/foo.md
	reInternalPath = regexp.MustCompile(`(?:\.\.?/)?(?:indexes|slack|google|notion|user-provided|_meta)/[^\s,)>]+\.md`)

	reLearnBlock     = regexp.MustCompile(`(?s)===LEARN:\s*(.+?)\s*===[ \t]*\r?\n(.*?)===ENDLEARN===[ \t]*\r?\n?`)
	reReactBlock     = regexp.MustCompile(`===REACT:\s*(\S+?)===`)
)

// isCreationTask detects if a query likely involves document/slide/sheet creation.
func isCreationTask(query string) bool {
	creationPatterns := []string{
		"만들어", "작성해", "생성해", "작성하", "생성하", "만들",
		"create", "write", "generate", "draft",
		"문서", "보고서", "회의록", "스프레드시트", "프레젠테이션", "슬라이드",
		"시트 만", "doc 만", "노션",
	}
	lower := strings.ToLower(query)
	for _, pattern := range creationPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// isComplexQuery detects if a query requires deeper reasoning.
func isComplexQuery(query string) bool {
	complexPatterns := []string{
		// Korean analysis/comparison requests
		"분석해", "비교해", "정리해", "요약해", "브리핑", "현황",
		"장단점", "평가해", "검토해", "진단해", "리뷰해",
		// English analysis patterns
		"analyze", "compare", "summarize", "evaluate", "review",
		"pros and cons", "trade-off", "assessment",
		// Reasoning indicators
		"왜", "어떻게", "why", "how does", "how can", "how should",
		"explain why", "what are the implications",
	}
	lower := strings.ToLower(query)
	for _, pattern := range complexPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	// Long queries suggest complex intent
	if len(query) > 200 {
		return true
	}
	// Multiple questions
	if strings.Count(query, "?") >= 2 {
		return true
	}
	return false
}

// convertToSlackMrkdwn converts standard Markdown formatting to Slack mrkdwn.
func convertToSlackMrkdwn(text string) string {
	// **bold** → *bold*
	text = reMarkdownBold.ReplaceAllString(text, "*$1*")
	// [text](url) → <url|text>
	text = reMarkdownLink.ReplaceAllString(text, "<$2|$1>")
	// ## Header → *Header*
	text = reMarkdownHeader.ReplaceAllString(text, "*$1*")
	// ~~strike~~ → ~strike~
	text = reMarkdownStrike.ReplaceAllString(text, "~$1~")
	// Fix word boundaries: *bold*이에요 → *bold*​이에요 (insert zero-width space)
	text = reMrkdwnBoundary.ReplaceAllString(text, "$1$2\u200B$3")
	// Strip internal file path links: <../indexes/people.md|인물 인덱스> → 인물 인덱스
	text = reInternalLink.ReplaceAllString(text, "$1")
	// Strip bare internal file paths
	text = reInternalPath.ReplaceAllString(text, "")
	return text
}

// fetchThreadFiles downloads file attachments from recent messages in the thread
// (excluding the current message). This allows the bot to access files shared earlier.
func (b *Bot) fetchThreadFiles(ctx context.Context, channel, threadTS, currentMsgTS string) map[string][]byte {
	files := make(map[string][]byte)

	msgs, _, _, err := b.slackClient.GetConversationRepliesContext(ctx,
		&slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadTS,
			Limit:     10,
		},
	)
	if err != nil {
		b.logger.Warn("failed to fetch thread files", "error", err)
		return files
	}

	for _, msg := range msgs {
		if msg.Timestamp == currentMsgTS {
			continue
		}
		for _, f := range msg.Files {
			url := f.URLPrivateDownload
			if url == "" {
				url = f.URLPrivate
			}
			if url == "" {
				continue
			}
			name := f.Name
			if name == "" {
				name = f.ID
			}
			if _, exists := files[name]; exists {
				continue
			}
			data, err := b.downloadSlackFile(ctx, url)
			if err != nil {
				b.logger.Warn("failed to download thread file", "file", name, "error", err)
				continue
			}
			files[name] = data
		}
	}

	return files
}

// downloadSlackFile downloads a Slack-hosted file using the bot token as Bearer auth.
func (b *Bot) downloadSlackFile(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+b.botToken)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("download file: HTTP %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxFileDownload)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read file body: %w", err)
	}

	return data, nil
}
