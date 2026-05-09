# Setup Guide — xylolabs-kb

## Prerequisites

| Requirement | Version | Notes |
|-------------|---------|-------|
| Go | 1.26+ | [go.dev/dl](https://go.dev/dl/) |
| Git | any | for cloning |
| Docker & Docker Compose | v2+ | optional; for containerised deployment |
| A Slack workspace | — | with permission to install apps |
| A Google Cloud project | — | optional; for Google Workspace indexing |
| A Notion workspace | — | optional; for Notion indexing |

xylolabs-kb uses `modernc.org/sqlite` (pure Go) — no system SQLite installation is required.

---

## 1. Clone and Build

```bash
git clone https://github.com/xylolabsinc/xylolabs-kb.git
cd xylolabs-kb

# Build the binary
go build -o bin/xylolabs-kb ./cmd/xylolabs-kb/

# Verify
./bin/xylolabs-kb --version
```

---

## 2. Create a Slack App

### 2a. Create the app

1. Go to [api.slack.com/apps](https://api.slack.com/apps) and click **Create New App**
2. Choose **From scratch**
3. Give it a name (e.g., `xylolabs-kb`) and select your workspace
4. Click **Create App**

### 2b. Configure Bot Token Scopes

1. In the left sidebar, click **OAuth & Permissions**
2. Scroll to **Bot Token Scopes** and add all of the following:

| Scope | Purpose |
|-------|---------|
| `app_mentions:read` | Detect @mentions in channels |
| `channels:history` | Read messages in public channels |
| `channels:join` | Auto-join public channels |
| `channels:read` | List public channels |
| `chat:write` | Post bot replies to channels and threads |
| `groups:history` | Read messages in private channels (where invited) |
| `groups:read` | List private channels |
| `files:read` | Download file attachments |
| `im:history` | Read DM message history |
| `im:read` | Access DM channels |
| `im:write` | Send DM replies |
| `users:read` | Look up user display names |
| `users:read.email` | Look up user email addresses |

### 2c. Enable Socket Mode

1. In the left sidebar, click **Socket Mode**
2. Toggle **Enable Socket Mode** to On
3. You will be prompted to create an App-Level Token — give it a name (e.g., `socket-token`) and add the scope `connections:write`
4. Click **Generate** and copy the token (starts with `xapp-`) — this is your `SLACK_APP_TOKEN`

### 2d. Subscribe to Events

1. In the left sidebar, click **Event Subscriptions**
2. Toggle **Enable Events** to On
3. Under **Subscribe to bot events**, add:
   - `app_mention`
   - `message.channels`
   - `message.groups`
   - `message.im`
   - `channel_created`
4. Click **Save Changes**

The bot automatically joins all public channels during sync and new channels in real-time.

### 2e. Install to Workspace

1. In the left sidebar, click **OAuth & Permissions**
2. Click **Install to Workspace** (or **Reinstall** if already installed)
3. Approve the permissions
4. Copy the **Bot User OAuth Token** (starts with `xoxb-`) — this is your `SLACK_BOT_TOKEN`

### 2f. Copy the Signing Secret

1. In the left sidebar, click **Basic Information**
2. Scroll to **App Credentials**
3. Copy the **Signing Secret** — this is your `SLACK_SIGNING_SECRET`

---

## 3. Set Up Google Workspace (Optional)

Skip this section if you do not need Google Workspace indexing. Set `GOOGLE_ENABLED=false` in your `.env`.

### 3a. Create or select a Google Cloud project

1. Go to [console.cloud.google.com](https://console.cloud.google.com)
2. Create a new project or select an existing one

### 3b. Enable APIs

In **APIs & Services > Library**, enable:
- **Google Drive API**
- **Google Docs API**
- **Google Sheets API**
- **Google Slides API**
- **Google Calendar API**

### 3c. Option A — OAuth2 (individual user access)

Best for indexing a personal or small team workspace.

1. Go to **APIs & Services > Credentials**
2. Click **Create Credentials > OAuth 2.0 Client ID**
3. Application type: **Desktop app**
4. Click **Create** and download the JSON file
5. Set `GOOGLE_CREDENTIALS_FILE` to the path of that JSON file
6. On first run, the bot prints an authorization URL. Open it in a browser, sign in with the Google account whose Drive you want to index, and grant the requested permissions. The OAuth token is saved to `GOOGLE_TOKEN_FILE` and reused on subsequent runs.

**Required OAuth scopes (configured automatically from credentials):**
- `https://www.googleapis.com/auth/drive.readonly`
- `https://www.googleapis.com/auth/documents.readonly`
- `https://www.googleapis.com/auth/spreadsheets.readonly`
- `https://www.googleapis.com/auth/presentations.readonly`
- `https://www.googleapis.com/auth/calendar.readonly`

### 3c. Option B — Service Account (domain-wide delegation)

Best for indexing an entire Google Workspace organization.

1. Go to **IAM & Admin > Service Accounts**
2. Click **Create Service Account**, give it a name, and click **Done**
3. Click the service account, go to **Keys**, click **Add Key > Create new key**, choose JSON, and download the file
4. In **Google Workspace Admin Console**, go to **Security > API Controls > Domain-wide delegation**
5. Click **Add new**, enter the service account's **Client ID**, and add these scopes:
   ```
   https://www.googleapis.com/auth/drive.readonly,
   https://www.googleapis.com/auth/documents.readonly,
   https://www.googleapis.com/auth/spreadsheets.readonly,
   https://www.googleapis.com/auth/presentations.readonly,
   https://www.googleapis.com/auth/calendar.readonly
   ```
6. Set `GOOGLE_CREDENTIALS_FILE` to the path of the service account JSON key file
7. Leave `GOOGLE_TOKEN_FILE` unset (service accounts do not use token files)

---

## 4. Create a Notion Integration (Optional)

Skip this section if you do not need Notion indexing. Set `NOTION_ENABLED=false` in your `.env`.

### 4a. Create the integration

1. Go to [notion.so/my-integrations](https://www.notion.so/my-integrations)
2. Click **New integration**
3. Name it (e.g., `xylolabs-kb`), select your workspace
4. Under **Capabilities**, enable:
   - **Read content**
5. Click **Submit**
6. Copy the **Internal Integration Token** (starts with `ntn_`) — this is your `NOTION_API_KEY`

### 4b. Share pages with the integration

For each Notion page or database you want indexed:

1. Open the page in Notion
2. Click **...** (three dots) in the top-right corner
3. Click **Connections**
4. Find and add your integration by name

You only need to share root pages — the connector recursively crawls child pages.

### 4c. Get page IDs

For each root page you share with the integration:
1. Open the page in Notion
2. Copy its URL — it looks like `https://notion.so/Workspace-Name-abc123def456...`
3. The page ID is the last segment of the URL (the hex string)

Set `NOTION_ROOT_PAGES` to a comma-separated list of these IDs.

---

## 4b. Set Up Gemini AI (Optional)

The Gemini API powers the Slack bot (question answering) and image content extraction. Without it, the bot will not respond to messages but KB ingestion still works.

### Get an API key

1. Go to [Google AI Studio](https://aistudio.google.com/apikey)
2. Click **Create API Key**
3. Select your Google Cloud project (or create one)
4. Copy the key — this is your `GEMINI_API_KEY`

### Enable App Home for DMs

To receive DMs, enable the **Messages Tab** in your Slack app:

1. Go to your Slack app settings → **App Home**
2. Under **Show Tabs**, enable **Messages Tab**
3. Check **Allow users to send Slash commands and messages from the messages tab**

---

## 5. Configure Environment

```bash
cp .env.example .env
```

Edit `.env` and fill in the values you collected in steps 2–4:

```env
# General
LOG_LEVEL=info
DB_PATH=./data/xylolabs-kb.db
ATTACHMENT_PATH=./data/attachments

# API
API_HOST=0.0.0.0
API_PORT=8080

# Gemini AI (optional — enables bot and image extraction)
GEMINI_API_KEY=your-api-key
GEMINI_MODEL=gemini-3.1-flash-lite-preview

# Knowledge Base Repo (markdown Git repo)
KB_REPO_DIR=/opt/knowledge

# Slack (required if Slack credentials are set)
SLACK_ENABLED=true
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
SLACK_SIGNING_SECRET=...
SLACK_SYNC_INTERVAL=60s   # Code default: 5m; .env.example overrides to 60s
SLACK_CHANNELS=   # leave empty to monitor all channels the bot is in

# Google (optional)
GOOGLE_ENABLED=false
GOOGLE_CREDENTIALS_FILE=./credentials.json
GOOGLE_TOKEN_FILE=./token.json
GOOGLE_SYNC_INTERVAL=5m    # Code default: 15m; .env.example overrides to 5m
GOOGLE_DRIVE_FOLDERS=  # leave empty for all accessible

# Notion (optional)
NOTION_ENABLED=false
NOTION_API_KEY=ntn_...
NOTION_SYNC_INTERVAL=5m    # Code default: 10m; .env.example overrides to 5m
NOTION_ROOT_PAGES=abc123,def456
```

> **Note:** The `SLACK_ENABLED`, `GOOGLE_ENABLED`, and `NOTION_ENABLED` variables in `.env.example` are for documentation convenience. The application auto-detects enabled connectors based on credential presence: Slack requires both `SLACK_BOT_TOKEN` and `SLACK_APP_TOKEN`, Google requires `GOOGLE_CREDENTIALS_FILE` to exist on disk, and Notion requires `NOTION_API_KEY` to be set.

---

## 6. First Run

```bash
# Create data directory
mkdir -p data/attachments

# Start the service
./bin/xylolabs-kb
```

Expected startup log output:

```
2025-12-01T09:00:00Z INFO storage opened db=./data/xylolabs-kb.db
2025-12-01T09:00:00Z INFO slack connector started
2025-12-01T09:00:00Z INFO api server listening addr=0.0.0.0:8080
2025-12-01T09:00:00Z INFO worker started sources=[slack]
2025-12-01T09:00:01Z INFO sync started source=slack
2025-12-01T09:00:15Z INFO sync complete source=slack documents_indexed=1247 duration_ms=14203
```

If you enabled Google and it is the first run with OAuth2, you will see:

```
2025-12-01T09:00:00Z INFO google auth required url=https://accounts.google.com/o/oauth2/...
```

Open that URL, complete the OAuth flow, and the service will continue automatically.

### Verify the API is up

```bash
curl http://localhost:8080/health
# {"status":"ok","time":"..."}

curl http://localhost:8080/api/v1/stats
# {"total_documents":1247,...}
```

---

## 7. Running as a System Service

### systemd (Linux)

Create `/etc/systemd/system/xylolabs-kb.service`:

```ini
[Unit]
Description=Xylolabs Knowledge Base Worker
After=network.target

[Service]
Type=simple
User=ubuntu
Group=ubuntu
WorkingDirectory=/opt/xylolabs-kb
ExecStart=/opt/xylolabs-kb/bin/xylolabs-kb
EnvironmentFile=/opt/xylolabs-kb/.env
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable xylolabs-kb
sudo systemctl start xylolabs-kb
sudo journalctl -u xylolabs-kb -f
```

### Automated Deployment

The `scripts/deploy.sh` script automates the full deployment cycle:

```bash
# Build ARM64 binaries, upload to server, restart service
./scripts/deploy.sh

# Also upload .env configuration
./scripts/deploy.sh --with-env

# Also install crontab for KB generation
./scripts/deploy.sh --with-cron
```

This script:
1. Cross-compiles `xylolabs-kb` and `kb-gen` for linux/arm64
2. Uploads binaries and scripts to the AWS server
3. Installs the systemd service file
4. Restarts the service and verifies health

### KB Generation Crontab

When deployed with `--with-cron`, the following cron jobs are installed:

```cron
# Incremental KB generation — every 6 hours (run as ubuntu to avoid permission issues)
7 */6 * * *  ubuntu  cd /opt/knowledge && /opt/xylolabs-kb/scripts/generate-kb.sh

# Weekly full rebuild — Sunday 3 AM
17 3 * * 0   ubuntu  /opt/xylolabs-kb/scripts/regenerate-kb.sh
```

### launchd (macOS)

Create `~/Library/LaunchAgents/com.xylolabs.kb.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.xylolabs.kb</string>
    <key>ProgramArguments</key>
    <array>
        <string>/opt/xylolabs-kb/bin/xylolabs-kb</string>
    </array>
    <key>WorkingDirectory</key>
    <string>/opt/xylolabs-kb</string>
    <key>EnvironmentVariables</key>
    <dict>
        <!-- add your env vars here or use KeepAlive with a wrapper script -->
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/var/log/xylolabs-kb.log</string>
    <key>StandardErrorPath</key>
    <string>/var/log/xylolabs-kb.log</string>
</dict>
</plist>
```

```bash
launchctl load ~/Library/LaunchAgents/com.xylolabs.kb.plist
```

---

## Troubleshooting

### Slack: "not_in_channel" errors during sync

The bot must be invited to each channel it indexes. In Slack, `/invite @xylolabs-kb` in each channel you want monitored. If `SLACK_CHANNELS` is set, only those channels are attempted.

### Slack: Socket Mode connection drops repeatedly

Check that `SLACK_APP_TOKEN` starts with `xapp-` (not `xoxb-`). App-level tokens are distinct from bot tokens.

### Google: "invalid_grant" on token refresh

The OAuth token has expired or been revoked. Delete `GOOGLE_TOKEN_FILE` and restart the service to re-authorize.

### Google: "insufficientPermissions"

The OAuth2 scopes or service account domain-wide delegation scopes do not include all required APIs. Review step 3 and ensure Drive, Docs, and Sheets APIs are all enabled and scoped.

### Notion: pages not being indexed

Ensure you have explicitly shared each root page with the integration (step 4b). Notion does not grant integrations access to all pages by default — each page must be connected individually.

### Notion: "Could not find page" for a page ID

Double-check the page ID from the URL. Notion page URLs end in a hyphenated title followed by a 32-character hex ID — only the hex part is the page ID.

### High disk usage

Check `ATTACHMENT_PATH` — all file attachments are stored locally. To reduce disk usage:
- Set `SLACK_CHANNELS` to index only high-value channels
- Set `GOOGLE_DRIVE_FOLDERS` to index only specific folders
- Implement a retention policy by periodically deleting old attachment files and clearing their `local_path` in the database

### Database locked errors

SQLite allows only one writer at a time. If you see `SQLITE_BUSY` or "database is locked" errors, check that only one instance of xylolabs-kb is running against the same `DB_PATH`.

### Sync falling behind

If a source has a large backlog on first run, initial sync can take many minutes. Monitor progress with:

```bash
curl http://localhost:8080/stats
curl http://localhost:8080/sync/status
```

The service continues normal operation while syncing — the API remains available throughout.
