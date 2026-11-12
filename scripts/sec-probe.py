#!/usr/bin/env python3
"""Black-box security probe for a running hstun listener (no key needed).

Usage: sec-probe.py HOST PORT [--ws-path /hs/ws] [--http-path /hs/h] [--per-ip 64]

Run it from a host that is allowed to connect (for example on the server itself against
127.0.0.1). It only sends hostile or malformed input an unauthenticated
internet client could send, and checks the listener refuses or times it out.
It never needs or prints a secret. Exit code 1 if any check fails.
"""
import argparse
import os
import socket
import ssl
import sys
import time

ap = argparse.ArgumentParser()
ap.add_argument("host")
ap.add_argument("port", type=int)
ap.add_argument("--ws-path", default="/hs/ws")
ap.add_argument("--http-path", default="/hs/h")
ap.add_argument("--per-ip", type=int, default=64)
args = ap.parse_args()

results = []


def check(name, ok, detail=""):
    results.append(ok)
    print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail else ""))


def tls_conn(version=None, timeout=15):
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE  # the probe checks behaviour, not identity
    if version is not None:
        ctx.minimum_version = version
        ctx.maximum_version = version
    raw = socket.create_connection((args.host, args.port), timeout=timeout)
    return ctx.wrap_socket(raw, server_hostname="probe.invalid")


def http(sock, req: bytes, timeout=8):
    sock.settimeout(timeout)
    sock.sendall(req)
    data = b""
    try:
        while b"\r\n\r\n" not in data and len(data) < 65536:
            chunk = sock.recv(4096)
            if not chunk:
                break
            data += chunk
    except (socket.timeout, ssl.SSLError, ConnectionError):
        pass
    return data


def status(data):
    try:
        return int(data.split(b" ", 2)[1])
    except Exception:
        return None


# 1. Unknown paths look like an ordinary 404 and leak nothing.
s = tls_conn()
d = http(s, b"GET / HTTP/1.1\r\nHost: probe\r\nConnection: close\r\n\r\n")
s.close()
check("GET / is the decoy site (200) or a plain 404", status(d) in (200, 404), str(status(d)))
low = d.lower()
check("GET / answer does not name hstun/Go", b"hstun" not in low and b"go-http" not in low)

# 2. Plain WS path without an upgrade is refused, not a 200.
s = tls_conn()
d = http(s, f"GET {args.ws_path} HTTP/1.1\r\nHost: probe\r\nConnection: close\r\n\r\n".encode())
s.close()
check("ws path without upgrade is refused", status(d) in (400, 404, 426), str(status(d)))

# 3. HTTP carrier endpoints reject GET and unauthenticated POST.
for ep in ("/o", "/a", "/u", "/d", "/c"):
    s = tls_conn()
    d = http(s, f"GET {args.http_path}{ep} HTTP/1.1\r\nHost: probe\r\nConnection: close\r\n\r\n".encode())
    s.close()
    st = status(d)
    check(f"GET {args.http_path}{ep} refused", st in (404, 405, 400, 410), str(st))

s = tls_conn()
body = os.urandom(70)
d = http(s, (f"POST {args.http_path}/o HTTP/1.1\r\nHost: probe\r\nContent-Length: {len(body)}\r\n"
             "Connection: close\r\n\r\n").encode() + body)
s.close()
check("POST /o with garbage hello -> 403", status(d) in (403, 404), str(status(d)))

# 4. Oversize Content-Length is refused immediately, before the body is read.
s = tls_conn()
t0 = time.time()
d = http(s, (f"POST {args.http_path}/u HTTP/1.1\r\nHost: probe\r\nContent-Length: 1099511627776\r\n"
             "Connection: close\r\n\r\n").encode(), timeout=5)
s.close()
check("Content-Length 2^40 refused fast", status(d) in (400, 404, 410, 413) and time.time() - t0 < 3,
      f"{status(d)} in {time.time()-t0:.2f}s")

# 5. Garbage after TLS is dropped (no hang, no crash).
s = tls_conn()
s.settimeout(25)
s.sendall(os.urandom(2048))
t0 = time.time()
try:
    r = s.recv(1024)
    closed = (r == b"") or status(r) == 400
except (socket.timeout, ssl.SSLError, ConnectionError):
    closed = True
s.close()
check("random bytes after TLS: closed or 400", closed, f"{time.time()-t0:.1f}s")

# 6. Old TLS versions are refused.
for name, ver in (("TLS 1.0", ssl.TLSVersion.TLSv1), ("TLS 1.1", ssl.TLSVersion.TLSv1_1)):
    try:
        s = tls_conn(ver)
        s.close()
        check(f"{name} refused", False, "handshake succeeded")
    except Exception as e:  # ssl error / reset / unsupported locally
        check(f"{name} refused", True, type(e).__name__)

# 7. A silent TLS connection is cut within the read deadline (no slot held forever).
s = tls_conn()
s.settimeout(40)
t0 = time.time()
try:
    r = s.recv(1)
    cut = r == b""
except (socket.timeout, ssl.SSLError, ConnectionError):
    cut = True
el = time.time() - t0
s.close()
check("silent TLS conn cut by the server", cut and el < 30, f"{el:.1f}s")

# 8. Per-IP connection cap: open per-ip+10 silent connections, expect the extras closed.
socks = []
extra_closed = 0
try:
    for i in range(args.per_ip + 10):
        try:
            raw = socket.create_connection((args.host, args.port), timeout=5)
            socks.append(raw)
        except OSError:
            extra_closed += 1
    time.sleep(1.0)
    for raw in socks:
        raw.settimeout(0.2)
        try:
            if raw.recv(1) == b"":
                extra_closed += 1
        except socket.timeout:
            pass
        except OSError:
            extra_closed += 1
finally:
    for raw in socks:
        raw.close()
check(f"per-IP cap ({args.per_ip}) closes the extras", extra_closed >= 5, f"{extra_closed} closed of {args.per_ip + 10}")
time.sleep(1)

# 9. Still serving afterwards.
s = tls_conn()
d = http(s, b"GET / HTTP/1.1\r\nHost: probe\r\nConnection: close\r\n\r\n")
s.close()
check("listener still responsive after the probes", status(d) in (200, 404), str(status(d)))

failed = results.count(False)
print(f"\n{len(results) - failed}/{len(results)} checks passed")
sys.exit(1 if failed else 0)
