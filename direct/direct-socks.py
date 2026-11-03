#!/usr/bin/env python3
"""SOCKS5 exit for the Moscow tunnel. TCP CONNECT and UDP ASSOCIATE. DNS happens here."""
import select
import socket
import threading
import time

LISTEN = ("10.67.0.2", 11080)
IDLE = 180


def recvall(conn, n):
    buf = b""
    while len(buf) < n:
        chunk = conn.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("short read")
        buf += chunk
    return buf


def read_addr(conn, atyp):
    if atyp == 1:
        host = socket.inet_ntoa(recvall(conn, 4))
    elif atyp == 3:
        host = recvall(conn, recvall(conn, 1)[0]).decode()
    elif atyp == 4:
        host = socket.inet_ntop(socket.AF_INET6, recvall(conn, 16))
    else:
        return None
    port = int.from_bytes(recvall(conn, 2), "big")
    return host, port


def fail(client, code):
    try:
        client.sendall(b"\x05" + bytes([code]) + b"\x00\x01" + b"\x00" * 6)
    except Exception:
        pass


def tcp_connect(client, host, port):
    remote = None
    try:
        remote = socket.create_connection((host, port), timeout=20)
        remote.settimeout(None)
        client.sendall(b"\x05\x00\x00\x01" + b"\x00" * 6)
        pair = [client, remote]
        while True:
            ready, _, _ = select.select(pair, [], [], IDLE)
            if not ready:
                return
            for sock in ready:
                data = sock.recv(65536)
                if not data:
                    return
                other = remote if sock is client else client
                other.sendall(data)
    finally:
        client.close()
        if remote is not None:
            remote.close()


def parse_udp(pkt):
    # SOCKS5 UDP request: RSV RSV FRAG ATYP ADDR PORT DATA. Fragments are not accepted.
    if len(pkt) < 7 or pkt[2] != 0:
        return None
    atyp = pkt[3]
    i = 4
    try:
        if atyp == 1:
            host = socket.inet_ntoa(pkt[i:i + 4])
            i += 4
        elif atyp == 3:
            n = pkt[i]
            i += 1
            host = pkt[i:i + n].decode()
            i += n
        elif atyp == 4:
            host = socket.inet_ntop(socket.AF_INET6, pkt[i:i + 16])
            i += 16
        else:
            return None
        port = int.from_bytes(pkt[i:i + 2], "big")
        i += 2
    except (IndexError, OSError, UnicodeError, ValueError):
        return None
    if port == 0:
        return None
    return host, port, pkt[i:]


def pack_udp(ip, port, data):
    try:
        addr = b"\x01" + socket.inet_aton(ip)
    except OSError:
        addr = b"\x04" + socket.inet_pton(socket.AF_INET6, ip)
    return b"\x00\x00\x00" + addr + port.to_bytes(2, "big") + data


def resolve(host, cache):
    if host in cache:
        return cache[host]
    infos = socket.getaddrinfo(host, 0, socket.AF_UNSPEC, socket.SOCK_DGRAM)
    if not infos:
        return None
    fam, _, _, _, sockaddr = infos[0]
    chosen = (fam, sockaddr[0])
    cache[host] = chosen
    return chosen


def forward_from_client(relay, v4, v6, cache, client_ep):
    while True:
        try:
            pkt, ep = relay.recvfrom(65535)
        except BlockingIOError:
            return client_ep
        if client_ep is None:
            client_ep = ep
        if ep != client_ep:
            continue
        parsed = parse_udp(pkt)
        if not parsed:
            continue
        host, port, payload = parsed
        try:
            fam, ip = resolve(host, cache)
        except socket.gaierror:
            continue
        out = v4 if fam == socket.AF_INET else v6
        if out is None:
            continue
        try:
            out.sendto(payload, (ip, port))
        except OSError:
            continue


def forward_replies(sock, relay, client_ep):
    while True:
        try:
            data, (ip, port) = sock.recvfrom(65535)
        except BlockingIOError:
            return
        if client_ep is None:
            continue
        try:
            relay.sendto(pack_udp(ip, port, data), client_ep)
        except OSError:
            return


def udp_associate(client):
    relay = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    v4 = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    v6 = None
    try:
        relay.bind((LISTEN[0], 0))
        port = relay.getsockname()[1]
        client.sendall(
            b"\x05\x00\x00\x01"
            + socket.inet_aton(LISTEN[0])
            + port.to_bytes(2, "big")
        )
        try:
            v6 = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM)
            v6.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
        except OSError:
            v6 = None
        outs = [v4] + ([v6] if v6 is not None else [])
        for sock in [relay] + outs:
            sock.setblocking(False)
        client.setblocking(False)
        client_ep = None
        cache = {}
        last = time.monotonic()
        while True:
            timeout = IDLE - (time.monotonic() - last)
            if timeout <= 0:
                return
            ready, _, _ = select.select([client, relay] + outs, [], [], timeout)
            if not ready:
                return
            if client in ready:
                try:
                    if client.recv(65536) == b"":
                        return
                except BlockingIOError:
                    pass
            if relay in ready:
                client_ep = forward_from_client(relay, v4, v6, cache, client_ep)
            for sock in outs:
                if sock in ready:
                    forward_replies(sock, relay, client_ep)
            last = time.monotonic()
    finally:
        client.close()
        relay.close()
        v4.close()
        if v6 is not None:
            v6.close()


def handle(client):
    try:
        ver, nmethods = recvall(client, 2)
        recvall(client, nmethods)
        client.sendall(b"\x05\x00")
        ver, cmd, _rsv, atyp = recvall(client, 4)
        if ver != 5:
            fail(client, 1)
            client.close()
            return
        target = read_addr(client, atyp)
        if target is None:
            fail(client, 8)
            client.close()
            return
        host, port = target
        if cmd == 1:
            tcp_connect(client, host, port)
            return
        if cmd == 3:
            udp_associate(client)
            return
        fail(client, 7)
        client.close()
    except Exception:
        fail(client, 5)
        try:
            client.close()
        except Exception:
            pass


def main():
    server = socket.socket()
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind(LISTEN)
    server.listen(256)
    while True:
        client, _ = server.accept()
        threading.Thread(target=handle, args=(client,), daemon=True).start()


if __name__ == "__main__":
    main()
