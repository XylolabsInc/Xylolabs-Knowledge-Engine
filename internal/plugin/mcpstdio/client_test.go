package mcpstdio

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xylolabsinc/xylolabs-kb/internal/plugin"
	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
)

// testServer prepares testdata/server.mjs to run with the daouoffice
// plugin's node_modules, or skips when node or those modules are missing.
func testServer(t *testing.T) []string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	modules, err := filepath.Abs(filepath.Join("..", "..", "..", "plugins", "daouoffice", "node_modules"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"@modelcontextprotocol/server", "zod"} {
		if _, err := os.Stat(filepath.Join(modules, pkg)); err != nil {
			t.Skipf("%s is not installed in plugins/daouoffice (run npm install there)", pkg)
		}
	}
	dir := t.TempDir()
	if err := os.Symlink(modules, filepath.Join(dir, "node_modules")); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("testdata", "server.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "server.mjs")
	if err := os.WriteFile(script, src, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{node, script}
}

type testClient struct {
	plugin.Plugin
	countFile string
	logs      *bytes.Buffer
}

func newTestClient(t *testing.T) *testClient {
	t.Helper()
	command := testServer(t)
	t.Setenv("PLUGIN_SECRET_KEY", "must-not-reach-the-plugin")
	countFile := filepath.Join(t.TempDir(), "count")
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := New(ctx, Config{
		Name: "test", Title: "테스트", Command: command,
		Env:     []string{"COUNT_FILE=" + countFile, "EXTRA_VAR=extra"},
		DataDir: t.TempDir(),
	}, logger)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return &testClient{Plugin: p, countFile: countFile, logs: logs}
}

func (c *testClient) count(t *testing.T, tool string) int {
	t.Helper()
	data, err := os.ReadFile(c.countFile)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), tool+"\n")
}

func call(t *testing.T, p plugin.Plugin, req plugin.CallRequest) (*plugin.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if req.Identity.Platform == "" {
		req.Identity = plugin.Identity{Platform: "slack", UserID: "U1", UserName: "홍길동"}
	}
	return p.Call(ctx, req)
}

func TestClientTools(t *testing.T) {
	c := newTestClient(t)
	if got := c.Instructions(); got != "Test plugin instructions." {
		t.Errorf("Instructions() = %q", got)
	}
	tools := map[string]plugin.Tool{}
	for _, tool := range c.Tools() {
		tools[tool.Name] = tool
	}
	for name, want := range map[string]struct {
		write bool
		role  string
	}{
		"echo": {false, ""}, "submit": {true, ""}, "save_note": {false, ""}, "plain": {true, ""}, "harmless_write": {true, ""},
		"connect": {false, plugin.RoleConnect}, "busy": {false, ""}, "files": {false, ""},
		"kind_write": {true, ""},
	} {
		got, ok := tools[name]
		if !ok {
			t.Errorf("tool %s missing", name)
			continue
		}
		if got.Write != want.write || got.Role != want.role {
			t.Errorf("tool %s: write=%v role=%q, want write=%v role=%q", name, got.Write, got.Role, want.write, want.role)
		}
	}

	params := tools["echo"].Parameters
	if _, ok := params["$schema"]; ok {
		t.Errorf("echo parameters keep $schema: %v", params)
	}
	if _, ok := params["additionalProperties"]; ok {
		t.Errorf("echo parameters keep additionalProperties: %v", params)
	}
	props, _ := params["properties"].(map[string]any)
	n, _ := props["n"].(map[string]any)
	if n["type"] != "integer" || n["minimum"] != nil || n["maximum"] != nil {
		t.Errorf("echo.n = %v; want an integer without 2^53 bounds", n)
	}
	text, _ := props["text"].(map[string]any)
	if text["description"] != "Text to echo" {
		t.Errorf("echo.text = %v", text)
	}
	if req, _ := params["required"].([]string); !reflect.DeepEqual(req, []string{"text"}) {
		t.Errorf("echo required = %#v", params["required"])
	}

	want := []plugin.AccountField{
		{Name: "username", Label: "아이디"},
		{Name: "password", Label: "비밀번호", Secret: true},
		{Name: "allowKick", Label: "기존 접속을 끊어도 됩니다", Optional: true, Checkbox: true},
	}
	if got := c.AccountFields(); !reflect.DeepEqual(got, want) {
		t.Errorf("AccountFields()\n got  %+v\n want %+v", got, want)
	}
	if c.Name() != "test" || c.Title() != "테스트" || c.DataDir() == "" {
		t.Errorf("Name/Title/DataDir = %q %q %q", c.Name(), c.Title(), c.DataDir())
	}
}

