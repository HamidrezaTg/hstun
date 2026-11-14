#!/usr/bin/env bash
# Interoperability of this build with an older release: a new exit with an old entry and an old exit with a new entry,
# on every carrier, each moving 1 MB through the tunnel and back. Padding, cookies, the browser ClientHello, session
# rotation and the capability stream all have to degrade quietly against a peer that does not know them.
#
#   HSTUN_OLD=/path/to/older/hstun scripts/test-compat.sh        (needs the new binary: scripts/package.sh, or go build)
#
# No root, nothing outside a temporary directory. Skipped when HSTUN_OLD is not set.
set -euo pipefail
cd "$(dirname "$0")/.."
[ -n "${HSTUN_OLD:-}" ] && [ -x "$HSTUN_OLD" ] || { echo "test-compat: set HSTUN_OLD to an older hstun binary (skipped)"; exit 0; }
if G="$(mise where go@1.26 2>/dev/null)" && [ -x "$G/bin/go" ]; then export PATH="$G/bin:$PATH"; fi
T=$(mktemp -d); pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$T"; }
trap cleanup EXIT
NEW="$T/hstun-new"
go build -mod=vendor -o "$NEW" ./cmd/hstun
echo "new: $("$NEW" version), old: $("$HSTUN_OLD" version)"
base=$((20000 + RANDOM % 20000))
ok() { echo "ok   $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout "$T/key.pem" -out "$T/cert.pem" -days 2 \
  -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" 2>/dev/null
"$NEW" keygen -out "$T/secret" >/dev/null
cat >"$T/echo.py" <<PY
import asyncio
async def h(r, w):
    try:
        while True:
            d = await r.read(65536)
            if not d: break
            w.write(d); await w.drain()
    finally: w.close()
async def main():
    s = await asyncio.start_server(h, "127.0.0.1", $((base + 1)))
    async with s: await s.serve_forever()
asyncio.run(main())
PY
python3 "$T/echo.py" & pids+=($!)
cat >"$T/rt.py" <<'PY'
import os, socket, sys, threading
port, n = int(sys.argv[1]), int(sys.argv[2])
want = os.urandom(n)
s = socket.create_connection(("127.0.0.1", port), timeout=30)
threading.Thread(target=lambda: s.sendall(want), daemon=True).start()
got = b""
while len(got) < n:
    d = s.recv(65536)
    if not d: break
    got += d
sys.exit(0 if got == want else 1)
PY

# $1 = exit binary, $2 = entry binary, $3 = carrier, $4 = label
scenario() {
  local xbin=$1 ebin=$2 carrier=$3 label=$4 i port fwd
  i=$((i_next++)); port=$((base + 10 + i * 4)); fwd=$((port + 1))
  cat >"$T/s$i.toml" <<EOF
[server]
role         = "exit"
listen       = "127.0.0.1:$port"
tls_cert     = "$T/cert.pem"
tls_key      = "$T/key.pem"
carriers     = ["ws", "http"]
ws_path      = "/q1/ws$i"
http_path    = "/q1/h$i"
secret_files = ["$T/secret"]
[[forward]]
name   = "x1"
target = "127.0.0.1:$((base + 1))"
EOF
  cat >"$T/c$i.toml" <<EOF
[client]
role        = "entry"
server      = "127.0.0.1:$port"
carrier     = "$carrier"
ws_path     = "/q1/ws$i"
http_path   = "/q1/h$i"
conns       = 2
secret_file = "$T/secret"
name        = "compat"
tls_verify  = "ca_file"
ca_file     = "$T/cert.pem"
[[forward]]
name   = "x1"
listen = "127.0.0.1:$fwd"
EOF
  "$xbin" server -config "$T/s$i.toml" >"$T/s$i.log" 2>&1 & local sp=$!; pids+=("$sp")
  sleep 0.7
  "$ebin" client -config "$T/c$i.toml" >"$T/c$i.log" 2>&1 & local cp=$!; pids+=("$cp")
  local up=0
  for _ in $(seq 1 40); do
    if python3 "$T/rt.py" "$fwd" 2048 2>/dev/null; then up=1; break; fi
    sleep 0.5
  done
  [ "$up" = 1 ] || { cat "$T/s$i.log" "$T/c$i.log" | tail -20; fail "$label ($carrier): no traffic"; }
  python3 "$T/rt.py" "$fwd" 1048576 || { tail -20 "$T/s$i.log" "$T/c$i.log"; fail "$label ($carrier): 1 MB did not come back intact"; }
  # a second burst after a pause: the capability stream and padding must not disturb a running session
  sleep 2; python3 "$T/rt.py" "$fwd" 300000 || fail "$label ($carrier): second transfer failed"
  kill "$cp" "$sp" 2>/dev/null || true; wait "$cp" "$sp" 2>/dev/null || true
  ok "$label, carrier $carrier"
}
i_next=0
for c in ws http chunk; do
  scenario "$NEW" "$HSTUN_OLD" "$c" "new exit, old entry"
  scenario "$HSTUN_OLD" "$NEW" "$c" "old exit, new entry"
done
echo "test-compat: all passed"
