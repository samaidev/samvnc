#!/bin/bash
# Test CDP drag on slider - this goes through Chrome's internal input pipeline
set +e
API="http://127.0.0.1:8120"

echo "=== 1. Kill old SamVNC, restart with new binary ==="
ssh -o ConnectTimeout=10 login07 '
for p in $(ps -u $USER -o pid,cmd | grep samvnc-server | grep -v grep | awk "{print \$1}"); do
    kill -9 $p 2>/dev/null
done
sleep 2
screen -dmS samvnc bash -c "exec ~/singularity/proot-5.1.0/src/proot -r ~/singularity/rootfs -b /dev -b /proc -b /sys -b /tmp /bin/bash -c \"export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin; export HOME=/root; export DISPLAY=:101; exec /root/samvnc-server -listen=127.0.0.1:8120 -tunnel=false -cdp-addr=127.0.0.1:9222 -verbose\""
echo "restarted"
sleep 15
'

echo "=== 2. API status ==="
curl -sS -m 5 $API/api/status
echo

echo "=== 3. Fresh navigate to login ==="
curl -sS -m 30 -X POST $API/api/navigate -H "Content-Type: application/json" -d '{"url":"https://modelscope.cn/login"}'
echo
sleep 5

echo "=== 4. Switch to SMS + fill phone + check agreement ==="
curl -sS -m 15 -X POST $API/api/iframe/click -H "Content-Type: application/json" \
    -d '{"iframe":"passport","selector":".login-tabs-tab:nth-child(2)"}' > /dev/null
sleep 2
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"var el=document.getElementById(\"fm-sms-login-id\"); el.focus(); el.value=\"13528475138\"; el.dispatchEvent(new Event(\"input\",{bubbles:true})); var cb=document.getElementById(\"fm-agreement-checkbox\"); if(cb&&!cb.checked){cb.click()}; \"done\""}' > /dev/null
sleep 1

echo "=== 5. Check slider state ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"var w=document.getElementById(\"nc_2_wrapper\"); var s=document.getElementById(\"nc_2_n1z\"); JSON.stringify({wrapperH:w?w.offsetHeight:0,sliderH:s?s.offsetHeight:0,sliderCls:s?s.className:\"none\",text:document.querySelector(\".scale_text\")?.textContent||\"none\"})"}'
echo

echo
echo "=== 6. CDP drag at slider position (731, 330) -> (1061, 330) ==="
# iframe at x=721,y=141, slider is at offset ~190 within iframe
# So slider is at y = 141+190 = 331, x starts at 721+10=731
curl -sS -m 30 -X POST $API/api/cdp/drag -H "Content-Type: application/json" \
    -d '{"x1":731,"y1":331,"x2":1061,"y2":331,"steps":25}'
echo
sleep 3

echo "=== 7. Check slider after CDP drag ==="
curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"var w=document.getElementById(\"nc_2_wrapper\"); var s=document.getElementById(\"nc_2_n1z\"); JSON.stringify({wrapperH:w?w.offsetHeight:0,sliderH:s?s.offsetHeight:0,sliderCls:s?s.className:\"none\",text:document.querySelector(\".scale_text\")?.textContent||\"none\"})"}'
echo

echo
echo "=== 8. If still not verified, try grid search with CDP drag ==="
for Y in 300 310 320 330 340 350 360 370 380; do
    echo -n "  CDP drag y=$Y: "
    # Reset slider
    curl -sS -m 5 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
        -d '{"iframe":"passport","script":"var r=document.querySelector(\".btn_refresh\"); if(r){r.click()}\"\""}' > /dev/null 2>&1
    sleep 1
    # CDP drag
    curl -sS -m 20 -X POST $API/api/cdp/drag -H "Content-Type: application/json" \
        -d "{\"x1\":731,\"y1\":$Y,\"x2\":1061,\"y2\":$Y,\"steps\":25}" > /dev/null 2>&1
    sleep 2
    # Check
    STATUS=$(curl -sS -m 5 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
        -d '{"iframe":"passport","script":"document.querySelector(\".scale_text\")?.textContent||\"none\""}' | python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get(\"result\",\"?\"))" 2>/dev/null)
    echo "$STATUS"
    if [ "$STATUS" = "Verified" ]; then
        echo "  ** CDP DRAG VERIFIED at y=$Y!"
        echo "=== 9. Click Get code ==="
        curl -sS -m 15 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
            -d '{"iframe":"passport","script":"document.querySelector(\".send-btn-link\")?.click();\"clicked\""}'
        echo
        sleep 5
        BTN=$(curl -sS -m 5 -X POST $API/api/iframe/eval -H "Content-Type: application/json" \
            -d '{"iframe":"passport","script":"document.querySelector(\".send-btn-link\")?.textContent||\"none\""}' | python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get(\"result\",\"?\"))" 2>/dev/null)
        echo "  button text: $BTN"
        echo "  ** CHECK YOUR PHONE FOR SMS!"
        break
    fi
done
