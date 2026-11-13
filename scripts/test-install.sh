#!/usr/bin/env bash
# Staging test of install.sh: no root, no systemd, nothing outside a temporary directory.
# Installs an exit (direct, self-signed) and an entry from the release tarball, starts both by hand,
# sends data through the tunnel, then uninstalls with --purge and checks nothing is left.
# Needs the tarball for this machine's CPU (run scripts/package.sh first, or pass HSTUN_TARBALL) and python3.
set -euo pipefail
cd "$(dirname "$0")/.."
arch=$(uname -m); case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
TARBALL="${HSTUN_TARBALL:-$(ls dist/hstun_*_linux_"$arch".tar.gz 2>/dev/null | head -1)}"
[ -f "$TARBALL" ] || { echo "test-install: no tarball (run scripts/package.sh)" >&2; exit 1; }
T=$(mktemp -d); pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$T"; }
trap cleanup EXIT
base=$((20000 + RANDOM % 20000))
export HSTUN_TEST=1 HSTUN_ETC_BASE="$T/etc" HSTUN_BIN_DIR="$T/bin" HSTUN_SHARE_DIR="$T/share" HSTUN_WWW_DIR="$T/www" HSTUN_SYSTEMD_DIR="$T/systemd" HSTUN_TARBALL="$TARBALL"
mkdir -p "$T/systemd" "$T/etc"
ok() { echo "ok   $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }

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

./install.sh exit --yes --mode direct --forward-target "127.0.0.1:$((base + 1))" --port "$base" --backend-port "$((base + 5))" >"$T/exit.log" 2>&1 || { cat "$T/exit.log"; fail "exit install"; }
ok "exit installed (direct, self-signed)"
J=$("$T/bin/hstun-ctl" show-join); [ "${J#hstun-join:}" != "$J" ] || fail "show-join"
ok "show-join"
"$T/bin/hstun" server -config "$T/etc/hstun/server.toml" >"$T/server.log" 2>&1 & pids+=($!)
sleep 1
./install.sh entry --yes --name e1 --join "$J" --secret-file "$T/etc/hstun/secret" --forward-listen "127.0.0.1:$((base + 2))" >"$T/entry.log" 2>&1 || { cat "$T/entry.log"; fail "entry install"; }
ok "entry installed from the join string"
"$T/bin/hstun" client -config "$T/etc/hstun-e1/client.toml" >"$T/client.log" 2>&1 & pids+=($!)
for _ in $(seq 1 20); do
  admin=$(sed -n 's/^admin_listen = "\(.*\)"/\1/p' "$T/etc/hstun-e1/client.toml")
  curl -s -m 2 "http://$admin/stats" 2>/dev/null | grep -q '"carrier"' && break; sleep 0.5
done
curl -s -m 2 "http://$admin/stats" | grep -q '"carrier"' || { cat "$T/client.log"; fail "no session"; }
ok "tunnel up"
python3 - "$((base + 2))" <<'PY' || fail "echo through the tunnel"
import os, socket, sys, threading
n = 2 << 20; port = int(sys.argv[1]); want = os.urandom(n)
s = socket.create_connection(("127.0.0.1", port), timeout=20)
threading.Thread(target=lambda: s.sendall(want), daemon=True).start()
got = b""
while len(got) < n:
    d = s.recv(1 << 20)
    if not d: break
    got += d
sys.exit(0 if got == want else 1)
PY
ok "2 MB echoed through the tunnel"
for n in e1 ""; do ./install.sh uninstall --yes --purge ${n:+--name "$n"} >/dev/null 2>&1 || fail "uninstall ${n:-default}"; done
[ -z "$(ls -A "$T/etc" 2>/dev/null)" ] && [ ! -e "$T/bin/hstun" ] && [ -z "$(ls -A "$T/systemd")" ] || fail "files left after --purge"
ok "uninstall --purge leaves nothing"
echo "install.sh staging test passed"
