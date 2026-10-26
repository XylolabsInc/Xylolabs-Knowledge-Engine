// Package mcpstdio runs a plugin as an MCP server on stdio (any language) and
// adapts it to plugin.Plugin, using the official MCP Go SDK.
//
// Gating fails closed: a tool runs without approval only when its _meta
// "xylolabs/kind" is "read" or "local", or, with no kind, when its
// annotations say readOnlyHint. Kind "write" always gates; destructiveHint is
// ignored, so a plugin cannot opt out of approval by declaring a write
// harmless.
// A tool's _meta "xylolabs/role" ("connect", "login") hides it from the
// model. Each tools/call carries the person and their credentials in request
// _meta (see the meta* keys). The child process gets a minimal environment, so
// the bot's own secrets (tokens, PLUGIN_SECRET_KEY) never reach it.
package mcpstdio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xylolabsinc/xylolabs-kb/internal/plugin"
)

// Request and tool _meta keys shared with plugins.
const (
	metaUser        = "xylolabs/user"
	metaCredentials = "xylolabs/credentials"
	metaAllowKick   = "xylolabs/allowKick"
	metaMode        = "xylolabs/mode"
	metaAttachments = "xylolabs/attachments"
	metaRole        = "xylolabs/role"
	metaKind        = "xylolabs/kind"
	metaPrivate     = "xylolabs/private"
	metaName        = "xylolabs/name"
)

const (
	listTimeout       = 30 * time.Second
	closedGrace       = 500 * time.Millisecond
	maxStderrLineSize = 2000
)

// Config describes how to start one plugin.
type Config struct {
	Name    string   // tool prefix, e.g. "daouoffice"
	Title   string   // display name, e.g. "다우오피스"
	Command []string // argv, e.g. ["node", "plugins/daouoffice/src/server.mjs"]
	Dir     string   // working directory; empty means the bot's
	Env     []string // KEY=VALUE pairs added to a minimal inherited environment
	DataDir string   // plugin data directory, also the registry's inbox root
}

// inheritedEnv lists the variables the child process inherits from the bot.
// Everything else (Slack and Gemini tokens, PLUGIN_SECRET_KEY) is withheld.
var inheritedEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TZ", "TMPDIR", "TEMP", "TMP", "NODE_EXTRA_CA_CERTS", "PLAYWRIGHT_BROWSERS_PATH"}

var inheritedPrefixes = []string{"LC_", "XDG_"}

type client struct {
	cfg    Config
	logger *slog.Logger
	mcp    *mcp.Client

	mu           sync.Mutex
	session      *mcp.ClientSession
	done         chan struct{} // closed when session ends
	tools        []plugin.Tool
	fields       []plugin.AccountField
	instructions string
	closed       bool
}

// New starts the plugin process, initializes the MCP session and lists its tools.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (plugin.Plugin, error) {
	if cfg.Name == "" || len(cfg.Command) == 0 {
		return nil, errors.New("mcpstdio: name and command are required")
	}
	if cfg.Title == "" {
		cfg.Title = cfg.Name
	}
	c := &client{cfg: cfg, logger: logger.With("component", "mcpstdio", "plugin", cfg.Name)}
	// No ClientOptions.Logger: the SDK would log request params, which carry credentials.
	c.mcp = mcp.NewClient(&mcp.Implementation{Name: "xylolabs-kb", Version: "1.0.0"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{},
		ToolListChangedHandler: func(_ context.Context, req *mcp.ToolListChangedRequest) {
			go c.refreshTools(req.Session)
		},
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connectLocked(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *client) Name() string  { return c.cfg.Name }
func (c *client) Title() string { return c.cfg.Title }

func (c *client) DataDir() string { return c.cfg.DataDir }

func (c *client) Instructions() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instructions
}

func (c *client) Tools() []plugin.Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]plugin.Tool(nil), c.tools...)
}

