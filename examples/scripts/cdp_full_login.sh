#!/bin/bash
# Full operation using CDP click + CDP drag only (no X11, no isolated world JS)
set +e
API="http://127.0.0.1:8120"

echo "=== 1. Restart SamVNC ==="
ssh -o ConnectTimeout=10 login07 '
for p in $(ps -u $USER -o pid,cmd | grep samvnc-server | grep -v grep | awk "{print \$1}"); do kill -9 $p 2>/dev/null; done
sleep 2
screen -dmS samvnc bash -c "exec ~/singularity/proot-5.1.0/src/proot -r ~/singularity/rootfs -b /dev -b /proc -b /sys -b /tmp /bin/bash -c \"export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin; export HOME=/root; export DISPLAY=:101; exec /root/samvnc-server -listen=127.0.0.1:8120 -tunnel=false -cdp-addr=127.0.0.1:9222 -verbose\""
sleep 15
echo restarted
'

echo "=== 2. Fresh navigate ==="
curl -sS -m 30 -X POST $API/api/navigate -H "Content-Type: application/json" -d '{"url":"https://modelscope.cn/login"}'
echo
sleep 8

echo "=== 3. CDP click on SMS tab (951, 160) ==="
curl -sS -m 10 -X POST $API/api/cdp/click -H "Content-Type: application/json" -d '{"x":951,"y":160}'
echo
sleep 2

echo "=== 4. Verify SMS tab active ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"var tabs=document.querySelectorAll(\".login-tabs-tab\"); JSON.stringify([{t:tabs[0]?.textContent.trim().substring(0,10),a:tabs[0]?.className.includes(\"active\")},{t:tabs[1]?.textContent.trim().substring(0,10),a:tabs[1]?.className.includes(\"active\")}])"}'
echo

echo "=== 5. CDP click on phone input (893, 201) ==="
curl -sS -m 10 -X POST $API/api/cdp/click -H "Content-Type: application/json" -d '{"x":893,"y":201}'
echo
sleep 1

echo "=== 6. Type phone via X11 (X11 type works for text input) ==="
curl -sS -m 10 -X POST $API/api/type -H "Content-Type: application/json" -d '{"text":"13528475138"}'
echo
sleep 1

echo "=== 7. Verify phone ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"document.getElementById(\"fm-sms-login-id\")?.value||\"no input\""}'
echo

echo "=== 8. CDP click on agreement checkbox (728, 305) ==="
curl -sS -m 10 -X POST $API/api/cdp/click -H "Content-Type: application/json" -d '{"x":728,"y":305}'
echo
sleep 1

echo "=== 9. Verify checkbox ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"document.getElementById(\"fm-agreement-checkbox\")?.checked||false"}'
echo

echo "=== 10. Check slider ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"var w=document.getElementById(\"nc_2_wrapper\"); var s=document.getElementById(\"nc_2_n1z\"); JSON.stringify({wrapperH:w?w.offsetHeight:0,sliderH:s?s.offsetHeight:0,sliderCls:s?s.className:\"none\",text:document.querySelector(\".scale_text\")?.textContent||\"none\"})"}'
echo

echo "=== 11. If slider exists, CDP drag ==="
curl -sS -m 30 -X POST $API/api/cdp/drag -H "Content-Type: application/json" \
    -d '{"x1":731,"y1":330,"x2":1061,"y2":330,"steps":25}'
echo
sleep 3

echo "=== 12. Check slider after drag ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"document.querySelector(\".scale_text\")?.textContent||\"none\""}'
echo

echo "=== 13. Screenshot ==="
curl -sS -m 10 -o /tmp/cdp_result.png $API/api/screenshot 2>&1
ls -la /tmp/cdp_result.png 2>&1

echo "=== 14. Start aitun ==="
nohup setsid ~/.local/bin/aitun -p 8120 --host 127.0.0.1 --no-p2p > ~/aitun_8120b.log 2>&1 &
disown
sleep 12
echo "=== 15. URL ==="
grep -oE "https://aitun\.cc/[A-Z0-9]+" ~/aitun_8120b.log 2>&1 | head -1
