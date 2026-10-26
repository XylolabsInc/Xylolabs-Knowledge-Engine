package plugin

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
)

const testPassword = "pa55word-테스트"

// fakePlugin records every call and answers through respond.
type fakePlugin struct {
	name    string
	tools   []Tool
	fields  []AccountField
	dataDir string
	respond func(req CallRequest) (*Result, error)
	// blockExecute makes execute calls wait for their context to end.
	blockExecute bool

	mu    sync.Mutex
	calls []fakeCall
}

type fakeCall struct {
	req   CallRequest
	files map[string][]byte // attachment contents read at call time
}

func (f *fakePlugin) Name() string                  { return f.name }
func (f *fakePlugin) Title() string                 { return "테스트오피스" }
func (f *fakePlugin) Instructions() string          { return "Ask before inventing purposes." }
func (f *fakePlugin) Tools() []Tool                 { return f.tools }
func (f *fakePlugin) AccountFields() []AccountField { return f.fields }
func (f *fakePlugin) DataDir() string               { return f.dataDir }
func (f *fakePlugin) Close() error                  { return nil }

func (f *fakePlugin) Call(ctx context.Context, req CallRequest) (*Result, error) {
	if f.blockExecute && req.Mode == ModeExecute {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	files := map[string][]byte{}
	for _, a := range req.Attachments {
		data, err := os.ReadFile(a.Path)
		if err != nil {
			return nil, err
		}
		files[a.Name] = data
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{req: req, files: files})
	f.mu.Unlock()
	if f.respond != nil {
		return f.respond(req)
	}
	return &Result{Text: "ok " + req.Tool + " " + string(req.Mode)}, nil
}

func (f *fakePlugin) callsTo(tool string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.req.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

func newFakePlugin(t *testing.T) *fakePlugin {
	return &fakePlugin{
		name:    "testoffice",
		dataDir: t.TempDir(),
		tools: []Tool{
			{Name: "card_usage", Description: "Card usage", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
			{Name: "save_photos", Description: "Save photos"},
			{Name: "submit_purchase", Description: "Submit a purchase requisition", Write: true},
			{Name: "connect", Role: RoleConnect},
			{Name: "login", Role: RoleLogin},
		},
		fields: []AccountField{
			{Name: "username", Label: "아이디"},
			{Name: "password", Label: "비밀번호", Secret: true},
			{Name: "allowKick", Label: "기존 접속을 끊어도 됩니다", Optional: true, Checkbox: true},
		},
	}
}

type testEnv struct {
	reg   *Registry
	store *storage.SQLiteStore
	p     *fakePlugin
	logs  *syncBuffer
	now   time.Time
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store, err := storage.New(filepath.Join(t.TempDir(), "kb.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	vault, err := NewVault(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{store: store, p: newFakePlugin(t), logs: logs, now: time.Now()}
	env.reg = NewRegistry(store, vault, logger)
	env.reg.now = func() time.Time { return env.now }
	if err := env.reg.Register(env.p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if strings.Contains(logs.String(), testPassword) {
			t.Errorf("password appeared in logs:\n%s", logs.String())
		}
	})
	return env
}

var owner = Identity{Platform: "slack", UserID: "U1", UserName: "홍길동", Channel: "C1", ThreadID: "100.1", MessageID: "100.1", Text: "구매 품의 올려줘"}

func (e *testEnv) connect(t *testing.T, allowKick bool) {
	t.Helper()
	e.p.respond = func(req CallRequest) (*Result, error) {
		if req.Tool == "connect" {
			return &Result{Text: "연결됨", Data: map[string]any{"accountName": "홍길동 (개발본부)", "accountId": "hong"}}, nil
		}
		return &Result{Text: "ok"}, nil
	}
	kick := ""
	if allowKick {
		kick = "true"
	}
	if _, err := e.reg.Connect(context.Background(), owner, "testoffice", map[string]string{
		"username": "hong", "password": testPassword, "allowKick": kick,
	}); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	e.p.respond = nil
}

func requestCtx(id Identity) (context.Context, *Prompts) {
	ctx, prompts := ContextWithPrompts(context.Background())
	return ContextWithIdentity(ctx, id), prompts
}

func TestDeclarations(t *testing.T) {
	env := newTestEnv(t)
	decls := env.reg.Declarations()
	byName := map[string]string{}
	for _, d := range decls {
		byName[d.Name] = d.Description
		if d.Parameters == nil || d.Parameters["type"] != "object" {
			t.Errorf("%s parameters = %v", d.Name, d.Parameters)
		}
	}
	for _, want := range []string{"testoffice_card_usage", "testoffice_save_photos", "testoffice_submit_purchase",
		"accounts_status", "accounts_connect", "accounts_disconnect"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing declaration %s", want)
		}
	}
	for _, hidden := range []string{"testoffice_connect", "testoffice_login", "connect", "login"} {
		if _, ok := byName[hidden]; ok {
			t.Errorf("hidden tool %s is declared", hidden)
		}
	}
	if len(decls) != 6 {
		t.Errorf("got %d declarations, want 6", len(decls))
	}
	if !strings.Contains(byName["testoffice_submit_purchase"], "nothing is submitted") {
		t.Errorf("write tool description lacks the approval note: %q", byName["testoffice_submit_purchase"])
	}
	if strings.Contains(byName["testoffice_card_usage"], "nothing is submitted") {
		t.Error("read tool description has the approval note")
	}

	for name, want := range map[string]bool{
		"testoffice_card_usage": true, "accounts_status": true,
		"testoffice_connect": false, "testoffice_nope": false, "card_usage": false,
	} {
		if got := env.reg.Handles(name); got != want {
			t.Errorf("Handles(%s) = %v, want %v", name, got, want)
		}
	}
	if !strings.Contains(env.reg.Instructions(), "Ask before inventing purposes.") {
		t.Errorf("Instructions() = %q", env.reg.Instructions())
	}
}

func TestRegister(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	noVault := NewRegistry(nil, nil, logger)
	p := newFakePlugin(t)
	if err := noVault.Register(p); !errors.Is(err, ErrNoVault) {
		t.Errorf("Register(account plugin, no vault) = %v, want ErrNoVault", err)
	}
	p.fields = nil
	if err := noVault.Register(p); err != nil {
		t.Errorf("Register(plugin without accounts, no vault) = %v", err)
	}
	if err := noVault.Register(p); err == nil {
		t.Error("registering the same name twice succeeded")
	}
	for _, bad := range []string{"Dao", "dao_office", "accounts", ""} {
		if err := noVault.Register(&fakePlugin{name: bad}); err == nil {
			t.Errorf("Register(%q) succeeded", bad)
		}
	}
}

func TestDispatchNeedsIdentity(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.reg.Dispatch(context.Background(), "testoffice_card_usage", nil, nil); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("Dispatch without identity = %v, want ErrNoIdentity", err)
	}
}

func TestDispatchNotConnected(t *testing.T) {
	env := newTestEnv(t)
	ctx, prompts := requestCtx(owner)

	for _, tool := range []string{"testoffice_card_usage", "testoffice_submit_purchase"} {
		out, err := env.reg.Dispatch(ctx, tool, map[string]any{}, nil)
		if err != nil {
			t.Fatalf("Dispatch(%s) error = %v", tool, err)
		}
		if out["status"] != "not_connected" || !strings.Contains(out["message"].(string), "DM") {
			t.Errorf("Dispatch(%s) = %v", tool, out)
		}
	}
	if len(env.p.calls) != 0 {
		t.Errorf("plugin was called %d times for an unconnected user", len(env.p.calls))
	}
	list := prompts.List()
	if len(list) != 1 || list[0].Kind != PromptConnect || list[0].Plugin != "testoffice" || list[0].UserKey != "slack:U1" || list[0].Text == "" {
		t.Errorf("prompts = %+v; want one connect prompt", list)
	}
	first, err := env.reg.NeedsConnectPrompt(context.Background(), "slack:U1")
	if err != nil || len(first) != 0 {
		t.Errorf("NeedsConnectPrompt after a connect prompt = %+v, %v; want none", first, err)
	}
	other, _ := env.reg.NeedsConnectPrompt(context.Background(), "slack:U2")
	if len(other) != 1 || other[0].Kind != PromptConnect || other[0].Text == "" {
		t.Errorf("NeedsConnectPrompt(new user) = %+v", other)
	}
	if err := env.reg.MarkPrompted(context.Background(), "slack:U2", "testoffice"); err != nil {
		t.Fatal(err)
	}
	if again, _ := env.reg.NeedsConnectPrompt(context.Background(), "slack:U2"); len(again) != 0 {
		t.Errorf("NeedsConnectPrompt after MarkPrompted = %+v", again)
	}
}

func TestDispatchRead(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, true)
	env.p.respond = func(req CallRequest) (*Result, error) {
		return &Result{
			Text:  "사용내역 3건 (비밀번호 " + testPassword + ")",
			Data:  map[string]any{"rows": []any{map[string]any{"memo": testPassword}}, "count": 3},
			Files: []File{{Name: "usage.png", MimeType: "image/png", Data: []byte("png")}},
		}, nil
	}
	ctx, prompts := requestCtx(owner)
	png := []byte("\x89PNG\r\n\x1a\n0000")
	out, err := env.reg.Dispatch(ctx, "testoffice_card_usage", map[string]any{"month": "2026-10"},
		map[string][]byte{"../../etc/IMG 1.png": png, "note.heic": []byte("heic bytes")})
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}

	calls := env.p.callsTo("card_usage")
	if len(calls) != 1 {
		t.Fatalf("card_usage called %d times", len(calls))
	}
	req := calls[0].req
	if req.Mode != ModeExecute || !req.AllowKick || req.Credentials["password"] != testPassword || req.Credentials["username"] != "hong" {
		t.Errorf("request = %+v", req.LogValue())
	}
	if req.Args["month"] != "2026-10" || req.Identity.Key() != "slack:U1" {
		t.Errorf("args/identity = %v %v", req.Args, req.Identity)
	}
	if len(req.Attachments) != 2 {
		t.Fatalf("attachments = %+v", req.Attachments)
	}
	for _, a := range req.Attachments {
		if !filepath.IsAbs(a.Path) || !strings.HasPrefix(a.Path, filepath.Join(env.p.dataDir, "inbox")) || strings.Contains(a.Name, "/") {
			t.Errorf("attachment %+v is not a bare name under the inbox", a)
		}
		if _, err := os.Stat(a.Path); !os.IsNotExist(err) {
			t.Errorf("attachment %s still exists after a read call", a.Path)
		}
	}
	mimes := map[string]string{}
	for _, a := range req.Attachments {
		mimes[a.Name] = a.MimeType
	}
	if mimes["IMG 1.png"] != "image/png" || mimes["note.heic"] != "image/heic" {
		t.Errorf("mime types = %v", mimes)
	}
	if !bytes.Equal(calls[0].files["IMG 1.png"], png) {
		t.Error("attachment content not passed through")
	}

	if out["status"] != "ok" || !strings.Contains(out["text"].(string), "사용내역 3건") {
		t.Errorf("result = %v", out)
	}
	if strings.Contains(stringify(out), testPassword) {
		t.Errorf("password reached the model: %v", out)
	}
	if names, _ := out["files"].([]string); len(names) != 1 || names[0] != "usage.png" {
		t.Errorf("files = %v", out["files"])
	}
	list := prompts.List()
	if len(list) != 1 || list[0].Kind != PromptNotice || len(list[0].Files) != 1 || list[0].Text == "" {
		t.Errorf("prompts = %+v; want one notice with the file", list)
	}
}

