#!/usr/bin/env bash
#
# Example usage of the ms-login service.
# Run this after starting the service with ./run.sh or python3 daemon.py
#
set -euo pipefail

HOST="${MS_LOGIN_HOST:-127.0.0.1}"
PORT="${MS_LOGIN_PORT:-8765}"
BASE="http://${HOST}:${PORT}"

echo "=== 1. Health check ==="
curl -s "${BASE}/health" | python3 -m json.tool
echo ""

echo "=== 2. Set phone number (replace with your real number) ==="
curl -s -X POST "${BASE}/config" \
  -H 'Content-Type: application/json' \
  -d '{"phone": "13800138000"}' | python3 -m json.tool
echo ""

echo "=== 3. Start login flow (this navigates, switches to SMS tab, fills phone, clicks Get code) ==="
RESP=$(curl -s -X POST "${BASE}/login/start" \
  -H 'Content-Type: application/json' \
  -d '{"phone": "13800138000"}')
echo "$RESP" | python3 -c "
import json, sys
d = json.load(sys.stdin)
d.pop('screenshot_base64', None)
print(json.dumps(d, indent=2, ensure_ascii=False))
"
SID=$(echo "$RESP" | python3 -c "import json,sys; print(json.load(sys.stdin)['session_id'])")
echo ""
echo "session_id = $SID"
echo ""

echo "=== 4. Check current status ==="
curl -s "${BASE}/login/status" | python3 -c "
import json, sys
d = json.load(sys.stdin)
d.pop('screenshot_base64', None)
print(json.dumps(d, indent=2, ensure_ascii=False))
"
echo ""

echo "=== 5. After receiving SMS, submit code ==="
echo "  curl -X POST ${BASE}/login/sms -H 'Content-Type: application/json' \\"
echo "    -d '{\"session_id\": \"$SID\", \"code\": \"1234\"}'"
echo ""

echo "=== 6. If CAPTCHA appeared, open VNC URL in browser, complete manually, then: ==="
echo "  curl -X POST ${BASE}/login/continue -H 'Content-Type: application/json' \\"
echo "    -d '{\"session_id\": \"$SID\"}'"
echo ""

echo "=== 7. List all sessions ==="
curl -s "${BASE}/sessions" | python3 -m json.tool
