#!/bin/bash
# Strategy: Chrome with CDP port available, but SamVNC connects CDP only briefly.
# 1. Start Chrome with CDP
# 2. Start SamVNC WITH cdp-addr
# 3. Use CDP click to switch SMS, fill phone, check agreement
# 4. Kill SamVNC (disconnects CDP)
# 5. Wait 10s
# 6. Restart SamVNC WITHOUT cdp-addr (no CDP connection)
# 7. Now slider should render (no active CDP connection)
# 8. Use VNC (manual or X11) to drag slider
set +e

echo "=== 1. Kill old ==="
ssh -o ConnectTimeout=10 login07 '
screen -S desktop -X quit 2>/dev/null
screen -S samvnc -X quit 2>/dev/null
for p in $(ps -u $USER -o pid,cmd | grep -iE "Xvfb|google-chrome|samvnc-server|/.local/bin/aitun" | grep -v grep | awk "{print \$1}"); do kill -9 $p 2>/dev/null; done
sleep 3
rm -f ~/singularity/rootfs/root/chrome_profile/SingletonLock* 2>/dev/null
rm -f ~/singularity/rootfs/tmp/.X*-lock ~/singularity/rootfs/tmp/.X11-unix/X* 2>/dev/null
echo cleaned
'

echo
echo "=== 2. Start Chrome WITH CDP + SamVNC with CDP ==="
ssh -o ConnectTimeout=10 login07 '
cat > ~/singularity/rootfs/root/start_with_cdp.sh << "INNEREOF"
#!/bin/bash
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export HOME=/root
export DISPLAY=:101
Xvfb :101 -screen 0 1280x720x24 -ac +extension RANDR -auth /root/.Xauthority > /root/xvfb.log 2>&1 &
sleep 3
xauth add :101 . $(mcookie) 2>/dev/null
google-chrome --no-sandbox --disable-gpu --disable-dev-shm-usage --no-first-run --no-default-browser-check --disable-features=TranslateUI --window-size=1280,720 --start-maximized --user-data-dir=/root/chrome_profile --remote-debugging-port=9222 --remote-debugging-address=127.0.0.1 --disable-blink-features=AutomationControlled https://modelscope.cn/login > /root/chrome.log 2>&1 &
sleep 5
exec /root/samvnc-server -listen=127.0.0.1:8122 -tunnel=false -cdp-addr=127.0.0.1:9222 -verbose
INNEREOF
chmod +x ~/singularity/rootfs/root/start_with_cdp.sh
screen -dmS desktop bash -c "exec ~/singularity/proot-5.1.0/src/proot -r ~/singularity/rootfs -b /dev -b /proc -b /sys -b /tmp /bin/bash /root/start_with_cdp.sh"
sleep 15
echo started
'

echo
echo "=== 3. CDP click SMS tab ==="
ssh -o ConnectTimeout=10 login07 'curl -sS -m 10 -X POST http://127.0.0.1:8122/api/cdp/click -H "Content-Type: application/json" -d "{\"x\":951,\"y\":160}"'
echo
sleep 2

echo "=== 4. CDP click phone input ==="
ssh -o ConnectTimeout=10 login07 'curl -sS -m 10 -X POST http://127.0.0.1:8122/api/cdp/click -H "Content-Type: application/json" -d "{\"x\":893,\"y\":201}"'
echo
sleep 1

echo "=== 5. Iframe eval set phone value ==="
ssh -o ConnectTimeout=10 login07 'curl -sS -m 15 -X POST http://127.0.0.1:8122/api/iframe/eval -H "Content-Type: application/json" -d "{\"iframe\":\"passport\",\"script\":\"var el=document.getElementById(\\\"fm-sms-login-id\\\"); el.focus(); el.value=\\\"13528475138\\\"; el.dispatchEvent(new Event(\\\"input\\\",{bubbles:true})); el.value\"}"'
echo

echo "=== 6. Iframe eval check checkbox ==="
ssh -o ConnectTimeout=10 login07 'curl -sS -m 15 -X POST http://127.0.0.1:8122/api/iframe/eval -H "Content-Type: application/json" -d "{\"iframe\":\"passport\",\"script\":\"var cb=document.getElementById(\\\"fm-agreement-checkbox\\\"); if(cb&&!cb.checked){cb.click()}; cb.checked\"}"'
echo

echo
echo "=== 7. Verify phone + checkbox ==="
ssh -o ConnectTimeout=10 login07 'curl -sS -m 15 -X POST http://127.0.0.1:8122/api/iframe/eval -H "Content-Type: application/json" -d "{\"iframe\":\"passport\",\"script\":\"JSON.stringify({phone:document.getElementById(\\\"fm-sms-login-id\\\")?.value,checked:document.getElementById(\\\"fm-agreement-checkbox\\\")?.checked,smsActive:document.querySelectorAll(\\\".login-tabs-tab\\\")[1]?.className.includes(\\\"active\\\")})\"}"'
echo

echo
echo "=== 8. NOW: Kill SamVNC (disconnect CDP) but keep Chrome running ==="
ssh -o ConnectTimeout=10 login07 '
for p in $(ps -u $USER -o pid,cmd | grep samvnc-server | grep -v grep | awk "{print \$1}"); do kill -9 $p 2>/dev/null; done
echo "SamVNC killed, CDP disconnected"
sleep 10
echo "waited 10s"

echo
echo "=== 9. Restart SamVNC WITHOUT CDP (slider should render now) ==="
screen -dmS samvnc bash -c "exec ~/singularity/proot-5.1.0/src/proot -r ~/singularity/rootfs -b /dev -b /proc -b /sys -b /tmp /bin/bash -c \"export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin; export HOME=/root; export DISPLAY=:101; exec /root/samvnc-server -listen=127.0.0.1:8122 -tunnel=false -quality high -fps 20 -verbose\""
sleep 10
echo "SamVNC restarted (no CDP)"

echo
echo "=== 10. Start aitun ==="
nohup setsid ~/.local/bin/aitun -p 8122 --host 127.0.0.1 --no-p2p > ~/aitun_8122.log 2>&1 &
disown
sleep 12
echo "=== 11. URL ==="
grep -oE "https://aitun\.cc/[A-Z0-9]+" ~/aitun_8122.log 2>&1 | head -1

echo
echo "=== 12. Screenshot (check if slider rendered) ==="
curl -sS -m 10 -o /tmp/slider_check.png http://127.0.0.1:8122/api/screenshot 2>&1
ls -la /tmp/slider_check.png 2>&1
'
