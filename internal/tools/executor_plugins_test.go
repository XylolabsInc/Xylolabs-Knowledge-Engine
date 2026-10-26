package tools

import (
	"context"
	"log/slog"
	"testing"

	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
	"github.com/xylolabsinc/xylolabs-kb/internal/plugin"
)

// stubPlugin is a plugin without accounts, so the registry needs no store.
type stubPlugin struct {
	name  string
	tools []plugin.Tool
	got   []plugin.CallRequest
}

func (s *stubPlugin) Name() string                         { return s.name }
func (s *stubPlugin) Title() string                        { return "스텁" }
func (s *stubPlugin) Instructions() string                 { return "" }
func (s *stubPlugin) Tools() []plugin.Tool                 { return s.tools }
func (s *stubPlugin) AccountFields() []plugin.AccountField { return nil }
func (s *stubPlugin) DataDir() string                      { return "" }
func (s *stubPlugin) Close() error                         { return nil }

func (s *stubPlugin) Call(_ context.Context, req plugin.CallRequest) (*plugin.Result, error) {
	s.got = append(s.got, req)
	return &plugin.Result{Text: "결과 " + req.Tool}, nil
}

func TestToolExecutorPlugins(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	reg := plugin.NewRegistry(nil, nil, logger)
	stub := &stubPlugin{name: "stub", tools: []plugin.Tool{
		{Name: "lookup", Description: "Look something up"},
		{Name: "connect", Role: plugin.RoleConnect},
	}}
	clash := &stubPlugin{name: "attach", tools: []plugin.Tool{{Name: "file", Description: "Clashes with attach_file"}}}
	for _, p := range []plugin.Plugin{stub, clash} {
		if err := reg.Register(p); err != nil {
			t.Fatal(err)
		}
	}

	e := newTestExecutor()
	before := len(e.Declarations())
	e.SetPlugins(reg)
	decls := e.Declarations()
	names := map[string]int{}
	for _, d := range decls {
		names[d.Name]++
	}
	if names["stub_lookup"] != 1 || names["stub_connect"] != 0 || names["attach_file"] != 1 {
		t.Errorf("declarations = %v", names)
	}
	if len(decls) != before+1 {
		t.Errorf("got %d declarations, want %d built-in + 1 plugin tool", len(decls), before)
	}

	ctx := plugin.ContextWithIdentity(context.Background(), plugin.Identity{Platform: "slack", UserID: "U1"})
	resp := e.Execute(ctx, gemini.FunctionCall{Name: "stub_lookup", Args: map[string]any{"q": "x"}})
	out, _ := resp.Response.(map[string]any)
	if out["status"] != "ok" || out["text"] != "결과 lookup" {
		t.Errorf("Execute(stub_lookup) = %v", resp.Response)
	}
	if len(stub.got) != 1 || stub.got[0].Mode != plugin.ModeExecute || stub.got[0].Args["q"] != "x" {
		t.Errorf("plugin calls = %+v", stub.got)
	}

	resp = e.Execute(ctx, gemini.FunctionCall{Name: "stub_connect"})
	if out, _ := resp.Response.(map[string]any); out["error"] == nil {
		t.Errorf("hidden tool was dispatched: %v", resp.Response)
	}
	e.Execute(ctx, gemini.FunctionCall{Name: "attach_file", Args: map[string]any{"file_name": "a.txt", "content": "x"}})
	if len(clash.got) != 0 {
		t.Error("a plugin tool shadowed a built-in tool")
	}
}
