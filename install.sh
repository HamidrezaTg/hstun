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
#   install.sh exit   [flags]   the server side: listens, is the exit (kharej host)
#   install.sh entry  [flags]   the client side: owns the client-facing ports (Iran host)
#   hstun-ctl status|logs|restart|update|show-join|show-secret|uninstall   after installing
# Run it with no arguments for a menu. Every question has a flag or an HSTUN_* environment variable,
# so it runs unattended with --yes. See README.md ("Install").
set -Eeuo pipefail

INSTALLER_VERSION="0.2.11"
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
if [ -t 1 ]; then R=$'\e[31m'; G=$'\e[32m'; Y=$'\e[33m'; B=$'\e[1m'; N=$'\e[0m'; else R=; G=; Y=; B=; N=; fi
info() { printf '%s\n' "${G}[hstun]${N} $*"; }
warn() { printf '%s\n' "${Y}[hstun] warning:${N} $*" >&2; }
die() { printf '%s\n' "${R}[hstun] error:${N} $*" >&2; exit 1; }
# shellcheck disable=SC2154  # rc is set inside the trap string
trap 'rc=$?; [ -n "${STAGE:-}" ] && rm -rf "$STAGE"; exit $rc' EXIT

# ---------- input ----------
YES=0
TTY=""
if [ -t 0 ]; then TTY=/dev/stdin; elif [ -r /dev/tty ] && (: </dev/tty) 2>/dev/null; then TTY=/dev/tty; fi

# ask VAR "prompt" [default] [optional]: keeps a value that is already set (flag, env, saved config).
ask() {
  local name=$1 prompt=$2 def=${3-} opt=${4-} ans
  if [ -n "${!name-}" ]; then return 0; fi
  if [ "$YES" = 1 ] || [ -z "$TTY" ]; then
    if [ -z "$def" ] && [ -z "$opt" ]; then die "missing value: $prompt (pass the matching flag, see: install.sh help)"; fi
    printf -v "$name" '%s' "$def"
    return 0
  fi
  read -r -p "$prompt${def:+ [$def]}: " ans <"$TTY" || true
  printf -v "$name" '%s' "${ans:-$def}"
  if [ -z "${!name}" ] && [ -z "$opt" ]; then die "a value is required: $prompt"; fi
}
ask_secret() { # VAR "prompt": hidden input
  local name=$1 prompt=$2 ans
  [ -z "$TTY" ] && die "no terminal to read the secret from; use --secret-file or --secret-from"
  read -r -s -p "$prompt: " ans <"$TTY" || true
  echo
  printf -v "$name" '%s' "$ans"
}
confirm() { # "question" [y|n default]
  local q=$1 def=${2:-n} ans
  if [ "$YES" = 1 ]; then return 0; fi
  if [ -z "$TTY" ]; then [ "$def" = y ]; return; fi
  read -r -p "$q [$([ "$def" = y ] && echo Y/n || echo y/N)]: " ans <"$TTY" || true
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
      if [ "$APT_UPDATED" = 0 ]; then apt-get update -qq >/dev/null; APT_UPDATED=1; fi
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" >/dev/null ;;
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
  [ -x "$BIN_DIR/hstun" ] && cp -p "$BIN_DIR/hstun" "$BIN_DIR/hstun.prev"
  install -m 0755 "$STAGE/hstun" "$BIN_DIR/hstun"
  rm -rf "$SHARE_DIR/deploy" "$SHARE_DIR/decoy-example" "$SHARE_DIR/licenses"
  cp -r "$STAGE/share/." "$SHARE_DIR/"
  install -m 0755 "$STAGE/share/install.sh" "$BIN_DIR/hstun-ctl"
  info "installed $("$BIN_DIR/hstun" version) to $BIN_DIR"
}

# ---------- instance state ----------
NAME=""
ROLE="" MODE="" FORWARD_TARGET="" PORT="" DOMAIN="" CERT="" KEY="" ACME="" EMAIL="" ALLOW_FROM="" DECOY_DIR="" BACKEND_PORT=""
JOIN="" SERVER="" CARRIER="" WS_PATH="" HTTP_PATH="" SECRET_FILE="" SECRET_FROM="" TLS_VERIFY="" SPKI="" CONNECT="" SNI=""
FORWARD_LISTEN="" FORWARD_NAME="" CONNS="" CHUNK_PARALLEL="" SOCKS_LISTEN="" NO_FW="" ADMIN_PORT="" SELF_SIGNED="" PUBLIC_ADDR=""
FOLLOW=0 PURGE=0 ALL=0 DRY_RUN=0 ADOPT_ONLY=""
ADOPTED="" ADOPT_UNITS="" ADOPT_BIN=""

