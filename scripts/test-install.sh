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

# --- a failed first install leaves nothing behind; leftovers of older runs can be cleaned (the paris dead end) ---
export HSTUN_BACKUP_DIR="$T/backup"; mkdir -p "$HSTUN_BACKUP_DIR"
rbnoyes=(--mode direct --forward-target "127.0.0.1:$((base + 1))" --backend-port "$((base + 5))")
rbflags=(--yes "${rbnoyes[@]}")
for at in check units firewall; do
  if HSTUN_TEST_FAIL_AT=$at ./install.sh exit "${rbflags[@]}" --name rb --port "$((base + 70))" >"$T/rb-$at.log" 2>&1; then fail "a forced failure at $at did not fail the install"; fi
  grep -q "forced failure at $at" "$T/rb-$at.log" && grep -q "nothing of this install was left behind" "$T/rb-$at.log" || { cat "$T/rb-$at.log"; fail "failure at $at: no rollback message"; }
  [ ! -e "$T/etc/hstun-rb" ] && [ -z "$(ls -A "$T/systemd")" ] || { ls -R "$T/etc" "$T/systemd"; fail "files left after a failed install at $at"; }
done
ok "an exit install that fails at check, units or firewall leaves no files behind"
./install.sh exit "${rbflags[@]}" --name rb --port "$((base + 70))" >"$T/rb-ok.log" 2>&1 || { cat "$T/rb-ok.log"; fail "install after the failed attempts"; }
J2=$("$T/bin/hstun-ctl" --name rb show-join 2>/dev/null)
for at in check units; do
  if HSTUN_TEST_FAIL_AT=$at ./install.sh entry --yes --name rbe --join "$J2" --forward-listen "127.0.0.1:$((base + 71))" >"$T/rbe-$at.log" 2>&1; then fail "entry failure at $at did not fail"; fi
  [ ! -e "$T/etc/hstun-rbe" ] && [ ! -e "$T/systemd/hstun-rbe-client.service" ] || fail "files left after a failed entry install at $at"
done
ok "an entry install that fails leaves no files behind either"
mkdir -p "$T/etc/hstun-keep"; echo mine >"$T/etc/hstun-keep/mycert.pem"
if HSTUN_TEST_FAIL_AT=check ./install.sh exit "${rbflags[@]}" --name keep --port "$((base + 72))" >/dev/null 2>&1; then fail "failure at check passed"; fi
[ -f "$T/etc/hstun-keep/mycert.pem" ] && [ ! -e "$T/etc/hstun-keep/server.toml" ] || fail "rollback removed a file that was there before, or kept one it wrote"
rm -rf "${T:?}/etc/hstun-keep"
if HSTUN_TEST_FAIL_AT=units-int ./install.sh exit "${rbflags[@]}" --name rbi --port "$((base + 75))" >"$T/rbi.log" 2>&1; then fail "Ctrl+C at units did not stop the install"; fi
[ ! -e "$T/etc/hstun-rbi" ] && ! compgen -G "$T/systemd/hstun-rbi-*" >/dev/null || { cat "$T/rbi.log"; ls -R "$T/etc" "$T/systemd"; fail "files left after Ctrl+C during the install"; }
ok "Ctrl+C during a first install also leaves nothing behind"
ok "the rollback removes only what the install wrote"
./install.sh uninstall --yes --purge --name rb >/dev/null 2>&1