func (c *client) AccountFields() []plugin.AccountField {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]plugin.AccountField(nil), c.fields...)
}

// connectLocked spawns the process and initializes a session. c.mu must be held.
func (c *client) connectLocked(ctx context.Context) error {
	cmd := exec.Command(c.cfg.Command[0], c.cfg.Command[1:]...)
	cmd.Dir = c.cfg.Dir
	cmd.Env = childEnv(os.Environ(), c.cfg.Env)
	cmd.Stderr = &stderrLogger{logger: c.logger}

	session, err := c.mcp.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("mcpstdio: %s: start: %w", c.cfg.Name, err)
	}
	tools, fields, err := listTools(ctx, session)
	if err != nil {
		_ = session.Close()
		return fmt.Errorf("mcpstdio: %s: %w", c.cfg.Name, err)
	}
	done := make(chan struct{})
	go func() {
		_ = session.Wait()
		close(done)
	}()

	c.session, c.done = session, done
	c.tools, c.fields = tools, fields
	init := session.InitializeResult()
	if init != nil {
		c.instructions = init.Instructions
	}
	version := ""
	if init != nil {
		version = init.ProtocolVersion
	}
	c.logger.Info("plugin connected", "protocol", version, "tools", len(tools), "accounts", len(fields) > 0)
	return nil
}

// current returns a live session, restarting the process if it has exited.
// Nothing has been sent when this restarts, so it is safe for every call.
func (c *client) current(ctx context.Context) (*mcp.ClientSession, chan struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil, fmt.Errorf("mcpstdio: %s: closed", c.cfg.Name)
	}
	if c.session != nil && !isDone(c.done) {
		return c.session, c.done, nil
	}
	if c.session != nil {
		c.logger.Warn("plugin process exited; restarting")
		_ = c.session.Close()
		c.session = nil
	}
	if err := c.connectLocked(ctx); err != nil {
		return nil, nil, err
	}
	return c.session, c.done, nil
}

// restart replaces dead with a new session unless another call already did.
func (c *client) restart(ctx context.Context, dead *mcp.ClientSession) (*mcp.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("mcpstdio: %s: closed", c.cfg.Name)
	}
	if c.session != nil && c.session != dead && !isDone(c.done) {
		return c.session, nil
	}
	c.logger.Warn("plugin connection closed; restarting once")
	if c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
	if err := c.connectLocked(ctx); err != nil {
		return nil, err
	}
	return c.session, nil
}

