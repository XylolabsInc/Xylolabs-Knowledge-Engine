# Plugin System and the DaouOffice Plugin — Design

**Date**: 2026-10-26
**Status**: Approved scope (user, 2026-10-26), implementation in progress
**Scope**: Xylolabs-Knowledge-Engine. Ports the automation in the private
`xylolabs-daouoffice` repository into this bot as a plugin named
`daouoffice`, behind a generic plugin layer so another groupware or ERP can be
added later without touching the bot.

## Decisions (from the user)

| Topic | Decision |
|---|---|
| Accounts | Every person connects their own DaouOffice account. The bot asks each person to connect the first time they talk to it. ID and password are collected. |
| Purchasing | Purchase requisitions (구매 품의서) are the priority. Paying at a shop is out of scope for now. |
| Runtime | The existing AWS host that already runs `xylolabs-kb` (Ubuntu 26.04, arm64, 2 vCPU, 3.7 GB RAM). |
| Scope | Read-only lookups, photo → 검수조서, 구매 품의서, 법인카드 지출결의, 출장 결재, 증명서 발급. All of them. |

## Constraints that shape the design

1. **This repository is public.** Names of staff on approval lines, card ↔
   project mappings, department IDs, and anyone's purchase history are not
   committed here. They live in a server-side config file and in the plugin
   database. The repository ships a placeholder example only.
2. **One DaouOffice session per account, 30-minute absolute expiry.** A login
   shows a "동시 접근 제한" modal when the same account is signed in elsewhere;
   accepting it kills the other session. The bot must never kick someone out
   without their consent.
3. **Writes are irreversible and notify the approval line.** The model may
   prepare a document, but only a deterministic, human-confirmed step may
   submit it. A model claiming "the user approved" must not be able to submit.
4. **The automation is Node + Playwright** (about 10k lines, proven DOM flows).
   The bot is a pure-Go binary (no CGO). Go's `plugin` package needs CGO, and
   rewriting the DOM flows in Go throws away the proven code.
5. **Memory is tight on the host** (about 1.4 GB available). Chromium must be
   shared and contexts closed when idle.
6. **Passwords must not land in chat history or in the model's context.** Bots
   cannot delete a user's DM message, so credentials are collected through a
   modal form, never as a chat message, and never passed to the LLM.

## Architecture

```
Slack / Discord
   │  messages, button clicks, modal submissions
   ▼
internal/bot ── Platform (PostPrompt, UpdatePrompt, OpenForm, OpenDM)
   │  connect flow, approval flow, "승인"/"취소" text fallback
   ▼
internal/tools.ToolExecutor ── built-in tools + plugin.Registry tools
   ▼
internal/plugin
   ├─ Registry        prefixes tool names, gates write tools, pending actions
   ├─ Vault           AES-256-GCM credential encryption (PLUGIN_SECRET_KEY)
   └─ mcpstdio        generic plugin over MCP stdio (official Go SDK)
        │ JSON-RPC over stdin/stdout, request _meta carries identity
        ▼
plugins/daouoffice  (Node 24, @modelcontextprotocol/server v2, Playwright)
   ├─ sessions        one Chromium, one BrowserContext per account, per-account lock
   ├─ dao/*           ported DaouOffice modules (forms, approval line, purchase,
   │                  inspection, claim, trip, certificate, cards, matching)
   ├─ db              node:sqlite, plugin-owned state (replaces purchases.mjs etc.)
   └─ guide/*.md      rules from the skills, without names
```

### Why MCP over stdio

- The plugin is out of process, in any language, and is spawned and supervised
  by the bot. A future system ships as another MCP server plus one config entry.
- MCP already carries what the bot needs: tool listing with JSON Schema
  (converted to Gemini function declarations), tool annotations
  (`readOnlyHint`, `destructiveHint`) for gating, `_meta` on each request for
  identity, and image or resource content for files.
- Verified with a spike on 2026-10-26: Go SDK `github.com/modelcontextprotocol/go-sdk`
  v1.7.0 client ↔ Node `@modelcontextprotocol/server` 2.4.0 over stdio
  negotiates protocol `2025-11-25`; request `_meta` arrives as
  `ctx.mcpReq._meta`; tool `_meta` and annotations round-trip.

## Go side