set_paths() {
  ETC="$ETC_BASE/hstun${NAME:+-$NAME}"
  UNIT="hstun${NAME:+-$NAME}"
  DECOY_TARGET="$WWW_DIR/hstun${NAME:+-$NAME}-decoy"
}
CONF_KEYS=(ROLE MODE FORWARD_TARGET PORT DOMAIN CERT KEY BACKEND_PORT ADMIN_PORT WS_PATH HTTP_PATH SERVER CARRIER TLS_VERIFY SPKI CONNECT SNI
  FORWARD_LISTEN FORWARD_NAME CONNS CHUNK_PARALLEL SOCKS_LISTEN ALLOW_FROM PUBLIC_ADDR FW_PORTS CONNLIMIT_PORT SELF_SIGNED
  ADOPTED ADOPT_UNITS ADOPT_BIN)
FW_PORTS="" CONNLIMIT_PORT=""
load_conf() { # saved values only fill what is still empty (flags win)
  [ -f "$ETC/install.conf" ] || return 0
  local k v line
  while IFS= read -r line; do
    k=${line%%=*}; v=${line#*=}
    case " ${CONF_KEYS[*]} " in *" $k "*) [ -n "${!k-}" ] || printf -v "$k" '%s' "$(eval "printf '%s' $v")" ;; esac
  done <"$ETC/install.conf"
}
save_conf() {
  local k
  { for k in "${CONF_KEYS[@]}"; do printf '%s=%q\n' "$k" "${!k-}"; done; } >"$ETC/install.conf"
  chmod 0600 "$ETC/install.conf"
}
reset_conf() { local k; for k in "${CONF_KEYS[@]}"; do printf -v "$k" '%s' ""; done; }
inst_bin() { echo "${ADOPT_BIN:-$BIN_DIR/hstun}"; }
# an existing hstun that this script did not create must never be overwritten by an install
guard_existing() {
  if [ -n "$ADOPTED" ]; then die "instance ${NAME:-default} was adopted from a hand-made setup; install would rewrite its files. Use update, restart or uninstall, or pick another --name."; fi
  if [ ! -f "$ETC/install.conf" ] && { [ -f "$ETC/server.toml" ] || [ -f "$ETC/client.toml" ]; }; then
    die "found an hstun setup in $ETC that was not installed by hstun-ctl. Run: install.sh adopt   (or choose another --name)"
  fi
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
fw_connlimit() { # port: at most 64 concurrent connections per source IP (nginx limit_conn does not count silent connections)
  [ "$TEST" = 1 ] && return 0
  [ -n "$NO_FW" ] && return 0
  if ufw_active && [ -f /etc/ufw/before.rules ]; then
    if ! grep -q "hstun-connlimit $UNIT begin" /etc/ufw/before.rules; then
      cp -p /etc/ufw/before.rules "/etc/ufw/before.rules.pre-$UNIT"
      awk -v u="$UNIT" -v p="$1" '{print} /^-A ufw-before-input -i lo -j ACCEPT/ && !d {
        print "# hstun-connlimit " u " begin"
        print "-A ufw-before-input -p tcp --dport " p " --syn -m connlimit --connlimit-above 64 --connlimit-mask 32 -j REJECT --reject-with tcp-reset"
        print "# hstun-connlimit " u " end"; d=1 }' /etc/ufw/before.rules >/etc/ufw/before.rules.new && mv /etc/ufw/before.rules.new /etc/ufw/before.rules
      ufw reload >/dev/null
      # shellcheck disable=SC2034  # saved to install.conf through CONF_KEYS
      CONNLIMIT_PORT=$1
      info "ufw: per-source cap of 64 concurrent connections on $1/tcp"
    fi
  else
    warn "ufw is not active: add a per-source connection cap on $1/tcp yourself (iptables -m connlimit --connlimit-above 64)"
  fi
}
fw_remove() {
  [ "$TEST" = 1 ] && return 0
  local p
  if ufw_active; then
    for p in $FW_PORTS; do ufw --force delete allow "$p/tcp" >/dev/null 2>&1 || true; done
    if grep -q "hstun-connlimit $UNIT begin" /etc/ufw/before.rules 2>/dev/null; then
      sed -i "/# hstun-connlimit $UNIT begin/,/# hstun-connlimit $UNIT end/d" /etc/ufw/before.rules
      ufw reload >/dev/null
    fi
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
        listen [::]:$PORT ssl http2;
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
render_server_toml() {
  local listen tls="" trust="" allow=""
  case "$MODE" in
    nginx) listen="127.0.0.1:$BACKEND_PORT"; trust='trust_proxy  = "loopback"' ;;
    cloudflared) listen="127.0.0.1:$BACKEND_PORT"; trust='trust_proxy  = "cloudflared"' ;;
    direct)
      listen="0.0.0.0:$PORT"
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
  local verify="tls_verify  = \"system\"" conn="" sni="" socks="" ch="" conns=$CONNS
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
ping        = "25s"
idle_timeout = "10m"
max_streams_per_session = 256
forward_max_conns_per_ip = 64
admin_listen = "127.0.0.1:$ADMIN_PORT"

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
  echo "${a:-YOUR.SERVER.IP}"
}
make_join() { # prints the join string (everything the entry needs except the secret)
  local host=${DOMAIN:-${PUBLIC_ADDR:-$(detect_addr)}} srv port=$PORT tls=system spki="" car=${CARRIER:-ws}
  [ "$MODE" = cloudflared ] && { port=443; car=chunk; }
  srv="$host:$port"
  if [ "$MODE" = direct ] && [ -z "$DOMAIN" ] || { [ "$MODE" = direct ] && [ -n "$SELF_SIGNED" ]; }; then tls=spki; spki=$(spki_of "$ETC/cert.pem"); fi
  printf 'hstun-join:%s' "$(printf 'server=%s\ncarrier=%s\nws_path=%s\nhttp_path=%s\ntls=%s\nspki=%s\n' "$srv" "$car" "$WS_PATH" "$HTTP_PATH" "$tls" "$spki" | base64 -w0)"
}
start_unit() { # unit
  sysd daemon-reload
  sysd enable --now "$1" >/dev/null 2>&1 || sysd restart "$1"
  sysd restart "$1"
}

