package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
)

// Store is the persistence the registry needs. *storage.SQLiteStore satisfies it.
type Store interface {
	UpsertPluginAccount(ctx context.Context, a storage.PluginAccount) error
	GetPluginAccount(ctx context.Context, userKey, plugin string) (*storage.PluginAccount, error)
	DeletePluginAccount(ctx context.Context, userKey, plugin string) error
	SetPluginAccountStatus(ctx context.Context, userKey, plugin, status string) error
	ListPluginAccounts(ctx context.Context, userKey string) ([]storage.PluginAccount, error)
	RecordPluginPrompt(ctx context.Context, userKey, plugin string) error
	HasPluginPrompt(ctx context.Context, userKey, plugin string) (bool, error)
	CreatePluginAction(ctx context.Context, a storage.PluginAction) error
	GetPluginAction(ctx context.Context, id string) (*storage.PluginAction, error)
	TransitionPluginAction(ctx context.Context, id, from, to, result string) (bool, error)
	SetPluginActionPrompt(ctx context.Context, id, channel, message string) error
	PendingPluginActionInThread(ctx context.Context, userKey, channel, thread string, now time.Time) (*storage.PluginAction, error)
	ExpirePluginActions(ctx context.Context, now time.Time) ([]storage.PluginAction, error)
	FailStalePluginActions(ctx context.Context, before time.Time, result string) ([]storage.PluginAction, error)
}

// ActionKind is what a pending action does once a person decides.
type ActionKind string

const (
	ActionCall       ActionKind = "call"       // run a write tool in Mode execute
	ActionLoginKick  ActionKind = "login_kick" // sign in, ending another session, then re-run the message
	ActionDisconnect ActionKind = "disconnect" // forget the person's stored account
)

// ActionStatus is where an action is in its life.
type ActionStatus string

const (
	StatusPending   ActionStatus = "pending"
	StatusRunning   ActionStatus = "running"
	StatusDone      ActionStatus = "done"
	StatusFailed    ActionStatus = "failed"
	StatusCancelled ActionStatus = "cancelled"
	StatusExpired   ActionStatus = "expired"
)

// Origin is the message an action came from.
type Origin struct {
	Platform  string `json:"platform"`
	Channel   string `json:"channel"`
	ThreadID  string `json:"thread"`
	MessageID string `json:"message"`
	UserName  string `json:"user_name,omitempty"`
	IsDM      bool   `json:"is_dm,omitempty"`
	Text      string `json:"text,omitempty"` // login_kick only
}

// Action is a plugin call held for a person's decision.
type Action struct {
	ID            string
	Kind          ActionKind
	UserKey       string
	Plugin        string
	Tool          string
	Status        ActionStatus
	Preview       string
	Origin        Origin
	Attachments   []Attachment
	PromptChannel string // where the bot posted the buttons, once recorded
	PromptMessage string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

const (
	accountOK      = "ok"
	accountInvalid = "invalid"

	toolAccountsStatus     = "accounts_status"
	toolAccountsConnect    = "accounts_connect"
	toolAccountsDisconnect = "accounts_disconnect"

	defaultCallTimeout    = 2 * time.Minute
	defaultExecuteTimeout = 5 * time.Minute
	defaultActionTTL      = 30 * time.Minute

	maxModelTextRunes = 20000
	maxModelDataBytes = 64 << 10
	minSecretLen      = 4

	// stateWriteTimeout bounds a status write made after a call, which must
	// land even when the call used up its own deadline.
	stateWriteTimeout = 10 * time.Second
)

var (
	pluginNameRE = regexp.MustCompile(`^[a-z][a-z0-9]{0,30}$`)
	toolNameRE   = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)
)

// Registry owns the registered plugins, gates their write tools behind
// approval, and keeps per-person accounts and pending actions.
type Registry struct {
	store  Store
	vault  *Vault
	logger *slog.Logger

	now            func() time.Time
	newID          func() string
	callTimeout    time.Duration
	executeTimeout time.Duration
	actionTTL      time.Duration

	mu      sync.RWMutex
	plugins []Plugin

	vaultWarn sync.Once
}

// NewRegistry creates a Registry. vault may be nil; plugins that need
// accounts then cannot be registered.
func NewRegistry(store Store, vault *Vault, logger *slog.Logger) *Registry {
	return &Registry{
		store:          store,
		vault:          vault,
		logger:         logger.With("component", "plugin-registry"),
		now:            time.Now,
		newID:          uuid.NewString,
		callTimeout:    defaultCallTimeout,
		executeTimeout: defaultExecuteTimeout,
		actionTTL:      defaultActionTTL,
	}
}

// Register adds a plugin. A plugin that needs accounts is refused when the
// registry has no vault (ErrNoVault); the caller should log why and close it.
func (r *Registry) Register(p Plugin) error {
	name := p.Name()
	if !pluginNameRE.MatchString(name) || name == "accounts" {
		return fmt.Errorf("plugin: register %q: name must be lowercase letters and digits, and not \"accounts\"", name)
	}
	if len(p.AccountFields()) > 0 && r.vault == nil {
		return fmt.Errorf("plugin: register %s: %w", name, ErrNoVault)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range r.plugins {
		if q.Name() == name {
			return fmt.Errorf("plugin: register %s: already registered", name)
		}
	}
	r.plugins = append(r.plugins, p)
	r.logger.Info("plugin registered", "plugin", name, "tools", len(p.Tools()), "accounts", len(p.AccountFields()) > 0)
	return nil
}

// Plugins returns the registered plugins in registration order.
func (r *Registry) Plugins() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.plugins)
}

// AccountPlugins returns the plugins that need a per-person account.
func (r *Registry) AccountPlugins() []Plugin {
	var out []Plugin
	for _, p := range r.Plugins() {
		if len(p.AccountFields()) > 0 {
			out = append(out, p)
		}
	}
	return out
}