func TestDispatchPluginError(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, false)
	env.p.respond = func(req CallRequest) (*Result, error) {
		return nil, &Error{Code: CodeValidation, Message: "구입목적을 적어 주세요."}
	}
	ctx, _ := requestCtx(owner)
	out, err := env.reg.Dispatch(ctx, "testoffice_save_photos", nil, nil)
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if out["status"] != "error" || out["code"] != CodeValidation || out["message"] != "구입목적을 적어 주세요." {
		t.Errorf("result = %v", out)
	}

	env.p.respond = func(req CallRequest) (*Result, error) { return nil, errors.New("pipe broke") }
	if _, err := env.reg.Dispatch(ctx, "testoffice_save_photos", nil, nil); err == nil {
		t.Error("transport error was not returned")
	}
}

// submit queues a write call and returns its action ID.
func (e *testEnv) submit(t *testing.T, attachments map[string][]byte) string {
	t.Helper()
	ctx, prompts := requestCtx(owner)
	e.p.respond = func(req CallRequest) (*Result, error) {
		if req.Mode == ModeExecute {
			return &Result{Text: "상신 완료", Data: map[string]any{"docId": "D1"}, Files: []File{{Name: "doc.pdf", Data: []byte("%PDF"), Private: true}}}, nil
		}
		return &Result{Text: "품의서 미리보기", Files: []File{{Name: "preview.png", Data: []byte("png")}}}, nil
	}
	out, err := e.reg.Dispatch(ctx, "testoffice_submit_purchase", map[string]any{"key": "p1"}, attachments)
	if err != nil {
		t.Fatalf("Dispatch(write) error = %v", err)
	}
	if out["status"] != "awaiting_approval" || out["preview"] != "품의서 미리보기" ||
		!strings.Contains(out["message"].(string), "Nothing has been submitted") {
		t.Fatalf("Dispatch(write) = %v", out)
	}
	list := prompts.List()
	if len(list) != 1 || list[0].Kind != PromptApproval || list[0].ActionID == "" ||
		!strings.Contains(list[0].Text, "품의서 미리보기") || len(list[0].Files) != 1 {
		t.Fatalf("prompts = %+v; want one approval prompt with the preview file", list)
	}
	return list[0].ActionID
}

