#!/usr/bin/env bash
# Staging test of the hstun-ctl commands: test, doctor, edit (with automatic undo), backup/restore, update with
# automatic rollback, rollback. No root, no systemd, nothing outside a temporary directory.
# Needs the tarball for this machine's CPU (run scripts/package.sh first, or pass HSTUN_TARBALL) and python3.
set -euo pipefail
cd "$(dirname "$0")/.."
arch=$(uname -m); case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
TARBALL="${HSTUN_TARBALL:-$(ls dist/hstun_*_linux_"$arch".tar.gz 2>/dev/null | head -1)}"
[ -f "$TARBALL" ] || { echo "test-ctl: no tarball (run scripts/package.sh)" >&2; exit 1; }
T=$(mktemp -d); pids=()
cleanup() { for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$T"; }
trap cleanup EXIT
base=$((20000 + RANDOM % 20000))
export HSTUN_TEST=1 HSTUN_ETC_BASE="$T/etc" HSTUN_BIN_DIR="$T/bin" HSTUN_SHARE_DIR="$T/share" HSTUN_WWW_DIR="$T/www" HSTUN_SYSTEMD_DIR="$T/systemd" HSTUN_TARBALL="$TARBALL" HSTUN_NO_UPDATE_CHECK=1
mkdir -p "$T/systemd" "$T/etc"
ok() { echo "ok   $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }
sum() { sha256sum "$1" | cut -d' ' -f1; }

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
python3 "$T/echo.py" & echo_pid=$!; pids+=("$echo_pid")

./install.sh exit --yes --mode direct --forward-target "127.0.0.1:$((base + 1))" --port "$base" --backend-port "$((base + 5))" >"$T/exit.log" 2>&1 || { cat "$T/exit.log"; fail "exit install"; }
"$T/bin/hstun" server -config "$T/etc/hstun/server.toml" >"$T/server.log" 2>&1 & spid=$!; pids+=("$spid")
sleep 1
J=$("$T/bin/hstun-ctl" show-join 2>/dev/null)
./install.sh entry --yes --name e1 --join "$J" --forward-listen "127.0.0.1:$((base + 2))" >"$T/entry.log" 2>&1 || { cat "$T/entry.log"; fail "entry install"; }
"$T/bin/hstun" client -config "$T/etc/hstun-e1/client.toml" >"$T/client.log" 2>&1 & cpid=$!; pids+=("$cpid")
admin=$(sed -n 's/^admin_listen = "\(.*\)"/\1/p' "$T/etc/hstun-e1/client.toml")
for _ in $(seq 1 20); do curl -s -m 2 "http://$admin/stats" 2>/dev/null | grep -q '"carrier"' && break; sleep 0.5; done
ok "exit and entry running"

# --- test ---
"$T/bin/hstun-ctl" --name e1 test >"$T/test.log" 2>&1 || { cat "$T/test.log"; fail "test on a working tunnel"; }
grep -q "stream through the tunnel" "$T/test.log" || fail "test did not check a stream"
ok "test passes on a working tunnel"
kill "$echo_pid"; sleep 0.5
if "$T/bin/hstun-ctl" --name e1 test >"$T/test2.log" 2>&1; then cat "$T/test2.log"; fail "test passed although the exit's target is gone"; fi
grep -q "closed the stream" "$T/test2.log" || { cat "$T/test2.log"; fail "test did not explain the closed stream"; }
ok "test fails and explains it when the exit's target is down"
python3 "$T/echo.py" & pids+=($!); sleep 0.5

# --- test --matrix: every row is measured with hstun probe and gets a verdict ---
HSTUN_MATRIX_CARRIERS="ws" HSTUN_MATRIX_FPS="go chrome" "$T/bin/hstun-ctl" --name e1 test --matrix --quick >"$T/matrix.log" 2>&1 || { cat "$T/matrix.log"; fail "test --matrix"; }
grep -Eq '^ws +go +ok ' "$T/matrix.log" && grep -Eq '^ws +chrome +ok ' "$T/matrix.log" || { cat "$T/matrix.log"; fail "matrix rows"; }
grep -q "Best working row" "$T/matrix.log" && grep -q "A stranger on" "$T/matrix.log" || { cat "$T/matrix.log"; fail "matrix summary"; }
ok "test --matrix measures carrier x fingerprint rows and names the best one"

# --- doctor ---
"$T/bin/hstun-ctl" doctor >"$T/doc.log" 2>&1 || { cat "$T/doc.log"; fail "doctor on the exit"; }
grep -q "configuration" "$T/doc.log" && grep -q "certificate is valid" "$T/doc.log" || { cat "$T/doc.log"; fail "doctor output"; }
ok "doctor checks configuration and certificate"

# --- backup, restore --test, restore ---
( cd "$T" && "$T/bin/hstun-ctl" backup out.tar.gz >/dev/null 2>&1 ) || fail "backup"
[ "$(stat -c %a "$T/out.tar.gz")" = 600 ] || fail "backup is not private (mode 600)"
"$T/bin/hstun-ctl" restore "$T/out.tar.gz" --test >"$T/rt.log" 2>&1 || { cat "$T/rt.log"; fail "restore --test"; }
grep -q "instance e1" "$T/rt.log" && grep -q "test only" "$T/rt.log" || fail "restore --test output"
cert=$(sum "$T/etc/hstun/cert.pem"); secret=$(sum "$T/etc/hstun/secret")
rm -rf "${T:?}/etc/hstun" "${T:?}/etc/hstun-e1"
"$T/bin/hstun-ctl" restore "$T/out.tar.gz" --yes >"$T/rs.log" 2>&1 || { cat "$T/rs.log"; fail "restore"; }
[ "$(sum "$T/etc/hstun/cert.pem")" = "$cert" ] && [ "$(sum "$T/etc/hstun/secret")" = "$secret" ] && [ -f "$T/etc/hstun-e1/client.toml" ] || fail "restore did not bring the files back"
ok "backup is private, restore --test reports, restore brings everything back"
echo "garbage" >"$T/bad.tar.gz"
if "$T/bin/hstun-ctl" restore "$T/bad.tar.gz" --test >/dev/null 2>&1; then fail "restore accepted a bad file"; fi
ok "restore refuses a file that is not a backup"

# --- edit: applied, and undone when unhealthy ---
newport=$((base + 40))
"$T/bin/hstun-ctl" edit --yes --port "$newport" >"$T/edit.log" 2>&1 || { cat "$T/edit.log"; fail "edit"; }
grep -q "$newport" "$T/etc/hstun/server.toml" || fail "edit did not change the port"
ok "edit applies a new port"
before=$(sum "$T/etc/hstun/server.toml")
if HSTUN_TEST_HEALTH=fail "$T/bin/hstun-ctl" edit --yes --port "$((base + 41))" >"$T/edit2.log" 2>&1; then fail "edit reported success on an unhealthy tunnel"; fi
[ "$(sum "$T/etc/hstun/server.toml")" = "$before" ] || fail "edit did not restore the previous settings"
grep -q "restoring the previous settings" "$T/edit2.log" || fail "edit did not say it restored"
ok "edit restores the previous settings when the tunnel does not come back"

# --- update with automatic rollback, then manual rollback ---
cp "$T/bin/hstun" "$T/bin/hstun.old"; printf 'z' >>"$T/bin/hstun.old"; cp "$T/bin/hstun.old" "$T/bin/hstun"
old=$(sum "$T/bin/hstun")
if HSTUN_TEST_HEALTH=fail "$T/bin/hstun-ctl" update >"$T/upd1.log" 2>&1; then fail "update succeeded on an unhealthy tunnel"; fi
[ "$(sum "$T/bin/hstun")" = "$old" ] || fail "update was not rolled back"
grep -q "going back to the previous one" "$T/upd1.log" || fail "update did not say it rolled back"
ok "update rolls back automatically when the tunnel does not come back"
"$T/bin/hstun-ctl" update >"$T/upd2.log" 2>&1 || { cat "$T/upd2.log"; fail "update"; }
[ "$(sum "$T/bin/hstun")" != "$old" ] && [ "$(sum "$T/bin/hstun.prev")" = "$old" ] || fail "update did not install the new binary and keep .prev"
"$T/bin/hstun-ctl" rollback --yes >/dev/null 2>&1 || fail "rollback"
[ "$(sum "$T/bin/hstun")" = "$old" ] || fail "rollback did not restore the previous binary"
ok "update keeps the previous binary and rollback restores it"
# --- rotate-secret: exit adds a second secret, the entry follows, the exit finishes ---
sessions() { curl -s -m 2 "http://$admin/stats" 2>/dev/null | grep -o '"carrier"' | wc -l; }
restart_server() { kill "$spid" 2>/dev/null || true; sleep 0.5; "$T/bin/hstun" server -config "$T/etc/hstun/server.toml" >>"$T/server.log" 2>&1 & spid=$!; pids+=("$spid"); sleep 1; }
restart_client() { kill "$cpid" 2>/dev/null || true; sleep 0.5; "$T/bin/hstun" client -config "$T/etc/hstun-e1/client.toml" >>"$T/client.log" 2>&1 & cpid=$!; pids+=("$cpid"); for _ in $(seq 1 20); do [ "$(sessions)" -ge 1 ] && return 0; sleep 0.5; done; return 1; }
"$T/bin/hstun-ctl" edit --yes --port "$base" >/dev/null 2>&1 || fail "edit back to the original port"
old_secret=$(sum "$T/etc/hstun/secret")
"$T/bin/hstun-ctl" rotate-secret >"$T/rot.log" 2>&1 || { cat "$T/rot.log"; fail "rotate-secret start"; }
grep -q 'secret_files = \[.*secret", ".*secret.new"\]' "$T/etc/hstun/server.toml" || fail "server does not list both secrets"
code=$(grep -o "rotate-secret --join '[^']*'" "$T/rot.log" | head -1 | sed "s/.*--join '//; s/'$//")
[ -n "$code" ] || fail "no pair code printed for the rotation"
restart_server
restart_client || fail "the entry with the OLD secret must still connect while both are accepted"
ok "rotation started: the exit accepts the old and the new secret"
"$T/bin/hstun-ctl" --name e1 rotate-secret --join "$code" >"$T/rot2.log" 2>&1 || { cat "$T/rot2.log"; fail "entry rotate-secret"; }
[ "$(sum "$T/etc/hstun-e1/secret")" != "$old_secret" ] || fail "the entry still has the old secret"
restart_client || fail "the entry with the NEW secret does not connect"
ok "the entry switched to the new secret"
"$T/bin/hstun-ctl" rotate-secret --finish --yes >"$T/rot3.log" 2>&1 || { cat "$T/rot3.log"; fail "rotate-secret --finish"; }
[ "$(sum "$T/etc/hstun/secret")" = "$(sum "$T/etc/hstun-e1/secret")" ] && [ ! -e "$T/etc/hstun/secret.new" ] || fail "finish did not install the new secret"
grep -q 'secret_files = \["[^"]*/secret"\]' "$T/etc/hstun/server.toml" || fail "finish left two secrets in the config"
restart_server; sleep 3
[ "$(sessions)" -ge 1 ] || fail "the entry lost its session after finish"
ok "rotation finished: one secret, tunnel up"
if "$T/bin/hstun-ctl" rotate-secret --finish --yes >/dev/null 2>&1; then fail "finish without a rotation succeeded"; fi
ok "finish without a started rotation is refused"

# --- pin ---
"$T/bin/hstun-ctl" pin 0.3.1 >/dev/null 2>&1 || fail "pin"
[ "$(cat "$T/etc/hstun.pin")" = "0.3.1" ] || fail "pin file"
"$T/bin/hstun-ctl" pin 2>&1 | grep -q "pinned to 0.3.1" || fail "pin does not show the version"
if "$T/bin/hstun-ctl" pin not-a-version >/dev/null 2>&1; then fail "pin accepted a bad version"; fi
"$T/bin/hstun-ctl" pin --clear >/dev/null 2>&1; [ ! -e "$T/etc/hstun.pin" ] || fail "pin --clear"
ok "pin, show and clear work, bad versions are refused"

echo "test-ctl: all passed"
