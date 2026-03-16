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

	"github.com/bwmarrin/discordgo"

	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
)

// DiscordPlatform implements the Platform interface for Discord.
type DiscordPlatform struct {
	session   *discordgo.Session
	botUserID string
	guildID   string
	logger    *slog.Logger
	http      *http.Client

	trackedThreads   map[string]bool
	trackedThreadsMu sync.RWMutex

	notBotThreads   map[string]time.Time
	notBotThreadsMu sync.RWMutex

	done chan struct{}
}

// NewDiscordPlatform creates a DiscordPlatform.
func NewDiscordPlatform(session *discordgo.Session, botUserID, guildID string, logger *slog.Logger) *DiscordPlatform {
	return &DiscordPlatform{
		session:        session,
		botUserID:      botUserID,
		guildID:        guildID,
		logger:         logger.With("component", "discord-platform"),
		http:           &http.Client{Timeout: 30 * time.Second},
		trackedThreads: make(map[string]bool),
		notBotThreads:  make(map[string]time.Time),
		done:           make(chan struct{}),
	}
}

// Session returns the underlying discordgo session.
func (p *DiscordPlatform) Session() *discordgo.Session { return p.session }

// Name returns the platform identifier.
func (p *DiscordPlatform) Name() string { return "discord" }

// BotUserID returns the bot's user ID.
func (p *DiscordPlatform) BotUserID() string { return p.botUserID }

