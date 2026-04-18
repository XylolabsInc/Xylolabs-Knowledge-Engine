package bot

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
	"github.com/xylolabsinc/xylolabs-kb/internal/kbrepo"
	"github.com/xylolabsinc/xylolabs-kb/internal/tools"
)

//go:embed system_prompt.txt
var systemPromptTemplate string

// Bot handles incoming messages with Gemini-powered responses.
// It is platform-agnostic: all platform I/O is delegated to b.platform.
type Bot struct {
	platform     Platform
	gemini       *gemini.Client
	kbReader     *kbrepo.Reader
	proModel     string
	systemPrompt string
	location     *time.Location
	logger       *slog.Logger
	toolExecutor *tools.ToolExecutor
	extractor    *extractor.Extractor
	wg           sync.WaitGroup
}

// New creates a Bot handler backed by the given Platform.
func New(platform Platform, geminiClient *gemini.Client, kbReader *kbrepo.Reader, proModel, systemPromptFile string, location *time.Location, logger *slog.Logger) *Bot {
	return &Bot{
		platform:     platform,
		gemini:       geminiClient,
		kbReader:     kbReader,
		proModel:     proModel,
		systemPrompt: loadSystemPrompt(systemPromptFile),
		location:     location,
		logger:       logger.With("component", "bot"),
	}
}

