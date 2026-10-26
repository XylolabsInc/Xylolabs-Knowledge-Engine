#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# deploy.sh — Deploy xylolabs-kb to AWS server
#
# Builds the Go binary for ARM64 and stages it on the server with the
# DaouOffice plugin (plugins/daouoffice) and its Node packages. Only then does
# it stop the service (which waits for a running plugin submission), swap the
# staged binary and plugin in, start it, and verify health and plugin
# registration. A failed step before the stop leaves the running service and
# its files untouched.
#
# Environment:
#   DAOUOFFICE_CONFIG_LOCAL  Local company config to upload as
#                            /opt/xylolabs-kb/configs/daouoffice.json (0600).
#                            Unset or missing: the server copy is left alone.
#   PLUGIN_READY_TIMEOUT     Seconds to wait for the plugin to register (150).
#   NODE_MAJOR, NODE_VERSION For --provision: Node major line (24) or an exact
#                            version (e.g. 24.22.1).
# =============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Server config
SERVER_HOST="${SERVER_HOST:-brain.internal.xylolabs.com}"
SERVER_USER="${SERVER_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/xylolabs-bots.pem}"
REMOTE_DIR="/opt/xylolabs-kb"
REMOTE_KB_DIR="/opt/knowledge"

# DaouOffice plugin. Its data dir holds per-person sessions and records and is
# never touched by a deploy.
PLUGIN_SRC="$PROJECT_DIR/plugins/daouoffice"
REMOTE_PLUGIN_DIR="$REMOTE_DIR/plugins/daouoffice"
REMOTE_PLUGIN_DATA_DIR="$REMOTE_DIR/data/plugins/daouoffice"
REMOTE_PLUGIN_CONFIG="$REMOTE_DIR/configs/daouoffice.json"
REMOTE_NODE_DIR="/opt/node"
PLUGIN_READY_TIMEOUT="${PLUGIN_READY_TIMEOUT:-150}"
NODE_MAJOR="${NODE_MAJOR:-24}"
PLUGIN_ENABLED=false # set by detect_plugin from the server's .env

# Build config
BINARY_NAME="xylolabs-kb"
BUILD_OUTPUT="$PROJECT_DIR/bin/${BINARY_NAME}-linux-arm64"

LOG_PREFIX="[deploy]"
log() { echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') $LOG_PREFIX $*"; }
die() { log "FATAL: $*"; exit 1; }

# Apparent size, humanized. Do not use `du` here: over NFS it reports
# server-side allocated blocks, which read as 512B for a freshly written file
# and as the post-compression size on a ZFS-backed export - so build output on
# a network-mounted work dir gets misreported.
human_size() {
    local bytes
    bytes=$(wc -c < "$1" 2>/dev/null | tr -d ' ') || { echo "unknown"; return; }
    [ -n "$bytes" ] || { echo "unknown"; return; }
    awk -v b="$bytes" 'BEGIN {
        split("B KB MB GB", u, " "); i = 1
        while (b >= 1024 && i < 4) { b /= 1024; i++ }
        printf (i == 1 ? "%d%s" : "%.1f%s"), b, u[i]
    }'
}

# Keepalives hold the connection through long commands: stopping the service
# can wait minutes for a running plugin submission.
SSH_OPTS=(-i "$SSH_KEY" -o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=20)

ssh_cmd() {
    # Callers build remote command strings here on purpose (paths, users).
    # shellcheck disable=SC2029
    ssh "${SSH_OPTS[@]}" "${SERVER_USER}@${SERVER_HOST}" "$@"
}

scp_cmd() {
    scp "${SSH_OPTS[@]}" "$@"
}

# ssh_bash runs the bash script on stdin on the server with the given
# arguments. ssh joins its arguments into one command line for the remote
# shell, so each one is quoted here; an empty one would otherwise vanish.
ssh_bash() {
    local quoted=() arg
    for arg in "$@"; do
        quoted+=("$(printf '%q' "$arg")")
    done
    ssh_cmd bash -s -- "${quoted[@]}"
}