# leftovers: config files without install.conf and without a service
mkorphan() { mkdir -p "$T/etc/hstun-orph"; printf 'role = "exit"\n' >"$T/etc/hstun-orph/server.toml"; printf 'secretvalue\n' >"$T/etc/hstun-orph/secret"; }
mkorphan
if setsid -w ./install.sh exit "${rbnoyes[@]}" --name orph --port "$((base + 73))" </dev/null >"$T/orph1.log" 2>&1; then fail "install over leftovers passed without a terminal and without --yes"; fi
grep -q "hstun-ctl clean --name orph" "$T/orph1.log" || { cat "$T/orph1.log"; fail "the error does not name the clean command"; }
[ -f "$T/etc/hstun-orph/server.toml" ] || fail "leftovers were removed without asking"
printf 'n\n' >"$T/ansn"
if HSTUN_TTY="$T/ansn" ./install.sh exit "${rbnoyes[@]}" --name orph --port "$((base + 73))" >/dev/null 2>&1; then fail "declining the clean-up still installed"; fi
[ -f "$T/etc/hstun-orph/server.toml" ] || fail "declined clean-up removed the leftovers"
ok "leftovers block the install with the exact command; declining changes nothing"
if ./install.sh uninstall --yes --name orph >"$T/orph2.log" 2>&1; then fail "uninstall of leftovers passed"; fi
grep -q "hstun-ctl clean --name orph" "$T/orph2.log" && ! grep -q "install it first" "$T/orph2.log" || { cat "$T/orph2.log"; fail "uninstall does not point at clean"; }
printf '0\n' >"$T/ans0"
HSTUN_NO_UPDATE_CHECK=1 HSTUN_TTY="$T/ans0" "$T/bin/hstun-ctl" >"$T/orph-menu.log" 2>&1 || { cat "$T/orph-menu.log"; fail "menu with leftovers"; }
grep -q "Remove leftovers" "$T/orph-menu.log" && ! grep -q "Adopt hand-installed" "$T/orph-menu.log" || { cat "$T/orph-menu.log"; fail "menu does not offer the clean-up"; }
ok "uninstall and the menu point at the clean-up"
printf 'y\n' >"$T/ansy"
HSTUN_TTY="$T/ansy" "$T/bin/hstun-ctl" clean >"$T/orph3.log" 2>&1 || { cat "$T/orph3.log"; fail "clean"; }
[ ! -e "$T/etc/hstun-orph" ] || fail "clean left the directory"
bk=$(ls "$HSTUN_BACKUP_DIR"/hstun-leftover-hstun-orph-*.tar.gz 2>/dev/null | head -1); [ -n "$bk" ] || fail "clean wrote no backup"
tar -tzf "$bk" | grep -q 'hstun-orph/secret' || fail "the backup lacks the secret"
[ "$(stat -c %a "$bk")" = 600 ] || fail "the backup is not private"
ok "clean backs the leftovers up (private archive) and removes them"
mkorphan
./install.sh exit "${rbflags[@]}" --name orph --port "$((base + 73))" >"$T/orph4.log" 2>&1 || { cat "$T/orph4.log"; fail "install over leftovers with --yes"; }
grep -q "private backup" "$T/orph4.log" && [ -f "$T/etc/hstun-orph/install.conf" ] || { cat "$T/orph4.log"; fail "--yes did not clean and install"; }
./install.sh uninstall --yes --purge --name orph >/dev/null 2>&1
ok "install --yes removes leftovers (with a backup) and installs"
# a directory that a service still runs from keeps the adopt path
mkdir -p "$T/etc/hstun-ref"; printf 'role = "exit"\n' >"$T/etc/hstun-ref/server.toml"
printf '[Service]\nExecStart=%s/bin/hstun server -config %s/etc/hstun-ref/server.toml\n' "$T" "$T" >"$T/systemd/hstun-ref-server.service"
if ./install.sh exit "${rbflags[@]}" --name ref --port "$((base + 74))" >"$T/ref.log" 2>&1; then fail "install over a directory with a service passed"; fi
grep -q "hstun-ctl adopt" "$T/ref.log" || { cat "$T/ref.log"; fail "a directory with a service lost the adopt hint"; }
[ -f "$T/etc/hstun-ref/server.toml" ] || fail "a directory with a service was removed"
rm -rf "${T:?}/etc/hstun-ref" "$T/systemd/hstun-ref-server.service"
ok "a directory that a service uses still points at adopt and is never removed"
echo "install.sh staging test passed"
