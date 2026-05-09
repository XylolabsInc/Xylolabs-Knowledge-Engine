# AGENTS.md - AI Agent Guidelines for xylolabs-kb

## Project Overview

- **Language:** Go 1.26+
- **Module:** `github.com/xylolabsinc/xylolabs-kb`
- **Purpose:** Always-running knowledge base service with Slack, Discord, Google Workspace, and Notion connectors
- **Storage:** SQLite + FTS5 (pure Go via `modernc.org/sqlite`) — no CGO
- **API:** HTTP REST API served by the `internal/api` package
- **Entry point:** `cmd/xylolabs-kb/main.go`

---

## Architecture Rules

### Domain types are sacred

All core types live in `internal/kb/types.go`. This is the single source of truth.

- **Never duplicate** `Document`, `Attachment`, `SearchResult`, `SearchQuery`, `SyncState`, `Stats`, `Storage`, or `Connector` in other packages
- **Always import** `github.com/xylolabsinc/xylolabs-kb/internal/kb` for domain types
- Modifying `types.go` requires updating **all connectors** and **storage implementation** that depend on changed fields

### Interface contracts

| Interface | Defined in | Implemented in |
|-----------|-----------|----------------|
| `kb.Storage` | `internal/kb/types.go` | `internal/storage/` |
| `kb.Connector` | `internal/kb/types.go` | `internal/slack/`, `internal/google/`, `internal/notion/`, `internal/discord/` |
| `tools.MessagePoster` | `internal/tools/messenger.go` | `internal/bot/slack_platform.go`, `internal/bot/discord_messenger.go` |
| `tools.ChannelResolver` | `internal/tools/messenger.go` | `internal/bot/slack_platform.go`, `internal/bot/discord_messenger.go` |
| `bot.Platform` | `internal/bot/platform.go` | `internal/bot/slack_platform.go`, `internal/bot/discord_platform.go` |

Every connector must implement all four methods: `Name() Source`, `Start(done <-chan struct{}) error`, `Sync() error`, `Stop() error`.

### Package ownership

| Package | Responsibility | Do not put here |
|---------|---------------|-----------------|
| `internal/kb/` | Domain types and interfaces only | Business logic, SQL, HTTP |
| `internal/storage/` | SQLite + FTS5 persistence | HTTP, connector logic |
| `internal/slack/` | Slack Socket Mode + sync | Storage calls outside kb.Storage |
| `internal/discord/` | Discord gateway connector + message sync | Storage calls outside kb.Storage |
| `internal/google/` | Google Workspace API sync | Slack or Notion logic |
| `internal/notion/` | Notion API sync | Storage schema |
| `internal/attachment/` | File download and local storage | API routing |
| `internal/worker/` | Background sync scheduling | Direct API calls |
| `internal/api/` | HTTP handlers and routing | Sync orchestration |
| `internal/bot/` | Platform-agnostic bot (Gemini-powered Q&A, Slack + Discord) | Storage calls, connector logic |
| `internal/tools/` | MessagePoster/ChannelResolver interfaces for bot tools | Platform-specific logic |
| `internal/kbrepo/` | Markdown KB repo reader for bot context | HTTP, SQL, connector logic |
| `internal/gemini/` | Gemini API client (text + vision) | Storage, bot logic |
| `internal/extractor/` | Content extraction (PDF, images, Office, web) | HTTP routing, sync logic |
| `cmd/xylolabs-kb/` | Wiring, startup, shutdown | Business logic |
| `cmd/kb-gen/` | Gemini-powered KB generation tool | Runtime wiring |

### Discord platform abstraction

The bot layer is platform-agnostic. Adding Discord required a thin abstraction so `internal/bot/` can serve both Slack and Discord without duplicating Q&A logic.

