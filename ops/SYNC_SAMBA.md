# Invoice Data Sync to Samba

Use this to keep runtime data on local disk for SQLite safety while syncing a backup copy to the Samba project path.

## Files

- Script: scripts/sync_invoice_data_to_samba.sh
- Service template: ops/invoice-data-sync.service
- Timer template: ops/invoice-data-sync.timer

## Install on host

Run as root on the host where Docker runs:

cp /mnt/nas-projects/my-work/all-work/invoice/ops/invoice-data-sync.service /etc/systemd/system/invoice-data-sync.service
cp /mnt/nas-projects/my-work/all-work/invoice/ops/invoice-data-sync.timer /etc/systemd/system/invoice-data-sync.timer
systemctl daemon-reload
systemctl enable --now invoice-data-sync.timer

## Verify

systemctl status invoice-data-sync.timer
systemctl list-timers | grep invoice-data-sync
journalctl -u invoice-data-sync.service --since "10 min ago"

## Manual run

SRC_DIR=/srv/invoice/data DST_DIR=/mnt/nas-projects/my-work/all-work/invoice/data/invoice /mnt/nas-projects/my-work/all-work/invoice/scripts/sync_invoice_data_to_samba.sh
