// Package notify shows desktop notifications for the few things the owner
// should hear about, such as deletes held for review or a sign-in that
// expired.
//
// Linux: the freedesktop.org notification service on the session bus,
// which KDE Plasma (and GNOME) show like any app's notifications.
// Windows: a CloudSync icon in the notification area, whose balloon
// messages Windows 10 and 11 show as notifications. The icon also has a
// menu (Controls) and a colour and tooltip that follow the sync (SetState).
//
// A Notifier never repeats itself: the same message under the same key is
// shown at most once an hour, and a new message replaces the last one with
// its key instead of stacking up.
package notify

import (
	"sync"
	"time"
)

// Repeat is how long the same message stays quiet after being shown.
const Repeat = time.Hour

// Message is one notification.
type Message struct {
	// Key groups messages about the same thing, such as "held:MyFiles". A
	// message replaces the one shown last under its key.
	Key   string
	Title string
	Body  string

	// Critical marks something only the owner can sort out: it stays on
	// screen until dismissed, where the system allows that.
	Critical bool
}

// Notifier shows Messages. A nil *Notifier shows nothing, so callers can
// carry on when notifications aren't available.
type Notifier struct {
	b   backend
	now func() time.Time

	mu   sync.Mutex
	last map[string]shown // by Key
}

type shown struct {
	body string
	at   time.Time
}

type backend interface {
	show(m Message) error
	clear(key string)
	close()
}

// New connects to the system's notifications.
func New() (*Notifier, error) {
	b, err := newBackend()
	if err != nil {
		return nil, err
	}
	return newNotifier(b), nil
}

func newNotifier(b backend) *Notifier {
	return &Notifier{b: b, now: time.Now, last: map[string]shown{}}
}

// Send shows m unless the same message under its key was shown less than
// Repeat ago. It reports whether m was shown.
func (n *Notifier) Send(m Message) (bool, error) {
	if n == nil {
		return false, nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if s, ok := n.last[m.Key]; ok && s.body == m.Title+"\n"+m.Body && n.now().Sub(s.at) < Repeat {
		return false, nil
	}
	if err := n.b.show(m); err != nil {
		return false, err
	}
	n.last[m.Key] = shown{body: m.Title + "\n" + m.Body, at: n.now()}
	return true, nil
}

// Clear takes down the notification shown under key, if the system allows
// that, for instance once the problem it described is gone, and lets the
// same message be shown again.
func (n *Notifier) Clear(key string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.last[key]; ok {
		delete(n.last, key)
		n.b.clear(key)
	}
}

// controller is a backend that has a tray icon with a menu (Windows).
type controller interface {
	setControls(Controls)
	setState(st State, text string)
}

// SetControls gives the tray icon its menu (Windows; elsewhere, and for a
// nil Notifier, it does nothing).
func (n *Notifier) SetControls(c Controls) {
	if n == nil {
		return
	}
	if cb, ok := n.b.(controller); ok {
		cb.setControls(c)
	}
}

// SetState sets the tray icon's colour and tooltip.
func (n *Notifier) SetState(st State, text string) {
	if n == nil {
		return
	}
	if cb, ok := n.b.(controller); ok {
		cb.setState(st, text)
	}
}

// Close disconnects; on Windows it also removes the icon.
func (n *Notifier) Close() {
	if n != nil {
		n.b.close()
	}
}