| File | Role |
|------|------|
| `internal/bot/platform.go` | `Platform` interface + `IncomingMessage` type (source of truth for bot input) |
| `internal/bot/slack_platform.go` | `Platform` implementation for Slack (wraps `*slack.Client`) |
| `internal/bot/discord_platform.go` | `Platform` implementation for Discord (wraps `*discordgo.Session`) |
| `internal/bot/discord_messenger.go` | `MessagePoster` + `ChannelResolver` implementation for Discord |
| `internal/bot/formatting.go` | Platform-agnostic text formatting utilities shared by both platforms |
| `internal/discord/connector.go` | Discord gateway connector — implements `kb.Connector`, indexes messages |
| `internal/discord/converter.go` | Converts Discord messages to `kb.Document` |
| `internal/tools/messenger.go` | `MessagePoster` and `ChannelResolver` interfaces consumed by bot tools |

**Rule:** All Q&A and tool-dispatch logic lives in `internal/bot/` against the `Platform` interface. Never add Slack-specific or Discord-specific API calls there. Platform-specific code belongs in the corresponding `*_platform.go` or `discord_messenger.go` files.

### No exported types from `internal/`

Internal packages expose interfaces and functions, not concrete structs, to external callers. The `kb` package is the exception — it exports types for use by all other internal packages.

---

## Code Conventions

### Logging

```go
// CORRECT
slog.Info("connector started", "source", source, "channels", len(channels))
slog.Error("sync failed", "source", source, "error", err)

// WRONG — never use these
fmt.Println("connector started")
log.Printf("sync failed: %v", err)
```

Always use `log/slog`. Pass structured key-value pairs, never format strings into the message.

### Error wrapping

```go
// CORRECT
return fmt.Errorf("slack: fetch messages: %w", err)
return fmt.Errorf("storage: upsert document %s: %w", doc.ID, err)

// WRONG
return fmt.Errorf("failed: %v", err)
return err  // naked return loses context
```

Format: `"<package/component>: <action>: %w"`

### Context propagation

Every function that performs I/O (HTTP, SQL, file ops) must accept a `context.Context` as its first parameter and respect cancellation.

```go
// CORRECT
func (c *Connector) fetchMessages(ctx context.Context, channel string) ([]Message, error) {
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    ...
}

// WRONG — ignores context
func (c *Connector) fetchMessages(channel string) ([]Message, error) {
    resp, err := http.Get(url)
    ...
}
```

### SQL queries

- Always use prepared statements — never string-interpolate user input into SQL
- Wrap multi-step writes in transactions
- Use `modernc.org/sqlite` driver — do not import `database/sql` drivers that require CGO

```go
// CORRECT
stmt, err := db.PrepareContext(ctx, `SELECT id, title FROM documents WHERE source = ?`)
rows, err := stmt.QueryContext(ctx, source)

// WRONG
rows, err := db.QueryContext(ctx, "SELECT id, title FROM documents WHERE source = '"+source+"'")
```

### Rate limiting

All external API calls must be rate-limited. Use `golang.org/x/time/rate` or a per-connector token bucket. Never fire unbounded goroutine fans against external APIs.

### Graceful shutdown

The main goroutine closes a `done` channel on SIGINT/SIGTERM. All long-running operations must select on it:

```go
for {
    select {
    case <-done:
        return nil
    case <-ticker.C:
        if err := c.Sync(); err != nil {
            slog.Error("sync error", "source", c.Name(), "error", err)
        }
    }
}
```

---

## Testing

### Run tests

```bash
# All unit tests with race detector
go test -race ./...

# Single package
go test -race ./internal/storage/...

# Integration tests (require live credentials via env)
go test -tags integration ./...

# Coverage report
go test -race -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html
```

### Test conventions

- **Table-driven tests** for all logic with multiple input cases
- **Mock `kb.Storage`** for connector unit tests — do not hit a real database
- **Integration tests** use build tag `//go:build integration` and read from environment variables
- Test files live alongside the code they test (`foo_test.go` next to `foo.go`)
- Subtests named descriptively: `t.Run("empty query returns no results", func(t *testing.T) {...})`

---

## Building

