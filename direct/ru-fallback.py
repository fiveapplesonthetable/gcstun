#!/usr/bin/env python3
"""Russian-exit SOCKS. Dial direct; on connect failure or silence, replay via the UK tunnel."""
import os
import select
import socket
import threading
import time

LISTEN = ("127.0.0.1", 11081)
UK = ("10.67.0.2", 11080)
CONNECT_TIMEOUT = 4
FIRST_BYTE = 5
MAX_REPLAY = 32768
LOG_PATH = "/var/log/ru-fallback.log"

threading.stack_size(256 * 1024)


def log(msg):
    line = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()) + " " + msg
    print(line, flush=True)
    try:
        if os.path.exists(LOG_PATH) and os.path.getsize(LOG_PATH) > 1_000_000:
            os.replace(LOG_PATH, LOG_PATH + ".1")
        with open(LOG_PATH, "a", encoding="utf-8") as fh:
            fh.write(line + "\n")
    except OSError:
        pass


def recvall(conn, n):
    buf = b""
    while len(buf) < n:
        chunk = conn.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("short read")
        buf += chunk
    return buf


def reason(exc):
    text = str(exc).lower()
    if isinstance(exc, socket.timeout) or "timed out" in text:
        return "timeout"
    if "refused" in text:
        return "refused"
    if "unreachable" in text:
        return "unreachable"
    if "name or service" in text or "nodename" in text or "temporary failure" in text:
        return "dns"
    return type(exc).__name__


def dial_direct(host, port):
    infos = socket.getaddrinfo(host, port, socket.AF_INET, socket.SOCK_STREAM)
    if not infos:
        raise OSError("no ipv4")
    fam, typ, proto, _, addr = infos[0]
    sock = socket.socket(fam, typ, proto)
    sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    sock.settimeout(CONNECT_TIMEOUT)
    try:
        sock.connect(addr)
    except Exception:
        sock.close()
        raise
    sock.settimeout(None)
    return sock


def dial_uk(host, port):
    sock = socket.create_connection(UK, timeout=8)
    sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    sock.settimeout(8)
    sock.sendall(b"\x05\x01\x00")
    if recvall(sock, 2) != b"\x05\x00":
        sock.close()
        raise OSError("uk auth")
    try:
        addr = socket.inet_aton(host)
        req = b"\x05\x01\x00\x01" + addr
    except OSError:
        raw = host.encode()
        if len(raw) > 255:
            raise OSError("name too long")
        req = b"\x05\x01\x00\x03" + bytes([len(raw)]) + raw
    sock.sendall(req + port.to_bytes(2, "big"))
    resp = recvall(sock, 4)
    if resp[1] != 0:
        sock.close()
        raise OSError("uk rep %s" % resp[1])
    atyp = resp[3]
    if atyp == 1:
        recvall(sock, 6)
    elif atyp == 3:
        recvall(sock, recvall(sock, 1)[0] + 2)
    elif atyp == 4:
        recvall(sock, 18)
    else:
        sock.close()
        raise OSError("uk atyp")
    sock.settimeout(None)
    return sock


def send_pending(sock, buf):
    if not buf:
        return
    n = sock.send(buf)
    del buf[:n]


