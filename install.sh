#!/usr/bin/env bash
# hstun installer and manager (Debian/Ubuntu first; dnf/yum/pacman for the packages it needs).
#
#   bash <(curl -Ls https://raw.githubusercontent.com/OWNER/hstun/main/install.sh)
#
# While the repository is private both the script and the release need a token with read access:
#   export GITHUB_TOKEN=...   # fine-grained token, Contents: read
#   bash <(curl -Ls -H "Authorization: Bearer $GITHUB_TOKEN" https://raw.githubusercontent.com/OWNER/hstun/main/install.sh)
#
# One command, two roles (one role per instance, any number of instances with --name):
#   install.sh exit   [flags]   the server side: listens, is the exit (the outside host)
#   install.sh entry  [flags]   the client side: owns the client-facing ports (the restricted host)
#   hstun-ctl status|logs|restart|update|show-join|show-secret|uninstall   after installing
# Run it with no arguments for a menu. Every question has a flag or an HSTUN_* environment variable,
# so it runs unattended with --yes. See README.md ("Install").
set -Eeuo pipefail
# run from a directory that was just deleted (uninstall --purge from inside /etc/hstun): move out of it, quietly
pwd >/dev/null 2>&1 || cd / 2>/dev/null || true

INSTALLER_VERSION="0.5.0"
REPO="${HSTUN_REPO:-HamidrezaTg/hstun}"
WANT_VERSION="${HSTUN_VERSION:-}"
TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"

# Directories can be moved for staging and tests (HSTUN_TEST=1 also skips users, packages, systemd and the firewall).
TEST="${HSTUN_TEST:-0}"
ETC_BASE="${HSTUN_ETC_BASE:-/etc}"
BIN_DIR="${HSTUN_BIN_DIR:-/usr/local/bin}"
SHARE_DIR="${HSTUN_SHARE_DIR:-/usr/share/hstun}"
WWW_DIR="${HSTUN_WWW_DIR:-/var/www}"
SYSTEMD_DIR="${HSTUN_SYSTEMD_DIR:-/etc/systemd/system}"
SVC_USER="${HSTUN_USER:-hstunsvc}"

# ---------- output ----------
if [ -t 1 ]; then R=$'\e[31m'; G=$'\e[32m'; Y=$'\e[33m'; B=$'\e[1m'; D=$'\e[2m'; N=$'\e[0m'; else R=; G=; Y=; B=; D=; N=; fi
info() { printf '%s\n' "${G}[hstun]${N} $*"; }
warn() { printf '%s\n' "${Y}[hstun] warning:${N} $*" >&2; }
die() { printf '%s\n' "${R}[hstun] error:${N} $*" >&2; exit 1; }
# shellcheck disable=SC2154  # rc is set inside the trap string
trap 'rc=$?; [ -n "${STAGE:-}" ] && rm -rf "$STAGE"; exit $rc' EXIT

# ---------- input ----------
YES=0
TTY=""
if [ -t 0 ]; then TTY=/dev/stdin; elif [ -r /dev/tty ] && (: </dev/tty) 2>/dev/null; then TTY=/dev/tty; fi
if [ -n "${HSTUN_TTY:-}" ]; then TTY=$HSTUN_TTY; fi # tests feed answers from a file
if [ -n "$TTY" ]; then exec 9<"$TTY"; fi # one descriptor for every prompt, so a file of answers is read in order

# ask VAR "prompt" [default] [optional]: keeps a value that is already set (flag, env, saved config).
ask() {
  local name=$1 prompt=$2 def=${3-} opt=${4-} ans dv="DEF_$1"
  if [ -n "${!name-}" ]; then return 0; fi
  if [ -n "${!dv-}" ]; then def=${!dv}; fi # the value this instance had before "Change settings"
  if [ "$YES" = 1 ] || [ -z "$TTY" ]; then
    if [ -z "$def" ] && [ -z "$opt" ]; then die "missing value: $prompt (pass the matching flag, see: install.sh help)"; fi
    printf -v "$name" '%s' "$def"
    return 0
  fi
  read -r -p "$prompt${def:+ [$def]}: " ans <&9 || true
  printf -v "$name" '%s' "${ans:-$def}"
  if [ -z "${!name}" ] && [ -z "$opt" ]; then die "a value is required: $prompt"; fi
}
ask_secret() { # VAR "prompt": hidden input
  local name=$1 prompt=$2 ans
  [ -z "$TTY" ] && die "no terminal to read the secret from; use --secret-file or --secret-from"
  read -r -s -p "$prompt: " ans <&9 || true
  echo
  printf -v "$name" '%s' "$ans"
}
confirm() { # "question" [y|n default]
  local q=$1 def=${2:-n} ans
  if [ "$YES" = 1 ]; then return 0; fi
  if [ -z "$TTY" ]; then [ "$def" = y ]; return; fi
  read -r -p "$q [$([ "$def" = y ] && echo Y/n || echo y/N)]: " ans <&9 || true
  ans=${ans:-$def}
  case "$ans" in y | Y | yes) return 0 ;; *) return 1 ;; esac
}

# ---------- system helpers ----------
is_root() { [ "$(id -u)" = 0 ]; }
need_root() { [ "$TEST" = 1 ] || is_root || die "run as root (sudo -i, then run this again)"; }
have() { command -v "$1" >/dev/null 2>&1; }
rand_hex() { openssl rand -hex "$1"; }
rand_path() { printf '/%s/%s' "$(rand_hex 6)" "$(rand_hex 4)"; }
own() { [ "$TEST" = 1 ] && return 0; chown "$@"; }
# a port counts as busy when something listens on it or another (maybe stopped) instance has reserved it
reserved_port() {
  local f
  for f in "$ETC_BASE"/hstun/install.conf "$ETC_BASE"/hstun-*/install.conf; do
    [ -f "$f" ] || continue
    [ "$f" = "$ETC/install.conf" ] && continue
    grep -qE "^(PORT|BACKEND_PORT|ADMIN_PORT)=$1\$" "$f" && return 0
  done
  return 1
}
port_busy() { ss -ltnH "( sport = :$1 )" 2>/dev/null | grep -q . || reserved_port "$1"; }
free_port() { local p=$1; while port_busy "$p"; do p=$((p + 1)); done; echo "$p"; }

PM=""
detect_pm() {
  if have apt-get; then PM=apt; elif have dnf; then PM=dnf; elif have yum; then PM=yum; elif have pacman; then PM=pacman; fi
}
APT_UPDATED=0
pkg_install() {
  [ "$TEST" = 1 ] && return 0
  [ "${HSTUN_SKIP_PKG:-0}" = 1 ] && return 0
  detect_pm
  case "$PM" in
    apt)
      local log; log=$(mktemp)
      if [ "$APT_UPDATED" = 0 ]; then apt-get update -qq >"$log" 2>&1 || { cat "$log" >&2; rm -f "$log"; die "apt-get update failed"; }; APT_UPDATED=1; fi
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" >"$log" 2>&1 || { cat "$log" >&2; rm -f "$log"; die "could not install: $*"; }
      rm -f "$log" ;;
    dnf) dnf install -y -q "$@" >/dev/null ;;
    yum) yum install -y -q "$@" >/dev/null ;;
    pacman) pacman -S --noconfirm --needed "$@" >/dev/null ;;
    *) die "no supported package manager; install these yourself: $*" ;;
  esac
}
ensure_cmds() { # command[:package] ...
  local spec cmd pkg missing=()
  for spec in "$@"; do
    cmd=${spec%%:*}; pkg=${spec#*:}
    have "$cmd" || missing+=("$pkg")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    [ "$TEST" = 1 ] && { warn "test mode: not installing ${missing[*]}"; return 0; }
    info "installing requirements: ${missing[*]}"
    pkg_install "${missing[@]}"
  fi
}
detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *) die "unsupported CPU: $(uname -m) (amd64 and arm64 are built; build from source with HSTUN_BUILD=1)" ;;
  esac
}
have_systemd() { [ "$TEST" != 1 ] && [ "${HSTUN_SKIP_SYSTEMD:-0}" != 1 ] && [ -d /run/systemd/system ]; }
sysd() { have_systemd || return 0; systemctl "$@"; }

# ---------- release download ----------
STAGE=""
gh_curl() {
  local a=()
  [ -n "$TOKEN" ] && a=(-H "Authorization: Bearer $TOKEN")
  curl -fsSL ${a[@]+"${a[@]}"} "$@"
}
ensure_go() {
  local need=1.26.11 have_v
  have_v=$(go env GOVERSION 2>/dev/null | sed 's/^go//' || true)
  if [ -n "$have_v" ] && [ "$(printf '%s\n%s\n' "$have_v" "$need" | sort -V | head -1)" = "$need" ]; then return 0; fi
  info "Go >= $need is needed to build from source; fetching the official toolchain from go.dev"
  local arch dir; arch=$(detect_arch); dir="$SHARE_DIR/go-toolchain"
  mkdir -p "$dir"
  curl -fsSL "https://go.dev/dl/go${need}.linux-${arch}.tar.gz" | tar -xz -C "$dir" --strip-components=1
  export PATH="$dir/bin:$PATH"
}
build_from_source() {
  ensure_cmds curl tar
  local ref=${WANT_VERSION:+v${WANT_VERSION#v}}; ref=${ref:-main}
  info "building from source ($REPO@$ref)"
  gh_curl -o "$STAGE/src.tgz" "https://api.github.com/repos/$REPO/tarball/$ref" || die "cannot download the source (private repository? set GITHUB_TOKEN)"
  mkdir "$STAGE/src"; tar -xzf "$STAGE/src.tgz" -C "$STAGE/src" --strip-components=1
  ensure_go
  local ver; ver=$(sed -n 's/^var version = "\(.*\)"$/\1/p' "$STAGE/src/cmd/hstun/main.go")
  (cd "$STAGE/src" && CGO_ENABLED=0 go build -trimpath -mod=vendor -ldflags "-s -w -X main.version=${ver}" -o "$STAGE/hstun" ./cmd/hstun)
  mkdir -p "$STAGE/share"
  cp "$STAGE/src/install.sh" "$STAGE/share/install.sh"
  cp -r "$STAGE/src/deploy" "$STAGE/share/deploy"
  cp -r "$STAGE/src/deploy/decoy-example" "$STAGE/share/decoy-example"
}
fetch_release() {
  STAGE=$(mktemp -d)
  if [ -n "${HSTUN_TARBALL:-}" ]; then
    info "using local release file $HSTUN_TARBALL"
    tar -xzf "$HSTUN_TARBALL" -C "$STAGE"; return 0
  fi
  if [ "${HSTUN_BUILD:-0}" = 1 ]; then build_from_source; return 0; fi
  ensure_cmds curl tar jq openssl:openssl
  if [ -z "$TOKEN" ] && have gh && gh auth status >/dev/null 2>&1; then TOKEN=$(gh auth token 2>/dev/null || true); fi
  local arch api json tag ver name url surl
  arch=$(detect_arch)
  if [ -n "$WANT_VERSION" ]; then api="https://api.github.com/repos/$REPO/releases/tags/v${WANT_VERSION#v}"; else api="https://api.github.com/repos/$REPO/releases/latest"; fi
  json=$(gh_curl -H "Accept: application/vnd.github+json" "$api") || die "cannot read the release at $api. For a private repository export GITHUB_TOKEN (Contents: read)."
  tag=$(jq -r .tag_name <<<"$json"); ver=${tag#v}
  name="hstun_${ver}_linux_${arch}.tar.gz"
  url=$(jq -r --arg n "$name" '.assets[] | select(.name==$n) | .url' <<<"$json")
  surl=$(jq -r '.assets[] | select(.name=="SHA256SUMS") | .url' <<<"$json")
  [ -n "$url" ] && [ -n "$surl" ] || die "release $tag has no $name or no SHA256SUMS; try HSTUN_BUILD=1"
  info "downloading hstun $ver ($arch)"
  gh_curl -H "Accept: application/octet-stream" -o "$STAGE/$name" "$url"
  gh_curl -H "Accept: application/octet-stream" -o "$STAGE/SHA256SUMS" "$surl"
  (cd "$STAGE" && grep " ${name}\$" SHA256SUMS | sha256sum -c - >/dev/null) || die "checksum mismatch for $name"
  tar -xzf "$STAGE/$name" -C "$STAGE"
}
install_files() {
  [ -x "$STAGE/hstun" ] || die "the release has no hstun binary"
  install -d "$BIN_DIR" "$SHARE_DIR"
  # keep the previous binary as .prev, but never overwrite a rollback copy with an identical binary
  if [ -x "$BIN_DIR/hstun" ] && ! cmp -s "$BIN_DIR/hstun" "$STAGE/hstun"; then cp -p "$BIN_DIR/hstun" "$BIN_DIR/hstun.prev"; fi
  install -m 0755 "$STAGE/hstun" "$BIN_DIR/hstun"
  rm -rf "$SHARE_DIR/deploy" "$SHARE_DIR/decoy-example" "$SHARE_DIR/licenses"
  cp -r "$STAGE/share/." "$SHARE_DIR/"
  install -m 0755 "$STAGE/share/install.sh" "$BIN_DIR/hstun-ctl"
  info "installed $("$BIN_DIR/hstun" version) to $BIN_DIR"
}

# ---------- instance state ----------
NAME=""
ROLE="" MODE="" FORWARD_TARGET="" PORT="" DOMAIN="" CERT="" KEY="" ACME="" EMAIL="" ALLOW_FROM="" DECOY_DIR="" BACKEND_PORT=""
JOIN="" JOIN_SECRET="" NO_SECRET=0 SERVER="" CARRIER="" WS_PATH="" HTTP_PATH="" SECRET_FILE="" SECRET_FROM="" TLS_VERIFY="" SPKI="" CONNECT="" SNI=""
FORWARD_LISTEN="" FORWARD_NAME="" CONNS="" CHUNK_PARALLEL="" SOCKS_LISTEN="" NO_FW="" ADMIN_PORT="" SELF_SIGNED="" PUBLIC_ADDR=""
MAX_MBIT="" MAX_NEW_STREAMS="" DRAIN_TIMEOUT="" TLS_FINGERPRINT="" PADDING="" TLS_FRAGMENT="" SESSION_AGE="" MATRIX=0 QUICK=0
FOLLOW=0 PURGE=0 ALL=0 IPV6="" FINISH=0 SECRET_OVERRIDE="" EDIT_MODE=0 FILE="" FILE_TEST=0 FORCE=0 RECONFIGURE=0 REINSTALL=0 NEW_CERT=0 CUSTOMIZE=0 SKIP_LOAD="" DRY_RUN=0 ADOPT_ONLY=""
ADOPTED="" ADOPT_UNITS="" ADOPT_BIN=""

set_paths() {
  ETC="$ETC_BASE/hstun${NAME:+-$NAME}"
  UNIT="hstun${NAME:+-$NAME}"
  DECOY_TARGET="$WWW_DIR/hstun${NAME:+-$NAME}-decoy"
}
CONF_KEYS=(ROLE MODE FORWARD_TARGET PORT DOMAIN CERT KEY BACKEND_PORT ADMIN_PORT WS_PATH HTTP_PATH SERVER CARRIER TLS_VERIFY SPKI CONNECT SNI
  FORWARD_LISTEN FORWARD_NAME CONNS CHUNK_PARALLEL SOCKS_LISTEN ALLOW_FROM PUBLIC_ADDR FW_PORTS CONNLIMIT_PORT SELF_SIGNED
  ADOPTED ADOPT_UNITS ADOPT_BIN IPV6 MAX_MBIT MAX_NEW_STREAMS DRAIN_TIMEOUT TLS_FINGERPRINT PADDING TLS_FRAGMENT SESSION_AGE)
FW_PORTS="" CONNLIMIT_PORT=""
load_conf() { # saved values only fill what is still empty (flags win)
  [ -f "$ETC/install.conf" ] || return 0
  local k v line
  while IFS= read -r line; do
    k=${line%%=*}; v=${line#*=}
    case " $SKIP_LOAD " in *" $k "*) continue ;; esac
    case " ${CONF_KEYS[*]} " in *" $k "*) [ -n "${!k-}" ] || printf -v "$k" '%s' "$(eval "printf '%s' $v")" ;; esac
  done <"$ETC/install.conf"
}
save_conf() {
  local k
  { for k in "${CONF_KEYS[@]}"; do printf '%s=%q\n' "$k" "${!k-}"; done; } >"$ETC/install.conf"
  chmod 0600 "$ETC/install.conf"
}
reset_conf() { local k; for k in "${CONF_KEYS[@]}"; do printf -v "$k" '%s' ""; done; }
put_file() { # SRC DST: replace a binary that may be running (copy beside it, then rename; cp onto a running binary fails with "Text file busy")
  local t="$2.new.$$"
  cp -p "$1" "$t" && mv -f "$t" "$2"
}
inst_bin() { echo "${ADOPT_BIN:-$BIN_DIR/hstun}"; }
# an existing hstun that this script did not create must never be overwritten by an install
guard_existing() { # ROLE-WANTED
  if [ -n "$ROLE" ] && [ "$ROLE" != "${1:-$ROLE}" ]; then die "instance ${NAME:-default} is an $ROLE; use --name to install an ${1} next to it"; fi
  if [ -n "$ADOPTED" ]; then die "instance ${NAME:-default} was adopted from a hand-made setup; install would rewrite its files. Use update, restart or uninstall, or pick another --name."; fi
  if [ ! -f "$ETC/install.conf" ] && { [ -f "$ETC/server.toml" ] || [ -f "$ETC/client.toml" ]; }; then
    die "found an hstun setup in $ETC that was not installed by hstun-ctl. Run: install.sh adopt   (or choose another --name)"
  fi
}
# An instance of this name exists: say what it is and let the user keep, change or reinstall it.
# Returns 1 when the user chose to keep it unchanged.
existing_prompt() {
  [ -f "$ETC/install.conf" ] || return 0
  grep -q '^ADOPTED=1' "$ETC/install.conf" && return 0 # guard_existing explains that one
  local sum c k dump=""
  sum=$( (reset_conf; load_conf
    if [ "$ROLE" = exit ]; then printf 'exit, mode %s, port %s%s, forwards to %s' "$MODE" "${PORT:-$BACKEND_PORT}" "${DOMAIN:+, domain $DOMAIN}" "$FORWARD_TARGET"
    else printf 'entry, %s via %s, listens on %s' "$SERVER" "${CARRIER:-ws}" "$FORWARD_LISTEN"; fi) )
  info "instance ${NAME:-default} is already installed: $sum"
  if [ "$REINSTALL" = 1 ]; then reinstall_wipe; return 0; fi
  if [ "$RECONFIGURE" = 1 ] && { [ "$EDIT_MODE" != 1 ] || [ "$YES" = 1 ] || [ -z "$TTY" ]; }; then return 0; fi
  if [ "$YES" = 1 ] || [ -z "$TTY" ]; then return 0; fi
  if [ "$EDIT_MODE" = 1 ]; then c=2
  else
    echo "  1) Keep it as it is (nothing changes)"
    echo "  2) Change settings (the certificate, secret and paths are kept, so paired entries keep working)"
    echo "  3) Reinstall from scratch (new certificate, secret and paths: every entry must be paired again)"
    c=""; ask c "Choose" 1
  fi
  case "$c" in
    1) info "nothing changed"; return 1 ;;
    2) for k in MODE FORWARD_TARGET PORT DOMAIN ALLOW_FROM SERVER CARRIER FORWARD_LISTEN FORWARD_NAME CONNECT SNI; do
         dump=$(reset_conf; load_conf; printf 'DEF_%s=%q\n' "$k" "${!k-}")
         eval "$dump"; SKIP_LOAD="$SKIP_LOAD $k"
       done ;;
    3) reinstall_wipe ;;
    *) die "choose 1, 2 or 3" ;;
  esac
}
reinstall_wipe() {
  # stop the running instance first, otherwise its own listener makes the new port check fail
  ( reset_conf; load_conf; for u in $(service_units); do sysd stop "$u"; done ) || true
  rm -f "${ETC:?}/cert.pem" "${ETC:?}/key.pem" "${ETC:?}/secret" "${ETC:?}/install.conf" "${ETC:?}/server.toml" "${ETC:?}/client.toml"
  info "starting from scratch: new certificate, secret and paths"
}
need_installed() { [ -f "$ETC/install.conf" ] || die "no hstun instance${NAME:+ named $NAME} found in $ETC (install it first)"; load_conf; }

