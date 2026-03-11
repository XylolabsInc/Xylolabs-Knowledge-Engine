#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# deploy.sh — Deploy xylolabs-kb to OCI server
#
# Builds the Go binary for ARM64, uploads to server, restarts the service,
# and verifies health.
# =============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Server config
SERVER_HOST="${SERVER_HOST:-your-server-ip}"
SERVER_USER="${SERVER_USER:-ubuntu}"
SSH_KEY="${SSH_KEY:-$PROJECT_DIR/key.pem}"
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
        log "Uploading .env..."
        scp_cmd "$PROJECT_DIR/.env" "${SERVER_USER}@${SERVER_HOST}:/tmp/xylolabs-kb.env"
        ssh_cmd "sudo mv /tmp/xylolabs-kb.env $REMOTE_DIR/.env && sudo chmod 600 $REMOTE_DIR/.env"
    else
        log "No .env file found, skipping (ensure $REMOTE_DIR/.env exists on server)"
    fi
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
# Install crontab
# -----------------------------------------------------------------------------
install_cron() {
    log "Installing crontab..."
    ssh_cmd "cat <<'CRON' | sudo tee /etc/cron.d/xylolabs-kb > /dev/null
# Incremental KB generation — every 6 hours
7 */6 * * *  root  cd $REMOTE_KB_DIR && $REMOTE_DIR/scripts/generate-kb.sh >> /var/log/generate-kb.log 2>&1

# Weekly full rebuild — Sunday 3 AM
17 3 * * 0   root  $REMOTE_DIR/scripts/regenerate-kb.sh >> /var/log/generate-kb.log 2>&1
CRON"
    log "Crontab installed"
}

# =============================================================================
# Main
# =============================================================================
main() {
    local with_env=false
    local with_cron=false

    for arg in "$@"; do
        case "$arg" in
            --with-env)  with_env=true ;;
            --with-cron) with_cron=true ;;
            --help)
                echo "Usage: $0 [--with-env] [--with-cron]"
                echo "  --with-env   Also upload .env file"
                echo "  --with-cron  Also install crontab"
                exit 0
                ;;
        esac
    done

    log "Starting deployment to $SERVER_HOST"

    build
    upload

    if [ "$with_env" = true ]; then
        upload_env
    fi

    restart
    verify_health

    if [ "$with_cron" = true ]; then
        install_cron
    fi

    log "Deployment complete!"
}

main "$@"
