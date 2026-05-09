# Architecture — xylolabs-kb

## Component Diagram

```
┌─────────────────────────────────────────────────────────────────────┐
│                          xylolabs-kb process                        │
│                                                                     │
│  External Sources          Connectors           Internal            │
│  ─────────────────         ──────────           ────────            │
│                                                                     │
│  ┌───────────┐            ┌──────────────┐                          │
│  │  Slack    │◄──WS/API──►│   Slack      │                          │
│  │  API      │            │   Connector  │──┐                       │
│  └───────────┘            └──────────────┘  │                       │
│                                             │                       │
│  ┌───────────┐            ┌──────────────┐  │   ┌───────────────┐  │
│  │  Google   │◄──REST────►│   Google     │──┼──►│  KB Engine    │  │
│  │  APIs     │            │   Connector  │  │   │ (orchestrator)│  │
│  └───────────┘            └──────────────┘  │   └───────┬───────┘  │
│                                             │           │           │
│  ┌───────────┐            ┌──────────────┐  │           │           │
│  │  Notion   │◄──REST────►│   Notion     │──┘           │           │
│  │  API      │            │   Connector  │              │           │
│  └───────────┘            └──────────────┘              │           │
│                                                         │           │
│                         ┌───────────────────────────────┼─────┐    │
│                         │                               │     │    │
│                  ┌──────▼──────┐   ┌───────────┐  ┌────▼───┐  │    │
│                  │   Storage   │   │  Worker   │  │  API   │  │    │
│                  │  (SQLite +  │   │ Scheduler │  │ Server │  │    │
│                  │    FTS5)    │   └───────────┘  └────────┘  │    │
│                  └──────┬──────┘                              │    │
│                         │                                     │    │
│                  ┌──────▼──────┐                              │    │
│                  │ Attachment  │                              │    │
│                  │  Manager   │                              │    │
│                  └─────────────┘                              │    │
│                         └──────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────┘
```

## Component Descriptions

### `internal/kb` — Domain layer

The core of the system. Contains:

- **`types.go`** — all domain types (`Document`, `Attachment`, `SearchResult`, `SearchQuery`, `SyncState`, `Stats`) and the two key interfaces (`Storage`, `Connector`)

This package has no dependencies on any other internal package. Every other package imports `kb` for its types. This is the single source of truth for the data model.

### `internal/storage` — Persistence layer

Implements `kb.Storage` using SQLite + FTS5.

Responsibilities:
- Schema creation and migration on startup
- `UpsertDocument` — inserts or replaces a document and updates the FTS5 virtual table atomically in a transaction
- `Search` — queries the `documents_fts` FTS5 table, joins with `documents` for full fields, returns BM25-ranked results with snippets
- `GetSyncState` / `SetSyncState` — cursor persistence for incremental sync
- `UpsertAttachment` / `GetAttachments` — attachment record management
- `GetStats` — aggregate counts via SQL aggregation

Uses `modernc.org/sqlite` (pure Go, no CGO). The database file path is configured via `DB_PATH`.

### `internal/slack` — Slack connector

Implements `kb.Connector` for Slack.

Two operational modes run concurrently:

1. **Socket Mode listener** (`Start`) — opens a WebSocket connection to Slack's Socket Mode API. Receives real-time events (`message`, `app_mention`, `channel_created`) and ingests them immediately as `Document` values via `storage.UpsertDocument`.

2. **Periodic full sync** (`Sync`) — called by the Worker scheduler at `SLACK_SYNC_INTERVAL`. Uses `conversations.history` and `conversations.replies` to fetch messages the bot may have missed (e.g., posted while the process was down). Respects the `SyncState` cursor (a Slack timestamp string) to fetch only new messages.

**Auto-join channels:** During each sync, the connector automatically joins all public channels the bot is not a member of. It also listens for `channel_created` events and auto-joins newly created public channels in real-time.

Message threading: reply documents set `ParentID` to the root message's internal document ID.