func TestClientCallMeta(t *testing.T) {
	c := newTestClient(t)
	photo := filepath.Join(t.TempDir(), "IMG_1.jpg")
	if err := os.WriteFile(photo, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := call(t, c, plugin.CallRequest{
		Tool:        "echo",
		Args:        map[string]any{"text": "안녕", "n": 3},
		Credentials: map[string]string{"username": "hong", "password": "pa55word"},
		AllowKick:   true,
		Mode:        plugin.ModeExecute,
		Attachments: []plugin.Attachment{{Name: "IMG_1.jpg", MimeType: "image/jpeg", Path: photo}},
	})
	if err != nil {
		t.Fatalf("Call(echo) error = %v", err)
	}
	if res.Text != "echo: 안녕" {
		t.Errorf("Text = %q", res.Text)
	}
	data, _ := res.Data.(map[string]any)
	user, _ := data["user"].(map[string]any)
	if user["key"] != "slack:U1" || user["name"] != "홍길동" || user["platform"] != "slack" {
		t.Errorf("user meta = %v", data["user"])
	}
	if data["mode"] != "execute" || data["allowKick"] != true || data["username"] != "hong" || data["hasPassword"] != true || data["n"] != float64(3) {
		t.Errorf("meta = %v", data)
	}
	atts, _ := data["attachments"].([]any)
	if len(atts) != 1 || !reflect.DeepEqual(atts[0], map[string]any{"name": "IMG_1.jpg", "mimeType": "image/jpeg", "size": float64(5)}) {
		t.Errorf("attachments = %v", data["attachments"])
	}
	if data["secretEnvVisible"] != false || data["extraEnv"] != "extra" {
		t.Errorf("environment: secret visible %v, extra %v", data["secretEnvVisible"], data["extraEnv"])
	}
	if strings.Contains(c.logs.String(), "pa55word") {
		t.Errorf("logs contain the password:\n%s", c.logs.String())
	}

	res, err = call(t, c, plugin.CallRequest{Tool: "submit", Args: map[string]any{"key": "p1"}, Mode: plugin.ModePreview})
	if err != nil || res.Text != "preview p1" {
		t.Errorf("Call(submit, preview) = %+v, %v", res, err)
	}
}

func TestClientErrors(t *testing.T) {
	c := newTestClient(t)
	_, err := call(t, c, plugin.CallRequest{Tool: "busy", Mode: plugin.ModeExecute})
	if !errors.Is(err, plugin.ErrDuplicateSession) {
		t.Fatalf("Call(busy) = %v, want ErrDuplicateSession", err)
	}
	var pe *plugin.Error
	if !errors.As(err, &pe) || pe.Message != "다른 곳에서 접속 중입니다" {
		t.Errorf("error = %#v", err)
	}

	// Missing required argument: the SDK reports it as a tool error.
	_, err = call(t, c, plugin.CallRequest{Tool: "echo", Args: map[string]any{}, Mode: plugin.ModeExecute})
	if !errors.As(err, &pe) || pe.Message == "" || pe.Code != plugin.CodeValidation {
		t.Errorf("Call(echo without text) = %v; want a VALIDATION *plugin.Error with a message", err)
	}

	if _, err := call(t, c, plugin.CallRequest{Tool: "nope"}); err == nil {
		t.Error("Call(unknown tool) succeeded")
	}
}

func TestClientFiles(t *testing.T) {
	c := newTestClient(t)
	res, err := call(t, c, plugin.CallRequest{Tool: "files", Mode: plugin.ModeExecute})
	if err != nil {
		t.Fatalf("Call(files) error = %v", err)
	}
	if res.Text != "two files" || len(res.Files) != 4 {
		t.Fatalf("result = %+v", res)
	}
	img, pdf := res.Files[0], res.Files[1]
	if named := res.Files[2]; named.Name != "재직증명서.pdf" || !named.Private || string(named.Data) != "%PDF-1.4 second" {
		t.Errorf("file named by resource _meta = %s private=%v %q", named.Name, named.Private, named.Data)
	}
	if byURI := res.Files[3]; byURI.Name != "견적서 A.pdf" || byURI.Private {
		t.Errorf("file named by URI = %s private=%v", byURI.Name, byURI.Private)
	}
	if img.Name != "image-2.png" || img.MimeType != "image/png" || !bytes.HasPrefix(img.Data, []byte("\x89PNG")) || img.Private {
		t.Errorf("image = %+v", img)
	}
	if pdf.Name != "cert.pdf" || pdf.MimeType != "application/pdf" || string(pdf.Data) != "%PDF-1.4 test" || !pdf.Private {
		t.Errorf("pdf = %s %s %q private=%v", pdf.Name, pdf.MimeType, pdf.Data, pdf.Private)
	}
}

func TestClientLargeResult(t *testing.T) {
	c := newTestClient(t)
	const size = 6 << 20 // 8 MB once base64-encoded on one line
	res, err := call(t, c, plugin.CallRequest{Tool: "big", Args: map[string]any{"bytes": size}, Mode: plugin.ModeExecute})
	if err != nil {
		t.Fatalf("Call(big) error = %v", err)
	}
	if len(res.Files) != 1 || len(res.Files[0].Data) != size || res.Files[0].Data[size-1] != 7 {
		t.Errorf("got %d files, first %d bytes; want one of %d", len(res.Files), len(res.Files[0].Data), size)
	}
}

func TestClientRestart(t *testing.T) {
	c := newTestClient(t)

	t.Run("process that exited is restarted before the next call", func(t *testing.T) {
		if _, err := call(t, c, plugin.CallRequest{Tool: "exit_after", Mode: plugin.ModeExecute}); err != nil {
			t.Fatalf("Call(exit_after) error = %v", err)
		}
		time.Sleep(300 * time.Millisecond)
		res, err := call(t, c, plugin.CallRequest{Tool: "echo", Args: map[string]any{"text": "again"}, Mode: plugin.ModeExecute})
		if err != nil || res.Text != "echo: again" {
			t.Fatalf("Call(echo) after exit = %+v, %v", res, err)
		}
	})

	t.Run("write in execute mode is not retried", func(t *testing.T) {
		_, err := call(t, c, plugin.CallRequest{Tool: "crash_submit", Mode: plugin.ModeExecute})
		if err == nil || !strings.Contains(err.Error(), "outcome is unknown") {
			t.Errorf("Call(crash_submit) = %v; want an unknown-outcome error", err)
		}
		if n := c.count(t, "crash_submit"); n != 1 {
			t.Errorf("crash_submit ran %d times, want 1", n)
		}
	})

	t.Run("read is retried once", func(t *testing.T) {
		if _, err := call(t, c, plugin.CallRequest{Tool: "crash_read", Mode: plugin.ModeExecute}); err == nil {
			t.Error("Call(crash_read) succeeded")
		}
		if n := c.count(t, "crash_read"); n != 2 {
			t.Errorf("crash_read ran %d times, want 2", n)
		}
	})

	t.Run("still usable afterwards", func(t *testing.T) {
		res, err := call(t, c, plugin.CallRequest{Tool: "echo", Args: map[string]any{"text": "ok"}, Mode: plugin.ModeExecute})
		if err != nil || res.Text != "echo: ok" {
			t.Errorf("Call(echo) = %+v, %v", res, err)
		}
	})

	if err := c.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
	if _, err := call(t, c, plugin.CallRequest{Tool: "echo", Args: map[string]any{"text": "x"}}); err == nil {
		t.Error("Call after Close succeeded")
	}
}

func TestChildEnv(t *testing.T) {
	got := childEnv([]string{"PATH=/bin", "SLACK_BOT_TOKEN=x", "PLUGIN_SECRET_KEY=y", "LC_ALL=ko_KR.UTF-8", "HOME=/h", "broken"},
		[]string{"DAOUOFFICE_CONFIG=/c.json"})
	want := []string{"PATH=/bin", "LC_ALL=ko_KR.UTF-8", "HOME=/h", "DAOUOFFICE_CONFIG=/c.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("childEnv() = %v, want %v", got, want)
	}
}

func TestIsWrite(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name string
		a    *mcp.ToolAnnotations
		kind string
		want bool
	}{
		{"no annotations, no kind", nil, "", true},
		{"read only", &mcp.ToolAnnotations{ReadOnlyHint: true}, "", false},
		{"not read only", &mcp.ToolAnnotations{}, "", true},
		{"destructive false no longer opts out", &mcp.ToolAnnotations{DestructiveHint: &no}, "", true},
		{"kind local", &mcp.ToolAnnotations{DestructiveHint: &no}, "local", false},
		{"kind read without annotations", nil, "read", false},
		{"kind write beats read only", &mcp.ToolAnnotations{ReadOnlyHint: true}, "write", true},
		{"unknown kind falls back to annotations", &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &yes}, "other", false},
		{"unknown kind without read only", nil, "other", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWrite(tt.a, tt.kind); got != tt.want {
				t.Errorf("isWrite() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToolError(t *testing.T) {
	tests := []struct {
		name    string
		res     *mcp.CallToolResult
		want    plugin.Error
		matches error
	}{
		{
			name:    "coded error",
			res:     &mcp.CallToolResult{IsError: true, StructuredContent: map[string]any{"code": "AUTH_FAILED", "message": "틀림"}},
			want:    plugin.Error{Code: "AUTH_FAILED", Message: "틀림"},
			matches: plugin.ErrAuthFailed,
		},
		{
			name: "login error after a write is an unknown outcome",
			res: &mcp.CallToolResult{IsError: true, StructuredContent: map[string]any{
				"code": "UPSTREAM", "message": "중간에 실패", "data": map[string]any{"afterWrite": true, "original": "DUPLICATE_SESSION"},
			}},
			want: plugin.Error{Code: "UPSTREAM", Message: "중간에 실패", OutcomeUnknown: true},
		},
		{
			name: "SDK argument rejection has no code",
			res:  &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "Input validation error"}}},
			want: plugin.Error{Code: plugin.CodeValidation, Message: "Input validation error"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := toolError(tt.res)
			var pe *plugin.Error
			if !errors.As(err, &pe) || *pe != tt.want {
				t.Fatalf("toolError() = %#v, want %#v", err, tt.want)
			}
			if tt.matches != nil && !errors.Is(err, tt.matches) {
				t.Errorf("errors.Is(%v, %v) = false", err, tt.matches)
			}
			if tt.want.OutcomeUnknown && errors.Is(err, plugin.ErrDuplicateSession) {
				t.Error("an after-write failure must not look like a login-stage error")
			}
		})
	}
}

