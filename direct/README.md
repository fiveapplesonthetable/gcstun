# Current path: direct WireGuard exit

This folder is what is actually running. The GCS tunnel in the parent README is the
previous transport. It is still in the repo. Traffic does not use it.

The phone profile did not change. Shadowrocket still speaks VLESS to Moscow
`176.113.82.126:2053`. The client UUID is not in this repo.

```
phone  --VLESS TCP-->  Moscow xray :2053
                          |  Russian site name
                          |---> ru-fallback.py  127.0.0.1:11081
                          |       try the site from Moscow
                          |       if that hangs, open it through the UK SOCKS
                          |
                          |  everything else, including UDP
                          +--WireGuard-->  direct-socks.py  10.67.0.2:11080
                                             dials the site from the UK address
```

Sites see `88.98.202.76` for the UK exit and `176.113.82.126` for a Russian site that
answered directly. A normal "what is my IP" page is not a Russian name, so it shows
the UK address. `https://yandex.ru/internet/` shows the Moscow address.

## Why WireGuard and a SOCKS program

WireGuard is only the pipe. Moscow is `10.67.0.1`, this machine is `10.67.0.2`, UDP
port `51821` on Moscow. This machine dials Moscow, because the home router does not
forward inbound ports. A keepalive every 25 seconds keeps the router's NAT mapping
alive while the tunnel is idle. Real traffic is not delayed by that timer. Moscow
learns the home address from the packets, so a new public IP is picked up on the next
packet.

`direct-socks.py` is what opens the website. It speaks SOCKS5 on `10.67.0.2:11080`
and does both TCP CONNECT and UDP ASSOCIATE. DNS for those connections happens on
the UK machine. YouTube video and WhatsApp calls are UDP. Before UDP ASSOCIATE
existed, those packets reached this program and were refused. YouTube then waited
about half a minute and retried the video over TCP, which is why a video eventually
played. UDP is accepted now.

## Russian sites

xray matches the site name against the geosite lists shipped with xray (`tld-ru`,
`category-ru`, the bank, media, shop, and travel lists, plus `yandex` and `vk`).
A match goes to `ru-fallback.py`. That program tries a direct connection from
Moscow for 4 seconds, and waits 5 seconds for the first byte. If Moscow cannot
open it, the same bytes are sent through the UK SOCKS and a line is written to
`/var/log/ru-fallback.log`.

Gosuslugi does not answer from the Moscow VPS. The fallback sends it through the
UK, so the page loads after those few seconds. The feed of names is not a perfect
test: an address inside Russia is the wrong signal, because Google also has
machines there. The name is what decides.

## Files

| File | Where it runs |
|---|---|
| `direct-socks.py` | UK, `/home/zim/gcstun/direct-socks.py`, service `gcstun-direct-socks` |
| `ru-fallback.py` | Moscow, `/root/ru-fallback.py`, service `ru-fallback` |
| `entry.json` | Moscow, `/root/entry.json`, service `xray-entry`. `<UUID>` stands in for the client id |
| `wg-exit.uk.conf.example` | UK, `/etc/wireguard/wg-exit.conf` |
| `wg-exit.moscow.conf.example` | Moscow, `/etc/wireguard/wg-exit.conf` |
| `*.service` | the systemd units |

Private keys are not here. The two WireGuard public keys are.

Moscow also allows `51821/udp`. The old phone tunnel on `51820` and the `nzela`
service were stopped. The GCS relay and mux client can still be installed. They are
not on this path.
