package plugin

import "errors"

// Error codes a plugin reports in structuredContent.code.
const (
	CodeNotConnected     = "NOT_CONNECTED"
	CodeAuthFailed       = "AUTH_FAILED"
	CodeDuplicateSession = "DUPLICATE_SESSION"
	CodeRateLimited      = "RATE_LIMITED"
	CodeValidation       = "VALIDATION"
	CodeNotFound         = "NOT_FOUND"
	CodeConflict         = "CONFLICT"
	CodeUpstream         = "UPSTREAM"
	CodeInternal         = "INTERNAL"
)

// Error is an error a plugin reported about a call. Compare with errors.Is
// against the sentinels below; any *Error with the same Code matches.
type Error struct {
	Code    string
	Message string // Korean, safe to show the user
	// OutcomeUnknown is set when the plugin failed after it had started
	// changing the other system, so the change may or may not have happened.
	OutcomeUnknown bool
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "plugin: " + e.Code
	}
	return "plugin: " + e.Code + ": " + e.Message
}

// Is reports whether target is an *Error with the same code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Sentinels for the codes the registry reacts to.
var (
	ErrNotConnected     = &Error{Code: CodeNotConnected}
	ErrAuthFailed       = &Error{Code: CodeAuthFailed}
	ErrDuplicateSession = &Error{Code: CodeDuplicateSession}
)

// Registry errors.
var (
	ErrNoIdentity     = errors.New("plugin: no user identity in context")
	ErrUnknownPlugin  = errors.New("plugin: unknown plugin")
	ErrNoVault        = errors.New("plugin: PLUGIN_SECRET_KEY is not set, so accounts cannot be stored")
	ErrActionNotFound = errors.New("plugin: action not found")
	ErrNotOwner       = errors.New("plugin: only the person who asked can decide this action")
	ErrNotPending     = errors.New("plugin: action is no longer pending")
	ErrExpired        = errors.New("plugin: action expired")
	// ErrVaultOpen means a stored account no longer decrypts, which points at
	// a changed PLUGIN_SECRET_KEY rather than at the person's password.
	ErrVaultOpen = errors.New("plugin: stored account does not decrypt with PLUGIN_SECRET_KEY")
)
