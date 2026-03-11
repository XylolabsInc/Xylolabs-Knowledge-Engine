#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# regenerate-kb.sh — Full knowledge base rebuild
#
# Resets sync state to epoch and runs generate-kb.sh to rebuild everything.
# Intended for weekly scheduled runs (e.g., Sunday 3 AM).
# =============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KB_REPO_DIR="${KB_REPO_DIR:-/opt/knowledge}"
LOG_PREFIX="[regenerate-kb]"

log() { echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') $LOG_PREFIX $*"; }

log "Starting full knowledge base rebuild"

# Reset sync state to epoch
SYNC_FILE="$KB_REPO_DIR/_meta/sync-state.json"
if [ -f "$SYNC_FILE" ]; then
    log "Resetting sync state to epoch"
    cat > "$SYNC_FILE" << 'EOF'
{
  "slack": "1970-01-01T00:00:00Z",
  "google": "1970-01-01T00:00:00Z",
  "notion": "1970-01-01T00:00:00Z"
}
EOF
fi

# Run the incremental generator (which will now fetch everything since epoch)
# It may take multiple runs to process all documents due to the 500-doc cap
MAX_ITERATIONS=10
for i in $(seq 1 $MAX_ITERATIONS); do
    log "Rebuild iteration $i of $MAX_ITERATIONS"
    if ! "$SCRIPT_DIR/generate-kb.sh"; then
        log "WARNING: generate-kb.sh failed on iteration $i"
        break
    fi

    # Check if there are more documents to process
    # If the last run processed 0 docs, we're done
    # (generate-kb.sh logs "No documents processed" when nothing new)
    sleep 2
done

log "Full rebuild complete"