func TestDispatchWriteOnlyPreviews(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, false)
	id := env.submit(t, map[string][]byte{"photo.jpg": []byte("\xff\xd8\xff\xe0jpeg")})

	calls := env.p.callsTo("submit_purchase")
	if len(calls) != 1 || calls[0].req.Mode != ModePreview {
		t.Fatalf("submit_purchase calls = %+v; want one preview", calls)
	}
	act, err := env.store.GetPluginAction(context.Background(), id)
	if err != nil || act == nil {
		t.Fatalf("GetPluginAction() = %v, %v", act, err)
	}
	if act.Status != "pending" || act.Kind != "call" || act.UserKey != "slack:U1" || act.Channel != "C1" || act.Thread != "100.1" ||
		act.Args != `{"key":"p1"}` || act.Preview != "품의서 미리보기" {
		t.Errorf("stored action = %+v", act)
	}
	if strings.Contains(act.Args+act.Attachments+act.Origin, testPassword) {
		t.Error("stored action contains the password")
	}
	if !act.ExpiresAt.After(env.now.Add(29 * time.Minute)) {
		t.Errorf("ExpiresAt = %v, want about 30 minutes out", act.ExpiresAt)
	}
	path := calls[0].req.Attachments[0].Path
	if _, err := os.Stat(path); err != nil {
		t.Errorf("pending action's attachment is gone: %v", err)
	}

	pending, err := env.reg.PendingInThread(context.Background(), owner)
	if err != nil || pending == nil || pending.ID != id || len(pending.Attachments) != 1 {
		t.Errorf("PendingInThread() = %+v, %v", pending, err)
	}
	if err := env.reg.SetActionPrompt(context.Background(), id, "C1", "200.2"); err != nil {
		t.Fatal(err)
	}
}

