package bot

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
	"github.com/xylolabsinc/xylolabs-kb/internal/plugin"
	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
	"github.com/xylolabsinc/xylolabs-kb/internal/tools"
)

const testPassword = "pa55word-테스트"

// event is one thing the fake platform was asked to do, in order.
type event struct {
	kind    string // reply, prompt, update, upload, form
	channel string
	thread  string
	message string // prompt ID (prompt) or target message (update)
	text    string
	buttons []Button
	form    Form
}

// fakePlatform records every call. Posting an empty body fails the test.
type fakePlatform struct {
	t      *testing.T
	mu     sync.Mutex
	events []event
	files  map[string][]byte // download URL → bytes
	seq    int
}

func (f *fakePlatform) record(e event) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	if e.kind == "prompt" {
		e.message = fmt.Sprintf("P%d", f.seq)
	}
	f.events = append(f.events, e)
	return e.message
}

func (f *fakePlatform) list(kind string) []event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []event
	for _, e := range f.events {
		if kind == "" || e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakePlatform) PostReply(_ context.Context, channel, threadID, text string) error {
	if strings.TrimSpace(text) == "" {
		f.t.Errorf("PostReply with an empty body in %s/%s", channel, threadID)
	}
	f.record(event{kind: "reply", channel: channel, thread: threadID, text: text})
	return nil
}

func (f *fakePlatform) UploadFile(_ context.Context, channel, threadID, filename string, _ []byte) error {
	f.record(event{kind: "upload", channel: channel, thread: threadID, text: filename})
	return nil
}

func (f *fakePlatform) PostPrompt(_ context.Context, channel, threadID string, p Prompt) (string, error) {
	if strings.TrimSpace(p.Text) == "" {
		f.t.Errorf("PostPrompt with an empty body in %s/%s", channel, threadID)
	}
	return f.record(event{kind: "prompt", channel: channel, thread: threadID, text: p.Text, buttons: p.Buttons}), nil
}

func (f *fakePlatform) UpdatePrompt(_ context.Context, channel, messageID, text string) error {
	if strings.TrimSpace(text) == "" {
		f.t.Errorf("UpdatePrompt with an empty body on %s", messageID)
	}
	f.record(event{kind: "update", channel: channel, message: messageID, text: text})
	return nil
}

func (f *fakePlatform) OpenForm(_ context.Context, trigger string, form Form) error {
	f.record(event{kind: "form", message: trigger, form: form})
	return nil
}

func (f *fakePlatform) OpenDM(_ context.Context, userID string) (string, error) {
	return "D-" + userID, nil
}
func (f *fakePlatform) AddReaction(context.Context, string, string, string) error {
	return nil
}
func (f *fakePlatform) ResolveUserName(_ context.Context, userID string) string {
	if userID == "U1" {
		return "홍길동"
	}
	return userID
}
func (f *fakePlatform) FetchThreadHistory(context.Context, string, string, string) []gemini.Message {
	return nil
}
func (f *fakePlatform) FetchThreadFiles(context.Context, string, string, string) map[string][]byte {
	return nil
}
func (f *fakePlatform) IsTrackedThread(string, string) bool { return false }
func (f *fakePlatform) TrackThread(string, string)          {}
func (f *fakePlatform) DownloadFile(_ context.Context, url string) ([]byte, error) {
	return f.files[url], nil
}
func (f *fakePlatform) CleanMentions(_ context.Context, text string) string { return text }
func (f *fakePlatform) ExtractURLs(string) []string                         { return nil }
func (f *fakePlatform) FormatResponse(text string) string                   { return text }
func (f *fakePlatform) StripBotMention(text string) string                  { return strings.TrimSpace(text) }
func (f *fakePlatform) Name() string                                        { return "slack" }
func (f *fakePlatform) BotUserID() string                                   { return "UBOT" }
func (f *fakePlatform) StartCleanup()                                       {}
func (f *fakePlatform) StopCleanup()                                        {}

// fakeGen is the model. It calls tool once per user turn, then answers.
type fakeGen struct {
	mu    sync.Mutex
	tool  string // tool to call on a fresh user turn; "" answers at once
	args  map[string]any
	reqs  []gemini.GenerateRequest
	calls atomic.Int32
}

func (g *fakeGen) Generate(_ context.Context, req gemini.GenerateRequest) (*gemini.GenerateResponse, error) {
	g.calls.Add(1)
	g.mu.Lock()
	g.reqs = append(g.reqs, req)
	tool, args := g.tool, g.args
	g.mu.Unlock()
	last := req.Messages[len(req.Messages)-1]
	if tool != "" && len(last.FunctionResponses) == 0 {
		return &gemini.GenerateResponse{FunctionCalls: []gemini.FunctionCall{{Name: tool, Args: args}}}, nil
	}
	return &gemini.GenerateResponse{Text: "답변입니다."}, nil
}

