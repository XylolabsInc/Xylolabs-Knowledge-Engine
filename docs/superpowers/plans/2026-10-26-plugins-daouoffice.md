# Plugin System and DaouOffice Plugin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a generic plugin layer to the bot and ship `plugins/daouoffice`, so each person connects their own DaouOffice account and can look things up, turn Slack photos into 검수조서, and prepare and submit 구매 품의서, 지출결의, 출장, and 증명서 with a human approval step.

**Architecture:** Go `internal/plugin` (registry, vault, MCP stdio adapter) behind `ToolExecutor`; bot interaction layer (buttons, modals, approval and connect flows); a Node 24 MCP server in `plugins/daouoffice` that ports the proven Playwright flows from the private `xylolabs-daouoffice` repo, multi-user, with its own `node:sqlite` DB.

**Spec:** `docs/superpowers/specs/2026-10-26-plugins-daouoffice-design.md`

**Source being ported:** `/Users/hletrd/flash-shared/xylolabs-daouoffice` (private). Read its `CLAUDE.md`, `docs/`, `.claude/skills/`, and `src/` for the flows. Copy DOM logic faithfully; change only the seams.

## Global constraints

- **This repo is PUBLIC.** Never commit: staff names other than placeholders, card numbers, DaouOffice IDs (department, document, form), purchase records, merchant/amount history, API response bodies, credentials. Tests use made-up data (`홍길동`, `1234`, `테스트상점`).
- Go: AGENTS.md rules (slog only, `fmt.Errorf("pkg: action: %w")`, context first, prepared statements, no CGO, table-driven tests). New deps: `github.com/modelcontextprotocol/go-sdk v1.7.0` only.
- Node: Node 24 LTS, ESM, `node:test`. Deps: `@modelcontextprotocol/server@2.4.0`, `zod@^4`, `playwright@1.64.1`, `sharp` (latest), `heic-convert` (latest). No other runtime deps without a reason.
- Never log passwords, request `_meta.xylolabs/credentials`, or DaouOffice response bodies.
- Gated tools must not submit anything in preview mode. Only `Mode=execute`, sent by the Go registry after a human approval, may submit.
- Validation gate per Go task: `go vet ./... && go build ./... && go test -race ./...`. Per Node task: `cd plugins/daouoffice && npm test`.
- Do not commit. The orchestrator reviews and commits each unit (signed, Conventional Commits + gitmoji, no Co-Authored-By).

## Shared contracts

### `_meta` keys sent by the Go registry on every `tools/call`

| Key | Value |
|---|---|
| `xylolabs/user` | `{ "key": "slack:U123", "name": "<display name>", "platform": "slack" }` |
| `xylolabs/credentials` | `{ "username": "...", "password": "..." }` (absent for unconnected users) |
| `xylolabs/allowKick` | `true` to accept the duplicate-login modal |
| `xylolabs/mode` | `"preview"` or `"execute"` (absent → `"execute"` for read tools, `"preview"` for write tools) |
| `xylolabs/attachments` | `[{ "name": "IMG_1.jpg", "mimeType": "image/jpeg", "path": "/abs/path" }]` |

### Tool metadata sent by the plugin

- `annotations.readOnlyHint=true` → read tool. `annotations.destructiveHint=true` and `readOnlyHint=false` → gated write tool. Local DB writes (`save_photos`, `draft_purchase`, …) use `readOnlyHint=false, destructiveHint=false` → not gated.
- `_meta["xylolabs/role"]`: `"connect"` (hidden; input schema = account form fields with `title`; `password` has `"format": "password"`; optional booleans render as checkboxes) or `"login"` (hidden; signs in with `allowKick`, returns profile).

### Errors from the plugin

Tool result `isError: true`, `structuredContent: { code, message }`, `content[0].text = message`. Codes: `NOT_CONNECTED`, `AUTH_FAILED`, `DUPLICATE_SESSION`, `RATE_LIMITED`, `VALIDATION` (fix the input), `NOT_FOUND`, `CONFLICT` (already filed, already withdrawn), `UPSTREAM` (DaouOffice broke), `INTERNAL`.