# ---------- certificates ----------
spki_of() { openssl x509 -in "$1" -pubkey -noout | openssl pkey -pubin -outform der | openssl dgst -sha256 -hex | sed 's/^.*= //'; }
cert_selfsigned() { # dir
  local san=()
  [ -n "$DOMAIN" ] && san=(-addext "subjectAltName=DNS:$DOMAIN")
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout "$1/key.pem" -out "$1/cert.pem" \
    -days 3650 -subj "/CN=${DOMAIN:-localhost}" ${san[@]+"${san[@]}"} 2>/dev/null
  chmod 0600 "$1/key.pem"; chmod 0644 "$1/cert.pem"
}
cert_acme() {
  [ -n "$DOMAIN" ] || die "--acme needs --domain"
  info "requesting a Let's Encrypt certificate for $DOMAIN (port 80 must be free and the name must point at this host)"
  ensure_cmds certbot
  local em=(--register-unsafely-without-email)
  [ -n "$EMAIL" ] && em=(-m "$EMAIL")
  certbot certonly --standalone --non-interactive --agree-tos "${em[@]}" -d "$DOMAIN" >/dev/null || die "certbot failed (is port 80 free and $DOMAIN pointing here?)"
  CERT="/etc/letsencrypt/live/$DOMAIN/fullchain.pem"; KEY="/etc/letsencrypt/live/$DOMAIN/privkey.pem"
}

# ---------- firewall ----------
ufw_active() { have ufw && ufw status 2>/dev/null | grep -q '^Status: active'; }
fw_allow() { # port [from]
  [ "$TEST" = 1 ] && return 0
  [ -n "$NO_FW" ] && return 0
  if ! ufw_active; then warn "ufw is not active: open $1/tcp in your firewall yourself"; return 0; fi
  if [ -n "${2:-}" ]; then ufw allow from "$2" to any port "$1" proto tcp comment "$UNIT" >/dev/null; else ufw allow "$1/tcp" comment "$UNIT" >/dev/null; fi
  FW_PORTS="$FW_PORTS $1"
  info "ufw: allowed $1/tcp${2:+ from $2}"
}
connlimit_edit() { # FILE CHAIN MASK PORT: add the per-source cap after the loopback accept line (idempotent)
  local f=$1 chain=$2 mask=$3 port=$4
  [ -f "$f" ] || return 1
  grep -q "hstun-connlimit $UNIT begin" "$f" && return 0
  cp -p "$f" "$f.pre-$UNIT"
  awk -v u="$UNIT" -v p="$port" -v ch="$chain" -v m="$mask" '{print} $0 ~ ("^-A " ch " -i lo -j ACCEPT") && !d {
    print "# hstun-connlimit " u " begin"
    print "-A " ch " -p tcp --dport " p " --syn -m connlimit --connlimit-above 64 --connlimit-mask " m " -j REJECT --reject-with tcp-reset"
    print "# hstun-connlimit " u " end"; d=1 }' "$f" >"$f.new" && mv "$f.new" "$f"
}
fw_connlimit() { # port: at most 64 concurrent connections per source IP (nginx limit_conn does not count silent connections)
  [ "$TEST" = 1 ] && return 0
  [ -n "$NO_FW" ] && return 0
  if ufw_active && [ -f /etc/ufw/before.rules ]; then
    connlimit_edit /etc/ufw/before.rules ufw-before-input 32 "$1"
    # IPv6: per /64, only when ufw manages IPv6 and this exit listens on it
    if [ "$IPV6" = 1 ] && [ -f /etc/ufw/before6.rules ] && grep -qi '^IPV6=yes' /etc/default/ufw 2>/dev/null; then
      connlimit_edit /etc/ufw/before6.rules ufw6-before-input 64 "$1" || true
    fi
    ufw reload >/dev/null
    # shellcheck disable=SC2034  # saved to install.conf through CONF_KEYS
    CONNLIMIT_PORT=$1
    info "ufw: per-source cap of 64 concurrent connections on $1/tcp"
  else
    warn "ufw is not active: add a per-source connection cap on $1/tcp yourself (iptables -m connlimit --connlimit-above 64)"
  fi
}
fw_remove() {
  [ "$TEST" = 1 ] && return 0
  local p f
  if ufw_active; then
    for p in $FW_PORTS; do ufw --force delete allow "$p/tcp" >/dev/null 2>&1 || true; done
    for f in /etc/ufw/before.rules /etc/ufw/before6.rules; do
      if grep -q "hstun-connlimit $UNIT begin" "$f" 2>/dev/null; then
        sed -i "/# hstun-connlimit $UNIT begin/,/# hstun-connlimit $UNIT end/d" "$f"
        ufw reload >/dev/null
      fi
    done
  fi
}

# ---------- rendering ----------
render_units() { # prints nothing; writes unit files
  local cap=""
  if [ "$ROLE" = exit ] && [ "$MODE" = direct ] && [ "$PORT" -lt 1024 ] 2>/dev/null; then
    cap=$'AmbientCapabilities=CAP_NET_BIND_SERVICE\nCapabilityBoundingSet=CAP_NET_BIND_SERVICE'
  else
    cap=$'AmbientCapabilities=\nCapabilityBoundingSet='
  fi
  local sub=server; [ "$ROLE" = entry ] && sub=client
  cat >"$SYSTEMD_DIR/$UNIT-$sub.service" <<EOF
[Unit]
Description=hstun $sub ($ROLE${NAME:+, $NAME})
After=network-online.target
Wants=network-online.target

[Service]
User=$SVC_USER
Group=$SVC_USER
ExecStart=$BIN_DIR/hstun $sub -config $ETC/$sub.toml
Restart=always
RestartSec=3
LimitNOFILE=8192
MemoryMax=512M
TasksMax=4096
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
$cap
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectClock=true
ProtectHostname=true
ProcSubset=pid
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
UMask=0077

[Install]
WantedBy=multi-user.target
EOF
}
render_nginx_units() {
  local nb; nb=$(command -v nginx || echo /usr/sbin/nginx)
  cat >"$SYSTEMD_DIR/$UNIT-nginx.service" <<EOF
[Unit]
Description=nginx in front of hstun ($UNIT)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
RuntimeDirectory=$UNIT-nginx
RuntimeDirectoryMode=0755
ExecStartPre=$nb -t -c $ETC/nginx.conf
ExecStart=$nb -c $ETC/nginx.conf -g 'daemon off;'
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
RestartSec=3
LimitNOFILE=8192
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
ProtectKernelModules=yes
LockPersonality=yes
RestrictRealtime=yes

[Install]
WantedBy=multi-user.target
EOF
  cat >"$SYSTEMD_DIR/$UNIT-nginx-reload.service" <<EOF
[Unit]
Description=Reload the hstun nginx ($UNIT) so it picks up a renewed certificate

[Service]
Type=oneshot
ExecStart=/bin/systemctl reload $UNIT-nginx.service
EOF
  cat >"$SYSTEMD_DIR/$UNIT-nginx-reload.path" <<EOF
[Unit]
Description=Reload the hstun nginx ($UNIT) when its certificate changes

[Path]
PathChanged=$CERT
Unit=$UNIT-nginx-reload.service

[Install]
WantedBy=multi-user.target
EOF
  cat >"$SYSTEMD_DIR/$UNIT-nginx-reload.timer" <<EOF
[Unit]
Description=Weekly safety reload of the hstun nginx ($UNIT)

[Timer]
OnCalendar=weekly
RandomizedDelaySec=6h
Persistent=true
Unit=$UNIT-nginx-reload.service

[Install]
WantedBy=timers.target
EOF
}
render_nginx_conf() {
  local ngu=nobody
  getent passwd www-data >/dev/null 2>&1 && ngu=www-data
  [ "$ngu" = nobody ] && getent passwd nginx >/dev/null 2>&1 && ngu=nginx
  cat >"$ETC/nginx.conf" <<EOF
# nginx in front of hstun ($UNIT): ordinary TLS termination, a static decoy site, and a reverse proxy for exactly two paths.
user $ngu;
worker_processes 1;
pid /run/$UNIT-nginx/nginx.pid;
error_log stderr warn;
events { worker_connections 1024; }

http {
    access_log off;
    server_tokens off;
    include /etc/nginx/mime.types;
    default_type application/octet-stream;
    client_body_temp_path /run/$UNIT-nginx/body;
    proxy_temp_path /run/$UNIT-nginx/proxy;
    fastcgi_temp_path /run/$UNIT-nginx/fastcgi;
    uwsgi_temp_path /run/$UNIT-nginx/uwsgi;
    scgi_temp_path /run/$UNIT-nginx/scgi;

    limit_conn_zone \$binary_remote_addr zone=perip:1m;
    ssl_session_cache shared:SSL:2m;
    ssl_session_timeout 10m;
    sendfile on;
    keepalive_timeout 75s;
    client_header_timeout 10s;   # also bounds the TLS handshake
    client_body_timeout 20s;
    send_timeout 20s;

    server {
        listen 0.0.0.0:$PORT ssl http2;
$([ "$IPV6" = 1 ] && echo "        listen [::]:$PORT ssl http2;")
        server_name $DOMAIN;
        ssl_certificate     $CERT;
        ssl_certificate_key $KEY;
        ssl_protocols TLSv1.2 TLSv1.3;
        limit_conn perip 64;
        client_max_body_size 16m;

        root $DECOY_TARGET;
        index index.html;

        proxy_intercept_errors on;
        proxy_buffer_size 64k;
        proxy_buffers 4 64k;
        proxy_busy_buffers_size 64k;
        client_body_buffer_size 128k;
        error_page 404 @nf;
        location @nf { return 404; }

        location / {
            try_files \$uri \$uri/ =404;
        }

        # WebSocket carrier: only a real upgrade request reaches hstun.
        location = $WS_PATH {
            if (\$http_upgrade != "websocket") { return 404; }
            proxy_pass http://127.0.0.1:$BACKEND_PORT;
            proxy_http_version 1.1;
            proxy_set_header Upgrade \$http_upgrade;
            proxy_set_header Connection "upgrade";
            proxy_set_header Host \$host;
            proxy_set_header X-Real-IP \$remote_addr;
            proxy_buffering off;
            proxy_read_timeout 3600s;
            proxy_send_timeout 3600s;
        }

        # HTTP and chunk carriers: POST only, streamed without buffering.
        location ^~ $HTTP_PATH/ {
            if (\$request_method != POST) { return 404; }
            proxy_pass http://127.0.0.1:$BACKEND_PORT;
            proxy_http_version 1.1;
            proxy_set_header Connection "";
            proxy_set_header Host \$host;
            proxy_set_header X-Real-IP \$remote_addr;
            proxy_buffering off;
            proxy_request_buffering off;
            proxy_read_timeout 120s;
            proxy_send_timeout 120s;
        }
    }
}
EOF
}
check_limits() { # --max-mbit, --max-new-streams and --drain-timeout, whichever were given
  case "$MAX_MBIT" in "") ;; *[!0-9.]* | . | *.*.*) die "--max-mbit must be a number (megabits per second, 0 = no limit)" ;; esac
  case "$MAX_NEW_STREAMS" in "") ;; *[!0-9]*) die "--max-new-streams must be a whole number (new streams per second, 0 = no limit)" ;; esac
  case "$DRAIN_TIMEOUT" in "" | [0-9]*s | [0-9]*m) ;; *) die "--drain-timeout must look like 10s or 1m (0s = stop at once)" ;; esac
  case "$PADDING" in "" | off | light | strong) ;; *) die "--padding must be off, light or strong" ;; esac
  case "$TLS_FRAGMENT" in "" | off | tcp | record | both) ;; *) die "--tls-fragment must be off, tcp, record or both" ;; esac
  case "$SESSION_AGE" in "" | auto | off | [0-9]*m | [0-9]*h) ;; *) die "--session-age must be auto, off, or a duration like 30m or 2h (at least 1m)" ;; esac
  case "$TLS_FINGERPRINT" in "" | go | chrome | chrome_nopq | firefox | safari | ios | android) ;; *) die "--tls-fingerprint must be chrome, chrome_nopq, firefox, safari, ios, android or go" ;; esac
}
render_server_toml() {
  local listen tls="" trust="" allow="" lim=""
  check_limits
  [ -n "$MAX_MBIT" ] && lim="${lim}max_mbit_per_key = $MAX_MBIT
"
  [ -n "$MAX_NEW_STREAMS" ] && lim="${lim}max_new_streams_per_sec_per_key = $MAX_NEW_STREAMS
"
  [ -n "$DRAIN_TIMEOUT" ] && lim="${lim}drain_timeout = \"$DRAIN_TIMEOUT\"
"
  [ -n "$PADDING" ] && lim="${lim}padding = \"$PADDING\"
"
  case "$MODE" in
    nginx) listen="127.0.0.1:$BACKEND_PORT"; trust='trust_proxy  = "loopback"' ;;
    cloudflared) listen="127.0.0.1:$BACKEND_PORT"; trust='trust_proxy  = "cloudflared"' ;;
    direct)
      if [ "$IPV6" = 1 ]; then listen="[::]:$PORT"; else listen="0.0.0.0:$PORT"; fi # [::] is dual-stack (bindv6only=0 is checked)
      tls="tls_cert     = \"$ETC/cert.pem\"
