#!/usr/bin/env bash
# gcstun ENTRY (client) installer. Runs on the box inside Russia. It's a SOCKS5 proxy
# that a standard xray VLESS inbound forwards to; it reads/writes the GCS bucket only
# (whitelisted), never contacting the exit directly.
#
#   ./install-entry.sh <gcs-key.json> [bucket] [socks-listen] [install-dir]
#
# After running, point your xray VLESS inbound's SOCKS *outbound* at the listen address.
# The phone's vless:// QR references only the entry box, so it never changes when you
# swap or add exits.
set -euo pipefail

KEY="${1:?usage: install-entry.sh <gcs-key.json> [bucket] [socks-listen] [install-dir]}"
BUCKET="${2:-cyclevpn-xport-eu}"
LISTEN="${3:-127.0.0.1:10921}"
DIR="${4:-/root/gcstun}"
HERE="$(cd "$(dirname "$0")" && pwd)"
BIN="$HERE/gcstun-linux"
[ -f "$BIN" ] || { echo "no gcstun-linux — build it: GOOS=linux GOARCH=amd64 go build -o gcstun-linux ." >&2; exit 1; }

SUDO=""; [ "$(id -u)" != 0 ] && SUDO="sudo"

mkdir -p "$DIR"
install -m0755 "$BIN" "$DIR/gcstun"
install -m0600 "$KEY" "$DIR/gcs-key.json"

$SUDO tee /etc/systemd/system/gcsmuxcli.service >/dev/null <<UNIT
[Unit]
Description=gcstun mux client (GCS entry)
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=$DIR/gcstun muxclient -key $DIR/gcs-key.json -bucket $BUCKET -listen $LISTEN
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
UNIT

$SUDO systemctl daemon-reload
$SUDO systemctl enable --now gcsmuxcli
sleep 2

echo "=================================================================="
echo " ENTRY node up on bucket '$BUCKET'  (service: gcsmuxcli, SOCKS $LISTEN)"
echo "   Now point your xray VLESS inbound's SOCKS outbound at $LISTEN:"
echo '     "outbounds":[{"protocol":"socks","settings":{"servers":[{"address":"127.0.0.1","port":10921}]}}]'
echo "   Then restart xray. The phone QR does not change."
echo "=================================================================="
