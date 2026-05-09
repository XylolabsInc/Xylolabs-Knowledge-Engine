#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# deploy.sh — Deploy xylolabs-kb to AWS server
#
# Builds the Go binary for ARM64, uploads to server, restarts the service,
# and verifies health.
# =============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Server config
SERVER_HOST="${SERVER_HOST:-brain.internal.xylolabs.com}"
SERVER_USER="${SERVER_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/xylolabs-bots.pem}"
REMOTE_DIR="/opt/xylolabs-kb"
REMOTE_KB_DIR="/opt/knowledge"

# Build config
BINARY_NAME="xylolabs-kb"
BUILD_OUTPUT="$PROJECT_DIR/bin/${BINARY_NAME}-linux-arm64"

LOG_PREFIX="[deploy]"
log() { echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') $LOG_PREFIX $*"; }
die() { log "FATAL: $*"; exit 1; }

ssh_cmd() {
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "${SERVER_USER}@${SERVER_HOST}" "$@"
}

scp_cmd() {
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no "$@"
}

# -----------------------------------------------------------------------------
# Build
# -----------------------------------------------------------------------------
build() {
    log "Building for linux/arm64..."
    cd "$PROJECT_DIR"
    GOOS=linux GOARCH=arm64 go build -o "$BUILD_OUTPUT" ./cmd/xylolabs-kb/
    log "Built: $BUILD_OUTPUT ($(du -h "$BUILD_OUTPUT" | cut -f1))"

    log "Building kb-gen for linux/arm64..."
    GOOS=linux GOARCH=arm64 go build -o "$PROJECT_DIR/bin/kb-gen-linux-arm64" ./cmd/kb-gen/
    log "Built: kb-gen-linux-arm64 ($(du -h "$PROJECT_DIR/bin/kb-gen-linux-arm64" | cut -f1))"
}

# -----------------------------------------------------------------------------
# Upload
# -----------------------------------------------------------------------------
upload() {
    log "Uploading binary and scripts to $SERVER_HOST..."

    # Ensure remote directories exist
    ssh_cmd "sudo mkdir -p $REMOTE_DIR/bin $REMOTE_DIR/scripts $REMOTE_DIR/configs"

    # Upload binaries
    scp_cmd "$BUILD_OUTPUT" "${SERVER_USER}@${SERVER_HOST}:/tmp/${BINARY_NAME}"
    ssh_cmd "sudo mv /tmp/${BINARY_NAME} $REMOTE_DIR/bin/${BINARY_NAME} && sudo chmod +x $REMOTE_DIR/bin/${BINARY_NAME}"

    scp_cmd "$PROJECT_DIR/bin/kb-gen-linux-arm64" "${SERVER_USER}@${SERVER_HOST}:/tmp/kb-gen"
    ssh_cmd "sudo mv /tmp/kb-gen $REMOTE_DIR/bin/kb-gen && sudo chmod +x $REMOTE_DIR/bin/kb-gen"

    # Upload scripts
    scp_cmd "$PROJECT_DIR/scripts/generate-kb.sh" "${SERVER_USER}@${SERVER_HOST}:/tmp/generate-kb.sh"
    scp_cmd "$PROJECT_DIR/scripts/regenerate-kb.sh" "${SERVER_USER}@${SERVER_HOST}:/tmp/regenerate-kb.sh"
    ssh_cmd "sudo mv /tmp/generate-kb.sh /tmp/regenerate-kb.sh $REMOTE_DIR/scripts/ && sudo chmod +x $REMOTE_DIR/scripts/*.sh"

    # Upload systemd service file
    scp_cmd "$PROJECT_DIR/configs/xylolabs-kb.service" "${SERVER_USER}@${SERVER_HOST}:/tmp/xylolabs-kb.service"
    ssh_cmd "sudo mv /tmp/xylolabs-kb.service /etc/systemd/system/xylolabs-kb.service"

    log "Upload complete"
}

# -----------------------------------------------------------------------------
# Upload .env (only if it exists locally and --with-env flag is passed)
# -----------------------------------------------------------------------------
upload_env() {
    if [ -f "$PROJECT_DIR/.env" ]; then
        # Validate critical env vars before uploading
        local missing=()
        for var in KB_REPO_DIR GEMINI_API_KEY SLACK_BOT_TOKEN; do
            if ! grep -q "^${var}=" "$PROJECT_DIR/.env"; then
                missing+=("$var")
            fi
        done
        if [ ${#missing[@]} -gt 0 ]; then
            log "WARNING: .env is missing critical variables: ${missing[*]}"
            log "The bot may not function correctly without these."
        fi

        log "Uploading .env..."
        # Copy .env to temp, fix SYSTEM_PROMPT_FILE path for server
        local tmpenv
        tmpenv=$(mktemp)
        sed 's|^SYSTEM_PROMPT_FILE=.*|SYSTEM_PROMPT_FILE=/opt/xylolabs-kb/system-prompt.txt|' "$PROJECT_DIR/.env" > "$tmpenv"
        scp_cmd "$tmpenv" "${SERVER_USER}@${SERVER_HOST}:/tmp/xylolabs-kb.env"
        ssh_cmd "sudo mv /tmp/xylolabs-kb.env $REMOTE_DIR/.env && sudo chmod 600 $REMOTE_DIR/.env"
        rm -f "$tmpenv"
    else
        log "No .env file found, skipping (ensure $REMOTE_DIR/.env exists on server)"
    fi
}

# -----------------------------------------------------------------------------
# Upload system prompt file
# -----------------------------------------------------------------------------
upload_system_prompt() {
    local prompt_file="$PROJECT_DIR/system-prompt.txt"
    if [ ! -f "$prompt_file" ]; then
        # Fall back to the example
        prompt_file="$PROJECT_DIR/system-prompt-example.txt"
    fi

    if [ ! -f "$prompt_file" ]; then
        log "WARNING: No system prompt file found, skipping"
        return
    fi

    log "Uploading system prompt from $prompt_file..."
    scp_cmd "$prompt_file" "${SERVER_USER}@${SERVER_HOST}:/tmp/system-prompt.txt"
    ssh_cmd "sudo mv /tmp/system-prompt.txt $REMOTE_DIR/system-prompt.txt && sudo chmod 644 $REMOTE_DIR/system-prompt.txt"
    log "System prompt uploaded"
}

# -----------------------------------------------------------------------------
# Restart service
# -----------------------------------------------------------------------------
restart() {
    log "Restarting xylolabs-kb service..."
    ssh_cmd "sudo systemctl daemon-reload && sudo systemctl enable xylolabs-kb && sudo systemctl restart xylolabs-kb"
    sleep 3
    local status
    status=$(ssh_cmd "systemctl is-active xylolabs-kb" 2>/dev/null || echo "unknown")
    if [ "$status" != "active" ]; then
        log "WARNING: Service status is '$status'"
        ssh_cmd "sudo journalctl -u xylolabs-kb --no-pager -n 20"
        die "Service failed to start"
    fi
    log "Service is active"
}

# -----------------------------------------------------------------------------
# Health check
# -----------------------------------------------------------------------------
verify_health() {
    log "Verifying API health..."
    local retries=5
    for i in $(seq 1 $retries); do
        local status
        status=$(ssh_cmd "curl -sf -o /dev/null -w '%{http_code}' http://localhost:8080/health" 2>/dev/null || echo "000")
        if [ "$status" = "200" ]; then
            log "Health check passed"
            return 0
        fi
        log "Health check attempt $i/$retries failed (HTTP $status), waiting..."
        sleep 2
    done
    die "Health check failed after $retries attempts"
}

# -----------------------------------------------------------------------------
# Slack deploy notification
# -----------------------------------------------------------------------------
notify_slack() {
    # Load .env to get SLACK_BOT_TOKEN
    local env_file="$PROJECT_DIR/.env"
    if [ ! -f "$env_file" ]; then
        log "No .env file, skipping Slack notification"
        return 0
    fi

    local token
    token=$(grep '^SLACK_BOT_TOKEN=' "$env_file" | cut -d'=' -f2-)
    if [ -z "$token" ]; then
        log "No SLACK_BOT_TOKEN found, skipping Slack notification"
        return 0
    fi

    local channel_name="${DEPLOY_NOTIFY_CHANNEL:-자일로랩스-정상영업합니다}"

    # Use python3 for all JSON/Slack work to avoid shell escaping issues
    local tmpscript
    tmpscript=$(mktemp /tmp/deploy-notify-XXXXXX.py)
    cat > "$tmpscript" <<'PYEOF'
import json, sys, urllib.request, os

token = os.environ["SLACK_TOKEN"]
channel_name = os.environ["SLACK_CHANNEL"]
deploy_time = os.environ["DEPLOY_TIME"]

headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}

# Find channel by name (paginate)
channel_id = None
cursor = ""
for _ in range(10):  # max 10 pages
    url = f"https://slack.com/api/conversations.list?types=public_channel,private_channel&limit=200&exclude_archived=true"
    if cursor:
        url += f"&cursor={cursor}"
    req = urllib.request.Request(url, headers=headers)
    with urllib.request.urlopen(req) as resp:
        data = json.loads(resp.read())
    for ch in data.get("channels", []):
        if ch.get("name") == channel_name or ch.get("name_normalized") == channel_name:
            channel_id = ch["id"]
            break
    if channel_id:
        break
    cursor = data.get("response_metadata", {}).get("next_cursor", "")
    if not cursor:
        break

if not channel_id:
    print(f"Channel '{channel_name}' not found", file=sys.stderr)
    sys.exit(0)  # non-fatal

# Build message with Korean changelog
changelog = os.environ.get("CHANGELOG", "").strip()
msg = f":rocket: *Xylolabs Knowledge Engine* 배포 완료 ({deploy_time})"
if changelog:
    msg += f"\n\n:memo: *변경 내역:*\n{changelog}"

payload = json.dumps({"channel": channel_id, "text": msg, "unfurl_links": False}).encode()
req = urllib.request.Request("https://slack.com/api/chat.postMessage", data=payload, headers=headers, method="POST")
with urllib.request.urlopen(req) as resp:
    result = json.loads(resp.read())

if result.get("ok"):
    print(f"Sent to #{channel_name}")
else:
    print(f"Slack API error: {result.get('error', 'unknown')}", file=sys.stderr)
PYEOF

    # Build changelog from git commits since last deploy tag
    local changelog=""
    cd "$PROJECT_DIR"
    local last_deploy_tag
    last_deploy_tag=$(git tag -l 'deploy-*' --sort=-version:refname | head -1)
    if [ -n "$last_deploy_tag" ]; then
        changelog=$(git log --pretty=format:"%s" "${last_deploy_tag}..HEAD" 2>/dev/null || echo "")
    else
        changelog=$(git log --pretty=format:"%s" -10 2>/dev/null || echo "")
    fi

    # Translate changelog to Korean using Gemini API
    local gemini_key
    gemini_key=$(grep '^GEMINI_API_KEY=' "$env_file" | cut -d'=' -f2-)
    local language
    language=$(grep '^LANGUAGE=' "$env_file" | cut -d'=' -f2- || echo "ko")
    language="${language:-ko}"

    if [ -n "$gemini_key" ] && [ -n "$changelog" ]; then
        local translate_script
        translate_script=$(mktemp /tmp/deploy-translate-XXXXXX.py)
        # Write changelog and key to temp files to avoid shell escaping issues
        local changelog_file key_file
        changelog_file=$(mktemp /tmp/deploy-changelog-XXXXXX.txt)
        key_file=$(mktemp /tmp/deploy-key-XXXXXX.txt)
        echo "$changelog" > "$changelog_file"
        echo "$gemini_key" > "$key_file"

        cat > "$translate_script" <<'TRANSLATEEOF'
import json, sys, os, urllib.request

key_file = os.environ["KEY_FILE"]
changelog_file = os.environ["CHANGELOG_FILE"]
lang = os.environ.get("LANG_TARGET", "ko")

api_key = open(key_file).read().strip()
raw = open(changelog_file).read().strip()

if not raw:
    sys.exit(0)

lang_names = {"ko": "Korean", "en": "English", "ja": "Japanese"}
lang_name = lang_names.get(lang, lang)

prompt = f"""Translate these git commit messages into concise {lang_name} bullet points.
Keep gitmoji. Remove conventional commit prefixes (feat/fix/etc) and scope.
Each line should be a short, natural description of the change.

Input:
{raw}

Output (one bullet per line, {lang_name}):"""

payload = json.dumps({
    "contents": [{"parts": [{"text": prompt}]}],
    "generationConfig": {"temperature": 0.1, "maxOutputTokens": 1024}
}).encode()

url = f"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash-lite:generateContent?key={api_key}"
req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"}, method="POST")
try:
    with urllib.request.urlopen(req, timeout=30) as resp:
        data = json.loads(resp.read())
    text = data["candidates"][0]["content"]["parts"][0]["text"].strip()
    print(text)
except Exception as e:
    print(f"Translation failed: {e}", file=sys.stderr)
    for line in raw.strip().split("\n"):
        if line.strip():
            print(f"• {line.strip()}")
TRANSLATEEOF

        changelog=$(KEY_FILE="$key_file" CHANGELOG_FILE="$changelog_file" LANG_TARGET="$language" \
            python3 "$translate_script" 2>&1 || echo "$changelog")
        rm -f "$translate_script" "$changelog_file" "$key_file"
    else
        changelog=$(echo "$changelog" | sed 's/^/• /')
    fi

    SLACK_TOKEN="$token" \
    SLACK_CHANNEL="$channel_name" \
    DEPLOY_TIME="$(date '+%Y-%m-%d %H:%M:%S %Z')" \
    CHANGELOG="$changelog" \
    python3 "$tmpscript" && log "Slack notification sent" || log "WARNING: Slack notification failed"

    # Tag this deploy for next changelog diff
    local deploy_tag="deploy-$(date -u '+%Y%m%d-%H%M%S')"
    git tag "$deploy_tag" 2>/dev/null && log "Tagged deploy: $deploy_tag" || true

    rm -f "$tmpscript"
}

# -----------------------------------------------------------------------------
# Install crontab
# -----------------------------------------------------------------------------
install_cron() {
    log "Installing crontab..."
    ssh_cmd "cat <<'CRON' | sudo tee /etc/cron.d/xylolabs-kb > /dev/null
# Incremental KB generation — every 6 hours (run as ubuntu to avoid permission issues)
7 */6 * * *  ubuntu  cd $REMOTE_KB_DIR && $REMOTE_DIR/scripts/generate-kb.sh >> /var/log/generate-kb.log 2>&1

# Weekly full rebuild — Sunday 3 AM
17 3 * * 0   ubuntu  $REMOTE_DIR/scripts/regenerate-kb.sh >> /var/log/generate-kb.log 2>&1
CRON"
    log "Crontab installed"
}

# -----------------------------------------------------------------------------
# Upload nginx config
# -----------------------------------------------------------------------------
upload_nginx_config() {
    local nginx_conf="$PROJECT_DIR/configs/nginx-xylolabs-kb.conf"
    if [ ! -f "$nginx_conf" ]; then
        die "Nginx config not found: $nginx_conf"
    fi

    log "Uploading nginx config..."
    scp_cmd "$nginx_conf" "${SERVER_USER}@${SERVER_HOST}:/tmp/nginx-xylolabs-kb.conf"
    ssh_cmd "sudo mv /tmp/nginx-xylolabs-kb.conf /etc/nginx/sites-available/xylolabs-kb.conf && \
             sudo ln -sf /etc/nginx/sites-available/xylolabs-kb.conf /etc/nginx/sites-enabled/xylolabs-kb.conf && \
             sudo nginx -t && \
             sudo systemctl reload nginx"
    log "Nginx config uploaded and reloaded"
}

# =============================================================================
# Main
# =============================================================================
main() {
    local with_env=false
    local with_cron=false
    local with_nginx=false

    for arg in "$@"; do
        case "$arg" in
            --with-env)   with_env=true ;;
            --with-cron)  with_cron=true ;;
            --with-nginx) with_nginx=true ;;
            --help)
                echo "Usage: $0 [--with-env] [--with-cron] [--with-nginx]"
                echo "  --with-env    Also upload .env file"
                echo "  --with-cron   Also install crontab"
                echo "  --with-nginx  Also upload nginx config"
                exit 0
                ;;
        esac
    done

    log "Starting deployment to $SERVER_HOST"

    build
    upload

    if [ "$with_env" = true ]; then
        upload_env
        upload_system_prompt
    fi

    restart
    verify_health
    notify_slack

    if [ "$with_cron" = true ]; then
        install_cron
    fi

    if [ "$with_nginx" = true ]; then
        upload_nginx_config
    fi

    log "Deployment complete!"
}

main "$@"