func (g *fakeGen) lastUserText() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.reqs) - 1; i >= 0; i-- {
		msgs := g.reqs[i].Messages
		for j := len(msgs) - 1; j >= 0; j-- {
			if msgs[j].Role == "user" && msgs[j].Content != "" {
				return msgs[j].Content
			}
		}
	}
	return ""
}

// testPlugin is a fake plugin. Write tools return a preview, or in execute
// mode a result with a private file. dupUntilLogin makes every call except
// login report another live session until login has run.
type testPlugin struct {
	mu            sync.Mutex
	calls         []plugin.CallRequest
	dataDir       string
	connectErr    error
	executeErrs   []error       // returned by successive execute calls of the write tool
	gate          chan struct{} // when set, write executes wait for it to close
	dupUntilLogin bool
	loggedIn      bool
}

func (p *testPlugin) Name() string         { return "testoffice" }
func (p *testPlugin) Title() string        { return "테스트오피스" }
func (p *testPlugin) Instructions() string { return "테스트오피스 도구 안내." }
func (p *testPlugin) DataDir() string      { return p.dataDir }
func (p *testPlugin) Close() error         { return nil }
func (p *testPlugin) Tools() []plugin.Tool {
	return []plugin.Tool{
		{Name: "card_usage", Description: "Card usage"},
		{Name: "save_photos", Description: "Save photos"},
		{Name: "submit_purchase", Description: "Submit", Write: true},
		{Name: "connect", Role: plugin.RoleConnect},
		{Name: "login", Role: plugin.RoleLogin},
	}
}
func (p *testPlugin) AccountFields() []plugin.AccountField {
	return []plugin.AccountField{
		{Name: "username", Label: "아이디"},
		{Name: "password", Label: "비밀번호", Secret: true},
		{Name: "allowKick", Label: "기존 접속을 끊어도 됩니다", Optional: true, Checkbox: true},
	}
}

func (p *testPlugin) Call(_ context.Context, req plugin.CallRequest) (*plugin.Result, error) {
	if p.gate != nil && req.Tool == "submit_purchase" && req.Mode == plugin.ModeExecute {
		<-p.gate
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	switch req.Tool {
	case "connect":
		if p.connectErr != nil {
			return nil, p.connectErr
		}
		return &plugin.Result{Text: "확인", Data: map[string]any{"accountName": "홍길동 (개발본부)", "accountId": "hong"}}, nil
	case "login":
		p.loggedIn = true
		return &plugin.Result{Text: "로그인"}, nil
	}
	if p.dupUntilLogin && !p.loggedIn {
		return nil, &plugin.Error{Code: plugin.CodeDuplicateSession, Message: "다른 곳에서 접속 중"}
	}
	if req.Tool == "submit_purchase" {
		if req.Mode != plugin.ModeExecute {
			return &plugin.Result{Text: "품의서 미리보기", Files: []plugin.File{{Name: "preview.png", Data: []byte("png")}}}, nil
		}
		if len(p.executeErrs) > 0 {
			err := p.executeErrs[0]
			p.executeErrs = p.executeErrs[1:]
			if err != nil {
				return nil, err
			}
		}
		return &plugin.Result{Text: "상신 완료: 문서 D1", Files: []plugin.File{{Name: "doc.pdf", Data: []byte("%PDF"), Private: true}}}, nil
	}
	return &plugin.Result{Text: "ok " + req.Tool}, nil
}

func (p *testPlugin) count(tool string, mode plugin.Mode) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c.Tool == tool && (mode == "" || c.Mode == mode) {
			n++
		}
	}
	return n
}

