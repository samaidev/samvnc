#!/usr/bin/env bash
# Launch the ms-login HTTP service.
# Usage: ./run.sh [host] [port]
set -euo pipefail
cd "$(dirname "$0")"

HOST="${1:-${MS_LOGIN_HOST:-127.0.0.1}}"
PORT="${2:-${MS_LOGIN_PORT:-8765}}"

# Use the venv python that has fastapi + uvicorn installed
PYTHON="/home/z/.venv/bin/python3"

export MS_LOGIN_HOST="$HOST"
export MS_LOGIN_PORT="$PORT"

echo "Starting ms-login service on http://${HOST}:${PORT}"
echo "  samvnc: ${SAMVNC_BASE_URL:-https://aitun.cc/GMCLNGFZ}"
echo "  VNC:    ${VNC_URL:-https://aitun.cc/GMCLNGFZ/}"
echo "  Ctrl+C to stop"
exec "$PYTHON" service.py
