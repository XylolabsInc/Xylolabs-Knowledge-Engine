# daouoffice plugin

An MCP server (stdio) that lets the Xylolabs bot work in DaouOffice for each person with their own
account: lookups (근태, 연차, 결재함, 내 기안, 문서, 양식, 법인카드), and gated writes that only run
after a person approves them in Slack or Discord. The bot (`internal/plugin/mcpstdio`) spawns and
supervises it. Design: `docs/superpowers/specs/2026-10-26-plugins-daouoffice-design.md`.

This repository is public. Names, card numbers, DaouOffice IDs, purchase records and API responses
never go into this directory; company rules live in a server-side config file and per-person state in
the plugin database.

## Run

```bash
cd plugins/daouoffice
npm ci --omit=dev
npx playwright install --with-deps chromium   # once per host (Chromium and its system libraries)

DAOUOFFICE_DATA_DIR=/opt/xylolabs-kb/data/plugins/daouoffice \
DAOUOFFICE_CONFIG=/opt/xylolabs-kb/configs/daouoffice.json \
node --disable-warning=ExperimentalWarning src/server.mjs
```

`--disable-warning=ExperimentalWarning` silences Node 24's notice for `node:sqlite`. stdout carries
MCP only; logs are JSON lines on stderr (the bot copies them into its own log).

| Env var | Default | Meaning |
|---|---|---|
| `DAOUOFFICE_DATA_DIR` | `plugins/daouoffice/data` | `daouoffice.db` and `files/<owner>/{photos,receipts,quotes,out}` |
| `DAOUOFFICE_CONFIG` | unset → empty rules | Company rules JSON. Unset starts the server, but DaouOffice tools refuse without `baseUrl`. A path that does not exist or does not parse stops the server |
| `DAOUOFFICE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DAOUOFFICE_MAX_CONTEXTS` | `2` | DaouOffice pages open at once (each roughly 150–300 MB) |
| `DAOUOFFICE_MAX_PARKED` | `6` | Idle logged-in contexts kept without a page, so their sessions are not stranded |
| `DAOUOFFICE_QUOTE_ALLOW_PRIVATE` | unset | Tests only. `1` lets `capture_quote` open loopback and private addresses. Otherwise captures refuse non-public hosts, checked by DNS for every request of the capture context, redirects included: cloud metadata, RFC 1918, CGNAT and Tailscale, link-local, `*.local`, `*.internal` |

## Company config

Copy `config.example.json` to the server and fill it in. Never commit the filled file.

| Key | Meaning |
|---|---|
| `baseUrl` | `https://<tenant>.daouoffice.com` |
| `people.ceo`, `.cfo`, `.cto` | Names as they appear in the org tree; approval lines refer to them by role |
| `cfoFrom` | Project lines add the CFO (합의) from this amount (KRW) |
| `projects.<name>.reviewer` | 합의 reviewer per project. A project without one stops the tool so a person is asked |
| `projects.<name>.subProjects` | Sub-projects the requisition form offers for that project |
| `budgets`, `cards.<last4>` | Budget names; card → project, budget, issuer |
| `lines.purchase`, `.claim`, `.inspection` | `"project"`, `"ctoFinal"`, or a list of `{ "role" \| "name", "type", "dept"? }` |
| `trip.approvers`, `trip.referers` | Trip line and references, same entry shape |
| `purposeMap`, `minPurchaseKrw` | Expense purpose mapping; the requisition warning threshold |
| `purchase.formName`, `.projectBudget`, `.payWhen`, `.payMethods`, `.cardMethod`, `.cardIssuers`, `.rateRange` | 구매 품의서 양식 선택지. Empty means the current form's values (budget `프로젝트 사업비`, 지급요청일 `당월`/`익월`, …). A non-empty `projects.<name>.subProjects` makes the sub-project mandatory for new requisitions |
| `departments.<name>.code` | Fallback department code when the org tree does not give one |
| `cardMatch.vendorAliases`, `.subscriptionMerchants`, `.requisitionFrom` | Receipt vendor ↔ card merchant aliases, subscription merchant regexes, date from which purchases need a requisition |
| `inspection.perOrderMerchants`, `.perOrderFrom`, `.serialFromKrw` | Merchants (regexes, default `ALIEXPRESS`) whose charges get one 검수조서 per requisition, and from which payment date; amount from which an item needs a serial photo besides the device keywords |