### `internal/plugin`

```go
type Identity struct {
    Platform, UserID, UserName, Channel, ThreadID, MessageID string
    IsDM bool
}
func (i Identity) Key() string // "slack:U123"

type Mode string // "preview" | "execute"

type Tool struct {
    Name        string         // without the plugin prefix
    Description string
    Parameters  map[string]any // sanitized JSON Schema (Gemini subset)
    Write       bool           // destructiveHint && !readOnlyHint → gated
    Role        string         // "" | "connect" | "login" — hidden from the model
}

type Attachment struct{ Name, MimeType, Path string }
type File struct {
    Name, MimeType string
    Data           []byte
    Private        bool // deliver by DM only (certificates)
}
type Result struct {
    Text  string
    Files []File
    Data  any // structuredContent
}

type CallRequest struct {
    Identity    Identity
    Tool        string
    Args        map[string]any
    Credentials map[string]string // decrypted, never logged
    AllowKick   bool
    Mode        Mode
    Attachments []Attachment
}

type Plugin interface {
    Name() string         // "daouoffice"
    Title() string        // "다우오피스"
    Instructions() string // system-prompt fragment
    Tools() []Tool
    AccountFields() []AccountField // from the connect tool's input schema
    Call(ctx context.Context, req CallRequest) (*Result, error)
    Close() error
}
```

Plugin errors are typed so the bot can react without parsing prose:
`ErrNotConnected`, `ErrAuthFailed`, `ErrDuplicateSession`, `ErrPlugin{Code, Message}`.
The MCP adapter maps a tool result with `isError: true` and
`structuredContent.code` to these.

**Registry**

- Exposes `Declarations()` as `<plugin>_<tool>` (e.g. `daouoffice_card_usage`),
  skipping `Role != ""` tools, plus three account tools owned by the registry:
  `accounts_status`, `accounts_connect(plugin)`, `accounts_disconnect(plugin)`.
- `Dispatch(ctx, name, args)`:
  1. Resolve plugin and tool; read `Identity` and attachments from `ctx`.
  2. No stored account → queue a connect prompt, return
     `{status: "not_connected"}` so the model tells the user a DM was sent.
  3. Read tool → call with `Mode=execute`, return the result.
  4. Write tool → call with `Mode=preview`. On success store a pending action
     (args, attachments copied to the plugin inbox, preview text, owner,
     channel, thread, expiry 30 min) and queue an approval prompt. Return
     `{status: "awaiting_approval", preview}` to the model. Nothing is submitted.
  5. `ErrDuplicateSession` → store a `login_kick` pending action that holds the
     original message, and queue a prompt "다른 곳에서 접속 중입니다. 봇이
     로그인하면 그 접속이 끊깁니다. [끊고 진행] [취소]".
  6. `ErrAuthFailed` → mark the account invalid and queue a reconnect prompt.
     No retry, to avoid locking the account.
- `Approve(ctx, actionID, approver Identity)`: only the owner, only `pending`,
  not expired; atomically flips to `running`, calls `Mode=execute` with the
  stored args, stores the result, returns it for posting.
- `Cancel(actionID, by Identity)`.
- `Connect(ctx, id, plugin, values)`: calls the hidden `connect` tool, which
  signs in and reports the account profile. A duplicate-login modal after a
  correct password counts as a successful check (the modal only appears after
  authentication) and the plugin backs out without kicking anyone. On success
  the values are encrypted and stored.

Prompts are queued through a context outbox (same pattern as
`tools.ContextWithOutbox`) and posted by the bot after its reply, so the
model's text and the deterministic buttons arrive in order.

**Schema sanitizer**: drop `$schema`, `additionalProperties`, `$defs`,
`$ref`, `default`, `examples`; drop `minimum`/`maximum` outside int32; keep
`type`, `properties`, `required`, `items`, `enum`, `description`, `format`
(only `date`/`date-time`), `nullable`.

**Vault**: AES-256-GCM, 32-byte key from `PLUGIN_SECRET_KEY` (base64). The
account row stores `nonce || ciphertext` of the JSON values. No key → plugins
that need accounts stay disabled and the bot logs why.

### Storage (`internal/storage`, migration N+1)