func TestApprove(t *testing.T) {
	stranger := Identity{Platform: "slack", UserID: "U9", Channel: "C1", ThreadID: "100.1"}
	sameIDOtherPlatform := Identity{Platform: "discord", UserID: "U1"}

	t.Run("owner approves once and it executes once", func(t *testing.T) {
		env := newTestEnv(t)
		env.connect(t, false)
		id := env.submit(t, map[string][]byte{"photo.jpg": []byte("jpeg")})
		res, act, err := env.reg.Approve(context.Background(), id, owner)
		if err != nil {
			t.Fatalf("Approve() error = %v", err)
		}
		if res.Text != "상신 완료" || len(res.Files) != 1 || !res.Files[0].Private || act.Status != StatusDone {
			t.Errorf("Approve() = %+v, %+v", res, act)
		}
		calls := env.p.callsTo("submit_purchase")
		if len(calls) != 2 || calls[1].req.Mode != ModeExecute || calls[1].req.Args["key"] != "p1" ||
			calls[1].req.Credentials["password"] != testPassword || string(calls[1].files["photo.jpg"]) != "jpeg" {
			t.Fatalf("calls = %+v", calls)
		}
		if _, err := os.Stat(calls[1].req.Attachments[0].Path); !os.IsNotExist(err) {
			t.Error("attachments not removed after the action finished")
		}
		row, _ := env.store.GetPluginAction(context.Background(), id)
		if row.Status != "done" || !strings.Contains(row.Result, "D1") {
			t.Errorf("stored action = %+v", row)
		}
		if _, _, err := env.reg.Approve(context.Background(), id, owner); !errors.Is(err, ErrNotPending) {
			t.Errorf("second Approve() = %v, want ErrNotPending", err)
		}
		if n := len(env.p.callsTo("submit_purchase")); n != 2 {
			t.Errorf("submit_purchase called %d times, want 2 (preview + one execute)", n)
		}
	})

	for _, tc := range []struct {
		name string
		by   Identity
	}{
		{"another user is refused", stranger},
		{"same user ID on another platform is refused", sameIDOtherPlatform},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.connect(t, false)
			id := env.submit(t, nil)
			if _, _, err := env.reg.Approve(context.Background(), id, tc.by); !errors.Is(err, ErrNotOwner) {
				t.Errorf("Approve() = %v, want ErrNotOwner", err)
			}
			if _, err := env.reg.Cancel(context.Background(), id, tc.by); !errors.Is(err, ErrNotOwner) {
				t.Errorf("Cancel() = %v, want ErrNotOwner", err)
			}
			if n := len(env.p.callsTo("submit_purchase")); n != 1 {
				t.Errorf("submit_purchase called %d times, want only the preview", n)
			}
			row, _ := env.store.GetPluginAction(context.Background(), id)
			if row.Status != "pending" {
				t.Errorf("status = %s, want pending", row.Status)
			}
		})
	}

	t.Run("concurrent double click executes once", func(t *testing.T) {
		env := newTestEnv(t)
		env.connect(t, false)
		id := env.submit(t, nil)
		var wg sync.WaitGroup
		var ok, notPending atomic.Int32
		for range 6 {
			wg.Go(func() {
				_, _, err := env.reg.Approve(context.Background(), id, owner)
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ErrNotPending):
					notPending.Add(1)
				default:
					t.Errorf("Approve() = %v", err)
				}
			})
		}
		wg.Wait()
		executes := 0
		for _, c := range env.p.callsTo("submit_purchase") {
			if c.req.Mode == ModeExecute {
				executes++
			}
		}
		if ok.Load() != 1 || notPending.Load() != 5 || executes != 1 {
			t.Errorf("ok=%d notPending=%d executes=%d; want 1, 5, 1", ok.Load(), notPending.Load(), executes)
		}
	})

	t.Run("expired action is refused", func(t *testing.T) {
		env := newTestEnv(t)
		env.connect(t, false)
		id := env.submit(t, map[string][]byte{"photo.jpg": []byte("jpeg")})
		path := env.p.callsTo("submit_purchase")[0].req.Attachments[0].Path
		env.now = env.now.Add(31 * time.Minute)
		if _, _, err := env.reg.Approve(context.Background(), id, owner); !errors.Is(err, ErrExpired) {
			t.Errorf("Approve() = %v, want ErrExpired", err)
		}
		row, _ := env.store.GetPluginAction(context.Background(), id)
		if row.Status != "expired" {
			t.Errorf("status = %s, want expired", row.Status)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("attachments of an expired action were kept")
		}
		if n := len(env.p.callsTo("submit_purchase")); n != 1 {
			t.Errorf("submit_purchase called %d times, want only the preview", n)
		}
	})

	t.Run("unknown action", func(t *testing.T) {
		env := newTestEnv(t)
		if _, _, err := env.reg.Approve(context.Background(), "nope", owner); !errors.Is(err, ErrActionNotFound) {
			t.Errorf("Approve() = %v, want ErrActionNotFound", err)
		}
	})

	t.Run("failure before submit reopens the action", func(t *testing.T) {
		env := newTestEnv(t)
		env.connect(t, false)
		id := env.submit(t, nil)
		env.p.respond = func(req CallRequest) (*Result, error) {
			return nil, &Error{Code: CodeDuplicateSession, Message: "다른 곳에서 접속 중"}
		}
		if _, act, err := env.reg.Approve(context.Background(), id, owner); !errors.Is(err, ErrDuplicateSession) || act.Status != StatusPending {
			t.Fatalf("Approve() = %v, %+v; want ErrDuplicateSession and pending", err, act)
		}
		// Sign in with the kick, then approve again.
		env.p.respond = nil
		if _, _, err := env.reg.LoginAndResume(context.Background(), id, owner); err != nil {
			t.Fatalf("LoginAndResume(call) error = %v", err)
		}
		if logins := env.p.callsTo("login"); len(logins) != 1 || !logins[0].req.AllowKick {
			t.Errorf("login calls = %+v", logins)
		}
		if _, act, err := env.reg.Approve(context.Background(), id, owner); err != nil || act.Status != StatusDone {
			t.Errorf("Approve() after login = %+v, %v", act, err)
		}
	})

	t.Run("failure during submit fails the action", func(t *testing.T) {
		env := newTestEnv(t)
		env.connect(t, false)
		id := env.submit(t, nil)
		env.p.respond = func(req CallRequest) (*Result, error) {
			return nil, &Error{Code: CodeUpstream, Message: "다우오피스 응답 없음"}
		}
		_, act, err := env.reg.Approve(context.Background(), id, owner)
		if !errors.Is(err, &Error{Code: CodeUpstream}) || act.Status != StatusFailed {
			t.Errorf("Approve() = %v, %+v", err, act)
		}
		if _, _, err := env.reg.Approve(context.Background(), id, owner); !errors.Is(err, ErrNotPending) {
			t.Errorf("Approve() after failure = %v, want ErrNotPending", err)
		}
	})
}

