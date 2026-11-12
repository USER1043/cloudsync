package notify

import (
	"encoding/binary"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func fullControls() Controls {
	nop := func() {}
	return Controls{
		Snapshot: func() Snapshot { return Snapshot{} },
		SyncNow:  nop, Pause: nop, Resume: nop, StartSync: nop, StopSync: nop,
		OpenFolder: func(int) {}, OpenLog: nop, SetStartAtLogin: func(bool) {}, Exit: nop,
	}
}

func labels(m []menuItem) []string {
	var out []string
	for _, it := range m {
		if it.separator {
			out = append(out, "-")
		} else {
			out = append(out, it.label)
		}
	}
	return out
}

func find(m []menuItem, label string) (menuItem, bool) {
	for _, it := range m {
		if it.label == label {
			return it, true
		}
	}
	return menuItem{}, false
}

func TestBuildMenuRunning(t *testing.T) {
	s := Snapshot{Status: "CloudSync: in sync", Running: true, Folders: []string{"MyFiles"},
		StartAtLogin: true, CanChangeLogin: true, CanOpenLogFile: true}
	m := buildMenu(fullControls(), s)
	want := []string{"CloudSync: in sync", "-", "Sync now", "Pause syncing", "Stop sync", "-",
		"Open folder", "Open log", "Start at login", "-", "Exit"}
	if got := labels(m); !slices.Equal(got, want) {
		t.Errorf("menu = %q\nwant   %q", got, want)
	}
	if it, _ := find(m, "CloudSync: in sync"); !it.disabled {
		t.Error("the status line should be greyed")
	}
	if it, _ := find(m, "Start at login"); !it.checked || it.disabled {
		t.Errorf("Start at login = %+v", it)
	}
}

func TestBuildMenuPausedAndStopped(t *testing.T) {
	m := buildMenu(fullControls(), Snapshot{Running: true, Paused: true})
	if _, ok := find(m, "Resume syncing"); !ok {
		t.Errorf("paused menu = %q", labels(m))
	}
	if _, ok := find(m, "Pause syncing"); ok {
		t.Error("paused menu still offers Pause")
	}

	m = buildMenu(fullControls(), Snapshot{Running: false})
	if it, ok := find(m, "Start sync"); !ok || it.disabled {
		t.Errorf("stopped menu: Start sync = %+v, %v", it, ok)
	}
	for _, l := range []string{"Sync now", "Pause syncing"} {
		if it, ok := find(m, l); !ok || !it.disabled {
			t.Errorf("%s should be greyed when sync isn't running: %+v", l, it)
		}
	}
	m = buildMenu(fullControls(), Snapshot{StartSyncCannot: "not signed in"})
	if it, _ := find(m, "Start sync"); !it.disabled {
		t.Error("Start sync should be greyed when it can't work")
	}
}

func TestBuildMenuSeveralFoldersAndMissingCallbacks(t *testing.T) {
	m := buildMenu(fullControls(), Snapshot{Running: true, Folders: []string{"A", "B"}})
	if _, ok := find(m, "Open folder: B"); !ok {
		t.Errorf("menu = %q", labels(m))
	}
	// A tray with only an Exit item has no stray separators.
	m = buildMenu(Controls{Exit: func() {}}, Snapshot{Status: "x"})
	if got, want := labels(m), []string{"x", "-", "Exit"}; !slices.Equal(got, want) {
		t.Errorf("menu = %q, want %q", got, want)
	}
}

func TestChoose(t *testing.T) {
	var syncNow, pause, resume, stop, start, folder, login atomic.Int32
	var gotFolder, gotLogin atomic.Int32
	c := Controls{
		SyncNow: func() { syncNow.Add(1) }, Pause: func() { pause.Add(1) }, Resume: func() { resume.Add(1) },
		StopSync: func() { stop.Add(1) }, StartSync: func() { start.Add(1) },
		OpenFolder: func(i int) { folder.Add(1); gotFolder.Store(int32(i)) },
		SetStartAtLogin: func(on bool) {
			login.Add(1)
			if on {
				gotLogin.Store(1)
			}
		},
	}
	running := Snapshot{Running: true, Folders: []string{"A", "B"}}
	for _, id := range []int{idSyncNow, idPauseResume, idStartStop, idFolder + 1, idLogin} {
		if !choose(c, running, id) {
			t.Errorf("item %d did nothing", id)
		}
	}
	choose(c, Snapshot{Running: true, Paused: true}, idPauseResume)
	choose(c, Snapshot{}, idStartStop)
	if choose(c, running, idFolder+5) || choose(c, running, 999) {
		t.Error("an unknown item did something")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) &&
		syncNow.Load()+pause.Load()+resume.Load()+stop.Load()+start.Load()+folder.Load()+login.Load() < 7 {
		time.Sleep(5 * time.Millisecond)
	}
	if syncNow.Load() != 1 || pause.Load() != 1 || resume.Load() != 1 || stop.Load() != 1 || start.Load() != 1 {
		t.Errorf("calls: syncNow %d pause %d resume %d stop %d start %d",
			syncNow.Load(), pause.Load(), resume.Load(), stop.Load(), start.Load())
	}
	if folder.Load() != 1 || gotFolder.Load() != 1 {
		t.Errorf("folder calls %d, index %d", folder.Load(), gotFolder.Load())
	}
	if login.Load() != 1 || gotLogin.Load() != 1 {
		t.Errorf("login toggled %d times, on=%d (it was off, so it should turn on)", login.Load(), gotLogin.Load())
	}
}

func TestIconBits(t *testing.T) {
	states := []State{StateOK, StateBusy, StateAttention, StateError, StatePaused, StateStopped}
	for _, size := range []int{16, 24, 32} {
		for _, st := range states {
			b := iconBits(st, size)
			maskRow := (size + 31) / 32 * 4
			if want := 40 + size*size*4 + maskRow*size; len(b) != want {
				t.Fatalf("size %d state %d: %d bytes, want %d", size, st, len(b), want)
			}
			if h := binary.LittleEndian.Uint32(b[8:]); h != uint32(size*2) {
				t.Errorf("size %d state %d: height field %d, want %d (colour plus mask)", size, st, h, size*2)
			}
			alpha := func(x, y int) byte { return b[40+((size-1-y)*size+x)*4+3] }
			if alpha(0, 0) != 0 {
				t.Errorf("size %d state %d: corner alpha %d, want 0", size, st, alpha(0, 0))
			}
			// The symbol is white inside a coloured cloud (or the cloud's
			// outline, for the hollow stopped one): both must be there.
			var white, coloured int
			for y := 0; y < size; y++ {
				for x := 0; x < size; x++ {
					c := iconImage(st, size).NRGBAAt(x, y)
					switch {
					case c.A < 255:
					case c.R == 255 && c.G == 255 && c.B == 255:
						white++
					default:
						coloured++
					}
				}
			}
			if coloured == 0 || (st != StateStopped && white == 0) {
				t.Errorf("size %d state %d: %d white and %d coloured pixels", size, st, white, coloured)
			}
		}
	}
	// Every state looks different.
	seen := map[string]State{}
	for _, st := range states {
		k := string(iconBits(st, 32))
		if other, dup := seen[k]; dup {
			t.Errorf("states %d and %d have the same icon", other, st)
		}
		seen[k] = st
	}
}
