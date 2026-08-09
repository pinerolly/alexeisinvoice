#!/usr/bin/env bash
set -euo pipefail

SRC_DIR="${SRC_DIR:-/srv/invoice/data}"
DST_DIR="${DST_DIR:-/mnt/nas-projects/my-work/all-work/invoice/data/invoice}"
LOCK_FILE="${LOCK_FILE:-/tmp/invoice-data-sync.lock}"
LOG_FILE="${LOG_FILE:-/var/log/invoice-data-sync.log}"

mkdir -p "$DST_DIR"
mkdir -p "$(dirname "$LOG_FILE")"

exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  exit 0
fi

if [[ ! -d "$SRC_DIR" ]]; then
  echo "$(date -Is) [ERROR] Source directory not found: $SRC_DIR" >> "$LOG_FILE"
  exit 1
fi

if command -v rsync >/dev/null 2>&1; then
  rsync -a --delete "$SRC_DIR/" "$DST_DIR/"
else
  rm -rf "$DST_DIR"/*
  cp -a "$SRC_DIR/." "$DST_DIR/"
fi

echo "$(date -Is) [OK] Synced $SRC_DIR -> $DST_DIR" >> "$LOG_FILE"
