package notify

import (
	"fmt"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	service = "org.freedesktop.Notifications"
	objPath = "/org/freedesktop/Notifications"
	iface   = "org.freedesktop.Notifications"
	icon    = "folder-sync" // from the icon theme (Breeze has it)
)

// freedesktop sends notifications over the session bus.
type freedesktop struct {
	conn *dbus.Conn

	mu  sync.Mutex
	ids map[string]uint32 // notification ID per key, so a new one replaces it
}

func newBackend() (backend, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connecting to the session bus: %w", err)
	}
	return &freedesktop{conn: conn, ids: map[string]uint32{}}, nil
}

// markup escapes text for the body, which notification servers may read
// as simple HTML: a path with "&" or "<" would otherwise break it.
var markup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func (f *freedesktop) show(m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	urgency, expire := byte(1), int32(-1) // normal; the server's own timeout
	if m.Critical {
		urgency, expire = 2, 0 // critical; until dismissed
	}
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(urgency)}
	var id uint32
	err := f.conn.Object(service, objPath).Call(iface+".Notify", 0,
		"CloudSync", f.ids[m.Key], icon, m.Title, markup.Replace(m.Body), []string{}, hints, expire).Store(&id)
	if err != nil {
		return fmt.Errorf("showing a notification: %w", err)
	}
	f.ids[m.Key] = id
	return nil
}

func (f *freedesktop) clear(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.ids[key]; ok {
		f.conn.Object(service, objPath).Call(iface+".CloseNotification", 0, id)
		delete(f.ids, key)
	}
}

func (f *freedesktop) close() { f.conn.Close() }
