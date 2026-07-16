#!/usr/bin/env bash
# gcstun EXIT (relay) installer. The exit is OUTBOUND-ONLY — no public IP, no inbound
# ports — so it runs on ANY machine with internet: a cloud box, or a home server / VM
# behind NAT. Its egress IP becomes the VPN exit.
#
#   ./install-exit.sh <gcs-key.json> [bucket] [install-dir]
#
# One relay per bucket (the multiplexed sequence assumes a single reader). To switch
# exits, stop the old relay before starting a new one on the same bucket. To run several
# exits at once, give each its own bucket (see README "Adding / switching exit nodes").
set -euo pipefail

KEY="${1:?usage: install-exit.sh <gcs-key.json> [bucket] [install-dir]}"
BUCKET="${2:-cyclevpn-xport-eu}"
DIR="${3:-$HOME/gcstun}"
HERE="$(cd "$(dirname "$0")" && pwd)"
BIN="$HERE/gcstun-linux"
[ -f "$BIN" ] || { echo "no gcstun-linux — build it: GOOS=linux GOARCH=amd64 go build -o gcstun-linux ." >&2; exit 1; }

SUDO=""; [ "$(id -u)" != 0 ] && SUDO="sudo"

mkdir -p "$DIR"
install -m0755 "$BIN" "$DIR/gcstun"
install -m0600 "$KEY" "$DIR/gcs-key.json"

$SUDO tee /etc/systemd/system/gcstun-relay.service >/dev/null <<UNIT
[Unit]
Description=gcstun mux relay (GCS exit)
After=network-online.target
Wants=network-online.target
[Service]
User=$(whoami)
ExecStart=$DIR/gcstun muxrelay -key $DIR/gcs-key.json -bucket $BUCKET
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
UNIT

$SUDO systemctl daemon-reload
$SUDO systemctl enable --now gcstun-relay
sleep 2

IP="$(curl -s --max-time 8 https://api.ipify.org || echo '?')"
echo "=================================================================="
echo " EXIT node up on bucket '$BUCKET'  (service: gcstun-relay)"
echo "   status:  systemctl status gcstun-relay"
echo "   the VPN will now exit from this machine's IP: $IP"
echo "=================================================================="