func TestCancel(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, false)
	id := env.submit(t, map[string][]byte{"photo.jpg": []byte("jpeg")})
	path := env.p.callsTo("submit_purchase")[0].req.Attachments[0].Path
	act, err := env.reg.Cancel(context.Background(), id, owner)
	if err != nil || act.Status != StatusCancelled {
		t.Fatalf("Cancel() = %+v, %v", act, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("attachments of a cancelled action were kept")
	}
	if _, _, err := env.reg.Approve(context.Background(), id, owner); !errors.Is(err, ErrNotPending) {
		t.Errorf("Approve() after cancel = %v, want ErrNotPending", err)
	}
	if _, err := env.reg.Cancel(context.Background(), id, owner); !errors.Is(err, ErrNotPending) {
		t.Errorf("second Cancel() = %v, want ErrNotPending", err)
	}
	if p, _ := env.reg.PendingInThread(context.Background(), owner); p != nil {
		t.Errorf("PendingInThread() after cancel = %+v", p)
	}
}

func TestExpire(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, false)
	id := env.submit(t, map[string][]byte{"photo.jpg": []byte("jpeg")})
	path := env.p.callsTo("submit_purchase")[0].req.Attachments[0].Path

	if got, err := env.reg.Expire(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("Expire() before the deadline = %+v, %v", got, err)
	}
	env.now = env.now.Add(time.Hour)
	got, err := env.reg.Expire(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].Status != StatusExpired || got[0].Origin.Channel != "C1" {
		t.Fatalf("Expire() = %+v, %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("attachments of an expired action were kept")
	}
}

func TestDuplicateSessionLoginKick(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, false)
	env.p.respond = func(req CallRequest) (*Result, error) {
		if req.Tool == "login" {
			return &Result{Text: "로그인"}, nil
		}
		return nil, &Error{Code: CodeDuplicateSession, Message: "다른 곳에서 접속 중"}
	}
	ctx, prompts := requestCtx(owner)
	out, err := env.reg.Dispatch(ctx, "testoffice_save_photos", nil, map[string][]byte{"a.jpg": []byte("jpeg")})
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if out["status"] != "duplicate_session" {
		t.Errorf("result = %v", out)
	}
	list := prompts.List()
	if len(list) != 1 || list[0].Kind != PromptLoginKick || list[0].ActionID == "" || list[0].Text == "" {
		t.Fatalf("prompts = %+v; want one login_kick prompt", list)
	}
	id := list[0].ActionID

	if _, _, err := env.reg.LoginAndResume(context.Background(), id, Identity{Platform: "slack", UserID: "U9"}); !errors.Is(err, ErrNotOwner) {
		t.Errorf("LoginAndResume(stranger) = %v, want ErrNotOwner", err)
	}
	if _, _, err := env.reg.Approve(context.Background(), id, owner); err == nil {
		t.Error("Approve() accepted a login_kick action")
	}

	act, files, err := env.reg.LoginAndResume(context.Background(), id, owner)
	if err != nil {
		t.Fatalf("LoginAndResume() error = %v", err)
	}
	if act.Origin.Text != "구매 품의 올려줘" || act.Origin.ThreadID != "100.1" || string(files["a.jpg"]) != "jpeg" || act.Status != StatusDone {
		t.Errorf("LoginAndResume() = %+v, files %v", act, files)
	}
	logins := env.p.callsTo("login")
	if len(logins) != 1 || !logins[0].req.AllowKick || logins[0].req.Credentials["password"] != testPassword {
		t.Errorf("login calls = %+v", logins)
	}
	if _, _, err := env.reg.LoginAndResume(context.Background(), id, owner); !errors.Is(err, ErrNotPending) {
		t.Errorf("second LoginAndResume() = %v, want ErrNotPending", err)
	}
}

func TestAuthFailedMarksInvalid(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, false)
	env.p.respond = func(req CallRequest) (*Result, error) {
		return nil, &Error{Code: CodeAuthFailed, Message: "비밀번호가 틀렸습니다"}
	}
	ctx, prompts := requestCtx(owner)
	out, err := env.reg.Dispatch(ctx, "testoffice_card_usage", nil, nil)
	if err != nil || out["status"] != "auth_failed" {
		t.Fatalf("Dispatch() = %v, %v", out, err)
	}
	acct, _ := env.store.GetPluginAccount(context.Background(), "slack:U1", "testoffice")
	if acct.Status != "invalid" {
		t.Errorf("account status = %s, want invalid", acct.Status)
	}
	if list := prompts.List(); len(list) != 1 || list[0].Kind != PromptConnect {
		t.Errorf("prompts = %+v; want a reconnect prompt", list)
	}

	// No retry: the next call does not reach the plugin.
	before := len(env.p.calls)
	out, _ = env.reg.Dispatch(ctx, "testoffice_card_usage", nil, nil)
	if out["status"] != "auth_failed" || len(env.p.calls) != before {
		t.Errorf("second Dispatch() = %v, plugin calls %d -> %d", out, before, len(env.p.calls))
	}
}

