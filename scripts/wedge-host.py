#!/usr/bin/env python3
"""Hold half-dead connections against a live hstun server and watch that a
legitimate request through its SOCKS port stays fast.

Usage: wedge-host.py TUNNEL_HOST:PORT SOCKS_HOST:PORT [count] [seconds]
Kinds: silent TCP, TLS then silent, partial HTTP request (slowloris),
oversize Content-Length. It never sends a valid handshake.
"""
import socket, ssl, sys, time, threading, statistics

tunnel, socks = sys.argv[1], sys.argv[2]
count = int(sys.argv[3]) if len(sys.argv) > 3 else 300
secs = int(sys.argv[4]) if len(sys.argv) > 4 else 60
th, tp = tunnel.rsplit(":", 1); tp = int(tp)
sh, sp = socks.rsplit(":", 1); sp = int(sp)
ctx = ssl.create_default_context(); ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE
held = []

def attack(kind):
    try:
        s = socket.create_connection((th, tp), 5)
        if kind != "silent":
            s = ctx.wrap_socket(s, server_hostname=th)
        if kind == "slowloris":
            s.sendall(b"GET /hs/ws HTTP/1.1\r\nHost: x\r\nX-A: ")
        elif kind == "oversize":
            s.sendall(b"POST /hs/h/u HTTP/1.1\r\nHost: x\r\nContent-Length: 1099511627776\r\nX-Hs-Sid: 00000000000000000000000000000000\r\n\r\n")
        held.append(s)
    except Exception:
        pass

def legit():
    s = socket.create_connection((sh, sp), 10); s.settimeout(15)
    s.sendall(b"\x05\x01\x00"); s.recv(2)
    host = b"api.ipify.org"
    s.sendall(b"\x05\x01\x00\x03" + bytes([len(host)]) + host + (443).to_bytes(2, "big"))
    r = s.recv(10)
    if r[1] != 0: raise RuntimeError("socks reply %d" % r[1])
    t = ssl.create_default_context().wrap_socket(s, server_hostname="api.ipify.org")
    t.sendall(b"GET / HTTP/1.1\r\nHost: api.ipify.org\r\nConnection: close\r\n\r\n")
    t.recv(512); t.close()

kinds = ["silent", "tls", "slowloris", "oversize"]
ths = [threading.Thread(target=attack, args=(kinds[i % 4],)) for i in range(count)]
[t.start() for t in ths]; [t.join() for t in ths]
print("holding %d half-dead connections" % len(held))
lat = []; end = time.time() + secs
while time.time() < end:
    t0 = time.time()
    try:
        legit(); lat.append(time.time() - t0)
    except Exception as e:
        print("legit request FAILED:", e); lat.append(99)
    time.sleep(2)
lat.sort()
print("legit latency: median %.2fs p99 %.2fs over %d requests" % (statistics.median(lat), lat[int(len(lat) * 0.99) - 1], len(lat)))
for s in held:
    try: s.close()
    except Exception: pass
