#!/usr/bin/env bash
# Starts the benchmark's MCP servers as long-running HTTP services, so no arm
# pays (or times out on) server start-up: the official VictoriaMetrics server
# takes ~40s to become ready, longer than Claude Code waits for a stdio server.
#
#   ./bench/servers.sh up     # start and wait until all four answer
#   ./bench/servers.sh down
#
# Ports: 18081 metrics, 18082 logs, 18083 traces (official servers), 18084 spoor.
set -euo pipefail
cd "$(dirname "$0")/.."

NET=${BENCH_NETWORK:-test_default}           # docker network of the backends (make it-up)
VM=${BENCH_VM:-http://victoriametrics:8428}  # backend URLs as seen from that network
VL=${BENCH_VL:-http://victorialogs:9428}
VT=${BENCH_VT:-http://victoriatraces:10428}
SPOOR_BIN=${SPOOR_BIN:-$PWD/bin/spoor}
PIDFILE=bench/results/spoor.pid

initialize='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"bench","version":"0"}}}'

ready() {
  curl -s -m 5 -X POST "http://localhost:$1/mcp" -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' -d "$initialize" | grep -q '"result"'
}

up() {
  mkdir -p bench/results
  docker run -d --rm --name bench-mcp-vm --network "$NET" -p 18081:8080 \
    -e VM_INSTANCE_ENTRYPOINT="$VM" -e VM_INSTANCE_TYPE=single -e MCP_SERVER_MODE=http -e MCP_LISTEN_ADDR=:8080 \
    ghcr.io/victoriametrics/mcp-victoriametrics >/dev/null
  docker run -d --rm --name bench-mcp-vl --network "$NET" -p 18082:8080 \
    -e VL_INSTANCE_ENTRYPOINT="$VL" -e MCP_SERVER_MODE=http -e MCP_LISTEN_ADDR=:8080 \
    ghcr.io/victoriametrics/mcp-victorialogs >/dev/null
  docker run -d --rm --name bench-mcp-vt --network "$NET" -p 18083:8080 \
    -e VT_INSTANCE_ENTRYPOINT="$VT" -e MCP_SERVER_MODE=http -e MCP_LISTEN_ADDR=:8080 \
    ghcr.io/victoriametrics-community/mcp-victoriatraces >/dev/null
  [ -x "$SPOOR_BIN" ] || { echo "build first: make build" >&2; exit 1; }
  SPOOR_METRICS_URL=${SPOOR_METRICS_URL:-http://localhost:8428} \
  SPOOR_LOGS_URL=${SPOOR_LOGS_URL:-http://localhost:9428} \
  SPOOR_TRACES_URL=${SPOOR_TRACES_URL:-http://localhost:10428} \
    nohup "$SPOOR_BIN" serve --http :18084 > bench/results/spoor.log 2>&1 &
  echo $! > "$PIDFILE"
  for port in 18081 18082 18083 18084; do
    for _ in $(seq 1 60); do ready "$port" && break; sleep 2; done
    ready "$port" || { echo "MCP server on :$port did not become ready" >&2; exit 1; }
    echo "ready :$port"
  done
}

down() {
  docker rm -f bench-mcp-vm bench-mcp-vl bench-mcp-vt >/dev/null 2>&1 || true
  [ -f "$PIDFILE" ] && kill "$(cat "$PIDFILE")" 2>/dev/null || true
  rm -f "$PIDFILE"
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  *) echo "usage: $0 up|down" >&2; exit 2 ;;
esac
