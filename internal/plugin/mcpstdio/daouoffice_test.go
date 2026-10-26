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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/plugin"
	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
)

// daouofficeTools is every model tool of plugins/daouoffice and whether it
// waits for approval.
var daouofficeTools = map[string]bool{
	"guide": false, "me": false, "list_forms": false, "my_drafts": false, "read_document": false,
	"approval_inbox": false, "attendance_today": false, "leave_balance": false,
	"list_cards": false, "card_usage": false, "withdraw_document": true,
	"save_photos": false, "save_receipt": false, "mark_inspection": false, "mark_payment": false,
	"inspection_candidates": false, "payment_status": false,
	"file_inspection_report": true, "attach_photos_to_claim": true,
	"draft_purchase": false, "list_purchases": false, "capture_quote": false, "submit_purchase": true,
	"file_expense_claim": true, "file_trip": true, "list_trips": false,
	"issue_certificate": true, "certificate_history": false,
}

// startDaouoffice runs the real plugins/daouoffice server without a company
// config, so no tool can reach DaouOffice. It skips when node or the plugin's
// modules are missing.
func startDaouoffice(t *testing.T) (plugin.Plugin, *bytes.Buffer) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "plugins", "daouoffice"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"@modelcontextprotocol/server", "zod", "playwright", "sharp"} {
		if _, err := os.Stat(filepath.Join(root, "node_modules", pkg)); err != nil {
			t.Skipf("%s is not installed in plugins/daouoffice (run npm install there)", pkg)
		}
	}
	t.Setenv("DAOUOFFICE_CONFIG", "")
	dataDir := t.TempDir()
	logs := &bytes.Buffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := New(ctx, Config{
		Name: "daouoffice", Title: "다우오피스",
		Command: []string{node, "--disable-warning=ExperimentalWarning", filepath.Join(root, "src", "server.mjs")},
		Env:     []string{"DAOUOFFICE_DATA_DIR=" + dataDir},
		DataDir: dataDir,
	}, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatalf("New() error = %v\n%s", err, logs)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, logs
}

func TestDaouofficePlugin(t *testing.T) {
	p, logs := startDaouoffice(t)

	t.Run("tools and approval gating", func(t *testing.T) {
		seen := map[string]bool{}
		for _, tool := range p.Tools() {
			if tool.Role != "" {
				continue
			}
			seen[tool.Name] = true
			write, known := daouofficeTools[tool.Name]
			if !known {
				t.Errorf("unexpected tool %s (add it to daouofficeTools with its gating)", tool.Name)
				continue
			}
			if tool.Write != write {
				t.Errorf("%s: Write = %v, want %v", tool.Name, tool.Write, write)
			}
			if strings.TrimSpace(tool.Description) == "" {
				t.Errorf("%s has no description", tool.Name)
			}
		}
		var missing []string
		for name := range daouofficeTools {
			if !seen[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("missing tools %v", missing)
		}
	})

	t.Run("account form is login id and password", func(t *testing.T) {
		var secret, plain int
		for _, f := range p.AccountFields() {
			switch {
			case f.Secret:
				secret++
			case !f.Checkbox:
				plain++
			}
		}
		if secret != 1 || plain != 1 {
			t.Errorf("AccountFields() = %+v, want one id and one password", p.AccountFields())
		}
	})

	t.Run("registry declares every tool", func(t *testing.T) {
		store, err := storage.New(filepath.Join(t.TempDir(), "kb.db"), slog.New(slog.NewTextHandler(logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		vault, err := plugin.NewVault(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
		if err != nil {
			t.Fatal(err)
		}
		reg := plugin.NewRegistry(store, vault, slog.New(slog.NewTextHandler(logs, nil)))
		if err := reg.Register(p); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
		names := map[string]bool{}
		for _, d := range reg.Declarations() {
			names[d.Name] = true
		}
		for name := range daouofficeTools {
			if !names["daouoffice_"+name] {
				t.Errorf("declaration daouoffice_%s missing", name)
			}
		}
		if names["daouoffice_connect"] || names["daouoffice_login"] {
			t.Error("connect or login is visible to the model")
		}

		// Before connecting: the guide runs, a read tool asks the person to connect.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		id := plugin.Identity{Platform: "slack", UserID: "U1", UserName: "홍길동", Channel: "D1"}
		rctx, prompts := plugin.ContextWithPrompts(plugin.ContextWithIdentity(ctx, id))
		out, err := reg.Dispatch(rctx, "daouoffice_guide", map[string]any{"topic": "index"}, nil)
		if err != nil || out["status"] == "not_connected" {
			t.Fatalf("Dispatch(guide) before connecting = %v, %v", out, err)
		}
		if text, _ := out["text"].(string); !strings.Contains(text, "inspection") {
			t.Errorf("Dispatch(guide) = %v", out)
		}
		if len(prompts.List()) != 0 {
			t.Errorf("guide queued prompts %+v", prompts.List())
		}
		out, err = reg.Dispatch(rctx, "daouoffice_list_purchases", map[string]any{}, nil)
		if err != nil || out["status"] != "not_connected" {
			t.Errorf("Dispatch(list_purchases) before connecting = %v, %v; want not_connected", out, err)
		}
	})

	t.Run("write preview without credentials is not connected", func(t *testing.T) {
		_, err := call(t, p, plugin.CallRequest{Tool: "submit_purchase", Args: map[string]any{"key": "test", "revision": 1}, Mode: plugin.ModePreview})
		if !errors.Is(err, plugin.ErrNotConnected) {
			t.Errorf("submit_purchase without credentials error = %v, want NOT_CONNECTED", err)
		}
	})
}