tls_key      = \"$ETC/key.pem\""
      if [ -n "$ALLOW_FROM" ]; then
        local items="" c
        for c in ${ALLOW_FROM//,/ }; do items="$items\"$c\", "; done
        allow="allow_from    = [${items%, }]"
      fi ;;
  esac
  cat >"$ETC/server.toml" <<EOF
# hstun exit, generated by install.sh (mode: $MODE). Edit and run: systemctl restart $UNIT-server
[server]
role         = "exit"
listen       = "$listen"
$trust
$tls
$allow
carriers     = ["ws", "http"]
ws_path      = "$WS_PATH"
http_path    = "$HTTP_PATH"
decoy_dir    = "$DECOY_TARGET"
secret_files = ["$ETC/secret"]

max_conns       = 512
max_conns_per_ip = 64
max_sessions_per_key    = 16
max_streams_per_session = 256
auth_fail_per_min_per_ip = 10
idle_timeout    = "10m"
admin_listen    = "127.0.0.1:$ADMIN_PORT"
${lim}
[[forward]]
name   = "x1"
target = "$FORWARD_TARGET"

[socks_exit]
enabled      = false
deny_private = true
allow_ports  = []
EOF
}
render_client_toml() {
  local verify="tls_verify  = \"system\"" conn="" sni="" socks="" ch="" drain="" fp="" conns=$CONNS
  check_limits
  [ -n "$TLS_FINGERPRINT" ] && fp="tls_fingerprint = \"$TLS_FINGERPRINT\""
  [ -n "$TLS_FRAGMENT" ] && fp="${fp}
tls_fragment = \"$TLS_FRAGMENT\""
  [ -n "$PADDING" ] && fp="${fp}
padding = \"$PADDING\""
  [ -n "$SESSION_AGE" ] && fp="${fp}
session_age = \"$SESSION_AGE\""
  [ -n "$DRAIN_TIMEOUT" ] && drain="drain_timeout = \"$DRAIN_TIMEOUT\""
  [ "$TLS_VERIFY" = spki ] && verify="tls_verify  = \"spki\"
spki_sha256 = \"$SPKI\""
  [ -n "$CONNECT" ] && conn="connect     = \"$CONNECT\""
  [ -n "$SNI" ] && sni="sni         = \"$SNI\""
  [ "$CARRIER" = chunk ] && ch="chunk_parallel = $CHUNK_PARALLEL"
  [ -n "$SOCKS_LISTEN" ] && socks="
[socks]
enabled = true
listen  = \"$SOCKS_LISTEN\""
  cat >"$ETC/client.toml" <<EOF
# hstun entry, generated by install.sh. Edit and run: systemctl restart $UNIT-client
[client]
role        = "entry"
server      = "$SERVER"
$conn
$sni
carrier     = "$CARRIER"
ws_path     = "$WS_PATH"
http_path   = "$HTTP_PATH"
conns       = $conns
$ch
secret_file = "$ETC/secret"
name        = "$(hostname -s 2>/dev/null || echo entry)${NAME:+-$NAME}"
$verify
$fp
ping        = "25s"
idle_timeout = "10m"
max_streams_per_session = 256
forward_max_conns_per_ip = 64
admin_listen = "127.0.0.1:$ADMIN_PORT"
$drain

[[forward]]
name   = "$FORWARD_NAME"
listen = "$FORWARD_LISTEN"
$socks
EOF
}
check_conf() { # file mode
  "$BIN_DIR/hstun" check -config "$1" -mode "$2" >/dev/null || die "generated config is invalid: $1"
}
install_decoy() {
  if [ -n "$DECOY_DIR" ]; then DECOY_TARGET="$DECOY_DIR"; return 0; fi
  [ -d "$DECOY_TARGET" ] && return 0
  install -d -m 0755 "$DECOY_TARGET"
  cp "$SHARE_DIR/decoy-example/"* "$DECOY_TARGET/"
  # a decoy identical on every host is itself a fingerprint: give each host its own title
  local titles=("Workshop notes" "Garden log" "Bench notes" "Study notes" "Trail journal" "Kitchen notes") t
  t=${titles[$((RANDOM % ${#titles[@]}))]}
  sed -i "s/Field notes/$t/g" "$DECOY_TARGET/index.html" "$DECOY_TARGET/notes.html" "$DECOY_TARGET/about.html" 2>/dev/null || true
  chmod 0644 "$DECOY_TARGET"/*
}

# ---------- roles ----------
prepare_dirs() {
  install -d -m 0750 "$ETC"
  own "root:$SVC_USER" "$ETC"
}
make_secret() {
  if [ -s "$ETC/secret" ]; then info "keeping the existing secret"; return 0; fi
  "$BIN_DIR/hstun" keygen -out "$ETC/secret" >/dev/null
  own "$SVC_USER:$SVC_USER" "$ETC/secret"; chmod 0600 "$ETC/secret"
}
detect_addr() {
  local a
  a=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)
  [ -n "$a" ] || a=$(ip -6 route get 2606:4700:4700::1111 2>/dev/null | sed -n 's/.* src \([0-9a-f:]*\).*/\1/p' | head -1)
  echo "${a:-YOUR.SERVER.IP}"
}
# the host part of host:port with brackets for an IPv6 literal
bracket() { case "$1" in \[*) echo "$1" ;; *:*) echo "[$1]" ;; *) echo "$1" ;; esac; }
unbracket() { local h=$1; h=${h#[}; echo "${h%]}"; }
# true when this host can serve IPv6 on a dual-stack socket: IPv6 enabled, a global address, bindv6only=0
ipv6_ok() {
  [ -d /proc/sys/net/ipv6 ] || return 1
  [ "$(cat /proc/sys/net/ipv6/conf/all/disable_ipv6 2>/dev/null || echo 1)" = 0 ] || return 1
  [ "$(cat /proc/sys/net/ipv6/bindv6only 2>/dev/null || echo 0)" = 0 ] || return 1
  ip -6 addr show scope global 2>/dev/null | grep -q 'inet6 '
}
want_ipv6() { # sets IPV6 to 1 or 0 once (flag, saved value, or detection)
  [ -n "$IPV6" ] && return 0
  if [ "$TEST" = 1 ]; then IPV6=0; elif ipv6_ok; then IPV6=1; else IPV6=0; fi
}
make_join() { # [secret]: prints the join string (everything the entry needs, secret only when asked) or, with "secret", the pair code
  local host=${DOMAIN:-${PUBLIC_ADDR:-$(detect_addr)}} srv port=$PORT tls=system spki="" car=${CARRIER:-ws}
  [ "$MODE" = cloudflared ] && { port=443; car=chunk; }
  srv="$(bracket "$host"):$port"
  if [ "$MODE" = direct ] && [ -z "$DOMAIN" ] || { [ "$MODE" = direct ] && [ -n "$SELF_SIGNED" ]; }; then tls=spki; spki=$(spki_of "$ETC/cert.pem"); fi
  local text kind=join
  text=$(printf 'server=%s\ncarrier=%s\nws_path=%s\nhttp_path=%s\ntls=%s\nspki=%s\n' "$srv" "$car" "$WS_PATH" "$HTTP_PATH" "$tls" "$spki")
  if [ "${1:-}" = secret ]; then
    local sf=${SECRET_OVERRIDE:-$ETC/secret}
    [ -r "$sf" ] || die "cannot read $sf (run as root)"
    text=$(printf '%s\nsecret=%s\n' "$text" "$(tr -d '\n' <"$sf")"); kind=pair
  fi
  printf 'hstun-%s:%s' "$kind" "$(printf '%s\n' "$text" | base64 -w0)"
}
start_unit() { # unit
  sysd daemon-reload
  sysd enable --now "$1" >/dev/null 2>&1 || sysd restart "$1"
  sysd restart "$1"
}

install_exit() {
  need_root
  set_paths; existing_prompt || return 0; load_conf; guard_existing exit
  ROLE="exit"
  if [ -z "$MODE" ]; then
    if [ "$YES" != 1 ] && [ -n "$TTY" ]; then
      echo "How should the exit be reached?"
      echo "  1) nginx   public TLS port with your domain and a real certificate, nginx answers like a website (recommended)"
      echo "  2) direct  hstun itself listens publicly (self-signed certificate pinned by the entry, or your own certificate)"
      echo "  3) cloudflared  hstun listens on loopback behind a local Cloudflare Tunnel"
      local c=""; ask c "Choose 1, 2 or 3" 1
      case "$c" in 1) MODE=nginx ;; 2) MODE=direct ;; 3) MODE=cloudflared ;; *) die "choose 1, 2 or 3" ;; esac
    else MODE=nginx; fi
  fi
  case "$MODE" in nginx | direct | cloudflared) ;; *) die "--mode must be nginx, direct or cloudflared" ;; esac
  ask FORWARD_TARGET "Where should tunnelled traffic go on this host? Your proxy's address, for example a VLESS inbound on 127.0.0.1:19990" ""
  case "$FORWARD_TARGET" in *:*) ;; *) die "--forward-target must look like 127.0.0.1:19990" ;; esac
  ADMIN_PORT=${ADMIN_PORT:-$(free_port 9109)}

  case "$MODE" in
    nginx)
      ask DOMAIN "Domain that points at this host (for the certificate)" ""
      ask PORT "Public TLS port" "$(port_busy 443 && echo 2083 || echo 443)"
      BACKEND_PORT=${BACKEND_PORT:-$(free_port 24083)}
      ensure_cmds nginx openssl ;;
    direct)
      ask PORT "Public TLS port" "$(port_busy 443 && echo 2083 || echo 443)"
      ask DOMAIN "Domain that points at this host (empty = self-signed certificate, pinned by the entry)" "" opt
      if [ -z "$ALLOW_FROM" ] && [ "$YES" != 1 ] && [ -n "$TTY" ] && ! confirm "Only accept connections from specific IP addresses?" n; then :
      else ask ALLOW_FROM "Allowed addresses as CIDRs, comma separated (empty = anyone)" "" opt; fi
      ensure_cmds openssl ;;
    cloudflared)
      ask BACKEND_PORT "Loopback port for the cloudflared ingress" "$(free_port 18444)"
      ask DOMAIN "Cloudflare hostname that will reach this exit (for the printed snippet)" "" opt
      PORT=${PORT:-$BACKEND_PORT}
      ensure_cmds openssl ;;
  esac
  [ "$MODE" != cloudflared ] && port_busy "$PORT" && ! [ -f "$ETC/install.conf" ] && die "port $PORT is already in use; pick another with --port"

  fetch_release; install_files; ensure_user_account
  prepare_dirs
  WS_PATH=${WS_PATH:-$(rand_path)}; HTTP_PATH=${HTTP_PATH:-$(rand_path)}
  make_secret
  ADMIN_PORT=${ADMIN_PORT:-9109}

  case "$MODE" in
    nginx)
      if [ -z "$CERT" ] || [ -z "$KEY" ]; then
        if [ -n "$ACME" ] || { [ "$YES" != 1 ] && [ -n "$TTY" ] && confirm "Get a Let's Encrypt certificate for $DOMAIN now (certbot, needs port 80 free)?" y; }; then cert_acme
        else die "give --cert and --key (existing certificate files) or use --acme"; fi
      fi
      [ -r "$CERT" ] && [ -r "$KEY" ] || die "certificate files not readable: $CERT / $KEY" ;;
    direct)
      if [ -n "$CERT" ] && [ -n "$KEY" ]; then cp "$CERT" "$ETC/cert.pem"; cp "$KEY" "$ETC/key.pem"
      elif [ -n "$DOMAIN" ] && [ -n "$ACME" ]; then cert_acme; cp "$CERT" "$ETC/cert.pem"; cp "$KEY" "$ETC/key.pem"
      elif [ -s "$ETC/cert.pem" ] && [ -s "$ETC/key.pem" ] && [ -n "$SELF_SIGNED" ] && [ "$NEW_CERT" != 1 ]; then
        info "keeping the existing certificate (entries that pinned it keep working)"
      else SELF_SIGNED=1; cert_selfsigned "$ETC"; fi
      own "$SVC_USER:$SVC_USER" "$ETC/cert.pem" "$ETC/key.pem"; chmod 0600 "$ETC/key.pem" ;;
  esac

  want_ipv6
  install_decoy
  render_server_toml
  own "root:$SVC_USER" "$ETC/server.toml"; chmod 0640 "$ETC/server.toml"
  check_conf "$ETC/server.toml" server
  render_units
  if [ "$MODE" = nginx ]; then
    render_nginx_conf; own "root:root" "$ETC/nginx.conf"; chmod 0644 "$ETC/nginx.conf"
    render_nginx_units
  fi
  if [ "$MODE" = direct ] && [ -n "$DOMAIN" ] && [ -n "$ACME" ] && [ -d /etc/letsencrypt ] && ! [ "$TEST" = 1 ]; then
    install -d /etc/letsencrypt/renewal-hooks/deploy
    cat >"/etc/letsencrypt/renewal-hooks/deploy/$UNIT.sh" <<EOF