func (p *testPlugin) callsTo(tool string) []plugin.CallRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []plugin.CallRequest
	for _, c := range p.calls {
		if c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

type testBot struct {
	*Bot
	pf     *fakePlatform
	gen    *fakeGen
	plug   *testPlugin
	reg    *plugin.Registry
	store  *storage.SQLiteStore
	dbPath string
	logs   *syncBuffer
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

func newTestBot(t *testing.T) *testBot {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dbPath := filepath.Join(t.TempDir(), "kb.db")
	store, err := storage.New(dbPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	vault, err := plugin.NewVault(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	reg := plugin.NewRegistry(store, vault, logger)
	plug := &testPlugin{dataDir: t.TempDir()}
	if err := reg.Register(plug); err != nil {
		t.Fatal(err)
	}
	pf := &fakePlatform{t: t, files: map[string][]byte{}}
	gen := &fakeGen{}
	exec := tools.NewToolExecutor(nil, nil, "", nil, nil, nil, logger)
	exec.SetPlugins(reg)
	b := New(pf, nil, nil, "", "", "ko", time.UTC, logger)
	b.gemini = gen
	b.SetToolExecutor(exec)
	b.SetPlugins(reg)
	tb := &testBot{Bot: b, pf: pf, gen: gen, plug: plug, reg: reg, store: store, dbPath: dbPath, logs: logs}
	t.Cleanup(func() {
		b.Wait()
		if strings.Contains(logs.String(), testPassword) {
			t.Errorf("password appeared in logs")
		}
	})
	return tb
}

var owner = plugin.Identity{Platform: "slack", UserID: "U1", UserName: "홍길동"}

// connect stores an account for U1 through the registry.
func (tb *testBot) connect(t *testing.T) {
	t.Helper()
	if _, err := tb.reg.Connect(context.Background(), owner, "testoffice",
		map[string]string{"username": "hong", "password": testPassword}); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
}

func message(user, text, ts, thread string) *IncomingMessage {
	return &IncomingMessage{Platform: "slack", Channel: "C1", MessageID: ts, ThreadID: thread, UserID: user, Text: text}
}

// ask sends a message and waits for the bot's background work.
func (tb *testBot) ask(msg *IncomingMessage) {
	tb.HandleMention(context.Background(), msg)
	tb.Wait()
}

func (tb *testBot) submitForm(f *FormSubmission) {
	tb.HandleFormSubmission(context.Background(), f)
	tb.Wait()
}

func (tb *testBot) click(user, actionID, value, messageID string) {
	tb.HandleAction(context.Background(), &Action{
		Platform: "slack", UserID: user, Channel: "C1", MessageID: messageID, ActionID: actionID, Value: value, Trigger: "T1",
	})
	tb.Wait()
}

// requestApproval asks for a write and returns the approval prompt.
func (tb *testBot) requestApproval(t *testing.T) event {
	t.Helper()
	tb.gen.tool, tb.gen.args = "testoffice_submit_purchase", map[string]any{"key": "p1"}
	tb.ask(message("U1", "품의 올려줘", "1.0", ""))
	for _, p := range tb.pf.list("prompt") {
		if len(p.buttons) == 2 && p.buttons[0].ActionID == actionApprove {
			return p
		}
	}
	t.Fatalf("no approval prompt; events = %+v", tb.pf.list(""))
	return event{}
}

func TestFirstContactDMsOnce(t *testing.T) {
	tb := newTestBot(t)
	tb.ask(message("U1", "안녕", "1.0", ""))

	events := tb.pf.list("")
	if len(events) != 2 || events[0].kind != "reply" || events[0].channel != "C1" || events[0].thread != "1.0" {
		t.Fatalf("events = %+v; want the reply, then the DM", events)
	}
	dm := events[1]
	if dm.kind != "prompt" || dm.channel != "D-U1" || dm.thread != "" || len(dm.buttons) != 1 ||
		dm.buttons[0].ActionID != actionConnect || dm.buttons[0].Value != "testoffice" || !strings.Contains(dm.text, "테스트오피스") {
		t.Errorf("first-contact DM = %+v", dm)
	}

	tb.ask(message("U1", "또 안녕", "2.0", ""))
	if n := len(tb.pf.list("prompt")); n != 1 {
		t.Errorf("%d connect prompts after the second message, want 1", n)
	}
	for _, p := range tb.pf.list("prompt") {
		if !strings.HasPrefix(p.channel, "D-") {
			t.Errorf("connect prompt posted outside a DM: %+v", p)
		}
	}
}

func TestSystemPromptHasPluginInstructions(t *testing.T) {
	tb := newTestBot(t)
	tb.ask(message("U1", "안녕", "1.0", ""))
	tb.gen.mu.Lock()
	sys := tb.gen.reqs[0].SystemPrompt
	tb.gen.mu.Unlock()
	if !strings.Contains(sys, "테스트오피스 도구 안내.") || !strings.Contains(sys, "nothing is submitted until the user presses the button") {
		t.Errorf("system prompt lacks the plugin instructions")
	}
}

func TestConnectForm(t *testing.T) {
	t.Run("button opens the form", func(t *testing.T) {
		tb := newTestBot(t)
		tb.click("U1", actionConnect, "testoffice", "P1")
		forms := tb.pf.list("form")
		if len(forms) != 1 || forms[0].message != "T1" {
			t.Fatalf("forms = %+v", forms)
		}
		f := forms[0].form
		if f.CallbackID != formConnect || f.Metadata != "testoffice" || f.Intro == "" || len(f.Fields) != 3 {
			t.Fatalf("form = %+v", f)
		}
		if pw := f.Fields[1]; pw.ID != "password" || !pw.Secret || pw.Label != "비밀번호 (앱 비밀번호 권장)" {
			t.Errorf("password field = %+v", pw)
		}
		if cb := f.Fields[2]; !cb.Checkbox || !cb.Optional {
			t.Errorf("checkbox field = %+v", cb)
		}
	})

	t.Run("submission connects and reports", func(t *testing.T) {
		tb := newTestBot(t)
		tb.submitForm(&FormSubmission{
			Platform: "slack", UserID: "U1", CallbackID: formConnect, Metadata: "testoffice",
			Values: map[string]string{"username": "hong", "password": testPassword, "allowKick": "예"},
		})
		replies := tb.pf.list("reply")
		if len(replies) != 2 || replies[0].channel != "D-U1" || !strings.Contains(replies[0].text, "확인하는 중") ||
			replies[1].text != "연결했습니다: 홍길동 (개발본부)" {
			t.Fatalf("DMs = %+v", replies)
		}
		acct, _ := tb.store.GetPluginAccount(context.Background(), "slack:U1", "testoffice")
		if acct == nil || !acct.AllowKick {
			t.Errorf("account = %+v; want stored with allowKick from the checkbox", acct)
		}
	})

	t.Run("wrong password is reported once, without retry", func(t *testing.T) {
		tb := newTestBot(t)
		tb.plug.connectErr = &plugin.Error{Code: plugin.CodeAuthFailed, Message: "틀림"}
		tb.submitForm(&FormSubmission{
			Platform: "slack", UserID: "U1", CallbackID: formConnect, Metadata: "testoffice",
			Values: map[string]string{"username": "hong", "password": testPassword},
		})
		if n := tb.plug.count("connect", ""); n != 1 {
			t.Errorf("connect called %d times, want 1", n)
		}
		prompts := tb.pf.list("prompt")
		if len(prompts) != 1 || prompts[0].channel != "D-U1" || !strings.Contains(prompts[0].text, "아이디나 비밀번호가 맞지 않습니다") ||
			prompts[0].buttons[0].ActionID != actionConnect {
			t.Errorf("prompts = %+v; want a DM with a button to enter it again", prompts)
		}
		if acct, _ := tb.store.GetPluginAccount(context.Background(), "slack:U1", "testoffice"); acct != nil {
			t.Error("account stored after a wrong password")
		}
	})
}

func TestApproveFlow(t *testing.T) {
	tb := newTestBot(t)
	tb.connect(t)
	prompt := tb.requestApproval(t)
	id := prompt.buttons[0].Value

	// Reply first, then the preview file, then the buttons, all in the thread.
	var order []string
	for _, e := range tb.pf.list("") {
		if e.channel == "C1" {
			order = append(order, e.kind)
		}
	}
	if strings.Join(order, ",") != "reply,upload,prompt" {
		t.Errorf("thread events = %v; want reply, upload, prompt", order)
	}
	if prompt.thread != "1.0" || prompt.buttons[0].Label != "상신" || prompt.buttons[0].Style != "primary" ||
		prompt.buttons[1].ActionID != actionCancel || prompt.buttons[1].Style != "danger" || !strings.Contains(prompt.text, "품의서 미리보기") {
		t.Errorf("approval prompt = %+v", prompt)
	}
	if n := tb.plug.count("submit_purchase", plugin.ModeExecute); n != 0 {
		t.Fatalf("executed %d times before approval", n)
	}

	t.Run("non-owner is refused", func(t *testing.T) {
		before := len(tb.pf.list("update"))
		tb.click("U9", actionApprove, id, prompt.message)
		if len(tb.pf.list("update")) != before || tb.plug.count("submit_purchase", plugin.ModeExecute) != 0 {
			t.Fatal("a stranger's click changed something")
		}
		dms := tb.pf.list("reply")
		if last := dms[len(dms)-1]; last.channel != "D-U9" || last.text != textNotOwner {
			t.Errorf("notice to the stranger = %+v", last)
		}
	})

	t.Run("owner approves", func(t *testing.T) {
		tb.click("U1", actionApprove, id, prompt.message)
		updates := tb.pf.list("update")
		if len(updates) != 2 || updates[0].message != prompt.message || !strings.HasSuffix(updates[0].text, textSubmitting) ||
			!strings.HasSuffix(updates[1].text, textSubmitted) {
			t.Fatalf("updates = %+v", updates)
		}
		if n := tb.plug.count("submit_purchase", plugin.ModeExecute); n != 1 {
			t.Errorf("executed %d times, want 1", n)
		}
		var result, note bool
		for _, r := range tb.pf.list("reply") {
			if r.channel == "C1" && r.thread == "1.0" && r.text == "상신 완료: 문서 D1" {
				result = true
			}
			if r.channel == "C1" && strings.HasPrefix(r.text, textPrivateFiles) && strings.Contains(r.text, "doc.pdf") {
				note = true
			}
		}
		if !result || !note {
			t.Errorf("result posted = %v, private-file note = %v; replies = %+v", result, note, tb.pf.list("reply"))
		}
		var dmUpload bool
		for _, u := range tb.pf.list("upload") {
			if u.text == "doc.pdf" {
				dmUpload = u.channel == "D-U1"
			}
		}
		if !dmUpload {
			t.Errorf("private file not sent by DM: %+v", tb.pf.list("upload"))
		}
	})

	t.Run("a late second click only says it is done", func(t *testing.T) {
		before := len(tb.pf.list(""))
		tb.click("U1", actionApprove, id, prompt.message)
		events := tb.pf.list("")[before:]
		if len(events) != 1 || events[0].channel != "D-U1" || events[0].text != "이미 처리된 요청입니다 (상태: 완료)." ||
			tb.plug.count("submit_purchase", plugin.ModeExecute) != 1 {
			t.Errorf("events after a late click = %+v", events)
		}
	})
}

func TestDoubleClickExecutesOnce(t *testing.T) {
	tb := newTestBot(t)
	tb.connect(t)
	prompt := tb.requestApproval(t)
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			tb.HandleAction(context.Background(), &Action{
				Platform: "slack", UserID: "U1", Channel: "C1", MessageID: prompt.message,
				ActionID: actionApprove, Value: prompt.buttons[0].Value,
			})
		})
	}
	wg.Wait()
	tb.Wait()
	if n := tb.plug.count("submit_purchase", plugin.ModeExecute); n != 1 {
		t.Errorf("executed %d times, want 1", n)
	}
	final := 0
	for _, u := range tb.pf.list("update") {
		if strings.HasSuffix(u.text, textSubmitted) {
			final++
		}
	}
	if final != 1 {
		t.Errorf("%d final updates, want 1", final)
	}
}

func TestCancelButton(t *testing.T) {
	tb := newTestBot(t)
	tb.connect(t)
	prompt := tb.requestApproval(t)
	tb.click("U1", actionCancel, prompt.buttons[1].Value, prompt.message)
	updates := tb.pf.list("update")
	if len(updates) != 1 || !strings.HasSuffix(updates[0].text, textCancelled) {
		t.Errorf("updates = %+v", updates)
	}
	tb.click("U1", actionApprove, prompt.buttons[0].Value, prompt.message)
	if n := tb.plug.count("submit_purchase", plugin.ModeExecute); n != 0 {
		t.Errorf("a cancelled action executed %d times", n)
	}
}

func TestTextFallback(t *testing.T) {
	for _, tc := range []struct {
		word        string
		wantExecute int
		wantSuffix  string
	}{
		{"승인", 1, textSubmitted},
		{" OK ", 1, textSubmitted},
		{"진행", 1, textSubmitted},
		{"취소", 0, textCancelled},
	} {
		t.Run(strings.TrimSpace(tc.word), func(t *testing.T) {
			tb := newTestBot(t)
			tb.connect(t)
			prompt := tb.requestApproval(t)
			modelCalls := tb.gen.calls.Load()

			tb.ask(message("U1", tc.word, "2.0", "1.0"))
			if got := tb.gen.calls.Load(); got != modelCalls {
				t.Errorf("model called %d more times for a decision word", got-modelCalls)
			}
			if n := tb.plug.count("submit_purchase", plugin.ModeExecute); n != tc.wantExecute {
				t.Errorf("executed %d times, want %d", n, tc.wantExecute)
			}
			updates := tb.pf.list("update")
			if len(updates) == 0 || updates[len(updates)-1].message != prompt.message || !strings.HasSuffix(updates[len(updates)-1].text, tc.wantSuffix) {
				t.Errorf("updates = %+v", updates)
			}
		})
	}

	t.Run("other words and other people go to the model", func(t *testing.T) {
		tb := newTestBot(t)
		tb.connect(t)
		tb.requestApproval(t)
		tb.gen.tool = ""
		for _, msg := range []*IncomingMessage{
			message("U1", "승인해줘", "2.0", "1.0"),
			message("U9", "승인", "3.0", "1.0"),
			message("U1", "승인", "4.0", "9.9"), // another thread
		} {
			before := tb.gen.calls.Load()
			tb.ask(msg)
			if tb.gen.calls.Load() == before {
				t.Errorf("%q from %s in %s did not reach the model", msg.Text, msg.UserID, msg.ThreadID)
			}
		}
		if n := tb.plug.count("submit_purchase", plugin.ModeExecute); n != 0 {
			t.Errorf("executed %d times", n)
		}
	})
}

func TestLoginKickResumesTheMessage(t *testing.T) {
	tb := newTestBot(t)
	tb.connect(t)
	tb.plug.dupUntilLogin = true
	tb.pf.files["https://files/a.jpg"] = []byte("\xff\xd8\xff\xe0 jpeg")
	tb.gen.tool, tb.gen.args = "testoffice_save_photos", map[string]any{}

	msg := message("U1", "사진 저장해줘", "1.0", "")
	msg.Files = []FileAttachment{{ID: "F1", Name: "a.jpg", MimeType: "image/jpeg", URL: "https://files/a.jpg"}}
	tb.ask(msg)

	var kick event
	for _, p := range tb.pf.list("prompt") {
		if len(p.buttons) == 2 && p.buttons[0].ActionID == actionKick {
			kick = p
		}
	}
	if kick.message == "" || kick.channel != "C1" || kick.buttons[0].Label != "끊고 진행" || kick.buttons[1].ActionID != actionCancel {
		t.Fatalf("no login_kick prompt; events = %+v", tb.pf.list(""))
	}
	if n := tb.plug.count("login", ""); n != 0 {
		t.Fatalf("login ran before the person agreed")
	}

	modelCalls := tb.gen.calls.Load()
	tb.click("U1", actionKick, kick.buttons[0].Value, kick.message)

	logins := tb.plug.callsTo("login")
	if len(logins) != 1 || !logins[0].AllowKick {
		t.Fatalf("login calls = %+v", logins)
	}
	if tb.gen.calls.Load() == modelCalls {
		t.Fatal("the stored message was not run again")
	}
	if got := tb.gen.lastUserText(); !strings.Contains(got, "사진 저장해줘") {
		t.Errorf("re-run asked the model %q", got)
	}
	saves := tb.plug.callsTo("save_photos")
	if len(saves) != 2 || len(saves[1].Attachments) != 1 || saves[1].Attachments[0].Name != "a.jpg" {
		t.Errorf("save_photos calls = %+v; want a second call with the photo", saves)
	}
	var replied bool
	for _, r := range tb.pf.list("reply") {
		if r.channel == "C1" && r.thread == "1.0" && r.text == "답변입니다." {
			replied = true
		}
	}
	if !replied {
		t.Errorf("no answer in the original thread: %+v", tb.pf.list("reply"))
	}
}

func TestDuplicateSessionOnApproveOffersKick(t *testing.T) {
	tb := newTestBot(t)
	tb.connect(t)
	prompt := tb.requestApproval(t)
	tb.plug.mu.Lock()
	tb.plug.executeErrs = []error{&plugin.Error{Code: plugin.CodeDuplicateSession, Message: "다른 곳에서 접속 중"}}
	tb.plug.mu.Unlock()

	tb.click("U1", actionApprove, prompt.buttons[0].Value, prompt.message)
	var offer event
	for _, p := range tb.pf.list("prompt") {
		if len(p.buttons) == 2 && p.buttons[0].ActionID == actionKickApprove {
			offer = p
		}
	}
	if offer.message == "" || offer.buttons[0].Label != "끊고 상신" {
		t.Fatalf("no [끊고 상신] prompt; events = %+v", tb.pf.list(""))
	}

	tb.click("U1", actionKickApprove, offer.buttons[0].Value, offer.message)
	if logins := tb.plug.callsTo("login"); len(logins) != 1 || !logins[0].AllowKick {
		t.Errorf("login calls = %+v", logins)
	}
	if n := tb.plug.count("submit_purchase", plugin.ModeExecute); n != 2 {
		t.Errorf("execute calls = %d, want 2 (refused once, then done)", n)
	}
	updates := tb.pf.list("update")
	if last := updates[len(updates)-1]; last.message != offer.message || !strings.HasSuffix(last.text, textSubmitted) {
		t.Errorf("last update = %+v", last)
	}
}

// setActionTime moves an action's expiry or last update into the past.
func (tb *testBot) setActionTime(t *testing.T, id, column string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", tb.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE plugin_actions SET "+column+" = ? WHERE id = ?", at.UTC(), id); err != nil {
		t.Fatal(err)
	}
}