### Results

- `content`: `text` (Korean, Slack-friendly markdown) first; `image` blocks (base64 PNG/JPEG) for previews and screenshots; `resource` blocks with `blob` for PDFs (`_meta["xylolabs/private"]=true` → DM only).
- `structuredContent`: machine data (document id, status, mismatches).
- Gated tools in preview return the preview text and, when useful, an image. In execute they return the document id, the server-side status, and the verification outcome.

## Phase A (parallel)

### Task A1 (Go core): storage, plugin package, MCP adapter, executor hook

**Files:**
- Modify: `internal/storage/migrations.go` (version 6), `internal/storage/sqlite.go` (or new `internal/storage/plugins.go`)
- Create: `internal/plugin/{plugin.go,errors.go,identity.go,vault.go,schema.go,registry.go,prompts.go}` + tests
- Create: `internal/plugin/mcpstdio/{client.go,client_test.go,testdata/server.mjs}`
- Modify: `internal/tools/executor.go` (registry field, `Declarations` append, `dispatch` default → registry)
- Modify: `go.mod`, `go.sum`

- [ ] Migration 6 with the three tables from the spec; `SQLiteStore` methods: `UpsertPluginAccount`, `GetPluginAccount`, `DeletePluginAccount`, `SetPluginAccountStatus`, `ListPluginAccounts(userKey)`, `RecordPluginPrompt`, `HasPluginPrompt`, `CreatePluginAction`, `GetPluginAction`, `TransitionPluginAction(id, from, to, result)` (atomic `UPDATE … WHERE status = from`), `SetPluginActionPrompt`, `PendingPluginActionInThread(userKey, channel, thread)`, `ExpirePluginActions(now)`. Tests with a temp DB.
- [ ] `plugin.Store` interface listing exactly the methods the registry needs; `*storage.SQLiteStore` satisfies it.
- [ ] Types and errors from the spec. `Identity` context helpers `ContextWithIdentity`/`IdentityFromContext`. Prompt outbox `ContextWithPrompts`/`PromptsFromContext` holding `[]Prompt` where `Prompt{Kind: connect|approval|login_kick|notice, UserKey, Plugin, ActionID, Text, Files}`.
- [ ] Vault (AES-256-GCM) with `NewVault(base64Key)`, `Seal(map[string]string) ([]byte, error)`, `Open([]byte) (map[string]string, error)`; tests for round trip, wrong key, tampering.
- [ ] Schema sanitizer per spec; tests with the MCP Node SDK's real output shape (`$schema`, int bounds ±9007199254740991, `additionalProperties`).
- [ ] Registry per spec: `NewRegistry(store, vault, logger)`, `Register(p Plugin)`, `Declarations()`, `Handles(name) bool`, `Dispatch(ctx, name, args, attachments map[string][]byte) (map[string]any, error)`, `Connect(ctx, id, plugin, values map[string]string) (accountName string, err error)`, `Disconnect`, `Approve(ctx, actionID string, by Identity) (*Result, *Action, error)`, `Cancel`, `LoginAndResume(ctx, actionID, by)`, `AccountPlugins() []Plugin`, `NeedsConnectPrompt(userKey) []Plugin`, `MarkPrompted`, `Close()`. Attachments are written to `<dataDir>/inbox/<actionID or request id>/` with sanitized names and passed as paths; stored actions keep them until done. Tests with a fake `Plugin`: prefixing, hidden tools excluded, not-connected path, read path, write path creates pending action + prompt and never calls execute, approve by owner only, approve twice fails, expired fails, duplicate-session creates login_kick, auth-failed marks invalid.
- [ ] `mcpstdio.New(ctx, Config{Name, Title, Command []string, Dir, Env, DataDir}, logger) (plugin.Plugin, error)`: spawns the command, initializes, lists tools (and re-lists after a reconnect), maps annotations/roles, sanitizes schemas, `Call` sets `_meta` keys, converts content (text, image, resource blob) to `plugin.Result`, maps error codes. Restarts the process once on `ErrConnectionClosed`. `Instructions()` comes from the server's `initialize` result `instructions`. Test with `testdata/server.mjs` (uses the plugin's `node_modules` if present, else skip).
- [ ] `ToolExecutor.SetPlugins(r *plugin.Registry)`; `Declarations()` appends `r.Declarations()`; unknown names go to `r.Dispatch` with `attachmentsFromContext(ctx)`.