#!/bin/sh
# copy the renewed certificate for hstun and restart it (hstun reads the certificate at start)
cp /etc/letsencrypt/live/$DOMAIN/fullchain.pem $ETC/cert.pem && cp /etc/letsencrypt/live/$DOMAIN/privkey.pem $ETC/key.pem
chown $SVC_USER:$SVC_USER $ETC/cert.pem $ETC/key.pem && chmod 0600 $ETC/key.pem
systemctl restart $UNIT-server.service
EOF
    chmod 0755 "/etc/letsencrypt/renewal-hooks/deploy/$UNIT.sh"
  fi

  case "$MODE" in
    nginx) fw_allow "$PORT"; fw_connlimit "$PORT" ;;
    direct) fw_allow "$PORT"; [ -z "$ALLOW_FROM" ] && fw_connlimit "$PORT" || true ;;
  esac
  save_conf
  start_unit "$UNIT-server.service"
  if [ "$MODE" = nginx ]; then
    sysd daemon-reload
    sysd enable --now "$UNIT-nginx.service" >/dev/null 2>&1 || true
    sysd restart "$UNIT-nginx.service"
    sysd enable --now "$UNIT-nginx-reload.path" "$UNIT-nginx-reload.timer" >/dev/null 2>&1 || true
    if have_systemd; then
      sleep 2
      port_busy "$PORT" || warn "nothing is listening on $PORT yet: check 'systemctl status $UNIT-nginx' and 'journalctl -u $UNIT-nginx'"
    fi
  fi
  save_conf
  print_exit_summary
}
print_exit_summary() {
  local pair; pair=$(make_join secret)
  echo
  info "${B}exit installed${N} (mode $MODE${NAME:+, instance $NAME})"
  echo "  config   $ETC/server.toml    secret $ETC/secret    forward x1 -> $FORWARD_TARGET"
  echo "  manage   hstun-ctl${NAME:+ --name $NAME} status | logs | restart | update | show-join | uninstall"
  if [ "$MODE" = cloudflared ]; then
    echo
    echo "  Point your cloudflared ingress at hstun (plain HTTP on loopback), then restart cloudflared:"
    echo "    - hostname: ${DOMAIN:-tunnel.example.org}"
    echo "      service: http://127.0.0.1:$BACKEND_PORT"
    echo "  Clients on a path that cuts long connections should use --carrier chunk."
  fi
  echo
  echo "  On the entry host run this one command (the code contains the secret: send it only over a private channel):"
  echo
  echo "    hstun-ctl entry --join '$pair'"
  echo
  echo "  If hstun-ctl is not installed there yet, use the installer of this exact version (same release on both hosts):"
  echo "    bash <(curl -Ls https://raw.githubusercontent.com/$REPO/v$INSTALLER_VERSION/install.sh) entry --join '...'"
  echo "  Print it again any time:  hstun-ctl${NAME:+ --name $NAME} show-join      (without the secret: show-join --no-secret)"
  echo "  Next: make sure your proxy (for example a VLESS inbound) is listening on $FORWARD_TARGET."
  if [ "$MODE" = direct ] && [ -n "$SELF_SIGNED" ] && [ -z "$DOMAIN" ]; then
    echo "  Tip: an IP address with a self-signed certificate is the easiest setup to recognise. With a domain, nginx mode looks like an ordinary website."
  fi
}
ensure_user_account() {
  [ "$TEST" = 1 ] && return 0
  id "$SVC_USER" >/dev/null 2>&1 && return 0
  local uid=61999 sh; sh=$(command -v nologin || echo /usr/sbin/nologin)
  while getent passwd "$uid" >/dev/null || getent group "$uid" >/dev/null; do uid=$((uid - 1)); done
  # a uid above SYS_UID_MAX makes useradd warn; raise the limit for this call only
  useradd -K SYS_UID_MAX=65000 --system --uid "$uid" --user-group --no-create-home --shell "$sh" "$SVC_USER" 2>/dev/null \
    || useradd --system --uid "$uid" --user-group --no-create-home --shell "$sh" "$SVC_USER"
  info "created the service account $SVC_USER (uid $uid)"
}

parse_join() {
  local raw=${JOIN#hstun-join:} text k v
  raw=${raw#hstun-pair:}
  text=$(printf '%s' "$raw" | base64 -d 2>/dev/null) || die "the join string is not valid base64"
  while IFS='=' read -r k v; do
    case "$k" in
      server) [ -n "$SERVER" ] || SERVER=$v ;;
      carrier) [ -n "$CARRIER" ] || CARRIER=$v ;;
      ws_path) [ -n "$WS_PATH" ] || WS_PATH=$v ;;
      http_path) [ -n "$HTTP_PATH" ] || HTTP_PATH=$v ;;
      tls) [ -n "$TLS_VERIFY" ] || TLS_VERIFY=$v ;;
      spki) [ -n "$SPKI" ] || SPKI=$v ;;
      secret) JOIN_SECRET=$v ;;
    esac
  done <<<"$text"
}
install_entry() {
  need_root
  set_paths; existing_prompt || return 0; load_conf; guard_existing entry
  ROLE=entry
  [ -n "$JOIN" ] && parse_join
  ask SERVER "Exit address (host:port)" ""
  ask CARRIER "Carrier (ws, http, chunk or auto; chunk is for paths that cut long connections, auto fails over by itself)" ws
  case "$CARRIER" in ws | http | chunk | auto) ;; *) die "--carrier must be ws, http, chunk or auto" ;; esac
  ask WS_PATH "WebSocket path (from the join string)" ""
  ask HTTP_PATH "HTTP path (from the join string)" ""
  ask TLS_VERIFY "Certificate check (system = CA certificate, spki = pinned self-signed)" system
  [ "$TLS_VERIFY" = spki ] && ask SPKI "SPKI sha256 of the exit certificate (hex)" ""
  case "$TLS_VERIFY" in system | spki) ;; *) die "--tls-verify must be system or spki" ;; esac
  local adv=0 who=""
  if [ "$CUSTOMIZE" = 1 ]; then adv=1
  elif [ "$YES" != 1 ] && [ -n "$TTY" ] && [ -z "$CONNECT$SNI$FORWARD_LISTEN$FORWARD_NAME" ] \
    && confirm "Change advanced options (dial address, TLS name, forward name, listen address)?" n; then adv=1; fi
  if [ "$adv" = 1 ]; then
    ask CONNECT "Dial this ip:port instead of resolving the name (empty = resolve)" "" opt
    ask SNI "TLS server name (empty = the host of the exit address)" "" opt
    ask FORWARD_LISTEN "Listen address for your proxy clients (host:port)" "0.0.0.0:20151"
    ask FORWARD_NAME "Forward name (must match the exit; default x1)" x1
  else
    FORWARD_NAME=${FORWARD_NAME:-x1}
    if [ -z "$FORWARD_LISTEN" ]; then
      if [ "$YES" != 1 ] && [ -n "$TTY" ]; then
        local dp=${DEF_FORWARD_LISTEN##*:}; dp=${dp:-$(free_port 20151)}
        echo "Who will connect to the tunnel on this host?"
        echo "  1) anyone: users connect to this host's public port $dp (the installer opens it in ufw)"
        echo "  2) only programs on this host (127.0.0.1)"
        ask who "Choose" 1
        case "$who" in 1) FORWARD_LISTEN="0.0.0.0:$dp" ;; 2) FORWARD_LISTEN="127.0.0.1:$dp" ;; *) die "choose 1 or 2" ;; esac
      else FORWARD_LISTEN="127.0.0.1:20151"; fi
    fi
  fi
  if [ "$CARRIER" = chunk ]; then CONNS=${CONNS:-1}; ask CHUNK_PARALLEL "Parallel requests for the chunk carrier" 8; else CONNS=${CONNS:-2}; fi
  ADMIN_PORT=${ADMIN_PORT:-$(free_port 9110)}
  case "$FORWARD_LISTEN" in *:*) ;; *) die "--forward-listen must look like 127.0.0.1:20151" ;; esac
  local lp=${FORWARD_LISTEN##*:}
  if ! [ -f "$ETC/install.conf" ] && port_busy "$lp"; then die "port $lp is already in use; pick another with --forward-listen"; fi

  fetch_release; install_files; ensure_user_account
  prepare_dirs
  if [ ! -s "$ETC/secret" ] || [ -n "$SECRET_FILE$SECRET_FROM$JOIN_SECRET" ]; then
    if [ -n "$SECRET_FILE" ]; then install -m 0600 "$SECRET_FILE" "$ETC/secret"
    elif [ -n "$JOIN_SECRET" ]; then
      ( umask 077; printf '%s\n' "$JOIN_SECRET" >"$ETC/secret" )
    elif [ -n "$SECRET_FROM" ]; then
      ensure_cmds ssh:openssh-client
      info "fetching the secret from $SECRET_FROM over ssh (it is not printed)"
      local target=${SECRET_FROM%%:*} sport=""; case "$SECRET_FROM" in *:*) sport=${SECRET_FROM##*:} ;; esac
      ( umask 077; ssh ${sport:+-p "$sport"} "$target" "cat ${REMOTE_SECRET:-/etc/hstun/secret} 2>/dev/null || sudo -n cat ${REMOTE_SECRET:-/etc/hstun/secret}" >"$ETC/secret" ) || die "could not fetch the secret from $SECRET_FROM"
    else
      local s=""; ask_secret s "This join string has no secret. Paste the secret (hstun-ctl show-secret on the exit; input is hidden)"
      [ -n "$s" ] || die "empty secret"
      ( umask 077; printf '%s\n' "$s" >"$ETC/secret" )
    fi
    own "$SVC_USER:$SVC_USER" "$ETC/secret"; chmod 0600 "$ETC/secret"
  else info "keeping the existing secret"; fi
  [ -s "$ETC/secret" ] || die "the secret file is empty"

  render_client_toml
  own "root:$SVC_USER" "$ETC/client.toml"; chmod 0640 "$ETC/client.toml"
  check_conf "$ETC/client.toml" client
  render_units
  case "$FORWARD_LISTEN" in 0.0.0.0:* | \[::\]:* | :*) fw_allow "$lp" ;; esac
  save_conf
  start_unit "$UNIT-client.service"
  save_conf
  info "${B}entry installed${N}${NAME:+ (instance $NAME)}: $FORWARD_LISTEN -> $SERVER, carrier $CARRIER$(case "$CARRIER" in ws) echo " (WebSocket)" ;; auto) echo " (ws, then http, then chunk)" ;; esac), forward $FORWARD_NAME"
  case "${SERVER%:*}" in *[!0-9.]* | "") ;; *) [ "$TLS_VERIFY" = spki ] && echo "  note     the exit is an IP address, so no TLS server name is sent. That is normal with a pinned certificate." ;; esac
  if have_systemd || [ "$TEST" = 1 ]; then
    local n=0
    for _ in $(seq 1 30); do
      n=$(curl -s -m 2 "http://127.0.0.1:$ADMIN_PORT/stats" 2>/dev/null | grep -o '"id":' | wc -l || true)
      [ "${n:-0}" -ge 1 ] && break
      sleep 1
    done
    if [ "${n:-0}" -ge 1 ]; then info "tunnel is up ($n session(s))"; else warn "no session yet; check: hstun-ctl${NAME:+ --name $NAME} logs"; fi
  fi
  echo "  manage   hstun-ctl${NAME:+ --name $NAME} status | logs | restart | update | uninstall"
}

