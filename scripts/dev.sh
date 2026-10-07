#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
AGENT_PID=""
SERVER_PID=""
WEB_PID=""

cleanup() {
  local exit_code=$?
  trap - EXIT INT TERM
  for pid in "$WEB_PID" "$SERVER_PID" "$AGENT_PID"; do
    if [[ -n "$pid" ]]; then
      kill -TERM "$pid" 2>/dev/null || true
    fi
  done
  for pid in "$WEB_PID" "$SERVER_PID" "$AGENT_PID"; do
    if [[ -n "$pid" ]]; then
      wait "$pid" 2>/dev/null || true
    fi
  done
  for executable in "$TMP_DIR/lernae-agent" "$TMP_DIR/lernae-server"; do
    if [[ -e "$executable" ]]; then
      unlink "$executable"
    fi
  done
  rmdir "$TMP_DIR" 2>/dev/null || true
  exit "$exit_code"
}

trap cleanup EXIT
trap 'exit 0' INT TERM

cd "$ROOT"
DEV_API_PROXY_TARGET="$("${GO:-go}" run ./cmd/provider-config dev-api-proxy-target)"
"${GO:-go}" build -o "$TMP_DIR/lernae-agent" ./cmd/agent
"${GO:-go}" build -o "$TMP_DIR/lernae-server" ./cmd/server

"$TMP_DIR/lernae-agent" &
AGENT_PID=$!
"$TMP_DIR/lernae-server" &
SERVER_PID=$!
(cd "$ROOT/apps/web" && LERNAE_DEV_API_PROXY_TARGET="$DEV_API_PROXY_TARGET" "$ROOT/apps/web/node_modules/.bin/vite" --host 127.0.0.1) &
WEB_PID=$!

printf 'Lernae Agent, Server, and Web are starting. Press Ctrl-C to stop all three.\n'
wait -n "$AGENT_PID" "$SERVER_PID" "$WEB_PID"
