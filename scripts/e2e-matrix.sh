#!/usr/bin/env bash
# End-to-end speed matrix through the SOCKS5 port of an hstun server, from the
# server host: exit IP, small HTTPS, 5 MB down, 2 MB up, repeated.
# Usage: e2e-matrix.sh [socks_host:port] [runs]
set -u
SOCKS="${1:-127.0.0.1:34567}"
RUNS="${2:-4}"
echo "== hstun e2e matrix via socks5h://$SOCKS, $RUNS runs"
echo -n "exit IP: "; curl -s --max-time 25 --socks5-hostname "$SOCKS" https://api.ipify.org; echo
for i in $(seq 1 "$RUNS"); do
  echo -n "run $i  small: "
  curl -s -o /dev/null --max-time 25 -w "%{http_code} %{time_total}s" --socks5-hostname "$SOCKS" https://www.cloudflare.com/cdn-cgi/trace
  echo -n "  down 5MB: "
  curl -s -o /dev/null --max-time 90 -w "%{http_code} %{speed_download} B/s" --socks5-hostname "$SOCKS" "https://speed.cloudflare.com/__down?bytes=5000000"
  echo -n "  up 2MB: "
  head -c 2000000 /dev/urandom | curl -s -o /dev/null --max-time 90 -w "%{http_code} %{speed_upload} B/s" --socks5-hostname "$SOCKS" --data-binary @- https://speed.cloudflare.com/__up
  echo
done
