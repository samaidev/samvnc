package main

import (
        "encoding/json"
        "fmt"
        "image"
        "image/png"
        "log"
        "net/http"
        "os"
        "os/exec"
        "strings"
        "sync"
        "time"

        x "github.com/BurntSushi/xgb"
        "github.com/BurntSushi/xgb/xproto"
        "github.com/BurntSushi/xgb/xtest"
)

type DesktopController struct {
        conn *x.Conn
        root xproto.Window
        mu   sync.Mutex
}

type ClickReq struct{ X, Y int }
type TypeReq struct{ Text string }
type DragReq struct{ X1, Y1, X2, Y2, Steps int }

func main() {
        display := os.Getenv("DISPLAY")
        if display == "" { display = ":101" }
        dc, err := NewDC(display)
        if err != nil { log.Fatalf("X11: %v", err) }
        defer dc.Close()
        port := os.Getenv("PORT")
        if port == "" { port = "9090" }
        log.Printf("DesktopController on :%s (display=%s)", port, display)
        mux := http.NewServeMux()
        mux.HandleFunc("/health", dc.health)
        mux.HandleFunc("/screenshot", dc.shot)
        mux.HandleFunc("/click", dc.click)
        mux.HandleFunc("/type", dc.typ)
        mux.HandleFunc("/drag", dc.drag)
        mux.HandleFunc("/keydown", dc.kdown)
        mux.HandleFunc("/keyup", dc.kup)
        mux.HandleFunc("/", dc.health)
        log.Fatal(http.ListenAndServe(":"+port, mux))
}

func NewDC(d string) (*DesktopController, error) {
        c, err := x.NewConnDisplay(d)
        if err != nil { return nil, err }
        if err := xtest.Init(c); err != nil { c.Close(); return nil, err }
        r := xproto.Setup(c).DefaultScreen(c).Root
        log.Printf("X11 connected display=%s root=%d", d, r)
        return &DesktopController{conn: c, root: r}, nil
}
func (dc *DesktopController) Close() { dc.conn.Close() }

