package notify

import (
	"testing"
	"time"
)

type fake struct {
	shown   []Message
	cleared []string
}

func (f *fake) show(m Message) error { f.shown = append(f.shown, m); return nil }
func (f *fake) clear(key string)     { f.cleared = append(f.cleared, key) }
func (f *fake) close()               {}

func TestSendRepeats(t *testing.T) {
	f := &fake{}
	n := newNotifier(f)
	now := time.Unix(1000, 0)
	n.now = func() time.Time { return now }

	held := Message{Key: "held:R", Title: "Deletes held", Body: "63 files", Critical: true}
	if ok, _ := n.Send(held); !ok {
		t.Fatal("first message not shown")
	}
	now = now.Add(10 * time.Minute)
	if ok, _ := n.Send(held); ok {
		t.Error("the same message was shown again within the hour")
	}
	// A changed message under the same key replaces the old one at once.
	if ok, _ := n.Send(Message{Key: "held:R", Title: "Deletes held", Body: "70 files"}); !ok {
		t.Error("a changed message was not shown")
	}
	// Other keys are independent.
	if ok, _ := n.Send(Message{Key: "conflict:R", Title: "Conflict", Body: "a.txt"}); !ok {
		t.Error("a message under another key was not shown")
	}
	now = now.Add(Repeat)
	if ok, _ := n.Send(Message{Key: "held:R", Title: "Deletes held", Body: "70 files"}); !ok {
		t.Error("the same message was not shown again after an hour")
	}
	// Clear takes the notification down and lets it be shown again.
	n.Clear("held:R")
	n.Clear("never-shown")
	if len(f.cleared) != 1 || f.cleared[0] != "held:R" {
		t.Errorf("cleared %q, want only held:R", f.cleared)
	}
	if ok, _ := n.Send(Message{Key: "held:R", Title: "Deletes held", Body: "70 files"}); !ok {
		t.Error("a cleared message was not shown again")
	}
	if len(f.shown) != 5 {
		t.Errorf("shown %d messages, want 5", len(f.shown))
	}
}

func TestNilNotifier(t *testing.T) {
	var n *Notifier
	if ok, err := n.Send(Message{Key: "k", Title: "t"}); ok || err != nil {
		t.Errorf("nil Send = %v, %v", ok, err)
	}
	n.Clear("k")
	n.Close()
}