# ---------- management ----------
units_of_instance() {
  local u
  if [ -n "$ADOPTED" ]; then for u in $ADOPT_UNITS; do echo "$u"; done; return 0; fi
  for u in server client nginx nginx-reload nginx-reload.path nginx-reload.timer; do
    [ -f "$SYSTEMD_DIR/$UNIT-$u.service" ] && echo "$UNIT-$u.service"
    [ -f "$SYSTEMD_DIR/$UNIT-$u" ] && echo "$UNIT-$u"
  done
}
daemon_units() { # the unit that runs the hstun process
  local u
  if [ -n "$ADOPTED" ]; then echo "${ADOPT_UNITS%% *}"; return 0; fi
  for u in $(units_of_instance); do case "$u" in *-server.service | *-client.service) echo "$u" ;; esac; done
}
service_units() { # hstun process plus the nginx in front of it
  daemon_units
  local u
  for u in $(units_of_instance); do
    if [ -n "$ADOPTED" ]; then
      case "$u" in "${ADOPT_UNITS%% *}" | *reload* | *.path | *.timer) ;; *) echo "$u" ;; esac
    else
      case "$u" in *-nginx.service) echo "$u" ;; esac
    fi
  done
}
cmd_status() {
  set_paths; need_installed
  echo "instance   ${NAME:-default}   role $ROLE${MODE:+   mode $MODE}   config $ETC${ADOPTED:+   (adopted)}"
  echo "binary     $("$(inst_bin)" version 2>/dev/null || echo missing)   $(inst_bin)"
  if [ "$ROLE" = entry ]; then echo "carrier    ${CARRIER:-ws}   server ${SERVER:-?}   listens ${FORWARD_LISTEN:-?}"
  else echo "carriers   ws, http   public ${PORT:-?}${DOMAIN:+   domain $DOMAIN}   forwards to ${FORWARD_TARGET:-?}"; fi
  local u
  for u in $(units_of_instance); do printf '%-40s %s\n' "$u" "$(systemctl is-active "$u" 2>/dev/null || true)"; done
  if [ -n "$ADMIN_PORT" ]; then
    local st; st=$(curl -s -m 3 "http://127.0.0.1:$ADMIN_PORT/stats" 2>/dev/null || true)
    if [ -n "$st" ]; then
      if have jq; then jq -c '{sessions: (.sessions|length), streams_opened, stream_errors, handshake_ok, handshake_fail}' <<<"$st"; else echo "$st"; fi
    else echo "admin      http://127.0.0.1:$ADMIN_PORT/stats not answering"; fi
  fi
}
cmd_logs() {
  set_paths; need_installed
  local args=(-n 100 --no-pager); [ "$FOLLOW" = 1 ] && args=(-n 50 -f)
  local u us=()
  for u in $(service_units); do us+=(-u "$u"); done
  journalctl "${us[@]}" "${args[@]}"
}
cmd_restart() {
  need_root; set_paths; need_installed
  local u
  for u in $(service_units); do sysd restart "$u"; info "restarted $u"; done
}
# the new binary must accept this instance's configuration before anything is replaced
preflight_conf() {
  local m=server; [ "$ROLE" = entry ] && m=client
  "$STAGE/hstun" check -config "$ETC/$m.toml" -mode "$m" >/dev/null 2>&1 \
    || die "the new binary rejects $ETC/$m.toml (instance ${NAME:-default}); nothing was changed. Run: $STAGE/hstun check -config $ETC/$m.toml"
}
pinned_version() { cat "$ETC_BASE/hstun.pin" 2>/dev/null | head -1 || true; }
cmd_pin() { # pin VERSION | pin --clear | pin (shows)
  local v=${FILE:-}
  if [ "$FINISH" = 1 ] || [ "$v" = clear ]; then need_root; rm -f "${ETC_BASE:?}/hstun.pin"; info "unpinned: update follows the latest release again"; return 0; fi
  if [ -z "$v" ]; then
    local cur; cur=$(pinned_version)
    if [ -n "$cur" ]; then info "pinned to $cur (hstun-ctl pin --clear to follow the latest release)"; else info "not pinned: update follows the latest release"; fi
    return 0
  fi
  need_root
  v=${v#v}
  [[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "a version looks like 0.4.0"
  install -d "$ETC_BASE"
  printf '%s\n' "$v" >"$ETC_BASE/hstun.pin"
  info "pinned to $v: hstun-ctl update installs exactly this release (an explicit --version still wins; pin --clear to follow the latest)"
}
latest_version() { # newest release without the leading v; empty when it cannot be read
  if [ -n "${HSTUN_TEST_LATEST:-}" ]; then echo "$HSTUN_TEST_LATEST"; return 0; fi # test hook
  have jq || return 0
  local json
  json=$(gh_curl -m 4 -H "Accept: application/vnd.github+json" "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null) || return 0
  jq -r '.tag_name // empty' <<<"$json" | sed 's/^v//'
}
cmd_update() {
  need_root; set_paths; need_installed
  local before; before=$("$(inst_bin)" version 2>/dev/null || true)
  if [ -z "${HSTUN_TARBALL:-}" ] && [ "${HSTUN_BUILD:-0}" != 1 ] && [ "$FORCE" != 1 ]; then
    ensure_cmds curl jq
    local want=${WANT_VERSION#v} cur=${before#hstun }
    [ -n "$want" ] || want=$(pinned_version)
    [ -n "$want" ] || want=$(latest_version)
    if [ -n "$want" ] && [ "$want" = "$cur" ]; then info "already up to date ($cur); nothing was changed (--force reinstalls it)"; return 0; fi
  fi
  [ -n "$WANT_VERSION" ] || WANT_VERSION=$(pinned_version)
  fetch_release
  local dirs=() d u
  if [ "$ALL" = 1 ]; then
    for d in "$ETC_BASE"/hstun "$ETC_BASE"/hstun-*; do [ -f "$d/install.conf" ] && dirs+=("$d"); done
  else dirs=("$ETC"); fi
  for d in "${dirs[@]}"; do ( reset_conf; NAME=${d##*/hstun}; NAME=${NAME#-}; set_paths; load_conf; preflight_conf ); done
  install_files
  local failed=0 rc
  for d in "${dirs[@]}"; do
    set +e
    (
      set -e
      reset_conf; NAME=${d##*/hstun}; NAME=${NAME#-}; set_paths; load_conf
      if [ -n "$ADOPT_BIN" ] && [ "$ADOPT_BIN" != "$BIN_DIR/hstun" ]; then
        if [ -x "$ADOPT_BIN" ] && ! cmp -s "$ADOPT_BIN" "$BIN_DIR/hstun"; then cp -p "$ADOPT_BIN" "$ADOPT_BIN.prev"; fi
        install -m 0755 "$BIN_DIR/hstun" "$ADOPT_BIN"
      fi
      for u in $(daemon_units); do sysd restart "$u"; done
      if ! health_ok; then
        warn "instance ${NAME:-default} did not come back healthy with the new binary: going back to the previous one"
        [ -f "$BIN_DIR/hstun.prev" ] && put_file "$BIN_DIR/hstun.prev" "$BIN_DIR/hstun"
        [ -n "$ADOPT_BIN" ] && [ -f "$ADOPT_BIN.prev" ] && put_file "$ADOPT_BIN.prev" "$ADOPT_BIN"
        for u in $(daemon_units); do sysd restart "$u"; done
        exit 1
      fi
    )
    rc=$?; set -e
    [ "$rc" = 0 ] || failed=1
  done
  [ "$failed" = 0 ] || die "the update was rolled back for at least one instance; the previous version is running again (see the warning above and: hstun-ctl logs)"
  [ "$ALL" = 1 ] || info "restarted this instance only; other instances keep running the old binary until restarted (hstun-ctl update --all)"
  info "updated: $before -> $("$(inst_bin)" version)   (previous binary kept as $(inst_bin).prev)"
}
cmd_show_join() {
  set_paths; need_installed
  [ "$ROLE" = exit ] || die "show-join is for an exit"
  if [ "$NO_SECRET" = 1 ]; then make_join; else warn "this code contains the secret: send it only over a private channel (--no-secret prints the version without it)"; make_join secret; fi
  echo
}
cmd_show_secret() {
  set_paths; need_installed
  [ -r "$ETC/secret" ] || die "cannot read $ETC/secret (run as root)"
  cat "$ETC/secret"
}
cmd_uninstall() {
  need_root; set_paths; need_installed
  confirm "Remove hstun instance ${NAME:-default} ($ROLE)?" n || { info "nothing removed"; return 0; }
  local u
  for u in $(units_of_instance); do
    sysd disable --now "$u" >/dev/null 2>&1 || true
    rm -f "${SYSTEMD_DIR:?}/${u:?}"
  done
  sysd daemon-reload
  fw_remove
  rm -f "/etc/letsencrypt/renewal-hooks/deploy/${UNIT:?}.sh"
  [ -n "$ADOPTED" ] && info "firewall rules and certificates of an adopted instance were not created by hstun-ctl and are left in place"
  if [ "$PURGE" = 1 ]; then
    [ -n "$ADOPT_BIN" ] && [ "$ADOPT_BIN" != "$BIN_DIR/hstun" ] && rm -f "$ADOPT_BIN" "$ADOPT_BIN.prev"
    rm -rf "${ETC:?}"; [ -d "$DECOY_TARGET" ] && [ -z "$DECOY_DIR" ] && rm -rf "${DECOY_TARGET:?}"
    info "removed $ETC (configuration, secret and certificate)"
    local left=0 d
    for d in "$ETC_BASE"/hstun "$ETC_BASE"/hstun-*; do [ -f "$d/install.conf" ] && left=$((left + 1)); done
    if [ "$left" = 0 ]; then rm -f "${BIN_DIR:?}/hstun" "$BIN_DIR/hstun.prev" "$BIN_DIR/hstun-ctl"; rm -rf "${SHARE_DIR:?}"; info "no instances left: removed the binary and shared files"; fi
  else
    [ -n "$ADOPTED" ] && rm -f "${ETC:?}/install.conf"
    info "services removed; configuration and secret kept in $ETC (add --purge to delete them)"
  fi
}

# ---------- adopting hand-installed instances ----------
# toml_val FILE KEY [SECTION]: first `KEY = value` (quotes and trailing comments stripped); with SECTION only inside [SECTION] / [[SECTION]]
toml_val() {
  awk -v k="$2" -v want="${3:-}" '
    /^[ \t]*\[/ { sec=$0; gsub(/[][ \t]/, "", sec); next }
    want != "" && sec != want { next }
    { line=$0; sub(/^[ \t]+/, "", line) }
    index(line, k) == 1 { rest=substr(line, length(k) + 1)
      if (rest ~ /^[ \t]*=/) { sub(/^[ \t]*=[ \t]*/, "", rest); sub(/[ \t]*#.*$/, "", rest); gsub(/^"|"$/, "", rest); print rest; exit } }' "$1"
}
nginx_val() { awk -v k="$2" '{ for (i = 1; i < NF; i++) if ($i == k) { v=$(i + 1); sub(/;.*$/, "", v); n=split(v, a, ":"); print a[n]; exit } }' "$1"; }
# prints "unitfile|binary|server-or-client|config" for every hstun unit under SYSTEMD_DIR
adopt_candidates() {
  local f line bin sub cfg
  for f in "$SYSTEMD_DIR"/hstun*.service; do
    [ -f "$f" ] || continue
    line=$(sed -n 's/^ExecStart=//p' "$f" | head -1)
    # shellcheck disable=SC2086  # word splitting of the ExecStart line is the point
    set -- $line
    bin=${1:-}; sub=${2:-}
    case "${bin##*/}" in hstun | hstun-*) ;; *) continue ;; esac
    case "$sub" in server | client) ;; *) continue ;; esac
    cfg=""; shift 2
    while [ $# -gt 0 ]; do
      case "$1" in -config | --config) cfg=${2:-}; shift ;; -config=* | --config=*) cfg=${1#*=} ;; esac
      shift
    done
    [ -n "$cfg" ] && echo "${f##*/}|$bin|$sub|$cfg"
  done
}
# adopt_one UNITFILE BIN SUB CONFIG: runs in a subshell; writes only $ETC/install.conf, never touches units, config, binary or firewall
adopt_one() {
  local unitfile=$1 bin=$2 sub=$3 cfg=$4 dir base f nginxu="" reload="" units
  dir=${cfg%/*}; base=${dir##*/}
  if [ "${dir%/*}" != "$ETC_BASE" ]; then warn "$unitfile: config $cfg is not under $ETC_BASE/hstun[-NAME]/, skipped"; return 1; fi
  case "$base" in hstun) NAME="" ;; hstun-*) NAME=${base#hstun-} ;; *) warn "$unitfile: config directory $dir is not hstun or hstun-NAME, skipped"; return 1 ;; esac
  case "$NAME" in *[!A-Za-z0-9_]*) warn "$unitfile: instance name '$NAME' has unsupported characters, skipped"; return 1 ;; esac
  case "$sub:${cfg##*/}" in server:server.toml | client:client.toml) ;; *) warn "$unitfile: expected $dir/$sub.toml, found ${cfg##*/}, skipped"; return 1 ;; esac
  set_paths
  if [ -f "$ETC/install.conf" ]; then info "$unitfile: already managed ($ETC)"; return 0; fi
  [ -r "$cfg" ] || { warn "$unitfile: cannot read $cfg (run as root)"; return 1; }
  [ -x "$bin" ] || { warn "$unitfile: $bin is not executable, skipped"; return 1; }
  "$bin" check -config "$cfg" -mode "$sub" >/dev/null 2>&1 || { warn "$unitfile: $bin does not accept $cfg, left alone"; return 1; }
  units=$unitfile
  if [ "$sub" = server ]; then
    ROLE="exit"
    local listen trust
    listen=$(toml_val "$cfg" listen server); trust=$(toml_val "$cfg" trust_proxy server)
    case "$trust" in
      loopback) MODE=nginx; BACKEND_PORT=${listen##*:} ;;
      cloudflared) MODE=cloudflared; BACKEND_PORT=${listen##*:} ;;
      *) MODE=direct; PORT=${listen##*:} ;;
    esac
    WS_PATH=$(toml_val "$cfg" ws_path server); HTTP_PATH=$(toml_val "$cfg" http_path server)
    FORWARD_TARGET=$(toml_val "$cfg" target forward)
    ADMIN_PORT=$(toml_val "$cfg" admin_listen server); ADMIN_PORT=${ADMIN_PORT##*:}
    DECOY_DIR=$(toml_val "$cfg" decoy_dir server)
    if [ "$MODE" = direct ] && [ -f "$ETC/cert.pem" ]; then SELF_SIGNED=1; fi
    if [ "$MODE" = nginx ]; then
      for f in "$SYSTEMD_DIR"/hstun*.service; do
        if [ -f "$f" ] && grep -q "^ExecStart=.*$ETC/nginx.conf" "$f"; then nginxu=${f##*/}; fi
      done
      if [ -n "$nginxu" ]; then
        units="$units $nginxu"
        for f in "$SYSTEMD_DIR"/hstun*.service; do
          if [ -f "$f" ] && grep -q "systemctl reload $nginxu" "$f"; then reload=${f##*/}; fi
        done
        if [ -n "$reload" ]; then
          units="$units $reload"
          for f in "${reload%.service}.path" "${reload%.service}.timer"; do
            if [ -f "$SYSTEMD_DIR/$f" ]; then units="$units $f"; fi
          done
        fi
      fi
      if [ -r "$ETC/nginx.conf" ]; then
        PORT=$(nginx_val "$ETC/nginx.conf" listen)
        DOMAIN=$(nginx_val "$ETC/nginx.conf" server_name)
        if [ "$DOMAIN" = _ ]; then DOMAIN=""; fi
      fi
    fi
  else
    ROLE=entry
    SERVER=$(toml_val "$cfg" server client); CARRIER=$(toml_val "$cfg" carrier client)
    WS_PATH=$(toml_val "$cfg" ws_path client); HTTP_PATH=$(toml_val "$cfg" http_path client)
    CONNECT=$(toml_val "$cfg" connect client); SNI=$(toml_val "$cfg" sni client)
    CONNS=$(toml_val "$cfg" conns client); TLS_VERIFY=$(toml_val "$cfg" tls_verify client)
    SPKI=$(toml_val "$cfg" spki_sha256 client)
    FORWARD_LISTEN=$(toml_val "$cfg" listen forward); FORWARD_NAME=$(toml_val "$cfg" name forward)
    ADMIN_PORT=$(toml_val "$cfg" admin_listen client); ADMIN_PORT=${ADMIN_PORT##*:}
    if [ "$(toml_val "$cfg" enabled socks)" = true ]; then SOCKS_LISTEN=$(toml_val "$cfg" listen socks); fi
    CARRIER=${CARRIER:-ws}
  fi
  ADOPTED=1; ADOPT_UNITS=$units
  if [ "$bin" != "$BIN_DIR/hstun" ]; then ADOPT_BIN=$bin; fi
  if [ "$DRY_RUN" = 1 ]; then
    info "would adopt $unitfile as instance ${NAME:-default}: role $ROLE${MODE:+, mode $MODE}, binary $bin ($("$bin" version 2>/dev/null || echo ?))"
    echo "           units: $units"
    echo "           ports: ${PORT:+public $PORT }${BACKEND_PORT:+backend $BACKEND_PORT }${ADMIN_PORT:+admin $ADMIN_PORT}${FORWARD_LISTEN:+ forward $FORWARD_LISTEN}${FORWARD_TARGET:+ -> $FORWARD_TARGET}"
    return 0
  fi
  save_conf
  info "adopted $unitfile as instance ${NAME:-default}: role $ROLE${MODE:+, mode $MODE}, $("$bin" version 2>/dev/null || echo "$bin"), units: $units"
}
ensure_ctl() { # hstun-ctl on PATH without touching the hstun binary the live units run
  [ -x "$BIN_DIR/hstun-ctl" ] && return 0
  install -d "$BIN_DIR"
  if [ -f "$0" ] && [ -r "$0" ] && [ "${0#/dev/}" = "$0" ]; then install -m 0755 "$0" "$BIN_DIR/hstun-ctl"
  else fetch_release; install -m 0755 "$STAGE/share/install.sh" "$BIN_DIR/hstun-ctl"; fi
  info "installed $BIN_DIR/hstun-ctl (the hstun binaries your units run were not touched)"
}
cmd_adopt() {
  need_root
  local cands unitfile bin sub cfg n=0 bad=0
  cands=$(adopt_candidates)
  [ -n "$cands" ] || die "no hstun systemd units (ExecStart ... hstun server|client -config FILE) found in $SYSTEMD_DIR"
  while IFS='|' read -r unitfile bin sub cfg; do
    if [ -n "$ADOPT_ONLY" ] && [ "$ADOPT_ONLY" != "$unitfile" ] && [ "$ADOPT_ONLY.service" != "$unitfile" ]; then continue; fi
    n=$((n + 1))
    if ! ( reset_conf; adopt_one "$unitfile" "$bin" "$sub" "$cfg" ); then bad=$((bad + 1)); fi
  done <<<"$cands"
  [ "$n" -gt 0 ] || die "no hstun unit named $ADOPT_ONLY"
  if [ "$DRY_RUN" = 1 ]; then info "dry run: nothing was written"; return 0; fi
  ensure_ctl
  [ "$bad" = 0 ] || warn "$bad unit(s) could not be adopted (see above); they keep running untouched"
  info "next: hstun-ctl status | logs | restart | update (checks every config with the new binary first, keeps .prev copies)"
}
cmd_forget() { # stop managing an adopted instance; units, config and binary stay exactly as they are
  need_root; set_paths; need_installed
  [ -n "$ADOPTED" ] || die "forget is only for adopted instances; use uninstall for this one"
  confirm "Stop managing ${NAME:-default} with hstun-ctl (it keeps running as it is)?" n || { info "nothing changed"; return 0; }
  rm -f "${ETC:?}/install.conf"
  info "forgot ${NAME:-default}"
}

# ---------- health, test, doctor ----------
listening() { ss -ltnH "( sport = :$1 )" 2>/dev/null | grep -q .; }
sessions_of() { # admin port: number of live sessions, empty when the admin page does not answer
  local a; a=$(curl -s -m 2 "http://127.0.0.1:$1/stats" 2>/dev/null || true)
  [ -n "$a" ] && printf '%s' "$a" | grep -o '"id":' | wc -l
  return 0
}
# health_ok: the loaded instance is running and, within 25 seconds, ready (entry: a session is up; exit: its port listens)
health_ok() {
  if [ "$TEST" = 1 ] || ! have_systemd; then [ "${HSTUN_TEST_HEALTH:-ok}" = ok ]; return; fi
  local i u n
  for i in $(seq 1 25); do
    u=$(daemon_units | head -1)
    if [ -n "$u" ] && systemctl is-active --quiet "$u"; then
      if [ "$ROLE" = entry ]; then
        n=$(sessions_of "$ADMIN_PORT"); [ "${n:-0}" -ge 1 ] && return 0
      else
        listening "${PORT:-$BACKEND_PORT}" && return 0
      fi
    fi
    [ "$i" -lt 25 ] && sleep 1
  done
  return 1
}
CHK_FAIL=0
chk() { # ok|fail|warn LABEL [DETAIL]
  local d=""; [ -n "${3:-}" ] && d="  ($3)"
  case "$1" in
    ok) printf '  %s[ ok ]%s  %s%s\n' "$G" "$N" "$2" "$d" ;;
    warn) printf '  %s[warn]%s  %s%s\n' "$Y" "$N" "$2" "$d" ;;
    *) printf '  %s[FAIL]%s  %s%s\n' "$R" "$N" "$2" "$d"; CHK_FAIL=1 ;;
  esac
}
tcp_ok() { # host port [seconds]
  (exec 7<>"/dev/tcp/$1/$2") 2>/dev/null &
  local pid=$! i
  for i in $(seq 1 "${3:-5}"); do kill -0 "$pid" 2>/dev/null || { wait "$pid"; return; }; sleep 1; done
  kill "$pid" 2>/dev/null; return 1
}
cmd_test_matrix() { # entry: every carrier x TLS fingerprint through `hstun probe`, with a verdict per row
  [ "$ROLE" = entry ] || die "the matrix runs on the entry host (the restricted side), where the path to the exit is"
  have jq || die "the matrix needs jq"
  local bin cfg server soak=20 idle=10 bulk=4194304 c f out v ms dn up sl line best="" bestv=0 any=0
  bin=$(inst_bin); cfg=$ETC/client.toml
  "$bin" probe -h 2>&1 | grep -q -- '-carrier' || die "this hstun ($("$bin" version 2>/dev/null)) has no probe command: run hstun-ctl update first"
  if [ "$QUICK" = 1 ]; then soak=5; idle=3; bulk=1048576; fi
  server=$(sed -n 's/^server *= *"\(.*\)"/\1/p' "$cfg" | head -1)
  echo "Path matrix for ${NAME:-default}: $server (soak ${soak}s, idle gap ${idle}s, ${bulk} bytes each way)"
  echo "This opens extra tunnel sessions for a few minutes; the running tunnel is not touched."
  printf '%-7s %-12s %-9s %9s %9s %9s  %s\n' carrier fingerprint verdict "connect" "down" "up" "notes"
  for c in ${HSTUN_MATRIX_CARRIERS:-ws http chunk}; do # the variables narrow the matrix (tests)
    for f in ${HSTUN_MATRIX_FPS:-go chrome chrome_nopq firefox}; do
      out=$("$bin" probe -config "$cfg" -carrier "$c" -fingerprint "$f" -soak "${soak}s" -idle "${idle}s" -bulk "$bulk" -json 2>/dev/null) || true
      if [ -z "$out" ]; then printf '%-7s %-12s %-9s\n' "$c" "$f" "error"; continue; fi
      v=$(printf '%s' "$out" | jq -r '.verdict'); ms=$(printf '%s' "$out" | jq -r '.connect_ms|floor')
      dn=$(printf '%s' "$out" | jq -r '.down_mbit // 0|.*10|floor|./10'); up=$(printf '%s' "$out" | jq -r '.up_mbit // 0|.*10|floor|./10')
      sl=$(printf '%s' "$out" | jq -r 'if .exit_probe then ([.steps[]|select(.ok|not)|.bytes]|first // empty) as $b | (if $b then "fails at \($b) bytes; " else "" end) + (if .soak_sent>0 then "soak lost \(.soak_lost)/\(.soak_sent)" else "" end) elif .connected then "exit has no probe streams (older than 0.5.0)" else (.error // "") end')
      printf '%-7s %-12s %-9s %7s ms %6s Mb %6s Mb  %s\n' "$c" "$f" "$v" "$ms" "$dn" "$up" "$sl"
      if [ "$v" = ok ]; then
        any=1
        # downloads decide the ranking: users download far more than they upload
        if awk -v a="$dn" -v b="$bestv" 'BEGIN{exit !(a>b)}'; then best="$c $f"; bestv=$dn; fi
      fi
    done
  done
  # the ClientHello fragmentation modes on the best ws fingerprint (record-level splitting broke one real path)
  if [ -n "$best" ]; then
    f=${best#* }
    for fr in tcp record; do
      out=$("$bin" probe -config "$cfg" -carrier ws -fingerprint "$f" -fragment "$fr" -soak 5s -idle 2s -bulk 1048576 -json 2>/dev/null) || true
      [ -n "$out" ] || continue
      v=$(printf '%s' "$out" | jq -r '.verdict'); ms=$(printf '%s' "$out" | jq -r '.connect_ms|floor')
      printf '%-7s %-12s %-9s %7s ms   (tls_fragment=%s)\n' ws "$f" "$v" "$ms" "$fr"
    done
  fi
  # what a stranger sees on the same port: the front page and a path nobody has
  line=$(curl -sk -m 8 -o /dev/null -w '%{http_code}' "https://$server/" 2>/dev/null || echo "-")
  out=$(curl -sk -m 8 -o /dev/null -w '%{http_code}' "https://$server/$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')" 2>/dev/null || echo "-")
  echo "A stranger on $server sees: / -> $line, a random path -> $out (both should look like an ordinary website)"
  if [ "$any" = 1 ]; then
    echo "Best working row by download speed: carrier ${best% *}, fingerprint ${best#* }."
    echo "Use it with: hstun-ctl edit --carrier ${best% *} --tls-fingerprint ${best#* }"
  else
    echo "No row worked: the path to the exit is blocked or the exit is down. Try hstun-ctl doctor."
    return 1
  fi
}
cmd_test() { # end-to-end check of one instance: unit, sessions, and a stream through to the exit's target
  set_paths; need_installed
  if [ "$MATRIX" = 1 ]; then cmd_test_matrix; return; fi
  CHK_FAIL=0
  echo "Testing instance ${NAME:-default} ($ROLE)"
  local u n
  u=$(daemon_units | head -1)
  if [ "$TEST" != 1 ] && have_systemd; then
    if systemctl is-active --quiet "$u" 2>/dev/null; then chk ok "service $u is running"; else chk fail "service $u is not running" "hstun-ctl logs"; fi
  fi
  if [ -n "$ADMIN_PORT" ]; then
    n=$(sessions_of "$ADMIN_PORT")
    if [ "${n:-0}" -ge 1 ]; then chk ok "$n tunnel session(s) up"
    elif [ "$ROLE" = entry ]; then chk fail "no tunnel session" "the exit is unreachable or the secret/paths differ: hstun-ctl logs"
    else chk warn "no entry is connected yet"; fi
  fi
  if [ "$ROLE" = entry ]; then
    local h p=${FORWARD_LISTEN##*:} line
    h=$(unbracket "${FORWARD_LISTEN%:*}")
    case "$h" in 0.0.0.0 | "" | "::") h=127.0.0.1 ;; esac
    if (exec 7<>"/dev/tcp/$h/$p") 2>/dev/null; then
      # a stream through the tunnel: if the exit cannot reach its target it closes the stream at once
      exec 7<>"/dev/tcp/$h/$p"
      if read -r -t 2 -u 7 line; then chk ok "a stream through the tunnel was answered by the exit's target"
      elif [ "$?" -gt 128 ]; then chk ok "a stream through the tunnel stayed open (the exit's target accepted it)"
      else chk fail "the exit closed the stream at once" "nothing listens on the exit's forward target? check the proxy on the exit host"; fi
      exec 7<&-
    else chk fail "nothing listens on $FORWARD_LISTEN on this host" "hstun-ctl restart"; fi
  else
    local t=$FORWARD_TARGET
    if tcp_ok "$(unbracket "${t%:*}")" "${t##*:}" 3; then chk ok "forward target $t accepts connections"; else chk fail "forward target $t does not accept connections" "start your proxy (for example the VLESS inbound) there"; fi
    if [ -n "${PORT:-$BACKEND_PORT}" ] && { [ "$TEST" = 1 ] || listening "${PORT:-$BACKEND_PORT}"; }; then chk ok "port ${PORT:-$BACKEND_PORT} is listening"; else chk fail "port ${PORT:-$BACKEND_PORT} is not listening" "hstun-ctl logs"; fi
  fi
  [ "$CHK_FAIL" = 0 ] && info "test passed" || { warn "test found problems"; return 1; }
}
cmd_doctor() { # read-only checks of this host and one instance
  set_paths; need_installed
  CHK_FAIL=0
  echo "Checking instance ${NAME:-default} ($ROLE) on $(hostname -s 2>/dev/null)"
  local v
  v=$("$(inst_bin)" version 2>/dev/null) && chk ok "binary $v" || chk fail "binary $(inst_bin) does not run"
  local m=server; [ "$ROLE" = entry ] && m=client
  if "$(inst_bin)" check -config "$ETC/$m.toml" -mode "$m" >/dev/null 2>&1; then chk ok "configuration $ETC/$m.toml is valid"; else chk fail "configuration $ETC/$m.toml is rejected" "$(inst_bin) check -config $ETC/$m.toml"; fi
  if [ "$TEST" != 1 ] && have_systemd; then
    local u
    for u in $(service_units); do
      if systemctl is-enabled --quiet "$u" 2>/dev/null; then chk ok "$u starts at boot"; else chk warn "$u does not start at boot" "systemctl enable $u"; fi
    done
    if [ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null || echo yes)" = yes ]; then chk ok "clock is synchronised"; else chk warn "clock is not synchronised" "TLS and handshakes need a correct time"; fi
  fi
  local free; free=$(df -Pk "$ETC_BASE" 2>/dev/null | awk 'NR==2{print int($4/1024)}')
  if [ "${free:-1000}" -ge 100 ]; then chk ok "disk space (${free:-?} MB free)"; else chk warn "little disk space (${free} MB free)"; fi
  if [ "$ROLE" = exit ]; then
    local port=${PORT:-$BACKEND_PORT}
    if [ "$MODE" = nginx ] && [ -r "$ETC/nginx.conf" ] && have nginx; then
      if nginx -t -c "$ETC/nginx.conf" >/dev/null 2>&1; then chk ok "nginx configuration is valid"; else chk fail "nginx configuration is rejected" "nginx -t -c $ETC/nginx.conf"; fi
    fi
    local cert=$ETC/cert.pem; [ "$MODE" = nginx ] && cert=${CERT:-}
    if [ -n "$cert" ] && [ -r "$cert" ]; then
      if openssl x509 -in "$cert" -noout -checkend $((14 * 86400)) >/dev/null 2>&1; then chk ok "certificate is valid for more than 14 days"
      elif openssl x509 -in "$cert" -noout -checkend 0 >/dev/null 2>&1; then chk warn "certificate expires within 14 days" "$cert"
      else chk fail "certificate has expired" "$cert"; fi
    fi
    if [ "$TEST" != 1 ] && [ -n "$port" ]; then
      if tcp_ok 127.0.0.1 "$port" 2; then chk ok "port $port answers on IPv4"; else chk fail "port $port does not answer on IPv4 (127.0.0.1)" "hstun-ctl logs"; fi
      if [ "$IPV6" = 1 ]; then
        if tcp_ok ::1 "$port" 2; then chk ok "port $port answers on IPv6 (::1)"; else chk fail "port $port does not answer on IPv6" "the exit is set up for IPv6 but nothing listens on [::]:$port"; fi
      fi
    fi
    if [ -n "$DOMAIN" ] && [ "$TEST" != 1 ]; then
      local aaaa; aaaa=$(getent ahostsv6 "$DOMAIN" 2>/dev/null | awk '$1 !~ /^::ffff:/ {print $1; exit}')
      if [ -n "$aaaa" ] && [ "$IPV6" != 1 ] && [ "$MODE" != cloudflared ]; then
        chk fail "$DOMAIN has an IPv6 (AAAA) record ($aaaa) but this exit does not listen on IPv6" "clients that prefer IPv6 cannot connect: remove the AAAA record or reinstall with --ipv6"
      elif [ -n "$aaaa" ] && [ "$IPV6" = 1 ]; then
        if ip -6 -o addr show 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | grep -qx "$aaaa"; then chk ok "AAAA record $aaaa belongs to this host"; else chk warn "AAAA record $aaaa is not an address of this host" "fine behind a CDN, otherwise fix the DNS record"; fi
      fi
    fi
    if [ -n "$DOMAIN" ]; then
      local ip mine; ip=$(getent hosts "$DOMAIN" 2>/dev/null | awk '{print $1; exit}')
      mine=$(ip -4 -o addr show 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | tr '\n' ' ')
      if [ -z "$ip" ]; then chk fail "$DOMAIN does not resolve" "create the DNS record"
      elif [[ " $mine " == *" $ip "* ]]; then chk ok "$DOMAIN resolves to this host ($ip)"
      else chk warn "$DOMAIN resolves to $ip, not to an address of this host" "fine behind NAT or a CDN, otherwise fix the DNS record"; fi
    fi
    if [ "$TEST" != 1 ] && ufw_active && [ "$MODE" != cloudflared ]; then
      if ufw status | grep -qE "^$port(/tcp)?[[:space:]]"; then chk ok "ufw allows port $port"; else chk warn "no ufw rule for port $port" "ufw allow $port/tcp"; fi
    fi
  else
    local sh sp=${SERVER##*:}
    sh=$(unbracket "${SERVER%:*}")
    if [ "$TEST" != 1 ]; then
      if getent hosts "$sh" >/dev/null 2>&1 || [[ "$sh" =~ ^[0-9.]+$ ]]; then chk ok "exit address $sh resolves"; else chk fail "exit address $sh does not resolve"; fi
      local tgt=${CONNECT:-$(bracket "$sh"):$sp}
      if tcp_ok "$(unbracket "${tgt%:*}")" "${tgt##*:}" 5; then chk ok "TCP connection to the exit ($tgt) works"; else chk fail "cannot open a TCP connection to the exit ($tgt)" "firewall or blocked route; try --carrier chunk through a CDN"; fi
    fi
  fi
  cmd_test || CHK_FAIL=1
  [ "$CHK_FAIL" = 0 ] && info "no problems found" || { warn "problems found (see [FAIL] lines)"; return 1; }
}

# ---------- snapshots: settings changes and updates that undo themselves ----------
snapshot_instance() { # DIR
  tar -C "$ETC_BASE" -cf "$1/etc.tar" "${ETC##*/}"
  local u; for u in $(units_of_instance); do cp -p "$SYSTEMD_DIR/$u" "$1/" 2>/dev/null || true; done
}
restore_instance() { # DIR
  rm -rf "${ETC:?}"; tar -C "$ETC_BASE" -xf "$1/etc.tar"
  local f; for f in "$1"/*.service "$1"/*.path "$1"/*.timer; do [ -f "$f" ] && cp -p "$f" "$SYSTEMD_DIR/"; done
  sysd daemon-reload
  local u; for u in $(service_units); do sysd restart "$u"; done
}
cmd_edit() { # change an installed instance; undone automatically when it does not come back healthy
  need_root; set_paths
  [ -f "$ETC/install.conf" ] || die "no hstun instance${NAME:+ named $NAME} found in $ETC (install it first)"
  local role snap rc=0
  role=$(sed -n 's/^ROLE=//p' "$ETC/install.conf" | head -1)
  snap=$(mktemp -d)
  ( reset_conf; load_conf; snapshot_instance "$snap" ) # flags given on the command line must survive, so load only inside the subshell
  EDIT_MODE=1; RECONFIGURE=1
  set +e; ( set -e; if [ "$role" = exit ]; then install_exit; else install_entry; fi ); rc=$?; set -e
  if [ "$rc" = 0 ] && ( reset_conf; set_paths; load_conf; health_ok ); then
    info "settings applied and the tunnel is healthy"; rm -rf "${snap:?}"; return 0
  fi
  warn "the change did not work (exit code $rc or the tunnel did not come back): restoring the previous settings"
  ( reset_conf; set_paths; load_conf; restore_instance "$snap" )
  rm -rf "${snap:?}"
  die "previous settings restored. Firewall rules opened for the new settings (if any) were not closed."
}
cmd_rollback() { # put the previous binary back (the .prev copy every update leaves)
  need_root; set_paths; need_installed
  local b; b=$(inst_bin)
  [ -f "$b.prev" ] || die "no previous binary ($b.prev) to go back to"
  confirm "Go back from $("$b" version 2>/dev/null) to $("$b.prev" version 2>/dev/null)?" n || { info "nothing changed"; return 0; }
  cp -p "$b" "$b.next"; put_file "$b.prev" "$b"; mv "$b.next" "$b.prev"
  local u; for u in $(daemon_units); do sysd restart "$u"; done
  info "rolled back to $("$b" version 2>/dev/null) (the newer one is kept as $b.prev, run rollback again to return)"
}

# ---------- secret rotation ----------
# Exit: `rotate-secret` adds a second secret next to the first (both are accepted), prints the new pair code;
# every entry runs `rotate-secret --join CODE`; then `rotate-secret --finish` on the exit drops the old secret.
rotate_set_secret_files() { # CONFIG VALUE-LINE
  local cfg=$1 line=$2 cur="secret_files = [\"$ETC/secret\"]" cur2="secret_files = [\"$ETC/secret\", \"$ETC/secret.new\"]"
  if grep -qF -- "$cur" "$cfg" || grep -qF -- "$cur2" "$cfg"; then
    local tmp; tmp=$(mktemp)
    awk -v want="$line" -v c1="$cur" -v c2="$cur2" 'index($0, c1) == 1 || index($0, c2) == 1 { print want; next } { print }' "$cfg" >"$tmp" && cat "$tmp" >"$cfg"
    rm -f "$tmp"
  else die "$cfg has a secret_files line this command does not know; edit it by hand (list both secret files, restart, then remove the old one)"; fi
}
cmd_rotate_secret() {
  need_root; set_paths; need_installed
  if [ "$ROLE" = entry ]; then
    [ -n "$JOIN" ] || die "on an entry run: hstun-ctl rotate-secret --join 'hstun-pair:...' (the new pair code printed by the exit)"
    parse_join
    [ -n "$JOIN_SECRET" ] || die "that code has no secret: use the hstun-pair code from 'hstun-ctl rotate-secret' on the exit"
    cp -p "$ETC/secret" "$ETC/secret.prev"
    ( umask 077; printf '%s\n' "$JOIN_SECRET" >"$ETC/secret" ); own "$SVC_USER:$SVC_USER" "$ETC/secret"; chmod 0600 "$ETC/secret"
    local u; for u in $(daemon_units); do sysd restart "$u"; done
    if health_ok; then rm -f "$ETC/secret.prev"; info "the entry now uses the new secret and its tunnel is up"
    else
      mv -f "$ETC/secret.prev" "$ETC/secret"; for u in $(daemon_units); do sysd restart "$u"; done
      die "the tunnel did not come up with the new secret; the old secret is back. Is the rotation started on the exit, and is the code complete?"
    fi
    return 0
  fi
  local cfg="$ETC/server.toml" u
  if [ "$FINISH" = 1 ]; then
    [ -s "$ETC/secret.new" ] || die "no rotation in progress"
    confirm "Drop the old secret now? Entries that still use it will be refused." n || { info "nothing changed"; return 0; }
    cp -p "$cfg" "$cfg.pre-rotate"
    mv -f "$ETC/secret.new" "$ETC/secret"
    rotate_set_secret_files "$cfg" "secret_files = [\"$ETC/secret\"]"
    "$(inst_bin)" check -config "$cfg" -mode server >/dev/null 2>&1 || { mv -f "$cfg.pre-rotate" "$cfg"; die "the new configuration is rejected; nothing changed"; }
    for u in $(daemon_units); do sysd restart "$u"; done
    rm -f "$cfg.pre-rotate"
    info "rotation finished: only the new secret is accepted"
    return 0
  fi
  if [ -s "$ETC/secret.new" ]; then
    warn "a rotation is already in progress. Pair code with the new secret:"
    SECRET_OVERRIDE="$ETC/secret.new"; echo "  hstun-ctl rotate-secret --join '$(make_join secret)'"
    echo "  When every entry uses it, run on this host: hstun-ctl${NAME:+ --name $NAME} rotate-secret --finish"
    return 0
  fi
  cp -p "$cfg" "$cfg.pre-rotate"
  "$(inst_bin)" keygen -out "$ETC/secret.new" >/dev/null
  own "$SVC_USER:$SVC_USER" "$ETC/secret.new"; chmod 0600 "$ETC/secret.new"
  rotate_set_secret_files "$cfg" "secret_files = [\"$ETC/secret\", \"$ETC/secret.new\"]"
  if ! "$(inst_bin)" check -config "$cfg" -mode server >/dev/null 2>&1; then mv -f "$cfg.pre-rotate" "$cfg"; rm -f "$ETC/secret.new"; die "the configuration with two secrets is rejected; nothing changed"; fi
  for u in $(daemon_units); do sysd restart "$u"; done
  if ! health_ok; then
    mv -f "$cfg.pre-rotate" "$cfg"; rm -f "$ETC/secret.new"; for u in $(daemon_units); do sysd restart "$u"; done
    die "the exit did not come back with two secrets; the previous configuration is restored"
  fi
  rm -f "$cfg.pre-rotate"
  info "rotation started: the exit accepts the old and the new secret. Nothing is interrupted."
  echo "  1. On every entry run (the code contains the new secret, send it privately):"
  SECRET_OVERRIDE="$ETC/secret.new"; echo "       hstun-ctl rotate-secret --join '$(make_join secret)'"
  echo "  2. When every entry is done, run here:  hstun-ctl${NAME:+ --name $NAME} rotate-secret --finish"
}

# ---------- backup and restore ----------
cmd_backup() { # every managed instance: configuration, secrets, certificates, units
  need_root
  local out=${FILE:-hstun-backup-$(date +%Y%m%d-%H%M).tar.gz} stage d n=0
  stage=$(mktemp -d); mkdir -p "$stage/etc" "$stage/systemd"
  for d in "$ETC_BASE"/hstun "$ETC_BASE"/hstun-*; do
    [ -f "$d/install.conf" ] || continue
    cp -a "$d" "$stage/etc/"; n=$((n + 1))
    ( reset_conf; NAME=$(name_of "$d"); set_paths; load_conf; for u in $(units_of_instance); do cp -p "$SYSTEMD_DIR/$u" "$stage/systemd/" 2>/dev/null || true; done )
  done
  [ "$n" -gt 0 ] || { rm -rf "${stage:?}"; die "no managed instances to back up"; }
  echo "$INSTALLER_VERSION" >"$stage/VERSION"
  ( umask 077; tar -czf "$out" -C "$stage" etc systemd VERSION )
  rm -rf "${stage:?}"
  info "backup written: $out ($n instance(s)). It contains secrets and private keys: keep it private."
}
cmd_restore() { # FILE [--test]: --test only reads and reports
  need_root
  [ -n "$FILE" ] && [ -f "$FILE" ] || die "usage: hstun-ctl restore FILE [--test]"
  local stage d name role mode bad=0
  stage=$(mktemp -d)
  tar -tzf "$FILE" >/dev/null 2>&1 || { rm -rf "${stage:?}"; die "$FILE is not a readable backup"; }
  tar --same-owner -xzf "$FILE" -C "$stage" || { rm -rf "${stage:?}"; die "could not read $FILE"; }
  [ -d "$stage/etc" ] || { rm -rf "${stage:?}"; die "$FILE is not an hstun backup"; }
  echo "Backup made by hstun-ctl $(cat "$stage/VERSION" 2>/dev/null || echo '?'):"
  for d in "$stage"/etc/hstun*; do
    [ -f "$d/install.conf" ] || continue
    name=$(name_of "$d")
    role=$(sed -n 's/^ROLE=//p' "$d/install.conf" | head -1); mode=$(sed -n 's/^MODE=//p' "$d/install.conf" | head -1)
    printf '  instance %-10s %s%s' "${name:-default}" "$role" "${mode:+, mode $mode}"
    if [ -s "$d/secret" ] && [ -f "$d/$([ "$role" = exit ] && echo server || echo client).toml" ]; then echo "   (configuration and secret present)"; else echo "   ${R}(incomplete: configuration or secret missing)${N}"; bad=1; fi
    [ -d "$ETC_BASE/${d##*/}" ] && echo "           exists on this host and would be overwritten"
  done
  if [ "$bad" = 1 ]; then rm -rf "${stage:?}"; die "the backup is incomplete"; fi
  if [ "$FILE_TEST" = 1 ]; then rm -rf "${stage:?}"; info "test only: nothing was changed"; return 0; fi
  confirm "Restore these instances now? Existing ones with the same names are overwritten." n || { rm -rf "${stage:?}"; info "nothing changed"; return 0; }
  [ -x "$BIN_DIR/hstun" ] || { fetch_release; install_files; }
  ensure_user_account
  for d in "$stage"/etc/hstun*; do
    [ -f "$d/install.conf" ] || continue
    ( reset_conf; NAME=$(name_of "$d"); set_paths; load_conf
      for u in $(service_units); do sysd stop "$u" || true; done
      rm -rf "${ETC:?}"; cp -a "$d" "$ETC_BASE/" )
  done
  for f in "$stage"/systemd/*; do [ -f "$f" ] && cp -p "$f" "$SYSTEMD_DIR/"; done
  sysd daemon-reload
  for d in "$stage"/etc/hstun*; do
    [ -f "$d/install.conf" ] || continue
    ( reset_conf; NAME=$(name_of "$d"); set_paths; load_conf; for u in $(units_of_instance); do case "$u" in *.path | *.timer) sysd enable --now "$u" >/dev/null 2>&1 || true ;; *) sysd enable --now "$u" >/dev/null 2>&1 || sysd restart "$u" ;; esac; done )
  done
  rm -rf "${stage:?}"
  info "restored. Firewall rules are not part of a backup: check them with hstun-ctl doctor."
}

cmd_version() { echo "hstun-ctl $INSTALLER_VERSION; binary: $("$BIN_DIR/hstun" version 2>/dev/null || echo not installed)"; }

# ---------- menu ----------
list_instances() { # config directory of every managed instance
  local d
  for d in "$ETC_BASE"/hstun "$ETC_BASE"/hstun-*; do [ -f "$d/install.conf" ] && echo "$d"; done
  return 0
}
have_role() { # true when a managed instance with this role exists
  local d
  while IFS= read -r d; do [ -n "$d" ] && grep -qs "^ROLE=$1\$" "$d/install.conf" && return 0; done < <(list_instances)
  return 1
}
name_of() { local n=${1##*/hstun}; echo "${n#-}"; }
instance_line() { # config directory: one status line
  (
    reset_conf; NAME=$(name_of "$1"); set_paths; load_conf
    local st="?" sess="" a unit
    unit=$(daemon_units | head -1)
    [ -n "$unit" ] && st=$(systemctl is-active "$unit" 2>/dev/null || true)
    if [ -n "$ADMIN_PORT" ]; then
      a=$(curl -s -m 1 "http://127.0.0.1:$ADMIN_PORT/stats" 2>/dev/null || true)
      # grep finds nothing when there is no session, and with pipefail that would end the whole menu
      [ -n "$a" ] && sess=$(printf '%s' "$a" | grep -o '"id":' | wc -l || true)
    fi
    local what=${MODE:-${CARRIER:-}}
    printf '  %-10s %-6s %-12s %-9s %s\n' "${NAME:-default}" "$ROLE" "$what" "${st:-?}" "${sess:+$sess session(s)}"
  )
}
update_banner() { # one red line when a newer release exists; cached for 6 hours, never waits long
  [ "${HSTUN_NO_UPDATE_CHECK:-0}" = 1 ] && return 0
  if [ -n "$(pinned_version)" ]; then printf '%s\n' "${D}  Pinned to $(pinned_version): update installs exactly that release.${N}"; return 0; fi
  have jq || return 0
  local cache="$ETC_BASE/.hstun-latest" latest="" cur
  if [ -f "$cache" ] && [ -n "$(find "$cache" -mmin -360 2>/dev/null)" ]; then latest=$(cat "$cache" 2>/dev/null || true)
  else
    latest=$(latest_version 2>/dev/null || true)
    if [ -n "$latest" ]; then printf '%s\n' "$latest" >"$cache" 2>/dev/null || true; fi
  fi
  cur=$("$BIN_DIR/hstun" version 2>/dev/null | sed 's/^hstun //' || true); cur=${cur:-$INSTALLER_VERSION}
  if [ -n "$latest" ] && [ "$latest" != "$cur" ] && [ "$(printf '%s\n%s\n' "$cur" "$latest" | sort -V | tail -1)" = "$latest" ]; then
    printf '%s\n' "${R}${B}  A newer release is available: $latest (installed: $cur). Choose Update.${N}"
  fi
}
has_unmanaged() { # true when an hstun unit exists whose config directory is not managed yet
  local line cfg
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    cfg=${line##*|}
    [ -f "${cfg%/*}/install.conf" ] || return 0
  done < <(adopt_candidates)
  return 1
}
pick_instance() { # [role]: sets NAME to the instance to act on (asks when there are several)
  local want=${1:-} d r dirs=() i c
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    if [ -n "$want" ]; then r=$(sed -n 's/^ROLE=//p' "$d/install.conf" | head -1); [ "$r" = "$want" ] || continue; fi
    dirs+=("$d")
  done < <(list_instances)
  case ${#dirs[@]} in
    0) warn "no ${want:+$want }instance is installed"; return 1 ;;
    1) NAME=$(name_of "${dirs[0]}") ;;
    *) echo "Which instance?"
       for i in "${!dirs[@]}"; do printf '  %d) %s\n' $((i + 1)) "$(name_of "${dirs[$i]}" | sed 's/^$/default/')"; done
       c=""; ask c "Choose" 1
       case "$c" in *[!0-9]* | "") die "choose a number" ;; esac
       [ "$c" -ge 1 ] && [ "$c" -le "${#dirs[@]}" ] || die "choose a number from the list"
       NAME=$(name_of "${dirs[$((c - 1))]}") ;;
  esac
  set_paths
}
act_status() { local d; while IFS= read -r d; do reset_conf; NAME=$(name_of "$d"); set_paths; cmd_status; echo; done < <(list_instances); }
act_pair() { pick_instance exit && { need_installed; echo "Send this one command to the entry host. It contains the secret, so use a private channel:"; echo; echo "  hstun-ctl entry --join '$(make_join secret)'"; }; }
act_logs() { pick_instance && { FOLLOW=1; echo "Live log. Press Ctrl+C to return to the menu."; cmd_logs; }; }
act_pin() { local v=""; ask v "Version to pin (for example 0.4.0; empty = follow the latest release)" "" opt; FINISH=0; if [ -z "$v" ]; then FINISH=1; fi; FILE=$v; cmd_pin; }
act_rotate() {
  pick_instance || return 0
  local role; role=$(sed -n 's/^ROLE=//p' "$ETC/install.conf" | head -1)
  if [ "$role" = entry ]; then ask JOIN "Paste the new pair code printed by the exit's rotate-secret" ""; fi
  cmd_rotate_secret
}
act_test() { pick_instance && cmd_test; }
act_doctor() { pick_instance && cmd_doctor; }
act_edit() { pick_instance && cmd_edit; }
act_rollback() { pick_instance && cmd_rollback; }
act_restore() { local f=""; ask f "Backup file" ""; FILE=$f; if confirm "Only check the backup, change nothing?" y; then FILE_TEST=1; fi; cmd_restore; }
act_restart() { pick_instance && cmd_restart; }
act_update() { local n; n=$(list_instances | wc -l); [ "$n" -gt 1 ] && ALL=1; pick_instance || return 0; cmd_update; }
act_uninstall() { pick_instance || return 0; confirm "Also delete its configuration, secret and certificate?" n && PURGE=1; cmd_uninstall; }
act_install_exit() { NAME=""; set_paths; install_exit; }
act_install_entry() { NAME=""; set_paths; install_entry; }
act_install_more() {
  local role="" nm=""
  echo "  1) another exit   2) another entry"; ask role "Choose" 1
  ask nm "Name for the new instance (letters and digits)" ""
  case "$nm" in *[!A-Za-z0-9_]* | "") die "the name must be letters, digits or underscore" ;; esac
  NAME=$nm; set_paths
  [ -f "$ETC/install.conf" ] && die "an instance named $nm already exists"
  case "$role" in 1) install_exit ;; 2) install_entry ;; *) die "choose 1 or 2" ;; esac
}
menu() {
  [ -n "$TTY" ] || die "no terminal; run: install.sh help"
  trap ':' INT # Ctrl+C stops a live log and returns here instead of leaving the script
  local c i d insts labels actions hints
  while true; do
    reset_conf; NAME=""; set_paths; SKIP_LOAD=""; FOLLOW=0; ALL=0; PURGE=0; FINISH=0; FILE=""; JOIN=""; JOIN_SECRET=""
    insts=$(list_instances)
    labels=(); actions=(); hints=()
    echo
    echo "${B}hstun $INSTALLER_VERSION${N}  (outside host = exit, restricted host = entry)"
    update_banner
    if [ -n "$insts" ]; then
      echo "Installed on this host:"
      while IFS= read -r d; do instance_line "$d"; done <<<"$insts"
      echo
      labels+=("Status"); actions+=(act_status); hints+=("units, versions, sessions")
      labels+=("Test the tunnel"); actions+=(act_test); hints+=("end-to-end check: is traffic really passing?")
      labels+=("Check for problems"); actions+=(act_doctor); hints+=("ports, firewall, certificate, DNS, clock, configuration")
      if have_role exit; then
        labels+=("Show the pair code"); actions+=(act_pair); hints+=("the one command that connects an entry to this exit")
      fi
      labels+=("Live log"); actions+=(act_logs); hints+=("follow the journal, Ctrl+C returns here")
      labels+=("Settings"); actions+=(act_edit); hints+=("change port, domain, target...; undone automatically if it breaks")
      labels+=("Restart"); actions+=(act_restart); hints+=("restart the tunnel service")
      labels+=("Update"); actions+=(act_update); hints+=("latest release; nothing happens when you are current")
      labels+=("Pin a version"); actions+=(act_pin); hints+=("keep every host on one release; update installs exactly it")
      labels+=("Roll back the last update"); actions+=(act_rollback); hints+=("go back to the previous binary")
      labels+=("Rotate the secret"); actions+=(act_rotate); hints+=("new secret without downtime: exit first, then each entry")
      labels+=("Backup"); actions+=(cmd_backup); hints+=("one private archive with settings, secrets and certificates")
      labels+=("Restore a backup"); actions+=(act_restore); hints+=("check a backup first, then restore it")
      labels+=("Install another instance"); actions+=(act_install_more); hints+=("a second exit or entry on this host")
      labels+=("Uninstall"); actions+=(act_uninstall); hints+=("remove an instance (you can keep its settings)")
    else
      labels+=("Install an exit"); actions+=(act_install_exit); hints+=("the outside host: receives the traffic and passes it to your proxy")
      labels+=("Install an entry"); actions+=(act_install_entry); hints+=("the restricted host: your users connect here")
    fi
    if has_unmanaged; then labels+=("Adopt hand-installed units"); actions+=(cmd_adopt); hints+=("manage the hstun services you set up by hand"); fi
    for i in "${!labels[@]}"; do printf '  %d) %-28s %s\n' $((i + 1)) "${labels[$i]}" "${D}${hints[$i]}${N}"; done
    echo "  0) Exit"
    c=""; read -r -p "Choose: " c <&9 || exit 0
    case "$c" in 0 | q | "") exit 0 ;; esac
    case "$c" in *[!0-9]*) warn "choose a number"; continue ;; esac
    if [ "$c" -ge 1 ] && [ "$c" -le "${#labels[@]}" ]; then
      local rc=0
      set +e; ( set -e; "${actions[$((c - 1))]}" ); rc=$?; set -e
      [ "$rc" = 0 ] || warn "that action stopped with an error (see above)"
      read -r -p "Press Enter to return to the menu " c <&9 || exit 0
    else warn "choose a number from the list"; fi
  done
}