# -----------------------------------------------------------------------------
# Build
# -----------------------------------------------------------------------------
build() {
    log "Building for linux/arm64..."
    cd "$PROJECT_DIR"
    GOOS=linux GOARCH=arm64 go build -o "$BUILD_OUTPUT" ./cmd/xylolabs-kb/
    log "Built: $BUILD_OUTPUT ($(human_size "$BUILD_OUTPUT"))"

    log "Building kb-gen for linux/arm64..."
    GOOS=linux GOARCH=arm64 go build -o "$PROJECT_DIR/bin/kb-gen-linux-arm64" ./cmd/kb-gen/
    log "Built: kb-gen-linux-arm64 ($(human_size "$PROJECT_DIR/bin/kb-gen-linux-arm64"))"
}

# -----------------------------------------------------------------------------
# Upload
# -----------------------------------------------------------------------------
upload() {
    log "Uploading binary and scripts to $SERVER_HOST..."

    # Ensure remote directories exist
    ssh_cmd "sudo mkdir -p $REMOTE_DIR/bin $REMOTE_DIR/scripts $REMOTE_DIR/configs"

    # Stage the service binary; restart() swaps it in once the service has
    # stopped, so a deploy that fails before then changes nothing that runs.
    scp_cmd "$BUILD_OUTPUT" "${SERVER_USER}@${SERVER_HOST}:/tmp/${BINARY_NAME}"
    ssh_cmd "sudo mv /tmp/${BINARY_NAME} $REMOTE_DIR/bin/${BINARY_NAME}.new && sudo chmod +x $REMOTE_DIR/bin/${BINARY_NAME}.new"

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
# DaouOffice plugin
# -----------------------------------------------------------------------------
stage_plugin() {
    [ -d "$PLUGIN_SRC" ] || die "Plugin source not found: $PLUGIN_SRC"
    log "Staging plugins/daouoffice in $REMOTE_PLUGIN_DIR.new..."

    # Ship the source only: no installed packages, tests, local data or
    # databases, and no local company config.
    local -a tar_opts=(
        --exclude=node_modules
        --exclude=daouoffice/test
        --exclude=daouoffice/data
        --exclude='*.db*'
        --exclude=daouoffice/config.json
        --exclude=.DS_Store
        --exclude='._*'
    )
    if tar --version 2>/dev/null | grep -q bsdtar; then
        tar_opts+=(--no-xattrs --no-mac-metadata)
    fi
    # Compress with gzip, not tar -z: bsdtar pads its -z output to whole
    # records, and GNU tar on the host rejects the trailing zeros.
    COPYFILE_DISABLE=1 tar -C "$PROJECT_DIR/plugins" -cf - "${tar_opts[@]}" daouoffice \
        | gzip -c \
        | ssh_cmd "umask 077 && cat > /tmp/xylolabs-daouoffice.tgz" \
        || die "Uploading the plugin failed; the running service was not touched"

    # Unpack next to the live plugin. The live one keeps running untouched
    # until restart() swaps the two.
    ssh_bash "$REMOTE_PLUGIN_DIR" "$REMOTE_PLUGIN_DATA_DIR" "$SERVER_USER" <<'REMOTE' \
        || die "Staging the plugin failed; the running service was not touched"
set -euo pipefail
dir=$1 data_dir=$2 owner=$3
new="$dir.new"
archive=/tmp/xylolabs-daouoffice.tgz
trap 'rm -f "$archive"' EXIT
sudo mkdir -p "$(dirname "$dir")"
sudo rm -rf "$new"
sudo install -d -o "$owner" -g "$owner" -m 0755 "$new"
tar -xzf "$archive" -C "$new" --strip-components=1 --no-same-owner --warning=no-unknown-keyword
[ -d "$data_dir" ] || sudo install -d -o "$owner" -g "$owner" -m 0700 "$data_dir"
REMOTE
    log "Plugin staged"
}
# install_plugin_deps installs the plugin's runtime packages with the Node at
# /opt/node and the headless Chromium Playwright drives. With "true" it also
# installs Chromium's system packages (needs sudo; used by --provision).
install_plugin_deps() {
    local with_deps="${1:-false}" browsers_path="${2:-}"
    log "Installing plugin packages in the staged plugin..."
    ssh_bash "$REMOTE_PLUGIN_DIR" "$REMOTE_NODE_DIR" "$with_deps" "$browsers_path" <<'REMOTE' \
        || die "Installing the plugin's packages failed; the running service was not touched"
set -euo pipefail
dir=$1 node_dir=$2 with_deps=$3 browsers_path=$4
export PATH="$node_dir/bin:$PATH"
# Install browsers where the service will look for them.
if [ -n "$browsers_path" ]; then
    export PLAYWRIGHT_BROWSERS_PATH="$browsers_path"
fi
command -v node >/dev/null || { echo "node not found in $node_dir/bin; run deploy.sh --provision" >&2; exit 1; }
cd "$dir.new"
want="$(node --version) $(sha256sum package-lock.json | cut -d' ' -f1)"
live_stamp="$dir/node_modules/.xylolabs-deploy-stamp"
if [ -f "$live_stamp" ] && [ "$(cat "$live_stamp")" = "$want" ]; then
    # Same lockfile and Node: hard-link the live packages instead of
    # reinstalling. Nothing writes to them, and npm ci would unlink, not edit.
    cp -al "$dir/node_modules" node_modules
    echo "Reusing node_modules: same Node ($(node --version)) and package-lock.json"
else
    npm ci --omit=dev --no-audit --no-fund
    echo "$want" > node_modules/.xylolabs-deploy-stamp
fi
if [ "$with_deps" = true ]; then
    # System packages need root. Run that part with sudo, and the browser
    # download as this user, so the browser lands in this user's cache.
    sudo -n env PATH="$PATH" ./node_modules/.bin/playwright install-deps chromium-headless-shell
fi
./node_modules/.bin/playwright install chromium-headless-shell
REMOTE
    log "Plugin packages ready"
}
# upload_plugin_config ships the company rules (approval lines, cards,
# projects). The file holds staff names and card numbers: it is never printed
# and never committed, and only goes up when DAOUOFFICE_CONFIG_LOCAL names it.
upload_plugin_config() {
    local src="${DAOUOFFICE_CONFIG_LOCAL:-}"
    if [ -z "$src" ]; then
        log "DAOUOFFICE_CONFIG_LOCAL not set; leaving the server's company config as it is"
        return 0
    fi
    if [ ! -f "$src" ]; then
        log "WARNING: DAOUOFFICE_CONFIG_LOCAL file not found; leaving the server's company config as it is"
        return 0
    fi
    # Report only where the JSON breaks, never the content.
    python3 - "$src" <<'PYEOF' || die "DAOUOFFICE_CONFIG_LOCAL is not valid JSON; not uploading it"
import json, sys
try:
    json.load(open(sys.argv[1], encoding="utf-8"))
except (ValueError, OSError) as e:
    sys.exit("invalid JSON at line %s, column %s" % (getattr(e, "lineno", "?"), getattr(e, "colno", "?")))
PYEOF
    log "Uploading company config ($(human_size "$src")) to $REMOTE_PLUGIN_CONFIG..."
    ssh_cmd "sudo mkdir -p $REMOTE_DIR/configs && sudo sh -c 'umask 077 && cat > $REMOTE_PLUGIN_CONFIG.tmp && chown $SERVER_USER:$SERVER_USER $REMOTE_PLUGIN_CONFIG.tmp && chmod 0600 $REMOTE_PLUGIN_CONFIG.tmp && mv $REMOTE_PLUGIN_CONFIG.tmp $REMOTE_PLUGIN_CONFIG'" < "$src" \
        || die "Uploading the company config failed"
    log "Company config uploaded"
}

# provision installs what the plugin needs on a fresh host: Node.js LTS in
# /opt/node (checked against nodejs.org's SHA256 list) and Korean fonts.
# Chromium's system packages follow in install_plugin_deps when the plugin is
# enabled. Not run by default.
provision() {
    log "Provisioning Node.js ${NODE_VERSION:-$NODE_MAJOR.x} in $REMOTE_NODE_DIR and Korean fonts..."
    ssh_bash "$REMOTE_NODE_DIR" "$NODE_MAJOR" "${NODE_VERSION:-}" <<'REMOTE'
set -euo pipefail
node_dir=$1 major=$2 version=$3
case "$(uname -m)" in
    aarch64|arm64) arch=arm64 ;;
    x86_64) arch=x64 ;;
    *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