install_exit() {
  need_root
  set_paths; load_conf; guard_existing
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
  ask FORWARD_TARGET "Where does the tunnelled TCP go on this host (the loopback inbound of your panel, host:port)" ""
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
      ask ALLOW_FROM "Only accept connections from these CIDRs (comma separated, empty = anyone)" "" opt
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
      else SELF_SIGNED=1; cert_selfsigned "$ETC"; fi
      own "$SVC_USER:$SVC_USER" "$ETC/cert.pem" "$ETC/key.pem"; chmod 0600 "$ETC/key.pem" ;;
  esac

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
  local join; join=$(make_join)
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
  echo "  Join string for the entry host (it contains everything except the secret):"
  echo "    $join"
  echo
  echo "  On the entry host run:"
  echo "    hstun-ctl entry --join '$join' --secret-from root@THIS-HOST"
  echo "  or copy the secret yourself:  hstun-ctl show-secret   (here)   then   --secret-file FILE   (there)"
  echo "  Create the inner proxy (for example a VLESS inbound) listening on $FORWARD_TARGET before testing."
}
ensure_user_account() {
  [ "$TEST" = 1 ] && return 0
  id "$SVC_USER" >/dev/null 2>&1 && return 0
  local uid=61999 sh; sh=$(command -v nologin || echo /usr/sbin/nologin)
  while getent passwd "$uid" >/dev/null || getent group "$uid" >/dev/null; do uid=$((uid - 1)); done
  useradd --system --uid "$uid" --user-group --no-create-home --shell "$sh" "$SVC_USER"
  info "created the service account $SVC_USER (uid $uid)"
}

