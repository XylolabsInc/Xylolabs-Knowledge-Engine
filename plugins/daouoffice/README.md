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
| `departments.<name>.code` | Fallback department code when the org tree does not give one |
| `cardMatch.vendorAliases`, `.subscriptionMerchants`, `.requisitionFrom` | Receipt vendor ↔ card merchant aliases, subscription merchant regexes, date from which purchases need a requisition |

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

`src/tools/{purchase,inspection,claim,trip,cert}.mjs` are filled by the next modules.

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

One headless Chromium, one context per account (at most two), per-account lock, FIFO queue beyond
that. Each use checks `GET /api/portal/common/notification/badge` and logs in only on 401 (sessions
end about 30 minutes after login no matter what). Idle contexts are parked on `about:blank` after 10
minutes and closed when their session has expired, because closing a context leaves the server session
alive and the next login would meet our own leftover as "another session". Logins are limited to one
attempt per minute and ten per hour per account; a wrong password is not retried.

## Tests

```bash
npm test
```

`test/login.test.mjs` drives a local fake login page in real Chromium and skips when Chromium is not
installed. The HEIC test needs macOS `sips` to create a sample and skips elsewhere.