func (r *Registry) plugin(name string) Plugin {
	for _, p := range r.Plugins() {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// lookup resolves a model-facing tool name to a plugin and a visible tool.
func (r *Registry) lookup(name string) (Plugin, Tool, bool) {
	for _, p := range r.Plugins() {
		rest, ok := strings.CutPrefix(name, p.Name()+"_")
		if !ok {
			continue
		}
		for _, t := range p.Tools() {
			if t.Name == rest && t.Role == "" {
				return p, t, true
			}
		}
	}
	return nil, Tool{}, false
}

func roleTool(p Plugin, role string) (Tool, bool) {
	for _, t := range p.Tools() {
		if t.Role == role {
			return t, true
		}
	}
	return Tool{}, false
}

// Instructions returns the system-prompt fragment for all plugins, or "".
func (r *Registry) Instructions() string {
	var b strings.Builder
	for _, p := range r.Plugins() {
		s := strings.TrimSpace(p.Instructions())
		if s == "" {
			continue
		}
		fmt.Fprintf(&b, "## %s (%s_* tools)\n%s\n\n", p.Title(), p.Name(), s)
	}
	if b.Len() == 0 {
		return ""
	}
	return "# Connected systems\n" +
		"Tools marked as needing approval only prepare a preview; nothing is submitted until the user presses the button the bot posts. " +
		"Never say something was submitted unless a tool result says so. Never ask for a password in chat.\n\n" +
		strings.TrimSpace(b.String())
}

const writeToolNote = "This tool only prepares a preview: nothing is submitted until the user presses the approval button the bot posts."

// Declarations returns the model-facing tools: "<plugin>_<tool>" for every
// tool without a role, plus the account tools when any plugin needs accounts.
func (r *Registry) Declarations() []gemini.FunctionDeclaration {
	var decls []gemini.FunctionDeclaration
	for _, p := range r.Plugins() {
		for _, t := range p.Tools() {
			if t.Role != "" {
				continue
			}
			name := p.Name() + "_" + t.Name
			if !toolNameRE.MatchString(name) {
				r.logger.Warn("plugin tool skipped: name not allowed by the model API", "plugin", p.Name(), "tool", t.Name)
				continue
			}
			desc := "[" + p.Title() + "] " + strings.TrimSpace(t.Description)
			if t.Write {
				desc = appendSentence(desc, writeToolNote)
			}
			params := t.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			decls = append(decls, gemini.FunctionDeclaration{Name: name, Description: desc, Parameters: params})
		}
	}

	accountPlugins := r.AccountPlugins()
	if len(accountPlugins) == 0 {
		return decls
	}
	var names, titles []string
	for _, p := range accountPlugins {
		names = append(names, p.Name())
		titles = append(titles, p.Name()+" = "+p.Title())
	}
	pluginParam := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plugin": map[string]any{
				"type":        "string",
				"enum":        toAny(names),
				"description": "System to act on (" + strings.Join(titles, ", ") + ")",
			},
		},
		"required": []string{"plugin"},
	}
	return append(decls,
		gemini.FunctionDeclaration{
			Name: toolAccountsStatus,
			Description: "List the external systems (groupware, ERP) the user can connect an account to, " +
				"and whether each is connected. Never returns passwords.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		gemini.FunctionDeclaration{
			Name: toolAccountsConnect,
			Description: "Send the user a DM with a button that opens a form for connecting (or reconnecting) " +
				"their account to the given system. The ID and password go only into that form: never ask " +
				"for them or accept them in chat.",
			Parameters: pluginParam,
		},
		gemini.FunctionDeclaration{
			Name: toolAccountsDisconnect,
			Description: "Ask the user to confirm forgetting their stored account for the given system. It only posts a " +
				"confirmation button; nothing is removed until the user presses it. Use only when the user asks to disconnect.",
			Parameters: pluginParam,
		},
	)
}

// Handles reports whether name is a tool this registry dispatches.
func (r *Registry) Handles(name string) bool {
	switch name {
	case toolAccountsStatus, toolAccountsConnect, toolAccountsDisconnect:
		return len(r.AccountPlugins()) > 0
	}
	_, _, ok := r.lookup(name)
	return ok
}

// call is one model tool call being dispatched.
type call struct {
	p         Plugin
	tool      Tool
	id        Identity
	args      map[string]any
	files     map[string][]byte
	creds     map[string]string
	allowKick bool
	secrets   []string
}

func (c *call) request(mode Mode, atts []Attachment) CallRequest {
	return CallRequest{
		Identity:    c.id,
		Tool:        c.tool.Name,
		Args:        c.args,
		Credentials: c.creds,
		AllowKick:   c.allowKick,
		Mode:        mode,
		Attachments: atts,
	}
}

func (c *call) fullName() string { return c.p.Name() + "_" + c.tool.Name }

// Dispatch runs a model tool call for the person in ctx (ContextWithIdentity)
// and returns what the model sees. Read and local tools run in Mode execute;
// write tools run in Mode preview and leave a pending action. attachments are
// the files of the message being answered. Prompts go to the Prompts in ctx.
func (r *Registry) Dispatch(ctx context.Context, name string, args map[string]any, attachments map[string][]byte) (map[string]any, error) {
	id, ok := IdentityFromContext(ctx)
	if !ok || id.Platform == "" || id.UserID == "" {
		return nil, ErrNoIdentity
	}
	if args == nil {
		args = map[string]any{}
	}
	switch name {
	case toolAccountsStatus:
		return r.accountsStatus(ctx, id)
	case toolAccountsConnect:
		return r.accountsConnect(ctx, id, args)
	case toolAccountsDisconnect:
		return r.accountsDisconnect(ctx, id, args)
	}

	p, tool, ok := r.lookup(name)
	if !ok {
		return nil, fmt.Errorf("plugin: unknown tool %s", name)
	}
	DecodeJSONArgs(args, tool.JSONArgs)
	c := &call{p: p, tool: tool, id: id, args: args, files: attachments}
	if len(p.AccountFields()) > 0 {
		acct, creds, err := r.openAccount(ctx, id.Key(), p)
		if err != nil {
			return r.callFailed(ctx, c, err)
		}
		c.creds, c.allowKick, c.secrets = creds, acct.AllowKick, secretValues(p, creds)
	}
	if tool.Write {
		return r.preview(ctx, c)
	}
	return r.run(ctx, c)
}

// run calls a read or local tool and returns its result.
func (r *Registry) run(ctx context.Context, c *call) (map[string]any, error) {
	reqID := r.newID()
	atts, err := writeInbox(c.p, reqID, c.files)
	if err != nil {
		return nil, err
	}
	defer r.removeInbox(c.p, reqID)

	cctx, cancel := context.WithTimeout(ctx, r.callTimeout)
	defer cancel()
	res, err := c.p.Call(cctx, c.request(ModeExecute, atts))
	if err != nil {
		return r.callFailed(ctx, c, err)
	}
	out := modelResult(res, c.secrets)
	if res != nil && len(res.Files) > 0 {
		r.queuePrompt(ctx, Prompt{
			Kind: PromptNotice, UserKey: c.id.Key(), Plugin: c.p.Name(),
			Text: fmt.Sprintf("%s에서 받은 파일입니다.", c.p.Title()), Files: res.Files,
		})
		out["files"] = fileNames(res.Files)
		out["files_message"] = "These files will be posted to the conversation after your reply (private ones by DM). Mention them briefly; do not describe their bytes."
	}
	return out, nil
}

