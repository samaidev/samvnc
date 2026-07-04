#!/bin/bash
# Analyze slider captcha: find slider element, track, dimensions
set +e
API="http://127.0.0.1:8115"

echo "=== 1. Find slider elements ==="
curl -sS -m 15 -X POST $API/api/iframe/eval \
    -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"var all=document.querySelectorAll(\"*\"); var r=[]; for(var i=0;i<all.length;i++){var cls=all[i].className||\"\"; var id=all[i].id||\"\"; if(typeof cls==\"string\"&&(cls.includes(\"slide\")||cls.includes(\"nc_\")||cls.includes(\"captcha\")||cls.includes(\"slider\")||id.includes(\"nc_\"))){var rect=all[i].getBoundingClientRect(); r.push({tag:all[i].tagName,cls:cls.substring(0,50),id:id,rect:{x:rect.x,y:rect.y,w:rect.width,h:rect.height}})}} JSON.stringify(r.slice(0,20))"}'
echo

echo
echo "=== 2. Get slider track and button details ==="
curl -sS -m 15 -X POST $API/api/iframe/eval \
    -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"var track=document.querySelector(\".nc_iconfont.btn_slide\")||document.querySelector(\"[class*=btn_slide]\"); var bar=document.querySelector(\".scale_text\")||document.querySelector(\"[class*=scale_text]\"); var container=document.querySelector(\"[class*=nc-container]\")||document.querySelector(\"[class*=captcha]\"); JSON.stringify({slider:track?{rect:track.getBoundingClientRect().toJSON(),cls:track.className}:{},bar:bar?{rect:bar.getBoundingClientRect().toJSON(),text:bar.textContent}:{},container:container?{rect:container.getBoundingClientRect().toJSON(),cls:container.className}:{}})"}'
echo

echo
echo "=== 3. Screenshot ==="
curl -sS -m 10 -o /tmp/slider_check.png $API/api/screenshot 2>&1
ls -la /tmp/slider_check.png 2>&1

echo
echo "=== 4. Check slider status text ==="
curl -sS -m 15 -X POST $API/api/iframe/eval \
    -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"var el=document.querySelector(\".scale_text\")||document.querySelector(\".nc-lang-cnt\"); el?el.textContent:\"no slider text\""}'
echo

echo
echo "=== 5. Check if slider needs to be dragged ==="
curl -sS -m 15 -X POST $API/api/iframe/eval \
    -H "Content-Type: application/json" \
    -d '{"iframe":"passport","script":"var slider=document.querySelector(\".nc_iconfont.btn_slide\"); if(slider){var r=slider.getBoundingClientRect(); JSON.stringify({x:r.x,y:r.y,w:r.width,h:r.height,style:slider.style.cssText.substring(0,100)})} else {\"no slider found\"}"}'
echo
