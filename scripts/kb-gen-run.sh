#!/bin/bash
# kb-gen-run.sh — Export documents from API and run kb-gen for each source.
# Usage: kb-gen-run.sh [--full]
#   --full: Re-index all documents (ignores sync state)
set -euo pipefail

# Guard: refuse to run as root to prevent file ownership issues
if [ "$(id -u)" = "0" ]; then
    echo "ERROR: kb-gen-run.sh must NOT run as root (creates root-owned files in the KB repo)." >&2
    echo "Run as: sudo -u ubuntu $0 $*" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
KB_DIR="${KB_REPO_DIR:-/opt/knowledge}"
API_URL="${API_URL:-http://localhost:8080}"
KB_GEN="${SCRIPT_DIR}/../kb-gen"
WORK_DIR="/tmp/kb-gen-work"
SOURCES=("slack" "google" "notion")
PAGE_SIZE=200

ENV_FILE="${ENV_FILE:-/opt/xylolabs-kb/.env}"

# Load env file if available
if [ -f "$ENV_FILE" ]; then
    set -a
    source "$ENV_FILE"
    set +a
fi

mkdir -p "$WORK_DIR"

FULL_MODE=false
if [[ "${1:-}" == "--full" ]]; then
    FULL_MODE=true
    echo "[kb-gen] Full re-index mode"
fi

for SOURCE in "${SOURCES[@]}"; do
    echo "[kb-gen] Processing source: $SOURCE"

    # Determine start timestamp from sync state (unless --full)
    SINCE=""
    if [[ "$FULL_MODE" == false ]]; then
        SYNC_FILE="$KB_DIR/_meta/sync-state.json"
        if [ -f "$SYNC_FILE" ]; then
            SINCE=$(python3 -c "
import json
try:
    with open('$SYNC_FILE') as f:
        state = json.load(f)
    print(state.get('$SOURCE', ''))
except:
    pass
" 2>/dev/null || true)
        fi
    fi

    # Export documents from API with pagination using temp files
    OUTPUT_FILE="$WORK_DIR/${SOURCE}-docs.json"
    MERGED_FILE="$WORK_DIR/${SOURCE}-merged.json"

    # Initialize merged file
    echo '[]' > "$MERGED_FILE"

    OFFSET=0
    TOTAL=0

    while true; do
        URL="${API_URL}/api/v1/documents?source=${SOURCE}&limit=${PAGE_SIZE}&offset=${OFFSET}"
        if [[ -n "$SINCE" ]]; then
            URL="${URL}&since=${SINCE}"
        fi

        PAGE_FILE="$WORK_DIR/${SOURCE}-page-${OFFSET}.json"
        curl -sf "$URL" > "$PAGE_FILE" 2>/dev/null || echo '{"documents":[],"total":0}' > "$PAGE_FILE"

        # Extract total and merge documents using temp files
        python3 << PYEOF
import json

with open('$PAGE_FILE') as f:
    page = json.load(f)

with open('$MERGED_FILE') as f:
    merged = json.load(f)

page_docs = page.get('documents', [])
merged.extend(page_docs)

with open('$MERGED_FILE', 'w') as f:
    json.dump(merged, f)

# Write total to a tmp file for bash to read
with open('$WORK_DIR/${SOURCE}-total.txt', 'w') as f:
    f.write(str(page.get('total', 0)))

with open('$WORK_DIR/${SOURCE}-page-count.txt', 'w') as f:
    f.write(str(len(page_docs)))
PYEOF

        if [[ "$TOTAL" -eq 0 ]]; then
            TOTAL=$(cat "$WORK_DIR/${SOURCE}-total.txt")
        fi

        PAGE_COUNT=$(cat "$WORK_DIR/${SOURCE}-page-count.txt")
        rm -f "$PAGE_FILE" "$WORK_DIR/${SOURCE}-total.txt" "$WORK_DIR/${SOURCE}-page-count.txt"

        OFFSET=$((OFFSET + PAGE_SIZE))
        if [[ $OFFSET -ge $TOTAL ]] || [[ "$PAGE_COUNT" -eq 0 ]]; then
            break
        fi
    done

    # Build final output file
    DOC_COUNT=$(python3 -c "
import json
with open('$MERGED_FILE') as f:
    docs = json.load(f)
print(len(docs))
")
    echo "[kb-gen] $SOURCE: $DOC_COUNT documents to process"

    if [[ "$DOC_COUNT" -eq 0 ]] || [[ "$DOC_COUNT" == "0" ]]; then
        echo "[kb-gen] $SOURCE: no new documents, skipping"
        rm -f "$MERGED_FILE"
        continue
    fi

    # Wrap in expected format
    python3 << PYEOF
import json
with open('$MERGED_FILE') as f:
    docs = json.load(f)
output = {'documents': docs, 'total': len(docs)}
with open('$OUTPUT_FILE', 'w') as f:
    json.dump(output, f)
PYEOF
    rm -f "$MERGED_FILE"

    # Run kb-gen
    echo "[kb-gen] Running kb-gen for $SOURCE ($DOC_COUNT docs)..."
    "$KB_GEN" \
        --input "$OUTPUT_FILE" \
        --source "$SOURCE" \
        --kb-dir "$KB_DIR" \
        || echo "[kb-gen] WARNING: kb-gen failed for $SOURCE"

    # Clean up
    rm -f "$OUTPUT_FILE"
done

# Git commit and push changes in KB repo
if [ -d "$KB_DIR/.git" ]; then
    cd "$KB_DIR"
    if ! git diff --quiet HEAD 2>/dev/null || [ -n "$(git ls-files --others --exclude-standard)" ]; then
        git add -A
        git commit -m "kb-gen: auto-update $(date -u +%Y-%m-%dT%H:%M:%SZ)" || true
        git push origin main 2>/dev/null || true
        echo "[kb-gen] Changes committed and pushed"
    else
        echo "[kb-gen] No changes to commit"
    fi
fi

rm -rf "$WORK_DIR"
echo "[kb-gen] Done"
