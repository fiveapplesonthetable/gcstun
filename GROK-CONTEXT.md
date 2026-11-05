# gcstun — handover context for Grok

> **Stale (2026-11-05):** written 2026-11-02, when GCS was the live path. The phone now goes over the direct WireGuard exit (`direct/README.md`). Start with `HANDOVER.md`.

You are taking over testing/operating **gcstun**, a VPN transport that tunnels through
Google Cloud Storage to get past Russia's TSPU throttling. Repo: this directory
(`~/dev/gcstun`, GitHub `fiveapplesonthetable/gcstun`). Read `README.md` for the design
and wire protocol. This file is the live state as of 2026-11-02.

## How it works (one paragraph)

Phone (Shadowrocket) → VLESS → **Moscow entry box** → gcstun `muxclient` writes the
traffic as objects `up/<seq>` into GCS bucket `cyclevpn-xport-eu` → **exit relay on this
machine** (`muxrelay`) reads them, dials the real website, writes the replies back as
`down/<seq>` objects → entry reads them → phone. The two boxes never talk to each other;
Russia only sees traffic to `storage.googleapis.com`, which it whitelists. Sites see the
exit's IP (88.98.202.76).

## Machines

| Role | Where | Access | Key pieces |
|---|---|---|---|
| Exit (relay) | this machine (`fiat-sandbox`), egress IP 88.98.202.76 | local, **no sudo** | systemd `gcstun-relay` → `/home/zim/gcstun/gcstun muxrelay -key /home/zim/gcstun/gcs-key.json -bucket cyclevpn-xport-eu` |
| Entry | `176.113.82.126` (RUVDS, Korolev, Moscow Oblast), host `ruvds-p3dyc` | `ssh root@176.113.82.126` (key `~/.ssh/id_ed25519`, already authorised) | xray `/root/xray/xray run -c /root/entry.json`: VLESS inbound TCP **:2053**, no TLS → SOCKS outbound 127.0.0.1:**10921** = `gcsmuxcli.service` (`/root/gcstun-mux2 muxclient ... -bucket cyclevpn-xport-eu -listen 127.0.0.1:10921 -debug`) |

Other things on the entry box you should know about but **not touch** unless asked:
`gcscli.service` (old per-session client on :10920), `xhcli.service` (xray XHTTP fallback
on :10901), test xray instances on :10908/:10909.

GCS: project `cyclevpn`, service account `fiveapples@cyclevpn.iam.gserviceaccount.com`.
Buckets: `cyclevpn-xport-eu` (**production**), `cyclevpn-xport-test` (scratch, objects
auto-delete after 1 day — use this for any experiments).

Phone link (also emailed to the user as a QR):
`vless://<UUID: see /root/entry.json on Moscow>@176.113.82.126:2053?encryption=none&type=tcp&security=none#gcstun-RU`

## What happened today (2026-11-02)

1. The exit relay had been **crash-looping ~41 h** (systemd 203/EXEC) because
   `/home/zim/gcstun/` had been archived away to `/mnt/agent/home-archive/gcstun/`. The
   entry kept writing; >1000 `up/` objects piled up. Rebuilt the binary from this repo
   (`go build -o gcstun-linux .`), reinstalled binary + key to `/home/zim/gcstun/`;
   systemd's auto-restart picked it up. Relay is running and draining the backlog.
2. Local loopback test (relay + client both here, on `cyclevpn-xport-test`): works —
   page 0.77 s, 10 MB at ~78 Mbit/s. 5 of 15 parallel Wikipedia hits were HTTP 429 from
   Wikipedia rate-limiting the single exit IP, not a tunnel fault.
3. From inside Russia, via the entry's SOCKS 127.0.0.1:10921:
   - exit IP seen = 88.98.202.76 ✅ (traffic really goes through the relay)
   - google.com 200 in 1.23 s, youtube.com 200 in 1.34 s
   - 50 MB Cloudflare download: **18.3 MB/s (~146 Mbit/s)**
   - direct (no tunnel) 50 MB from Cloudflare: 8.4 MB/s — Cloudflare's speed-test host
     isn't throttled right now, so it's not a good throttle control. Use a plain foreign
     VPS/Hetzner URL for that comparison instead.
   - 15 parallel youtube.com: all 200

## NOT yet verified — your job

1. **Full phone path through xray :2053.** Everything above went straight to the SOCKS
   port. Test the VLESS inbound: run a throwaway xray client (on the entry box, e.g.
   `/root/xray/xray` with a temp config in `/tmp`, or on this machine) that connects to
   `176.113.82.126:2053` with the link above, exposes a local SOCKS port, and curl through
   it. Expect exit IP 88.98.202.76. Also check from outside Russia that :2053 is reachable
   (`nc -zv 176.113.82.126 2053`).
2. **Throttle proof.** From the entry box, compare a download from a normally-throttled
   foreign host (e.g. `https://speed.hetzner.de/100MB.bin` or
   `https://ash-speed.hetzner.com/100MB.bin`) direct vs through :10921. README claims
   direct stalls at ~16 KB; through the tunnel it should complete.
3. **Soak.** 10 minutes of repeated requests through :10921 (a loop of page fetches +
   one 50 MB download per minute). Watch for stalls/desync: on the entry,
   `journalctl -u gcsmuxcli -f`; here, `journalctl -u gcstun-relay -f`. Note any
   errors, the backlog (count of `up/` objects) and throughput over time.
