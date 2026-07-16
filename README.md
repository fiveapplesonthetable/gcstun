# gcstun

A VPN transport that defeats Russia's TSPU throttle by tunnelling through **Google
Cloud Storage** — a destination the throttle *whitelists* and lets through at full
speed. The two servers never talk to each other directly; they only ever read and
write objects in a GCS bucket, so there is nothing for the throttle to choke.

This is a different approach from connection-cycling (see the `cyclevpn` project). It
trades a bit of latency for **~10× the throughput**, which makes it excellent for
video and downloads.

## Two transports

gcstun has two modes. **Prefer `mux`.**

- **`muxclient` / `muxrelay` (recommended, default in production).** *Multiplexed:* ALL
  connections share one pair of object streams (`up/<seq>`, `down/<seq>`), so the GCS
  round-trip latency is amortised across every connection at once. 2–3× faster and more
  consistent for the many-connection workloads (YouTube, page loads). This is what the
  wire-protocol and how-it-works sections below describe under "mux".
- **`client` / `relay` (per-session, simpler).** One object-stream *per* connection
  (`req/<sid>`, `up/<sid>/<seq>`, `down/<sid>/<seq>`). Easier to reason about, but each
  connection pays the ~1s GCS round-trip on its own. Kept as a fallback.

> **Note on the two schemes:** the per-session mode announces each connection with a
> separate `req/<sid>` object; the mux mode has **no `req/`** — opening a connection is
> an OPEN *frame* folded into `up/`. See the wire protocol below.

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

**Mux vs per-session, many parallel connections** (the YouTube case — measured, 15
parallel requests):

| | per-session | **mux** |
|---|---|---|
| 15 parallel connections | 7–12 s, inconsistent | **~3.8 s, every time** |
| single-connection reliability | flaky under some timing | **solid** |

The mux batches all connections into one GCS round-trip, so the per-request latency is
shared instead of paid per connection — 2–3× faster for real browsing/video.

## Cost

GCS charges for **egress** (data read down to the RU box): ~**$0.12/GB**. Storage is
negligible (objects are deleted seconds after use); operations are negligible with the
default MB-sized chunks. New Google Cloud accounts get **$300 free for 90 days**.
Ballpark: light use pennies–$1/mo, heavy daily video a few $/mo. (Cloudflare R2 has
*free* egress and may be a cheaper vehicle if its domain is whitelisted — untested.)

---

## Detailed how-to (reproduce from scratch)

You need two servers — an **entry** inside Russia (small/cheap is fine) and an **exit**
outside Russia — plus a Google Cloud project. The phone talks to the entry with a
stock VLESS app; only the entry↔exit hop uses GCS.

### 1. Google Cloud: project, billing, key, bucket

In the console (or `gcloud`):

1. Create a project, e.g. `cyclevpn`, and **link a billing account** (Storage refuses
   to work without one; new accounts get $300 free credit).
2. Enable the **Cloud Storage API**.
3. **Service account:** IAM & Admin → Service Accounts → Create (`gcstun`) → grant role
   **Storage Admin** → Keys → Add key → **JSON**. Download it.
4. **Bucket:** create one in a region near both boxes, e.g. `europe-west3` (Frankfurt):
   ```bash
   gcloud storage buckets create gs://<your-bucket> --location=EUROPE-WEST3
   ```
5. **Lifecycle rule (cost safety):** auto-delete leftover objects so a crash can't
   accumulate storage. Save as `lifecycle.json` and apply:
   ```json
   {"rule":[{"action":{"type":"Delete"},"condition":{"age":1}}]}
   ```
   ```bash
   gcloud storage buckets update gs://<your-bucket> --lifecycle-file=lifecycle.json
   ```
6. Copy the key JSON to **both** servers as `/root/gcs-key.json` and lock it down:
   ```bash
   chmod 600 /root/gcs-key.json    # never commit this file
   ```

### 2. Build

```bash
GOOS=linux GOARCH=amd64 go build -o gcstun-linux .
scp gcstun-linux root@<exit>:/root/gcstun
scp gcstun-linux root@<entry>:/root/gcstun
```

> The systemd units below use the per-session `relay`/`client` modes for clarity. For
> the faster **mux** transport, just swap the subcommand: `muxrelay` on the exit and
> `muxclient` on the entry (same flags). Use one *or* the other on a given bucket, not
> both. (If two services share `/root/gcstun`, give each its own copy of the binary —
> replacing a file a running process holds fails with `ETXTBSY`.)

### 3. Exit box (outside Russia) — the relay

```bash
cat >/etc/systemd/system/gcsrelay.service <<'UNIT'
[Unit]
Description=gcstun relay (GCS exit)
After=network-online.target
[Service]
ExecStart=/root/gcstun relay -key /root/gcs-key.json -bucket <your-bucket>
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT
systemctl enable --now gcsrelay
```

### 4. Entry box (inside Russia) — the client + xray

```bash
cat >/etc/systemd/system/gcscli.service <<'UNIT'
[Unit]
Description=gcstun client (GCS entry)
After=network-online.target
[Service]
ExecStart=/root/gcstun client -key /root/gcs-key.json -bucket <your-bucket> -listen 127.0.0.1:10920
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT
systemctl enable --now gcscli
```

Then point your standard **xray VLESS entry** (the thing your phone connects to) at the
client. Its outbound is a SOCKS outbound to `127.0.0.1:10920`:

```json
{
  "inbounds":[{"listen":"0.0.0.0","port":2053,"protocol":"vless",
    "settings":{"clients":[{"id":"<your-uuid>"}],"decryption":"none"},
    "streamSettings":{"network":"tcp","security":"none"}}],
  "outbounds":[{"protocol":"socks",
    "settings":{"servers":[{"address":"127.0.0.1","port":10920}]}}]
}
```