func runJanitorOnce(tb *testBot) {
	done := make(chan struct{})
	close(done)
	RunPluginJanitor(done, tb.reg, time.Hour, tb.logger, tb.Bot)
}

func TestJanitor(t *testing.T) {
	t.Run("expired action updates its prompt", func(t *testing.T) {
		tb := newTestBot(t)
		tb.connect(t)
		prompt := tb.requestApproval(t)
		tb.setActionTime(t, prompt.buttons[0].Value, "expires_at", time.Now().Add(-time.Minute))
		runJanitorOnce(tb)
		updates := tb.pf.list("update")
		if len(updates) != 1 || updates[0].message != prompt.message || !strings.HasSuffix(updates[0].text, textExpired) {
			t.Fatalf("updates = %+v", updates)
		}
		tb.click("U1", actionApprove, prompt.buttons[0].Value, prompt.message)
		if n := tb.plug.count("submit_purchase", plugin.ModeExecute); n != 0 {
			t.Errorf("an expired action executed %d times", n)
		}
	})

	t.Run("action left running is reported to its owner", func(t *testing.T) {
		tb := newTestBot(t)
		tb.connect(t)
		prompt := tb.requestApproval(t)
		id := prompt.buttons[0].Value
		if ok, _ := tb.store.TransitionPluginAction(context.Background(), id, "pending", "running", ""); !ok {
			t.Fatal("could not move the action to running")
		}
		tb.setActionTime(t, id, "updated_at", time.Now().Add(-time.Hour))
		runJanitorOnce(tb)
		var dm bool
		for _, r := range tb.pf.list("reply") {
			if r.channel == "D-U1" && strings.Contains(r.text, "결과를 확인하지 못했습니다") && strings.Contains(r.text, "품의서 미리보기") {
				dm = true
			}
		}
		if !dm {
			t.Errorf("owner not told; replies = %+v", tb.pf.list("reply"))
		}
		if updates := tb.pf.list("update"); len(updates) != 1 || updates[0].message != prompt.message {
			t.Errorf("updates = %+v", updates)
		}
		if act, _ := tb.reg.Action(context.Background(), id); act.Status != plugin.StatusFailed {
			t.Errorf("status = %s, want failed", act.Status)
		}
	})
}

