//go:build linux

// Package tray — Linux implementation backed by the freedesktop
// StatusNotifierItem (SNI) D-Bus protocol (org.kde.StatusNotifierItem)
// with a com.canonical.dbusmenu menu. Pure Go (no CGO), so it
// cross-compiles with CGO_ENABLED=0.
//
// Desktop support: Ubuntu/GNOME (AppIndicator extension), KDE Plasma,
// XFCE, MATE, Cinnamon and any other shell hosting a
// org.kde.StatusNotifierWatcher. GNOME shells WITHOUT the AppIndicator
// extension cannot show SNI items — that is a shell limitation, the
// item is still registered correctly.
package tray

import (
        "errors"
        "fmt"
        "log"
        "os"
        "os/user"
        "path/filepath"
        "strconv"
        "sync"

        "github.com/godbus/dbus/v5"
        "github.com/godbus/dbus/v5/introspect"

        "github.com/samaidev/samvnc/internal/trayicon"
)

const (
        sniIface    = "org.kde.StatusNotifierItem"
        sniPath     = "/org/samvnc/StatusNotifierItem"
        menuIface   = "com.canonical.dbusmenu"
        watcherName = "org.kde.StatusNotifierWatcher"
        watcherPath = "/StatusNotifierWatcher"
)

// linuxTray implements Tray over a D-Bus session connection.
type linuxTray struct {
        mu sync.Mutex

        conn    *dbus.Conn
        ownName string // unique connection name, e.g. ":1.42"

        menu    []MenuItem // current logical menu
        items   []menuRow  // flattened dbusmenu rows (ids, props)
        rev     uint32     // dbusmenu layout revision
        tooltip string
        cb      func(itemID string)

        // closed by Stop to unblock Run
        done chan struct{}
}

// menuRow is one exported dbusmenu node.
type menuRow struct {
        id        int32
        parentID  int32
        label     string
        enabled   bool
        visible   bool
        separator bool
        checked   bool // toggle-type checkmark + state on
        goID      string
}

func newTray() Tray {
        return &linuxTray{
                done: make(chan struct{}),
                rev:  1,
        }
}

// ---------------------------------------------------------------- Tray API

func (t *linuxTray) Run() error {
        conn, err := sessionBus()
        if err != nil {
                return fmt.Errorf("tray: session bus: %w", err)
        }
        t.conn = conn
        t.ownName, _ = conn.Names()[0], conn.Names()

        // Export StatusNotifierItem object (methods).
        if err := conn.Export(&sniMethods{t}, sniPath, sniIface); err != nil {
                conn.Close()
                return fmt.Errorf("tray: export item: %w", err)
        }
        // Export DBusMenu object (methods).
        if err := conn.Export(&menuMethods{t}, sniPath, menuIface); err != nil {
                conn.Close()
                return fmt.Errorf("tray: export menu: %w", err)
        }
        // Export dynamic properties for both interfaces.
        if err := conn.Export(propsMethods{t}, sniPath, "org.freedesktop.DBus.Properties"); err != nil {
                conn.Close()
                return fmt.Errorf("tray: export props: %w", err)
        }
        // Introspectable on the combined node.
        node := introspect.Node{
                Name: sniPath,
                Interfaces: []introspect.Interface{
                        introspect.Interface{Name: sniIface},
                        introspect.Interface{Name: menuIface},
                },
        }
        _ = conn.Export(introspect.NewIntrospectable(&node), sniPath,
                "org.freedesktop.DBus.Introspectable")

        // Listen for menu activation events from the host (shell).
        if err := conn.AddMatchSignal(
                dbus.WithMatchObjectPath(dbus.ObjectPath(sniPath)),
                dbus.WithMatchInterface(menuIface),
                dbus.WithMatchMember("Event"),
        ); err != nil {
                log.Printf("tray: AddMatchSignal: %v", err)
        }
        sigCh := make(chan *dbus.Signal, 16)
        conn.Signal(sigCh)

        // Rebuild the flattened menu from any SetMenu calls made before Run.
        t.rebuildLocked()

        // Register with the shell's StatusNotifierWatcher.
        watcher := conn.Object(watcherName, dbus.ObjectPath(watcherPath))
        if err := watcher.Call(watcherName+".RegisterStatusNotifierItem", 0,
                string(sniPath)).Err; err != nil {
                conn.Close()
                return fmt.Errorf("tray: no StatusNotifierWatcher on session bus "+
                        "(desktop shell without tray support?): %w", err)
        }

        log.Printf("tray: SNI registered %s%s on %s", t.ownName, sniPath, sessionBusAddr())

        for {
                select {
                case <-t.done:
                        _ = watcher.Call(watcherName+".UnregisterStatusNotifierItem", 0,
                                string(sniPath)).Err
                        conn.Close()
                        return nil
                case sig, ok := <-sigCh:
                        if !ok {
                                select {
                                case <-t.done:
                                        return nil
                                default:
                                        return fmt.Errorf("tray: dbus connection closed")
                                }
                        }
                        t.handleSignal(sig)
                }
        }
}

