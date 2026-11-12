package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/notify"
	"github.com/USER1043/cloudsync/internal/state"
)

const (
	metaLastCheck  = "last_drive_check:" // + root name: the last full comparison with Drive
	metaLastUpload = "last_upload:"      // + root name: the last live pass that changed something

	statusFile  = "status.json"
	runLockFile = "run.lock"
	maxWarnings = 20
)

// runStatus is what `cloudsync run` is doing, kept in the state folder for
// `cloudsync status`. Notifications vanish; their text stays here.
type runStatus struct {
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`
	Updated  time.Time `json:"updated"`
	State    string    `json:"state"`  // e.g. "in sync", "uploading", "offline"
	Detail   string    `json:"detail"` // what's wrong, if anything
	Since    time.Time `json:"since"`  // when State began
	Warnings []warning `json:"warnings,omitempty"`
}

// warning is a notification as it was sent, newest first in runStatus.
type warning struct {
	At    time.Time `json:"at"`
	Key   string    `json:"key"`
	Title string    `json:"title"`
	Body  string    `json:"body"`

	// Critical is the notification's flag, kept so the tray (a separate
	// process on Windows) can show it the same way.
	Critical bool `json:"critical,omitempty"`
}

// statusKeeper writes runStatus to disk whenever it changes.
type statusKeeper struct {
	path string

	mu sync.Mutex
	st runStatus
}

func newStatusKeeper(dir string) *statusKeeper {
	now := time.Now()
	return &statusKeeper{
		path: filepath.Join(dir, statusFile),
		st:   runStatus{PID: os.Getpid(), Started: now, Since: now, State: "starting"},
	}
}

// set records what the daemon is doing.
func (k *statusKeeper) set(state, detail string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if state != k.st.State {
		k.st.Since = time.Now()
	}
	k.st.State, k.st.Detail = state, detail
	k.save()
}

// warn records a notification, replacing an older one with its key.
func (k *statusKeeper) warn(m notify.Message) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.st.Warnings = slices.DeleteFunc(k.st.Warnings, func(w warning) bool { return w.Key == m.Key })
	k.st.Warnings = slices.Insert(k.st.Warnings, 0, warning{At: time.Now(), Key: m.Key, Title: m.Title, Body: m.Body, Critical: m.Critical})
	if len(k.st.Warnings) > maxWarnings {
		k.st.Warnings = k.st.Warnings[:maxWarnings]
	}
	k.save()
}

// resolve drops the warning under key, once its problem is gone.
func (k *statusKeeper) resolve(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := len(k.st.Warnings)
	k.st.Warnings = slices.DeleteFunc(k.st.Warnings, func(w warning) bool { return w.Key == key })
	if len(k.st.Warnings) != n {
		k.save()
	}
}

// save writes the status atomically, so a reader never sees half of it.
// Callers hold mu. A failure only costs `cloudsync status` its detail.
func (k *statusKeeper) save() {
	k.st.Updated = time.Now()
	data, err := json.MarshalIndent(k.st, "", "  ")
	if err != nil {
		return
	}
	tmp := k.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, k.path)
	}
}

// lockRun takes the lock that marks `cloudsync run` as running, so a
// second one refuses to start and `cloudsync status` can tell.
func lockRun(dir string) (unlock func(), err error) {
	lock := flock.New(filepath.Join(dir, runLockFile))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", lock.Path(), err)
	}
	if !ok {
		return nil, errors.New("CloudSync is already running in the background (`cloudsync run`); see `cloudsync status`")
	}
	return func() { lock.Unlock() }, nil
}

// runIsRunning reports whether a `cloudsync run` holds its lock.
func runIsRunning(dir string) bool {
	lock := flock.New(filepath.Join(dir, runLockFile))
	ok, err := lock.TryLock()
	if err != nil {
		return false
	}
	if ok {
		lock.Unlock()
		return false
	}
	return true
}

func runStatusCmd(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("status", flag.ContinueOnError)
	if err := fl.Parse(args); err != nil {
		return err
	}
	e, err := loadEnv()
	if err != nil {
		return err
	}
	dir, err := config.StateDir()
	if err != nil {
		return err
	}
	w := os.Stdout
	var st runStatus
	data, err := os.ReadFile(filepath.Join(dir, statusFile))
	switch {
	case err == nil:
		json.Unmarshal(data, &st)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	running := runIsRunning(dir)
	printRunState(w, st, running, time.Now())
	if trayOwnsNotices {
		if trayIsRunning(dir) {
			fmt.Fprintln(w, "Tray icon: running (taskbar hidden icons, the ^ arrow).")
		} else {
			fmt.Fprintln(w, "Tray icon: not running. `cloudsync start` brings it back.")
		}
	}

	db, err := peekState()
	if err != nil {
		return err
	}
	if db == nil {
		fmt.Fprintln(w, "\nNothing has been synced yet: run `cloudsync plan`, then `cloudsync sync`.")
		return nil
	}
	defer db.Close()
	m, err := matcher(e.cfg)
	if err != nil {
		return err
	}
	for _, root := range e.cfg.Roots {
		if err := printRootStatus(ctx, w, root, m, db, time.Now()); err != nil {
			return fmt.Errorf("root %q: %w", root.Name, err)
		}
	}
	if len(st.Warnings) > 0 {
		fmt.Fprintln(w, "\nRecent warnings (newest first):")
		for _, wn := range st.Warnings {
			fmt.Fprintf(w, "  %s  %s: %s\n", when(wn.At, time.Now()), wn.Title, wn.Body)
		}
	}
	return nil
}

func printRunState(w io.Writer, st runStatus, running bool, now time.Time) {
	switch {
	case running:
		fmt.Fprintf(w, "CloudSync is running in the background (since %s): %s", when(st.Started, now), st.State)
		if !st.Since.IsZero() && st.State != "in sync" {
			fmt.Fprintf(w, " since %s", when(st.Since, now))
		}
		fmt.Fprintln(w, ".")
		if st.Detail != "" {
			fmt.Fprintf(w, "  %s\n", st.Detail)
		}
	case !st.Updated.IsZero():
		fmt.Fprintf(w, "CloudSync is not running in the background. Last seen %s: %s.\n", when(st.Updated, now), st.State)
		if st.Detail != "" {
			fmt.Fprintf(w, "  %s\n", st.Detail)
		}
		fmt.Fprintln(w, "Turn it on with `cloudsync start`.")
	default:
		fmt.Fprintln(w, "CloudSync is not running in the background. Turn it on with `cloudsync start`.")
	}
}

func printRootStatus(ctx context.Context, w io.Writer, root config.Root, m *filter.Matcher, db *state.DB, now time.Time) error {
	local, err := root.LocalPath()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\n== %s\nLocal: %s\nDrive: My Drive/%s\n", root.Name, local, strings.Join(root.RemoteSegments(), "/"))
	if done, err := firstSyncDone(ctx, db, root.Name); err != nil {
		return err
	} else if !done {
		fmt.Fprintln(w, "First sync not done yet: run `cloudsync plan`, then `cloudsync sync`.")
		return nil
	}
	meta := func(key string) time.Time {
		v, _, _ := db.Meta(ctx, key+root.Name)
		t, _ := time.Parse(time.RFC3339, v)
		return t
	}
	if t := meta(metaLastCheck); !t.IsZero() {
		fmt.Fprintf(w, "Drive last checked: %s. It's checked when `cloudsync run` starts and by `cloudsync sync`.\n", when(t, now))
	}
	if t := meta(metaLastUpload); !t.IsZero() {
		fmt.Fprintf(w, "Last upload: %s\n", when(t, now))
	}

	rootID, _, err := db.Meta(ctx, metaRootID+root.Name)
	if err != nil {
		return err
	}
	l, _, err := planLive(ctx, &session{root: root, local: local, rootID: rootID}, m, db, now, 0)
	if err != nil {
		return err
	}
	p := pendingFrom(l, "", now)
	if len(p.Files) == 0 {
		fmt.Fprintln(w, "Nothing here waiting to upload.")
	} else {
		paths := make([]string, len(p.Files))
		for i, f := range p.Files {
			paths[i] = f.Path
			if f.Deleted {
				paths[i] += " (deleted)"
			}
		}
		printList(w, "Waiting to upload", paths, 10)
	}
	if ev, _, ok, err := db.PendingDeletes(ctx, root.Name); err != nil {
		return err
	} else if ok {
		fmt.Fprintf(w, "Held deletes: %s, waiting for you since %s. Review with `cloudsync deletes`.\n",
			count(ev.Local+ev.Remote, "file", "files"), when(time.Unix(ev.At, 0), now))
	}
	return nil
}

// when shows t as a time of day, with the date if it isn't today, and how
// long ago it was.
func when(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	layout := "15:04"
	if t.YearDay() != now.YearDay() || t.Year() != now.Year() {
		layout = "2 Jan 15:04"
	}
	return fmt.Sprintf("%s (%s)", t.Format(layout), ago(now.Sub(t)))
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return count(int(d/time.Minute), "minute", "minutes") + " ago"
	case d < 48*time.Hour:
		return count(int(d/time.Hour), "hour", "hours") + " ago"
	}
	return count(int(d/(24*time.Hour)), "day", "days") + " ago"
}
