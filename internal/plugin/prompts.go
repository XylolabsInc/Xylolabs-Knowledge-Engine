package plugin

import (
	"context"
	"sync"
)

// PromptKind says what the bot should post and which buttons to add.
type PromptKind string

const (
	PromptConnect   PromptKind = "connect"    // DM with a connect button
	PromptApproval  PromptKind = "approval"   // thread post with approve and cancel buttons
	PromptLoginKick PromptKind = "login_kick" // thread post with "end the other session" and cancel buttons
	PromptNotice    PromptKind = "notice"     // thread post carrying files, no buttons
)

// Prompt is a deterministic message the bot posts after its reply.
type Prompt struct {
	Kind     PromptKind
	UserKey  string
	Plugin   string
	ActionID string // approval and login_kick
	Text     string // never empty
	Files    []File
}

// Prompts collects the prompts queued while answering one message. Like
// tools.Outbox it lives in the request context, because the registry is
// shared by every concurrent request.
type Prompts struct {
	mu   sync.Mutex
	list []Prompt
}

type promptsCtxKey struct{}

// ContextWithPrompts returns a copy of ctx carrying a fresh Prompts, and the Prompts.
func ContextWithPrompts(ctx context.Context) (context.Context, *Prompts) {
	p := &Prompts{}
	return context.WithValue(ctx, promptsCtxKey{}, p), p
}

// PromptsFromContext returns the Prompts in ctx, or nil.
func PromptsFromContext(ctx context.Context) *Prompts {
	p, _ := ctx.Value(promptsCtxKey{}).(*Prompts)
	return p
}

// List returns the prompts queued so far, in order.
func (p *Prompts) List() []Prompt {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Prompt(nil), p.list...)
}

// add queues a prompt. A second connect prompt for the same person and plugin
// is dropped, so a model that calls several tools of an unconnected plugin in
// one turn sends one DM.
func (p *Prompts) add(pr Prompt) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr.Kind == PromptConnect {
		for _, q := range p.list {
			if q.Kind == PromptConnect && q.UserKey == pr.UserKey && q.Plugin == pr.Plugin {
				return true
			}
		}
	}
	p.list = append(p.list, pr)
	return true
}