### Task A2 (Node foundation): server, runtime, sessions, DB, photos, core modules, read tools

**Files (all under `plugins/daouoffice/`):** `package.json`, `README.md`, `config.example.json`, `.gitignore` (node_modules), `src/server.mjs`, `src/runtime.mjs`, `src/errors.mjs`, `src/config.mjs`, `src/sessions.mjs`, `src/db.mjs`, `src/photos.mjs`, `src/dao/{login.mjs,api.mjs,dialogs.mjs,org.mjs,forms.mjs,approval.mjs,approvers.mjs,writing.mjs,kst.mjs,cards.mjs,card-match.mjs,documents.mjs}`, `src/tools/{index.mjs,account.mjs,read.mjs,guide.mjs,purchase.mjs,inspection.mjs,claim.mjs,trip.mjs,cert.mjs}` (the last five as `export function register() {}` stubs for Phase B), `guide/{index.md,session.md,approval.md}`, `test/*.test.mjs`.

- [ ] `runtime.mjs`: `createRuntime({config, db, sessions, dataDir, logger})` → `rt` with `rt.tool(server, name, {description, input (zod object), kind: 'read'|'local'|'write', role?}, handler)`. The wrapper reads `_meta` from `ctx.mcpReq._meta`, builds `call = {user, creds, allowKick, mode, attachments, owner}` (owner = account login id, resolved from creds or `NOT_CONNECTED`), enforces `mode` (`write` handler gets `call.mode`; a `write` tool called without `mode=execute` must behave as preview), catches `PluginError` and unknown errors and returns the error shape. Handlers return `{text, images?: [{data: Buffer, mimeType}], files?: [{name, data, mimeType, private}], data?}`.
- [ ] `sessions.mjs`: `SessionManager({baseUrl, maxContexts: 2, idleMs: 600000, launch})` with `withSession(owner, creds, {allowKick}, fn({page, api, context}))`, `close()`. Per-owner lock, FIFO wait when at capacity, lazy session check, login rate limit, idle close, browser close when empty. `launch` is injectable for tests (fake browser).
- [ ] `dao/login.mjs` from `session.mjs` `loginOnPage`: credentials as args; detect wrong password (still on `/login` with an error message, or the password field still present after submit) → `AUTH_FAILED`; duplicate modal handling per spec; replace fixed sleeps with condition waits where the source allows.
- [ ] `dao/api.mjs` (`makeApi(page)` from `session.mjs`), `dao/dialogs.mjs` (`dismissDialogs`), `dao/org.mjs` (session user, department id/code/name from the org tree), `dao/forms.mjs`, `dao/approval.mjs` (all exports from the source, minus `recon/` screenshots), `dao/writing.mjs`, `dao/kst.mjs`, `dao/cards.mjs`.
- [ ] `dao/approvers.mjs`: config-driven `projectApprovers(cfg, project, amount)`, `claimLine(cfg)`, `inspectionLine(cfg, drafterName)`, `tripLine(cfg)`, `describeLine`.
- [ ] `dao/card-match.mjs`: port `matchCard` and helpers; receipts, overrides, and pinned rules come from the DB (`db.receipts(owner)`, `db.overrides(owner, kind)`), purchases from `db.purchases(owner)`; keep the algorithm and tolerances; fix the `byAmount` collision by keeping all candidates per amount.
- [ ] `db.mjs`: `openDb(path)` creates all tables from the spec (including the ones Phase B fills) and exposes repository functions (`upsertAccount`, `account`, `purchases`, `purchase`, `upsertPurchase`, `receipts`, `addReceipt`, `photos`, `addPhoto`, `updatePhoto`, `overrides`, `addOverride`, `trips`, `upsertTrip`, `filings`, `addFiling`, `fxRate`, `addFxRate`). JSON columns are parsed on read.
- [ ] `photos.mjs`: `ingestPhoto(input path, dest dir)` → strips metadata with `sharp().rotate()`, converts HEIC with `heic-convert`, caps the long side at 3000 px and size at 4 MB, records the original capture date (read from EXIF before stripping), asserts no APP1/APP13 segments remain; returns `{file, sha256, takenDate, width, height}`.
- [ ] `tools/account.mjs`: `connect` (role connect; fields `username` "아이디", `password` "비밀번호" format password, `allowKick` boolean "기존 접속을 끊어도 됩니다"; signs in, reads profile, stores `accounts`, closes the context if it hit the duplicate modal without kick) and `login` (role login). `me` read tool.
- [ ] `tools/read.mjs`: `attendance_today`, `leave_balance`, `approval_inbox`, `my_drafts`, `read_document`, `list_forms`, `list_cards`, `card_usage`, `withdraw_document` (gated; uses `withdrawDocument`).
- [ ] `tools/guide.mjs` + `guide/*.md`: `guide(topic)` returns the markdown; `server.mjs` sets `instructions` to a short summary of the rules (ask instead of inventing purposes, never repeat the item name in 구입목적, Korean style rules, previews need approval, certificates are private).
- [ ] Tests: runtime gating and error mapping (in-memory transport), sessions with a fake browser (lock, capacity queue, idle close, rate limit), approvers, writing, card-match with synthetic rows, photos (a generated JPEG with EXIF GPS → stripped), db round trips.

