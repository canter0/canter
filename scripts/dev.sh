#!/bin/sh
# Run the web app and current control plane together for a local demo.
set -eu

cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
web_port=${CANTER_DEV_WEB_PORT:-3006}
api_port=${CANTER_DEV_API_PORT:-8086}
public_url="http://127.0.0.1:$web_port"
api_url="http://127.0.0.1:$api_port"

for port in "$web_port" "$api_port"; do
  case "$port" in ''|*[!0-9]*) echo "Demo ports must be integers." >&2; exit 1 ;; esac
  if [ "$port" -lt 1024 ] || [ "$port" -gt 65535 ]; then
    echo "Demo ports must be between 1024 and 65535." >&2
    exit 1
  fi
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "Port $port is already in use. Choose another CANTER_DEV_WEB_PORT or CANTER_DEV_API_PORT." >&2
    exit 1
  fi
done
if [ "$web_port" = "$api_port" ]; then
  echo "The web app and API need different ports." >&2
  exit 1
fi

mkdir -p bin
go build -o bin/canter-controlplane-dev ./cmd/canter-controlplane
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/canter-static-dev-linux ./cmd/canter-static

api_pid=
web_pid=
cleanup() {
  trap - EXIT INT TERM
  [ -z "$web_pid" ] || kill "$web_pid" 2>/dev/null || true
  [ -z "$api_pid" ] || kill "$api_pid" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

CANTER_PUBLIC_URL="$public_url" CANTER_CONTROLPLANE_ADDR="127.0.0.1:$api_port" \
  CANTER_COOKIE_SECURE=false CANTER_STATIC_SERVER_BINARY="$PWD/bin/canter-static-dev-linux" \
  ./bin/canter-controlplane-dev &
api_pid=$!

attempt=0
until curl --fail --silent --max-time 1 "$api_url/readyz" >/dev/null 2>&1; do
  if ! kill -0 "$api_pid" 2>/dev/null; then
    echo "The control plane stopped before it was ready. Check its error above." >&2
    exit 1
  fi
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 40 ]; then
    echo "The control plane did not become ready." >&2
    exit 1
  fi
  sleep 0.25
done

printf 'Canter demo: %s\nAPI: %s\n' "$public_url" "$api_url"
(
  cd web
  CANTER_API_ORIGIN="$api_url" CANTER_PUBLIC_URL="$public_url" CANTER_NEXT_DIST_DIR=.next-demo \
    exec pnpm exec next dev --webpack --hostname 127.0.0.1 --port "$web_port"
) &
web_pid=$!
wait "$web_pid"
