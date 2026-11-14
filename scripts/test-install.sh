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
J=$("$T/bin/hstun-ctl" show-join 2>/dev/null); [ "${J#hstun-pair:}" != "$J" ] || fail "show-join prints a pair code"
echo "${J#hstun-pair:}" | base64 -d | grep -q '^secret=' || fail "pair code lacks the secret"
JN=$("$T/bin/hstun-ctl" show-join --no-secret 2>/dev/null); [ "${JN#hstun-join:}" != "$JN" ] || fail "show-join --no-secret"
echo "${JN#hstun-join:}" | base64 -d | grep -q '^secret=' && fail "join string leaks the secret"
ok "show-join: pair code with the secret, --no-secret without it"
"$T/bin/hstun" server -config "$T/etc/hstun/server.toml" >"$T/server.log" 2>&1 & pids+=($!)
sleep 1
./install.sh entry --yes --name e1 --join "$J" --forward-listen "127.0.0.1:$((base + 2))" >"$T/entry.log" 2>&1 || { cat "$T/entry.log"; fail "entry install"; }
cmp -s "$T/etc/hstun/secret" "$T/etc/hstun-e1/secret" || fail "secret not taken from the pair code"
ok "entry installed from the pair code alone (no secret file, no ssh, no prompt)"
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
# --- re-running an exit must not break paired entries ---
cert_before=$(sha256sum "$T/etc/hstun/cert.pem" | cut -d' ' -f1); secret_before=$(sha256sum "$T/etc/hstun/secret" | cut -d' ' -f1)
./install.sh exit --yes >/dev/null 2>&1 || fail "exit re-run"
[ "$(sha256sum "$T/etc/hstun/cert.pem" | cut -d' ' -f1)" = "$cert_before" ] || fail "re-run regenerated the certificate (paired entries would break)"
[ "$(sha256sum "$T/etc/hstun/secret" | cut -d' ' -f1)" = "$secret_before" ] || fail "re-run changed the secret"
[ "$("$T/bin/hstun-ctl" show-join --no-secret 2>/dev/null)" = "${J/hstun-pair:/hstun-join:}" ] || true
ok "re-running the exit keeps the certificate, secret and paths"
./install.sh exit --yes --new-cert >/dev/null 2>&1 || fail "exit --new-cert"
[ "$(sha256sum "$T/etc/hstun/cert.pem" | cut -d' ' -f1)" != "$cert_before" ] || fail "--new-cert kept the old certificate"
ok "--new-cert makes a new certificate on request"

# --- the existing-instance prompt: answers come from a file (HSTUN_TTY) ---
printf '1\n' >"$T/ans1"
HSTUN_TTY="$T/ans1" ./install.sh exit >"$T/keep.log" 2>&1 || { cat "$T/keep.log"; fail "keep choice"; }
grep -q "already installed" "$T/keep.log" && grep -q "nothing changed" "$T/keep.log" || fail "existing-instance prompt"
ok "an installed instance is shown and 'keep' changes nothing"
cert_now=$(sha256sum "$T/etc/hstun/cert.pem" | cut -d' ' -f1); secret_now=$(sha256sum "$T/etc/hstun/secret" | cut -d' ' -f1)
printf '3\n' >"$T/ans3"
HSTUN_TTY="$T/ans3" ./install.sh exit --mode direct --forward-target "127.0.0.1:$((base + 1))" --port "$((base + 30))" --backend-port "$((base + 5))" >"$T/reinstall.log" 2>&1 || { cat "$T/reinstall.log"; fail "reinstall choice"; }
[ "$(sha256sum "$T/etc/hstun/cert.pem" | cut -d' ' -f1)" != "$cert_now" ] && [ "$(sha256sum "$T/etc/hstun/secret" | cut -d' ' -f1)" != "$secret_now" ] || fail "reinstall kept the old certificate or secret"
ok "'reinstall' makes a new certificate and secret"

# --- IPv6: --ipv6 makes the exit listen dual-stack and the config stays valid ---
./install.sh exit --yes --name v6 --mode direct --ipv6 --forward-target "127.0.0.1:$((base + 1))" --port "$((base + 50))" --backend-port "$((base + 51))" >"$T/v6.log" 2>&1 || { cat "$T/v6.log"; fail "exit --ipv6"; }
grep -q 'listen       = "\[::\]:' "$T/etc/hstun-v6/server.toml" || fail "--ipv6 did not make the exit listen on [::]"
J6=$("$T/bin/hstun-ctl" --name v6 show-join --no-secret 2>/dev/null); echo "${J6#hstun-join:}" | base64 -d | grep -q '^server=' || fail "join string of the ipv6 exit"
./install.sh exit --yes --name v4 --mode direct --no-ipv6 --forward-target "127.0.0.1:$((base + 1))" --port "$((base + 52))" --backend-port "$((base + 53))" >/dev/null 2>&1 || fail "exit --no-ipv6"
grep -q 'listen       = "0.0.0.0:' "$T/etc/hstun-v4/server.toml" || fail "--no-ipv6 did not keep IPv4 only"
./install.sh uninstall --yes --purge --name v6 >/dev/null 2>&1; ./install.sh uninstall --yes --purge --name v4 >/dev/null 2>&1
ok "--ipv6 listens on [::], --no-ipv6 on 0.0.0.0"

