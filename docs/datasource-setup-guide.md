# Datasource Setup Guide — xylolabs-kb

This guide walks you through obtaining every API token and credential required to connect xylolabs-kb to each supported datasource. It is written for someone who has never configured any of these services before. Follow each section end-to-end before moving to the next.

---

## Table of Contents

1. [Slack Bot Setup](#1-slack-bot-setup)
   - [1.1 Creating a Slack App](#11-creating-a-slack-app)
   - [1.2 Enabling Socket Mode](#12-enabling-socket-mode)
   - [1.3 Bot Token Scopes (OAuth & Permissions)](#13-bot-token-scopes-oauth--permissions)
   - [1.4 Event Subscriptions](#14-event-subscriptions)
   - [1.5 Installing to Workspace](#15-installing-to-workspace)
   - [1.6 Inviting the Bot to Channels](#16-inviting-the-bot-to-channels)
   - [1.7 Environment Variables Summary](#17-environment-variables-summary)
   - [1.8 Verification](#18-verification)
   - [1.9 Common Pitfalls and Troubleshooting](#19-common-pitfalls-and-troubleshooting)

2. [Google Workspace Setup](#2-google-workspace-setup)
   - [2.1 Create or Select a Google Cloud Project](#21-create-or-select-a-google-cloud-project)
   - [2.2 Enable Required APIs](#22-enable-required-apis)
   - [2.3 Option A: Service Account with Domain-Wide Delegation (Recommended)](#23-option-a-service-account-with-domain-wide-delegation-recommended)
   - [2.4 Option B: OAuth2 for Individual User Access](#24-option-b-oauth2-for-individual-user-access)
   - [2.5 Sharing Files with a Service Account (without Domain-Wide Delegation)](#25-sharing-files-with-a-service-account-without-domain-wide-delegation)
   - [2.6 Environment Variables Summary](#26-environment-variables-summary)
   - [2.7 Verification](#27-verification)
   - [2.8 Common Pitfalls and Troubleshooting](#28-common-pitfalls-and-troubleshooting)

3. [Notion Setup](#3-notion-setup)
   - [3.1 Creating a Notion Integration](#31-creating-a-notion-integration)
   - [3.2 Granting Page Access](#32-granting-page-access)
   - [3.3 Finding Page IDs](#33-finding-page-ids)
   - [3.4 API Version Compatibility](#34-api-version-compatibility)
   - [3.5 Environment Variables Summary](#35-environment-variables-summary)
   - [3.6 Verification](#36-verification)
   - [3.7 Common Pitfalls and Troubleshooting](#37-common-pitfalls-and-troubleshooting)

4. [Quick Reference — All Environment Variables](#4-quick-reference--all-environment-variables)

5. [Security Best Practices](#5-security-best-practices)

---

## 1. Slack Bot Setup

### Prerequisites

- A Slack workspace where you have **Owner** or **Admin** privileges, or explicit permission to install apps. (Member-level accounts cannot install custom apps unless the workspace allows it.)
- A browser logged in to that workspace at [slack.com](https://slack.com).
- The xylolabs-kb service running or about to be configured.

**App vs. Bot distinction:** A Slack *App* is the container — it holds settings, permissions, and credentials. A *Bot* is a workspace member created automatically when you install the app. The Bot is what joins channels and reads messages. You will interact with the App configuration UI, but the Bot is what appears in Slack to your team. You need three separate credentials: the **Bot User OAuth Token** (identifies the bot user), the **App-Level Token** (used for the real-time Socket Mode connection), and the **Signing Secret** (used to verify that incoming requests actually came from Slack).

---

### 1.1 Creating a Slack App

1. Open [https://api.slack.com/apps](https://api.slack.com/apps) in your browser. You must be signed in to the correct Slack workspace. If you manage multiple workspaces, check the workspace name shown in the top-right corner of the page and switch if needed.

2. Click the green **Create New App** button near the top right of the page.

3. A dialog titled "Create an app" appears with two options. Click **From scratch** (the left option). Do not choose "From an app manifest" unless you already have one.

4. A second dialog appears with two fields:
   - **App Name**: Enter `Xylolabs KB Bot` (or any name meaningful to your team — this name appears in Slack when the bot posts or is mentioned).
   - **Pick a workspace to develop your app in**: Click the dropdown and select your workspace from the list.

5. Click **Create App**.

6. You are now on the **Basic Information** page for your new app. This page is your main control panel. Keep this browser tab open — you will return here frequently throughout this setup.

> **Note:** The app is not yet installed in your workspace, and no credentials have been generated. The next sections walk through each required configuration step before installation.

---

### 1.2 Enabling Socket Mode

Socket Mode allows xylolabs-kb to receive real-time Slack events over a persistent WebSocket connection instead of requiring a publicly accessible HTTP endpoint. This means you can run the bot behind a firewall, on a laptop, or inside a private network without any port forwarding or domain name.

1. In the left sidebar of the app configuration page, scroll down to find **Socket Mode** and click it. (It is in the "Settings" section of the sidebar, below "Features".)

2. On the Socket Mode page, you will see a toggle labeled **Enable Socket Mode** with an off/on switch. Click the toggle to turn it **On**.

3. A dialog immediately appears asking you to create an **App-Level Token**. This is a special token that authorizes the WebSocket connection itself (separate from the bot token that reads messages).
   - In the **Token Name** field, enter a descriptive name such as `socket-mode-token` or `xylolabs-kb-socket`.
   - Under **Scopes**, click **Add Scope** and select `connections:write` from the dropdown. This is the only scope needed for Socket Mode. It allows the token to open and maintain WebSocket connections to Slack's event delivery servers.
   - Click **Generate**.

4. A green banner appears showing your new App-Level Token. It begins with `xapp-` followed by a long alphanumeric string. **Copy this token immediately** and store it somewhere safe (a password manager, a secrets vault, or directly into your `.env` file as `SLACK_APP_TOKEN`). Slack will not show you this token again after you close this dialog.

5. Click **Done** to close the dialog.

6. The Socket Mode page now shows "Socket Mode is enabled" with a green indicator.

> **Why Socket Mode?** Traditional Slack apps receive events by having Slack send HTTP POST requests to a public URL you specify. This requires your server to be reachable from the internet. Socket Mode reverses the connection: your app initiates an outbound WebSocket to Slack's servers, so no inbound firewall rules or public URLs are needed. This makes Socket Mode ideal for internal deployments and development environments.

---

### 1.3 Bot Token Scopes (OAuth & Permissions)

Scopes define exactly what data and actions the bot is authorized to perform. Slack enforces these strictly — if the bot attempts to call an API method it does not have a scope for, the call will fail with a `missing_scope` error. Request only the scopes you need (principle of least privilege).

1. In the left sidebar, click **OAuth & Permissions** (in the "Features" section).

2. Scroll down to the **Scopes** section. You will see two sub-sections: **Bot Token Scopes** and **User Token Scopes**. You only need Bot Token Scopes — do not add User Token Scopes.

3. Click **Add an OAuth Scope** under **Bot Token Scopes**. A search dropdown appears. Add each of the following scopes one at a time:

| Scope | What it does | Why xylolabs-kb needs it |
|-------|-------------|--------------------------|
| `channels:history` | Allows reading messages, files, and events in public channels | Required to fetch the message history that gets indexed into the knowledge base |
| `channels:read` | Allows listing public channels and reading their metadata (name, topic, member count) | Required to enumerate which channels exist so the bot can decide which to sync |
| `groups:history` | Allows reading messages in private channels (groups) the bot has been invited to | Required to index private channel content; without this the bot can only read public channels |
| `groups:read` | Allows listing private channels the bot belongs to | Required to enumerate private channels for sync; pairs with `groups:history` |
| `im:history` | Allows reading direct messages sent to the bot | Required if you want the bot to index direct messages it receives |
| `im:read` | Allows listing direct message conversations involving the bot | Required to enumerate DM conversations; pairs with `im:history` |
| `mpim:history` | Allows reading group direct messages (multi-party DMs) the bot is part of | Required if you want to index multi-person DM threads |
| `mpim:read` | Allows listing group direct message conversations | Required to enumerate group DMs; pairs with `mpim:history` |
| `users:read` | Allows looking up a user's display name, real name, and profile information by user ID | Required to resolve author attribution — Slack messages contain only user IDs, not names |
| `users:read.email` | Allows reading users' email addresses from their profiles | Required to populate the `author_email` field in the knowledge base for cross-source deduplication |
| `files:read` | Allows reading file metadata and downloading file content that has been shared in channels | Required to download and index file attachments (PDFs, images, documents) shared in Slack |
| `team:read` | Allows reading basic information about the workspace (team name, domain, enterprise ID) | Required to populate the `workspace` field in documents and support multi-workspace setups |

> **Minimum viable scope set:** If you only want to index public channels and do not need DMs, private channels, files, or email addresses, the minimum set is: `channels:history`, `channels:read`, `users:read`. Add the others as your requirements grow.

4. After adding all scopes, the **Bot Token Scopes** list should show all entries above. Scroll back to the top of the OAuth & Permissions page — you will see a yellow banner saying the app needs to be reinstalled. **Do not reinstall yet** — complete the Event Subscriptions setup first.

---

### 1.4 Event Subscriptions

Event Subscriptions tell Slack which real-time events to deliver to your app. In Socket Mode, events are pushed over the WebSocket connection rather than HTTP, but you still configure which event types you want to receive here.

1. In the left sidebar, click **Event Subscriptions** (in the "Features" section).

2. Toggle **Enable Events** to **On**. Because you already enabled Socket Mode, Slack will NOT ask you for a Request URL — it knows to deliver events over the WebSocket instead.

3. Scroll down to the **Subscribe to bot events** section. Click **Add Bot User Event** and add each of the following:

| Event | What triggers it | Why it is needed |
|-------|-----------------|------------------|
| `message.channels` | A new message is posted in a public channel the bot is a member of | Enables real-time ingestion of new public channel messages without waiting for the next polling cycle |
| `message.groups` | A new message is posted in a private channel the bot has been invited to | Same as above, for private channels |
| `message.im` | A new direct message is sent to the bot | Enables real-time ingestion of DMs if DM indexing is desired |
| `message.mpim` | A new message is posted in a group DM the bot is part of | Enables real-time ingestion of group DMs |
| `file_shared` | A file is shared in any channel the bot can see | Triggers immediate download and indexing of file attachments |
| `file_created` | A file is created (uploaded) in the workspace | Fires when a file is first uploaded, before it may be shared in a channel |

> **How events and polling interact:** xylolabs-kb uses a dual strategy. The Socket Mode event subscription ensures new content is ingested within seconds of being posted. The periodic sync (`SLACK_SYNC_INTERVAL`) acts as a safety net, catching anything that may have been missed (e.g., during a brief disconnection) and also backfilling historical messages from before the bot was running.

4. Click **Save Changes** at the bottom of the page.

---

### 1.5 Installing to Workspace

Now that scopes and events are configured, install the app to your workspace to generate the Bot User OAuth Token.

1. In the left sidebar, click **OAuth & Permissions**.

2. At the top of the page, click **Install to Workspace** (if this is the first installation) or **Reinstall to Workspace** (if you changed scopes after a previous installation). The button is green and prominently placed.

3. A Slack permission review page appears listing all the scopes you configured. Review them, then click **Allow**.

4. You are redirected back to the **OAuth & Permissions** page. At the top, under **OAuth Tokens for Your Workspace**, you will see:
   - **Bot User OAuth Token** — a long string beginning with `xoxb-`. Copy it.

   This is your `SLACK_BOT_TOKEN`. Store it securely. It grants the bot all the permissions you configured in the scopes section.

5. Next, get the **Signing Secret**. In the left sidebar, click **Basic Information**.

6. Scroll down to the **App Credentials** section. You will see several fields:
   - **App ID** — not needed for xylolabs-kb
   - **Client ID** — not needed
   - **Client Secret** — not needed
   - **Signing Secret** — click **Show** next to the hidden value, then copy it.

   This is your `SLACK_SIGNING_SECRET`. It is used to cryptographically verify that incoming webhook events actually originated from Slack (as opposed to a spoofed request). Even in Socket Mode, the signing secret may be used for certain verification flows.

At this point you have all three required Slack credentials:
- `SLACK_BOT_TOKEN` — starts with `xoxb-`
- `SLACK_APP_TOKEN` — starts with `xapp-` (obtained in step 1.2)
- `SLACK_SIGNING_SECRET` — a hex string (obtained above)

---

### 1.6 Inviting the Bot to Channels

The bot can only read messages from channels it has been invited to. Simply installing the app does not grant it access to any channels — each channel must be explicitly joined.

**How to invite the bot to a channel:**

1. Open Slack (the desktop or web app — not the API configuration site).

2. Navigate to the channel you want the bot to index. Click on the channel name in the left sidebar.

3. In the message input box at the bottom of the channel, type:
   ```
   /invite @Xylolabs KB Bot
   ```
   Replace `Xylolabs KB Bot` with whatever name you gave the app in step 1.1. As you type, Slack's autocomplete will suggest matching bot names — select the correct one.

4. Press Enter. Slack will confirm the bot has been invited with a message like "Xylolabs KB Bot has joined the channel."

5. Repeat for every channel you want indexed.

**Finding Channel IDs for `SLACK_CHANNELS`:**

The `SLACK_CHANNELS` environment variable accepts **channel IDs**, not channel names. Channel IDs are stable identifiers (like `C01ABC1234D`) that do not change even if the channel is renamed.

To find a channel's ID:

- **Method 1 (Slack web/desktop app):** Right-click the channel name in the left sidebar and select **Copy link** (or **View channel details** then **Copy channel ID** if your client shows that option). The link looks like `https://yourworkspace.slack.com/archives/C01ABC1234D`. The ID is the `C01...` part at the end.

- **Method 2 (API):** After configuring your bot token, call:
  ```bash
  curl -H "Authorization: Bearer xoxb-your-bot-token" \
       "https://slack.com/api/conversations.list?types=public_channel,private_channel&limit=200"
  ```
  This returns a JSON array of channels, each with an `id` field. Pipe to `jq '.channels[] | {id, name}'` for a readable list.

- **Method 3 (URL):** Open Slack in a browser. Navigate to the channel. The URL bar will show `https://app.slack.com/client/T.../C...`. The `C...` portion is the channel ID.

**`SLACK_CHANNELS` behavior:**
- If set to a comma-separated list of channel IDs (e.g., `C01ABC123,C02DEF456`), the bot only syncs those specific channels.
- If left empty, the bot syncs all channels it has been invited to.
- Setting a specific list is recommended for production to avoid accidentally indexing sensitive channels.

---

### 1.7 Environment Variables Summary

Add the following to your `.env` file:

```env
SLACK_ENABLED=true

# Bot User OAuth Token — from OAuth & Permissions page after installing to workspace
# Starts with xoxb-
SLACK_BOT_TOKEN=xoxb-your-bot-token-here

# App-Level Token — generated when enabling Socket Mode
# Starts with xapp-
SLACK_APP_TOKEN=xapp-your-app-token-here

# Signing Secret — from Basic Information > App Credentials
SLACK_SIGNING_SECRET=your-signing-secret-here

# How often to run a full sync pass (backfill / catch-up)
# Valid units: s (seconds), m (minutes), h (hours)
SLACK_SYNC_INTERVAL=60s

# Comma-separated channel IDs to monitor. Leave empty to monitor all channels the bot is in.
# Example: SLACK_CHANNELS=C01ABC1234D,C02DEF5678E
SLACK_CHANNELS=
```

---

### 1.8 Verification

**Step 1 — Verify the Bot Token is valid:**

```bash
curl -s -H "Authorization: Bearer xoxb-your-bot-token" \
     https://slack.com/api/auth.test | jq .
```

A successful response looks like:

```json
{
  "ok": true,
  "url": "https://yourworkspace.slack.com/",
  "team": "Your Workspace Name",
  "user": "xylolabs-kb-bot",
  "team_id": "T01XXXXXXX",
  "user_id": "U01XXXXXXX",
  "bot_id": "B01XXXXXXX",
  "is_enterprise_install": false
}
```

If `"ok"` is `false`, check the `"error"` field:
- `"invalid_auth"` — the token value is wrong or was not copied completely
- `"not_authed"` — the `Authorization` header is missing or malformed

**Step 2 — Verify the bot can list channels:**

```bash
curl -s -H "Authorization: Bearer xoxb-your-bot-token" \
     "https://slack.com/api/conversations.list?limit=5" | jq '.channels[].name'
```

This should return channel names. If you see `"missing_scope"` in the error, the `channels:read` scope was not added correctly — return to step 1.3.

**Step 3 — Start xylolabs-kb and check logs:**

```bash
./bin/xylolabs-kb
```

Look for these log lines:

```
INFO slack connector started
INFO sync started source=slack
INFO sync complete source=slack documents_indexed=NNN duration_ms=NNN
```

If you see `ERROR` lines mentioning Slack, the error message will indicate which credential or scope is at fault.

---

### 1.9 Common Pitfalls and Troubleshooting

**"not_in_channel" errors during sync**

The bot was not invited to a channel it is attempting to read. In the Slack app, go to the channel and type `/invite @YourBotName`. Repeat for each channel listed in `SLACK_CHANNELS`.

**Socket Mode connection drops immediately on startup**

Check that `SLACK_APP_TOKEN` starts with `xapp-` and not `xoxb-`. These are two completely different token types with different formats. Using the bot token where the app token is expected will cause the WebSocket handshake to fail.

**"missing_scope" API errors in logs**

A required scope was not added before the app was installed, or the app was not reinstalled after adding new scopes. Return to step 1.3, add the missing scope, then reinstall the app (step 1.5). The bot token is regenerated on reinstall but the value changes — update `SLACK_BOT_TOKEN` with the new value.

**Bot is in channels but messages are not appearing**

Verify that Event Subscriptions are enabled (step 1.4) and the relevant event types (`message.channels`, `message.groups`) are subscribed. Also verify Socket Mode is enabled (step 1.2). Without Socket Mode, events are delivered to an HTTP endpoint, which xylolabs-kb does not expose.

**Private channels not being indexed**

The bot must be explicitly invited to private channels — it cannot join them on its own. Also ensure `groups:history` and `groups:read` scopes are present. Public-channel-only apps are sometimes deployed without these scopes to limit access.

**"account_inactive" error**

The Slack workspace has been deactivated, or the bot was uninstalled. Reinstall the app following step 1.5.

---

## 2. Google Workspace Setup

### Prerequisites

- A **Google account** with access to [Google Cloud Console](https://console.cloud.google.com). Any Google account works for personal Drive access; for organization-wide access you need a Google Workspace administrator account.
- If using domain-wide delegation (Option A), you need access to the **Google Workspace Admin Console** at [admin.google.com](https://admin.google.com) with super-admin privileges. This is separate from Google Cloud Console.
- The files or folders you want to index must be accessible to the credential you configure (either owned by the authenticated user, or shared with the service account email).

**Service Account vs. OAuth2:**

| Method | Best for | Requires |
|--------|----------|---------|
| Service Account + Domain-Wide Delegation | Entire Google Workspace org; server-to-server; no interactive login | Google Workspace admin access for delegation setup |
| Service Account + manual sharing | Specific folders shared explicitly with the bot | No admin access needed; must share each folder manually |
| OAuth2 (personal credentials) | Individual user's Drive; development/testing | Interactive browser login on first run |

---

### 2.1 Create or Select a Google Cloud Project

All Google API credentials live inside a **Google Cloud Project**. If you already have a project for internal tooling, you can use it. Otherwise, create a new one to keep credentials isolated.

1. Go to [https://console.cloud.google.com](https://console.cloud.google.com). Sign in with the Google account you want to use (use a Google Workspace admin account if you plan to use domain-wide delegation).

2. At the top of the page, next to the Google Cloud logo, click the **project selector** dropdown (it shows the current project name or "Select a project" if none is active).

3. In the dialog that appears:
   - To use an existing project: find it in the list and click it.
   - To create a new project: click **New Project** in the top-right of the dialog. Enter a **Project name** (e.g., `xylolabs-kb`). Leave the **Organization** and **Location** fields at their defaults unless your organization requires specific placement. Click **Create**.

4. Wait a few seconds for the project to be created, then select it from the project dropdown so it becomes the active project. The project name will appear in the top bar.

---

### 2.2 Enable Required APIs

Google APIs must be explicitly enabled before your credentials can call them.

1. In the left sidebar, click **APIs & Services**, then click **Library** in the submenu. (Or go directly to [console.cloud.google.com/apis/library](https://console.cloud.google.com/apis/library).)

2. In the search box at the top, search for and enable each of the following APIs. For each one: search for it, click on it in the results, then click the blue **Enable** button.

| API | Why it is needed |
|-----|-----------------|
| **Google Drive API** | List files and folders in Drive; download raw file content; query for recently modified files |
| **Google Docs API** | Export Google Docs to plain text for full-text indexing |
| **Google Sheets API** | Export Google Sheets content to text/CSV for indexing |
| **Google Slides API** | (Optional) Export Google Slides to text if you want presentation content indexed |
| **Gmail API** | (Optional) Read Gmail messages if email indexing is desired |

3. After enabling each API, use the browser's back button to return to the API Library and search for the next one.

4. To verify all APIs are enabled: go to **APIs & Services > Enabled APIs & services**. All five should appear in the list.

---

### 2.3 Option A: Service Account with Domain-Wide Delegation (Recommended)

This method is recommended for production deployments in a Google Workspace organization. The service account acts as a non-human identity that can impersonate any user in your domain, giving the bot access to all Drive files across the organization.

**Part 1: Create the Service Account**

1. In the left sidebar of Google Cloud Console, click **IAM & Admin**, then click **Service Accounts**.

2. Click **+ Create Service Account** at the top.

3. Fill in the form:
   - **Service account name**: `xylolabs-kb` (this creates a display name; the email will be auto-generated)
   - **Service account ID**: This is auto-filled based on the name. The resulting email will look like `xylolabs-kb@your-project-id.iam.gserviceaccount.com`. Note this email — you will need it later.
   - **Service account description**: `Service account for xylolabs-kb knowledge base indexer`

4. Click **Create and Continue**.

5. On the "Grant this service account access to project" step: you do NOT need to assign any project-level IAM roles for this use case (Drive access is controlled by sharing and domain-wide delegation, not IAM). Click **Continue** without selecting any roles.

6. On the "Grant users access to this service account" step: leave blank. Click **Done**.

7. You are now on the Service Accounts list page and can see your new service account.

**Part 2: Create and Download a JSON Key**

1. Click on the service account email you just created to open its details page.

2. Click the **Keys** tab at the top of the service account detail page.

3. Click **Add Key**, then click **Create new key** in the dropdown.

4. In the dialog, select **JSON** as the key type. Click **Create**.

5. Your browser automatically downloads a file named something like `your-project-id-abc123def456.json`. This is your service account credentials file. Move it to a secure location inside the xylolabs-kb directory, for example:
   ```bash
   mv ~/Downloads/your-project-*.json /path/to/xylolabs-kb/credentials.json
   ```

6. **Immediately restrict the file permissions:**
   ```bash
   chmod 600 /path/to/xylolabs-kb/credentials.json
   ```
   This file is equivalent to a password — anyone with it can access all the data the service account can access.

7. Note the `client_id` field inside the JSON file — you will need it in the next part. You can view it with:
   ```bash
   jq .client_id /path/to/xylolabs-kb/credentials.json
   ```

**Part 3: Enable Domain-Wide Delegation on the Service Account**

1. Back on the service account detail page (click the service account in the list), click the **Details** tab.

2. Scroll down to find **Domain-wide delegation**. Click **Edit** (the pencil icon).

3. Check the checkbox labeled **Enable Google Workspace Domain-wide Delegation**.

4. In the **Product name for the consent screen** field, enter `Xylolabs KB Bot`.

5. Click **Save**.

6. You will now see a **View Client ID** link or the Client ID displayed directly. Note this number — it is a long numeric string like `123456789012345678901`.

**Part 4: Authorize the Service Account in Google Workspace Admin Console**

This step requires Google Workspace super-admin access. This is the step that allows the service account to impersonate users in your domain.

1. Open a new browser tab and go to [https://admin.google.com](https://admin.google.com). Sign in with your Google Workspace super-admin account (this may be a different account than the one you used for Google Cloud Console).

2. In the Admin Console, click **Security** in the left sidebar. If you do not see it, click **Show more** to expand the full menu.

3. Click **Access and data control**, then click **API controls**.

4. Scroll down to the **Domain wide delegation** section and click **Manage Domain Wide Delegation**.

5. Click **Add new** at the top of the delegation list.

6. A dialog appears with two fields:
   - **Client ID**: Enter the numeric Client ID you noted in Part 3 (from the service account's Details tab). It is a long number like `123456789012345678901`.
   - **OAuth Scopes (comma-delimited)**: Enter all of the following scopes on one line, separated by commas with no spaces:

```
https://www.googleapis.com/auth/drive.readonly,https://www.googleapis.com/auth/documents.readonly,https://www.googleapis.com/auth/spreadsheets.readonly
```

If you also want Gmail and Slides:
```
https://www.googleapis.com/auth/drive.readonly,https://www.googleapis.com/auth/documents.readonly,https://www.googleapis.com/auth/spreadsheets.readonly,https://www.googleapis.com/auth/gmail.readonly,https://www.googleapis.com/auth/presentations.readonly
```

The meaning of each scope:

| Scope | What it allows |
|-------|---------------|
| `drive.readonly` | List all files in Drive, read file metadata, download file content. This is the primary scope for Drive indexing. |
| `documents.readonly` | Export and read the textual content of Google Docs. Without this, only the filename is accessible, not the content. |
| `spreadsheets.readonly` | Read the cell values and structure of Google Sheets. |
| `gmail.readonly` | Read email messages, threads, labels, and attachments. Only include this if you intend to index email. |
| `presentations.readonly` | Read the slide content of Google Slides presentations. |

7. Click **Authorize**.

8. The delegation entry appears in the list. Domain-wide delegation can take up to 10 minutes to propagate across Google's systems.

**Part 5: Configure the Impersonation Subject**

When using domain-wide delegation, the service account impersonates a specific user whose data it accesses. Typically this is a shared service account or admin email in your org (e.g., `kb-bot@yourcompany.com`). This user must have access to the files you want indexed.

The xylolabs-kb connector reads the `GOOGLE_CREDENTIALS_FILE` and uses the service account directly. If your implementation requires specifying a `subject` (impersonation email), configure it as needed in the connector. Refer to the Google connector source in `internal/google/` for the exact configuration pattern used.

---

### 2.4 Option B: OAuth2 for Individual User Access

Use this option when you want to index the Drive files of a specific user account, not the entire organization. This is simpler to set up (no Admin Console access needed) but only indexes what that one user can see.

**Part 1: Configure the OAuth Consent Screen**

Before creating OAuth credentials, you must configure the OAuth consent screen that users see when authorizing the app.

1. In Google Cloud Console, go to **APIs & Services > OAuth consent screen** in the left sidebar.

2. Choose the **User Type**:
   - **Internal**: Only users within your Google Workspace organization can authorize. Choose this if the account granting access is in your org. Requires a Google Workspace account.
   - **External**: Any Google account can authorize. Choose this for personal Gmail accounts or testing.

3. Click **Create**.

4. Fill in the required fields on the "App information" page:
   - **App name**: `Xylolabs KB Bot`
   - **User support email**: Your email address
   - **Developer contact information**: Your email address

5. Click **Save and Continue**.

6. On the "Scopes" page, click **Add or Remove Scopes**. In the filter box, search for and add each of the following:
   - `https://www.googleapis.com/auth/drive.readonly`
   - `https://www.googleapis.com/auth/documents.readonly`
   - `https://www.googleapis.com/auth/spreadsheets.readonly`

   (Add `gmail.readonly` if needed.)

7. Click **Update**, then **Save and Continue**.

8. On the "Test users" page (shown only for External apps): click **+ Add Users** and add your own Google account email. This allows your account to complete the OAuth flow even though the app is not yet published. Click **Save and Continue**.

9. Click **Back to Dashboard**.

**Part 2: Create OAuth2 Client Credentials**

1. In the left sidebar, click **APIs & Services > Credentials**.

2. Click **+ Create Credentials** at the top, then select **OAuth client ID** from the dropdown.

3. In the "Application type" dropdown, select **Desktop app**.

4. In the **Name** field, enter `xylolabs-kb-desktop`.

5. Click **Create**.

6. A dialog shows your new **Client ID** and **Client Secret**. Click **Download JSON** to download the credentials file. (You can also close this dialog and re-download later from the Credentials page by clicking the download icon next to the credential.)

7. Save the downloaded file as `credentials.json` in the xylolabs-kb directory:
   ```bash
   mv ~/Downloads/client_secret_*.json /path/to/xylolabs-kb/credentials.json
   chmod 600 /path/to/xylolabs-kb/credentials.json
   ```

**Part 3: Complete the OAuth Flow on First Run**

1. Start xylolabs-kb with `GOOGLE_ENABLED=true` and `GOOGLE_CREDENTIALS_FILE` pointing to your credentials file.

2. On first startup, the Google connector will print a URL to the console:
   ```
   INFO google auth required url=https://accounts.google.com/o/oauth2/auth?client_id=...
   ```

3. Open that URL in a browser. Sign in with the Google account whose Drive you want to index.

4. Click through the consent screen, reviewing the permissions. Click **Allow**.

5. Your browser will attempt to redirect to `localhost` (this redirect will fail in the browser — that is expected for Desktop app credentials). Copy the full URL from the browser's address bar. It looks like:
   ```
   http://localhost/?code=4/0AbCdEfGh...&scope=...
   ```

6. Paste just the `code` value back into the terminal if the bot is waiting for it interactively, or set up the token exchange as described in the `internal/google/` connector source.

7. The token is saved to `GOOGLE_TOKEN_FILE` (default: `./token.json`) and reused on all subsequent runs. The token auto-refreshes using the refresh token stored in this file.

---

### 2.5 Sharing Files with a Service Account (without Domain-Wide Delegation)

If you do not have admin access to set up domain-wide delegation but still want to use a service account, you can manually share specific Drive folders with the service account's email address.

1. Find the service account's email address. It looks like `xylolabs-kb@your-project-id.iam.gserviceaccount.com`. You can see it on the Service Accounts page in Google Cloud Console, or in the `client_email` field of the downloaded JSON key.

2. Open Google Drive in a browser ([drive.google.com](https://drive.google.com)).

3. Right-click the folder you want to share with the bot.

4. Click **Share**.

5. In the "Share with people and groups" dialog, paste the service account email address into the search field.

6. Set the permission level to **Viewer** (read-only — never give Editor or Owner access unless needed).

7. Uncheck "Notify people" (the service account cannot receive email notifications).

8. Click **Share**.

9. The service account can now read all files in that folder and its subfolders.

10. Repeat for each folder you want indexed.

> **Limitation:** This approach requires manual re-sharing whenever new top-level folders are created. Domain-wide delegation is preferred for organization-wide deployments.

---

### 2.6 Environment Variables Summary

```env
GOOGLE_ENABLED=true

# Path to your credentials file:
# - For service account: the downloaded JSON key file
# - For OAuth2: the downloaded client_secret_*.json file
GOOGLE_CREDENTIALS_FILE=./credentials.json

# Path where OAuth2 token is cached (after the interactive auth flow).
# Not used for service accounts.
GOOGLE_TOKEN_FILE=./token.json

# How often to poll Google Drive for new or modified files
# Valid units: s (seconds), m (minutes), h (hours)
GOOGLE_SYNC_INTERVAL=5m

# Comma-separated Drive folder IDs to index.
# Leave empty to index all files accessible to the credential.
# Find folder IDs from the URL when viewing the folder in Drive:
#   https://drive.google.com/drive/folders/1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms
#   Folder ID: 1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms
GOOGLE_DRIVE_FOLDERS=
```

---

### 2.7 Verification

**Step 1 — Verify service account credentials are valid:**

```bash
# Install the Google Cloud CLI if not already installed, then:
gcloud auth activate-service-account \
    --key-file=/path/to/xylolabs-kb/credentials.json

gcloud auth list
# Should show your service account as active
```

Or use curl with a service account token (more complex; the Go client library handles this automatically).

**Step 2 — Verify the bot can list Drive files:**

Start xylolabs-kb and check the logs for the Google connector:

```
INFO google connector started
INFO sync started source=google
INFO sync complete source=google documents_indexed=NNN duration_ms=NNN
```

If you see errors, common messages and their meanings are in the troubleshooting section below.

**Step 3 — Verify via the API:**

```bash
curl "http://localhost:8080/search?q=test&source=google&limit=1"
```

If documents appear with `"source": "google"`, the connector is working.

```bash
curl http://localhost:8080/stats
# Check documents_by_source.google is greater than 0
```

---

### 2.8 Common Pitfalls and Troubleshooting

**"Error 403: The caller does not have permission"**

Possible causes:
- The required API (Drive, Docs, or Sheets) is not enabled in Google Cloud Console. Return to step 2.2 and enable all required APIs.
- For service accounts: domain-wide delegation is not yet propagated (can take up to 10 minutes after setup in Admin Console).
- For OAuth2: the user who completed the consent flow does not have access to the file or folder being requested.
- The service account was not shared on the specific file/folder (if not using domain-wide delegation).

**"Error 401: Invalid Credentials" or "invalid_grant"**

For OAuth2: the token file (`GOOGLE_TOKEN_FILE`) is stale, corrupted, or the refresh token has been revoked. Delete the token file and restart the bot to trigger a new OAuth flow:
```bash
rm ./token.json
./bin/xylolabs-kb
```

For service accounts: the JSON key file has been deleted from Google Cloud Console (keys can be deleted there), or the file path in `GOOGLE_CREDENTIALS_FILE` is wrong.

**"Error 404: File not found"**

The file ID stored in the bot's database no longer exists in Drive (it was deleted or moved to Trash). The bot will skip the file on next sync.

**"insufficientPermissions" when accessing Docs content**

The `documents.readonly` scope is not in the domain-wide delegation scope list, or the OAuth2 credentials do not include it. Return to step 2.3 Part 4 or step 2.4 Part 1 and add the scope. For OAuth2 you will also need to re-run the consent flow after adding the scope.

**OAuth2 consent screen shows "This app isn't verified"**

For External user type apps, Google shows a warning screen. Click **Advanced** (small link at the bottom), then **Go to Xylolabs KB Bot (unsafe)**. This warning only affects unverified apps — for internal use you can safely proceed. To remove the warning permanently, you would need to go through Google's app verification process, which is only needed for public-facing applications.

**Service account impersonation fails with "Client is unauthorized to retrieve access tokens using this method"**

Domain-wide delegation is not enabled on the service account, or the Client ID used in the Admin Console does not match the service account's actual Client ID. Verify by re-checking the Client ID in the service account's Details tab in Google Cloud Console, and compare it to what is entered in the Admin Console delegation entry.

---

## 3. Notion Setup

### Prerequisites

- A Notion workspace where you have **Owner** or **Workspace Member** access.
- The pages you want indexed must be accessible to you (you can view them in Notion).
- A Notion account. Note: integrations are per-workspace, not per-user.

---

### 3.1 Creating a Notion Integration

Notion integrations are **internal** (private to your workspace) and use a single long-lived API key. There is no OAuth flow or public app approval process — the token is generated immediately.

1. Open [https://www.notion.so/my-integrations](https://www.notion.so/my-integrations) in your browser. Sign in if prompted.

2. Click **+ New integration** at the top of the page.

3. You are on the integration creation form. Fill in the following fields:

   - **Name**: `Xylolabs KB` (this name will appear to workspace members when you add the integration to a page, so make it recognizable).
   - **Logo**: Optional — you can upload an icon to make the integration visually identifiable.
   - **Associated workspace**: Click the dropdown and select the Notion workspace you want to index. If you manage multiple workspaces, make sure this is the correct one.

4. Under **Capabilities**, select:
   - **Read content** — checked (required to read page content)
   - **Read comments** — checked (optional; include if you want page comments indexed)
   - **Read user information including email addresses** — checked (optional; include for author attribution with email)
   - **Insert content** — leave unchecked (the bot only reads, never writes)
   - **Update content** — leave unchecked
   - **No user information** / **No email addresses** — leave unchecked if you want author info

   The minimum required capability is **Read content**. The others are optional but recommended for richer metadata.

5. Click **Submit** at the bottom.

6. You are taken to the integration's configuration page. At the top under **Secrets**, you will see the **Internal Integration Token**. Click **Show** to reveal it, then click the copy icon.

   The token begins with `ntn_` followed by a long alphanumeric string (the newer token format). Older integrations may use a token beginning with `secret_` — both are valid.

   This is your `NOTION_API_KEY`. Store it securely.

> **Token behavior:** This token does not expire on its own. However, if you delete the integration from the Notion settings, the token is immediately revoked. You can also regenerate the token at any time from this page (click **Regenerate** next to the token), which invalidates the old token immediately. Update `NOTION_API_KEY` in your `.env` any time you regenerate.

---

### 3.2 Granting Page Access

This is the most critical and most commonly overlooked step. **Notion integrations have zero access to any page by default.** You must explicitly connect the integration to each page you want it to access. Connecting a parent page automatically grants access to all child pages.

**Why this design?** Notion's security model is intentional: integrations cannot silently access all workspace content. Each page owner must consciously grant access, preventing integrations from reading confidential pages they were not explicitly given permission to see.

**How to connect a page:**

1. Open Notion (browser or desktop app).

2. Navigate to the page you want the integration to access. Click on it in the left sidebar to open it.

3. In the top-right corner of the page, click the **...** button (three horizontal dots, also called the "More" menu). In newer Notion versions this may appear as a settings icon or be accessible via the top-right menu.

4. In the dropdown menu that appears, click **Connections** (in some Notion versions this is labeled "Add connections" or found under "Settings").

5. A search box appears labeled "Search for connections". Type the name of your integration (e.g., `Xylolabs KB`).

6. Click on your integration in the results. Notion will ask you to confirm: "Xylolabs KB will be able to read content from this page and subpages." Click **Confirm**.

7. A confirmation message appears briefly. The integration now has read access to this page and recursively to all pages and databases nested under it.

8. Repeat for each root-level page or database you want indexed. You do not need to connect every individual page — connecting a parent page covers all its children.

**Strategy for broad access:** To grant access to most of your workspace content, connect the integration to your top-level pages (the pages that appear directly in your sidebar without indentation). This will recursively grant access to everything nested beneath them.

**Strategy for selective access:** Connect only specific sections — for example, only the "Engineering" and "Product" top-level pages, excluding "HR" and "Finance".

---

### 3.3 Finding Page IDs

The `NOTION_ROOT_PAGES` environment variable requires the **page IDs** of the root pages you have shared with the integration. A page ID is a 32-character hexadecimal string that uniquely identifies a page in Notion.

**Method 1: From the browser URL**

1. Open a page in Notion in your web browser.

2. Look at the URL in the address bar. It will look like one of these formats:
   - `https://www.notion.so/Your-Workspace/Page-Title-abc123def456abc123def456abc123de`
   - `https://www.notion.so/abc123def456abc123def456abc123de`
   - `https://www.notion.so/yourworkspace/Page-Title-abc123def456abc123def456abc123de`

3. The page ID is the **last 32 characters** of the URL, potentially preceded by a hyphen and a title prefix. In the example above: `abc123def456abc123def456abc123de`.

4. The ID may or may not have hyphens in the URL. Notion's API accepts both formats:
   - With hyphens: `abc123de-f456-abc1-23de-f456abc123de`
   - Without hyphens: `abc123def456abc123def456abc123de`

   Both work in `NOTION_ROOT_PAGES`.

**Method 2: From the Notion desktop app**

1. Right-click on a page in the sidebar.

2. Select **Copy link** from the context menu.

3. Paste the link somewhere and extract the 32-character hex string from the end of the URL.

**Method 3: From the API**

After configuring your API key, you can search for pages using the API:

```bash
curl -s \
     -H "Authorization: Bearer ntn_your-api-key" \
     -H "Notion-Version: 2022-06-28" \
     -H "Content-Type: application/json" \
     -d '{"query": "Your Page Title"}' \
     https://api.notion.com/v1/search | jq '.results[] | {id: .id, title: (.properties.title.title[0].plain_text // .title[0].plain_text // "untitled")}'
```

This returns page IDs alongside their titles, making it easy to identify the correct pages.

---

### 3.4 API Version Compatibility

xylolabs-kb uses the Notion API version **`2022-06-28`**. This version string must be sent in the `Notion-Version` header on every API request.

Notion uses date-based versioning. Each version introduces changes to the API response format; specifying a version ensures the connector receives a predictable response structure regardless of what Notion deploys in the future.

If you are building on top of xylolabs-kb or writing custom scripts to interact with the Notion API, always include `Notion-Version: 2022-06-28` in your headers to ensure compatibility with the data structures the connector expects.

---

### 3.5 Environment Variables Summary

```env
NOTION_ENABLED=true

# Internal Integration Token — from notion.so/my-integrations
# Starts with ntn_ (newer) or secret_ (older)
NOTION_API_KEY=ntn_AbCdEfGhIjKlMnOpQrStUvWxYzAbCdEfGhIjKlMnOpQr

# How often to poll Notion for new or updated pages
# Valid units: s (seconds), m (minutes), h (hours)
NOTION_SYNC_INTERVAL=5m

# Comma-separated root page IDs to index.
# The connector recursively indexes all child pages and databases under each root.
# Example:
#   NOTION_ROOT_PAGES=abc123def456abc123def456abc123de,fedcba987654fedcba987654fedcba98
NOTION_ROOT_PAGES=
```

---

### 3.6 Verification

**Step 1 — Verify the API key is valid:**

```bash
curl -s \
     -H "Authorization: Bearer ntn_your-api-key" \
     -H "Notion-Version: 2022-06-28" \
     https://api.notion.com/v1/users/me | jq .
```

A successful response:

```json
{
  "object": "user",
  "id": "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
  "name": "Xylolabs KB",
  "avatar_url": null,
  "type": "bot",
  "bot": {
    "owner": {
      "type": "workspace",
      "workspace": true
    },
    "workspace_name": "Your Workspace Name"
  }
}
```

If you see `{"object":"error","status":401,"code":"unauthorized","message":"..."}`, the API key is wrong. Check that you copied the full token including the `ntn_` prefix.

**Step 2 — Verify page access:**

```bash
# Replace PAGE_ID with one of your root page IDs
curl -s \
     -H "Authorization: Bearer ntn_your-api-key" \
     -H "Notion-Version: 2022-06-28" \
     https://api.notion.com/v1/pages/YOUR-PAGE-ID | jq '{id: .id, title: (.properties.title.title[0].plain_text // "check .title field")}'
```

If you see `{"object":"error","status":404}`, the page either does not exist at that ID or the integration has not been connected to it. Return to step 3.2 and connect the integration to that page.

**Step 3 — Verify in xylolabs-kb:**

Start the service and check the logs:

```
INFO notion connector started
INFO sync started source=notion
INFO sync complete source=notion documents_indexed=NNN duration_ms=NNN
```

Then verify via the search API:

```bash
curl "http://localhost:8080/search?q=test&source=notion&limit=1"
curl http://localhost:8080/stats
# Check documents_by_source.notion is greater than 0
```

---

### 3.7 Common Pitfalls and Troubleshooting

**Pages not being indexed / "Could not find page" error**

The most common cause: the integration was not connected to the page. Every page must be explicitly connected via the "..." menu → Connections → [your integration]. Notion does not grant blanket workspace access. Return to step 3.2 and connect the integration to each root page.

**Page ID is wrong**

Notion page URLs contain the page title (hyphenated) before the ID. For example: `https://www.notion.so/Your-Page-Title-abc123def456abc123def456abc123de`. Only the last 32 characters (`abc123def456abc123def456abc123de`) are the ID — everything before and including the last hyphen is the title. Do not include hyphens unless you are using the UUID format (`abc123de-f456-abc1-23de-f456abc123de`).

**"API token is invalid" (401 error)**

The token was not copied fully, or the integration was deleted from the workspace. Go back to [notion.so/my-integrations](https://www.notion.so/my-integrations) and copy the token again. If the integration was deleted, create a new one and reconnect it to all pages.

**"Insufficient permissions" (403 error)**

The integration does not have the capability required for the operation. For example, if you enabled only "Read content" but not "Read user information", user lookup calls will fail. Return to [notion.so/my-integrations](https://www.notion.so/my-integrations), click on your integration, and enable the required capabilities under the "Capabilities" section.

**Databases not being indexed**

Databases are a special Notion content type. They must also be connected to the integration the same way as pages. In Notion, navigate to the database view, click **...** at the top right, go to **Connections**, and add the integration.

**Child pages are accessible but content is empty**

Some Notion page types (Synced Blocks, Linked Databases) may not export content via the API. This is a Notion API limitation — the content is stored in the source page, not the synced copy. Index the original source page instead.

**Rate limiting (HTTP 429)**

The Notion API has a rate limit of approximately 3 requests per second per integration. If you are indexing a very large workspace (thousands of pages) on the first sync, you may encounter 429 errors. xylolabs-kb handles these with automatic retry/backoff. The initial sync will simply take longer. Subsequent incremental syncs are much faster.

---

## 4. Quick Reference — All Environment Variables

| Variable | Required | Description | Example Value | Where to Obtain |
|----------|----------|-------------|---------------|-----------------|
| `LOG_LEVEL` | No | Log verbosity | `info` | — (choose: debug/info/warn/error) |
| `DB_PATH` | No | SQLite database file path | `./data/xylolabs-kb.db` | — (choose a path) |
| `ATTACHMENT_PATH` | No | Directory for downloaded attachments | `./data/attachments` | — (choose a path) |
| `API_HOST` | No | HTTP server bind address | `0.0.0.0` | — |
| `API_PORT` | No | HTTP server port | `8080` | — |
| `SLACK_ENABLED` | No | Enable Slack connector | `true` | — |
| `SLACK_BOT_TOKEN` | Yes (if Slack enabled) | Bot User OAuth Token | `xoxb-...` | api.slack.com/apps → OAuth & Permissions → Bot User OAuth Token |
| `SLACK_APP_TOKEN` | Yes (if Slack enabled) | App-Level Token for Socket Mode | `xapp-...` | api.slack.com/apps → Socket Mode → App-Level Token |
| `SLACK_SIGNING_SECRET` | Yes (if Slack enabled) | Request signing secret | 32-char hex | api.slack.com/apps → Basic Information → App Credentials → Signing Secret |
| `SLACK_SYNC_INTERVAL` | No | Polling interval for backfill | `60s` | — (e.g., 30s, 2m, 1h) |
| `SLACK_CHANNELS` | No | Channel IDs to monitor (empty = all) | `C01ABC123,C02DEF456` | Channel URL or `/invite` flow |
| `GOOGLE_ENABLED` | No | Enable Google Workspace connector | `false` | — |
| `GOOGLE_CREDENTIALS_FILE` | Yes (if Google enabled) | Path to credentials JSON | `./credentials.json` | Google Cloud Console → Credentials → Download |
| `GOOGLE_TOKEN_FILE` | No | OAuth2 token cache path | `./token.json` | — (auto-created on first OAuth flow) |
| `GOOGLE_SYNC_INTERVAL` | No | Polling interval for Drive changes | `5m` | — |
| `GOOGLE_DRIVE_FOLDERS` | No | Folder IDs to index (empty = all) | `1BxiMV...` | Drive folder URL: last segment |
| `NOTION_ENABLED` | No | Enable Notion connector | `false` | — |
| `NOTION_API_KEY` | Yes (if Notion enabled) | Internal Integration Token | `ntn_...` | notion.so/my-integrations → [integration] → Internal Integration Token |
| `NOTION_SYNC_INTERVAL` | No | Polling interval for page changes | `5m` | — |
| `NOTION_ROOT_PAGES` | Yes (if Notion enabled) | Root page IDs to crawl | `abc123...,def456...` | Notion page URL → last 32-char hex segment |

---

## 5. Security Best Practices

### Never commit tokens to version control

All tokens (`SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN`, `NOTION_API_KEY`, `credentials.json`, `token.json`) must never be committed to git. The `.gitignore` in xylolabs-kb already excludes `.env` and common credential files, but always verify:

```bash
git status
# If .env or credentials.json appears, do NOT git add them
```

If you accidentally commit a secret, **rotate it immediately** (generate a new token and revoke the old one) — do not rely on git history rewriting to protect it, as the secret may already be in remote logs.

### Use `.env` files properly

The `.env` file is for local development. For production:
- Use a secrets manager such as **HashiCorp Vault**, **AWS Secrets Manager**, **GCP Secret Manager**, or **Azure Key Vault** to inject environment variables at runtime.
- For Docker deployments, use Docker secrets or your orchestrator's secret management (Kubernetes Secrets, Docker Swarm secrets).
- For systemd services, use `EnvironmentFile` pointing to a file with restricted permissions (`chmod 600`).

### Principle of least privilege

- **Slack**: Only add the scopes you actually use. If you do not need to index DMs, do not add `im:history` and `im:read`. If you do not need file attachments, omit `files:read`.
- **Google**: Request only `drive.readonly`, `documents.readonly`, and `spreadsheets.readonly`. Add `gmail.readonly` only if email indexing is required. Use folder-level sharing (`GOOGLE_DRIVE_FOLDERS`) rather than organization-wide access when possible.
- **Notion**: Enable only "Read content". Add "Read user information" only if you need author attribution with email addresses.

### Rotate tokens periodically

| Token | Recommended rotation | How to rotate |
|-------|---------------------|---------------|
| `SLACK_BOT_TOKEN` | Annually or on team member departure | Reinstall the Slack app; a new token is generated |
| `SLACK_APP_TOKEN` | Annually | Delete the App-Level Token in Socket Mode settings and create a new one |
| `SLACK_SIGNING_SECRET` | If compromised | Rotate from Basic Information → App Credentials → Rotate |
| `NOTION_API_KEY` | Annually | Click "Regenerate" in the integration settings |
| Google service account key | Every 90 days | Create a new key in Cloud Console; delete the old one |
| Google OAuth2 token | Auto-refreshes | Manually revoke at [myaccount.google.com/permissions](https://myaccount.google.com/permissions) if needed |

### Audit access logs

- **Slack**: Workspace owners can view app activity in the Slack Admin dashboard under **Apps** → your app.
- **Google**: Go to Google Cloud Console → **APIs & Services** → **Credentials** to see last-used timestamps. Enable **Cloud Audit Logs** for detailed access logging.
- **Notion**: Go to [notion.so/my-integrations](https://www.notion.so/my-integrations) → your integration → **Activity** (if available) to see recent API calls.

### Secure the credentials files

```bash
# Service account JSON key
chmod 600 /path/to/xylolabs-kb/credentials.json

# OAuth2 token cache
chmod 600 /path/to/xylolabs-kb/token.json

# Environment file
chmod 600 /path/to/xylolabs-kb/.env
```

Do not run xylolabs-kb as root. Use a dedicated system user with access only to the data directory and credential files.

### Production secrets management

For production deployments, avoid using a `.env` file and instead inject secrets directly as environment variables from your secrets manager:

```bash
# Example: AWS Secrets Manager + systemd
ExecStartPre=/usr/local/bin/fetch-secrets.sh
# Where fetch-secrets.sh retrieves secrets and exports them

# Or use a tool like chamber, envconsul, or bank-vaults
# that injects secrets from your vault at process start time
```

The key property to enforce: **secrets are never written to disk in plaintext in production**.
