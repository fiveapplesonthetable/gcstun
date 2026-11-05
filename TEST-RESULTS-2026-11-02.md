# gcstun test results — 2026-11-02

Window: 19:22–19:40 UTC. Production services were not restarted, stopped, or edited.
No second mux client or relay was started. `gcs-key.json` was not printed.
Temp xray clients and configs were removed. Nothing was committed or pushed.
No new Shadowrocket mail was sent: the Moscow gcstun link was already mailed today,
and the Worker link still cannot carry bulk from this box.

| # | Check | Result |
|---|---|---|
| 1 | VLESS :2053 full path | **PASS** |
| 2 | Hetzner throttle proof | **PASS** (tunnel completes; direct does not) |
| 3 | 10-minute soak on :10921 | **PASS** |
| 4 | `up/` backlog drained | **PASS** |
| 5 | Worker bulk from Moscow | **FAIL** — worker is healthy; the Russian path is not |

## 1. Full phone path through xray :2053 — PASS

From this machine (outside Russia):

`nc -zv 176.113.82.126 2053` → connection succeeded.

Throwaway xray client (config in `/tmp`, deleted after) to
`176.113.82.126:2053`, UUID from the handover, TCP, no TLS:

| Where the client ran | Exit IP via ifconfig.me | google.com |
|---|---|---|
| Entry box, dialing its own public :2053 | 88.98.202.76 | HTTP 200, 87,051 B, 1.17 s |
| This machine | 88.98.202.76 | HTTP 200, 86,957 B, 1.29 s |

On the entry, youtube.com through the same client was HTTP 200, 951,974 B, 1.49 s.
Direct (no tunnel) ifconfig from the entry is 176.113.82.126. SOCKS `127.0.0.1:10921`
alone also exits as 88.98.202.76.

xray on the entry is the long-lived process `/root/xray/xray run -c /root/entry.json`
(pid 71346, up since Jul 16). There is no active systemd unit named `xray`. Inbound
is VLESS, listen `0.0.0.0:2053`, TCP, `security=none`, `decryption=none`, one user,
the handover UUID. SOCKS outbound is `127.0.0.1:10921`.

## 2. Throttle proof — PASS

`https://speed.hetzner.de/100MB.bin` is not a usable control. It does not resolve
from the entry, from this machine, or as a name the relay can connect to
(tunnel attempt: TLS `SSL_ERROR_SYSCALL` in 0.38 s, 0 B). The name is dead, not throttled.

`https://ash-speed.hetzner.com/100MB.bin` (100,000,000 B is actually 104,857,600 B):

| Path from the entry | Result |
|---|---|
| Direct, cap 25 s | TCP timeout, **0 B**, curl exit 28 |
| SOCKS `127.0.0.1:10921`, cap 120 s | HTTP 200, **104,857,600 B in 5.26 s**, 19,951,564 B/s (19.95 MB/s, 160 Mbit/s) |

README's "~16 KB then dead" pattern was not what happened. Direct never delivered
a byte; the handshake did not complete. The claim that matters did: the foreign
host is unusable straight from the Moscow box, and the same file finishes through
the tunnel.

## 3. Soak — PASS

19:26:37Z–19:36:40Z, through `127.0.0.1:10921` only. Each iteration fetched
google.com, youtube.com, and `gstatic.com/generate_204`, then one 50,000,000 B
object from `speed.cloudflare.com`.

First 10 minutes were one download per minute (19:26 through 19:35). In the last
minute the loop stopped padding to 60 s, so downloads 11–19 ran back to back
(19:35:50–19:36:40). All of them succeeded. That tail is extra load, not a stall.

- Pages: 19/19 google HTTP 200 (0.86–1.60 s), 19/19 youtube HTTP 200 (1.21–1.58 s),
  19/19 gstatic HTTP 204 (0.71–1.08 s). No error bodies.
- 50 MB downloads: 19/19 HTTP 200, exactly 50,000,000 B.
  Times 2.30–3.53 s. Speed **14.4–21.7 MB/s** (14,438,540–21,700,852 B/s).
  Last one was 2.43 s at 20.6 MB/s. No slowdown across the hour-tenth.
- `journalctl -u gcsmuxcli --since '2026-11-02 19:26:37'`: no lines.
  No resync, no per-stream gap. `gcsmuxcli` stayed `active`.
- `journalctl -u gcstun-relay` over the same window: no lines. The unit is not
  running with `-debug`, so a resync would not be logged. The process did not
  restart. `gcstun-relay` stayed `active`.
- `up/` count sampled once a minute: 0, except one sample of a single in-flight
  object (`up/622520`, then `up/622539` a minute later). That is the live sequence,
  not a pile. `down/` was 0–2 objects. Both were 0 again at 19:36:42Z.

Before this soak, the only mux-client log line today was the expected resync when
the relay came back at 19:06:31Z (`down resync 78603 -> 0`).

## 4. Backlog — PASS

Listed `cyclevpn-xport-eu` with the GCS JSON API (paginated `prefix=up/` and
`prefix=down/`), using the local service-account key. Counts only; the key was
not printed.