func TestConnect(t *testing.T) {
	t.Run("stores sealed credentials", func(t *testing.T) {
		env := newTestEnv(t)
		env.connect(t, true)
		c := env.p.callsTo("connect")
		if len(c) != 1 || c[0].req.Args["username"] != "hong" || c[0].req.Args["password"] != testPassword ||
			c[0].req.Args["allowKick"] != true || c[0].req.AllowKick {
			t.Fatalf("connect call = %+v", c)
		}
		acct, _ := env.store.GetPluginAccount(context.Background(), "slack:U1", "testoffice")
		if acct == nil || acct.AccountName != "홍길동 (개발본부)" || acct.AccountID != "hong" || !acct.AllowKick || acct.Status != "ok" {
			t.Fatalf("account = %+v", acct)
		}
		if bytes.Contains(acct.Secret, []byte(testPassword)) {
			t.Error("password stored in clear")
		}

		ctx, _ := requestCtx(owner)
		out, err := env.reg.Dispatch(ctx, "accounts_status", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s := stringify(out); !strings.Contains(s, "홍길동 (개발본부)") || strings.Contains(s, testPassword) || strings.Contains(s, "hong\"") {
			t.Errorf("accounts_status = %s", s)
		}
	})

	t.Run("reconnect clears invalid status", func(t *testing.T) {
		env := newTestEnv(t)
		env.connect(t, false)
		if err := env.store.SetPluginAccountStatus(context.Background(), "slack:U1", "testoffice", "invalid"); err != nil {
			t.Fatal(err)
		}
		env.connect(t, false)
		acct, _ := env.store.GetPluginAccount(context.Background(), "slack:U1", "testoffice")
		if acct.Status != "ok" {
			t.Errorf("status = %s, want ok", acct.Status)
		}
	})

	for _, tc := range []struct {
		name      string
		values    map[string]string
		respond   error
		wantErr   error
		wantStore bool
	}{
		{"wrong password stores nothing", map[string]string{"username": "hong", "password": testPassword}, &Error{Code: CodeAuthFailed, Message: "틀림"}, ErrAuthFailed, false},
		{"live session elsewhere still counts as verified", map[string]string{"username": "hong", "password": testPassword}, &Error{Code: CodeDuplicateSession}, nil, true},
		{"missing password", map[string]string{"username": "hong"}, nil, &Error{Code: CodeValidation}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.p.respond = func(CallRequest) (*Result, error) { return nil, tc.respond }
			name, err := env.reg.Connect(context.Background(), owner, "testoffice", tc.values)
			if tc.wantErr == nil && err != nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Connect() = %q, %v; want %v", name, err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), testPassword) {
				t.Error("error message contains the password")
			}
			acct, _ := env.store.GetPluginAccount(context.Background(), "slack:U1", "testoffice")
			if (acct != nil) != tc.wantStore {
				t.Errorf("account stored = %v, want %v", acct != nil, tc.wantStore)
			}
			if tc.wantStore && name != "hong" {
				t.Errorf("account name = %q, want the login ID as a fallback", name)
			}
		})
	}

	t.Run("unknown plugin", func(t *testing.T) {
		env := newTestEnv(t)
		if _, err := env.reg.Connect(context.Background(), owner, "nope", nil); !errors.Is(err, ErrUnknownPlugin) {
			t.Errorf("Connect() = %v, want ErrUnknownPlugin", err)
		}
	})
}

func TestAccountTools(t *testing.T) {
	env := newTestEnv(t)
	ctx, prompts := requestCtx(owner)
	out, err := env.reg.Dispatch(ctx, "accounts_connect", map[string]any{"plugin": "testoffice"}, nil)
	if err != nil || out["status"] != "connect_prompt_sent" {
		t.Fatalf("accounts_connect = %v, %v", out, err)
	}
	if list := prompts.List(); len(list) != 1 || list[0].Kind != PromptConnect {
		t.Errorf("prompts = %+v", list)
	}
	if len(env.p.calls) != 0 {
		t.Error("accounts_connect called the plugin")
	}
	if _, err := env.reg.Dispatch(ctx, "accounts_connect", map[string]any{"plugin": "nope"}, nil); !errors.Is(err, ErrUnknownPlugin) {
		t.Errorf("accounts_connect(unknown) = %v", err)
	}

}

// accounts_disconnect can be triggered by text the model read, so it only
// posts a confirmation; the owner's button press removes the account.
func TestDisconnectNeedsConfirmation(t *testing.T) {
	env := newTestEnv(t)
	ctx, prompts := requestCtx(owner)
	out, err := env.reg.Dispatch(ctx, "accounts_disconnect", map[string]any{}, nil)
	if err != nil || out["status"] != "not_connected" || len(prompts.List()) != 0 {
		t.Fatalf("disconnect without an account = %v, %v, prompts %+v", out, err, prompts.List())
	}

	env.connect(t, false)
	stored := func() bool {
		acct, _ := env.store.GetPluginAccount(context.Background(), "slack:U1", "testoffice")
		return acct != nil
	}
	ask := func() string {
		ctx, prompts := requestCtx(owner)
		out, err := env.reg.Dispatch(ctx, "accounts_disconnect", map[string]any{"plugin": "testoffice"}, nil)
		if err != nil || out["status"] != "awaiting_confirmation" || !strings.Contains(out["message"].(string), "Nothing was changed") {
			t.Fatalf("accounts_disconnect = %v, %v", out, err)
		}
		list := prompts.List()
		if len(list) != 1 || list[0].Kind != PromptDisconnect || list[0].ActionID == "" || !strings.Contains(list[0].Text, "홍길동 (개발본부)") {
			t.Fatalf("prompts = %+v", list)
		}
		return list[0].ActionID
	}

	id := ask()
	if !stored() {
		t.Fatal("account removed before confirmation")
	}
	if _, _, err := env.reg.Approve(context.Background(), id, Identity{Platform: "slack", UserID: "U9"}); !errors.Is(err, ErrNotOwner) || !stored() {
		t.Errorf("stranger's confirmation = %v; account stored %v", err, stored())
	}
	if _, err := env.reg.Cancel(context.Background(), id, owner); err != nil || !stored() {
		t.Errorf("cancel = %v; account stored %v", err, stored())
	}

	id = ask()
	res, act, err := env.reg.Approve(context.Background(), id, owner)
	if err != nil || act.Status != StatusDone || !strings.Contains(res.Text, "연결을 해제했습니다") {
		t.Fatalf("Approve(disconnect) = %+v, %+v, %v", res, act, err)
	}
	if stored() {
		t.Error("account still stored after a confirmed disconnect")
	}
	if len(env.p.calls) != 1 { // only the connect call
		t.Errorf("plugin calls = %d; disconnecting should not call the plugin", len(env.p.calls))
	}
}