// preview calls a write tool in Mode preview, stores a pending action and
// queues its approval prompt. It never calls Mode execute.
func (r *Registry) preview(ctx context.Context, c *call) (map[string]any, error) {
	actionID := r.newID()
	atts, err := writeInbox(c.p, actionID, c.files)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			r.removeInbox(c.p, actionID)
		}
	}()

	cctx, cancel := context.WithTimeout(ctx, r.callTimeout)
	defer cancel()
	res, err := c.p.Call(cctx, c.request(ModePreview, atts))
	if err != nil {
		return r.callFailed(ctx, c, err)
	}
	if res == nil {
		res = &Result{}
	}

	preview := strings.TrimSpace(redact(res.Text, c.secrets))
	if preview == "" {
		preview = fmt.Sprintf("%s: %s", c.p.Title(), c.tool.Name)
	}
	now := r.now().UTC()
	row := storage.PluginAction{
		ID:          actionID,
		Kind:        string(ActionCall),
		UserKey:     c.id.Key(),
		Plugin:      c.p.Name(),
		Tool:        c.tool.Name,
		Args:        mustJSON(c.args, "{}"),
		Attachments: mustJSON(atts, "[]"),
		Origin:      mustJSON(originOf(c.id, false), "{}"),
		Channel:     c.id.Channel,
		Thread:      c.id.ThreadID,
		Preview:     preview,
		Status:      string(StatusPending),
		CreatedAt:   now,
		ExpiresAt:   now.Add(r.actionTTL),
	}
	if err := r.store.CreatePluginAction(ctx, row); err != nil {
		return nil, fmt.Errorf("plugin: %s: store pending action: %w", c.fullName(), err)
	}
	keep = true

	r.queuePrompt(ctx, Prompt{
		Kind: PromptApproval, UserKey: c.id.Key(), Plugin: c.p.Name(), ActionID: actionID,
		Text:  preview + "\n\n" + fmt.Sprintf("버튼을 누르기 전에는 아무것도 제출되지 않습니다. %d분이 지나면 자동으로 취소됩니다.", int(r.actionTTL.Minutes())),
		Files: res.Files,
	})
	r.logger.Info("plugin action awaiting approval", "plugin", c.p.Name(), "tool", c.tool.Name, "action", actionID, "user", c.id.Key())

	text, truncated := truncateRunes(preview, maxModelTextRunes)
	out := map[string]any{
		"status":  "awaiting_approval",
		"preview": text,
		"message": "Nothing has been submitted. The preview was posted with approve and cancel buttons, and it is submitted only " +
			"when the user presses the approve button (or replies 승인 in this thread). Tell the user to check the preview " +
			"and press the button. Do not say it was submitted, and do not call this tool again for the same request.",
	}
	if truncated {
		out["preview_truncated"] = true
	}
	addData(out, res.Data, c.secrets)
	return out, nil
}

// callFailed turns a plugin error into what the model sees, queueing the
// prompts the error calls for.
func (r *Registry) callFailed(ctx context.Context, c *call, err error) (map[string]any, error) {
	err = redactErr(err, c.secrets)
	title := c.p.Title()
	switch {
	case errors.Is(err, ErrVaultOpen):
		return map[string]any{
			"status":  "error",
			"code":    "SERVER_CONFIG",
			"message": "서버 설정 문제로 계정을 읽지 못했습니다. 관리자에게 알려 주세요.",
		}, nil
	case errors.Is(err, ErrNotConnected):
		r.queueConnect(ctx, c.id, c.p, fmt.Sprintf("%s 계정이 아직 연결되지 않았습니다. 버튼을 누르면 뜨는 입력 창에 아이디와 비밀번호를 넣어 주세요. 비밀번호는 채팅에 쓰지 마세요.", title))
		return map[string]any{
			"status": "not_connected",
			"message": fmt.Sprintf("The user has not connected a %s account. A DM with a connect button was sent; they enter their ID "+
				"and password in the form it opens. Tell them to check the DM. Never ask for the password in chat.", title),
		}, nil
	case errors.Is(err, ErrAuthFailed):
		r.markInvalid(ctx, c.id.Key(), c.p)
		r.queueConnect(ctx, c.id, c.p, fmt.Sprintf("저장된 %s 비밀번호로 로그인하지 못했습니다. 비밀번호를 바꿨다면 다시 연결해 주세요.", title))
		return map[string]any{
			"status": "auth_failed",
			"message": fmt.Sprintf("%s rejected the stored ID or password, so the account was marked invalid and a DM with a "+
				"reconnect button was sent. Tell the user; do not retry.", title),
		}, nil
	case errors.Is(err, ErrDuplicateSession):
		return r.holdForKick(ctx, c, err)
	}

	var pe *Error
	if errors.As(err, &pe) {
		return map[string]any{"status": "error", "code": pe.Code, "message": redact(pe.Message, c.secrets)}, nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("plugin: %s: timed out: %w", c.fullName(), err)
	}
	return nil, fmt.Errorf("plugin: %s: %w", c.fullName(), err)
}