// StartCleanup begins periodic cleanup of thread caches.
func (p *DiscordPlatform) StartCleanup() {
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
func (p *DiscordPlatform) StopCleanup() {
	close(p.done)
}

func (p *DiscordPlatform) cleanupCaches() {
	p.trackedThreadsMu.Lock()
	if len(p.trackedThreads) > maxTrackedThreads {
		count := 0
		for k := range p.trackedThreads {
			delete(p.trackedThreads, k)
			count++
			if count >= len(p.trackedThreads)/2 {
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
		count := 0
		for k := range p.notBotThreads {
			delete(p.notBotThreads, k)
			count++
			if count >= len(p.notBotThreads)/2 {
				break
			}
		}
	}
	p.notBotThreadsMu.Unlock()
}

// IsTrackedThread returns true if the bot has previously replied in this thread.
func (p *DiscordPlatform) IsTrackedThread(channel, threadID string) bool {
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

	// Fetch recent messages from the thread to check for bot participation.
	msgs, err := p.session.ChannelMessages(threadID, 50, "", "", "")
	if err != nil {
		p.logger.Debug("failed to check thread for bot replies", "error", err)
		return false
	}

	for _, msg := range msgs {
		if msg.Author != nil && msg.Author.ID == p.botUserID {
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
func (p *DiscordPlatform) TrackThread(channel, threadID string) {
	p.trackedThreadsMu.Lock()
	defer p.trackedThreadsMu.Unlock()
	p.trackedThreads[channel+":"+threadID] = true
}

const discordMaxLen = 2000

// PostReply posts a reply to the given channel/thread.
func (p *DiscordPlatform) PostReply(ctx context.Context, channel, threadID, text string) error {
	targetChannel := threadID
	if targetChannel == "" {
		targetChannel = channel
	}

	// Split long messages at paragraph/newline boundaries.
	chunks := splitDiscordMessage(text)
	for _, chunk := range chunks {
		_, err := p.session.ChannelMessageSend(targetChannel, chunk)
		if err != nil {
			return fmt.Errorf("post message: %w", err)
		}
	}
	p.TrackThread(channel, threadID)
	return nil
}

// splitDiscordMessage splits text into chunks that fit within Discord's 2000-char limit.
func splitDiscordMessage(text string) []string {
	if len(text) <= discordMaxLen {
		return []string{text}
	}

	var chunks []string
	remaining := text
	for len(remaining) > 0 {
		if len(remaining) <= discordMaxLen {
			chunks = append(chunks, remaining)
			break
		}

		chunk := remaining[:discordMaxLen]

		// Try to split at a paragraph boundary first.
		cut := strings.LastIndex(chunk, "\n\n")
		if cut < discordMaxLen/2 {
			// Fall back to a single newline.
			cut = strings.LastIndex(chunk, "\n")
		}
		if cut < discordMaxLen/2 {
			// Fall back to a space.
			cut = strings.LastIndex(chunk, " ")
		}
		if cut < discordMaxLen/2 {
			// Hard cut at the limit.
			cut = discordMaxLen
		}

		chunks = append(chunks, remaining[:cut])
		remaining = strings.TrimLeft(remaining[cut:], "\n ")
	}
	return chunks
}

// UploadFile uploads a file to the given channel/thread.
func (p *DiscordPlatform) UploadFile(ctx context.Context, channel, threadID, filename string, data []byte) error {
	targetChannel := threadID
	if targetChannel == "" {
		targetChannel = channel
	}
	_, err := p.session.ChannelFileSendWithMessage(targetChannel, "", filename, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("upload file: %w", err)
	}
	return nil
}

// slackToUnicodeEmoji maps common Slack emoji names to Unicode equivalents.
var slackToUnicodeEmoji = map[string]string{
	"thumbsup":         "\U0001f44d",
	"thumbsdown":       "\U0001f44e",
	"+1":               "\U0001f44d",
	"-1":               "\U0001f44e",
	"heart":            "\u2764\ufe0f",
	"smile":            "\U0001f604",
	"laughing":         "\U0001f606",
	"thinking_face":    "\U0001f914",
	"eyes":             "\U0001f440",
	"tada":             "\U0001f389",
	"white_check_mark": "\u2705",
	"x":                "\u274c",
	"wave":             "\U0001f44b",
	"fire":             "\U0001f525",
	"rocket":           "\U0001f680",
	"star":             "\u2b50",
	"memo":             "\U0001f4dd",
	"bulb":             "\U0001f4a1",
	"warning":          "\u26a0\ufe0f",
	"100":              "\U0001f4af",
	"pray":             "\U0001f64f",
	"ok_hand":          "\U0001f44c",
}

// AddReaction adds an emoji reaction to a message.
func (p *DiscordPlatform) AddReaction(ctx context.Context, channel, messageID, emoji string) error {
	unicodeEmoji, ok := slackToUnicodeEmoji[emoji]
	if !ok {
		// Try using the name directly (may be a Unicode emoji or custom Discord emoji).
		unicodeEmoji = emoji
	}
	return p.session.MessageReactionAdd(channel, messageID, unicodeEmoji)
}

// ResolveUserName looks up the display name for a Discord user ID.
func (p *DiscordPlatform) ResolveUserName(ctx context.Context, userID string) string {
	member, err := p.session.GuildMember(p.guildID, userID)
	if err != nil {
		p.logger.Debug("failed to resolve user name", "user_id", userID, "error", err)
		return userID
	}
	if member.Nick != "" {
		return member.Nick
	}
	if member.User != nil {
		if member.User.GlobalName != "" {
			return member.User.GlobalName
		}
		if member.User.Username != "" {
			return member.User.Username
		}
	}
	return userID
}

// Discord-specific regexes.
var reDiscordUserMention = regexp.MustCompile(`<@!?(\d+)>`)

// CleanMentions replaces Discord user mentions (<@ID> or <@!ID>) with display names.
func (p *DiscordPlatform) CleanMentions(ctx context.Context, text string) string {
	return reDiscordUserMention.ReplaceAllStringFunc(text, func(match string) string {
		sub := reDiscordUserMention.FindStringSubmatch(match)
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
func (p *DiscordPlatform) StripBotMention(text string) string {
	// Discord mentions can be <@ID> or <@!ID>.
	text = strings.ReplaceAll(text, "<@!"+p.botUserID+">", "")
	text = strings.ReplaceAll(text, "<@"+p.botUserID+">", "")
	return strings.TrimSpace(text)
}

// ExtractURLs finds plain URLs in Discord message text.
func (p *DiscordPlatform) ExtractURLs(text string) []string {
	seen := make(map[string]bool)
	var urls []string
	for _, u := range rePlainURL.FindAllString(text, -1) {
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	return urls
}

// reDiscordInternalPath strips internal .md path references from responses.
var reDiscordInternalPath = regexp.MustCompile(`(?:\.\.?/)?(?:indexes|slack|google|notion|user-provided|_meta)/[^\s,)>]+\.md`)

// FormatResponse formats a response for Discord. Discord uses standard Markdown natively,
// so this is mostly a pass-through with cleanup of internal references.
func (p *DiscordPlatform) FormatResponse(text string) string {
	// Strip internal .md path references.
	text = reDiscordInternalPath.ReplaceAllString(text, "")

	// Strip any Slack-style internal links that may slip through.
	text = reInternalLink.ReplaceAllString(text, "$1")

	return text
}

// FetchThreadHistory retrieves prior messages from a Discord thread as Gemini messages.
func (p *DiscordPlatform) FetchThreadHistory(ctx context.Context, channel, threadID, excludeMsgID string) []gemini.Message {
	msgs, err := p.session.ChannelMessages(threadID, maxThreadHistory+5, "", "", "")
	if err != nil {
		p.logger.Warn("failed to fetch thread history", "error", err)
		return nil
	}

	// Discord returns messages newest-first; reverse to chronological order.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}

	var history []gemini.Message
	for _, msg := range msgs {
		if msg.ID == excludeMsgID {
			continue
		}
		text := msg.Content
		if text == "" {
			continue
		}

		if msg.Author != nil && msg.Author.ID == p.botUserID {
			history = append(history, gemini.Message{
				Role:    "model",
				Content: text,
			})
		} else {
			// Strip bot mentions from user messages.
			text = strings.ReplaceAll(text, "<@!"+p.botUserID+">", "")
			text = strings.ReplaceAll(text, "<@"+p.botUserID+">", "")
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			text = p.CleanMentions(ctx, text)

			senderName := "unknown"
			if msg.Author != nil {
				senderName = p.ResolveUserName(ctx, msg.Author.ID)
			}
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
func (p *DiscordPlatform) FetchThreadFiles(ctx context.Context, channel, threadID, excludeMsgID string) map[string][]byte {
	files := make(map[string][]byte)

	msgs, err := p.session.ChannelMessages(threadID, 10, "", "", "")
	if err != nil {
		p.logger.Warn("failed to fetch thread files", "error", err)
		return files
	}

	for _, msg := range msgs {
		if msg.ID == excludeMsgID {
			continue
		}
		for _, att := range msg.Attachments {
			url := att.URL
			if url == "" {
				continue
			}
			name := att.Filename
			if name == "" {
				name = att.ID
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

// DownloadFile downloads a file from a URL. Discord CDN URLs are public and do not require auth.
func (p *DiscordPlatform) DownloadFile(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

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
