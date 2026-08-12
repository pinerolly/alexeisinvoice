#!/usr/bin/env bash
set -euo pipefail

SRC_DIR="${SRC_DIR:-/srv/invoice/data}"
MINIO_DIR="${MINIO_DIR:-/srv/invoice/minio}"
REMOTE="${GDRIVE_REMOTE:-gdrive:alexeisinvoice-backups}"
RCLONE_CONFIG_FILE="${GDRIVE_RCLONE_CONFIG:-/srv/invoice/secrets/rclone.conf}"
INCLUDE_MINIO="${GDRIVE_BACKUP_INCLUDE_MINIO:-true}"
LOCK_FILE="${LOCK_FILE:-/tmp/invoice-gdrive-sync.lock}"
LOG_FILE="${LOG_FILE:-/var/log/invoice-gdrive-sync.log}"
ENABLED_RAW="${GDRIVE_BACKUP_ENABLED:-false}"

mkdir -p "$(dirname "$LOG_FILE")"

exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  exit 0
fi

lower_enabled="$(echo "$ENABLED_RAW" | tr '[:upper:]' '[:lower:]')"
if [[ "$lower_enabled" != "1" && "$lower_enabled" != "true" && "$lower_enabled" != "yes" ]]; then
  echo "$(date -Is) [INFO] Google Drive backup is disabled (set GDRIVE_BACKUP_ENABLED=true to enable)." >> "$LOG_FILE"
  exit 0
fi

if [[ ! -d "$SRC_DIR" ]]; then
  echo "$(date -Is) [ERROR] Source directory not found: $SRC_DIR" >> "$LOG_FILE"
  exit 1
fi

if ! command -v rclone >/dev/null 2>&1; then
  echo "$(date -Is) [ERROR] rclone is not installed" >> "$LOG_FILE"
  exit 1
fi

if [[ ! -f "$RCLONE_CONFIG_FILE" ]]; then
  echo "$(date -Is) [ERROR] rclone config not found: $RCLONE_CONFIG_FILE" >> "$LOG_FILE"
  exit 1
fi

export RCLONE_CONFIG="$RCLONE_CONFIG_FILE"

rclone copy "$SRC_DIR" "$REMOTE/invoice-data" --create-empty-src-dirs --transfers 4 --checkers 8
echo "$(date -Is) [OK] Synced invoice data: $SRC_DIR -> $REMOTE/invoice-data" >> "$LOG_FILE"

lower_minio="$(echo "$INCLUDE_MINIO" | tr '[:upper:]' '[:lower:]')"
if [[ "$lower_minio" == "1" || "$lower_minio" == "true" || "$lower_minio" == "yes" ]]; then
  if [[ -d "$MINIO_DIR" ]]; then
    rclone copy "$MINIO_DIR" "$REMOTE/minio-data" --create-empty-src-dirs --transfers 4 --checkers 8
    echo "$(date -Is) [OK] Synced MinIO data: $MINIO_DIR -> $REMOTE/minio-data" >> "$LOG_FILE"
  else
    echo "$(date -Is) [WARN] MinIO directory not found, skipping: $MINIO_DIR" >> "$LOG_FILE"
  fi
fi