// holdForKick stores a login_kick action for a call that hit another live
// session, and queues the prompt asking whether to end that session.
func (r *Registry) holdForKick(ctx context.Context, c *call, cause error) (map[string]any, error) {
	if _, ok := roleTool(c.p, RoleLogin); !ok {
		msg := ""
		if pe := (*Error)(nil); errors.As(cause, &pe) {
			msg = pe.Message
		}
		return map[string]any{"status": "error", "code": CodeDuplicateSession, "message": redact(msg, c.secrets)}, nil
	}
	actionID := r.newID()
	atts, err := writeInbox(c.p, actionID, c.files)
	if err != nil {
		return nil, err
	}
	now := r.now().UTC()
	row := storage.PluginAction{
		ID:          actionID,
		Kind:        string(ActionLoginKick),
		UserKey:     c.id.Key(),
		Plugin:      c.p.Name(),
		Tool:        c.tool.Name,
		Args:        mustJSON(c.args, "{}"),
		Attachments: mustJSON(atts, "[]"),
		Origin:      mustJSON(originOf(c.id, true), "{}"),
		Channel:     c.id.Channel,
		Thread:      c.id.ThreadID,
		Status:      string(StatusPending),
		CreatedAt:   now,
		ExpiresAt:   now.Add(r.actionTTL),
	}
	if err := r.store.CreatePluginAction(ctx, row); err != nil {
		r.removeInbox(c.p, actionID)
		return nil, fmt.Errorf("plugin: %s: store login_kick action: %w", c.fullName(), err)
	}
	r.queuePrompt(ctx, Prompt{
		Kind: PromptLoginKick, UserKey: c.id.Key(), Plugin: c.p.Name(), ActionID: actionID,
		Text: fmt.Sprintf("다른 곳에서 %s에 접속 중입니다. 봇이 로그인하면 그 접속이 끊깁니다. 계속할까요?", c.p.Title()),
	})
	return map[string]any{
		"status": "duplicate_session",
		"message": fmt.Sprintf("The user's %s account is signed in somewhere else, and signing in would end that session. "+
			"A message with buttons was posted asking whether to go ahead; nothing was done. Tell the user briefly and do not retry.", c.p.Title()),
	}, nil
}

// Approve runs a pending call action in Mode execute. Only the owner may
// approve, only once, and only before it expires: the status flips from
// pending to running in one conditional update, so a double click runs once.
// The call is not cancelled with ctx, because stopping a submit halfway
// leaves its outcome unknown.
//
// When the plugin fails before doing anything (not connected, password
// rejected, another live session) the action goes back to pending and the
// typed error is returned, so the person can fix that and approve again.
func (r *Registry) Approve(ctx context.Context, actionID string, by Identity) (*Result, *Action, error) {
	row, act, err := r.ownedAction(ctx, actionID, by)
	if err != nil {
		return nil, act, err
	}
	p := r.plugin(act.Plugin)
	if p == nil {
		return nil, act, fmt.Errorf("plugin: action %s: %w: %s", actionID, ErrUnknownPlugin, act.Plugin)
	}
	switch act.Kind {
	case ActionCall:
	case ActionDisconnect:
		return r.approveDisconnect(ctx, p, act)
	default:
		return nil, act, fmt.Errorf("plugin: action %s is %s, not a call", actionID, act.Kind)
	}
	if err := r.claim(ctx, p, act); err != nil {
		return nil, act, err
	}

	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.executeTimeout)
	defer cancel()

	req := CallRequest{Identity: identityFor(by, act.Origin), Tool: act.Tool, Mode: ModeExecute, Attachments: act.Attachments}
	if err := json.Unmarshal([]byte(row.Args), &req.Args); err != nil {
		r.finish(ctx, p, act, StatusFailed, mustJSON(map[string]string{"error": "stored arguments are unreadable"}, ""))
		return nil, act, fmt.Errorf("plugin: action %s: decode args: %w", actionID, err)
	}
	var secrets []string
	if len(p.AccountFields()) > 0 {
		acct, creds, err := r.openAccount(ectx, act.UserKey, p)
		if err != nil {
			r.reopen(ctx, act)
			return nil, act, err
		}
		req.Credentials, req.AllowKick, secrets = creds, acct.AllowKick, secretValues(p, creds)
	}

	r.logger.Info("plugin action approved", "plugin", p.Name(), "tool", act.Tool, "action", actionID, "user", act.UserKey)
	res, err := p.Call(ectx, req)
	if err != nil {
		if errors.Is(err, ErrNotConnected) || errors.Is(err, ErrAuthFailed) || errors.Is(err, ErrDuplicateSession) {
			if errors.Is(err, ErrAuthFailed) {
				r.markInvalid(ctx, act.UserKey, p)
			}
			r.reopen(ctx, act)
			return nil, act, redactErr(err, secrets)
		}
		err = redactErr(err, secrets)
		msg := err.Error()
		var pe *Error
		if errors.As(err, &pe) {
			msg = pe.Message
		}
		r.finish(ctx, p, act, StatusFailed, mustJSON(map[string]string{"error": msg}, ""))
		r.logger.Warn("plugin action failed", "plugin", p.Name(), "tool", act.Tool, "action", actionID, "error", err)
		return nil, act, fmt.Errorf("plugin: action %s: %w", actionID, err)
	}
	if res == nil {
		res = &Result{}
	}
	res.Text = redact(res.Text, secrets)
	stored := map[string]any{"text": res.Text}
	addData(stored, res.Data, secrets)
	r.finish(ctx, p, act, StatusDone, mustJSON(stored, ""))
	r.logger.Info("plugin action done", "plugin", p.Name(), "tool", act.Tool, "action", actionID, "files", len(res.Files))
	return res, act, nil
}

// approveDisconnect forgets the owner's account after they confirmed it.
func (r *Registry) approveDisconnect(ctx context.Context, p Plugin, act *Action) (*Result, *Action, error) {
	if err := r.claim(ctx, p, act); err != nil {
		return nil, act, err
	}
	sctx, cancel := stateCtx(ctx)
	defer cancel()
	if err := r.store.DeletePluginAccount(sctx, act.UserKey, act.Plugin); err != nil {
		r.finish(ctx, p, act, StatusFailed, mustJSON(map[string]string{"error": "could not delete the account"}, ""))
		return nil, act, fmt.Errorf("plugin: disconnect %s: %w", act.Plugin, err)
	}
	r.finish(ctx, p, act, StatusDone, "")
	r.logger.Info("plugin account disconnected", "plugin", act.Plugin, "user", act.UserKey)
	return &Result{Text: fmt.Sprintf("%s 계정 연결을 해제했습니다. 저장된 아이디와 비밀번호를 지웠습니다.", p.Title())}, act, nil
}