### `internal/google` — Google Workspace connector

Implements `kb.Connector` for Google Workspace. Polling-only (no push channel in this implementation).

`Sync()` performs:
1. Lists files in configured Drive folders (or all accessible files) using the Drive Changes API with a page token stored as the `SyncState` cursor
2. For Google Docs: fetches content via the Docs API, converts to plain text
3. For Google Sheets: fetches sheet values via the Sheets API
4. For Google Slides: fetches slide content via the Presentations API
5. For other file types (PDFs, images, Office docs): downloads and extracts content via the Extractor
6. Syncs Google Calendar events (past 90 days to next 90 days) across all accessible calendars, including attendees, location, and description

Auth auto-detects credential type from the JSON file: service account keys use JWT, OAuth2 credentials use token-based flow with cached tokens.

### `internal/notion` — Notion connector

Implements `kb.Connector` for Notion. Polling-only.

`Sync()` performs:
1. Starting from each root page in `NOTION_ROOT_PAGES`, recursively walks the page tree via the Notion Search API (filtered by `last_edited_time > lastSyncAt`)
2. Fetches block content for each page and concatenates block text into `Document.Content`
3. Database pages are treated as individual documents with their properties serialized into `Metadata`
4. Sets `ParentID` for sub-pages to preserve hierarchy

### `internal/attachment` — Attachment manager

Downloads files from authenticated source URLs and stores them locally under `ATTACHMENT_PATH/{source}/{documentID}/{filename}`.

Handles:
- Authorization headers (Slack requires `Authorization: Bearer <token>`, Google requires OAuth token)
- Deduplication: checks if `Attachment.LocalPath` already exists before re-downloading
- MIME type validation
- File size accounting for stats

### `internal/worker` — Background scheduler

Runs a ticker per enabled connector. At each interval:
1. Calls `connector.Sync()`
2. Logs success or error (with backoff tracking for repeated failures)
3. Updates `SyncState` on success

Worker goroutines respect the `done` channel for graceful shutdown.

### `internal/api` — HTTP REST API

Thin HTTP layer using the Go standard library (`net/http`). Reads directly from `kb.Storage`.

Routes:
- `GET /health` — liveness probe
- `GET /api/v1/search` — full-text search with filter parameters
- `GET /api/v1/documents` — list documents with source/since/limit/offset filters (used by kb-gen)
- `GET /api/v1/documents/{id}` — single document fetch
- `GET /api/v1/stats` — `GetStats` result
- `GET /api/v1/sources` — scheduler status + sync state for all sources
- `POST /api/v1/sync/{source}` — triggers immediate `Sync()` call on the named connector

### `internal/bot` — Slack bot handler

Implements Slack bot response logic (requires `GEMINI_API_KEY` and `KB_REPO_DIR`).

Responsibilities:
- Listen for `app_mention` and `message.im` events
- Extract user questions and attached files
- Load knowledge context from the markdown KB repo via `kbrepo.Reader`
- Build Gemini prompt with KB context + user question + attachment content
- Call Gemini API with function declarations to generate grounded responses
- Execute tool calls (Google Drive write, Notion write) in a multi-turn loop (max 5 iterations)
- Post replies to Slack threads or DMs
- Default response language is Korean (switches to English if user writes in English)
- Handle rate limiting and errors gracefully

Additional behaviors:
- **Function calling (tool use)**: supports 8 tools — `create_google_doc`, `create_drive_folder`, `upload_to_drive`, `delete_drive_file`, `rename_drive_file`, `edit_google_doc`, `create_notion_page`, `update_notion_page`. Uses Gemini's native function calling API with explicit `toolConfig` (mode: AUTO).
- **Thread tracking**: the bot maintains a dual cache (positive cache for tracked threads, 5-minute negative cache for non-bot threads) to enable thread continuation without @mentions
- **Multi-turn context**: up to 20 prior thread messages are fetched and included as conversation history for continuity
- **LEARN blocks**: when users share factual information, the bot extracts `===LEARN:===...===ENDLEARN===` blocks from Gemini responses and saves them to the KB repo via `kbReader.SaveFact()`
- **Slack mrkdwn conversion**: `**bold** → *bold*`, `[text](url) → <url|text>`, `## Header → *Header*`. Uses Block Kit with explicit mrkdwn type for reliable rendering
- **Reply truncation** at 3,000 characters
- **Identity**: identifies as "Xylolabs Knowledge Engine"; never discloses backend models or internal tool names

