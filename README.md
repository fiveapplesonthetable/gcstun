# gcstun

A VPN transport that defeats Russia's TSPU throttle by tunnelling through **Google
Cloud Storage** — a destination the throttle *whitelists* and lets through at full
speed. The two servers never talk to each other directly; they only ever read and
write objects in a GCS bucket, so there is nothing for the throttle to choke.

This is a different approach from connection-cycling (see the `cyclevpn` project). It
trades a bit of latency for **~10× the throughput**, which makes it excellent for
video and downloads.

---

## Why this works

We measured, from inside Russia, what TSPU throttles and what it doesn't:

| Destination | One-connection result |
|---|---|
| A normal foreign server (our exit) | **~16 KB, then dead** — and only ~2.6 new connections/sec |
| Cloudflare Workers, most VPS/CDN names | throttled the same way |
| **`storage.googleapis.com` (Google Cloud Storage)** | **172 MB @ 21 MB/s, no limit** ✅ |
| GitHub release assets, Fastly (jsdelivr) | also whitelisted |

Google's *storage* is whitelisted (its *compute*, e.g. `*.run.app`, is not). So if the
data rides GCS, the Russian box pulls it at ~24 MB/s (~190 Mbit/s) — the throttle waves
it through. The trick is that GCS is a **dropbox, not a router**: it hands out files, it
doesn't fetch websites for you. So we run a relay that deposits the web response as GCS
objects, and an entry that picks them up.

---

## Architecture

```
   phone            RU entry box (in Russia)              GCS bucket            Contabo exit box (outside RU)
  ┌────────┐  VLESS ┌──────────────────────────┐  read/write   ┌───────┐  read/write ┌──────────────────────┐
  │Shadow- │───────▶│ xray → gcstun client      │◀════════════▶│ objects│◀═══════════▶│ gcstun relay          │──▶ internet
  │ rocket │ (domestic) (SOCKS5, writes req/ +  │  storage.     │ req/  │             │ (dials the real site, │
  └────────┘         │  up/ objects, reads down/)│  googleapis   │ up/   │             │  writes down/ objects)│
                     └──────────────────────────┘  .com         │ down/ │             └──────────────────────┘
                      ▲ whitelisted, ~24 MB/s ─────────────────▶└───────┘
```

- **Neither box ever connects to the other.** The RU box only talks to
  `storage.googleapis.com` (whitelisted). The exit only talks to GCS + the open
  internet. TSPU sees only innocuous GCS traffic.
- **How the RU box tells the exit what to fetch:** it writes a tiny object
  `req/<sid>` = `"youtube.com:443"`. The exit polls GCS, sees it, and dials that
  destination. The instruction rides the same whitelisted dropbox as the data.
- **Directions:** the request (small) goes out as `up/<sid>/<seq>` objects; the
  response (big) comes back as `down/<sid>/<seq>` objects, each tagged with a
  last-chunk flag. Both sides delete objects as they consume them.

---

## What it's good (and not good) for

| Use | Verdict |
|---|---|
| **YouTube / video / large downloads** | **Excellent.** High throughput; video buffers so the latency is invisible. |
| Browsing | Works, but each round-trip goes through GCS (~1–2 s), so it feels a bit sluggish. |
| WhatsApp / Telegram **messages** | Fine (small TCP requests). |
| **Live voice/video calls (UDP)** | **No — structural.** Store-and-forward adds ~1–2 s per round-trip; a call needs <150 ms and can't buffer a conversation. Split-tunnel calls (send them direct). |

---

## Speed

Single stream, RU→GCS→Contabo, is **target-dependent** — the exit spends ~99% of its
time *waiting for the destination to send*, so throughput is bounded by how fast each
site paces data to the exit (its RTT / congestion control / load-balancing), not by the
tunnel:

| Source through the tunnel | Speed |
|---|---|
| Cloudflare | **116 Mbit/s** |
| dl.google | 42 Mbit/s |
| Hetzner | 30 Mbit/s |

So the tunnel itself does 100+ Mbit/s (≈20× the cycling tunnel); slower numbers are the
*destination* pacing that specific connection, which is outside the tunnel's control.
Raw GCS read from the RU box is ~190 Mbit/s, so GCS is never the limit.

## Cost

GCS charges for **egress** (data read down to the RU box): ~**$0.12/GB**. Storage is
negligible (objects are deleted seconds after use); operations are negligible with the
default MB-sized chunks. New Google Cloud accounts get **$300 free for 90 days**.
Ballpark: light use pennies–$1/mo, heavy daily video a few $/mo. (Cloudflare R2 has
*free* egress and may be a cheaper vehicle if its domain is whitelisted — untested.)

---

## Setup

1. **A Google Cloud project with billing enabled** and the Cloud Storage API on.
2. **A service-account key** (JSON) with the *Storage Admin* role. Put it on both
   servers as `/root/gcs-key.json` (mode 600). Never commit it.
3. **A bucket** in a region near both boxes (e.g. `europe-west3`). The code creates it
   if you use the helper, or make it in the console.
4. **Build and deploy:**
   ```bash
   GOOS=linux GOARCH=amd64 go build -o gcstun-linux .
   # exit box (outside Russia):
   ./gcstun relay  -key /root/gcs-key.json -bucket <bucket>
   # entry box (in Russia):
   ./gcstun client -key /root/gcs-key.json -bucket <bucket> -listen 127.0.0.1:10920
   ```
   Run each under systemd (`Restart=always`). Point your existing `xray` VLESS entry's
   SOCKS outbound at `127.0.0.1:10920` — the phone QR does not change.

Flags: `-window` (concurrent prefetch/upload), `-chunk` (bytes per downstream object),
`-poll` (GCS poll interval), `-debug`.

---

## How it works internally

- **client** (`client.go`): SOCKS5 server. Per connection: write `req/<sid>` (target),
  pump the app's bytes as `up/<sid>/<seq>` objects, and pull `down/<sid>/<seq>` with a
  concurrent prefetch window (writes them to the app in order). Each object is
  `[flag byte][data]`; flag bit 0 = last chunk.
- **relay** (`relay.go`): polls `req/` for new sessions, dials the destination, reads
  it into `down/` objects (accumulating up to `-chunk`, flushing within 100 ms so small
  replies are prompt, uploading concurrently), and applies `up/` objects to the
  destination (with proper TCP half-close).
- **gcs.go**: stdlib-only GCS client — service-account JWT → OAuth token, then object
  put/get/list/delete. Forces HTTP/1.1 so concurrent reads get their own connections.

Not affiliated with Google. Use responsibly and legally.