// LoginAndResume signs the owner in with permission to end their other
// session.
//
// For a login_kick action it ends the action and returns the original
// message's attachments; the bot then runs Origin.Text again. For a pending
// call action whose Approve returned ErrDuplicateSession it leaves the action
// pending, so the bot can call Approve again on the now live session.
func (r *Registry) LoginAndResume(ctx context.Context, actionID string, by Identity) (*Action, map[string][]byte, error) {
	_, act, err := r.ownedAction(ctx, actionID, by)
	if err != nil {
		return nil, nil, err
	}
	p := r.plugin(act.Plugin)
	if p == nil {
		return act, nil, fmt.Errorf("plugin: action %s: %w: %s", actionID, ErrUnknownPlugin, act.Plugin)
	}
	// Once claimed, the action must reach done or failed even if ctx ends.
	ctx = context.WithoutCancel(ctx)

	if act.Kind == ActionCall {
		if act.Status != StatusPending {
			return act, nil, ErrNotPending
		}
		if !r.now().Before(act.ExpiresAt) {
			r.expire(ctx, p, act)
			return act, nil, ErrExpired
		}
		return act, nil, r.login(ctx, act.UserKey, by, p)
	}

	if err := r.claim(ctx, p, act); err != nil {
		return act, nil, err
	}
	if err := r.login(ctx, act.UserKey, by, p); err != nil {
		r.finish(ctx, p, act, StatusFailed, mustJSON(map[string]string{"error": err.Error()}, ""))
		return act, nil, err
	}
	files, err := readInbox(act.Attachments)
	if err != nil {
		r.logger.Warn("plugin: could not reload attachments for resume", "action", actionID, "error", err)
	}
	r.finish(ctx, p, act, StatusDone, "")
	return act, files, nil
}

// login calls the plugin's hidden login tool with AllowKick.
func (r *Registry) login(ctx context.Context, userKey string, by Identity, p Plugin) error {
	tool, ok := roleTool(p, RoleLogin)
	if !ok {
		return fmt.Errorf("plugin: %s has no login tool", p.Name())
	}
	_, creds, err := r.openAccount(ctx, userKey, p)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.callTimeout)
	defer cancel()
	_, err = p.Call(cctx, CallRequest{
		Identity: by, Tool: tool.Name, Args: map[string]any{}, Credentials: creds, AllowKick: true, Mode: ModeExecute,
	})
	if errors.Is(err, ErrAuthFailed) {
		r.markInvalid(ctx, userKey, p)
	}
	if err != nil {
		return redactErr(fmt.Errorf("plugin: %s: login: %w", p.Name(), err), secretValues(p, creds))
	}
	r.logger.Info("plugin login with kick", "plugin", p.Name(), "user", userKey)
	return nil
}

// Cancel cancels a pending action. Only the owner may cancel.
func (r *Registry) Cancel(ctx context.Context, actionID string, by Identity) (*Action, error) {
	_, act, err := r.ownedAction(ctx, actionID, by)
	if err != nil {
		return act, err
	}
	ok, err := r.store.TransitionPluginAction(ctx, actionID, string(StatusPending), string(StatusCancelled), "")
	if err != nil {
		return act, fmt.Errorf("plugin: cancel %s: %w", actionID, err)
	}
	if !ok {
		return act, ErrNotPending
	}
	act.Status = StatusCancelled
	if p := r.plugin(act.Plugin); p != nil {
		r.removeInbox(p, actionID)
	}
	return act, nil
}

// Action returns an action by ID without changing it, or ErrActionNotFound.
// The bot uses it to check the owner before it updates a prompt.
func (r *Registry) Action(ctx context.Context, actionID string) (*Action, error) {
	row, err := r.store.GetPluginAction(ctx, actionID)
	if err != nil {
		return nil, fmt.Errorf("plugin: load action %s: %w", actionID, err)
	}
	if row == nil {
		return nil, ErrActionNotFound
	}
	return decodeAction(row), nil
}

// RecoverStale marks actions that have been running for longer than
// olderThan as failed and returns them, so the bot can tell their owners
// that the outcome is unknown. olderThan must exceed the execute timeout.
func (r *Registry) RecoverStale(ctx context.Context, olderThan time.Duration) ([]Action, error) {
	rows, err := r.store.FailStalePluginActions(ctx, r.now().Add(-olderThan),
		mustJSON(map[string]string{"error": "the bot stopped while this ran; the outcome is unknown"}, ""))
	if err != nil {
		return nil, fmt.Errorf("plugin: recover stale actions: %w", err)
	}
	actions := make([]Action, 0, len(rows))
	for i := range rows {
		act := decodeAction(&rows[i])
		if p := r.plugin(act.Plugin); p != nil {
			r.removeInbox(p, act.ID)
		}
		r.logger.Warn("plugin action was left running; marked failed", "plugin", act.Plugin, "tool", act.Tool, "action", act.ID, "user", act.UserKey)
		actions = append(actions, *act)
	}
	return actions, nil
}

// PendingInThread returns the person's newest pending action in their
// current thread (by.Channel, by.ThreadID), or nil. The bot uses it for the
// "승인" / "취소" text fallback.
func (r *Registry) PendingInThread(ctx context.Context, by Identity) (*Action, error) {
	row, err := r.store.PendingPluginActionInThread(ctx, by.Key(), by.Channel, by.ThreadID, r.now())
	if err != nil {
		return nil, fmt.Errorf("plugin: pending action in thread: %w", err)
	}
	if row == nil {
		return nil, nil
	}
	return decodeAction(row), nil
}

// SetActionPrompt records where the bot posted an action's buttons, so it
// can update that message later (for example when the action expires).
func (r *Registry) SetActionPrompt(ctx context.Context, actionID, channel, messageID string) error {
	if err := r.store.SetPluginActionPrompt(ctx, actionID, channel, messageID); err != nil {
		return fmt.Errorf("plugin: set action prompt: %w", err)
	}
	return nil
}

// Expire marks overdue pending actions expired, removes their attachments,
// and returns them so the bot can update their prompts. Call it periodically.
func (r *Registry) Expire(ctx context.Context) ([]Action, error) {
	rows, err := r.store.ExpirePluginActions(ctx, r.now())
	if err != nil {
		return nil, fmt.Errorf("plugin: expire actions: %w", err)
	}
	actions := make([]Action, 0, len(rows))
	for i := range rows {
		act := decodeAction(&rows[i])
		if p := r.plugin(act.Plugin); p != nil {
			r.removeInbox(p, act.ID)
		}
		actions = append(actions, *act)
	}
	return actions, nil
}