func (c *client) refreshTools(session *mcp.ClientSession) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	tools, fields, err := listTools(ctx, session)
	if err != nil {
		c.logger.Warn("plugin tool list refresh failed", "error", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != session {
		return
	}
	c.tools, c.fields = tools, fields
	c.logger.Info("plugin tools changed", "tools", len(tools))
}

func (c *client) toolInfo(name string) (plugin.Tool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.tools {
		if t.Name == name {
			return t, true
		}
	}
	return plugin.Tool{}, false
}

// Call runs one tools/call. A process that exited before the call is
// restarted first. A connection that closes during the call is retried once
// on a new process, except for a write tool in Mode execute: its outcome is
// unknown, so retrying could submit twice.
func (c *client) Call(ctx context.Context, req plugin.CallRequest) (*plugin.Result, error) {
	tool, ok := c.toolInfo(req.Tool)
	if !ok {
		return nil, fmt.Errorf("mcpstdio: %s: unknown tool %s", c.cfg.Name, req.Tool)
	}
	args := req.Args
	if args == nil {
		args = map[string]any{}
	}
	params := &mcp.CallToolParams{Name: req.Tool, Arguments: args, Meta: requestMeta(req)}

	session, done, err := c.current(ctx)
	if err != nil {
		return nil, err
	}
	res, err := session.CallTool(ctx, params)
	if err != nil && ctx.Err() == nil && connectionClosed(err, done) {
		if tool.Write && req.Mode == plugin.ModeExecute {
			return nil, fmt.Errorf("mcpstdio: %s: %s: plugin exited during the call, so its outcome is unknown: %w", c.cfg.Name, req.Tool, err)
		}
		session, err = c.restart(ctx, session)
		if err != nil {
			return nil, err
		}
		res, err = session.CallTool(ctx, params)
	}
	if err != nil {
		return nil, fmt.Errorf("mcpstdio: %s: call %s: %w", c.cfg.Name, req.Tool, err)
	}
	if res.IsError {
		return nil, toolError(res)
	}
	return convertResult(res), nil
}

// Close stops the plugin process.
func (c *client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.session == nil {
		return nil
	}
	err := c.session.Close()
	c.session = nil
	if err != nil && !errors.Is(err, mcp.ErrConnectionClosed) {
		return fmt.Errorf("mcpstdio: %s: close: %w", c.cfg.Name, err)
	}
	return nil
}

// listTools reads every tool and maps it to plugin.Tool.
func listTools(ctx context.Context, session *mcp.ClientSession) ([]plugin.Tool, []plugin.AccountField, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	var tools []plugin.Tool
	var fields []plugin.AccountField
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, nil, fmt.Errorf("list tools: %w", err)
		}
		schema, _ := t.InputSchema.(map[string]any)
		role, _ := t.Meta[metaRole].(string)
		kind, _ := t.Meta[metaKind].(string)
		params, jsonArgs := plugin.SanitizeSchema(schema)
		tools = append(tools, plugin.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
			JSONArgs:    jsonArgs,
			Write:       isWrite(t.Annotations, kind),
			Role:        role,
		})
		if role == plugin.RoleConnect {
			fields = plugin.AccountFieldsFromSchema(schema)
		}
	}
	return tools, fields, nil
}

// isWrite reports whether a tool is gated behind approval. Only kind "read"
// or "local", or readOnlyHint when there is no kind, lets a tool run at once.
func isWrite(a *mcp.ToolAnnotations, kind string) bool {
	switch kind {
	case "write":
		return true
	case "read", "local":
		return false
	}
	return a == nil || !a.ReadOnlyHint
}

func requestMeta(req plugin.CallRequest) mcp.Meta {
	meta := mcp.Meta{
		metaUser: map[string]any{
			"key":      req.Identity.Key(),
			"name":     req.Identity.UserName,
			"platform": req.Identity.Platform,
		},
		metaAllowKick: req.AllowKick,
	}
	if req.Mode != "" {
		meta[metaMode] = string(req.Mode)
	}
	if len(req.Credentials) > 0 {
		meta[metaCredentials] = req.Credentials
	}
	if len(req.Attachments) > 0 {
		meta[metaAttachments] = req.Attachments
	}
	return meta
}

// connectionClosed reports whether a call failed because the process went
// away. The SDK wraps that as ErrConnectionClosed; an EOF that races the
// session's end is caught by waiting briefly for done.
func connectionClosed(err error, done chan struct{}) bool {
	if errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, io.EOF) {
		return true
	}
	var wire *jsonrpc.Error
	if errors.As(err, &wire) {
		return false // the server answered
	}
	select {
	case <-done:
		return true
	case <-time.After(closedGrace):
		return false
	}
}

