# Handover — 2026-11-05

Read this first, then `direct/README.md` (the live path) and `README.md` (GCS design).
Secrets are not in this public repo: the phone UUID is in `/root/entry.json` on Moscow,
and the GCS key is at `/home/zim/gcstun/gcs-key.json` here. The July history and the
reasoners Worker UUID are in `~/.claude/projects/-mnt-agent/memory/project_russia_vpn.md`.

## What is live right now (checked 2026-11-05)

The phone (Shadowrocket) connects with VLESS TCP to Moscow `176.113.82.126:2053`. From
there, two routes:

- **direct (the default):** WireGuard 10.67.0.1 ↔ 10.67.0.2 to `direct-socks.py` on this
  machine. The UK exit IP is 88.98.202.76.
- **Russian site names:** these go to `ru-fallback.py` on Moscow, which tries the site
  from Moscow first and falls back to the UK.

| Box | Service | State |
|---|---|---|
| UK (this VM, no sudo, or `echo zim\|sudo -S`) | `gcstun-direct-socks` | active |
| | `wg-exit` interface | up, ping to 10.67.0.1 ≈ 48 ms. **But `wg-quick@wg-exit` shows `failed`**, so a reboot may not bring it back. Fix first. |
| | `gcstun-relay` (GCS muxrelay) | active, still polling GCS even though the phone no longer uses it |
| Moscow (`ssh root@176.113.82.126`) | `xray-entry`, `ru-fallback` | active |
| | `gcsmuxcli` (:10921), `gcscli` (:10920), `xhcli` (:10901) | active (old fallbacks, all still polling) |
| | `wg-quick@wg-exit` | inactive per systemd, though the tunnel answers |
| Contabo `169.58.27.100` (vps.reasoners.org) | old gcsrelay, cycrelay, Caddy, REALITY | paid monthly; not checked this session |

Test results for the GCS path, 2026-11-02 (`TEST-RESULTS-2026-11-02.md`):

- Every check passed: 160 Mbit/s through the tunnel against 0 B direct to Hetzner, plus a 10-minute soak.
- The reasoners.org Cloudflare Worker is throttled from Moscow: it stalls at about 6 KB.
- That makes GCS a proven fallback if the direct path breaks.

## Next tasks, in order

1. **Finish the deploy.**
   - Make `wg-quick@wg-exit` show active and survive a reboot on both boxes. The interface
     currently exists outside the unit, so the unit has to be reconciled with it.
   - Prove the phone path end to end: run a temp xray client against :2053 and check that
     ifconfig.me shows the UK IP and yandex.ru/internet shows Moscow.
   - Check UDP (YouTube, WhatsApp calls) and the gosuslugi fallback. Read `/var/log/ru-fallback.log`.
   - Decide whether GCS stays a hot standby or gets stopped.
   - Commit any config changes, scrubbed, to `direct/`.
2. **Cost audit.** Stop only what costs money. Keep reasoners restartable: don't delete the
   Worker, the DNS, or the Contabo data.
   - GCS: idle `muxclient`, `gcscli` and `muxrelay` poll every 40–60 ms, which means paid
     Class A/B operations around the clock. Measure them in the `cyclevpn` billing and
     metrics. If they're material, stop `gcscli` and `xhcli` on Moscow (those are dead
     paths). Ask before stopping the mux pair, because it's the fallback.
   - Contabo: a monthly fee. Only cancelling in the Contabo panel stops billing, and that's
     the user's action. First snapshot what's needed to rebuild it (configs into the repo,
     scrubbed). The Cloudflare Worker is on the free tier, so leave it.
   - Report what costs what (£/month) and what you stopped.
3. **Traffic/domains report.** The user asked "is it having good traffic, what domains".
   - xray access logging is off (0 matches for `access` in entry.json). Enabling it means
     editing `/root/entry.json` and restarting `xray-entry`, which drops the phone for
     about 1 s. That's fine, but say so.
   - Until then, use `/var/log/ru-fallback.log`, `wg show` transfer counters, and
     `ss`/conntrack on `direct-socks`.
   - Give top domains and bytes, without logging the user's content beyond what's needed.
     Turn logging back down after.
4. **Fragility.** The exit is this sandbox VM behind a home router. If it's off, the VPN
   is dead. Note options; don't build anything without asking.

## Hard rules

- Never run a second GCS relay or mux client on `cyclevpn-xport-eu`; use
  `cyclevpn-xport-test`.
- Never print `gcs-key.json`, private WireGuard keys, or UUIDs into the repo. The repo
  is public.
- Don't redeploy or edit the Cloudflare Worker without asking.
- Emails only via `~/.claude/skills/send-email/send_email.py`, subject prefix `[gcstun]`.
  The QR recipe is in `GROK-CONTEXT.md`.
- Push every commit to GitHub (`fiveapplesonthetable/gcstun`, `main`).