// Connect checks account form values with the plugin's hidden connect tool
// and, if they work, stores them sealed. It returns the account's display
// name. A plugin that finds another live session after the password was
// accepted reports ErrDuplicateSession; that still counts as verified, and
// nobody is signed out. values are never logged.
func (r *Registry) Connect(ctx context.Context, id Identity, pluginName string, values map[string]string) (string, error) {
	p := r.plugin(pluginName)
	if p == nil {
		return "", fmt.Errorf("plugin: connect %s: %w", pluginName, ErrUnknownPlugin)
	}
	if r.vault == nil {
		return "", ErrNoVault
	}
	fields := p.AccountFields()
	tool, ok := roleTool(p, RoleConnect)
	if len(fields) == 0 || !ok {
		return "", fmt.Errorf("plugin: connect %s: plugin has no account form", pluginName)
	}

	args := map[string]any{}
	creds := map[string]string{}
	allowKick := false
	for _, f := range fields {
		v := values[f.Name]
		if f.Checkbox {
			on := isTrue(v)
			args[f.Name] = on
			if f.Name == "allowKick" {
				allowKick = on
			}
			continue
		}
		if !f.Secret {
			v = strings.TrimSpace(v)
		}
		if v == "" {
			if !f.Optional {
				return "", &Error{Code: CodeValidation, Message: fmt.Sprintf("%s 칸을 채워 주세요.", f.Label)}
			}
			continue
		}
		args[f.Name] = v
		creds[f.Name] = v
	}

	secrets := secretValues(p, creds)
	cctx, cancel := context.WithTimeout(ctx, r.callTimeout)
	defer cancel()
	res, err := p.Call(cctx, CallRequest{Identity: id, Tool: tool.Name, Args: args, Credentials: creds, Mode: ModeExecute})
	if err != nil && !errors.Is(err, ErrDuplicateSession) {
		var pe *Error
		if errors.As(err, &pe) {
			return "", redactErr(pe, secrets)
		}
		return "", redactErr(fmt.Errorf("plugin: %s: connect: %w", pluginName, err), secrets)
	}
	name, accountID := profileOf(res, creds)

	sealed, err := r.vault.Seal(creds, bindKey(id.Key(), pluginName))
	if err != nil {
		return "", err
	}
	if err := r.store.UpsertPluginAccount(ctx, storage.PluginAccount{
		UserKey: id.Key(), Plugin: pluginName, Secret: sealed, AccountName: name, AccountID: accountID,
		AllowKick: allowKick, Status: accountOK, ConnectedAt: r.now().UTC(),
	}); err != nil {
		return "", fmt.Errorf("plugin: %s: store account: %w", pluginName, err)
	}
	if err := r.store.RecordPluginPrompt(ctx, id.Key(), pluginName); err != nil {
		r.logger.Warn("plugin: record prompt after connect", "plugin", pluginName, "error", err)
	}
	r.logger.Info("plugin account connected", "plugin", pluginName, "user", id.Key(), "allow_kick", allowKick)
	return name, nil
}

// NeedsConnectPrompt returns a first-contact connect prompt for every account
// plugin the person has neither connected nor been asked about. The bot posts
// them by DM and then calls MarkPrompted.
func (r *Registry) NeedsConnectPrompt(ctx context.Context, userKey string) ([]Prompt, error) {
	var prompts []Prompt
	for _, p := range r.AccountPlugins() {
		acct, err := r.store.GetPluginAccount(ctx, userKey, p.Name())
		if err != nil {
			return nil, fmt.Errorf("plugin: needs connect prompt: %w", err)
		}
		if acct != nil {
			continue
		}
		asked, err := r.store.HasPluginPrompt(ctx, userKey, p.Name())
		if err != nil {
			return nil, fmt.Errorf("plugin: needs connect prompt: %w", err)
		}
		if asked {
			continue
		}
		prompts = append(prompts, Prompt{
			Kind: PromptConnect, UserKey: userKey, Plugin: p.Name(),
			Text: fmt.Sprintf("%s 계정을 연결하면 여기서 바로 조회하고 처리할 수 있습니다. 버튼을 누르면 뜨는 입력 창에 아이디와 비밀번호를 넣어 주세요. 비밀번호는 채팅에 쓰지 마세요.", p.Title()),
		})
	}
	return prompts, nil
}

// MarkPrompted records that the person was asked to connect a plugin.
func (r *Registry) MarkPrompted(ctx context.Context, userKey, pluginName string) error {
	if err := r.store.RecordPluginPrompt(ctx, userKey, pluginName); err != nil {
		return fmt.Errorf("plugin: mark prompted: %w", err)
	}
	return nil
}

// Close closes every plugin.
func (r *Registry) Close() error {
	var errs []error
	for _, p := range r.Plugins() {
		if err := p.Close(); err != nil {
			errs = append(errs, fmt.Errorf("plugin: close %s: %w", p.Name(), err))
		}
	}
	return errors.Join(errs...)
}

func (r *Registry) accountsStatus(ctx context.Context, id Identity) (map[string]any, error) {
	accounts, err := r.store.ListPluginAccounts(ctx, id.Key())
	if err != nil {
		return nil, fmt.Errorf("plugin: accounts status: %w", err)
	}
	byPlugin := map[string]storage.PluginAccount{}
	for _, a := range accounts {
		byPlugin[a.Plugin] = a
	}
	list := []map[string]any{}
	for _, p := range r.AccountPlugins() {
		entry := map[string]any{"plugin": p.Name(), "title": p.Title(), "connected": false}
		if a, ok := byPlugin[p.Name()]; ok {
			entry["connected"] = a.Status == accountOK
			entry["status"] = a.Status
			entry["account"] = a.AccountName
			entry["connected_at"] = a.ConnectedAt.Format("2006-01-02 15:04")
		}
		list = append(list, entry)
	}
	return map[string]any{
		"accounts": list,
		"message":  "To connect or reconnect an account, call accounts_connect. Never ask for a password in chat.",
	}, nil
}

func (r *Registry) accountsConnect(ctx context.Context, id Identity, args map[string]any) (map[string]any, error) {
	p, err := r.accountPluginArg(args)
	if err != nil {
		return nil, err
	}
	r.queueConnect(ctx, id, p, fmt.Sprintf("아래 버튼을 눌러 %s 계정을 연결해 주세요. 아이디와 비밀번호는 버튼을 누르면 뜨는 입력 창에만 넣어 주세요.", p.Title()))
	return map[string]any{
		"status": "connect_prompt_sent",
		"message": fmt.Sprintf("A DM with a button for connecting the %s account was sent. Tell the user to open it; "+
			"they enter the ID and password in the form, never in chat.", p.Title()),
	}, nil
}