```sql
CREATE TABLE plugin_accounts (
  user_key TEXT NOT NULL, plugin TEXT NOT NULL,
  secret BLOB NOT NULL,            -- nonce||ciphertext
  account_name TEXT DEFAULT '',    -- e.g. "홍길동 (개발본부)"
  account_id TEXT DEFAULT '',      -- plugin's stable owner key (login id)
  allow_kick INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'ok',   -- ok | invalid
  connected_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
  PRIMARY KEY (user_key, plugin));
CREATE TABLE plugin_prompts (
  user_key TEXT NOT NULL, plugin TEXT NOT NULL, prompted_at DATETIME NOT NULL,
  PRIMARY KEY (user_key, plugin));
CREATE TABLE plugin_actions (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL,        -- call | login_kick
  user_key TEXT NOT NULL, plugin TEXT NOT NULL, tool TEXT NOT NULL,
  args TEXT NOT NULL, attachments TEXT NOT NULL DEFAULT '[]',
  origin TEXT NOT NULL DEFAULT '{}',               -- channel, thread, message, text
  preview TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,     -- pending | running | done | failed | cancelled | expired
  result TEXT NOT NULL DEFAULT '',
  prompt_channel TEXT DEFAULT '', prompt_message TEXT DEFAULT '',
  created_at DATETIME NOT NULL, expires_at DATETIME NOT NULL, updated_at DATETIME NOT NULL);
```

### Bot and platforms

New `Platform` methods (implemented for Slack and Discord):

```go
PostPrompt(ctx, channel, threadID string, p Prompt) (messageID string, err error)
UpdatePrompt(ctx, channel, messageID, text string) error // removes the buttons
OpenForm(ctx, trigger string, f Form) error
OpenDM(ctx, userID string) (channel string, err error)
```

`Prompt{Text, Buttons []Button}`, `Button{ActionID, Label, Value, Style}`,
`Form{CallbackID, Title, Submit, Fields []FormField, Metadata}`,
`FormField{ID, Label, Hint, Secret, Optional, Checkbox}`.

New `BotHandler` methods: `HandleAction(ctx, *Action)` and
`HandleFormSubmission(ctx, *FormSubmission)`. Slack delivers them as Socket
Mode `interactive` events (`block_actions`, `view_submission`); Discord as
`InteractionCreate` (button, modal submit). Slack's app config must have
**Interactivity & Shortcuts** turned on (no request URL in Socket Mode).

Flows in `internal/bot/plugins.go`:

- **First contact**: after any reply, if a plugin needs an account and the user
  has neither an account nor a prompt record, DM once:
  "다우오피스 계정을 연결하면 결재, 품의, 검수조서를 여기서 처리할 수 있습니다.
  [연결하기]". Record the prompt.
