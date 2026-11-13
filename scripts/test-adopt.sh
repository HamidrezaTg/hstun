#!/usr/bin/env bash
# Staging test of `install.sh adopt`: hand-made layouts like the ones people build before using the installer
# (nginx-fronted exit with reload units; an entry whose unit and binary have custom names), then adopt, update,
# guard, forget and purge. No root, no systemd, nothing outside a temporary directory.
# Needs the tarball for this machine's CPU (run scripts/package.sh first, or pass HSTUN_TARBALL).
set -euo pipefail
cd "$(dirname "$0")/.."
arch=$(uname -m); case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
TARBALL="${HSTUN_TARBALL:-$(ls dist/hstun_*_linux_"$arch".tar.gz 2>/dev/null | head -1)}"
[ -f "$TARBALL" ] || { echo "test-adopt: no tarball (run scripts/package.sh)" >&2; exit 1; }
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
export HSTUN_TEST=1 HSTUN_ETC_BASE="$T/etc" HSTUN_BIN_DIR="$T/bin" HSTUN_SHARE_DIR="$T/share" HSTUN_WWW_DIR="$T/www" HSTUN_SYSTEMD_DIR="$T/systemd" HSTUN_TARBALL="$TARBALL"
mkdir -p "$T/systemd" "$T/etc/hstun" "$T/etc/hstun-cf" "$T/bin" "$T/www/decoy" "$T/old"
ok() { echo "ok   $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }
has() { grep -q -- "$2" "$1" || fail "$1 lacks: $2"; }

tar -xzf "$TARBALL" -C "$T/old"
# the "old" binaries differ from the release by one trailing byte, so replacing them is observable
cp "$T/old/hstun" "$T/bin/hstun"; printf 'x' >>"$T/bin/hstun"
cp "$T/old/hstun" "$T/bin/hstun-chunk"; printf 'y' >>"$T/bin/hstun-chunk"
chmod 0755 "$T/bin/hstun" "$T/bin/hstun-chunk"
"$T/old/hstun" keygen -out "$T/etc/hstun/secret" >/dev/null
"$T/old/hstun" keygen -out "$T/etc/hstun-cf/secret" >/dev/null

cat >"$T/etc/hstun/server.toml" <<EOF
[server]
role         = "exit"
listen       = "127.0.0.1:24083"
trust_proxy  = "loopback"
carriers     = ["ws", "http"]
ws_path      = "/aa11/bb22"
http_path    = "/cc33/dd44"
decoy_dir    = "$T/www/decoy"
secret_files = ["$T/etc/hstun/secret"]
admin_listen = "127.0.0.1:9199"

[[forward]]
name   = "x1"
target = "127.0.0.1:19990"
EOF
cat >"$T/etc/hstun/nginx.conf" <<EOF
events {}
http { server { listen 2083 ssl; server_name hel.example.org; } }
EOF
cat >"$T/etc/hstun-cf/client.toml" <<EOF
[client]
role        = "entry"
server      = "bpd.example.org:443"
connect     = "104.21.43.22:443"
carrier     = "chunk"
ws_path     = "/aa11/bb22"
http_path   = "/cc33/dd44"
secret_file = "$T/etc/hstun-cf/secret"
name        = "irvps-chunk"
tls_verify  = "system"
admin_listen = "127.0.0.1:9198"

[[forward]]
name   = "x1"
listen = "127.0.0.1:20155"
EOF
cat >"$T/systemd/hstun-server.service" <<EOF
[Service]
User=hstunsvc
ExecStart=$T/bin/hstun server -config $T/etc/hstun/server.toml
EOF
cat >"$T/systemd/hstun-nginx.service" <<EOF
[Service]
ExecStart=/usr/sbin/nginx -c $T/etc/hstun/nginx.conf -g 'daemon off;'
EOF
cat >"$T/systemd/hstun-nginx-reload.service" <<EOF
[Service]
ExecStart=/bin/systemctl reload hstun-nginx.service
EOF
printf '[Path]\nPathChanged=/etc/x\n' >"$T/systemd/hstun-nginx-reload.path"
printf '[Timer]\nOnCalendar=daily\n' >"$T/systemd/hstun-nginx-reload.timer"
cat >"$T/systemd/hstun-chunk.service" <<EOF
[Service]
ExecStart=$T/bin/hstun-chunk client -config $T/etc/hstun-cf/client.toml
EOF
printf '[Service]\nExecStart=/usr/bin/other thing\n' >"$T/systemd/hstun-unrelated.service"

# 1. dry run changes nothing
./install.sh adopt --dry-run >"$T/dry.log" 2>&1 || { cat "$T/dry.log"; fail "dry run"; }
has "$T/dry.log" "would adopt hstun-server.service as instance default: role exit, mode nginx"
has "$T/dry.log" "would adopt hstun-chunk.service as instance cf: role entry"
has "$T/dry.log" "units: hstun-server.service hstun-nginx.service hstun-nginx-reload.service hstun-nginx-reload.path hstun-nginx-reload.timer"
[ ! -e "$T/etc/hstun/install.conf" ] && [ ! -e "$T/bin/hstun-ctl" ] || fail "dry run wrote files"
ok "dry run lists both instances and writes nothing"

# 2. adopt
cp "$T/bin/hstun" "$T/bin.hstun.before"; cp "$T/systemd/hstun-server.service" "$T/unit.before"; cp "$T/etc/hstun/server.toml" "$T/server.before"
./install.sh adopt >"$T/adopt.log" 2>&1 || { cat "$T/adopt.log"; fail "adopt"; }
for f in "$T/etc/hstun/install.conf" "$T/etc/hstun-cf/install.conf" "$T/bin/hstun-ctl"; do [ -e "$f" ] || fail "missing $f"; done
has "$T/etc/hstun/install.conf" "^ADOPTED=1"
has "$T/etc/hstun/install.conf" "^DOMAIN=hel.example.org"
has "$T/etc/hstun/install.conf" "^PORT=2083"
has "$T/etc/hstun/install.conf" "^BACKEND_PORT=24083"
has "$T/etc/hstun-cf/install.conf" "^ADOPT_BIN=$T/bin/hstun-chunk"
has "$T/etc/hstun-cf/install.conf" "^FORWARD_LISTEN=127.0.0.1:20155"
cmp -s "$T/bin/hstun" "$T/bin.hstun.before" && cmp -s "$T/systemd/hstun-server.service" "$T/unit.before" && cmp -s "$T/etc/hstun/server.toml" "$T/server.before" || fail "adopt changed live files"
ok "adopt registered both, live binary, unit and config untouched"
"$T/bin/hstun-ctl" adopt >"$T/again.log" 2>&1; has "$T/again.log" "already managed"
ok "adopt twice is harmless"

# 3. management works on adopted instances
"$T/bin/hstun-ctl" status >"$T/status.log" 2>&1 || fail "status"
has "$T/status.log" "(adopted)"
"$T/bin/hstun-ctl" --name cf status >"$T/status2.log" 2>&1 || fail "status cf"
has "$T/status2.log" "hstun-chunk.service"
J=$("$T/bin/hstun-ctl" show-join --no-secret); [ "${J#hstun-join:}" != "$J" ] || fail "show-join"
echo "${J#hstun-join:}" | base64 -d | grep -q "server=hel.example.org:2083" || fail "join content"
ok "status and show-join work on adopted instances"

# 4. an install must never overwrite an adopted or unmanaged setup
if ./install.sh exit --yes --forward-target 127.0.0.1:1 >"$T/guard.log" 2>&1; then fail "install over adopted succeeded"; fi
has "$T/guard.log" "was adopted"
mkdir -p "$T/etc/hstun-zz"; echo "[server]" >"$T/etc/hstun-zz/server.toml"
if ./install.sh exit --yes --name zz --forward-target 127.0.0.1:1 >"$T/guard2.log" 2>&1; then fail "install over unmanaged succeeded"; fi
has "$T/guard2.log" "not installed by hstun-ctl"
cmp -s "$T/etc/hstun/server.toml" "$T/server.before" || fail "guard did not protect the config"
rm -rf "$T/etc/hstun-zz"
ok "install refuses adopted and unmanaged setups"

# 5. update: preflight protects, then both binaries are replaced with .prev kept
cp "$T/etc/hstun-cf/client.toml" "$T/client.good"
echo 'bogus_key = 1' >>"$T/etc/hstun-cf/client.toml"
cp "$T/bin/hstun-chunk" "$T/chunk.before"
if "$T/bin/hstun-ctl" update --all >"$T/upd-bad.log" 2>&1; then fail "update accepted a bad config"; fi
cmp -s "$T/bin/hstun-chunk" "$T/chunk.before" && cmp -s "$T/bin/hstun" "$T/bin.hstun.before" || fail "failed preflight still replaced a binary"
ok "update stops before replacing anything when a config is rejected"
cp "$T/client.good" "$T/etc/hstun-cf/client.toml"
"$T/bin/hstun-ctl" update --all >"$T/upd.log" 2>&1 || { cat "$T/upd.log"; fail "update"; }
cmp -s "$T/bin/hstun" "$T/old/hstun" && cmp -s "$T/bin/hstun-chunk" "$T/old/hstun" || fail "binaries not replaced"
cmp -s "$T/bin/hstun.prev" "$T/bin.hstun.before" && cmp -s "$T/bin/hstun-chunk.prev" "$T/chunk.before" || fail ".prev copies wrong"
ok "update replaced shared and custom binaries, kept .prev"

# 6. forget, then purge
"$T/bin/hstun-ctl" --name cf forget --yes >/dev/null 2>&1 || fail "forget"
[ ! -e "$T/etc/hstun-cf/install.conf" ] && [ -e "$T/etc/hstun-cf/client.toml" ] && [ -e "$T/systemd/hstun-chunk.service" ] || fail "forget removed too much"
ok "forget removes only the registration"
"$T/bin/hstun-ctl" uninstall --purge --yes >/dev/null 2>&1 || fail "uninstall"
for f in "$T/systemd/hstun-server.service" "$T/systemd/hstun-nginx.service" "$T/systemd/hstun-nginx-reload.path" "$T/etc/hstun" "$T/bin/hstun"; do [ ! -e "$f" ] || fail "left $f"; done
[ -d "$T/www/decoy" ] || fail "purge deleted the adopted decoy directory"
[ -e "$T/systemd/hstun-chunk.service" ] || fail "uninstall touched another instance"
ok "purge removed the adopted exit only; the decoy directory and the other instance stay"
echo "test-adopt: all passed"