// loadSystemPrompt returns the system prompt template from filePath if provided,
// falling back to the embedded default if the path is empty or the file cannot be read.
func loadSystemPrompt(filePath string) string {
	if filePath == "" {
		return systemPromptTemplate
	}
	root, err := os.OpenRoot(filepath.Dir(filepath.Clean(filePath)))
	if err != nil {
		slog.Warn("failed to open custom system prompt directory, using default", "path", filePath, "error", err)
		return systemPromptTemplate
	}
	defer root.Close()

	data, err := root.ReadFile(filepath.Base(filePath))
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

// Wait blocks until all background goroutines started by the bot have finished.
func (b *Bot) Wait() {
	b.wg.Wait()
}

// IsTrackedThread delegates to the platform.
func (b *Bot) IsTrackedThread(channel, threadID string) bool {
	return b.platform.IsTrackedThread(channel, threadID)
}

// HandleMention processes a message where the bot was @mentioned.
func (b *Bot) HandleMention(ctx context.Context, msg *IncomingMessage) {
	query := b.platform.StripBotMention(msg.Text)
	if query == "" {
		query = "Hello! How can I help you?"
	}
	b.respond(ctx, msg, query)
}

// HandleDirectMessage processes a direct message to the bot.
func (b *Bot) HandleDirectMessage(ctx context.Context, msg *IncomingMessage) {
	query := strings.TrimSpace(msg.Text)
	if query == "" {
		return
	}
	b.respond(ctx, msg, query)
}

// respond loads KB context, builds the Gemini prompt, and posts the reply via the platform.
func (b *Bot) respond(ctx context.Context, msg *IncomingMessage, query string) {
	threadID := msg.ThreadID
	if threadID == "" {
		threadID = msg.MessageID
	}

	// Resolve user mentions in the query text.
	query = b.platform.CleanMentions(ctx, query)

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

	// Truncate KB context if it exceeds budget.
	if len(kbContext) > maxKBContextChars {
		originalLen := len(kbContext)
		kbContext = kbContext[:maxKBContextChars] + "\n\n[... KB context truncated due to size ...]"
		b.logger.Warn("kb context truncated", "original_length", originalLen, "max", maxKBContextChars)
	}

	// 2. Download any attached files from the message.
	var images []gemini.Image
	fileAttachments := make(map[string][]byte)
	var fileNames []string

	for _, f := range msg.Files {
		if f.URL == "" {
			continue
		}
		data, err := b.platform.DownloadFile(ctx, f.URL)
		if err != nil {
			b.logger.Warn("failed to download file", "file_id", f.ID, "error", err)
			continue
		}
		mimeType := f.MimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		fileName := f.Name
		if fileName == "" {
			fileName = f.ID
		}

		if strings.HasPrefix(mimeType, "image/") && len(data) > 0 {
			images = append(images, gemini.Image{MimeType: mimeType, Data: data})
		} else if mimeType == "application/pdf" && len(data) > 0 {
			if b.extractor != nil {
				result, err := b.extractor.ExtractFromBytes(ctx, data, mimeType, fileName)
				if err != nil {
					b.logger.Warn("PDF extraction failed, skipping inline", "file", fileName, "error", err)
					query = appendWithBudget(query, fmt.Sprintf("\n\n[Attached PDF: %s (could not be read)]", fileName), maxQueryBudget)
				} else {
					images = append(images, gemini.Image{MimeType: mimeType, Data: data})
					if result.Text != "" && !strings.HasPrefix(result.Text, "[") {
						content := result.Text
						if len(content) > 8000 {
							content = content[:8000] + "\n..."
						}
						query = appendWithBudget(query, fmt.Sprintf("\n\n---\nFile: %s\n%s\n---", fileName, content), maxQueryBudget)
					}
				}
			} else {
				images = append(images, gemini.Image{MimeType: mimeType, Data: data})
			}
		} else if b.extractor != nil {
			result, err := b.extractor.ExtractFromBytes(ctx, data, mimeType, fileName)
			if err == nil && result.Text != "" && !strings.HasPrefix(result.Text, "[") {
				content := result.Text
				if len(content) > 8000 {
					content = content[:8000] + "\n..."
				}
				query = appendWithBudget(query, fmt.Sprintf("\n\n---\nFile: %s\n%s\n---", fileName, content), maxQueryBudget)
			}
		}
		fileAttachments[fileName] = data
		fileNames = append(fileNames, fileName)
	}

	// Also fetch files from previous messages in the thread.
	if msg.ThreadID != "" {
		threadFiles := b.platform.FetchThreadFiles(ctx, msg.Channel, threadID, msg.MessageID)
		for name, data := range threadFiles {
			if _, exists := fileAttachments[name]; !exists {
				fileAttachments[name] = data
				fileNames = append(fileNames, name)
				if b.extractor != nil {
					result, err := b.extractor.ExtractFromBytes(ctx, data, "application/octet-stream", name)
					if err == nil && result.Text != "" && !strings.HasPrefix(result.Text, "[") {
						content := result.Text
						if len(content) > 8000 {
							content = content[:8000] + "\n..."
						}
						query = appendWithBudget(query, fmt.Sprintf("\n\n---\nFile: %s\n%s\n---", name, content), maxQueryBudget)
					}
				}
			}
		}
	}

	if len(fileNames) > 0 {
		query = appendWithBudget(query, "\n\n[Attached files: "+strings.Join(fileNames, ", ")+"]", maxQueryBudget)
	}

	// Store file attachments for tool executor (upload_to_drive) via context.
	if b.toolExecutor != nil && len(fileAttachments) > 0 {
		ctx = tools.ContextWithAttachments(ctx, fileAttachments)
	}

	// 2.5. Extract and fetch URLs from message text for context.
	if b.extractor != nil {
		for _, u := range b.platform.ExtractURLs(query) {
			result, err := b.extractor.ExtractFromURL(ctx, u)
			if err != nil {
				b.logger.Warn("failed to fetch URL", "url", u, "error", err)
				continue
			}
			if result.Text != "" {
				content := result.Text
				if len(content) > 5000 {
					content = content[:5000] + "..."
				}
				query = appendWithBudget(query, fmt.Sprintf("\n\n---\nLink: %s\n%s\n---", u, content), maxQueryBudget)
			}
		}
	}

	// 3. Build system prompt with full KB context.
	userName := b.platform.ResolveUserName(ctx, msg.UserID)
	now := time.Now().In(b.location)
	currentTime := now.Format("2006-01-02 (Monday) 15:04 MST")
	systemPrompt := fmt.Sprintf(b.systemPrompt, userName, platformFormattingInstructions(b.platform.Name())) + "\n\nCurrent date and time: " + currentTime + "\n\n--- Reference Materials ---\n" + kbContext + "\n---"

	if len(systemPrompt) > maxSystemPromptChars {
		systemPrompt = systemPrompt[:maxSystemPromptChars] + "\n[... truncated ...]"
		b.logger.Warn("system prompt truncated", "length", len(systemPrompt), "max", maxSystemPromptChars)
	}

	// 4. Build conversation messages with thread history for context continuity.
	var messages []gemini.Message
	if msg.ThreadID != "" {
		messages = b.platform.FetchThreadHistory(ctx, msg.Channel, threadID, msg.MessageID)
	}

	// 2.6. Extract and fetch URLs from thread history messages.
	if b.extractor != nil {
		for _, hm := range messages {
			if hm.Role != "user" {
				continue
			}
			for _, u := range b.platform.ExtractURLs(hm.Content) {
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
					query = appendWithBudget(query, fmt.Sprintf("\n\n---\nLink: %s\n%s\n---", u, content), maxQueryBudget)
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

	genReq := gemini.GenerateRequest{
		SystemPrompt:  systemPrompt,
		ThinkingLevel: "low",
		Messages:      messages,
		GoogleSearch:  true,
	}
	if b.toolExecutor != nil {
		genReq.Tools = b.toolExecutor.Declarations()
	}

	if b.proModel != "" && isCreationTask(query) {
		genReq.Model = b.proModel
		genReq.ThinkingLevel = "high"
	} else if isComplexQuery(query) {
		genReq.ThinkingLevel = "high"
	}

	// Tool calling loop: call Gemini, execute any tools, re-call with results.
	var genResp *gemini.GenerateResponse
	for i := 0; i <= maxToolIterations; i++ {
		var err error
		genResp, err = b.gemini.Generate(ctx, genReq)
		if err != nil {
			b.logger.Error("gemini generate failed", "error", err, "iteration", i)
			if postErr := b.platform.PostReply(ctx, msg.Channel, threadID, fmt.Sprintf("An error occurred: %v", err)); postErr != nil {
				b.logger.Warn("failed to post error reply", "error", postErr)
			}
			return
		}

		if len(genResp.FunctionCalls) == 0 {
			break
		}

		if b.toolExecutor == nil {
			b.logger.Warn("model returned function calls but no executor configured")
			break
		}

		b.logger.Info("executing tool calls", "count", len(genResp.FunctionCalls), "iteration", i)

		genReq.Messages = append(genReq.Messages, gemini.Message{
			Role:          "model",
			FunctionCalls: genResp.FunctionCalls,
		})

		var responses []gemini.FunctionResponse
		for _, fc := range genResp.FunctionCalls {
			if fc.Name == "google_search" {
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

		genReq.Messages = append(genReq.Messages, gemini.Message{
			Role:              "user",
			FunctionResponses: responses,
		})
	}

	// 5.5. Upload screenshot attachments if any were produced by tools.
	if b.toolExecutor != nil {
		if screenshotData, ok := b.toolExecutor.PopScreenshot(); ok {
			asyncCtx := context.WithoutCancel(ctx)
			b.wg.Add(1)
			go func(data []byte, channel, ts string) {
				defer b.wg.Done()
				uploadCtx, cancel := context.WithTimeout(asyncCtx, 60*time.Second)
				defer cancel()
				if err := b.platform.UploadFile(uploadCtx, channel, ts, "screenshot.png", data); err != nil {
					b.logger.Warn("failed to upload screenshot", "error", err, "size_bytes", len(data))
				} else {
					b.logger.Info("screenshot uploaded", "platform", b.platform.Name(), "channel", channel, "size_bytes", len(data))
				}
			}(screenshotData, msg.Channel, threadID)
		}
	}

	// 5. Check if the model decided to skip this message.
	responseText := genResp.Text
	if strings.TrimSpace(responseText) == "===SKIP===" {
		b.logger.Debug("skipping trivial message", "query", query)
		return
	}
	responseText = strings.ReplaceAll(responseText, "===SKIP===", "")
	responseText = strings.TrimSpace(responseText)

	// 7. Extract and save any LEARN blocks, then strip them from the reply.
	learnBlocks := reLearnBlock.FindAllStringSubmatch(responseText, -1)
	if strings.Contains(responseText, "===LEARN") {
		b.logger.Info("LEARN block detection", "matched", len(learnBlocks), "kb_reader", b.kbReader != nil)
	}
	if len(learnBlocks) > 0 && b.kbReader != nil {
		author := b.platform.ResolveUserName(ctx, msg.UserID)
		for _, lb := range learnBlocks {
			topic := strings.TrimSpace(lb[1])
			content := strings.TrimSpace(lb[2])
			b.wg.Add(1)
				go func(topic, content, author string) {
					defer b.wg.Done()
					if err := b.kbReader.SaveFact(topic, content, author); err != nil {
						b.logger.Warn("failed to save learned fact", "topic", topic, "error", err)
					}
				}(topic, content, author)
		}
		responseText = reLearnBlock.ReplaceAllString(responseText, "")
		responseText = strings.TrimSpace(responseText)
	}

	if strings.Contains(responseText, "===LEARN") {
		responseText = reLearnBlockCleanup.ReplaceAllString(responseText, "")
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

	b.logger.Info("response post-processing",
		"raw_length", len(genResp.Text),
		"after_strip_length", len(responseText),
		"query", query,
	)

	// 8. Format and post the response.
	replyText := b.platform.FormatResponse(responseText)
	if len(replyText) > maxReplyLength {
		truncated := replyText[:maxReplyLength-3]
		for len(truncated) > 0 && !utf8.RuneStart(truncated[len(truncated)-1]) {
			truncated = truncated[:len(truncated)-1]
		}
		replyText = truncated + "..."
	}

	if err := b.platform.PostReply(ctx, msg.Channel, threadID, replyText); err != nil {
		b.logger.Error("failed to post reply", "platform", b.platform.Name(), "channel", msg.Channel, "error", err)
	}

	// Add emoji reaction to the user's original message.
	if reactEmoji != "" {
		asyncCtx := context.WithoutCancel(ctx)
		b.wg.Add(1)
		go func(emoji string) {
			defer b.wg.Done()
			reactionCtx, cancel := context.WithTimeout(asyncCtx, 5*time.Second)
			defer cancel()
			if err := b.platform.AddReaction(reactionCtx, msg.Channel, msg.MessageID, emoji); err != nil {
				b.logger.Warn("failed to add reaction", "emoji", emoji, "error", err)
			}
		}(reactEmoji)
	}
}

// appendWithBudget appends addition to query if it fits within budget.
// If the addition would exceed budget, it is truncated with "..." or skipped.
func appendWithBudget(query, addition string, budget int) string {
	if len(query)+len(addition) <= budget {
		return query + addition
	}
	remaining := budget - len(query)
	if remaining <= 200 {
		return query
	}
	truncated := addition[:remaining-3]
	for len(truncated) > 0 && !utf8.RuneStart(truncated[len(truncated)-1]) {
		truncated = truncated[:len(truncated)-1]
	}
	return query + truncated + "..."
}