- **Connect**: button → modal (아이디, 비밀번호, "봇이 로그인할 때 내 기존
  접속을 끊어도 됩니다" checkbox) → ack immediately → DM "확인 중" →
  `Registry.Connect` → DM the result. The modal text is never logged.
- **Approval**: the preview prompt has [상신] [취소]. Click → owner check →
  `UpdatePrompt("상신 중…")` → `Registry.Approve` → post the result in the
  thread (files too; `Private` files go by DM) → `UpdatePrompt` with the
  outcome. A thread reply that is exactly `승인`, `상신`, `진행` or `취소` from
  the owner while an action is pending in that thread does the same without
  the model. This also covers a workspace where buttons are unavailable.
- **login_kick**: [끊고 진행] signs in with `AllowKick`, then re-runs the
  stored original message through `respond`.

`internal/slack/connector.go` routes `socketmode.EventTypeInteractive` to the
bot handler and acks first. AGENTS.md rule 11 (never post an empty body) holds
for every new post.

### Configuration

| Env var | Default | Meaning |
|---|---|---|
| `PLUGIN_SECRET_KEY` | *(empty → account plugins disabled)* | base64 of 32 random bytes |
| `DAOUOFFICE_ENABLED` | `false` | Start the daouoffice plugin |
| `DAOUOFFICE_COMMAND` | `node plugins/daouoffice/src/server.mjs` | Command line to spawn |
| `DAOUOFFICE_DATA_DIR` | `data/plugins/daouoffice` | Plugin DB, inbox, files |
| `DAOUOFFICE_CONFIG` | `configs/daouoffice.json` | Company rules (not committed) |

`cmd/xylolabs-kb/main.go` builds one registry shared by the Slack and Discord
executors and closes it on shutdown.

## Node side: `plugins/daouoffice`

```
package.json            "type": "module", engines node >=24
config.example.json     structure with placeholder names
src/server.mjs          McpServer + StdioServerTransport, tool registration
src/runtime.mjs         reads _meta (user, credentials, mode, attachments), wraps handlers,
                        maps PluginError → {isError, structuredContent:{code,message}}
src/errors.mjs          PluginError(code, message)
src/config.mjs          loads DAOUOFFICE_CONFIG, validates shape
src/sessions.mjs        SessionManager
src/db.mjs              node:sqlite schema and repository functions
src/photos.mjs          metadata strip, orientation, resize, HEIC → JPEG (sharp, heic-convert)
src/dao/*.mjs           ported modules
src/tools/*.mjs         one file per domain, each exports register(server, rt)
guide/*.md              rules ported from the skills, no names
scripts/import-legacy.mjs   one-off import from the private repo (run on the host)
test/*.test.mjs         node:test
```

### Session manager

- One `chromium.launch({headless: true})`, created on first use and closed
  when the last context closes.
- Per account: `{context, page, api, loggedInAt, lastUsed, lock}`. A context is
  created with `locale ko-KR`, `timezoneId Asia/Seoul`, viewport 1440×900.
- `withSession(owner, creds, {allowKick}, fn)` takes the account lock, checks
  `GET /api/portal/common/notification/badge`, signs in if needed, runs `fn`.
  At most two live contexts; extra work waits in a FIFO queue. Idle contexts
  close after 10 minutes.
- Login (`dao/login.mjs`, from `session.mjs`) takes credentials as arguments.
  Wrong password → `AUTH_FAILED` (no retry). Duplicate modal → if `allowKick`
  click "기존 접속 종료", else `DUPLICATE_SESSION` and close the context.
- No keepalive polling: the session is checked lazily before each action.
- Rate limit: at most one login per account per 60 s and 10 per hour.

### Identity and per-user facts

The owner key is the DaouOffice login ID. On connect the plugin reads
`GET /gw/api/user/session` and the organization tree to cache the person's
name, department ID, and department code. These replace the hard-coded
`DRAFT_DEPT_ID`, `DRAFT_DEPT_CODE`, and drafter name.

### Company config (`DAOUOFFICE_CONFIG`, server only)

```json
{
  "baseUrl": "https://<tenant>.daouoffice.com",
  "people": { "ceo": "<name>", "cfo": "<name>", "cto": "<name>" },
  "cfoFrom": 1000000,
  "projects": { "<project>": { "reviewer": "<name>", "subProjects": [] } },
  "budgets": ["운영자금", "프로젝트 사업비"],
  "cards": { "<last4>": { "project": "<project>", "budget": "프로젝트 사업비", "issuer": "<issuer>" } },
  "lines": {
    "purchase": "project",
    "claim": [{ "name": "<ceo>", "type": "결재" }],
    "inspection": "ctoFinal"
  },
  "trip": { "approvers": [{ "name": "<ceo>", "type": "결재" }], "referers": [{ "name": "<name>", "dept": "<dept>" }] },
  "purposeMap": [],
  "minPurchaseKrw": 100000
}
```

`ctoFinal`: the drafter alone when the drafter is the CTO, otherwise
`[{name: cto, type: 결재}]`. `project`: reviewer (합의) → CFO (합의, amount ≥
`cfoFrom`) → CEO (결재).

### Database (`node:sqlite`, `$DAOUOFFICE_DATA_DIR/daouoffice.db`)

All rows carry `owner` (DaouOffice login ID).

| Table | Replaces | Key columns |
|---|---|---|
| `accounts` | hard-coded drafter facts | owner, name, dept_id, dept_code, dept_name, updated_at |
| `purchases` | `PURCHASES` in `purchases.mjs`, `refiled.json` | id, owner, key, status, title, paid_at, amount_krw, usd, doc_id, data JSON, UNIQUE(owner,key) |
| `quotes` | `output/quotes/_verify.json` | purchase_id, file, url, captured_at, digest, seen_at |
| `receipts` | `receipts/` filename convention | owner, file, date, category, item, vendor, amount, currency, kind, order_id |
| `photos` | `pictures/_matches.json`, `_taken.json` | owner, file, sha256, taken_date, purchase_key, also_for JSON, shows, other_card, comments JSON |
| `overrides` | `HANDLED_ELSEWHERE`, `REFUNDED`, `PINNED`, `CLAIM_AS_IS`, `INSPECTION_DONE`, `INSPECTION_INCOMPLETE`, `ITEM_OVERRIDES` | owner, kind, match JSON, data JSON |
| `trips` | `TRIPS` in `trips.mjs` | owner, data JSON, doc_id, status |
| `filings` | `output/검수조서_상신기록.tsv` and similar | owner, kind, ref, doc_id, title, status, filed_at, detail JSON |
| `fx_rates` | `FX` constant | day, rate, at, source |

Files live under `$DAOUOFFICE_DATA_DIR/files/<owner>/{photos,receipts,quotes,out}`.

### Tools

Read (no approval):

| Tool | Purpose |
|---|---|
| `me` | Connected account, department |
| `attendance_today`, `leave_balance` | 근태, 연차 |
| `approval_inbox` | 결재할 문서 건수와 목록 |
| `my_drafts` | 내 기안 문서와 상태 |
| `read_document` | 문서 상세(제목, 상태, 결재선, 본문 칸) |
| `list_forms` | 결재 양식 |
| `list_cards`, `card_usage` | 법인카드, 사용내역 |
| `payment_status` | 결제별 영수증, 품의, 사진, 검수조서, 지출결의 상태 |
| `list_purchases` | 저장된 품의 초안과 상신 기록 |
| `inspection_candidates` | 검수조서가 필요한 결제와 사진 현황 |
| `guide` | 규칙 문서(주제별) |

Local writes (plugin DB only, no approval): `save_photos` (strip metadata,
store, link to a requisition and serials), `save_receipt`, `draft_purchase`
(validate and store), `capture_quote` (product page screenshot, returned for
the user to look at).

Gated writes (preview → approval → submit): `submit_purchase`,
`file_inspection_report`, `file_expense_claim`, `file_trip`,
`issue_certificate`, `withdraw_document`.

Every gated tool runs its full validation in preview mode and again in execute
mode. Execute keeps the existing safeguards: fill, read back, refuse on
mismatch, submit, re-read the server document field by field, and report
mismatches as a failure.

### Porting rules

- DOM flows are copied, not redesigned; only the seams change (credentials,
  department, approvers, file paths, data source).
- `sips` → `sharp`; `exiftool` → `sharp` re-encode plus a JPEG segment check
  (no APP1/APP13 left); macOS Chrome and raw CDP capture → Playwright
  headless in the shared browser.
- `writing.mjs` checks stay; validation messages stay in Korean.
- Module-level file reads become DB calls; no `process.exit`, no signal
  handlers, no cwd-relative paths.

### Legacy import

`scripts/import-legacy.mjs --repo <path> --owner <login id>` imports, for one
person: `PURCHASES` (with `refiled.json` merged), `TRIPS`, the override tables,
and `records/`. It runs on the host against a copy of the private repository;
nothing from it is committed here. Receipts and photos that are not in the
checkout are uploaded later through Slack.

## Out of scope (this round)

- Paying at a shop (AliExpress, 컴퓨존). AliExpress order intake also needs a
  human to sign in through a visible browser, which the host cannot show.
- NIPA 연구비카드 registration (a separate system with its own account).
- Approving other people's documents.

## Testing

- Go: table-driven tests for the registry (prefixing, hidden tools, gating,
  owner checks, expiry, double-approve), vault, schema sanitizer, storage
  methods, bot flows with a fake platform and a fake plugin, Slack
  interaction parsing. An MCP adapter test runs a tiny Node server when `node`
  is on PATH and skips otherwise.
- Node: `node --test` for pure rules (validatePurchase, formValues, writing,
  titles, approval lines, card matching, VAT split), photo stripping, the DB,
  the runtime's preview/execute gating and error mapping, and the session
  manager against a fake browser.
- Live: on the host, the MCP handshake, Chromium launch, and the login page.
  Account-bound checks run after a person connects through Slack.