The bot only activates when `GEMINI_API_KEY` is set. If not configured, Slack ingestion still works normally.

### `internal/tools` — Tool executor for write operations

Manages Google Drive and Notion write operations, invoked by the bot via Gemini function calling.

Components:
- **`executor.go`** — tool registry and dispatcher. Holds function declarations for all 8 tools, dispatches calls by name, manages file attachments from Slack messages
- **`google_writer.go`** — Google Drive write operations using the Drive API v3. `CreateDoc()`, `CreateFolder()`, `UploadFile()`, `DeleteFile()`, `RenameFile()`, `UpdateDocContent()`. Default folder: shared drive root
- **`notion_writer.go`** — Notion write operations via REST API. `CreatePage()` creates pages with title + content blocks; `AppendToPage()` adds content to existing pages. Supports heading, list, and paragraph block types with Notion's 2000-char rich_text limit

The executor is wired to the bot handler in `main.go` after all connectors are initialized. It requires the Google Drive service (from the Google connector) and/or the Notion API key.

### `internal/kbrepo` — Markdown KB repo reader

Provides hierarchical access to the markdown-based knowledge repository using a two-stage approach:

1. **Index layer** (always loaded): `indexes/*.md` files (topics, keywords, people, weekly summaries) plus source/channel README files
2. **Detail layer** (loaded on demand): Only the specific markdown files referenced by index sections that match the user's query

How `BuildContext(query)` works:
1. Runs `git pull --rebase` (rate-limited to once per 30 seconds)
2. Loads all index files
3. Splits each index into sections (by `##` headers)
4. Scores sections by counting keyword matches against the query
5. Extracts markdown links (`](path.md)`) from high-scoring sections
6. Also matches keywords against all detail file paths
7. Loads the top 10 highest-scoring detail files
8. Returns formatted context string combining indexes + relevant details

This keeps context small and focused even as the knowledge repo grows — the bot never loads the entire repo.

### `internal/gemini` — Gemini API client

Wraps the Google Gemini API for two use cases:

1. **Text generation** — for bot responses
   - Takes a prompt with KB context + user question
   - Calls Gemini with configurable thinking budget (none/low/medium/high)
   - Buffers complete response including thinking output

2. **Vision** — for image and PDF description
   - Accepts image bytes or PDF file path
   - Calls Gemini vision API
   - Returns text description of content

Handles:
- API key management
- Rate limiting (respects 429 responses)
- Timeout handling (120-second default, configurable via SetTimeout)
- Error wrapping with context
- Google Search / Function Calling mutual exclusion (Gemini API does not allow combining them in the same request; function calling takes priority)

### `internal/extractor` — Content extraction

Extracts text from various file formats during ingestion.

Supported formats:

| Format | Extractor | Output |
|--------|-----------|--------|
| PDF | Pure-Go PDF parser (text layer) | Extracted text |
| Images (PNG, JPG, GIF, WEBP) | Gemini vision | Image description |
| DOCX | ZIP + XML parser | Extracted text from all paragraphs |
| XLSX | ZIP + XML parser | All cell values (with row/col context) |
| PPTX | ZIP + XML parser | All slide text |
| Text, CSV, JSON | Direct read | Raw content |
| Web URLs | Readability algorithm | Article text + title |

Extracted content is:
- Appended to the document's main `Content` field
- Indexed into FTS5 for full-text search
- Stored with source attribution (e.g., "PDF: filename.pdf")