func isDone(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// toolError maps an isError result to a *plugin.Error. A result without a
// code is the SDK rejecting the arguments against the input schema, so it
// counts as VALIDATION. data.afterWrite means the plugin had already changed
// something on the server when it failed, so the outcome is unknown.
func toolError(res *mcp.CallToolResult) error {
	code, message := "", ""
	unknown := false
	if m, ok := res.StructuredContent.(map[string]any); ok {
		code, _ = m["code"].(string)
		message, _ = m["message"].(string)
		data, _ := m["data"].(map[string]any)
		unknown = data["afterWrite"] == true || m["afterWrite"] == true
	}
	if message == "" {
		message = joinText(res.Content)
	}
	if code == "" {
		code = plugin.CodeValidation
	}
	return &plugin.Error{Code: code, Message: message, OutcomeUnknown: unknown}
}

func joinText(content []mcp.Content) string {
	var parts []string
	for _, c := range content {
		if t, ok := c.(*mcp.TextContent); ok && strings.TrimSpace(t.Text) != "" {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// convertResult turns MCP content into text and files.
func convertResult(res *mcp.CallToolResult) *plugin.Result {
	out := &plugin.Result{Text: joinText(res.Content), Data: res.StructuredContent}
	for i, content := range res.Content {
		switch v := content.(type) {
		case *mcp.ImageContent:
			out.Files = append(out.Files, plugin.File{
				Name:     fileName(v.Meta, "", "image", i, v.MIMEType),
				MimeType: v.MIMEType,
				Data:     v.Data,
				Private:  isPrivate(v.Meta),
			})
		case *mcp.AudioContent:
			out.Files = append(out.Files, plugin.File{
				Name:     fileName(v.Meta, "", "audio", i, v.MIMEType),
				MimeType: v.MIMEType,
				Data:     v.Data,
				Private:  isPrivate(v.Meta),
			})
		case *mcp.EmbeddedResource:
			r := v.Resource
			if r == nil {
				continue
			}
			data := r.Blob
			if data == nil && r.Text != "" {
				data = []byte(r.Text)
			}
			if data == nil {
				continue
			}
			name := fileName(v.Meta, "", "", i, "")
			if name == "" {
				name = fileName(r.Meta, r.URI, "file", i, r.MIMEType)
			}
			out.Files = append(out.Files, plugin.File{
				Name:     name,
				MimeType: r.MIMEType,
				Data:     data,
				Private:  isPrivate(v.Meta) || isPrivate(r.Meta),
			})
		}
	}
	return out
}

func isPrivate(meta mcp.Meta) bool {
	b, _ := meta[metaPrivate].(bool)
	return b
}

var extByType = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif",
	"application/pdf": ".pdf", "text/plain": ".txt", "text/csv": ".csv",
}

// fileName picks a name from _meta "xylolabs/name", else the resource URI,
// else "<kind>-<n><ext>"; with kind "" it returns "" when meta has no name.
func fileName(meta mcp.Meta, uri, kind string, i int, mimeType string) string {
	if s, _ := meta[metaName].(string); strings.TrimSpace(s) != "" {
		return path.Base(strings.ReplaceAll(strings.TrimSpace(s), "\\", "/"))
	}
	if uri != "" {
		if u, err := url.Parse(uri); err == nil {
			if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
				return base
			}
		}
	}
	if kind == "" {
		return ""
	}
	ext := extByType[mimeType]
	if ext == "" {
		ext = ".bin"
	}
	return fmt.Sprintf("%s-%d%s", kind, i+1, ext)
}

// childEnv keeps the allowed parent variables and appends extra.
func childEnv(parent, extra []string) []string {
	var env []string
	for _, kv := range parent {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		allowed := false
		for _, k := range inheritedEnv {
			if key == k {
				allowed = true
			}
		}
		for _, p := range inheritedPrefixes {
			if strings.HasPrefix(key, p) {
				allowed = true
			}
		}
		if allowed {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// stderrLogger logs the plugin's stderr line by line.
type stderrLogger struct {
	logger *slog.Logger
	mu     sync.Mutex
	buf    []byte
}

func (w *stderrLogger) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) > maxStderrLineSize {
				w.emit(w.buf)
				w.buf = w.buf[:0]
			}
			return len(p), nil
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
}

func (w *stderrLogger) emit(line []byte) {
	s := strings.TrimRight(string(line), "\r")
	if strings.TrimSpace(s) == "" {
		return
	}
	if len(s) > maxStderrLineSize {
		s = s[:maxStderrLineSize]
	}
	w.logger.Info("plugin stderr", "line", s)
}