// accountsDisconnect queues a confirmation instead of deleting: the request
// can come from text the model read (a document, a forwarded message), so
// only the person's button press removes the account.
func (r *Registry) accountsDisconnect(ctx context.Context, id Identity, args map[string]any) (map[string]any, error) {
	p, err := r.accountPluginArg(args)
	if err != nil {
		return nil, err
	}
	acct, err := r.store.GetPluginAccount(ctx, id.Key(), p.Name())
	if err != nil {
		return nil, fmt.Errorf("plugin: disconnect %s: load account: %w", p.Name(), err)
	}
	if acct == nil {
		return map[string]any{
			"status":  "not_connected",
			"message": fmt.Sprintf("The user has no connected %s account, so there is nothing to disconnect.", p.Title()),
		}, nil
	}
	actionID := r.newID()
	now := r.now().UTC()
	preview := fmt.Sprintf("%s 계정(%s) 연결을 해제할까요? 해제하면 저장된 아이디와 비밀번호를 지웁니다.", p.Title(), acct.AccountName)
	if err := r.store.CreatePluginAction(ctx, storage.PluginAction{
		ID:        actionID,
		Kind:      string(ActionDisconnect),
		UserKey:   id.Key(),
		Plugin:    p.Name(),
		Args:      "{}",
		Origin:    mustJSON(originOf(id, false), "{}"),
		Channel:   id.Channel,
		Thread:    id.ThreadID,
		Preview:   preview,
		Status:    string(StatusPending),
		CreatedAt: now,
		ExpiresAt: now.Add(r.actionTTL),
	}); err != nil {
		return nil, fmt.Errorf("plugin: disconnect %s: store action: %w", p.Name(), err)
	}
	r.queuePrompt(ctx, Prompt{Kind: PromptDisconnect, UserKey: id.Key(), Plugin: p.Name(), ActionID: actionID, Text: preview})
	return map[string]any{
		"status": "awaiting_confirmation",
		"message": fmt.Sprintf("Nothing was changed. A message with buttons was posted; the %s account is disconnected "+
			"only if the user presses the disconnect button. Tell the user briefly.", p.Title()),
	}, nil
}

// accountPluginArg reads the "plugin" argument, defaulting to the only
// account plugin when there is just one.
func (r *Registry) accountPluginArg(args map[string]any) (Plugin, error) {
	plugins := r.AccountPlugins()
	name, _ := args["plugin"].(string)
	if name == "" && len(plugins) == 1 {
		return plugins[0], nil
	}
	for _, p := range plugins {
		if p.Name() == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("plugin: %w: %q", ErrUnknownPlugin, name)
}

// openAccount loads and decrypts a person's account. It returns
// ErrNotConnected when there is none and ErrAuthFailed when it is marked
// invalid or no longer opens.
func (r *Registry) openAccount(ctx context.Context, userKey string, p Plugin) (*storage.PluginAccount, map[string]string, error) {
	acct, err := r.store.GetPluginAccount(ctx, userKey, p.Name())
	if err != nil {
		return nil, nil, fmt.Errorf("plugin: %s: load account: %w", p.Name(), err)
	}
	if acct == nil {
		return nil, nil, ErrNotConnected
	}
	if acct.Status == accountInvalid {
		return nil, nil, ErrAuthFailed
	}
	if r.vault == nil {
		return nil, nil, ErrNoVault
	}
	creds, err := r.vault.Open(acct.Secret, bindKey(userKey, p.Name()))
	if err != nil {
		// Not the person's fault: every account fails the same way when the key
		// changed, so say it once, loudly, and leave the accounts as they are.
		r.vaultWarn.Do(func() {
			r.logger.Error("stored plugin accounts do not decrypt; was PLUGIN_SECRET_KEY changed? Accounts are left as they are",
				"plugin", p.Name(), "error", err)
		})
		r.logger.Debug("plugin account does not decrypt", "plugin", p.Name(), "user", userKey)
		return nil, nil, ErrVaultOpen
	}
	return acct, creds, nil
}

func (r *Registry) markInvalid(ctx context.Context, userKey string, p Plugin) {
	ctx, cancel := stateCtx(ctx)
	defer cancel()
	if err := r.store.SetPluginAccountStatus(ctx, userKey, p.Name(), accountInvalid); err != nil {
		r.logger.Warn("plugin: mark account invalid", "plugin", p.Name(), "user", userKey, "error", err)
	}
}

func (r *Registry) queueConnect(ctx context.Context, id Identity, p Plugin, text string) {
	r.queuePrompt(ctx, Prompt{Kind: PromptConnect, UserKey: id.Key(), Plugin: p.Name(), Text: text})
	if err := r.store.RecordPluginPrompt(ctx, id.Key(), p.Name()); err != nil {
		r.logger.Warn("plugin: record connect prompt", "plugin", p.Name(), "user", id.Key(), "error", err)
	}
}

func (r *Registry) queuePrompt(ctx context.Context, pr Prompt) {
	if !PromptsFromContext(ctx).add(pr) {
		r.logger.Warn("plugin prompt dropped: no prompt outbox in context", "kind", pr.Kind, "plugin", pr.Plugin, "user", pr.UserKey)
	}
}

// ownedAction loads an action and checks that by owns it.
func (r *Registry) ownedAction(ctx context.Context, actionID string, by Identity) (*storage.PluginAction, *Action, error) {
	row, err := r.store.GetPluginAction(ctx, actionID)
	if err != nil {
		return nil, nil, fmt.Errorf("plugin: load action %s: %w", actionID, err)
	}
	if row == nil {
		return nil, nil, ErrActionNotFound
	}
	act := decodeAction(row)
	if by.Key() != row.UserKey {
		r.logger.Warn("plugin action decision refused: not the owner", "action", actionID, "by", by.Key())
		return row, act, ErrNotOwner
	}
	return row, act, nil
}

// claim moves a pending, unexpired action to running, or says why it cannot.
func (r *Registry) claim(ctx context.Context, p Plugin, act *Action) error {
	if act.Status != StatusPending {
		return ErrNotPending
	}
	if !r.now().Before(act.ExpiresAt) {
		r.expire(ctx, p, act)
		return ErrExpired
	}
	ok, err := r.store.TransitionPluginAction(ctx, act.ID, string(StatusPending), string(StatusRunning), "")
	if err != nil {
		return fmt.Errorf("plugin: claim action %s: %w", act.ID, err)
	}
	if !ok {
		return ErrNotPending
	}
	act.Status = StatusRunning
	return nil
}

func (r *Registry) expire(ctx context.Context, p Plugin, act *Action) {
	ctx, cancel := stateCtx(ctx)
	defer cancel()
	if ok, err := r.store.TransitionPluginAction(ctx, act.ID, string(StatusPending), string(StatusExpired), ""); err != nil {
		r.logger.Warn("plugin: expire action", "action", act.ID, "error", err)
	} else if ok {
		act.Status = StatusExpired
		r.removeInbox(p, act.ID)
	}
}

// reopen puts a running action back to pending after a failure that happened
// before the plugin did anything.
func (r *Registry) reopen(ctx context.Context, act *Action) {
	ctx, cancel := stateCtx(ctx)
	defer cancel()
	if _, err := r.store.TransitionPluginAction(ctx, act.ID, string(StatusRunning), string(StatusPending), ""); err != nil {
		r.logger.Warn("plugin: reopen action", "action", act.ID, "error", err)
		return
	}
	act.Status = StatusPending
}

// finish ends a running action and removes its attachments.
func (r *Registry) finish(ctx context.Context, p Plugin, act *Action, status ActionStatus, result string) {
	ctx, cancel := stateCtx(ctx)
	defer cancel()
	if _, err := r.store.TransitionPluginAction(ctx, act.ID, string(StatusRunning), string(status), result); err != nil {
		r.logger.Warn("plugin: finish action", "action", act.ID, "status", status, "error", err)
	}
	act.Status = status
	r.removeInbox(p, act.ID)
}

func (r *Registry) removeInbox(p Plugin, id string) {
	dir := inboxDir(p, id)
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		r.logger.Warn("plugin: remove inbox", "plugin", p.Name(), "dir", dir, "error", err)
	}
}