usage() {
  cat <<EOF
hstun installer/manager $INSTALLER_VERSION

  install.sh                          menu
  install.sh exit  [flags]            install the server side
  install.sh entry [flags]            install the client side
  install.sh adopt [--dry-run] [--unit U]   register hand-installed hstun units (writes one small file per instance, changes nothing else)
  hstun-ctl status|test|doctor|logs|restart|edit|update|rollback|show-join|show-secret|backup|restore|uninstall|forget|version   (installed by the first run)
    test       end-to-end check of the tunnel         doctor   checks of this host, certificate, DNS, firewall, then test
    edit       change settings, undone if the tunnel does not come back
    rollback   go back to the previous binary          backup FILE / restore FILE [--test]   all instances in one private archive
    rotate-secret [--finish | --join CODE]   new secret without downtime (exit starts, entries follow, exit finishes)
    pin VERSION | pin --clear   make update install exactly one release (keep every host on the same one)

Common:    --name NAME   --yes   --version X.Y.Z   --repo OWNER/REPO   --no-firewall
Exit:      --mode nginx|direct|cloudflared   --forward-target HOST:PORT   --port N   --domain D
           --cert FILE --key FILE | --acme [--email E]   --allow-from CIDR[,CIDR]   --decoy-dir DIR   --public-addr ADDR   --backend-port N
Entry:     --join STRING   --server HOST:PORT   --carrier ws|http|chunk|auto   --ws-path P   --http-path P
           --secret-file FILE | --secret-from USER@HOST[:SSHPORT]   --tls-verify system|spki   --spki HEX
           --tls-fingerprint chrome|chrome_nopq|firefox|safari|ios|android|go (ClientHello the entry sends; default chrome)
           --tls-fragment off|tcp|record|both (split the first TLS record, defeats filters that read only the first packet)
           --session-age auto|off|30m (replace carrier sessions on a timer; entry, needs --conns 2 or more)
           --padding off|light|strong (random filler on small records; both ends; default light)
           --connect IP:PORT   --sni NAME   --forward-listen ADDR:PORT   --forward-name NAME   --conns N
           --chunk-parallel N   --socks ADDR:PORT
Limits:    --max-mbit N (exit: megabits per second per key, each direction)   --max-new-streams N (exit: new streams per second per key)
           --drain-timeout 10s (both: how long a stopping service lets connections in flight finish)
Install:   --reconfigure (change an installed instance)   --reinstall (new certificate, secret, paths)   --new-cert   --customize (entry: ask the advanced questions)
Test:      test --matrix [--quick]   (entry) try every carrier x TLS fingerprint with echo sizes, an idle gap, a soak and speeds
Manage:    --force (update even when current)   --purge (uninstall)   --all (update)   -f (logs)   --dry-run, --unit NAME (adopt)   --file FILE, --test (backup/restore)   --ipv6 / --no-ipv6 (exit listens on IPv6 too; default: detected)   --no-secret (show-join)

Environment: GITHUB_TOKEN (private repository), HSTUN_REPO, HSTUN_VERSION, HSTUN_TARBALL=FILE (offline), HSTUN_BUILD=1 (build from source).
EOF
}