if [ -n "$version" ]; then
    dist="https://nodejs.org/dist/v${version#v}"
else
    dist="https://nodejs.org/dist/latest-v${major}.x"
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The SHA256 list comes over HTTPS from nodejs.org; it guards against a
# corrupt or swapped download, not against a compromised nodejs.org.
curl -fsSL "$dist/SHASUMS256.txt" -o "$work/SHASUMS256.txt"
line=$(grep -E "  node-v[0-9]+\.[0-9]+\.[0-9]+-linux-${arch}\.tar\.xz$" "$work/SHASUMS256.txt" | head -n 1 || true)
[ -n "$line" ] || { echo "no linux-$arch tarball listed in $dist/SHASUMS256.txt" >&2; exit 1; }
sum=${line%% *}
file=${line##* }
want=${file#node-}
want=${want%%-linux-*}

current=$("$node_dir/bin/node" --version 2>/dev/null || true)
if [ "$current" = "$want" ]; then
    echo "Node.js $want is already installed in $node_dir"
else
    curl -fsSL "$dist/$file" -o "$work/$file"
    (cd "$work" && echo "$sum  $file" | sha256sum -c -)
    sudo rm -rf "$node_dir.new" "$node_dir.old"
    sudo mkdir -p "$node_dir.new"
    sudo tar -xJf "$work/$file" -C "$node_dir.new" --strip-components=1 --no-same-owner
    if [ -e "$node_dir" ]; then
        sudo mv "$node_dir" "$node_dir.old"
    fi
    sudo mv "$node_dir.new" "$node_dir"
    sudo rm -rf "$node_dir.old"
    echo "Installed Node.js $("$node_dir/bin/node" --version) in $node_dir"
fi

sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fonts-nanum
REMOTE
    log "Provisioning done"
}

# verify_plugin waits for main's registration line after a restart. It fails
# the deploy when the plugin is enabled on the server but did not register.
# server_env prints KEY's value as the service will see it through systemd's
# EnvironmentFile=: the last KEY=value line wins, surrounding whitespace and a
# CR are dropped, and one pair of outer quotes is removed. systemd has no
# "export" keyword and keeps a trailing "# comment" as part of the value, so
# those forms are reported rather than quietly accepted. Exit status 2: the
# server has no .env.
server_env() {
    ssh_bash "$REMOTE_DIR/.env" "$1" <<'REMOTE'
set -euo pipefail
file=$1 key=$2
sudo test -f "$file" || exit 2
if sudo grep -qE "^[[:space:]]*export[[:space:]]+${key}[[:space:]]*=" "$file"; then
    echo "WARNING: $file has 'export ${key}=...'; systemd ignores that line, so drop the 'export'" >&2
fi
line=$(sudo grep -E "^[[:space:]]*${key}[[:space:]]*=" "$file" | tail -n 1 || true)
[ -n "$line" ] || exit 0
value=${line#*=}
value=$(printf '%s' "$value" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')
case "$value" in
    \"*\") value=${value#\"}; value=${value%\"} ;;
    \'*\') value=${value#\'}; value=${value%\'} ;;
esac
case "$value" in
    *[[:space:]]\#*) echo "WARNING: ${key} in $file has a trailing comment; systemd keeps it as part of the value" >&2 ;;
esac
printf '%s\n' "$value"
REMOTE
}

# detect_plugin reads DAOUOFFICE_ENABLED from the server's .env once and sets
# PLUGIN_ENABLED. It logs what it decided and why.
detect_plugin() {
    local value rc=0
    value=$(server_env DAOUOFFICE_ENABLED) || rc=$?
    PLUGIN_ENABLED=false
    case "$rc" in
        0) ;;
        2)
            log "No $REMOTE_DIR/.env on the server: treating the DaouOffice plugin as disabled"
            return 0
            ;;
        *) die "Could not read $REMOTE_DIR/.env on the server" ;;
    esac
    case "$(printf '%s' "$value" | tr '[:upper:]' '[:lower:]')" in
        1 | true | yes | on)
            PLUGIN_ENABLED=true
            log "DAOUOFFICE_ENABLED=$value on the server: shipping the DaouOffice plugin"
            ;;
        *)
            log "DAOUOFFICE_ENABLED is '${value:-unset}' on the server: skipping the DaouOffice plugin steps"
            ;;
    esac
}