## Phase B (parallel, after Phase A)

### Task B1 (Go bot layer)

**Files:** `internal/bot/{platform.go,plugins.go,plugins_test.go,handler.go,slack_platform.go,discord_platform.go}`, `internal/slack/connector.go`, `internal/discord/connector.go`, `internal/config/config.go`, `cmd/xylolabs-kb/main.go`, `README.md`, `AGENTS.md`, `docs/slack-bot.md`, `.env.example` if present.

- [ ] Platform additions and types from the spec; Slack implementation (Block Kit section + actions; `chat.update`; `views.open` with `plain_text_input` and a checkboxes element; `conversations.open`). Discord implementation (message components; edit message; modal interaction response using the stored interaction for `trigger`; `UserChannelCreate`).
- [ ] `BotHandler.HandleAction`, `HandleFormSubmission`; Slack connector routes `EventTypeInteractive` (`block_actions`, `view_submission`) after `Ack`; Discord connector adds an `InteractionCreate` handler.
- [ ] `plugins.go` flows per spec: first-contact DM, connect modal, approval buttons, text fallback, login_kick resume, posting results (private files by DM), `UpdatePrompt` outcomes. Uses only `Platform` and `plugin.Registry`.
- [ ] `handler.go`: put `Identity` and the prompt outbox in `ctx`; before calling the model, intercept the text fallback; after the reply, flush prompts; then the first-contact check.
- [ ] Config fields and env vars from the spec; `main.go` builds the vault, registry, `mcpstdio` plugin when `DAOUOFFICE_ENABLED`, shares the registry with both executors, closes it on shutdown, and logs (without secrets) why a plugin is disabled.
- [ ] Docs: env table in `README.md`, package table in `AGENTS.md` (`internal/plugin`, `internal/plugin/mcpstdio`, `plugins/daouoffice`), Slack Interactivity step in `docs/slack-bot.md`.
- [ ] Tests: fake platform + fake plugin covering first contact once, connect submit, approve, cancel, non-owner click refused, text fallback, login_kick resume, private file by DM, no empty posts.