func TestCheckboxValue(t *testing.T) {
	for in, want := range map[string]string{"true": "true", "예": "true", " Yes ": "true", "": "", "아니오": "", "false": ""} {
		if got := checkboxValue(in); got != want {
			t.Errorf("checkboxValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisconnectNeedsTheButton(t *testing.T) {
	tb := newTestBot(t)
	tb.connect(t)
	tb.gen.tool, tb.gen.args = "accounts_disconnect", map[string]any{"plugin": "testoffice"}
	tb.ask(message("U1", "다우오피스 연결 끊어줘", "1.0", ""))

	var prompt event
	for _, p := range tb.pf.list("prompt") {
		if len(p.buttons) == 2 && p.buttons[0].ActionID == actionDisconnect {
			prompt = p
		}
	}
	if prompt.message == "" || prompt.channel != "C1" || prompt.thread != "1.0" || prompt.buttons[0].Label != "연결 해제" || prompt.buttons[1].ActionID != actionCancel {
		t.Fatalf("no disconnect prompt; events = %+v", tb.pf.list(""))
	}
	stored := func() bool {
		acct, _ := tb.store.GetPluginAccount(context.Background(), "slack:U1", "testoffice")
		return acct != nil
	}
	if !stored() {
		t.Fatal("the model removed the account without the button")
	}

	tb.click("U9", actionDisconnect, prompt.buttons[0].Value, prompt.message)
	if !stored() {
		t.Fatal("a stranger's click removed the account")
	}
	tb.click("U1", actionDisconnect, prompt.buttons[0].Value, prompt.message)
	if stored() {
		t.Error("account still stored after the owner confirmed")
	}
	updates := tb.pf.list("update")
	if last := updates[len(updates)-1]; last.message != prompt.message || !strings.Contains(last.text, "연결을 해제했습니다") {
		t.Errorf("last update = %+v", last)
	}
}

func TestFailureAfterWriteIsUnknownOutcome(t *testing.T) {
	tb := newTestBot(t)
	tb.connect(t)
	prompt := tb.requestApproval(t)
	tb.plug.mu.Lock()
	tb.plug.executeErrs = []error{&plugin.Error{Code: plugin.CodeUpstream, Message: "중간에 끊겼습니다.", OutcomeUnknown: true}}
	tb.plug.mu.Unlock()
	tb.click("U1", actionApprove, prompt.buttons[0].Value, prompt.message)

	var said string
	for _, r := range tb.pf.list("reply") {
		if r.channel == "C1" && r.thread == "1.0" && strings.Contains(r.text, "중간에 끊겼습니다") {
			said = r.text
		}
	}
	if !strings.Contains(said, "결과를 확인하지 못했습니다") || strings.Contains(said, "상신하지 못했습니다") {
		t.Errorf("thread message = %q; want an unknown outcome, not a failure", said)
	}
}

func TestShutdownWaitsForRunningApproval(t *testing.T) {
	tb := newTestBot(t)
	tb.connect(t)
	prompt := tb.requestApproval(t)
	tb.plug.gate = make(chan struct{})

	start := time.Now()
	tb.HandleAction(context.Background(), &Action{
		Platform: "slack", UserID: "U1", Channel: "C1", MessageID: prompt.message, ActionID: actionApprove, Value: prompt.buttons[0].Value,
	})
	if time.Since(start) > time.Second {
		t.Fatal("HandleAction blocked on the submission; connectors would miss Slack's 3-second window")
	}

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if tb.Shutdown(short) {
		t.Fatal("Shutdown reported done while a submission was running")
	}
	before := len(tb.pf.list(""))
	tb.HandleAction(context.Background(), &Action{
		Platform: "slack", UserID: "U1", Channel: "C1", MessageID: prompt.message, ActionID: actionCancel, Value: prompt.buttons[1].Value,
	})

	close(tb.plug.gate)
	if !tb.Shutdown(context.Background()) {
		t.Fatal("Shutdown did not finish after the submission completed")
	}
	if n := tb.plug.count("submit_purchase", plugin.ModeExecute); n != 1 {
		t.Errorf("executed %d times, want 1", n)
	}
	for _, e := range tb.pf.list("")[before:] {
		if e.kind == "update" && strings.HasSuffix(e.text, textCancelled) {
			t.Error("a click after shutdown began was handled")
		}
	}
	updates := tb.pf.list("update")
	if last := updates[len(updates)-1]; !strings.HasSuffix(last.text, textSubmitted) {
		t.Errorf("last update = %+v; the running submission should have finished", last)
	}
}

func TestDeadButtonsSayWhy(t *testing.T) {
	t.Run("settled action", func(t *testing.T) {
		tb := newTestBot(t)
		tb.connect(t)
		prompt := tb.requestApproval(t)
		tb.click("U1", actionCancel, prompt.buttons[1].Value, prompt.message)
		before := len(tb.pf.list(""))
		tb.click("U1", actionApprove, prompt.buttons[0].Value, prompt.message)
		events := tb.pf.list("")[before:]
		if len(events) != 1 || events[0].channel != "D-U1" || events[0].text != "이미 처리된 요청입니다 (상태: 취소됨)." {
			t.Errorf("events = %+v", events)
		}
	})

	t.Run("unknown action", func(t *testing.T) {
		tb := newTestBot(t)
		tb.click("U1", actionApprove, "no-such-action", "P1")
		replies := tb.pf.list("reply")
		if len(replies) != 1 || replies[0].channel != "D-U1" || replies[0].text != "이미 처리되었거나 없는 요청입니다." {
			t.Errorf("replies = %+v", replies)
		}
	})

	t.Run("second click while the first runs stays silent", func(t *testing.T) {
		tb := newTestBot(t)
		tb.connect(t)
		prompt := tb.requestApproval(t)
		tb.plug.gate = make(chan struct{})
		click := func() {
			tb.HandleAction(context.Background(), &Action{
				Platform: "slack", UserID: "U1", Channel: "C1", MessageID: prompt.message, ActionID: actionApprove, Value: prompt.buttons[0].Value,
			})
		}
		click()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, busy := tb.inflight.Load(prompt.buttons[0].Value); busy {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("first click never started")
			}
			time.Sleep(time.Millisecond)
		}
		click()
		time.Sleep(50 * time.Millisecond)
		close(tb.plug.gate)
		tb.Wait()
		for _, r := range tb.pf.list("reply") {
			if strings.HasPrefix(r.text, "이미 처리된 요청입니다") {
				t.Errorf("in-flight double click got a notice: %+v", r)
			}
		}
		if n := tb.plug.count("submit_purchase", plugin.ModeExecute); n != 1 {
			t.Errorf("executed %d times, want 1", n)
		}
	})
}

func TestJanitorLogsPlatformsWithoutABot(t *testing.T) {
	tb := newTestBot(t)
	discordUser := plugin.Identity{Platform: "discord", UserID: "D7", Channel: "C9", ThreadID: "9"}
	if _, err := tb.reg.Connect(context.Background(), discordUser, "testoffice",
		map[string]string{"username": "kim", "password": testPassword}); err != nil {
		t.Fatal(err)
	}
	ctx, prompts := plugin.ContextWithPrompts(plugin.ContextWithIdentity(context.Background(), discordUser))
	if _, err := tb.reg.Dispatch(ctx, "testoffice_submit_purchase", map[string]any{"key": "p1"}, nil); err != nil {
		t.Fatal(err)
	}
	id := prompts.List()[0].ActionID
	tb.setActionTime(t, id, "expires_at", time.Now().Add(-time.Minute))
	runJanitorOnce(tb)
	if !strings.Contains(tb.logs.String(), "no bot for this platform") || !strings.Contains(tb.logs.String(), "platform=discord") {
		t.Errorf("no warning for the discord action; logs:\n%s", tb.logs.String())
	}
	if len(tb.pf.list("update")) != 0 {
		t.Error("the slack bot touched a discord prompt")
	}
}