def pump(client, remote, host, port, replay):
    """Copy bytes. While the site has not answered, a hang replays through the UK."""
    client.setblocking(False)
    remote.setblocking(False)
    to_remote = bytearray()
    to_client = bytearray()
    started = time.monotonic()
    watching = True
    tried = False
    while True:
        if watching and not tried and time.monotonic() - started >= FIRST_BYTE:
            tried = True
            remote.close()
            try:
                remote = dial_uk(host, port)
            except Exception as exc:
                log("fallback %s:%s reason=no-first-byte uk=%s" % (host, port, reason(exc)))
                return
            remote.setblocking(False)
            to_remote = bytearray(replay)
            watching = False
            log("fallback %s:%s reason=no-first-byte via=uk" % (host, port))
        timeout = 0.5
        if watching:
            timeout = max(0.05, min(0.5, FIRST_BYTE - (time.monotonic() - started)))
        writers = []
        if to_remote:
            writers.append(remote)
        if to_client:
            writers.append(client)
        try:
            readable, writable, _ = select.select([client, remote], writers, [], timeout)
        except (OSError, ValueError):
            return
        if remote in writable:
            try:
                send_pending(remote, to_remote)
            except OSError:
                if watching and not tried:
                    tried = True
                    continue
                return
        if client in writable:
            try:
                send_pending(client, to_client)
            except OSError:
                return
        if remote in readable:
            try:
                data = remote.recv(65536)
            except OSError:
                data = b""
            if not data:
                if watching and not tried and not to_client:
                    tried = True
                    started = 0  # force the fallback branch
                    continue
                return
            to_client.extend(data)
            watching = False
        if client in readable:
            try:
                data = client.recv(65536)
            except OSError:
                return
            if not data:
                return
            to_remote.extend(data)
            if watching:
                if len(replay) + len(data) <= MAX_REPLAY:
                    replay.extend(data)
                else:
                    watching = False


def splice(client, remote):
    client.setblocking(False)
    remote.setblocking(False)
    pair = [client, remote]
    while True:
        try:
            ready, _, _ = select.select(pair, [], [], 180)
        except (OSError, ValueError):
            return
        if not ready:
            return
        for sock in ready:
            try:
                data = sock.recv(65536)
            except OSError:
                return
            if not data:
                return
            other = remote if sock is client else client
            try:
                other.setblocking(True)
                other.sendall(data)
                other.setblocking(False)
            except OSError:
                return


def handle(client):
    remote = None
    replied = False
    try:
        ver, nmethods = recvall(client, 2)
        recvall(client, nmethods)
        client.sendall(b"\x05\x00")
        ver, cmd, _rsv, atyp = recvall(client, 4)
        if ver != 5 or cmd != 1:
            client.sendall(b"\x05\x07\x00\x01" + b"\x00" * 6)
            replied = True
            return
        if atyp == 1:
            host = socket.inet_ntoa(recvall(client, 4))
        elif atyp == 3:
            host = recvall(client, recvall(client, 1)[0]).decode()
        elif atyp == 4:
            host = socket.inet_ntop(socket.AF_INET6, recvall(client, 16))
        else:
            client.sendall(b"\x05\x08\x00\x01" + b"\x00" * 6)
            replied = True
            return
        port = int.from_bytes(recvall(client, 2), "big")
        why = None
        try:
            remote = dial_direct(host, port)
        except Exception as exc:
            why = reason(exc)
            remote = None
        if remote is None:
            try:
                remote = dial_uk(host, port)
            except Exception as exc:
                log("fallback %s:%s reason=%s uk=%s" % (host, port, why, reason(exc)))
                client.sendall(b"\x05\x05\x00\x01" + b"\x00" * 6)
                replied = True
                return
            log("fallback %s:%s reason=%s via=uk" % (host, port, why))
            client.sendall(b"\x05\x00\x00\x01" + b"\x00" * 6)
            replied = True
            splice(client, remote)
            return
        client.sendall(b"\x05\x00\x00\x01" + b"\x00" * 6)
        replied = True
        pump(client, remote, host, port, bytearray())
    except Exception:
        if not replied:
            try:
                client.sendall(b"\x05\x05\x00\x01" + b"\x00" * 6)
            except Exception:
                pass
    finally:
        client.close()
        if remote is not None:
            try:
                remote.close()
            except Exception:
                pass


def main():
    server = socket.socket()
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind(LISTEN)
    server.listen(256)
    log("listening %s:%s" % LISTEN)
    while True:
        client, _addr = server.accept()
        threading.Thread(target=handle, args=(client,), daemon=True).start()


if __name__ == "__main__":
    main()