4. **Backlog check.** Confirm the `up/` backlog in `cyclevpn-xport-eu` has drained to
   near zero (list with the GCS API; a small helper lives in the Claude scratchpad, or
   write one using `gcs.go`'s `List`).
5. **Re-check the Cloudflare Worker proxy (`gate.reasoners.org`) from the entry box** — a
   separate, older VLESS-over-WebSocket-TLS proxy (Worker `vpngate` on the user's
   Cloudflare zone reasoners.org, UUID `<WORKER-UUID: see ~/.claude/projects/-mnt-agent/memory/project_russia_vpn.md>`, path `/`,
   SNI/Host `gate.reasoners.org`). Run a temp xray client on the box with a SOCKS inbound
   (config in `/tmp`, delete after). Claude's result 2026-11-02: small requests work
   (gstatic 204 in 0.9 s, exits via Cloudflare IPv6 2a09:bac5:…), but bulk is throttled
   dead — google.com got 5.9 KB of ~87 KB in 20 s, a big dl.google.com file got 0 B in 20 s,
   while the same file direct ran at 13 MB/s. Russia throttles by SNI: `gate.reasoners.org`
   is not whitelisted. Same as July. Note Worker limitation: it can't reach
   Cloudflare-hosted destinations (e.g. api.ipify.org) — use ifconfig.me.

   **Test AND debug it — goal: get bulk throughput through it from the Moscow box.**
   a. Re-run the baseline above (small / page / 20 s bulk, through vs direct).
   b. Check the worker itself is healthy from outside Russia (from this machine:
      same xray client config, bulk download should be fast). If it's slow here too, the
      problem is the worker, not Russia.
   c. Try the other hostname `vpngate.reasoners-edge.workers.dev` (same UUID).
   d. Try client-side DPI evasion in the xray client: a `freedom` outbound with
      `"fragment": {"packets":"tlshello","length":"10-30","interval":"10-20"}` chained via
      `sockopt.dialerProxy`; also `"noises"`; vary values. Also `fp`/uTLS fingerprints.
   e. Measure where it dies: bytes delivered before the stall (expect ~16–26 KB = the
      per-connection quota), and whether many short connections in parallel get more total.

   Already tried in July and FAILED from this box — don't redo unless something changed:
   byedpi/zapret desync (~30 strategies + TTL sweep, best 2×, still throttled); ECH
   (Russia blocks it → 0 B); domain fronting with whitelisted SNI (Cloudflare 403s);
   any `*.workers.dev`/`*.pages.dev`/other platform domain (all throttled); XHTTP
   (0 B). The RuVDS datacenter box is unusually heavily filtered, so a fix that fails
   here might still work on a phone on a normal Russian ISP — say so in results.

   Rules for the worker: **do NOT redeploy or edit the Cloudflare Worker** without asking
   the user (a bad deploy can permanently break that worker name; deploys only via
   wrangler; the old `cfworker/` deploy dir is no longer on disk). All debugging is client-side.
   Clean up any temp xray process/config on the box when done.
6. Write results to `~/dev/gcstun/TEST-RESULTS-2026-11-02.md` (pass/fail per item, numbers,
   anything broken + suggested fix). Don't commit or push unless the user asks.

## Hard rules

- **Never start a second relay or a second mux client on `cyclevpn-xport-eu`.** The
  mux protocol assumes exactly one reader on each side; a second one corrupts the
  stream for the phone. Experiments go on `cyclevpn-xport-test`.
- Don't restart, stop or edit the production services or `/root/entry.json` unless a
  test proves something is broken — and then say what you changed.
- Never print or copy the contents of `gcs-key.json` anywhere.
- No sudo on this machine. systemd `Restart=always` restarts `gcstun-relay` within 3 s
  if the process exits, so `kill <pid>` is how you restart it after replacing the binary.
- When using `&` + `wait` in shell tests, `wait` also waits for any background
  relay/client you started — wait on specific PIDs instead or the command hangs.
- The entry box has no `bc`; use `date +%s%N` arithmetic in bash or python3.

## Sending the user a Shadowrocket QR (do this when you have a link worth sending)

Make the QR from the `vless://` link and email it. The email helper only ever sends to
the user's own address, so you can't send it anywhere else by mistake.

```bash
LINK='vless://...#name'                      # the exact link to send
qrencode -s 10 -o /tmp/gcstun-01-shadowrocket-qr.png "$LINK"
printf 'Scan in Shadowrocket (+ > Scan QR), or paste:\n\n%s\n\n<what changed / test results>\n' "$LINK" > /tmp/email.txt
python3 ~/.claude/skills/send-email/send_email.py \
  --subject '[gcstun] Shadowrocket QR — <what this is>' \
  --body-file /tmp/email.txt \
  --attach /tmp/gcstun-01-shadowrocket-qr.png
tail -1 ~/.msmtp.log        # smtpstatus=250 = delivered
```

- Keep the `[gcstun]` subject prefix so it lands in the same Gmail thread.
- Current working link (already emailed 2026-11-02, gcstun via Moscow):
  `vless://<UUID: see /root/entry.json on Moscow>@176.113.82.126:2053?encryption=none&type=tcp&security=none#gcstun-RU`
- reasoners.org link (only send if your debugging actually makes bulk work from Russia):
  `vless://<WORKER-UUID: see ~/.claude/projects/-mnt-agent/memory/project_russia_vpn.md>@gate.reasoners.org:443?encryption=none&security=tls&type=ws&host=gate.reasoners.org&path=%2F&sni=gate.reasoners.org#CF-Worker-RU`
- Before sending a new link, prove it works from the Moscow box first (temp xray client
  using that link → SOCKS → curl a page and a 20 s bulk download).
- Never put passwords, private keys or `gcs-key.json` contents in an email.
