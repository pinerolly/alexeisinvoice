# Invoice Backup to Google Drive

Use this to keep runtime data on local disk while copying backups to Google Drive.

## Files

- Script: scripts/sync_invoice_data_to_gdrive.sh
- Service template: ops/invoice-gdrive-backup.service
- Timer template: ops/invoice-gdrive-backup.timer

## Data sources

- Invoice data: /srv/invoice/data
- MinIO data (optional): /srv/invoice/minio

## Configure on VPS

1. Install rclone.
2. Create rclone config file at /srv/invoice/secrets/rclone.conf.
3. Add these settings to /srv/invoice/secrets/invoice.env:

GDRIVE_BACKUP_ENABLED=true
GDRIVE_REMOTE=gdrive:alexeisinvoice-backups
GDRIVE_RCLONE_CONFIG=/srv/invoice/secrets/rclone.conf
GDRIVE_BACKUP_INCLUDE_MINIO=true

4. Configure remote with:

rclone config

5. Verify remote access with:

RCLONE_CONFIG=/srv/invoice/secrets/rclone.conf rclone lsd gdrive:

## Install and enable manually (if needed)

cp /mnt/nas-projects/my-work/all-work/invoice/ops/invoice-gdrive-backup.service /etc/systemd/system/invoice-gdrive-backup.service
cp /mnt/nas-projects/my-work/all-work/invoice/ops/invoice-gdrive-backup.timer /etc/systemd/system/invoice-gdrive-backup.timer
systemctl daemon-reload
systemctl enable --now invoice-gdrive-backup.timer

## Verify

systemctl status invoice-gdrive-backup.timer
systemctl list-timers | grep invoice-gdrive-backup
journalctl -u invoice-gdrive-backup.service --since "30 min ago"
tail -n 100 /var/log/invoice-gdrive-sync.log