// TestRegistryEndToEnd runs the registry against the real Node process:
// connect, a read call with an attachment, a write preview, and approval.
func TestRegistryEndToEnd(t *testing.T) {
	c := newTestClient(t)
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	store, err := storage.New(filepath.Join(t.TempDir(), "kb.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	vault, err := plugin.NewVault(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	reg := plugin.NewRegistry(store, vault, logger)
	if err := reg.Register(c.Plugin); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	names := map[string]bool{}
	for _, d := range reg.Declarations() {
		names[d.Name] = true
	}
	if !names["test_echo"] || !names["test_submit"] || names["test_connect"] || !names["accounts_connect"] {
		t.Errorf("declarations = %v", names)
	}

	id := plugin.Identity{Platform: "slack", UserID: "U1", UserName: "홍길동", Channel: "C1", ThreadID: "1.1"}
	name, err := reg.Connect(ctx, id, "test", map[string]string{"username": "hong", "password": "pa55word"})
	if err != nil || name != "홍길동 (개발본부)" {
		t.Fatalf("Connect() = %q, %v", name, err)
	}

	rctx, prompts := plugin.ContextWithPrompts(plugin.ContextWithIdentity(ctx, id))
	out, err := reg.Dispatch(rctx, "test_echo", map[string]any{"text": "hi"}, map[string][]byte{"a.jpg": []byte("\xff\xd8\xff\xe0 jpeg")})
	if err != nil {
		t.Fatalf("Dispatch(echo) error = %v", err)
	}
	data, _ := out["data"].(map[string]any)
	atts, _ := data["attachments"].([]any)
	if data["mode"] != "execute" || data["hasPassword"] != true || data["username"] != "hong" || len(atts) != 1 {
		t.Errorf("Dispatch(echo) = %v", out)
	}
	if first, _ := atts[0].(map[string]any); first["mimeType"] != "image/jpeg" {
		t.Errorf("attachment = %v", atts[0])
	}

	out, err = reg.Dispatch(rctx, "test_submit", map[string]any{"key": "p1"}, nil)
	if err != nil || out["status"] != "awaiting_approval" || out["preview"] != "preview p1" {
		t.Fatalf("Dispatch(submit) = %v, %v", out, err)
	}
	var actionID string
	for _, p := range prompts.List() {
		if p.Kind == plugin.PromptApproval {
			actionID = p.ActionID
		}
	}
	if actionID == "" {
		t.Fatalf("no approval prompt in %+v", prompts.List())
	}
	res, act, err := reg.Approve(ctx, actionID, id)
	if err != nil || res.Text != "execute p1" || act.Status != plugin.StatusDone {
		t.Fatalf("Approve() = %+v, %+v, %v", res, act, err)
	}
	if strings.Contains(logs.String(), "pa55word") || strings.Contains(c.logs.String(), "pa55word") {
		t.Error("password appeared in logs")
	}
}
