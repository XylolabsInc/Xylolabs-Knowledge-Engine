#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# backup-db.sh — Daily SQLite database backup with rotation
# =============================================================================

DB_PATH="${DB_PATH:-/opt/xylolabs-kb/xylolabs-kb.db}"
BACKUP_DIR="${BACKUP_DIR:-/opt/xylolabs-kb/backups}"
RETENTION_DAYS="${RETENTION_DAYS:-30}"
LOG_PREFIX="[backup-db]"

log() { echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') $LOG_PREFIX $*"; }

# Ensure backup directory exists
mkdir -p "$BACKUP_DIR"

# Check DB exists
if [ ! -f "$DB_PATH" ]; then
    log "ERROR: Database file not found: $DB_PATH"
    exit 1
fi

TIMESTAMP=$(date -u '+%Y%m%d-%H%M%S')
BACKUP_FILE="$BACKUP_DIR/xylolabs-kb-${TIMESTAMP}.db"

log "Starting backup of $DB_PATH..."

# Use SQLite .backup command for a consistent snapshot
if sqlite3 "$DB_PATH" ".backup '$BACKUP_FILE'"; then
    SIZE=$(du -h "$BACKUP_FILE" | cut -f1)
    log "Backup complete: $BACKUP_FILE ($SIZE)"
else
    log "ERROR: Backup failed"
    exit 1
fi

# Rotate old backups
DELETED=0
if [ "$RETENTION_DAYS" -gt 0 ]; then
    while IFS= read -r old_backup; do
        rm -f "$old_backup"
        DELETED=$((DELETED + 1))
    done < <(find "$BACKUP_DIR" -name "xylolabs-kb-*.db" -mtime +"$RETENTION_DAYS" -type f 2>/dev/null)
fi

if [ "$DELETED" -gt 0 ]; then
    log "Rotated $DELETED old backups (retention: ${RETENTION_DAYS} days)"
fi

log "Backup complete"
