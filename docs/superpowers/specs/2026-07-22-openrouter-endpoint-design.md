# OpenRouter (OpenAI-Compatible) Endpoint Support — Design

**Date**: 2026-07-22
**Status**: Approved design, pending implementation
**Scope**: Xylolabs-Knowledge-Engine. The sibling xylolabs-api change is
specified in that repository
(`docs/superpowers/specs/2026-07-22-openai-compatible-llm-endpoint-design.md`);
both repos share the same operator-facing convention: setting `LLM_ENDPOINT` +
`LLM_API_KEY` switches the service to an OpenAI-compatible provider, leaving
everything unset keeps native Gemini.

## Goal

Let the KB service (Slack/Discord bot, kb-gen, image extractor) talk to any
OpenAI-compatible `chat/completions` endpoint — primarily OpenRouter — as a
deploy-time switch. Native Gemini remains the zero-config default and keeps
its full feature set. Also bump every outdated `gemini-3.5-flash` default to
`gemini-3.6-flash` (current GA model).

## Current state

`internal/gemini/client.go` (443 lines) is a hand-rolled client for the
**native** Gemini REST API: POST
`{geminiAPIBase}/{model}:generateContent` with `x-goog-api-key`. Public
surface: `NewClient(apiKey, model, logger)`,
`Generate(ctx, GenerateRequest) (*GenerateResponse, error)`,
`GenerateFromImage(...)`, `SetTimeout(d)`.

Native features in active use: `thinkingConfig.thinkingBudget` (levels
none/low/medium/high → 0/2048/8192/32768), `googleSearch` grounding tool,
function calling (~20 tool declarations from `internal/tools/executor.go`,
`toolConfig` mode AUTO), inline base64 image parts, `thoughtSignature`
round-trip for thinking continuity across tool turns, 3-attempt retry on
429/5xx with Retry-After parsing, 120 s timeout, 50 MB response cap.

Consumers: `cmd/xylolabs-kb/main.go` (bot; per-request model swap to
`GeminiProModel` for creation tasks), `cmd/kb-gen/main.go` (batch KB
generation, `KB_GEN_MODEL`), `internal/extractor/image.go` (vision OCR via a
small `GeminiClient` interface).

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `LLM_ENDPOINT` | *(empty — native Gemini mode)* | Full URL of an OpenAI-compatible `chat/completions` endpoint, e.g. `https://openrouter.ai/api/v1/chat/completions` (same convention as xylolabs-api) |
| `LLM_API_KEY` | *(falls back to `GEMINI_API_KEY`)* | Bearer key for `LLM_ENDPOINT` |

- Added to `internal/config/config.go` next to the Gemini fields
  (`LLMEndpoint`, `LLMAPIKey`).
- Existing model variables keep their names and semantics: `GEMINI_MODEL`,
  `GEMINI_PRO_MODEL`, `KB_GEN_MODEL`. In OpenRouter mode the operator sets
  them to OpenRouter-style IDs (e.g. `google/gemini-3.6-flash`). No new model
  variables — the service already has three model slots and duplicating them
  per provider would double the surface for no benefit.
- Validation: if `LLM_ENDPOINT` is set it must parse as an `http(s)` URL.

## Client design

One client, two transports, selected at construction:

- `NewClient(apiKey, model, logger)` unchanged (native mode).
- New option `NewClient(...).WithEndpoint(baseURL string)` (or a
  `NewOpenAIClient` constructor — implementer's choice, but the returned type
  stays `*Client` so all consumers compile unchanged).
- `Generate`/`GenerateFromImage` keep their exact signatures and semantics;
  the transport translates `GenerateRequest`/`GenerateResponse` to/from the
  OpenAI wire format in new `openai_transport.go` within `internal/gemini/`
  (package ownership per AGENTS.md: this package owns the LLM API client).

Request mapping (OpenAI mode):

| Native concept | OpenAI mapping |
|---|---|
| `SystemPrompt` | leading `role: system` message |
| `Messages` user/model parts | `user`/`assistant` messages; text parts joined |
| Function call in history | `assistant` message with `tool_calls` |
| Function response part | `role: tool` message with `tool_call_id` |
| Inline image part | content array with `image_url` data URI |
| `ThinkingLevel` none/low/medium/high | omit / `reasoning_effort: "low"/"medium"/"high"` |
| `Tools` (`FunctionDeclaration`) | `tools: [{type: "function", function: {...}}]` |
| `GoogleSearch: true` | **silently dropped** — one `slog.Warn` per process (`sync.Once`), request proceeds without grounding |
| `thoughtSignature` | dropped (no OpenAI equivalent) |

Response mapping: `choices[0].message.content` → `Text`; `tool_calls` →
`FunctionCalls` (arguments JSON string decoded to map); `usage.total_tokens`
→ `TokensUsed`; a `reasoning`/`reasoning_content` field, when present, →
`Thinking` (best effort, informational only).

Shared behavior (both modes): retry policy, Retry-After handling, timeout,
50 MB cap, slog structured logging, context propagation — factored so the two
transports reuse the same HTTP execution path.

## Default model bump (independent of the transport work)

`gemini-3.5-flash` → `gemini-3.6-flash` in:

- `internal/gemini/client.go:20` (`defaultModel`)
- `internal/config/config.go:101-102` (`GEMINI_MODEL`, `GEMINI_PRO_MODEL` defaults)
- `cmd/kb-gen/main.go:62,110` (default + flag help)
- `scripts/generate-kb.sh:30`
- `scripts/deploy.sh:297` (hardcoded smoke-test URL)
- Docs: `README.md:211,220,232`, `docs/slack-bot.md:52,333,457`, `docs/setup.md:233`

## Deploy smoke test

`scripts/deploy.sh:297` currently hardcodes a native Gemini
`generateContent` URL. Change: when `LLM_ENDPOINT` is set in the deployed
env, smoke-test `POST {LLM_ENDPOINT}` with the configured key and a 1-token
prompt; otherwise keep the (model-bumped) native check.

## Testing

- Table-driven unit tests (`openai_transport_test.go`, alongside the code per
  AGENTS.md): request building for each mapping row above (system prompt,
  tool declarations, tool-call round-trip, image part, thinking levels,
  googleSearch drop) and response parsing (text, tool_calls, usage, error
  body). No live API calls; `httptest` server for the transport path.
- `go test -race ./...` green; `go build ./...` clean.
- Existing native-mode tests untouched and passing.

## Commit plan

1. `chore(gemini): ⬆️ bump default model to gemini-3.6-flash` (all bump
   sites + docs).
2. `feat(gemini): ✨ OpenAI-compatible endpoint support (OpenRouter)`
   (config + transport + tests + docs).

Deployment of the KB service itself is a separate follow-up decided by the
operator (not part of this change).

## Out of scope

- Runtime failover between providers.
- Mapping Google Search grounding onto OpenRouter's web plugin (decided:
  silently disable in OpenAI mode).
- Streaming (`streamGenerateContent` is unused today).
- Per-request provider selection.
