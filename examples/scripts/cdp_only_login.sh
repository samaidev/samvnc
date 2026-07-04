#!/bin/bash
# Full login using ONLY CDP operations (no X11, no isolated world JS)
set +e
API="http://127.0.0.1:8120"

echo "=== 1. Restart SamVNC ==="
ssh -o ConnectTimeout=10 login07 '
for p in $(ps -u $USER -o pid,cmd | grep samvnc-server | grep -v grep | awk "{print \$1}"); do kill -9 $p 2>/dev/null; done
sleep 2
screen -dmS samvnc bash -c "exec ~/singularity/proot-5.1.0/src/proot -r ~/singularity/rootfs -b /dev -b /proc -b /sys -b /tmp /bin/bash -c \"export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin; export HOME=/root; export DISPLAY=:101; exec /root/samvnc-server -listen=127.0.0.1:8120 -tunnel=false -cdp-addr=127.0.0.1:9222 -verbose\""
sleep 15
'

echo "=== 2. Fresh navigate ==="
curl -sS -m 30 -X POST $API/api/navigate -H "Content-Type: application/json" -d '{"url":"https://modelscope.cn/login"}'
echo
sleep 8

echo "=== 3. CDP click SMS tab (951, 160) ==="
curl -sS -m 10 -X POST $API/api/cdp/click -H "Content-Type: application/json" -d '{"x":951,"y":160}'
echo
sleep 2

echo "=== 4. Verify SMS active ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"var t=document.querySelectorAll(\".login-tabs-tab\"); t[1]?.className.includes(\"active\")"}'
echo

echo "=== 5. CDP click phone input (893, 201) ==="
curl -sS -m 10 -X POST $API/api/cdp/click -H "Content-Type: application/json" -d '{"x":893,"y":201}'
echo
sleep 1

echo "=== 6. CDP type phone number ==="
curl -sS -m 10 -X POST $API/api/cdp/type -H "Content-Type: application/json" -d '{"text":"13528475138"}'
echo
sleep 1

echo "=== 7. Verify phone ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"document.getElementById(\"fm-sms-login-id\")?.value||\"empty\""}'
echo

echo "=== 8. CDP click checkbox (728, 305) ==="
curl -sS -m 10 -X POST $API/api/cdp/click -H "Content-Type: application/json" -d '{"x":728,"y":305}'
echo
sleep 1

echo "=== 9. Verify checkbox ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"document.getElementById(\"fm-agreement-checkbox\")?.checked||false"}'
echo

echo "=== 10. Check slider ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"var w=document.getElementById(\"nc_2_wrapper\"); var s=document.getElementById(\"nc_2_n1z\"); JSON.stringify({wrapperH:w?w.offsetHeight:0,sliderH:s?s.offsetHeight:0,text:document.querySelector(\".scale_text\")?.textContent||\"none\"})"}'
echo

echo "=== 11. CDP drag slider (731, 330) -> (1061, 330) ==="
curl -sS -m 30 -X POST $API/api/cdp/drag -H "Content-Type: application/json" \
    -d '{"x1":731,"y1":330,"x2":1061,"y2":330,"steps":25}'
echo
sleep 3

echo "=== 12. Check slider status ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"document.querySelector(\".scale_text\")?.textContent||\"none\""}'
echo

echo "=== 13. If verified, click Get code ==="
# Use CDP click on Get code button
# Get code is at offsetTop~69, offsetLeft~288 within iframe
# So 721+288+28=1037, 141+69+20=230
curl -sS -m 10 -X POST $API/api/cdp/click -H "Content-Type: application/json" -d '{"x":1037,"y":230}'
echo
sleep 5

echo "=== 14. Check button text ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"document.querySelector(\".send-btn-link\")?.textContent||\"none\""}'
echo

echo "=== 15. Screenshot ==="
curl -sS -m 10 -o /tmp/final_login.png $API/api/screenshot 2>&1
ls -la /tmp/final_login.png 2>&1

echo "=== 16. Start aitun ==="
nohup setsid ~/.local/bin/aitun -p 8120 --host 127.0.0.1 --no-p2p > ~/aitun_final.log 2>&1 &
disown
sleep 12
echo "=== 17. URL ==="
grep -oE "https://aitun\.cc/[A-Z0-9]+" ~/aitun_final.log 2>&1 | head -1
