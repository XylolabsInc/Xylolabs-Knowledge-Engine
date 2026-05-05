package bot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
)

// SlackPlatform implements the Platform interface for Slack.
type SlackPlatform struct {
	client    *slack.Client
	botUserID string
	botToken  string
	logger    *slog.Logger
	http      *http.Client

	// Track threads where the bot has responded.
	// Key: "channel:thread_ts"
	trackedThreads   map[string]bool
	trackedThreadsMu sync.RWMutex

	// Negative cache for threads confirmed not involving the bot.
	notBotThreads   map[string]time.Time
	notBotThreadsMu sync.RWMutex

	done chan struct{}
}

// NewSlackPlatform creates a SlackPlatform.
func NewSlackPlatform(client *slack.Client, botUserID, botToken string, logger *slog.Logger) *SlackPlatform {
	return &SlackPlatform{
		client:         client,
		botUserID:      botUserID,
		botToken:       botToken,
		logger:         logger.With("component", "slack-platform"),
		http:           extractor.NewRestrictedHTTPClient(30 * time.Second),
		trackedThreads: make(map[string]bool),
		notBotThreads:  make(map[string]time.Time),
		done:           make(chan struct{}),
	}
}

// Name returns the platform identifier.
func (p *SlackPlatform) Name() string { return "slack" }

// BotUserID returns the bot's user ID.
func (p *SlackPlatform) BotUserID() string { return p.botUserID }

// StartCleanup begins periodic cleanup of thread caches.
func (p *SlackPlatform) StartCleanup() {
	go func() {
		ticker := time.NewTicker(threadCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-ticker.C:
				p.cleanupCaches()
			}
		}
	}()
}

// StopCleanup stops the periodic cleanup goroutine.
func (p *SlackPlatform) StopCleanup() {
	close(p.done)
}

func (p *SlackPlatform) cleanupCaches() {
	p.trackedThreadsMu.Lock()
	if len(p.trackedThreads) > maxTrackedThreads {
		target := len(p.trackedThreads) / 2
		count := 0
		for k := range p.trackedThreads {
			delete(p.trackedThreads, k)
			count++
			if count >= target {
				break
			}
		}
	}
	p.trackedThreadsMu.Unlock()

	now := time.Now()
	p.notBotThreadsMu.Lock()
	for k, checkedAt := range p.notBotThreads {
		if now.Sub(checkedAt) > notBotCacheTTL {
			delete(p.notBotThreads, k)
		}
	}
	if len(p.notBotThreads) > maxNotBotThreads {
		target := len(p.notBotThreads) / 2
		count := 0
		for k := range p.notBotThreads {
			delete(p.notBotThreads, k)
			count++
			if count >= target {
				break
			}
		}
	}
	p.notBotThreadsMu.Unlock()
}

// IsTrackedThread returns true if the bot has previously replied in this thread.
func (p *SlackPlatform) IsTrackedThread(channel, threadID string) bool {
	key := channel + ":" + threadID

	p.trackedThreadsMu.RLock()
	cached := p.trackedThreads[key]
	p.trackedThreadsMu.RUnlock()
	if cached {
		return true
	}

	p.notBotThreadsMu.RLock()
	if checkedAt, ok := p.notBotThreads[key]; ok && time.Since(checkedAt) < notBotCacheTTL {
		p.notBotThreadsMu.RUnlock()
		return false
	}
	p.notBotThreadsMu.RUnlock()

	msgs, _, _, err := p.client.GetConversationRepliesContext(
		context.Background(),
		&slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadID,
			Limit:     50,
		},
	)
	if err != nil {
		p.logger.Debug("failed to check thread for bot replies", "error", err)
		return false
	}

	for _, msg := range msgs {
		if msg.User == p.botUserID {
			p.TrackThread(channel, threadID)
			return true
		}
	}

	p.notBotThreadsMu.Lock()
	p.notBotThreads[key] = time.Now()
	p.notBotThreadsMu.Unlock()
	return false
}

// TrackThread marks a thread as one the bot has participated in.
func (p *SlackPlatform) TrackThread(channel, threadID string) {
	p.trackedThreadsMu.Lock()
	defer p.trackedThreadsMu.Unlock()
	p.trackedThreads[channel+":"+threadID] = true
}