The phone's `vless://…@<entry-ip>:2053?…` link (and its QR) references only the entry —
**it does not change** when you swap the exit or switch entry→GCS.

### 5. Verify

```bash
# on the entry box:
curl -x socks5h://127.0.0.1:10920 https://example.com -o /dev/null -w '%{http_code}\n'
# a bigger download shows the throughput:
curl -x socks5h://127.0.0.1:10920 https://speed.cloudflare.com/__down?bytes=50000000 -o /dev/null -w '%{speed_download}\n'
```

Flags: `-window` (concurrent prefetch/upload), `-chunk` (bytes per downstream object),
`-poll` (GCS poll interval), `-debug` (logs produced MB/s and read/flush wait split).

---

## Wire protocol — exactly which objects, when, and who deletes them

Everything is negotiated through objects in the one bucket. There is **no side channel
and no direct connection** between the two boxes.

### Object namespace

Each SOCKS connection gets a random 18-hex-char session id `sid`. Four object kinds:

| Object | Written by | Read by | Meaning |
|---|---|---|---|
| `req/<sid>` | entry (client) | exit (relay) | session open; body is `"host:port"` |
| `up/<sid>/<seq>` | entry | exit | upstream chunk (app → destination), `seq` = 0,1,2… |
| `down/<sid>/<seq>` | exit | entry | downstream chunk (destination → app), `seq` = 0,1,2… |
| `close/<sid>` | entry | exit | teardown signal |

Every `up`/`down` chunk is **`[1 flag byte][data]`**. Flag bit `0x1` set = **last chunk**
in that direction (client half-closed its write side / destination hit EOF).

### The negotiation, step by step

1. **Open.** The entry picks a random `sid`, writes `req/<sid>` = `"youtube.com:443"`,
   and immediately answers the app's SOCKS request with success. The exit is *listing*
   the `req/` prefix every ~60 ms; it sees `req/<sid>`, reads it, **deletes `req/<sid>`**
   (so it's processed once), and dials the destination. — *The only place anything is
   ever listed/searched. Everything below uses computed names, no searching.*

2. **Upstream (app → destination).** The entry writes the app's bytes as
   `up/<sid>/0`, `up/<sid>/1`, … The exit reads them **in order** by computed name —
   it GETs `up/<sid>/<seq>`, and if it 404s (not written yet) polls every ~60 ms until
   it appears — writes the bytes to the destination, and **deletes each `up` object
   right after consuming it**. A chunk with the last-flag set makes the exit half-close
   the destination's write side, then stop the upstream.

3. **Downstream (destination → app).** The exit reads the destination, accumulating up
   to `-chunk` bytes (but flushing within 100 ms so small replies aren't delayed),
   and writes `down/<sid>/0`, `down/<sid>/1`, … concurrently. The entry pulls them with
   a **prefetch window** (several `down/<sid>/<seq>` GETs in flight at once, polling on
   404), writes them to the app **in order**, and **deletes each `down` object right
   after consuming it**. The last-flagged chunk (destination EOF) ends the stream.

4. **Close.** When the app's connection ends (either side), the entry writes
   `close/<sid>`. The exit has a watcher GETting `close/<sid>` every 500 ms; on seeing
   it, it **deletes `close/<sid>`**, closes the destination socket, and tears the
   session down.

### Deletion / cleanup summary

- `req/<sid>` — deleted by the **exit** the instant it reads it.
- `up/<sid>/<seq>` — deleted by the **exit** as each is consumed.
- `down/<sid>/<seq>` — deleted by the **entry** as each is consumed.
- `close/<sid>` — deleted by the **exit** when it acts on it.
- **Backstop:** the bucket lifecycle rule (`age: 1` day) deletes anything a crash left
  behind, so storage never accumulates and cost stays near zero.

So in steady state the bucket holds only the handful of chunks currently in flight
(the prefetch/write window), and everything else has already been deleted by whoever
consumed it. Object names are deterministic (`sid` + direction + sequence), so neither
side ever has to search for "where the next file is" — it computes the name.

### Multiplexed (mux) protocol — the recommended mode

The mux mode collapses all connections onto **one** pair of object streams and has **no
`req/` and no per-connection objects**:

- `up/<seq>` — entry → exit, a **batch of frames** for many connections.
- `down/<seq>` — exit → entry, a batch of frames.

Each frame is `streamID(4) | type(1) | len(4) | data`, where type is **OPEN** (data =
`"host:port"`), **DATA** (raw bytes), or **CLOSE**. So instead of a separate `req/`
object, a new connection is just an **OPEN frame** at the front of its stream, riding in
`up/` alongside everyone else's data:

```
up/42:  [5|OPEN|"youtube.com:443"]  [6|OPEN|"wikipedia.org:443"]  [5|DATA|<bytes>] ...
```

- The entry gives each connection a unique **stream id** and batches all streams' bytes
  (coalesced ~15 ms, or sooner past a size cap) into the next `up/<seq>`.
- The exit reads `up/<seq>` in order, demuxes, dials on OPEN, writes DATA to the right
  socket; responses are batched back into `down/<seq>`.
- The entry reads `down/<seq>` (with a parallel prefetch window for throughput),
  demuxes, and delivers each frame **only to the connection whose stream id is on it**.

Because the whole batch is one GCS round-trip, the ~0.4 s Storage latency is paid once
for every active connection at once. And because a frame is delivered strictly by stream
id, many phones behind one entry (one bucket, same QR) never see each other's traffic.
Batch writes retry until success, and both ends read the sequence in strict order, so
the shared numbering stays in lockstep. (One entry per bucket — two separate entry boxes
would both number from `up/0` and collide.)

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
