package bot

import (
	"context"

	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
)

// IncomingMessage is a platform-agnostic representation of a received message.
type IncomingMessage struct {
	Platform  string // "slack", "discord"
	Channel   string
	ThreadID  string // Slack: thread_ts; Discord: thread/channel ID
	MessageID string // Slack: ts; Discord: snowflake
	UserID    string
	Text      string
	Files     []FileAttachment
	IsDM      bool
}

// FileAttachment represents a file attached to an incoming message.
type FileAttachment struct {
	ID       string
	Name     string
	MimeType string
	URL      string
	Size     int
}

// Platform abstracts all platform-specific operations (posting, reactions, user resolution, etc.).
type Platform interface {
	// Messaging
	PostReply(ctx context.Context, channel, threadID, text string) error
	UploadFile(ctx context.Context, channel, threadID, filename string, data []byte) error
	AddReaction(ctx context.Context, channel, messageID, emoji string) error

	// User resolution
	ResolveUserName(ctx context.Context, userID string) string

	// Thread history
	FetchThreadHistory(ctx context.Context, channel, threadID, excludeMsgID string) []gemini.Message
	FetchThreadFiles(ctx context.Context, channel, threadID, excludeMsgID string) map[string][]byte

	// Thread tracking
	IsTrackedThread(channel, threadID string) bool
	TrackThread(channel, threadID string)

	// File operations
	DownloadFile(ctx context.Context, url string) ([]byte, error)

	// Text processing
	CleanMentions(ctx context.Context, text string) string
	ExtractURLs(text string) []string
	FormatResponse(text string) string
	StripBotMention(text string) string

	// Identity
	Name() string
	BotUserID() string

	// Lifecycle
	StartCleanup()
	StopCleanup()
}

// BotHandler is the interface that platform connectors use to dispatch messages to the bot.
type BotHandler interface {
	HandleMention(ctx context.Context, msg *IncomingMessage)
	HandleDirectMessage(ctx context.Context, msg *IncomingMessage)
	IsTrackedThread(channel, threadID string) bool
}