```bash
# Development build
go build ./cmd/xylolabs-kb/

# Production (strip debug info)
go build -ldflags="-s -w" -o bin/xylolabs-kb ./cmd/xylolabs-kb/

# Cross-compile for Linux (Docker)
GOOS=linux GOARCH=amd64 go build -o bin/xylolabs-kb-linux ./cmd/xylolabs-kb/

# Verify no CGO
CGO_ENABLED=0 go build ./...
```

### Pre-commit checklist

Before committing any change:

```bash
go vet ./...
go build ./...
go test -race ./...
```

---

## Configuration

All configuration is via environment variables. The `cmd/xylolabs-kb/main.go` reads them at startup. There is no config file parsing — use `.env` loaded by the shell or Docker.

### Variable naming convention

`{SOURCE}_{SETTING}` — e.g., `SLACK_BOT_TOKEN`, `GOOGLE_SYNC_INTERVAL`, `NOTION_API_KEY`.

Global settings use no prefix: `LOG_LEVEL`, `DB_PATH`, `ATTACHMENT_PATH`, `API_HOST`, `API_PORT`.

---

## Scripts

| Script | Purpose |
|--------|---------|
| `scripts/deploy.sh` | Build ARM64 binaries, upload to AWS server, restart service |
| `scripts/generate-kb.sh` | Incremental KB generation via Gemini (falls back to Claude CLI) |
| `scripts/regenerate-kb.sh` | Full KB rebuild — resets sync state and re-processes all documents |

---

## Critical Rules

1. **NEVER commit secrets or API keys** — tokens, credentials JSON, OAuth tokens are gitignored; never hardcode them
2. **NEVER modify `types.go` without updating all connectors** — the Storage and Connector interfaces are the contract that all packages depend on
3. **ALWAYS run `go vet` and `go build` before committing** — CI must pass
4. **ALWAYS handle context cancellation** in every long-running operation
5. **ALWAYS use prepared statements** for SQL queries — no string interpolation
6. **ALWAYS rate-limit** external API calls — Slack, Google, and Notion all have strict rate limits
7. **NEVER use `fmt.Println` or bare `log.Printf`** — use `log/slog` exclusively
8. **NEVER use CGO-dependent sqlite drivers** — keep the build pure Go with `modernc.org/sqlite`
9. **ALWAYS wrap errors** with component context using `fmt.Errorf("component: action: %w", err)`
10. **NEVER expose internal package types** beyond the `kb` domain package — use interfaces

---

## Common Patterns

### Implementing a new connector

```go
package myconnector

import (
    "context"
    "fmt"
    "log/slog"

    "github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

type Connector struct {
    storage kb.Storage
    // ... config fields
}

func New(storage kb.Storage /* ...config */) *Connector {
    return &Connector{storage: storage}
}

func (c *Connector) Name() kb.Source { return kb.Source("mysource") }

func (c *Connector) Start(done <-chan struct{}) error {
    // real-time listener loop; select on done to exit
    return nil
}

func (c *Connector) Sync() error {
    ctx := context.Background()
    // fetch, upsert documents
    if err := c.storage.UpsertDocument(doc); err != nil {
        return fmt.Errorf("myconnector: sync: upsert: %w", err)
    }
    return nil
}

func (c *Connector) Stop() error {
    slog.Info("myconnector: stopped")
    return nil
}
```

### Upsert pattern for incremental sync

```go
existing, err := storage.GetDocumentBySourceID(kb.SourceSlack, msg.ID)
if err == nil && !existing.UpdatedAt.Before(msg.UpdatedAt) {
    // already up to date — skip
    continue
}
if err := storage.UpsertDocument(doc); err != nil {
    return fmt.Errorf("slack: upsert message %s: %w", msg.ID, err)
}
```

### Structured logging pattern

```go
slog.Info("sync complete",
    "source", c.Name(),
    "documents_indexed", count,
    "duration_ms", time.Since(start).Milliseconds(),
)
```