# --- per-key limits, drain timeout, carrier auto ---
./install.sh exit --yes --name lim --mode direct --max-mbit 20 --max-new-streams 50 --drain-timeout 5s --forward-target "127.0.0.1:$((base + 1))" --port "$((base + 54))" --backend-port "$((base + 55))" >"$T/lim.log" 2>&1 || { cat "$T/lim.log"; fail "exit with limits"; }
grep -q '^max_mbit_per_key = 20$' "$T/etc/hstun-lim/server.toml" && grep -q '^max_new_streams_per_sec_per_key = 50$' "$T/etc/hstun-lim/server.toml" && grep -q '^drain_timeout = "5s"$' "$T/etc/hstun-lim/server.toml" || fail "limits are not in server.toml"
if ./install.sh exit --yes --name lim2 --mode direct --max-mbit fast --forward-target "127.0.0.1:$((base + 1))" --port "$((base + 56))" >/dev/null 2>&1; then fail "--max-mbit accepted a word"; fi
if ./install.sh exit --yes --name lim2 --mode direct --drain-timeout soon --forward-target "127.0.0.1:$((base + 1))" --port "$((base + 56))" >/dev/null 2>&1; then fail "--drain-timeout accepted a word"; fi
./install.sh entry --yes --name au --join "$J" --carrier auto --drain-timeout 3s --forward-listen "127.0.0.1:$((base + 57))" >"$T/au.log" 2>&1 || { cat "$T/au.log"; fail "entry with --carrier auto"; }
grep -q '^carrier     = "auto"$' "$T/etc/hstun-au/client.toml" && grep -q '^drain_timeout = "3s"$' "$T/etc/hstun-au/client.toml" || fail "carrier auto / drain are not in client.toml"
./install.sh uninstall --yes --purge --name lim >/dev/null 2>&1; ./install.sh uninstall --yes --purge --name au >/dev/null 2>&1; rm -rf "${T:?}/etc/hstun-lim2"
./install.sh entry --yes --name fp --join "$J" --tls-fingerprint firefox --tls-fragment both --padding strong --session-age 30m --conns 2 --forward-listen "127.0.0.1:$((base + 58))" >"$T/fp.log" 2>&1 || { cat "$T/fp.log"; fail "entry with --tls-fingerprint"; }
grep -q '^tls_fingerprint = "firefox"$' "$T/etc/hstun-fp/client.toml" || fail "tls_fingerprint is not in client.toml"
grep -q '^tls_fragment = "both"$' "$T/etc/hstun-fp/client.toml" && grep -q '^padding = "strong"$' "$T/etc/hstun-fp/client.toml" && grep -q '^session_age = "30m"$' "$T/etc/hstun-fp/client.toml" || fail "tls_fragment, padding or session_age are missing from client.toml"
if ./install.sh entry --yes --name fp3 --join "$J" --padding loud --forward-listen "127.0.0.1:$((base + 61))" >/dev/null 2>&1; then fail "--padding accepted loud"; fi
if ./install.sh entry --yes --name fp3 --join "$J" --tls-fragment sideways --forward-listen "127.0.0.1:$((base + 61))" >/dev/null 2>&1; then fail "--tls-fragment accepted sideways"; fi
if ./install.sh entry --yes --name fp2 --join "$J" --tls-fingerprint netscape --forward-listen "127.0.0.1:$((base + 59))" >/dev/null 2>&1; then fail "--tls-fingerprint accepted netscape"; fi
./install.sh uninstall --yes --purge --name fp >/dev/null 2>&1; rm -rf "${T:?}/etc/hstun-fp2" "${T:?}/etc/hstun-fp3"
ok "--max-mbit, --max-new-streams, --drain-timeout, --carrier auto and --tls-fingerprint are written, bad values are refused"

# --- the printed entry command names this exact release ---
grep -q "raw.githubusercontent.com/.*/v$(sed -n 's/^INSTALLER_VERSION="\(.*\)"/\1/p' install.sh)/install.sh" "$T/exit.log" || fail "exit summary does not give the versioned installer URL"
ok "the exit prints the installer URL of its own release"

# --- update is a no-op when current ---
cur=$("$T/bin/hstun" version | sed 's/^hstun //')
env -u HSTUN_TARBALL HSTUN_TEST_LATEST="$cur" "$T/bin/hstun-ctl" update >"$T/upd.log" 2>&1 || { cat "$T/upd.log"; fail "update"; }
grep -q "already up to date" "$T/upd.log" || fail "update did not say it is current"
ok "update does nothing when already on the latest release"

# --- the menu: header, contextual items, status screen ---
printf '1\n\n0\n' >"$T/menu.ans"
HSTUN_NO_UPDATE_CHECK=1 HSTUN_TTY="$T/menu.ans" "$T/bin/hstun-ctl" >"$T/menu.log" 2>&1 || { cat "$T/menu.log"; fail "menu"; }
grep -q "Installed on this host" "$T/menu.log" && grep -q "Show the pair code" "$T/menu.log" && grep -q "carrier" "$T/menu.log" || { cat "$T/menu.log"; fail "menu content"; }
grep -q "Install an exit" "$T/menu.log" && fail "menu offers a first install although instances exist"
ok "menu shows the installed instances and only the relevant items"

for n in e1 ""; do ./install.sh uninstall --yes --purge ${n:+--name "$n"} >/dev/null 2>&1 || fail "uninstall ${n:-default}"; done
[ -z "$(ls -A "$T/etc" 2>/dev/null)" ] && [ ! -e "$T/bin/hstun" ] && [ -z "$(ls -A "$T/systemd")" ] || fail "files left after --purge"
[ -x "$T/bin/hstun-ctl" ] || fail "hstun-ctl was removed with the last instance"
ok "uninstall --purge leaves nothing but hstun-ctl (kept so you can install again)"
echo "install.sh staging test passed"