# verify_plugin waits for main's registration line after a restart and fails
# the deploy when the enabled plugin did not register.
verify_plugin() {
    local since=$1
    log "Waiting up to ${PLUGIN_READY_TIMEOUT}s for the DaouOffice plugin to register..."
    local deadline=$((SECONDS + PLUGIN_READY_TIMEOUT))
    local line tools
    while :; do
        line=$(ssh_cmd "sudo journalctl -u xylolabs-kb --since @$since -o cat --no-pager | grep -m 1 -E '\"msg\":\"daouoffice plugin (ready|disabled)' || true")
        case "$line" in
            *'daouoffice plugin ready'*)
                tools=$(printf '%s\n' "$line" | sed -n 's/.*"tools":\([0-9][0-9]*\).*/\1/p')
                log "DaouOffice plugin registered with ${tools:-an unknown number of} tools"
                return 0
                ;;
            *'daouoffice plugin disabled'*)
                log "$line"
                show_plugin_logs "$since"
                rollback_hint
                die "DaouOffice plugin is enabled but did not start"
                ;;
        esac
        [ "$SECONDS" -lt "$deadline" ] || break
        sleep 5
    done
    show_plugin_logs "$since"
    rollback_hint
    die "DaouOffice plugin is enabled but did not register within ${PLUGIN_READY_TIMEOUT}s"
}