Unknown keys are kept, so a module can add its own section.

## Wire contract with the bot

Request `_meta` on every `tools/call`:

| Key | Value |
|---|---|
| `xylolabs/user` | `{ key: "slack:U123", name, platform }` |
| `xylolabs/credentials` | `{ username, password }`; absent for people who are not connected |
| `xylolabs/allowKick` | `true` lets the login press `기존 접속 종료` |
| `xylolabs/mode` | Write tools run `execute` only for exactly `"execute"`; anything else is a preview |
| `xylolabs/attachments` | `[{ name, mimeType, path }]`, absolute paths in the bot's inbox (read, never deleted) |

Tools: `annotations` read = `{readOnlyHint: true}`, local (plugin DB only) = `{readOnlyHint: false,
destructiveHint: false}`, write = `{readOnlyHint: false, destructiveHint: true}` (gated by the bot).
Tool `_meta`: `xylolabs/kind` and, on the hidden tools, `xylolabs/role` (`connect`, `login`).

Results: `content[0]` is Korean text (never empty), then `image` blocks and `resource` blocks with a
base64 `blob` (`resource.uri` `file:///<name>`; `_meta["xylolabs/private"]` on the block and the
resource means DM only; `resource._meta["xylolabs/name"]` is the file name). `structuredContent` holds
machine data; previews add `preview: true`.

Errors: `isError: true`, `structuredContent: { code, message }`, text = message. Codes:
`NOT_CONNECTED`, `AUTH_FAILED`, `DUPLICATE_SESSION`, `RATE_LIMITED`, `VALIDATION`, `NOT_FOUND`,
`CONFLICT`, `UPSTREAM`, `INTERNAL`. When the SDK rejects arguments against the schema the result is
`isError` with text only (no code); treat it as `VALIDATION`.

The `connect` tool's input schema is the connect form (`username` 아이디, `password` 비밀번호 with
`format: "password"`, optional `allowKick` checkbox). Credentials come in its arguments. It never ends
another session: the duplicate-login modal only appears after a correct password, so it counts as
success. It returns `{ accountId, accountName, duplicate }`; `login` returns the same shape.

## Tools

| Tool | Kind | Purpose |
|---|---|---|
| `connect`, `login` | hidden | Account check from the connect form; `login` (no arguments) signs in and ends the other session, called only after the person pressed 끊고 진행 |
| `me` | read | Connected account, department, department code |
| `attendance_today`, `leave_balance` | read | 근태, 연차 |
| `approval_inbox` | read | 결재할 문서 건수와 목록 |
| `my_drafts` | read | 내 기안 문서와 상태 |
| `read_document` | read | 문서 제목, 상태, 결재선, 수신·참조, 첨부, 본문 칸 |
| `list_forms` | read | 결재 양식 |
| `list_cards`, `card_usage` | read | 법인카드, 사용내역과 상태별 합계 |
| `withdraw_document` | write | 상신취소 |
| `guide` | read | Rules by topic from `guide/*.md` (no account needed) |
| `save_photos` | local | Strip, date and store inspection photos from the attachments; link them to requisitions (`alsoFor`, `otherCard`, serials) |
| `save_receipt` | local | Store receipts, invoices and order pages (image or PDF) under the receipt naming convention |
| `mark_inspection` | local | Per requisition: handled elsewhere, photos missing an item, item amounts |
| `mark_payment` | local | Per card payment: handled elsewhere, refunded, claim as is, pinned receipts (card-match overrides) |
| `inspection_candidates` | read | Payments that need a 검수조서, with photos, checklist, serials and whether it can be filed |
| `payment_status` | read | Receipt, requisition, photos, serials, report and claim per payment |
| `file_inspection_report` | write | One 검수조서 per payment (per order for per-order merchants); `action` new, fix (withdraw and refile), refile (new report linking the completed one) |
| `attach_photos_to_claim` | write | Post inspection photos as comments on the expense claim that holds the payment |
| `draft_purchase` | local | 구매 품의 초안 저장, 검증 결과와 칸별 값 |
| `list_purchases` | read | 저장된 품의 초안과 올린 품의, 키와 판 |
| `capture_quote` | local | 상품 페이지 캡처(견적서 대용) 또는 사람이 찍은 화면을 초안에 붙인다 |
| `submit_purchase` | write | 구매 품의서 상신. `fix`(상신취소 후 같은 문서로), `refile`(새 결재선으로 다시) |