func (t *linuxTray) SetMenu(items []MenuItem) {
        t.mu.Lock()
        t.menu = items
        if t.conn != nil {
                t.rebuildLocked()
        }
        t.mu.Unlock()
}

func (t *linuxTray) SetTooltip(text string) {
        t.mu.Lock()
        t.tooltip = text
        t.mu.Unlock()
        if t.conn != nil {
                _ = t.conn.Emit(dbus.ObjectPath(sniPath), sniIface+".NewToolTip")
        }
}

func (t *linuxTray) SetCallback(cb func(itemID string)) {
        t.mu.Lock()
        t.cb = cb
        t.mu.Unlock()
}

func (t *linuxTray) Stop() {
        select {
        case <-t.done:
                return
        default:
        }
        close(t.done)
}

// ------------------------------------------------------------- internals

// handleSignal dispatches dbusmenu Event signals to the Go callback.
func (t *linuxTray) handleSignal(sig *dbus.Signal) {
        if sig.Name != menuIface+".Event" || len(sig.Body) < 2 {
                return
        }
        id, _ := sig.Body[0].(int32)
        event, _ := sig.Body[1].(string)
        if event != "clicked" {
                return
        }
        t.mu.Lock()
        var goID string
        for _, r := range t.items {
                if r.id == id {
                        goID = r.goID
                        break
                }
        }
        cb := t.cb
        t.mu.Unlock()
        if goID != "" && cb != nil {
                cb(goID)
        }
}

// rebuildLocked flattens t.menu into t.items and bumps the layout revision.
// Caller must hold t.mu.
func (t *linuxTray) rebuildLocked() {
        t.items = t.items[:0]
        next := int32(1)
        for _, m := range t.menu {
                row := menuRow{
                        id:        next,
                        parentID:  0,
                        label:     m.Label,
                        enabled:   !m.Disabled,
                        visible:   true,
                        separator: m.Separator,
                        checked:   m.Checked,
                        goID:      m.ID,
                }
                next++
                t.items = append(t.items, row)
        }
        t.rev++
        if t.conn != nil {
                // Notify hosts the layout changed.
                _ = t.conn.Emit(dbus.ObjectPath(sniPath), menuIface+".LayoutUpdated",
                        t.rev, int32(0))
        }
}

// pixmapARGB renders the SamVNC tray icons as SNI IconPixmap data
// (ARGB32, big-endian, row-major).
//
// Since the cross-platform icon unification, the pixmaps come from the
// embedded artwork in internal/trayicon (the same blue rounded square +
// white "V" as the Windows tray icon) instead of a programmatic drawing.
// Multiple sizes are offered and the shell picks the closest match.
func pixmapsARGB() []IconPixmap {
        out := make([]IconPixmap, 0, len(trayicon.Sizes))
        for _, size := range trayicon.Sizes {
                img, err := trayicon.ScaleTo(size)
                if err != nil {
                        log.Printf("tray: icon scale %d: %v", size, err)
                        continue
                }
                out = append(out, IconPixmap{
                        Width:  int32(size),
                        Height: int32(size),
                        Data:   trayicon.ARGB(img),
                })
        }
        if len(out) == 0 {
                // Should never happen (embedded assets always decode); keep one
                // blank 24px entry so the SNI signature stays valid.
                blank := make([]byte, 24*24*4)
                out = append(out, IconPixmap{Width: 24, Height: 24, Data: blank})
        }
        return out
}

// -------------------------------------------------------- dbus plumbing

// sessionBus connects to the user session bus, falling back to the
// standard /run/user/<uid>/bus socket when the env var is missing
// (e.g. when launched from a systemd unit or a headless SSH shell).
func sessionBus() (*dbus.Conn, error) {
        if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
                return dbus.SessionBus()
        }
        uid := os.Getuid()
        if u, err := user.Current(); err == nil {
                if n, err2 := strconv.Atoi(u.Uid); err2 == nil {
                        uid = n
                }
        }
        addr := fmt.Sprintf("unix:path=/run/user/%d/bus", uid)
        return dbus.Dial(addr)
}