// PostReply posts a reply to the given channel/thread using Slack Block Kit.
func (p *SlackPlatform) PostReply(ctx context.Context, channel, threadID, text string) error {
	const maxBlockTextLen = 3000

	var blocks []slack.Block
	remaining := text
	for len(remaining) > 0 {
		chunk := remaining
		if len(chunk) > maxBlockTextLen {
			cut := strings.LastIndex(chunk[:maxBlockTextLen], "\n\n")
			if cut < maxBlockTextLen/2 {
				cut = strings.LastIndex(chunk[:maxBlockTextLen], "\n")
			}
			if cut < maxBlockTextLen/2 {
				cut = maxBlockTextLen
			}
			chunk = remaining[:cut]
			remaining = strings.TrimLeft(remaining[cut:], "\n")
		} else {
			remaining = ""
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", chunk, false, false),
			nil, nil,
		))
	}

	if len(blocks) == 0 {
		return nil
	}

	fallback := blocks[0].(*slack.SectionBlock).Text.Text
	if len(fallback) > 300 {
		fallback = fallback[:297] + "..."
	}

	opts := []slack.MsgOption{
		slack.MsgOptionText(fallback, false),
		slack.MsgOptionTS(threadID),
		slack.MsgOptionBlocks(blocks...),
	}
	_, _, err := p.client.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		return fmt.Errorf("post message: %w", err)
	}
	p.TrackThread(channel, threadID)
	return nil
}

// UploadFile uploads a file to the given Slack channel/thread.
func (p *SlackPlatform) UploadFile(ctx context.Context, channel, threadID, filename string, data []byte) error {
	_, err := p.client.UploadFileContext(ctx, slack.UploadFileParameters{
		Channel:         channel,
		Filename:        filename,
		FileSize:        len(data),
		Reader:          bytes.NewReader(data),
		Title:           filename,
		ThreadTimestamp: threadID,
	})
	if err != nil {
		return fmt.Errorf("upload file: %w", err)
	}
	return nil
}

// AddReaction adds an emoji reaction to a message.
func (p *SlackPlatform) AddReaction(ctx context.Context, channel, messageID, emoji string) error {
	msgRef := slack.NewRefToMessage(channel, messageID)
	return p.client.AddReactionContext(ctx, emoji, msgRef)
}

// ResolveUserName looks up the display name for a Slack user ID.
func (p *SlackPlatform) ResolveUserName(ctx context.Context, userID string) string {
	info, err := p.client.GetUserInfoContext(ctx, userID)
	if err != nil {
		p.logger.Debug("failed to resolve user name", "user_id", userID, "error", err)
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

// CleanMentions replaces Slack user mentions (<@USERID>) with display names.
func (p *SlackPlatform) CleanMentions(ctx context.Context, text string) string {
	return reSlackUserMention.ReplaceAllStringFunc(text, func(match string) string {
		sub := reSlackUserMention.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		name := p.ResolveUserName(ctx, sub[1])
		if name == sub[1] {
			return match
		}
		return name
	})
}

// StripBotMention removes the bot's @mention from text.
func (p *SlackPlatform) StripBotMention(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, "<@"+p.botUserID+">", ""))
}

// ExtractURLs finds URLs in Slack message text (both Slack-formatted and plain).
func (p *SlackPlatform) ExtractURLs(text string) []string {
	return extractURLs(text)
}

// FormatResponse converts Markdown to Slack mrkdwn.
func (p *SlackPlatform) FormatResponse(text string) string {
	return convertToSlackMrkdwn(text)
}

// FetchThreadHistory retrieves prior messages from a Slack thread as Gemini messages.
func (p *SlackPlatform) FetchThreadHistory(ctx context.Context, channel, threadID, excludeMsgID string) []gemini.Message {
	msgs, _, _, err := p.client.GetConversationRepliesContext(ctx,
		&slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadID,
			Limit:     maxThreadHistory + 5,
		},
	)
	if err != nil {
		p.logger.Warn("failed to fetch thread history", "error", err)
		return nil
	}

	var history []gemini.Message
	for _, msg := range msgs {
		if msg.Timestamp == excludeMsgID {
			continue
		}
		text := msg.Text
		if text == "" {
			continue
		}

		if msg.User == p.botUserID {
			history = append(history, gemini.Message{
				Role:    "model",
				Content: text,
			})
		} else {
			text = strings.ReplaceAll(text, "<@"+p.botUserID+">", "")
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			text = p.CleanMentions(ctx, text)
			senderName := p.ResolveUserName(ctx, msg.User)
			history = append(history, gemini.Message{
				Role:    "user",
				Content: "[" + senderName + "] " + text,
			})
		}
	}

	if len(history) > maxThreadHistory {
		history = history[len(history)-maxThreadHistory:]
	}

	return history
}