| When | `up/` | `down/` |
|---|---|---|
| 19:22Z, before these tests | 0 | 0 |
| During soak | 0 or 1 (in flight) | 0–2 (in flight) |
| 19:36:42Z, soak finished | 0 | 0 |
| After the Worker tests | 0 | 0 |

The earlier >1000 `up/` pile is gone.

## 5. Cloudflare Worker `gate.reasoners.org` — FAIL (bulk from Moscow)

Client-side only. The Worker was not redeployed or edited. Temp xray on the entry
listened on `127.0.0.1:18082` and was killed afterwards. Not redone: byedpi/zapret,
ECH, domain fronting, XHTTP.

### Baseline from the entry (same shape as Claude's earlier run)

| Request | Result |
|---|---|
| `gstatic.com/generate_204` | HTTP 204, 0.86 s |
| ifconfig.me | `2a09:bac5:2a94:2482::3a3:14` (Cloudflare), 0.51 s |
| google.com, cap 20 s | HTTP 200, **5,921 B** then timeout (page is ~87 KB) |
| Chrome deb via Worker, cap 20 s | HTTP 200, **5,921 / 145,633,696 B** in 20.0 s (296 B/s) |
| Same deb, direct | HTTP 200, **full 145,633,696 B in 9.41 s**, 15.5 MB/s |

A 12 s byte curve of the same deb through the Worker stalled by ~1.8 s at ~4–6 KB
and ended at **exactly 5,921 B**. Waiting does not add bytes. The cliff is ~6 KB
on this path, not the 16–26 KB quota the note expected.

12 parallel connections, 8 s each: sizes
`10665, 5921, 5921, 10665, 0, 5921, 9479, 5921, 0, 1177, 5921, 8293`.
Total **69,884 B**, max 10,665 B. Extra connections do not add up to a useful pipe.

### Worker health from outside Russia (this machine)

Same VLESS+WS+TLS client. The Worker is fine. The first full-file attempt died
once at 94.9 MB / 6.8 s (`curl` exit 18, partial). The immediate retry completed:

| Hostname | Chrome deb (145,633,696 B) |
|---|---|
| `gate.reasoners.org` | full file, 8.09 s, 18.0 MB/s. gstatic 204 in 0.16 s. Exit `2a09:bac5:3802:2696::3d8:1d` |
| `vpngate.reasoners-edge.workers.dev` | full file, 5.31 s, 27.4 MB/s |

So a slow Worker is not the cause.

### Client-side attempts from the Moscow box (12 s bulk each)

| Variant | Bytes | What happened |
|---|---|---|
| `vpngate.reasoners-edge.workers.dev`, chrome fp | 2,363 | Still throttled. Worse than `gate.reasoners.org`. |
| fragment `tlshello` 10–30 / 10–20 ms | 3,549 | Arrived only at the end of the 12 s. Still stalled. |
| fragment `tlshello` 50–100 / 5–10 ms | 0 | TCP timeout. |
| fragment packets `1-3`, length 100–200 | 4,735 | Plateau ~4 KB at 1.5 s. |
| fragment `tlshello` length 1–5 / interval 1–5 | 0 | TCP timeout. |
| noise only (`rand` 10–20 B, delay 10–16 ms) | 8,293 | Best of the set. Plateau ~8 KB at 0.75 s, then dead. |
| fragment `tlshello` 10–20 + noise, firefox fp | 0 | TCP timeout. |
| fingerprint firefox, no fragment | 0 | Timeout, 0 B. |
| fingerprint safari | 0 | Timeout, 0 B. |
| fingerprint randomized | 0 | TCP reset at 8 s. |

Chrome is the only fingerprint that reliably gets the ~6 KB burst. None of the
fragment, noise, or fingerprint changes produced bulk. Noise-only's 8 KB is not
a usable improvement over 6 KB.

These configs were checked from outside first: `tlshello` 10–30 and packets `1-3`
both moved the Chrome deb at >15 MB/s, and noise-only completed the file at
18 MB/s. The Moscow failures are the path, not a broken client config.
`randomized` also failed from outside (reset), so that fingerprint is a bad client,
not a Russia result.

### Suggested next step

Do not point a phone at `gate.reasoners.org` for bulk from this evidence, and do
not mail that QR. The RuVDS datacenter path is heavily filtered; a normal Russian
ISP might still differ, and that is the one test this box cannot do. Another
round of byedpi, ECH, domain fronting, or XHTTP on this host is not worth it —
those already failed here. gcstun is the path that carries bulk today.

## Left alone on purpose

- `gcstun-relay`, `gcsmuxcli`, `/root/entry.json`, the GCS worker script.
- Pre-existing xray processes that were already running before these tests:
  `/root/testclient.json`, two `/root/testsocks.json`, `/root/xh-client.json`,
  and the production `entry.json`.

## Small doc nits (not changed)

- `speed.hetzner.de` does not resolve. The working control is
  `https://ash-speed.hetzner.com/100MB.bin`.
- `gcstun-relay` has no `-debug`, so a future resync will not show up in the
  journal. The client unit already has `-debug`.
