package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USER1043/cloudsync/internal/notify"
)

func TestTrayState(t *testing.T) {
	for _, tc := range []struct {
		state   string
		running bool
		want    notify.State
	}{
		{"in sync", true, notify.StateOK},
		{"uploading", true, notify.StateBusy},
		{"catching up with Drive", true, notify.StateBusy},
		{"starting", true, notify.StateBusy},
		{"stopping", true, notify.StateBusy},
		{"offline", true, notify.StateAttention},
		{"slowed down by Google", true, notify.StateAttention},
		{"paused: Drive is full", true, notify.StateAttention},
		{"retrying files that failed", true, notify.StateAttention},
		{"failing", true, notify.StateError},
		{statePaused, true, notify.StatePaused},
		{"in sync", false, notify.StateStopped}, // a stale file from a sync that has exited
		{"stopped", false, notify.StateStopped},
		{"stopped: needs you", false, notify.StateError},
		{"", false, notify.StateStopped},
	} {
		if got := trayState(runStatus{State: tc.state}, tc.running); got != tc.want {
			t.Errorf("trayState(%q, running=%v) = %d, want %d", tc.state, tc.running, got, tc.want)
		}
	}
}

func TestStatusLine(t *testing.T) {
	for _, tc := range []struct {
		st      runStatus
		running bool
		want    string
	}{
		{runStatus{State: "in sync"}, true, "CloudSync: in sync"},
		{runStatus{State: "uploading", Detail: "3 changes"}, true, "CloudSync: uploading (3 changes)"},
		{runStatus{}, true, "CloudSync: starting"},
		{runStatus{State: "stopped"}, false, "CloudSync: not running"},
		{runStatus{State: "in sync", Detail: "old"}, false, "CloudSync: not running"},
		{runStatus{State: "stopped: needs you", Detail: "sign in again"}, false, "CloudSync: stopped, needs you (sign in again)"},
	} {
		if got := statusLine(tc.st, tc.running); got != tc.want {
			t.Errorf("statusLine(%+v, %v) = %q, want %q", tc.st, tc.running, got, tc.want)
		}
	}
	long := statusLine(runStatus{State: "failing", Detail: strings.Repeat("word ", 40)}, true)
	if n := len([]rune(long)); n > 127 {
		t.Errorf("status line is %d characters; a tooltip holds 127", n)
	}
}

func TestWarningDiff(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	a := warning{Key: "offline", At: t0, Title: "Offline"}
	b := warning{Key: "held:Docs", At: t0.Add(time.Minute), Title: "Held"}
	shown := map[string]time.Time{}

	// Newest first in the status file; shown oldest first.
	fresh, gone := warningDiff(shown, []warning{b, a})
	if len(fresh) != 2 || fresh[0].Key != "offline" || fresh[1].Key != "held:Docs" || len(gone) != 0 {
		t.Fatalf("first look: fresh %v, gone %v", fresh, gone)
	}
	// Nothing changed.
	if fresh, gone := warningDiff(shown, []warning{b, a}); len(fresh) != 0 || len(gone) != 0 {
		t.Errorf("unchanged: fresh %v, gone %v", fresh, gone)
	}
	// One is raised again (new time) and the other is resolved.
	a2 := a
	a2.At = t0.Add(time.Hour)
	fresh, gone = warningDiff(shown, []warning{a2})
	if len(fresh) != 1 || fresh[0].Key != "offline" || !slices.Equal(gone, []string{"held:Docs"}) {
		t.Errorf("changed: fresh %v, gone %v", fresh, gone)
	}
	if _, ok := shown["held:Docs"]; ok {
		t.Error("a resolved warning is still remembered, so it could never show again")
	}
	// All resolved.
	if fresh, gone := warningDiff(shown, nil); len(fresh) != 0 || !slices.Equal(gone, []string{"offline"}) {
		t.Errorf("cleared: fresh %v, gone %v", fresh, gone)
	}
}

func TestWarningMessageKeepsCritical(t *testing.T) {
	k := newStatusKeeper(t.TempDir())
	k.warn(notify.Message{Key: "k", Title: "T", Body: "B", Critical: true})
	st := readStatus(filepath.Dir(k.path))
	if len(st.Warnings) != 1 {
		t.Fatalf("warnings = %+v", st.Warnings)
	}
	if m := st.Warnings[0].message(); !m.Critical || m.Key != "k" || m.Title != "T" || m.Body != "B" {
		t.Errorf("message = %+v", m)
	}
}

func TestTrayLock(t *testing.T) {
	dir := t.TempDir()
	if trayIsRunning(dir) {
		t.Fatal("tray running with no lock held")
	}
	unlock, ok, err := lockTray(dir)
	if err != nil || !ok {
		t.Fatalf("first lock: ok=%v err=%v", ok, err)
	}
	if !trayIsRunning(dir) {
		t.Error("lock held, but the tray reads as not running")
	}
	if _, ok, _ := lockTray(dir); ok {
		t.Error("a second tray got the lock")
	}
	unlock()
	if trayIsRunning(dir) {
		t.Error("tray still reads as running after unlock")
	}
}

func TestControlNeverBlocks(t *testing.T) {
	dm := &daemon{ctl: make(chan ctlOp, 2)}
	for range 10 {
		dm.control(ctlSyncNow)
	}
	if len(dm.ctl) != 2 {
		t.Errorf("queued %d, want 2", len(dm.ctl))
	}
}