// FetchThreadFiles downloads file attachments from prior messages in a thread.
func (p *SlackPlatform) FetchThreadFiles(ctx context.Context, channel, threadID, excludeMsgID string) map[string][]byte {
	files := make(map[string][]byte)

	msgs, _, _, err := p.client.GetConversationRepliesContext(ctx,
		&slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadID,
			Limit:     10,
		},
	)
	if err != nil {
		p.logger.Warn("failed to fetch thread files", "error", err)
		return files
	}

	for _, msg := range msgs {
		if msg.Timestamp == excludeMsgID {
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
			data, err := p.DownloadFile(ctx, url)
			if err != nil {
				p.logger.Warn("failed to download thread file", "file", name, "error", err)
				continue
			}
			files[name] = data
		}
	}

	return files
}

// DownloadFile downloads a Slack-hosted file using the bot token as Bearer auth.
func (p *SlackPlatform) DownloadFile(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.botToken)

	resp, err := p.http.Do(req)
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

// Slack-specific regexes.
var (
	reSlackURL         = regexp.MustCompile(`<(https?://[^|>]+)(?:\|[^>]*)?>`)
	reSlackUserMention = regexp.MustCompile(`<@(U[A-Z0-9]+)>`)

	reMarkdownBold   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reMarkdownLink   = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reMarkdownHeader = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reMarkdownStrike = regexp.MustCompile(`~~(.+?)~~`)
	reMrkdwnBoundary = regexp.MustCompile(`(\S)([*_~])(\pL)`)
	reInternalLink   = regexp.MustCompile(`<[^>]*?(?:indexes/|slack/|google/|notion/|user-provided/|_meta/)[^>]*?\|([^>]+)>`)
	reInternalPath   = regexp.MustCompile(`(?:\.\.?/)?(?:indexes|slack|google|notion|user-provided|_meta)/[^\s,)>]+\.md`)
)

// extractURLs finds URLs in Slack message text (both Slack-formatted and plain).
func extractURLs(text string) []string {
	seen := make(map[string]bool)
	var urls []string

	for _, match := range reSlackURL.FindAllStringSubmatch(text, -1) {
		u := match[1]
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}

	for _, u := range rePlainURL.FindAllString(text, -1) {
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}

	return urls
}

// convertToSlackMrkdwn converts standard Markdown formatting to Slack mrkdwn.
func convertToSlackMrkdwn(text string) string {
	text = reMarkdownBold.ReplaceAllString(text, "*$1*")
	text = reMarkdownLink.ReplaceAllString(text, "<$2|$1>")
	text = reMarkdownHeader.ReplaceAllString(text, "*$1*")
	text = reMarkdownStrike.ReplaceAllString(text, "~$1~")
	text = fixBoundariesPreserveLinks(text)
	text = reInternalLink.ReplaceAllString(text, "$1")
	text = reInternalPath.ReplaceAllString(text, "")
	return text
}

// fixBoundariesPreserveLinks applies the word-boundary zero-width space fix
// only to text outside of Slack link constructs (<...>), so URLs aren't corrupted.
func fixBoundariesPreserveLinks(text string) string {
	var result strings.Builder
	for len(text) > 0 {
		openIdx := strings.Index(text, "<")
		if openIdx < 0 {
			result.WriteString(reMrkdwnBoundary.ReplaceAllString(text, "$1$2\u200B$3"))
			break
		}
		result.WriteString(reMrkdwnBoundary.ReplaceAllString(text[:openIdx], "$1$2\u200B$3"))
		text = text[openIdx:]
		closeIdx := strings.Index(text, ">")
		if closeIdx < 0 {
			result.WriteString(reMrkdwnBoundary.ReplaceAllString(text, "$1$2\u200B$3"))
			break
		}
		result.WriteString(text[:closeIdx+1])
		text = text[closeIdx+1:]
	}
	return result.String()
}
