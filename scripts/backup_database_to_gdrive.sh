#!/usr/bin/env bash
set -euo pipefail

# Take a consistent snapshot of the live invoice database (via the app's own
# "-backupdb" mode, so we never copy the .db/.db-wal/.db-shm files directly
# while they might be mid-write) and mirror it, along with the evidence photo
# bucket, to Google Drive. Deliberately excludes MinIO's own .minio.sys
# internal state (bloom filter / usage cache files it rewrites continuously
# while running) since that's regenerated automatically and copying it live
# just produces checksum-mismatch noise. Kept separate from
# backup_invoices_to_gdrive.sh, which only mirrors rendered PDFs.

PROJECT_DIR="${PROJECT_DIR:-/home/ubuntu/docker/alexeisinvoice}"
BACKUP_HOST_DIR="${BACKUP_HOST_DIR:-/srv/invoice/data/db-backup}"
BACKUP_CONTAINER_PATH="${BACKUP_CONTAINER_PATH:-/data/db-backup/invoices-backup.db}"
DATA_HOST_DIR="${DATA_HOST_DIR:-/srv/invoice/data}"
MINIO_DIR="${MINIO_DIR:-/srv/invoice/minio/invoice-files}"
RCLONE_CONFIG="${RCLONE_CONFIG:-/home/ubuntu/.config/rclone/rclone.conf}"
REMOTE="${REMOTE:-gdrive:alexeisinvoice-backups}"
RETENTION_DAYS="${RETENTION_DAYS:-30}"
LOCK_FILE="${LOCK_FILE:-/tmp/invoice-db-backup.lock}"
LOG_FILE="${LOG_FILE:-/var/log/invoice-db-backup.log}"

log() { echo "$(date -Is) $*" | tee -a "$LOG_FILE" >&2; }

mkdir -p "$(dirname "$LOG_FILE")"

# Single instance only.
exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  log "[SKIP] another backup run is in progress"
  exit 0
fi

command -v rclone >/dev/null 2>&1 || { log "[ERROR] rclone not found"; exit 1; }
[[ -f "$RCLONE_CONFIG" ]] || { log "[ERROR] rclone config not found: $RCLONE_CONFIG"; exit 1; }

# 1) Write a consistent database snapshot (VACUUM INTO), safe to copy even
#    while the app is running and writing to the live db.
rm -rf "$BACKUP_HOST_DIR"
mkdir -p "$BACKUP_HOST_DIR"
cd "$PROJECT_DIR"
docker compose run --rm invoiceapp -backupdb "$BACKUP_CONTAINER_PATH" >/dev/null
log "[OK] Wrote consistent database snapshot"

# 2) Include the small in-progress draft JSON alongside it, if present.
if [[ -f "$DATA_HOST_DIR/invoice-data.json" ]]; then
  cp "$DATA_HOST_DIR/invoice-data.json" "$BACKUP_HOST_DIR/invoice-data.json"
fi

# 3) Upload as a dated snapshot so a bad backup never overwrites a good one.
DATE_TAG="$(date -u +%Y-%m-%dT%H-%M)"
rclone --config "$RCLONE_CONFIG" copy "$BACKUP_HOST_DIR" "$REMOTE/db-backups/$DATE_TAG" \
  --transfers 4 --checkers 8
log "[OK] Uploaded database backup -> $REMOTE/db-backups/$DATE_TAG"

# 4) Drop snapshots older than the retention window to bound Drive usage.
rclone --config "$RCLONE_CONFIG" delete "$REMOTE/db-backups" --min-age "${RETENTION_DAYS}d" >/dev/null 2>&1 || true
rclone --config "$RCLONE_CONFIG" rmdirs "$REMOTE/db-backups" --leave-root >/dev/null 2>&1 || true

# 5) Mirror the raw evidence photos too (covers photos never manually sent
#    to Drive via the app's "Enviar a Google Drive" button).
if [[ -d "$MINIO_DIR" ]]; then
  rclone --config "$RCLONE_CONFIG" copy "$MINIO_DIR" "$REMOTE/minio-data" \
    --create-empty-src-dirs --transfers 4 --checkers 8
  log "[OK] Synced MinIO data: $MINIO_DIR -> $REMOTE/minio-data"
else
  log "[WARN] MinIO directory not found, skipping: $MINIO_DIR"
fi

log "[DONE] Database backup complete"