rollback_hint() {
    log "The previous binary is kept as $REMOTE_DIR/bin/${BINARY_NAME}.prev and the previous plugin as $REMOTE_PLUGIN_DIR.prev"
}
show_plugin_logs() {
    log "Recent plugin log lines:"
    ssh_cmd "sudo journalctl -u xylolabs-kb --since @$1 -o cat --no-pager | grep -E 'daouoffice|mcpstdio|plugin' | tail -n 30" || true
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
    local swap_plugin="${1:-false}"
    local stop_timeout
    stop_timeout=$(ssh_cmd "systemctl show -p TimeoutStopUSec --value xylolabs-kb" 2>/dev/null || echo "unknown")
    log "Stopping xylolabs-kb; it first lets a running plugin submission finish (up to ${stop_timeout})..."
    ssh_bash "$REMOTE_DIR/bin/${BINARY_NAME}" "$REMOTE_PLUGIN_DIR" "$swap_plugin" <<'REMOTE' \
        || die "Restart failed"
set -euo pipefail
bin=$1 dir=$2 swap_plugin=$3
sudo systemctl daemon-reload
sudo systemctl enable --quiet xylolabs-kb
started=$SECONDS
sudo systemctl stop xylolabs-kb
echo "Stopped after $((SECONDS - started))s"

# Swap in the staged binary and plugin while nothing runs, keeping the
# previous ones for a rollback.
if [ -e "$bin.new" ]; then
    if [ -e "$bin" ]; then
        sudo mv -f "$bin" "$bin.prev"
    fi
    sudo mv "$bin.new" "$bin"
fi
if [ "$swap_plugin" = true ] && [ -d "$dir.new" ]; then
    for keep in data config.json; do
        if [ -e "$dir/$keep" ] && [ ! -e "$dir.new/$keep" ]; then
            sudo mv "$dir/$keep" "$dir.new/$keep"
        fi
    done
    sudo rm -rf "$dir.prev"
    if [ -e "$dir" ]; then
        sudo mv "$dir" "$dir.prev"
    fi
    sudo mv "$dir.new" "$dir"
    echo "Plugin swapped in"
fi

sudo systemctl start xylolabs-kb
REMOTE
    sleep 3
    local status
    status=$(ssh_cmd "systemctl is-active xylolabs-kb" 2>/dev/null || echo "unknown")
    if [ "$status" != "active" ]; then
        log "WARNING: Service status is '$status'"
        ssh_cmd "sudo journalctl -u xylolabs-kb --no-pager -n 20"
        rollback_hint
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

    # Translate changelog to Korean using Gemini API, or an OpenAI-compatible
    # endpoint (e.g. OpenRouter) when LLM_ENDPOINT is set in the deployed env
    # Note: these use `|| echo ""`/`|| echo "<default>"` fallbacks (not just a
    # bare grep|cut) because grep exits non-zero when a line is entirely
    # absent from .env, which would abort the script under `set -e` — a real
    # case now that an OpenRouter-only setup may omit GEMINI_API_KEY entirely.
    local gemini_key
    gemini_key=$(grep '^GEMINI_API_KEY=' "$env_file" | cut -d'=' -f2- || echo "")
    local gemini_model
    gemini_model=$(grep '^GEMINI_MODEL=' "$env_file" | cut -d'=' -f2- || echo "gemini-3.8-flash")
    gemini_model="${gemini_model:-gemini-3.8-flash}"
    local llm_endpoint llm_api_key llm_model
    llm_endpoint=$(grep '^LLM_ENDPOINT=' "$env_file" | cut -d'=' -f2- || echo "")
    llm_api_key=$(grep '^LLM_API_KEY=' "$env_file" | cut -d'=' -f2- || echo "")
    llm_model=$(grep '^LLM_MODEL=' "$env_file" | cut -d'=' -f2- || echo "")
    local language
    language=$(grep '^LANGUAGE=' "$env_file" | cut -d'=' -f2- || echo "ko")
    language="${language:-ko}"

    if { [ -n "$gemini_key" ] || [ -n "$llm_api_key" ]; } && [ -n "$changelog" ]; then
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

# LLM_ENDPOINT switches to an OpenAI-compatible chat/completions endpoint
# (e.g. OpenRouter); empty (the default) keeps native Gemini.
llm_endpoint = os.environ.get("LLM_ENDPOINT", "")
model = os.environ.get("GEMINI_MODEL", "gemini-3.8-flash")
if llm_endpoint:
    payload = json.dumps({
        "model": os.environ.get("LLM_MODEL", model),
        "messages": [{"role": "user", "content": prompt}],
    }).encode()
    req = urllib.request.Request(llm_endpoint, data=payload, headers={
        "Content-Type": "application/json",
        "Authorization": "Bearer " + (os.environ.get("LLM_API_KEY") or api_key),
    }, method="POST")
else:
    payload = json.dumps({
        "contents": [{"parts": [{"text": prompt}]}],
        "generationConfig": {"temperature": 0.1, "maxOutputTokens": 1024}
    }).encode()
    url = f"https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent?key={api_key}"
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"}, method="POST")

try:
    with urllib.request.urlopen(req, timeout=30) as resp:
        data = json.loads(resp.read())
    if llm_endpoint:
        text = data["choices"][0]["message"]["content"].strip()
    else:
        text = data["candidates"][0]["content"]["parts"][0]["text"].strip()
    print(text)
except Exception as e:
    print(f"Translation failed: {e}", file=sys.stderr)
    for line in raw.strip().split("\n"):
        if line.strip():
            print(f"• {line.strip()}")
TRANSLATEEOF

        local -a translate_env=(
            "KEY_FILE=$key_file"
            "CHANGELOG_FILE=$changelog_file"
            "LANG_TARGET=$language"
            "GEMINI_MODEL=$gemini_model"
            "LLM_ENDPOINT=$llm_endpoint"
            "LLM_API_KEY=$llm_api_key"
        )
        [ -n "$llm_model" ] && translate_env+=("LLM_MODEL=$llm_model")

        changelog=$(env "${translate_env[@]}" python3 "$translate_script" 2>&1 || echo "$changelog")
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
    # The cron entries redirect into /var/log/generate-kb.log. /var/log is
    # root:syslog and not writable by the ubuntu user the jobs run as, so
    # without this the shell cannot open the redirect and the job fails before
    # the script ever runs - silently, since there is nowhere to log it.
    log "Preparing cron log file..."
    ssh_cmd "sudo install -o $SERVER_USER -g $SERVER_USER -m 0644 /dev/null /var/log/generate-kb.log 2>/dev/null || sudo chown $SERVER_USER:$SERVER_USER /var/log/generate-kb.log"
    ssh_cmd "cat <<'ROTATE' | sudo tee /etc/logrotate.d/xylolabs-kb > /dev/null
/var/log/generate-kb.log {
    su root syslog
    weekly
    rotate 8
    compress
    delaycompress
    missingok
    notifempty
    create 0644 $SERVER_USER $SERVER_USER
}
ROTATE"

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
    local with_provision=false

    for arg in "$@"; do
        case "$arg" in
            --with-env)   with_env=true ;;
            --with-cron)  with_cron=true ;;
            --with-nginx) with_nginx=true ;;
            --provision)  with_provision=true ;;
            --help)
                echo "Usage: $0 [--with-env] [--with-cron] [--with-nginx] [--provision]"
                echo "  --with-env    Also upload .env file"
                echo "  --with-cron   Also install crontab"
                echo "  --with-nginx  Also upload nginx config"
                echo "  --provision   Also install Node.js LTS in /opt/node, Chromium's system packages and Korean fonts"
                echo "Set DAOUOFFICE_CONFIG_LOCAL=<file> to upload the DaouOffice company config."
                exit 0
                ;;
            *) die "Unknown option: $arg (see --help)" ;;
        esac
    done

    log "Starting deployment to $SERVER_HOST"

    build
    upload

    if [ "$with_env" = true ]; then
        upload_env
        upload_system_prompt
    fi

    if [ "$with_provision" = true ]; then
        provision
    fi

    # Everything up to restart() only stages files; the service keeps running.
    detect_plugin
    if [ "$PLUGIN_ENABLED" = true ]; then
        local browsers_path=""
        browsers_path=$(server_env PLAYWRIGHT_BROWSERS_PATH) || die "Could not read $REMOTE_DIR/.env on the server"
        stage_plugin
        install_plugin_deps "$with_provision" "$browsers_path"
        upload_plugin_config
    elif [ "$with_provision" = true ]; then
        log "Chromium's system packages were not installed because the plugin is disabled; deploy again with --provision after enabling it"
    fi

    local restart_at
    restart_at=$(ssh_cmd "date +%s")
    restart "$PLUGIN_ENABLED"
    verify_health
    if [ "$PLUGIN_ENABLED" = true ]; then
        verify_plugin "$restart_at"
    fi
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
