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

	// preloaded holds files already in memory, keyed by name, and rerun marks
	// a stored message the bot runs again. Connectors leave both empty.
	preloaded map[string][]byte
	rerun     bool
}

// FileAttachment represents a file attached to an incoming message.
type FileAttachment struct {
	ID       string
	Name     string
	MimeType string
	URL      string
	Size     int
}

// Button is one button on a prompt.
type Button struct {
	ActionID string // routes the click back to the bot, e.g. "plugin_approve"
	Label    string
	Value    string // the plugin action ID or plugin name the click is about
	Style    string // "", "primary" or "danger"
}

// Prompt is a message with buttons.
type Prompt struct {
	Text    string // never empty
	Buttons []Button
}

// FormField is one input of a form.
type FormField struct {
	ID       string
	Label    string
	Hint     string
	Secret   bool // a password; platforms without masking still never echo it
	Optional bool
	Checkbox bool // submitted as "true" or ""
}

// Form is a modal form a person fills in, opened from a button click.
type Form struct {
	CallbackID string
	Title      string
	Submit     string
	Intro      string // shown above the fields
	Fields     []FormField
	Metadata   string // returned unchanged with the submission
}

// Action is a button click.
type Action struct {
	Platform  string
	UserID    string
	Channel   string // where the clicked message is
	ThreadID  string
	MessageID string // the clicked message
	ActionID  string
	Value     string
	Trigger   string // opens a form in reply to this click (Slack trigger_id, Discord interaction)
}

// FormSubmission is a submitted form. Values are what the person typed,
// passwords included: never log them.
type FormSubmission struct {
	Platform   string
	UserID     string
	CallbackID string
	Metadata   string
	Values     map[string]string
}

// Platform abstracts all platform-specific operations (posting, reactions, user resolution, etc.).
type Platform interface {
	// Messaging
	PostReply(ctx context.Context, channel, threadID, text string) error
	UploadFile(ctx context.Context, channel, threadID, filename string, data []byte) error
	AddReaction(ctx context.Context, channel, messageID, emoji string) error

	// Interactive messages
	PostPrompt(ctx context.Context, channel, threadID string, p Prompt) (messageID string, err error)
	UpdatePrompt(ctx context.Context, channel, messageID, text string) error // replaces the text, removes the buttons
	OpenForm(ctx context.Context, trigger string, f Form) error
	OpenDM(ctx context.Context, userID string) (channel string, err error)

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
// HandleAction and HandleFormSubmission return at once and run their work in
// the background, so connectors call them inline, right after acknowledging.
type BotHandler interface {
	HandleMention(ctx context.Context, msg *IncomingMessage)
	HandleDirectMessage(ctx context.Context, msg *IncomingMessage)
	HandleAction(ctx context.Context, a *Action)
	HandleFormSubmission(ctx context.Context, f *FormSubmission)
	IsTrackedThread(channel, threadID string) bool
}