### `cmd/xylolabs-kb` — Entry point

`main.go` responsibilities:
1. Read all environment variables
2. Open `kb.Storage` (SQLite)
3. Instantiate enabled connectors, passing them the storage handle
4. (If `GEMINI_API_KEY` set) Initialize Gemini client and bot handler
5. Start the HTTP API server
6. Start the Worker scheduler
7. Call `connector.Start(done)` on each enabled connector
8. Block on SIGINT/SIGTERM
9. On signal: close `done`, call `connector.Stop()` on each, close storage

### `cmd/kb-gen` — KB generation tool

Standalone CLI tool that uses Gemini to transform raw documents into structured markdown for the knowledge base repository.

Responsibilities:
1. Read raw documents JSON (exported from the Go worker API)
2. Group documents into batches (Slack: by channel+date)
3. Build prompts with curation instructions from CLAUDE.md
4. Call Gemini API with configurable model and thinking level
5. Parse `===FILE: path===...===ENDFILE===` blocks from responses
6. Write markdown files to the KB repo directory
7. Update `_meta/sync-state.json` and `_meta/document-map.json`

Configurable via CLI flags or environment variables (`KB_GEN_MODEL`, `KB_GEN_THINKING`, `KB_GEN_API_KEY`).

### `scripts/` — Deployment and KB generation scripts

| Script | Purpose |
|--------|---------|
| `deploy.sh` | Build ARM64 binaries, upload to AWS server (bots.xylolabs.com), restart systemd service, verify health |
| `generate-kb.sh` | Incremental KB generation: fetch new documents from API, process with Gemini (kb-gen), fall back to Claude CLI on quota errors, commit + push to KB repo |
| `regenerate-kb.sh` | Full rebuild: reset sync state to epoch, run generate-kb.sh iteratively (up to 10 passes) |

The `generate-kb.sh` script is scheduled as a cron job (every 6 hours) and `regenerate-kb.sh` runs weekly (Sunday 3 AM).

---

## Data Flow

### Real-time Slack message ingestion

```
Slack platform
    │
    │  WebSocket event (Socket Mode)
    ▼
internal/slack Connector.Start() goroutine
    │
    │  parse event → build kb.Document
    ▼
internal/kb Storage.UpsertDocument(doc)
    │
    ├── INSERT OR REPLACE INTO documents ...
    └── INSERT OR REPLACE INTO documents_fts ...
    │
    ▼
(optional) Attachment URLs in message
    │
internal/attachment Manager.Download(url, token)
    │
    └── write to ATTACHMENT_PATH/slack/{docID}/{filename}
        Storage.UpsertAttachment(att)
```

### Periodic Google/Notion sync

```
internal/worker Scheduler (ticker fires)
    │
    ▼
connector.Sync()
    │
    ├── Storage.GetSyncState(source) → cursor
    │
    ├── External API call (paginated, rate-limited)
    │       ├── page 1 → []Document → Storage.UpsertDocument × N
    │       ├── page 2 → ...
    │       └── page N → ...
    │
    └── Storage.SetSyncState(newCursor)
```

### Search query

```
HTTP GET /api/v1/search?q=...&source=slack&limit=20
    │
    ▼
internal/api handler
    │
    ├── parse query params → kb.SearchQuery
    │
    ▼
Storage.Search(query)
    │
    ├── SELECT ... FROM documents_fts WHERE documents_fts MATCH ?
    │     ORDER BY bm25(documents_fts) LIMIT ? OFFSET ?
    │
    └── JOIN documents ON documents.id = documents_fts.rowid
    │
    ▼
[]kb.SearchResult (with Score, Snippet)
    │
    ▼
JSON response
```

### Slack bot mention or DM

