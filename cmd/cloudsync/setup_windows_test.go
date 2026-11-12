package main

import (
	"slices"
	"strings"
	"testing"
)

func TestLoginStartsTheTray(t *testing.T) {
	p := placement{name: "cloudsync", exe: `C:\Users\someone\AppData\Local\Programs\CloudSync\cloudsync.exe`, env: map[string]string{}}
	s := &startupEntry{p: p}
	if got, want := s.loginCommand(), `C:\Users\someone\AppData\Local\Programs\CloudSync\cloudsync-background.exe tray`; got != want {
		t.Errorf("login command = %q, want %q", got, want)
	}
	if !slices.Equal(s.runArgs(), []string{"run", "--log-file"}) {
		t.Errorf("run args = %v", s.runArgs())
	}

	// A scratch config is passed as flags, to both.
	s.p.env = map[string]string{"CLOUDSYNC_CONFIG": `C:\x y\config.toml`, "CLOUDSYNC_STATE_DIR": `C:\x y\state`}
	if got := s.trayArgs(); !slices.Equal(got, []string{"tray", "--config", `C:\x y\config.toml`, "--state-dir", `C:\x y\state`}) {
		t.Errorf("tray args = %v", got)
	}
	if cmd := s.loginCommand(); !strings.Contains(cmd, `"C:\x y\config.toml"`) {
		t.Errorf("a path with a space isn't quoted: %s", cmd)
	}
}

func TestEventNamesKeepScratchApart(t *testing.T) {
	a := controlEventName(placement{name: "cloudsync"}, "pause")
	b := controlEventName(placement{name: "cloudsync-1234abcd"}, "pause")
	if a == b || a != `Local\cloudsync-pause` {
		t.Errorf("names %q and %q", a, b)
	}
	if stopEventName(placement{name: "cloudsync"}) != `Local\cloudsync-stop` {
		t.Error("the stop event was renamed; running copies would no longer hear `stop`")
	}
}