func decodeAction(row *storage.PluginAction) *Action {
	act := &Action{
		ID:            row.ID,
		Kind:          ActionKind(row.Kind),
		UserKey:       row.UserKey,
		Plugin:        row.Plugin,
		Tool:          row.Tool,
		Status:        ActionStatus(row.Status),
		Preview:       row.Preview,
		PromptChannel: row.PromptChannel,
		PromptMessage: row.PromptMessage,
		CreatedAt:     row.CreatedAt,
		ExpiresAt:     row.ExpiresAt,
	}
	_ = json.Unmarshal([]byte(row.Origin), &act.Origin)
	_ = json.Unmarshal([]byte(row.Attachments), &act.Attachments)
	if act.Origin.Channel == "" {
		act.Origin.Channel = row.Channel
	}
	if act.Origin.ThreadID == "" {
		act.Origin.ThreadID = row.Thread
	}
	return act
}

func originOf(id Identity, withText bool) Origin {
	o := Origin{
		Platform: id.Platform, Channel: id.Channel, ThreadID: id.ThreadID, MessageID: id.MessageID,
		UserName: id.UserName, IsDM: id.IsDM,
	}
	if withText {
		o.Text = id.Text
	}
	return o
}

// identityFor is the identity an approved call runs as: the approver (who is
// the owner), filled in from where the action came from.
func identityFor(by Identity, o Origin) Identity {
	if by.UserName == "" {
		by.UserName = o.UserName
	}
	if by.Channel == "" {
		by.Channel, by.ThreadID, by.IsDM = o.Channel, o.ThreadID, o.IsDM
	}
	return by
}

// bindKey ties a sealed account to its row.
func bindKey(userKey, plugin string) string { return userKey + "\x00" + plugin }

// secretValues returns the values of the plugin's secret account fields, for redaction.
func secretValues(p Plugin, creds map[string]string) []string {
	var out []string
	for _, f := range p.AccountFields() {
		if v := creds[f.Name]; f.Secret && len(v) >= minSecretLen {
			out = append(out, v)
		}
	}
	return out
}

// stateCtx gives a status write its own deadline, detached from a call
// context that may already be over.
func stateCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
}

// redactErr returns err with any secret value removed from its text. It
// keeps err (and its chain) when nothing needs removing, and keeps the code
// of a plugin *Error when something does.
func redactErr(err error, secrets []string) error {
	if err == nil || len(secrets) == 0 {
		return err
	}
	msg := err.Error()
	if redact(msg, secrets) == msg {
		return err
	}
	var pe *Error
	if errors.As(err, &pe) {
		return &Error{Code: pe.Code, Message: redact(pe.Message, secrets), OutcomeUnknown: pe.OutcomeUnknown}
	}
	return errors.New(redact(msg, secrets))
}

// redact replaces any secret value in s, in case a plugin echoes one.
func redact(s string, secrets []string) string {
	for _, v := range secrets {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

// modelResult is what the model sees from a successful read or local call.
func modelResult(res *Result, secrets []string) map[string]any {
	out := map[string]any{"status": "ok"}
	if res == nil {
		return out
	}
	text, truncated := truncateRunes(redact(res.Text, secrets), maxModelTextRunes)
	if text != "" {
		out["text"] = text
	}
	if truncated {
		out["text_truncated"] = true
	}
	addData(out, res.Data, secrets)
	return out
}

// addData puts structured content into out as plain JSON values, with
// secrets redacted, unless it is too large.
func addData(out map[string]any, data any, secrets []string) {
	if data == nil {
		return
	}
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	if len(b) > maxModelDataBytes {
		out["data_truncated"] = true
		return
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return
	}
	out["data"] = redactValue(generic, secrets)
}

func redactValue(v any, secrets []string) any {
	switch x := v.(type) {
	case string:
		return redact(x, secrets)
	case map[string]any:
		for k, e := range x {
			x[k] = redactValue(e, secrets)
		}
	case []any:
		for i, e := range x {
			x[i] = redactValue(e, secrets)
		}
	}
	return v
}

// profileOf reads the account name and ID a connect tool reported.
func profileOf(res *Result, creds map[string]string) (name, id string) {
	if res != nil {
		if m, ok := res.Data.(map[string]any); ok {
			name = firstString(m, "accountName", "name")
			id = firstString(m, "accountId", "owner", "id", "username")
		}
	}
	if id == "" {
		id = creds["username"]
	}
	if name == "" {
		name = id
	}
	return name, id
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func fileNames(files []File) []string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name)
	}
	return names
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "on", "yes":
		return true
	}
	return false
}

func mustJSON(v any, fallback string) string {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return fallback
	}
	return string(b)
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func truncateRunes(s string, limit int) (string, bool) {
	if utf8.RuneCountInString(s) <= limit {
		return s, false
	}
	return string([]rune(s)[:limit]), true
}