```
Slack platform
    │
    │  WebSocket event (Socket Mode)
    │  event type: app_mention or message.im or thread reply
    ▼
internal/bot Handler
    │
    ├── Parse message text → question
    ├── Extract attachments (images, PDFs)
    ├── Check thread tracking (dual cache + API fallback)
    │
    ▼
internal/kbrepo Reader
    │
    ├── git pull --rebase (rate-limited)
    ├── Load index files (indexes/*.md, READMEs)
    ├── Score index sections by query keywords
    ├── Extract file references from top sections
    ├── Load top 10 relevant detail files
    │
    ▼
Build Gemini prompt
    │
    ├── System prompt (Korean default, Slack mrkdwn format)
    ├── KB indexes + relevant details as context
    ├── Thread history (up to 20 prior messages)
    ├── Append attachment images
    ├── Append user question
    │
    ▼
internal/gemini Client
    │
    └── Call Gemini API (with function declarations + toolConfig)
        ├── POST https://generativelanguage.googleapis.com/...
        ├── Thinking level: low
        │
        ▼
    Gemini response
    │
    ├── Text response (no function calls) → continue to post
    │
    └── Function call(s) returned:
            │
            ▼
        internal/tools ToolExecutor.Execute()
            │
            ├── create_google_doc → GoogleWriter.CreateDoc()
            ├── upload_to_drive → GoogleWriter.UploadFile()
            ├── create_notion_page → NotionWriter.CreatePage()
            ├── update_notion_page → NotionWriter.AppendToPage()
            │
            ▼
        Append tool call + results to conversation
            │
            └── Re-call Gemini with results (max 5 iterations)
                    │
                    └── Final text response
    │
    ├── Extract LEARN blocks → kbReader.SaveFact() (async)
    ├── Strip LEARN blocks from visible reply
    ├── Convert Markdown → Slack mrkdwn
    ├── Truncate to 3000 chars
    │
    ▼
Post reply directly
    │
    ├── chat.postMessage API call
    ├── Track thread in positive cache
    └── (if @mention) post in thread / (if DM) post in DM channel
```

### File attachment extraction during ingestion

```
Connector receives document with attachments
    │
    ├── Document.Attachments[0].SourceURL
    ├── Document.Attachments[0].MimeType
    │
    ▼
internal/attachment Manager.Download(url, token)
    │
    └── write to ATTACHMENT_PATH/slack/{docID}/{filename}
        Storage.UpsertAttachment(att)
    │
    ▼
(optional) Content extraction
    │
    ├── Determine file type from MIME type or extension
    │
    ▼
internal/extractor.Extract(filepath, mimeType)
    │
    ├── (if PDF) → PDF text extraction → extracted_text
    ├── (if image) → Gemini vision → description → extracted_text
    ├── (if DOCX/XLSX/PPTX) → ZIP + XML → extracted_text
    ├── (if text/CSV/JSON) → direct read → extracted_text
    │
    ▼
Append to document
    │
    ├── Document.Content += "\n\n[Extracted from " + filename + "]\n" + extracted_text
    │
    ▼
Storage.UpsertDocument(doc)
    │
    ├── INSERT OR REPLACE INTO documents ...
    └── INSERT OR REPLACE INTO documents_fts ...
        (FTS5 now indexes both original + extracted content)
```

---

## Storage Schema