parse_join() {
  local raw=${JOIN#hstun-join:} text k v
  text=$(printf '%s' "$raw" | base64 -d 2>/dev/null) || die "the join string is not valid base64"
  while IFS='=' read -r k v; do
    case "$k" in
      server) [ -n "$SERVER" ] || SERVER=$v ;;
      carrier) [ -n "$CARRIER" ] || CARRIER=$v ;;
      ws_path) [ -n "$WS_PATH" ] || WS_PATH=$v ;;
      http_path) [ -n "$HTTP_PATH" ] || HTTP_PATH=$v ;;
      tls) [ -n "$TLS_VERIFY" ] || TLS_VERIFY=$v ;;
      spki) [ -n "$SPKI" ] || SPKI=$v ;;
    esac
  done <<<"$text"
}
install_entry() {
  need_root
  set_paths; load_conf; guard_existing
  ROLE=entry
  [ -n "$JOIN" ] && parse_join
  ask SERVER "Exit address (host:port)" ""
  ask CARRIER "Carrier (ws, http or chunk; chunk is for paths that cut long connections)" ws
  case "$CARRIER" in ws | http | chunk) ;; *) die "--carrier must be ws, http or chunk" ;; esac
  ask WS_PATH "WebSocket path (from the join string)" ""
  ask HTTP_PATH "HTTP path (from the join string)" ""
  ask TLS_VERIFY "Certificate check (system = CA certificate, spki = pinned self-signed)" system
  [ "$TLS_VERIFY" = spki ] && ask SPKI "SPKI sha256 of the exit certificate (hex)" ""
  case "$TLS_VERIFY" in system | spki) ;; *) die "--tls-verify must be system or spki" ;; esac
  ask CONNECT "Dial this ip:port instead of resolving the name (empty = resolve)" "" opt
  ask SNI "TLS server name (empty = the host of the exit address)" "" opt
  ask FORWARD_LISTEN "Client-facing listen address for the tunnel (host:port)" "127.0.0.1:20151"
  ask FORWARD_NAME "Forward name (must match the exit; default x1)" x1
  if [ "$CARRIER" = chunk ]; then CONNS=${CONNS:-1}; ask CHUNK_PARALLEL "Parallel requests for the chunk carrier" 8; else CONNS=${CONNS:-2}; fi
  ADMIN_PORT=${ADMIN_PORT:-$(free_port 9110)}
  case "$FORWARD_LISTEN" in *:*) ;; *) die "--forward-listen must look like 127.0.0.1:20151" ;; esac
  local lp=${FORWARD_LISTEN##*:}
  if ! [ -f "$ETC/install.conf" ] && port_busy "$lp"; then die "port $lp is already in use; pick another with --forward-listen"; fi

  fetch_release; install_files; ensure_user_account
  prepare_dirs
  if [ ! -s "$ETC/secret" ]; then
    if [ -n "$SECRET_FILE" ]; then install -m 0600 "$SECRET_FILE" "$ETC/secret"
    elif [ -n "$SECRET_FROM" ]; then
      ensure_cmds ssh:openssh-client
      info "fetching the secret from $SECRET_FROM over ssh (it is not printed)"
      local target=${SECRET_FROM%%:*} sport=""; case "$SECRET_FROM" in *:*) sport=${SECRET_FROM##*:} ;; esac
      ( umask 077; ssh ${sport:+-p "$sport"} "$target" "cat ${REMOTE_SECRET:-/etc/hstun/secret} 2>/dev/null || sudo -n cat ${REMOTE_SECRET:-/etc/hstun/secret}" >"$ETC/secret" ) || die "could not fetch the secret from $SECRET_FROM"
    else
      local s=""; ask_secret s "Paste the secret (output of 'hstun-ctl show-secret' on the exit; input is hidden)"
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
  info "${B}entry installed${N}${NAME:+ (instance $NAME)}: forward $FORWARD_NAME on $FORWARD_LISTEN -> $SERVER ($CARRIER)"
  if have_systemd || [ "$TEST" = 1 ]; then
    local n=0
    for _ in $(seq 1 30); do
      n=$(curl -s -m 2 "http://127.0.0.1:$ADMIN_PORT/stats" 2>/dev/null | grep -o '"carrier"' | wc -l || true)
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
cmd_update() {
  need_root; set_paths; need_installed
  local before; before=$("$(inst_bin)" version 2>/dev/null || true)
  fetch_release
  local dirs=() d u
  if [ "$ALL" = 1 ]; then
    for d in "$ETC_BASE"/hstun "$ETC_BASE"/hstun-*; do [ -f "$d/install.conf" ] && dirs+=("$d"); done
  else dirs=("$ETC"); fi
  for d in "${dirs[@]}"; do ( reset_conf; NAME=${d##*/hstun}; NAME=${NAME#-}; set_paths; load_conf; preflight_conf ); done
  install_files
  for d in "${dirs[@]}"; do
    (
      reset_conf; NAME=${d##*/hstun}; NAME=${NAME#-}; set_paths; load_conf
      if [ -n "$ADOPT_BIN" ] && [ "$ADOPT_BIN" != "$BIN_DIR/hstun" ]; then
        [ -x "$ADOPT_BIN" ] && cp -p "$ADOPT_BIN" "$ADOPT_BIN.prev"
        install -m 0755 "$BIN_DIR/hstun" "$ADOPT_BIN"
      fi
      for u in $(daemon_units); do sysd restart "$u"; done
    )
  done
  [ "$ALL" = 1 ] || info "restarted this instance only; other instances keep running the old binary until restarted (hstun-ctl update --all)"
  info "updated: $before -> $("$(inst_bin)" version)   (previous binary kept as $(inst_bin).prev)"
}
cmd_show_join() {
  set_paths; need_installed
  [ "$ROLE" = exit ] || die "show-join is for an exit"
  make_join; echo
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

cmd_version() { echo "hstun-ctl $INSTALLER_VERSION; binary: $("$BIN_DIR/hstun" version 2>/dev/null || echo not installed)"; }

menu() {
  [ -n "$TTY" ] || die "no terminal; run: install.sh help"
  while true; do
    echo
    echo "${B}hstun $INSTALLER_VERSION${N}"
    echo "  1) Install an exit  (server side, on the kharej host)"
    echo "  2) Install an entry (client side, on the Iran host)"
    echo "  3) Status"
    echo "  4) Update to the latest release"
    echo "  5) Show join string (exit)"
    echo "  6) Logs"
    echo "  7) Uninstall"
    echo "  8) Adopt hand-installed hstun units (manage them with hstun-ctl)"
    echo "  0) Quit"
    local c=""; read -r -p "Choose: " c <"$TTY" || exit 0
    case "$c" in
      1) install_exit ;;
      2) install_entry ;;
      3) cmd_status ;;
      4) cmd_update ;;
      5) cmd_show_join ;;
      6) cmd_logs ;;
      7) cmd_uninstall ;;
      8) cmd_adopt ;;
      0 | q | "") exit 0 ;;
    esac
  done
}