func sessionBusAddr() string {
        if a := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" {
                return a
        }
        return fmt.Sprintf("unix:path=/run/user/%d/bus", os.Getuid())
}

// xdgDataDir is where a desktop launcher (.desktop) could live; kept for
// future use — SNI does not require it.
func xdgDataDir() string {
        if d := os.Getenv("XDG_DATA_HOME"); d != "" {
                return d
        }
        home, err := os.UserHomeDir()
        if err != nil {
                return filepath.Join("/tmp", ".local", "share")
        }
        return filepath.Join(home, ".local", "share")
}

// sniMethods implements org.kde.StatusNotifierItem.
type sniMethods struct{ t *linuxTray }

func (m *sniMethods) Activate(x, y int32)             {}
func (m *sniMethods) SecondaryActivate(x, y int32)    {}
func (m *sniMethods) Scroll(delta int32, orient string) {}

// propsMethods implements org.freedesktop.DBus.Properties for both the
// StatusNotifierItem and the DBusMenu interfaces on sniPath.
type propsMethods struct{ t *linuxTray }

func (m propsMethods) Get(iface, name string) (dbus.Variant, *dbus.Error) {
        v, ok := m.t.lookupProp(iface, name)
        if !ok {
                return dbus.MakeVariant(nil), dbus.MakeFailedError(errors.New(
                        fmt.Sprintf("unknown property %s.%s", iface, name)))
        }
        return dbus.MakeVariant(v), nil
}

func (m propsMethods) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
        out := map[string]dbus.Variant{}
        for _, name := range m.t.propNames(iface) {
                if v, ok := m.t.lookupProp(iface, name); ok {
                        out[name] = dbus.MakeVariant(v)
                }
        }
        return out, nil
}

func (m propsMethods) Set(iface, name string, v dbus.Variant) *dbus.Error {
        return dbus.MakeFailedError(errors.New("read-only property"))
}

// propNames lists supported properties per interface.
func (t *linuxTray) propNames(iface string) []string {
        switch iface {
        case sniIface:
                return []string{"Id", "Category", "Title", "Status", "WindowId",
                        "IconName", "IconPixmap", "OverlayIconPixmap", "AttentionIconName",
                        "AttentionIconPixmap", "ToolTip", "Menu", "ItemIsMenu"}
        case menuIface:
                return []string{"Version", "TextDirection", "Status", "IconThemePath"}
        }
        return nil
}

// lookupProp returns the current value of a property (dynamic reads).
func (t *linuxTray) lookupProp(iface, name string) (interface{}, bool) {
        t.mu.Lock()
        defer t.mu.Unlock()
        switch iface {
        case sniIface:
                switch name {
                case "Id":
                        return "SamVNC", true
                case "Category":
                        return "ApplicationStatus", true
                case "Title":
                        return "SamVNC", true
                case "Status":
                        return "Active", true
                case "WindowId":
                        return int32(0), true
                case "IconName":
                        return "", true
                case "IconPixmap":
                        return pixmapsARGB(), true
                case "OverlayIconPixmap", "AttentionIconName", "AttentionIconPixmap":
                        if name == "AttentionIconName" {
                                return "", true
                        }
                        return []IconPixmap{}, true
                case "ToolTip":
                        return ToolTip{
                                IconName: "", IconPixmap: pixmapsARGB(),
                                Title: "SamVNC", Description: t.tooltip,
                        }, true
                case "Menu":
                        return dbus.ObjectPath(sniPath), true
                case "ItemIsMenu":
                        return true, true
                }
        case menuIface:
                switch name {
                case "Version":
                        return uint32(3), true
                case "TextDirection":
                        return "ltr", true
                case "Status":
                        return "normal", true
                case "IconThemePath":
                        return "", true
                }
        }
        return nil, false
}

// D-Bus marshalling helper types (signatures checked by the spec):
//   IconPixmap: (iiay)
//   ToolTip:    sa(iiay)ss

type IconPixmap struct {
        Width  int32
        Height int32
        Data   []byte
}

type ToolTip struct {
        IconName    string
        IconPixmap  []IconPixmap
        Title       string
        Description string
}

// menuMethods implements com.canonical.dbusmenu.
type menuMethods struct{ t *linuxTray }