### Task B2 (Node purchase)

- [ ] `dao/purchases.mjs`: rules only (`formValues`, `validatePurchase`, constants), driven by config (`budgets`, `projects`, sub-projects, `minPurchaseKrw`); no data.
- [ ] `dao/fx.mjs` (Hana Bank rate via Naver page in the shared browser; cache in `fx_rates` per day), `dao/purchase-doc.mjs`, `dao/purchase-submit.mjs` (fill, attach quotes, first comparison, approval line, submit, find doc id, second comparison, `--fix`/`--refile` equivalents as options), `dao/quotes.mjs` (Playwright headless capture in a separate context of the shared browser, `expect` text checks, blocked/login detection).
- [ ] `tools/purchase.mjs`: `draft_purchase` (local; returns validation issues and the computed field table), `list_purchases` (read), `capture_quote` (local; returns the image; records digest; the approval of `submit_purchase` counts as the human look at the capture), `submit_purchase` (gated; preview = validation + field table + quote images; execute = full submit and verification).
- [ ] `guide/purchase.md`, `scripts/import-legacy.mjs` (purchases with `refiled.json`, `TRIPS`, override tables, `records/`), tests (rules from the source's real cases, rewritten with made-up names and amounts).

### Task B3 (Node inspection and status)

- [ ] `dao/inspection.mjs`, `dao/inspection-report.mjs` (plan, fill, attach receipts and photos, approval line by config, submit, verify, filings), `dao/status.mjs` (per-payment pipeline).
- [ ] `tools/inspection.mjs`: `save_photos` (local; ingest attachments, link to purchase key(s), serials, `otherCard`), `save_receipt` (local), `inspection_candidates` (read), `payment_status` (read), `file_inspection_report` (gated; preview = plan + photo thumbnails; execute = submit + verify).
- [ ] `guide/inspection.md`, tests (VAT split, adjustment rows, title, per-order split, serial append, intangible rules).

### Task B4 (Node claim, trip, certificate)

- [ ] `dao/expense-map.mjs` (purpose map from config + defaults), `dao/expense-claim.mjs`, `dao/trips.mjs` (validation, title), `dao/trip-docs.mjs`, `dao/trip-expenses.mjs`, `dao/trip-submit.mjs`, `dao/certificate.mjs`.
- [ ] `tools/claim.mjs` `file_expense_claim` (gated; month, card, purpose), `tools/trip.mjs` `file_trip` (gated) and `list_trips` (read), `tools/cert.mjs` `issue_certificate` (gated; self-approval; PDF as private resource) and `certificate_history` (read).
- [ ] `guide/{claim,trip,certificate}.md`, tests (month rules, title formats, trip purpose checks, duplicate detection logic, purpose mapping).

## Phase C: review, integration, deploy

- [ ] Independent review per unit (code-reviewer agents), fixes.
- [ ] End-to-end local run: Go registry ↔ real `plugins/daouoffice` server (no DaouOffice login): list tools, call `guide`, call a write tool in preview with fake creds and see `AUTH_FAILED`/`NOT_CONNECTED` mapping.
- [ ] `scripts/deploy.sh`: sync `plugins/` (without `node_modules`), `npm ci --omit=dev`, `npx playwright install chromium`; one-time provisioning function for Node 24 and Chromium system deps; keep the existing health check.
- [ ] Host: write `configs/daouoffice.json` from the private repo's rules (not committed), add `PLUGIN_SECRET_KEY` and `DAOUOFFICE_*` to `/opt/xylolabs-kb/.env`, deploy, check the logs for the MCP handshake and tool count, open the DaouOffice login page from the plugin's browser.
- [ ] After a person connects in Slack: run the read tools live, then the legacy import for that owner.
