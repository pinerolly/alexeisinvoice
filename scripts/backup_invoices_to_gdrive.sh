#!/usr/bin/env bash
set -euo pipefail

# Export every invoice as a PDF (reusing the app's own renderer) and mirror the
# PDFs to Google Drive. The Drive folder ends up containing ONLY the invoice
# PDFs, one per invoice, named "<id>_<client>_<date>.pdf".

PROJECT_DIR="${PROJECT_DIR:-/home/ubuntu/docker/alexeisinvoice}"
EXPORT_HOST_DIR="${EXPORT_HOST_DIR:-/srv/invoice/data/pdf-export}"
EXPORT_CONTAINER_DIR="${EXPORT_CONTAINER_DIR:-/data/pdf-export}"
RCLONE_CONFIG="${RCLONE_CONFIG:-/home/ubuntu/.config/rclone/rclone.conf}"
REMOTE="${REMOTE:-gdrive:Invoices}"
LOCK_FILE="${LOCK_FILE:-/tmp/invoice-gdrive-backup.lock}"
LOG_FILE="${LOG_FILE:-/var/log/invoice-gdrive-backup.log}"

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

# 1) Render all invoices to PDFs via the app's export mode.
rm -rf "$EXPORT_HOST_DIR"
cd "$PROJECT_DIR"
docker compose run --rm invoiceapp -exportpdfs "$EXPORT_CONTAINER_DIR" >/dev/null
count=$(find "$EXPORT_HOST_DIR" -maxdepth 1 -name '*.pdf' | wc -l)
log "[OK] Rendered $count invoice PDF(s)"

# 2) Mirror to Drive (removes PDFs for deleted invoices, uploads new/changed).
rclone --config "$RCLONE_CONFIG" sync "$EXPORT_HOST_DIR" "$REMOTE" \
  --transfers 8 --checkers 8 --include '*.pdf'
log "[OK] Mirrored PDFs -> $REMOTE"

# 3) Clean up local export.
rm -rf "$EXPORT_HOST_DIR"
log "[DONE] Backup complete"
