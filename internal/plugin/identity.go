package plugin

import "context"

// Identity is the person a request is for and where it came from.
type Identity struct {
	Platform  string // "slack" or "discord"
	UserID    string
	UserName  string
	Channel   string
	ThreadID  string // the thread the bot replies in
	MessageID string
	// Text is the message being answered. It is kept on a login_kick action so
	// the bot can run the message again after the person allows the kick.
	Text string
	IsDM bool
}

// Key is the stable per-person key, e.g. "slack:U123".
func (i Identity) Key() string { return i.Platform + ":" + i.UserID }

type identityCtxKey struct{}

// ContextWithIdentity returns a copy of ctx carrying id.
func ContextWithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFromContext returns the Identity in ctx, if any.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(Identity)
	return id, ok
}