func TestDispatchDecodesJSONArgs(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, false)
	env.p.tools = append(env.p.tools,
		Tool{Name: "tag", Description: "Tag", JSONArgs: [][]string{{"meta"}}},
		Tool{Name: "file_claim", Description: "File", Write: true, JSONArgs: [][]string{{"rows", "[]", "extra"}}},
	)
	ctx, prompts := requestCtx(owner)
	if _, err := env.reg.Dispatch(ctx, "testoffice_tag", map[string]any{"meta": `{"color":"red"}`}, nil); err != nil {
		t.Fatal(err)
	}
	if got := env.p.callsTo("tag")[0].req.Args["meta"]; !reflect.DeepEqual(got, map[string]any{"color": "red"}) {
		t.Errorf("read call got meta %#v, want the decoded object", got)
	}

	if _, err := env.reg.Dispatch(ctx, "testoffice_file_claim",
		map[string]any{"rows": []any{map[string]any{"extra": `{"n":1}`}}}, nil); err != nil {
		t.Fatal(err)
	}
	id := prompts.List()[0].ActionID
	if _, _, err := env.reg.Approve(context.Background(), id, owner); err != nil {
		t.Fatal(err)
	}
	calls := env.p.callsTo("file_claim")
	want := []any{map[string]any{"extra": map[string]any{"n": float64(1)}}}
	for _, c := range calls {
		if !reflect.DeepEqual(c.req.Args["rows"], want) {
			t.Errorf("%s call got rows %#v, want decoded objects", c.req.Mode, c.req.Args["rows"])
		}
	}
	if len(calls) != 2 {
		t.Errorf("file_claim calls = %d, want preview and execute", len(calls))
	}
}

// A changed PLUGIN_SECRET_KEY makes every stored account unreadable. That is
// a server problem: accounts must not be marked invalid, and nobody is asked
// to reconnect.
func TestVaultKeyChange(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, false)
	id := env.submit(t, nil)

	logs := &syncBuffer{}
	other, err := NewVault(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(env.store, other, slog.New(slog.NewTextHandler(logs, nil)))
	if err := reg.Register(env.p); err != nil {
		t.Fatal(err)
	}
	before := len(env.p.calls)
	for range 2 {
		ctx, prompts := requestCtx(owner)
		out, err := reg.Dispatch(ctx, "testoffice_card_usage", nil, nil)
		if err != nil || out["status"] != "error" || out["code"] != "SERVER_CONFIG" || out["message"] != "서버 설정 문제로 계정을 읽지 못했습니다. 관리자에게 알려 주세요." {
			t.Fatalf("Dispatch() = %v, %v", out, err)
		}
		if len(prompts.List()) != 0 {
			t.Errorf("prompts = %+v; nobody should be asked to reconnect", prompts.List())
		}
	}
	if acct, _ := env.store.GetPluginAccount(context.Background(), "slack:U1", "testoffice"); acct.Status != "ok" {
		t.Errorf("account status = %s, want ok", acct.Status)
	}
	if n := strings.Count(logs.String(), "PLUGIN_SECRET_KEY"); n != 1 {
		t.Errorf("key warning logged %d times, want once:\n%s", n, logs.String())
	}
	if _, act, err := reg.Approve(context.Background(), id, owner); !errors.Is(err, ErrVaultOpen) || act.Status != StatusPending {
		t.Errorf("Approve() = %v, %+v; want ErrVaultOpen with the action pending again", err, act)
	}
	if len(env.p.calls) != before {
		t.Error("the plugin was called without credentials")
	}
}

