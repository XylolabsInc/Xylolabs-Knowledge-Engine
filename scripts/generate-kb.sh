#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# generate-kb.sh — Incremental knowledge base generation
#
# Fetches new documents from the Go worker API, invokes Claude Code CLI to
# transform them into structured markdown, and commits to the knowledge repo.
# =============================================================================

# Guard: refuse to run as root to prevent file ownership issues
if [ "$(id -u)" = "0" ]; then
    echo "ERROR: generate-kb.sh must NOT run as root (creates root-owned files in the KB repo)." >&2
    echo "Run as: sudo -u ubuntu $0 $*" >&2
    exit 1
fi

# Configuration (override via environment)
API_BASE="${API_BASE:-http://localhost:8080}"
KB_REPO_DIR="${KB_REPO_DIR:-/opt/knowledge}"
LOCKFILE="${LOCKFILE:-/tmp/generate-kb.lock}"
LOG_PREFIX="[generate-kb]"
MAX_DOCS_PER_SOURCE="${MAX_DOCS_PER_SOURCE:-500}"
CLAUDE_MODEL="${CLAUDE_MODEL:-claude-opus-4-6}"
CLAUDE_MAX_BUDGET="${CLAUDE_MAX_BUDGET:-5.00}"

# KB generation backend: "gemini" (default, falls back to Claude CLI on quota errors) or "claude"
KB_BACKEND="${KB_BACKEND:-gemini}"
KB_GEN_MODEL="${KB_GEN_MODEL:-gemini-3.1-pro-preview}"
KB_GEN_THINKING="${KB_GEN_THINKING:-high}"
KB_GEN_API_KEY="${KB_GEN_API_KEY:-${GEMINI_API_KEY:-}}"

# Sources to process
SOURCES=("slack" "google" "notion")