# ---------- main ----------
CMD=""
while [ $# -gt 0 ]; do
  case "$1" in
    exit | entry | status | logs | restart | update | show-join | show-secret | uninstall | adopt | forget | test | doctor | edit | rollback | backup | restore | rotate-secret | pin | version | menu | help) CMD=$1 ;;
    install) ;;
    --name) NAME=$2; shift ;;
    --yes | -y) YES=1 ;;
    --version) WANT_VERSION=$2; shift ;;
    --repo) REPO=$2; shift ;;
    --no-firewall) NO_FW=1 ;;
    --mode) MODE=$2; shift ;;
    --forward-target) FORWARD_TARGET=$2; shift ;;
    --port) PORT=$2; shift ;;
    --backend-port) BACKEND_PORT=$2; shift ;;
    --domain) DOMAIN=$2; shift ;;
    --cert) CERT=$2; shift ;;
    --key) KEY=$2; shift ;;
    --acme) ACME=1 ;;
    --email) EMAIL=$2; shift ;;
    --allow-from) ALLOW_FROM=$2; shift ;;
    --decoy-dir) DECOY_DIR=$2; shift ;;
    --public-addr) PUBLIC_ADDR=$2; shift ;;
    --join) JOIN=$2; shift ;;
    --server) SERVER=$2; shift ;;
    --carrier) CARRIER=$2; shift ;;
    --ws-path) WS_PATH=$2; shift ;;
    --http-path) HTTP_PATH=$2; shift ;;
    --secret-file) SECRET_FILE=$2; shift ;;
    --secret-from) SECRET_FROM=$2; shift ;;
    --tls-verify) TLS_VERIFY=$2; shift ;;
    --spki) SPKI=$2; shift ;;
    --connect) CONNECT=$2; shift ;;
    --sni) SNI=$2; shift ;;
    --forward-listen) FORWARD_LISTEN=$2; shift ;;
    --forward-name) FORWARD_NAME=$2; shift ;;
    --conns) CONNS=$2; shift ;;
    --chunk-parallel) CHUNK_PARALLEL=$2; shift ;;
    --socks) SOCKS_LISTEN=$2; shift ;;
    --tls-fingerprint) TLS_FINGERPRINT=$2; shift ;;
    --tls-fragment) TLS_FRAGMENT=$2; shift ;;
    --padding) PADDING=$2; shift ;;
    --session-age) SESSION_AGE=$2; shift ;;
    --matrix) MATRIX=1 ;;
    --quick) QUICK=1 ;;
    --max-mbit) MAX_MBIT=$2; shift ;;
    --max-new-streams) MAX_NEW_STREAMS=$2; shift ;;
    --drain-timeout) DRAIN_TIMEOUT=$2; shift ;;
    --purge) PURGE=1 ;;
    --force) FORCE=1 ;;
    --finish) FINISH=1 ;;
    --clear) FINISH=1 ;;
    --ipv6) IPV6=1 ;;
    --no-ipv6) IPV6=0 ;;
    --test) FILE_TEST=1 ;;
    --file) FILE=$2; shift ;;
    --reconfigure) RECONFIGURE=1 ;;
    --reinstall) REINSTALL=1 ;;
    --new-cert) NEW_CERT=1 ;;
    --customize) CUSTOMIZE=1 ;;
    --no-secret) NO_SECRET=1 ;;
    --all) ALL=1 ;;
    --dry-run) DRY_RUN=1 ;;
    --unit) ADOPT_ONLY=$2; shift ;;
    -f) FOLLOW=1 ;;
    -h | --help) CMD=help ;;
    *) if { [ "$CMD" = backup ] || [ "$CMD" = restore ] || [ "$CMD" = pin ]; } && [ -z "$FILE" ]; then FILE=$1; else die "unknown argument: $1 (see: install.sh help)"; fi ;;
  esac
  shift
done
case "$NAME" in "" | [A-Za-z0-9]*) ;; *) die "--name must be letters and digits" ;; esac
[ -z "$NAME" ] || case "$NAME" in *[!A-Za-z0-9_]*) die "--name must be letters, digits or underscore" ;; esac
set_paths
case "$CMD" in
  exit) install_exit ;;
  entry) install_entry ;;
  status) cmd_status ;;
  logs) cmd_logs ;;
  restart) cmd_restart ;;
  update) cmd_update ;;
  show-join) cmd_show_join ;;
  show-secret) cmd_show_secret ;;
  uninstall) cmd_uninstall ;;
  adopt) cmd_adopt ;;
  forget) cmd_forget ;;
  test) cmd_test ;;
  doctor) cmd_doctor ;;
  edit) cmd_edit ;;
  rollback) cmd_rollback ;;
  backup) cmd_backup ;;
  restore) cmd_restore ;;
  rotate-secret) cmd_rotate_secret ;;
  pin) cmd_pin ;;
  version) cmd_version ;;
  help) usage ;;
  menu | "") if [ -n "$TTY" ]; then menu; else usage; fi ;;
esac