```sql
-- Primary document store
CREATE TABLE IF NOT EXISTS documents (
    id           TEXT PRIMARY KEY,
    source       TEXT NOT NULL,
    source_id    TEXT NOT NULL,
    parent_id    TEXT,
    title        TEXT,
    content      TEXT,
    content_type TEXT,
    author       TEXT,
    author_email TEXT,
    channel      TEXT,
    workspace    TEXT,
    url          TEXT,
    timestamp    DATETIME,
    updated_at   DATETIME,
    indexed_at   DATETIME NOT NULL DEFAULT (datetime('now')),
    metadata     TEXT  -- JSON
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_documents_source_id
    ON documents(source, source_id);

-- FTS5 virtual table for full-text search
CREATE VIRTUAL TABLE IF NOT EXISTS documents_fts USING fts5(
    title,
    content,
    content='documents',
    content_rowid='rowid',
    tokenize='porter unicode61'
);

-- Triggers to keep FTS5 in sync
CREATE TRIGGER IF NOT EXISTS documents_ai AFTER INSERT ON documents BEGIN
    INSERT INTO documents_fts(rowid, title, content)
        VALUES (new.rowid, new.title, new.content);
END;

CREATE TRIGGER IF NOT EXISTS documents_au AFTER UPDATE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, content)
        VALUES ('delete', old.rowid, old.title, old.content);
    INSERT INTO documents_fts(rowid, title, content)
        VALUES (new.rowid, new.title, new.content);
END;

CREATE TRIGGER IF NOT EXISTS documents_ad AFTER DELETE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, content)
        VALUES ('delete', old.rowid, old.title, old.content);
END;

-- Attachments
CREATE TABLE IF NOT EXISTS attachments (
    id            TEXT PRIMARY KEY,
    document_id   TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    filename      TEXT,
    mime_type     TEXT,
    size          INTEGER,
    source_url    TEXT,
    local_path    TEXT,
    downloaded_at DATETIME
);

-- Sync cursors per source
CREATE TABLE IF NOT EXISTS sync_state (
    source       TEXT PRIMARY KEY,
    last_sync_at DATETIME,
    cursor       TEXT,
    metadata     TEXT  -- JSON
);
```

---

## Sync Strategy

### Incremental sync

All connectors use cursor-based incremental sync to avoid re-processing already-indexed content:

| Source | Cursor type | How used |
|--------|------------|----------|
| Slack | Unix timestamp string (e.g., `"1733058720.000000"`) | Passed as `oldest` parameter to `conversations.history` |
| Google | Drive Changes page token | Passed to `changes.list` to get only new changes |
| Notion | ISO 8601 timestamp | Passed as `filter.last_edited_time.after` to Search API |

The cursor is stored in `sync_state.cursor` and updated atomically after a successful sync pass.

### Full re-index

A full re-index can be triggered by clearing the `sync_state` row for a source (or setting `cursor` to empty string). The next sync pass will fetch all content from the beginning. This is useful after schema changes or data corruption.

### Upsert semantics

`UpsertDocument` uses `INSERT OR REPLACE` semantics. A document is identified by `(source, source_id)`. If the same source document is encountered again (e.g., an edited Slack message), the existing record is replaced and the FTS5 index is updated via triggers.

---

## Error Handling Patterns

### Connector errors

Sync errors are logged at `ERROR` level but do not crash the service. The worker scheduler continues to fire on the next interval. This ensures a transient API outage does not require a process restart.

```go
if err := connector.Sync(); err != nil {
    slog.Error("sync failed", "source", connector.Name(), "error", err)
    // continue — next tick will retry
}
```

### Storage errors

Storage errors during upsert are returned to the connector as wrapped errors. The connector logs them and continues processing the remaining items in the batch (best-effort indexing).

### Attachment download errors

Failed downloads are logged and the `Attachment` record is still written to the database with an empty `LocalPath`. This allows retrying downloads without re-indexing the parent document.

### Shutdown errors

`Stop()` errors from connectors are logged but do not block shutdown. The process exits after all connectors' `Stop()` methods return (or a shutdown timeout elapses).

---

## Rate Limiting Strategy

Each connector maintains its own rate limiter:

| Source | Default rate | Mechanism |
|--------|-------------|-----------|
| Slack | 1 req/sec (Tier 3 methods) | `golang.org/x/time/rate` token bucket |
| Google Drive/Docs/Sheets | 100 req/100s per user | Token bucket, respects `429` retry-after |
| Notion | 3 req/sec | Token bucket |

On `429 Too Many Requests`, the connector reads the `Retry-After` header and pauses accordingly before retrying.

Attachment downloads are rate-limited separately from API calls at 5 concurrent downloads per source.
