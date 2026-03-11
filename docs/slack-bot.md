# Slack Bot Guide — xylolabs-kb

The Slack bot allows users to ask questions about knowledge base content directly in Slack. When `GEMINI_API_KEY` is configured, the bot listens for @mentions and direct messages, searches the KB for relevant context, and uses Gemini AI to generate grounded, contextual responses.

---

## Prerequisites

### 1. Gemini API Key

1. Go to [Google AI Studio](https://aistudio.google.com/apikey)
2. Click **Create API Key**
3. Copy the key and set `GEMINI_API_KEY` in your `.env`

### 2. Slack App Setup

The bot requires additional OAuth scopes beyond the standard xylolabs-kb connector scopes.

#### Additional Bot Token Scopes

Add these scopes to your Slack app's **OAuth & Permissions > Bot Token Scopes**:

| Scope | Purpose |
|-------|---------|
| `app_mentions:read` | Detect @mentions in channels |
| `channels:join` | Auto-join public channels |
| `chat:write` | Post replies to channels and threads |
| `im:history` | Read user DM history |
| `im:read` | Access DM channels |
| `im:write` | Send DM replies |
| `files:read` | Read file content and metadata |

#### Event Subscriptions

In **Event Subscriptions**, subscribe to:

| Event | Purpose |
|-------|---------|
| `app_mention` | Triggered when bot is @mentioned |
| `message.im` | Triggered for DMs to the bot |
| `message.channels` | (already required) Triggered for channel messages |
| `message.groups` | (already required) Triggered for private channel messages |
| `channel_created` | Auto-join newly created public channels |

### 3. Environment Configuration

Update your `.env`:

```env
# Gemini AI
GEMINI_API_KEY=your-api-key-here
GEMINI_MODEL=gemini-3.1-flash-lite-preview

# Knowledge Base Repo (markdown Git repo)
KB_REPO_DIR=/path/to/knowledge-repo

# Slack (existing config, now with bot support)
SLACK_ENABLED=true
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
SLACK_SIGNING_SECRET=...
```

The `KB_REPO_DIR` must point to a local clone of the markdown knowledge repository. The bot will `git pull` automatically before each query.

---

## How the Bot Works

### Message Flow

```
User mentions bot or sends DM
        │
        ▼
Bot handler receives event
        │
        ├── Extract question from message
        ├── Check for attached files (images, PDFs)
        │
        ▼
Load KB context from markdown repo
        │
        ├── git pull --rebase (rate-limited, 30s gap)
        ├── Load index files (topics, keywords, people, weekly)
        ├── Score index sections by query keywords
        ├── Extract file references from matching sections
        ├── Load top 10 relevant detail files
        │
        ▼
Build Gemini prompt
        │
        ├── System prompt (Korean default, English if asked)
        ├── KB indexes + relevant details as context
        ├── Include thread history (up to 20 messages)
        ├── Include attached images if any
        │
        ▼
Call Gemini API
        │
        ├── Send prompt with low thinking level
        ├── Get buffered response
        │
        ▼
Post reply directly in thread
        │
        ├── Extract and save LEARN blocks (async)
        ├── Convert Markdown → Slack mrkdwn
        ├── Truncate to 3000 chars
        └── chat.postMessage in thread (for mentions) or DM channel (for DMs)
```

The bot posts the final response directly without a typing indicator, keeping the conversation clean.

### Tool Calling (Function Calling)

The bot supports Gemini function calling to perform write operations on Google Drive and Notion. When a user asks the bot to create a document or upload a file, the bot uses Gemini's function calling mechanism — not text-based tool invocation.

#### Available Tools

| Tool | Description | Parameters |
|------|-------------|------------|
| `create_google_doc` | Create a new Google Docs document on Drive | `title` (required), `content` (required), `folder_id` (optional) |
| `create_drive_folder` | Create a folder in Google Drive | `name` (required), `parent_folder_id` (optional) |
| `upload_to_drive` | Upload attached files to Google Drive | `file_name` (optional, empty=all), `folder_id` (optional) |
| `delete_drive_file` | Delete a file or folder from Google Drive | `file_id` (required) |
| `rename_drive_file` | Rename a file or folder in Google Drive | `file_id` (required), `new_name` (required) |
| `edit_google_doc` | Replace content of an existing Google Doc | `file_id` (required), `content` (required) |
| `create_notion_page` | Create a new Notion page | `title` (required), `content` (required), `parent_page_id` (optional) |
| `update_notion_page` | Append content to an existing Notion page | `page_id` (required), `content` (required) |

#### Tool Calling Flow

```
User sends message (with optional file attachment)
        │
        ▼
Bot builds Gemini request with function declarations
        │
        ▼
Gemini returns response
        │
        ├── Text response → post reply (normal flow)
        │
        └── Function call(s) → execute tools
                │
                ├── Execute each tool (Google Drive API / Notion API)
                ├── Collect results (URLs, errors)
                ├── Append tool call + results to conversation
                │
                ▼
            Re-call Gemini with tool results (max 5 iterations)
                │
                └── Final text response → post reply with result URLs
```

#### Tool Usage Guide

- **"올려줘" / "업로드"** → `upload_to_drive` — uploads the original attached file as-is
- **"문서 만들어줘" / "작성해줘"** → `create_google_doc` — creates a new Google Docs document
- **"폴더 만들어줘"** → `create_drive_folder` — creates a folder, can be used before uploading
- **"삭제해줘"** → `delete_drive_file` — deletes a file or folder by ID
- **"이름 바꿔줘"** → `rename_drive_file` — renames a file or folder
- **"내용 수정해줘"** → `edit_google_doc` — replaces content of an existing Doc

#### Setup Requirements

For Google Drive write operations, the Service Account needs write scopes:
- `https://www.googleapis.com/auth/drive` (read-write)
- `https://www.googleapis.com/auth/documents` (read-write)

These must be added in the Google Workspace Admin Console under **Security > API Controls > Domain-wide Delegation**.

For Notion write operations, the integration needs **Insert content** and **Update content** capabilities.

### Thread Continuation

Once the bot replies in a thread, all subsequent messages in that thread are handled automatically — no @mention needed. The bot uses a dual-cache system:

1. **Positive cache** — threads where the bot has already replied (in-memory, no expiration)
2. **Negative cache** — threads confirmed not involving the bot (5-minute TTL)
3. **API fallback** — on cache miss, queries Slack API for bot messages in the thread

This enables natural multi-turn conversations without requiring users to @mention the bot in every message.

### Multi-turn Context

For thread conversations, the bot fetches up to 20 prior messages from the thread and includes them as conversation history in the Gemini prompt. This enables:

- Follow-up questions that reference earlier context
- The bot "remembering" what was discussed earlier in the thread
- Natural conversational flow without repeating context

Messages are formatted with proper user/model turn alternation as required by the Gemini API.

### Bidirectional Learning

When users share factual information (e.g., phone numbers, account details, company facts), the bot can learn and save it to the knowledge base. The system prompt instructs Gemini to output structured `===LEARN: topic===...===ENDLEARN===` blocks when new facts are shared.

These blocks are:
1. Extracted from the Gemini response
2. Stripped from the visible reply (users don't see them)
3. Saved asynchronously to the knowledge repo via `kbReader.SaveFact()`

The bot only learns factual information — opinions, questions, and already-known facts are ignored.

### Business Document Lookup

The bot has specialized rules for company information queries:

- **Business registration** (`사업자등록증`): provides exact values for registration numbers, corporate registration numbers, representative names, company address, business type
- **Financial details**: bank account numbers, seals (`인감`), articles of incorporation (`정관`)
- **Original document links**: always includes links to original files (Google Drive/Notion) when available

Example:
```
User: 사업자등록번호 알려줘
Bot:  사업자등록번호 123-45-67890임. 원본: <URL|사업자등록증>
```

### Response Formatting

Gemini responses are converted from Markdown to Slack mrkdwn before posting:

| Markdown | Slack mrkdwn | Display |
|----------|-------------|---------|
| `**bold**` | `*bold*` | **bold** |
| `[text](url)` | `<url\|text>` | linked text |
| `## Header` | `*Header*` | bold text |
| `~~strike~~` | `~strike~` | ~~strike~~ |

Responses are truncated at 3,000 characters to stay within Slack's message limits.

### Knowledge Retrieval

The bot uses a two-stage hierarchical approach to read the markdown knowledge repo:

1. **Load indexes** — always reads `indexes/*.md` (topics, keywords, people, weekly summaries) and source/channel READMEs
2. **Score sections** — splits each index into sections (by `##` headers), counts keyword matches against the query
3. **Extract references** — finds markdown links (`](path.md)`) in high-scoring sections
4. **Path matching** — also matches query keywords against all detail file paths
5. **Load details** — reads the top 10 highest-scoring detail files
6. **Build context** — combines indexes + relevant details into the Gemini prompt

This keeps the context window small and focused even as the knowledge repo grows to thousands of files.

---

## Usage Examples

### Mention in a Channel

```
User: @xylolabs-kb How do I deploy the API to production?
Bot:  Based on our knowledge base, the deployment process involves...
      [detailed response grounded in KB content]

      Sources:
      - Deployment Runbook (Notion)
      - DevOps Handbook (Google Docs)
```

### Direct Message

```
User: (DM) How do we handle authentication?
Bot:  (DM reply) Authentication is configured via OAuth2...
      [response grounded in KB documents about auth]
```

### With Attachments

```
User: @xylolabs-kb Can you explain what's in this PDF?
      [uploads architecture.pdf]
Bot:  This PDF describes the system architecture...
      [Gemini vision processes the PDF]
      [Response synthesizes PDF content + KB context]
```

### When KB Doesn't Have Content

```
User: @xylolabs-kb How do I set up a Mars colony?
Bot:  몰라요. 그런 정보 없음.
```

---

## Supported File Types in Attachments

When users attach files to messages they send to the bot, the system extracts content:

| Format | Extraction Method | Size Limit |
|--------|-------------------|-----------|
| **PDF** | Pure-Go PDF parser (text layers) | 50 MB |
| **PNG/JPG/GIF/WEBP** | Gemini vision API | 20 MB |
| **DOCX** | ZIP + XML parsing | 10 MB |
| **XLSX** | ZIP + XML parsing, extracts cell values | 10 MB |
| **PPTX** | ZIP + XML parsing, extracts slide text | 10 MB |
| **Text files** (.txt, .csv, .json) | Direct read | 10 MB |
| **Web URLs** (in message text) | Article extraction (readability algorithm) | N/A |

---

## Configuration Details

### Response Quality

Bot response quality depends on:

1. **KB content** — more diverse, well-structured markdown documents in the knowledge repo → better answers
2. **Index quality** — well-organized `indexes/*.md` files with links to detail documents improve retrieval
3. **Gemini model** — `gemini-3.1-flash-lite-preview` is recommended; see [Gemini models](https://ai.google.dev/models) for alternatives
4. **Thinking level** — responses use low thinking level for fast response times

### Rate Limiting

The bot respects Gemini API rate limits:

- **Tier**: Depends on your billing plan (free tier: 60 requests/minute)
- **Handling**: If rate limit is hit, the bot replies with "I'm handling too many requests. Please try again in a moment."

### Timeout

The Gemini client has a default 120-second timeout. The bot uses `ThinkingLevel: "low"` (2048 token budget) for fast response times.

### File Limits

- Maximum file download size: 10 MB per attachment
- Maximum reply length: 3,000 characters

---

## Troubleshooting

### "I can't process that message right now"

**Causes:**
- Gemini API key is invalid or expired
- API quota exceeded
- Network timeout

**Solution:**
1. Verify `GEMINI_API_KEY` is set and valid
2. Check Google AI Studio for quota status
3. Check bot logs: `docker compose logs xylolabs-kb | grep bot`

### Bot doesn't respond to @mentions

**Causes:**
- Bot hasn't joined the channel (auto-join requires `channels:join` scope)
- `app_mentions:read` scope missing
- `GEMINI_API_KEY` not set
- Event Subscriptions not configured (must have `app_mention`, `message.channels`, `message.im`, `channel_created`)

**Solution:**
1. Verify `channels:join` scope is added — the bot auto-joins all public channels
2. Verify all required event subscriptions are enabled
3. Ensure `GEMINI_API_KEY` is set in `.env`
4. Reinstall the app after adding scopes

### Bot doesn't respond to DMs

**Causes:**
- `im:read` or `im:write` scopes missing
- DM history isn't being fetched

**Solution:**
1. Verify `im:history`, `im:read`, `im:write` scopes are present
2. Restart the bot service: `docker compose restart xylolabs-kb`

### "I don't have information about that" too often

**Causes:**
- KB doesn't have relevant documents
- Search query isn't matching documents well

**Solution:**
1. Check what's in the KB: `curl http://localhost:8080/stats | jq '.total_documents'`
2. Test search manually: `curl "http://localhost:8080/search?q=your+question&limit=5"`
3. If results are irrelevant, add more detailed documentation to Slack/Google/Notion
4. Use more specific question phrases

### "Gemini Vision isn't processing my image"

**Causes:**
- Image format not supported (only PNG, JPG, GIF, WEBP)
- Image is corrupted or too large (>20 MB)
- Gemini API doesn't have vision capability enabled

**Solution:**
1. Convert image to PNG or JPG
2. Compress if over 20 MB
3. Verify `GEMINI_API_KEY` has vision access (free tier usually does)

### Attachment extraction slow

**Causes:**
- Large file (PDFs with scanned images take longer to OCR)
- Network latency to Gemini API

**Solution:**
1. For PDFs: ensure text layer exists (not pure scanned image)
2. Use compressed image formats (WEBP is smallest)
3. Check network latency: `ping api.gemini.google.com`

---

## Best Practices

### For Users

1. **Be specific** — "How do I fix the auth bug?" yields better results than "help"
2. **Use proper terminology** — use domain-specific terms that appear in KB content
3. **Ask one question at a time** — multiple questions in one message may confuse the bot
4. **Check sources** — bot always includes which KB documents it used; review them for accuracy

### For KB Maintainers

1. **Keep docs fresh** — old, outdated docs confuse the bot and users
2. **Use consistent terminology** — if your team says "deployment" and "release" interchangeably, add both as index terms
3. **Structure docs clearly** — use headers and short paragraphs; bot extracts snippets and they should be self-contained
4. **Add examples** — bot can cite specific code examples if they're in the KB
5. **Link related docs** — use cross-references so bot can build broader context

---

## Advanced Configuration

### Custom Models

To use a different Gemini model (e.g., `gemini-3.1-pro-preview` for higher quality):

```env
GEMINI_MODEL=gemini-3.1-pro-preview
```

### Disable the Bot

To disable bot functionality while keeping KB ingestion:

```env
GEMINI_API_KEY=  # leave empty
```

The bot will not respond to messages, but full KB search is still available via the API.

---

## Architecture Details

The bot handler (`internal/bot/handler.go`):

1. **Event routing** — receives Slack events and routes to appropriate handler
2. **Message parsing** — extracts text and attached files
3. **KB context** — calls `kbrepo.Reader.BuildContext(query)` to load relevant markdown context
4. **Gemini integration** — builds prompt with Korean system prompt, calls Gemini API
5. **Reply posting** — posts response directly to the thread or DM

The extractor (`internal/extractor/`) processes file attachments:

1. **PDF extraction** — uses pure-Go PDF parser
2. **Image description** — sends to Gemini vision API
3. **Office parsing** — XML extraction from DOCX/XLSX/PPTX
4. **URL extraction** — uses readability algorithm

---

## Support & Feedback

For issues or feature requests, check the main xylolabs-kb documentation at `/docs/architecture.md` or open an issue in the repository.