usage() {
  cat <<EOF
hstun installer/manager $INSTALLER_VERSION

  install.sh                          menu
  install.sh exit  [flags]            install the server side
  install.sh entry [flags]            install the client side
  install.sh adopt [--dry-run] [--unit U]   register hand-installed hstun units (writes one small file per instance, changes nothing else)
  hstun-ctl status|logs|restart|update|show-join|show-secret|uninstall|forget|version   (installed by the first run)

Common:    --name NAME   --yes   --version X.Y.Z   --repo OWNER/REPO   --no-firewall
Exit:      --mode nginx|direct|cloudflared   --forward-target HOST:PORT   --port N   --domain D
           --cert FILE --key FILE | --acme [--email E]   --allow-from CIDR[,CIDR]   --decoy-dir DIR   --public-addr ADDR   --backend-port N
Entry:     --join STRING   --server HOST:PORT   --carrier ws|http|chunk   --ws-path P   --http-path P
           --secret-file FILE | --secret-from USER@HOST[:SSHPORT]   --tls-verify system|spki   --spki HEX
           --connect IP:PORT   --sni NAME   --forward-listen ADDR:PORT   --forward-name NAME   --conns N
           --chunk-parallel N   --socks ADDR:PORT
Manage:    --purge (uninstall)   --all (update)   -f (logs)   --dry-run, --unit NAME (adopt)

Environment: GITHUB_TOKEN (private repository), HSTUN_REPO, HSTUN_VERSION, HSTUN_TARBALL=FILE (offline), HSTUN_BUILD=1 (build from source).
EOF
}

# ---------- main ----------
CMD=""
while [ $# -gt 0 ]; do
  case "$1" in
    exit | entry | status | logs | restart | update | show-join | show-secret | uninstall | adopt | forget | version | menu | help) CMD=$1 ;;
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
    --purge) PURGE=1 ;;
    --all) ALL=1 ;;
    --dry-run) DRY_RUN=1 ;;
    --unit) ADOPT_ONLY=$2; shift ;;
    -f) FOLLOW=1 ;;
    -h | --help) CMD=help ;;
    *) die "unknown argument: $1 (see: install.sh help)" ;;
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
  version) cmd_version ;;
  help) usage ;;
  menu | "") if [ -n "$TTY" ]; then menu; else usage; fi ;;
esac
