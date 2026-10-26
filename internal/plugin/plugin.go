// Package plugin connects out-of-process plugins (groupware, ERP) to the bot.
//
// A Plugin exposes tools. The Registry turns them into model function
// declarations named "<plugin>_<tool>" and dispatches the model's calls:
//
//   - Read and local tools run at once (Mode execute).
//   - Write tools run only in Mode preview from Dispatch. The registry stores a
//     pending Action and queues an approval Prompt; the tool runs in Mode
//     execute only from Approve, which a person triggers by pressing a button.
//     The model has no tool that approves anything.
//   - Tools with a Role (connect, login) are never shown to the model.
//
// Credentials are entered by a person in a form, sealed by the Vault, stored
// encrypted, and opened only to build a CallRequest. They are never logged
// and never appear in what Dispatch returns to the model.
//
// # Prompts and files
//
// Anything the bot must post besides the model's reply is queued on the
// request's Prompts (see ContextWithPrompts) and posted by the bot after the
// reply, so the reply and the buttons arrive in order:
//
//   - PromptConnect: DM the person a connect button (opens the account form).
//   - PromptApproval: post Text and Files (the preview) in the thread with
//     approve and cancel buttons for ActionID.
//   - PromptLoginKick: post Text in the thread with "end the other session"
//     and cancel buttons for ActionID.
//   - PromptNotice: post Files from a read or local tool in the thread.
//
// Files marked Private (certificates) go to the person by DM, never to a
// channel. Files from an approved action come back on the Result returned by
// Approve, and the bot posts them the same way. Every Prompt has non-empty
// Text, so a post never has an empty body.
//
// # Attachments
//
// Files attached to the message being answered are written to
// <DataDir>/inbox/<id>/ and passed to the plugin as absolute paths. They are
// removed after a read or local call, and kept for a pending action until it
// ends (done, failed, cancelled or expired).
package plugin

import (
	"context"
	"log/slog"
)

// Mode tells a plugin whether a write tool may change anything.
type Mode string

const (
	// ModePreview validates and describes a write without performing it.
	ModePreview Mode = "preview"
	// ModeExecute performs the call. Write tools get it only after approval.
	ModeExecute Mode = "execute"
)

// Roles of hidden tools.
const (
	// RoleConnect signs in with the account form values and reports the profile.
	RoleConnect = "connect"
	// RoleLogin signs in with stored credentials, ending another session if allowed.
	RoleLogin = "login"
)

// Tool describes one plugin tool.
type Tool struct {
	Name        string         // without the plugin prefix
	Description string         // shown to the model
	Parameters  map[string]any // sanitized JSON Schema (Gemini subset)
	Write       bool           // gated behind approval
	Role        string         // "" for model tools; RoleConnect or RoleLogin are hidden
}

// AccountField is one input of a plugin's account form, taken from the
// connect tool's input schema.
type AccountField struct {
	Name     string // argument name of the connect tool
	Label    string // form label
	Hint     string // help text under the input
	Secret   bool   // a password: masked, never echoed or logged
	Optional bool
	Checkbox bool // a boolean option; the form value is "true" or ""
}

// Attachment is a file from the user's message, saved where the plugin can read it.
type Attachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Path     string `json:"path"` // absolute
}

// File is a file a plugin returns for the bot to post.
type File struct {
	Name     string
	MimeType string
	Data     []byte
	Private  bool // deliver by DM only
}

// Result is what a plugin call returns.
type Result struct {
	Text  string // Korean, Slack-friendly markdown
	Files []File
	Data  any // structured content
}

// CallRequest is one tool call sent to a plugin.
type CallRequest struct {
	Identity    Identity
	Tool        string // without the plugin prefix
	Args        map[string]any
	Credentials map[string]string // decrypted; never log
	AllowKick   bool              // the person agreed to end another session on login
	Mode        Mode
	Attachments []Attachment
}

// LogValue keeps credentials and arguments out of logs even if a request is
// passed to a logger whole.
func (r CallRequest) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("user", r.Identity.Key()),
		slog.String("tool", r.Tool),
		slog.String("mode", string(r.Mode)),
		slog.Bool("credentials", len(r.Credentials) > 0),
		slog.Bool("allow_kick", r.AllowKick),
		slog.Int("attachments", len(r.Attachments)),
	)
}

// Plugin is one connected system.
type Plugin interface {
	Name() string         // tool prefix, e.g. "daouoffice"
	Title() string        // display name, e.g. "다우오피스"
	Instructions() string // system-prompt fragment
	Tools() []Tool
	// AccountFields lists the account form inputs. Empty means the plugin
	// needs no per-person account.
	AccountFields() []AccountField
	// DataDir is where the registry writes the plugin's inbox. Empty means the
	// plugin accepts no attachments.
	DataDir() string
	Call(ctx context.Context, req CallRequest) (*Result, error)
	Close() error
}