# -----------------------------------------------------------------------------
# Logging
# -----------------------------------------------------------------------------
log()  { echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') $LOG_PREFIX $*"; }
die()  { log "FATAL: $*"; exit 1; }

# -----------------------------------------------------------------------------
# Lockfile — prevent concurrent runs
# -----------------------------------------------------------------------------
acquire_lock() {
    if [ -f "$LOCKFILE" ]; then
        local pid
        pid=$(cat "$LOCKFILE" 2>/dev/null || echo "")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            die "Another instance is running (PID $pid)"
        fi
        log "Removing stale lockfile (PID $pid)"
        rm -f "$LOCKFILE"
    fi
    echo $$ > "$LOCKFILE"
    trap 'rm -f "$LOCKFILE"' EXIT
}

# -----------------------------------------------------------------------------
# Health check
# -----------------------------------------------------------------------------
health_check() {
    log "Checking API health at $API_BASE..."
    local status
    status=$(curl -sf -o /dev/null -w "%{http_code}" "$API_BASE/health" 2>/dev/null || echo "000")
    if [ "$status" != "200" ]; then
        die "API health check failed (HTTP $status). Is the Go worker running?"
    fi
    log "API is healthy"
}

# -----------------------------------------------------------------------------
# Read sync state from knowledge repo
# -----------------------------------------------------------------------------
read_sync_state() {
    local source="$1"
    local sync_file="$KB_REPO_DIR/_meta/sync-state.json"

    if [ ! -f "$sync_file" ]; then
        echo "1970-01-01T00:00:00Z"
        return
    fi

    # Extract timestamp for the given source
    local ts
    ts=$(python3 -c "
import json, sys
with open('$sync_file') as f:
    data = json.load(f)
print(data.get('$source', '1970-01-01T00:00:00Z'))
" 2>/dev/null || echo "1970-01-01T00:00:00Z")
    echo "$ts"
}

# -----------------------------------------------------------------------------
# Fetch documents from API
# -----------------------------------------------------------------------------
fetch_documents() {
    local source="$1"
    local since="$2"
    local output_file="$3"

    log "Fetching $source documents since $since..."

    local url="${API_BASE}/api/v1/documents?since=${since}&source=${source}&limit=${MAX_DOCS_PER_SOURCE}"
    local http_code
    http_code=$(curl -sf -w "%{http_code}" -o "$output_file" "$url" 2>/dev/null || echo "000")

    if [ "$http_code" != "200" ]; then
        log "WARNING: Failed to fetch $source documents (HTTP $http_code)"
        echo '{"documents":[],"total":0}' > "$output_file"
        return 1
    fi

    local total
    total=$(python3 -c "
import json
with open('$output_file') as f:
    data = json.load(f)
print(data.get('total', 0))
" 2>/dev/null || echo "0")

    log "Fetched $total $source documents"
    return 0
}

# -----------------------------------------------------------------------------
# Invoke Claude Code CLI to process documents
# -----------------------------------------------------------------------------
process_with_claude() {
    local raw_file="$1"
    local source="$2"

    log "Processing $source with Claude Code CLI..."

    local doc_count
    doc_count=$(python3 -c "
import json
with open('$raw_file') as f:
    data = json.load(f)
print(data.get('total', 0))
" 2>/dev/null || echo "0")

    if [ "$doc_count" = "0" ]; then
        log "No new $source documents to process, skipping"
        return 0
    fi

    local prompt
    prompt="Process the raw $source documents in the file at $raw_file (contains $doc_count documents as JSON).

For each document, create or update the corresponding markdown file in this knowledge base repo.

Rules:
- Follow the instructions in CLAUDE.md for file naming, frontmatter format, and structure
- Slack messages: group by channel and date into daily digests at slack/channels/{channel-name}/{YYYY-MM-DD}.md
- Google docs: write to google/docs/{doc-slug}.md
- Notion pages: write to notion/pages/{page-slug}.md
- Update the section README.md index file ($source/README.md) with links to all files
- Update _meta/document-map.json with source_id to file path mappings for each processed document
- Update _meta/sync-state.json: set the '$source' timestamp to the latest document timestamp you processed
- If a daily digest file already exists, merge new messages into it (append to the Messages table)
- If a document file already exists, overwrite it with the updated content"

    cd "$KB_REPO_DIR"

    if ! claude -p "$prompt" \
        --dangerously-skip-permissions \
        --model "$CLAUDE_MODEL" \
        --max-turns 50 \
        --no-session-persistence 2>&1; then
        log "WARNING: Claude CLI returned non-zero for $source"
        return 1
    fi

    log "Claude CLI finished processing $source"
    return 0
}

# -----------------------------------------------------------------------------
# Invoke kb-gen (Gemini) to process documents
# -----------------------------------------------------------------------------
process_with_gemini() {
    local raw_file="$1"
    local source="$2"

    local doc_count
    doc_count=$(python3 -c "
import json
with open('$raw_file') as f:
    data = json.load(f)
print(data.get('total', 0))
" 2>/dev/null || echo "0")

    if [ "$doc_count" = "0" ]; then
        log "No new $source documents to process, skipping"
        return 0
    fi

    log "Processing $source with Gemini ($KB_GEN_MODEL, thinking=$KB_GEN_THINKING)..."

    local kb_gen_bin
    # Try project-local binary first, then remote install path
    for candidate in \
        "${PROJECT_DIR:-}/bin/kb-gen" \
        "${REMOTE_DIR:-/opt/xylolabs-kb}/bin/kb-gen"; do
        if [ -f "$candidate" ] && [ -x "$candidate" ]; then
            kb_gen_bin="$candidate"
            break
        fi
    done

    if [ -z "${kb_gen_bin:-}" ]; then
        die "kb-gen binary not found. Build with: GOOS=linux GOARCH=arm64 go build -o bin/kb-gen ./cmd/kb-gen/"
    fi

    if "$kb_gen_bin" \
        --input "$raw_file" \
        --source "$source" \
        --kb-dir "$KB_REPO_DIR" \
        --model "$KB_GEN_MODEL" \
        --thinking "$KB_GEN_THINKING" \
        --api-key "$KB_GEN_API_KEY" 2>&1; then
        log "Gemini finished processing $source"
        return 0
    fi

    log "WARNING: Gemini kb-gen failed for $source, falling back to Claude Code CLI (Opus 4.6)..."
    process_with_claude "$raw_file" "$source"
}

# -----------------------------------------------------------------------------
# Git commit and push
# -----------------------------------------------------------------------------
git_commit_push() {
    cd "$KB_REPO_DIR"

    # Check for changes
    if git diff --quiet && git diff --cached --quiet && [ -z "$(git ls-files --others --exclude-standard)" ]; then
        log "No changes to commit"
        return 0
    fi

    log "Committing changes..."
    git add -A
    git commit -m "chore(kb): update knowledge base $(date -u '+%Y-%m-%dT%H:%M:%SZ')"

    log "Pushing to remote..."
    git pull --rebase origin main 2>/dev/null || true
    if ! git push origin main; then
        log "WARNING: Push failed, will retry once..."
        git pull --rebase origin main
        git push origin main || die "Push failed after retry"
    fi

    log "Changes pushed successfully"
}

# =============================================================================
# Main
# =============================================================================
main() {
    log "Starting knowledge base generation (backend=$KB_BACKEND)"

    acquire_lock
    health_check

    # Ensure KB repo is clean
    cd "$KB_REPO_DIR"
    git pull --rebase origin main 2>/dev/null || log "WARNING: git pull failed, continuing with local state"

    TMP_DIR=$(mktemp -d)
    trap 'rm -rf "${TMP_DIR:-}"; rm -f "$LOCKFILE"' EXIT

    local any_processed=false

    for source in "${SOURCES[@]}"; do
        local since
        since=$(read_sync_state "$source")
        local raw_file="$TMP_DIR/${source}-raw.json"

        if fetch_documents "$source" "$since" "$raw_file"; then
            if [ "$KB_BACKEND" = "gemini" ]; then
                if process_with_gemini "$raw_file" "$source"; then
                    any_processed=true
                fi
            else
                if process_with_claude "$raw_file" "$source"; then
                    any_processed=true
                fi
            fi
        fi
    done

    # --- People Directory ---
    log "Generating people knowledge from Google Workspace directory..."
    local kb_gen_bin=""
    for candidate in \
        "${PROJECT_DIR:-}/bin/kb-gen" \
        "${REMOTE_DIR:-/opt/xylolabs-kb}/bin/kb-gen"; do
        if [ -f "$candidate" ] && [ -x "$candidate" ]; then
            kb_gen_bin="$candidate"
            break
        fi
    done

    if [ -z "${kb_gen_bin:-}" ]; then
        log "WARNING: kb-gen binary not found, skipping people generation"
    elif [ -z "${GOOGLE_CREDS_FILE:-}" ]; then
        log "WARNING: GOOGLE_CREDS_FILE not set, skipping people generation"
    else
        if "$kb_gen_bin" \
            --fetch-people \
            --google-creds "$GOOGLE_CREDS_FILE" \
            --impersonate "${GOOGLE_IMPERSONATE_EMAIL:-}" \
            --domain "${GOOGLE_DOMAIN:-xylolabs.com}" \
            --kb-dir "$KB_REPO_DIR" \
            ${DRY_RUN:+--dry-run} 2>&1; then
            log "People knowledge generation complete"
            any_processed=true
        else
            log "WARNING: People knowledge generation failed"
        fi
    fi

    if [ "$any_processed" = true ]; then
        git_commit_push
    else
        log "No documents processed across any source"
    fi

    log "Knowledge base generation complete"
}

main "$@"
