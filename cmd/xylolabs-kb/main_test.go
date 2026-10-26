package main

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/xylolabsinc/xylolabs-kb/internal/config"
	"github.com/xylolabsinc/xylolabs-kb/internal/plugin"
	"github.com/xylolabsinc/xylolabs-kb/internal/storage"
)

func TestNewPluginRegistry(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	store, err := storage.New(filepath.Join(t.TempDir(), "kb.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))

	tests := []struct {
		name    string
		cfg     config.Config
		bot     bool
		wantNil bool
	}{
		{"not enabled", config.Config{PluginSecretKey: key}, true, true},
		{"no bot", config.Config{DaouOfficeEnabled: true, PluginSecretKey: key}, false, true},
		{"no key", config.Config{DaouOfficeEnabled: true}, true, true},
		{"bad key", config.Config{DaouOfficeEnabled: true, PluginSecretKey: "short"}, true, true},
		{"ready", config.Config{DaouOfficeEnabled: true, PluginSecretKey: key}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newPluginRegistry(&tt.cfg, store, tt.bot, logger)
			if (got == nil) != tt.wantNil {
				t.Errorf("newPluginRegistry() = %v, want nil %v", got, tt.wantNil)
			}
		})
	}
}

// TestStartDaouOffice spawns the real plugin and checks that it registers.
// It needs node and plugins/daouoffice/node_modules; it never signs in to
// DaouOffice, because no tool is called.
func TestStartDaouOffice(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "plugins", "daouoffice", "src", "server.mjs")
	for _, p := range []string{script, filepath.Join(root, "plugins", "daouoffice", "node_modules", "@modelcontextprotocol", "server")} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s is missing", p)
		}
	}

	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	store, err := storage.New(filepath.Join(t.TempDir(), "kb.db"), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	vault, err := plugin.NewVault(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	reg := plugin.NewRegistry(store, vault, logger)
	t.Cleanup(func() { _ = reg.Close() })

	dataDir := filepath.Join(t.TempDir(), "data dir")
	cfg := &config.Config{
		DaouOfficeEnabled: true,
		DaouOfficeCommand: strconv.Quote(node) + " --disable-warning=ExperimentalWarning " + strconv.Quote(script),
		DaouOfficeDataDir: dataDir,
		DaouOfficeConfig:  filepath.Join(t.TempDir(), "missing.json"),
	}
	startDaouOffice(make(chan struct{}), cfg, reg, logger)

	plugins := reg.Plugins()
	if len(plugins) != 1 || plugins[0].Name() != "daouoffice" {
		t.Fatalf("plugins = %v; logs:\n%s", plugins, logs.String())
	}
	p := plugins[0]
	if len(p.AccountFields()) == 0 || len(reg.Declarations()) < 5 || p.Instructions() == "" {
		t.Errorf("account fields %d, declarations %d, instructions %q", len(p.AccountFields()), len(reg.Declarations()), p.Instructions())
	}
	for _, d := range reg.Declarations() {
		if strings.HasSuffix(d.Name, "_connect") && d.Name != "accounts_connect" || strings.HasSuffix(d.Name, "_login") {
			t.Errorf("hidden tool declared: %s", d.Name)
		}
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Errorf("data dir not created: %v", err)
	}
	if !strings.Contains(logs.String(), "company config not found") {
		t.Errorf("missing config was not logged:\n%s", logs.String())
	}
}

// lockedBuffer is a bytes.Buffer safe for the plugin's stderr goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