func TestSecretsRedactedFromErrors(t *testing.T) {
	t.Run("read call error", func(t *testing.T) {
		env := newTestEnv(t)
		env.connect(t, false)
		env.p.respond = func(CallRequest) (*Result, error) { return nil, errors.New("boom with " + testPassword) }
		ctx, _ := requestCtx(owner)
		_, err := env.reg.Dispatch(ctx, "testoffice_card_usage", nil, nil)
		if err == nil || strings.Contains(err.Error(), testPassword) {
			t.Errorf("Dispatch() error = %v", err)
		}
	})

	t.Run("connect error", func(t *testing.T) {
		for _, cause := range []error{
			&Error{Code: CodeUpstream, Message: "rejected " + testPassword},
			errors.New("pipe " + testPassword),
		} {
			env := newTestEnv(t)
			env.p.respond = func(CallRequest) (*Result, error) { return nil, cause }
			_, err := env.reg.Connect(context.Background(), owner, "testoffice", map[string]string{"username": "hong", "password": testPassword})
			if err == nil || strings.Contains(err.Error(), testPassword) {
				t.Errorf("Connect() error = %v", err)
			}
		}
	})

	t.Run("login error stored on the action", func(t *testing.T) {
		env := newTestEnv(t)
		env.connect(t, false)
		env.p.respond = func(req CallRequest) (*Result, error) {
			if req.Tool == "login" {
				return nil, errors.New("login said " + testPassword)
			}
			return nil, &Error{Code: CodeDuplicateSession}
		}
		ctx, prompts := requestCtx(owner)
		if _, err := env.reg.Dispatch(ctx, "testoffice_save_photos", nil, nil); err != nil {
			t.Fatal(err)
		}
		id := prompts.List()[0].ActionID
		_, _, err := env.reg.LoginAndResume(context.Background(), id, owner)
		if err == nil || strings.Contains(err.Error(), testPassword) {
			t.Errorf("LoginAndResume() error = %v", err)
		}
		row, _ := env.store.GetPluginAction(context.Background(), id)
		if row.Status != "failed" || strings.Contains(row.Result, testPassword) {
			t.Errorf("stored action = %s %q", row.Status, row.Result)
		}
	})
}

// An execute that runs into its deadline must still be marked failed; the
// status write gets its own context.
func TestApproveDeadlineStillFinishes(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, false)
	id := env.submit(t, nil)
	env.reg.executeTimeout = 50 * time.Millisecond
	env.p.blockExecute = true
	_, act, err := env.reg.Approve(context.Background(), id, owner)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Approve() error = %v, want deadline exceeded", err)
	}
	row, _ := env.store.GetPluginAction(context.Background(), id)
	if row.Status != "failed" || act.Status != StatusFailed {
		t.Errorf("status = %s / %s, want failed (not stuck running)", row.Status, act.Status)
	}
}

func TestPromptsDedupeConnect(t *testing.T) {
	_, p := ContextWithPrompts(context.Background())
	p.add(Prompt{Kind: PromptConnect, UserKey: "slack:U1", Plugin: "a", Text: "x"})
	p.add(Prompt{Kind: PromptConnect, UserKey: "slack:U1", Plugin: "a", Text: "y"})
	p.add(Prompt{Kind: PromptConnect, UserKey: "slack:U1", Plugin: "b", Text: "z"})
	p.add(Prompt{Kind: PromptApproval, UserKey: "slack:U1", Plugin: "a", ActionID: "1", Text: "w"})
	if got := p.List(); len(got) != 3 {
		t.Errorf("List() = %+v; want 3 prompts", got)
	}
	var nilPrompts *Prompts
	if nilPrompts.add(Prompt{}) || nilPrompts.List() != nil {
		t.Error("nil Prompts should drop prompts")
	}
}

func TestCallRequestLogValueHidesCredentials(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("call", "req", CallRequest{Tool: "x", Credentials: map[string]string{"password": testPassword}, Args: map[string]any{"a": testPassword}})
	if strings.Contains(buf.String(), testPassword) {
		t.Errorf("log contains a secret: %s", buf.String())
	}
}

func TestSanitizeInboxName(t *testing.T) {
	tests := map[string]string{
		"photo.jpg":                       "photo.jpg",
		"../../etc/passwd":                "passwd",
		`C:\Users\a\b.png`:                "b.png",
		"..":                              "file",
		"":                                "file",
		"a<b>:c?.txt":                     "a_b__c_.txt",
		"line\nbreak.jpg":                 "line_break.jpg",
		strings.Repeat("가", 150) + ".jpg": strings.Repeat("가", 96) + ".jpg",
	}
	for in, want := range tests {
		if got := sanitizeInboxName(in); got != want {
			t.Errorf("sanitizeInboxName(%q) = %q, want %q", in, got, want)
		}
	}
	used := map[string]bool{}
	if a, b := uniqueName("a.jpg", used), uniqueName("A.jpg", used); a != "a.jpg" || b != "A (2).jpg" {
		t.Errorf("uniqueName = %q, %q", a, b)
	}
}

func stringify(v any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				b.WriteString(k + "\"")
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case []map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []string:
			for _, e := range x {
				b.WriteString(e + "\"")
			}
		case string:
			b.WriteString(x + "\"")
		default:
			b.WriteString("?")
		}
	}
	walk(v)
	return b.String()
}

func TestActionAndRecoverStale(t *testing.T) {
	env := newTestEnv(t)
	env.connect(t, false)
	id := env.submit(t, map[string][]byte{"photo.jpg": []byte("jpeg")})
	path := env.p.callsTo("submit_purchase")[0].req.Attachments[0].Path

	act, err := env.reg.Action(context.Background(), id)
	if err != nil || act.ID != id || act.UserKey != "slack:U1" || act.Status != StatusPending || act.Preview != "품의서 미리보기" {
		t.Fatalf("Action() = %+v, %v", act, err)
	}
	if _, err := env.reg.Action(context.Background(), "nope"); !errors.Is(err, ErrActionNotFound) {
		t.Errorf("Action(missing) = %v, want ErrActionNotFound", err)
	}

	if ok, _ := env.store.TransitionPluginAction(context.Background(), id, "pending", "running", ""); !ok {
		t.Fatal("could not move the action to running")
	}
	if got, err := env.reg.RecoverStale(context.Background(), 10*time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("RecoverStale() right after start = %+v, %v; want none", got, err)
	}
	env.now = env.now.Add(11 * time.Minute)
	got, err := env.reg.RecoverStale(context.Background(), 10*time.Minute)
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].Status != StatusFailed {
		t.Fatalf("RecoverStale() = %+v, %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("attachments of a recovered action were kept")
	}
}