// MenuLayoutItem is one node of the GetLayout response tree.
//
// Children is []interface{} on purpose: godbus derives the STATIC
// signature of an interface slice as "av" (variants) instead of trying
// to recurse into the element type — a []MenuLayoutItem would make the
// signature derivation loop forever ("container nesting too deep"
// panic). At encode time godbus walks the actual VALUE tree, which is
// finite (plain menus have no submenus). Same trick as DeedleFake/tray.
type MenuLayoutItem struct {
        ID       int32
        Props    map[string]dbus.Variant
        Children []interface{}
}

func (m *menuMethods) GetLayout(parentID int32, depth int32,
        props []string) (uint32, MenuLayoutItem, *dbus.Error) {

        m.t.mu.Lock()
        defer m.t.mu.Unlock()

        root := MenuLayoutItem{ID: 0, Props: map[string]dbus.Variant{
                "children-display": dbus.MakeVariant("submenu"),
        }}
        for i := range m.t.items {
                root.Children = append(root.Children, m.rowToLayout(&m.t.items[i]))
        }
        return m.t.rev, root, nil
}

func (m *menuMethods) rowToLayout(r *menuRow) MenuLayoutItem {
        return MenuLayoutItem{
                ID:       r.id,
                Props:    m.rowProps(r),
                Children: []interface{}{},
        }
}

func (m *menuMethods) rowProps(r *menuRow) map[string]dbus.Variant {
        p := map[string]dbus.Variant{
                "visible": dbus.MakeVariant(r.visible),
        }
        if r.separator {
                p["type"] = dbus.MakeVariant("separator")
                return p
        }
        p["type"] = dbus.MakeVariant("standard")
        p["label"] = dbus.MakeVariant(r.label)
        p["enabled"] = dbus.MakeVariant(r.enabled)
        if r.checked {
                p["toggle-type"] = dbus.MakeVariant("checkmark")
                p["toggle-state"] = dbus.MakeVariant(int32(1))
        } else {
                p["toggle-type"] = dbus.MakeVariant("")
                p["toggle-state"] = dbus.MakeVariant(int32(0))
        }
        return p
}

// MenuGroupProp is one row of the GetGroupProperties response.
type MenuGroupProp struct {
        ID    int32
        Props map[string]dbus.Variant
}

func (m *menuMethods) GetGroupProperties(ids []int32, props []string,
) ([]MenuGroupProp, *dbus.Error) {

        m.t.mu.Lock()
        defer m.t.mu.Unlock()
        want := map[int32]bool{}
        for _, id := range ids {
                want[id] = true
        }
        var out []MenuGroupProp
        for i := range m.t.items {
                r := &m.t.items[i]
                if len(ids) > 0 && !want[r.id] {
                        continue
                }
                out = append(out, MenuGroupProp{ID: r.id, Props: m.rowProps(r)})
        }
        return out, nil
}

func (m *menuMethods) GetProperty(id int32, name string,
) (dbus.Variant, *dbus.Error) {
        m.t.mu.Lock()
        defer m.t.mu.Unlock()
        for i := range m.t.items {
                r := &m.t.items[i]
                if r.id != id {
                        continue
                }
                p := m.rowProps(r)
                if v, ok := p[name]; ok {
                        return v, nil
                }
        }
        return dbus.MakeVariant(nil), dbus.MakeFailedError(
                errors.New("no such item"))
}

func (m *menuMethods) AboutToShow(parentID int32) (bool, *dbus.Error) {
        return false, nil
}

func (m *menuMethods) AboutToDisplay(ids []int32) *dbus.Error  { return nil }
func (m *menuMethods) Display(id int32) *dbus.Error            { return nil }

type dbusEvent struct {
        ID        int32
        EventID   string
        Data      dbus.Variant
        Timestamp uint32
}

func (m *menuMethods) Event(id int32, eventID string, data dbus.Variant,
        timestamp uint32) *dbus.Error {

        if eventID == "clicked" {
                // Same dispatch path as the signal listener; both arrive in
                // practice depending on the shell implementation.
                m.t.handleSignal(&dbus.Signal{
                        Name: menuIface + ".Event",
                        Body: []interface{}{id, eventID, data, timestamp},
                })
        }
        return nil
}

func (m *menuMethods) EventGroup(events []dbusEvent) *dbus.Error {
        for _, e := range events {
                _ = m.Event(e.ID, e.EventID, e.Data, e.Timestamp)
        }
        return nil
}