`src/tools/{claim,trip,cert}.mjs` are filled by the next modules.

## Writing a tool module

```js
export function register(server, rt) {
  rt.tool(server, 'submit_thing', {
    kind: 'write',                       // 'read' | 'local' | 'write'
    description: '언제 쓰고, 먼저 무엇을 물어야 하는지',
    input: z.object({ key: z.string() }),
  }, {
    async preview(args, call) { /* validate, show the field table; change nothing */ },
    async execute(args, call) { /* validate again, fill, submit, re-read and compare */ },
  });
}
```

- `call = { user, creds, allowKick, mode, attachments, owner }`; `owner` is the lower-cased login id
  and keys every DB row.
- Return `{ text, images?: [{ data, mimeType }], files?: [{ name, data, mimeType, private }], data? }`.
  Throw `PluginError(code, message)` for anything the person or the bot should act on.
- `rt.session(call, async ({ page, api, context, baseUrl, fresh }) => …)` runs on the person's
  logged-in DaouOffice page. `api(path, { method, body })` returns `{ status, ok, data }` and never
  the raw body. `rt.db.account(call.owner)` has the name and department (`deptId`, `deptCode`).
- Write guard: `submitDocument`, `approveDocument`, `withdrawDocument`, `deleteDraft`, `postComment`
  and every non-read `api()` call throw unless the call is an approved execute of a write tool (read
  and local tools are refused too). Read-only POST endpoints are listed in `READ_ONLY_POSTS`
  (`src/dao/api.mjs`); add one there only after checking it changes nothing. A new DAO function that
  changes DaouOffice calls `assertExecute('<what>')` first.
- Login errors are only safe before a write starts: the bot re-offers an action that fails with
  AUTH_FAILED or DUPLICATE_SESSION. After the first `assertExecute()` the runtime reports those as
  UPSTREAM (`data.afterWrite`). Do an execute's whole job in one `rt.session()` call.
- The bot deletes its inbox files when the call ends; copy any attachment you keep.
- Never log arguments, credentials or responses. Use `rt.logger` with plain fields.

## Sessions

One headless Chromium, one context per account, per-account lock. Each use checks
`GET /api/portal/common/notification/badge` and logs in only on 401 (sessions end about 30 minutes
after login no matter what).

No DaouOffice logout is known, and closing a context leaves its server session alive: the next login
would meet our own leftover as "another session". So contexts are not closed while their session may
live. At most `DAOUOFFICE_MAX_CONTEXTS` have a page; an idle one is **parked** (page closed, context
and cookies kept, almost no memory) after 10 minutes, or at once when someone else needs the page
slot, and resumed with a new page without logging in. Parked contexts close when their session has
expired; beyond `DAOUOFFICE_MAX_PARKED` the one closest to expiry is closed. When every page slot is
busy, requests wait in a FIFO queue.

The connect check runs in a throwaway context and never touches the account's live session until the
new password is proven. Logins are limited to one attempt per minute and ten per hour per account,
and the connect check also per chat user; disconnecting does not reset the limiter. A wrong password
is not retried. `rt.sessions.withScratchContext(fn)` gives a cookie-less context of the same browser
(product pages, exchange rates) under the same page limit.

## Tests

```bash
npm test
```

`test/login.test.mjs` drives a local fake login page in real Chromium and skips when Chromium is not
installed. The HEIC test needs macOS `sips` to create a sample and skips elsewhere.
