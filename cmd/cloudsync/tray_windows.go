package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/notify"
)

const (
	trayPoll    = time.Second
	trayLogName = "tray.log"
)

// trayApp is `cloudsync tray`: the taskbar icon and its menu. It is a
// process of its own, so it stays when the sync is stopped and can start it
// again. It reads what the sync is doing from the status file, asks it to do
// things through named events, and shows its warnings as notifications.
type trayApp struct {
	p     placement
	svc   *startupEntry
	notes *notify.Notifier
	cfg   config.Config
	log   func(format string, args ...any)

	exit     chan struct{}
	exitOnce sync.Once

	shown map[string]time.Time // warnings already shown, by key
}

func runTray(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("tray", flag.ContinueOnError)
	cfgFile := fl.String("config", "", "use this config file")
	stateDir := fl.String("state-dir", "", "use this state folder")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fl.Arg(0))
	}
	if *cfgFile != "" {
		os.Setenv("CLOUDSYNC_CONFIG", *cfgFile)
	}
	if *stateDir != "" {
		os.Setenv("CLOUDSYNC_STATE_DIR", *stateDir)
	}
	p, err := place()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.stateDir, 0o700); err != nil {
		return err
	}
	unlock, ok, err := lockTray(p.stateDir)
	if err != nil {
		return err
	}
	if !ok {
		return nil // already showing its icon
	}
	defer unlock()

	svc, err := newService(p)
	if err != nil {
		return err
	}
	t := &trayApp{p: p, svc: svc.(*startupEntry), exit: make(chan struct{}), shown: map[string]time.Time{}}
	t.log = func(format string, args ...any) {
		f, err := os.OpenFile(filepath.Join(p.stateDir, trayLogName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "%s "+format+"\n", append([]any{time.Now().Format("2006-01-02 15:04:05")}, args...)...)
	}
	if e, err := loadEnv(); err == nil {
		t.cfg = e.cfg
	}

	if t.notes, err = notify.New(); err != nil {
		t.log("Couldn't add the icon to the taskbar: %v", err)
		return fmt.Errorf("adding the CloudSync icon to the taskbar: %w", err)
	}
	defer t.notes.Close()
	t.notes.SetControls(t.controls())
	t.log("Tray started (%s).", buildVersion())

	// `cloudsync stop` and `update` ask the tray to close this way.
	watchEvent(ctx, controlEventName(p, "trayexit"), true, t.close)

	// Starting the tray starts the sync too; it is what runs at login.
	t.startSync()
	t.poll()

	tick := time.NewTicker(trayPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.exit:
			t.log("Tray exited.")
			return nil
		case <-tick.C:
			t.poll()
		}
	}
}

func (t *trayApp) close() { t.exitOnce.Do(func() { close(t.exit) }) }

// startSync starts the sync if it isn't running.
func (t *trayApp) startSync() {
	if err := t.svc.launchRun(); err != nil {
		t.log("Couldn't start the sync: %v", err)
	}
}

// poll shows the sync's state in the icon, and its new warnings as
// notifications.
func (t *trayApp) poll() {
	st, running := readStatus(t.p.stateDir), runIsRunning(t.p.stateDir)
	t.notes.SetState(trayState(st, running), statusLine(st, running))

	// A status file left by a sync that has since exited holds old warnings,
	// except the one that says why it stopped.
	warnings := st.Warnings
	if !running && st.State != "stopped: needs you" {
		warnings = nil
	}
	fresh, gone := warningDiff(t.shown, warnings)
	for _, key := range gone {
		t.notes.Clear(key)
	}
	for _, w := range fresh {
		if _, err := t.notes.Send(w.message()); err != nil {
			t.log("Couldn't show a notification: %v", err)
		}
	}
}

// snapshot is what the menu shows when it opens.
func (t *trayApp) snapshot() notify.Snapshot {
	st, running := readStatus(t.p.stateDir), runIsRunning(t.p.stateDir)
	s := notify.Snapshot{
		Status:       statusLine(st, running),
		Running:      running,
		Paused:       running && st.State == statePaused,
		StartAtLogin: t.svc.registered(),
	}
	_, err := os.Stat(backgroundExe(t.p))
	s.CanChangeLogin = err == nil
	_, err = os.Stat(filepath.Join(t.p.stateDir, logFileName))
	s.CanOpenLogFile = err == nil
	for _, root := range t.cfg.Roots {
		s.Folders = append(s.Folders, root.Name)
	}
	return s
}

func (t *trayApp) controls() notify.Controls {
	ask := func(what string) func() {
		return func() {
			if err := setEvent(controlEventName(t.p, what)); err != nil {
				t.log("Couldn't ask the sync to %s: %v", what, err)
			}
		}
	}
	return notify.Controls{
		Snapshot: t.snapshot,
		SyncNow:  ask("syncnow"),
		Pause:    ask("pause"),
		Resume:   ask("resume"),
		StartSync: func() {
			t.startSync()
		},
		StopSync: func() {
			if err := t.svc.haltRun(); err != nil {
				t.log("Couldn't stop the sync: %v", err)
			}
		},
		OpenFolder: func(i int) {
			if i < 0 || i >= len(t.cfg.Roots) {
				return
			}
			if dir, err := t.cfg.Roots[i].LocalPath(); err == nil {
				t.open(dir)
			}
		},
		OpenLog: func() { t.open(filepath.Join(t.p.stateDir, logFileName)) },
		SetStartAtLogin: func(on bool) {
			var err error
			if on {
				err = t.svc.register()
			} else {
				err = t.svc.unregister()
			}
			if err != nil {
				t.log("Couldn't change start at login: %v", err)
			}
		},
		Exit: func() {
			// Let the sync upload its last changes before the icon goes.
			if err := t.svc.haltRun(); err != nil {
				t.log("Couldn't stop the sync: %v", err)
			}
			deadline := time.Now().Add(flushTimeout + 20*time.Second)
			for runIsRunning(t.p.stateDir) && time.Now().Before(deadline) {
				time.Sleep(250 * time.Millisecond)
			}
			t.close()
		},
	}
}

// open shows a file or folder the way Explorer does.
func (t *trayApp) open(path string) {
	if err := exec.Command("explorer.exe", path).Start(); err != nil {
		t.log("Couldn't open %s: %v", path, err)
	}
}