func (dc *DesktopController) move(x, y int) error {
        dc.mu.Lock(); defer dc.mu.Unlock()
        // Try XSendEvent first (sends directly to focused window, bypasses XTEST)
        // Then XTEST as fallback
        xtest.FakeInput(dc.conn, 6, 0, 0, dc.root, int16(x), int16(y), 0)
        // Also warp pointer
        xproto.WarpPointer(dc.conn, 0, dc.root, 0, 0, 0, 0, int16(x), int16(y))
        dc.conn.Sync()
        return nil
}
func (dc *DesktopController) btn(b int, press bool) error {
        dc.mu.Lock(); defer dc.mu.Unlock()
        var t byte = xproto.KeyPress; if !press { t = xproto.KeyRelease }
        xtest.FakeInput(dc.conn, t, byte(b), 0, dc.root, 0, 0, 0)
        dc.conn.Sync()
        return nil
}
func (dc *DesktopController) Click(x, y int) error {
        if err := dc.move(x, y); err != nil { return err }
        time.Sleep(50*time.Millisecond)
        if err := dc.btn(1, true); err != nil { return err }
        time.Sleep(50*time.Millisecond)
        return dc.btn(1, false)
}
func (dc *DesktopController) Type(s string) error {
        cmd := exec.Command("xdotool", "type", "--clearmodifiers", s)
        cmd.Env = append(os.Environ(), "DISPLAY="+os.Getenv("DISPLAY"))
        _, err := cmd.CombinedOutput()
        return err
}
func (dc *DesktopController) Drag(x1,y1,x2,y2,steps int) error {
        if steps<=0 { steps=25 }
        dc.move(x1,y1); time.Sleep(100*time.Millisecond)
        dc.btn(1,true); time.Sleep(100*time.Millisecond)
        for i:=1;i<=steps;i++ {
                x:=x1+(x2-x1)*i/steps; y:=y1+(y2-y1)*i/steps
                j:=(i%3)-1
                dc.move(x,y+j); time.Sleep(30*time.Millisecond)
        }
        return dc.btn(1,false)
}
func (dc *DesktopController) Shot() (*image.RGBA, error) {
        dc.mu.Lock(); defer dc.mu.Unlock()
        s:=xproto.Setup(dc.conn).DefaultScreen(dc.conn)
        w,h:=int(s.WidthInPixels),int(s.HeightInPixels)
        r,err:=xproto.GetImage(dc.conn,xproto.ImageFormatZPixmap,xproto.Drawable(dc.root),0,0,uint16(w),uint16(h),0xffffffff).Reply()
        if err!=nil { return nil,err }
        img:=image.NewRGBA(image.Rect(0,0,w,h))
        for i:=0;i<w*h;i++ { o:=i*4; img.Pix[i*4]=r.Data[o+2]; img.Pix[i*4+1]=r.Data[o+1]; img.Pix[i*4+2]=r.Data[o]; img.Pix[i*4+3]=255 }
        return img,nil
}
func (dc *DesktopController) health(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(map[string]bool{"ok":true}) }
func (dc *DesktopController) shot(w http.ResponseWriter, r *http.Request) {
        img,err:=dc.Shot(); if err!=nil { http.Error(w,err.Error(),500); return }
        w.Header().Set("Content-Type","image/png"); png.Encode(w,img)
}
func (dc *DesktopController) click(w http.ResponseWriter, r *http.Request) {
        var q ClickReq; json.NewDecoder(r.Body).Decode(&q)
        log.Printf("[click] %d,%d",q.X,q.Y)
        if err:=dc.Click(q.X,q.Y); err!=nil { http.Error(w,fmt.Sprintf(`{"error":"%s"}`,err),500); return }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok":true,"x":q.X,"y":q.Y})
}
func (dc *DesktopController) typ(w http.ResponseWriter, r *http.Request) {
        var q TypeReq; json.NewDecoder(r.Body).Decode(&q)
        log.Printf("[type] %s",q.Text)
        if err:=dc.Type(q.Text); err!=nil { http.Error(w,fmt.Sprintf(`{"error":"%s"}`,err),500); return }
        json.NewEncoder(w).Encode(map[string]interface{}{"ok":true})
}
func (dc *DesktopController) drag(w http.ResponseWriter, r *http.Request) {
        var q DragReq; json.NewDecoder(r.Body).Decode(&q)
        if q.Steps<=0 { q.Steps=25 }
        log.Printf("[drag] (%d,%d)->(%d,%d)",q.X1,q.Y1,q.X2,q.Y2)
        if err:=dc.Drag(q.X1,q.Y1,q.X2,q.Y2,q.Steps); err!=nil { http.Error(w,fmt.Sprintf(`{"error":"%s"}`,err),500); return }
        json.NewEncoder(w).Encode(map[string]bool{"ok":true})
}
func (dc *DesktopController) kdown(w http.ResponseWriter, r *http.Request) {
        var q struct{Key string}; json.NewDecoder(r.Body).Decode(&q)
        ks:=keysym(q.Key); if ks==0 { http.Error(w,`{"error":"unknown"}`,400); return }
        dc.keyev(ks,true); json.NewEncoder(w).Encode(map[string]interface{}{"ok":true,"key":q.Key})
}
func (dc *DesktopController) kup(w http.ResponseWriter, r *http.Request) {
        var q struct{Key string}; json.NewDecoder(r.Body).Decode(&q)
        ks:=keysym(q.Key); if ks==0 { http.Error(w,`{"error":"unknown"}`,400); return }
        dc.keyev(ks,false); json.NewEncoder(w).Encode(map[string]interface{}{"ok":true,"key":q.Key})
}
func (dc *DesktopController) keyev(ks xproto.Keysym, press bool) {
        // Convert keysym to keycode via min/max keycode range
        min := xproto.Setup(dc.conn).MinKeycode
        max := xproto.Setup(dc.conn).MaxKeycode
        count := int(max - min + 1)
        // Actually just use the current mapping
        km, err := xproto.GetKeyboardMapping(dc.conn, min, byte(count)).Reply()
        if err != nil || len(km.Keysyms) == 0 { return }
        per := int(km.KeysymsPerKeycode)
        if per == 0 { return }
        for i := 0; i < count; i++ {
                for j := 0; j < per; j++ {
                        idx := i*per + j
                        if idx < len(km.Keysyms) && km.Keysyms[idx] == ks {
                                kc := byte(int(min) + i)
                                var t byte = xproto.KeyPress; if !press { t = xproto.KeyRelease }
                                dc.mu.Lock(); xtest.FakeInput(dc.conn, t, kc, 0, dc.root, 0, 0, 0); dc.conn.Sync(); dc.mu.Unlock()
                                return
                        }
                }
        }
}
func keysym(n string) xproto.Keysym {
        n=strings.ToLower(n)
        switch n {
        case "return","enter": return 0xff0d
        case "tab": return 0xff09
        case "backspace": return 0xff08
        case "space": return 0x0020
        case "shift": return 0xffe1
        case "ctrl","control": return 0xffe3
        case "esc","escape": return 0xff1b
        }
        if len(n)==1 { return xproto.Keysym(n[0]) }
        return 0
}
