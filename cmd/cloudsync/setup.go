package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/USER1043/cloudsync/internal/config"
)

// placement is where `cloudsync start` puts CloudSync for the config in
// use. The usual config gets the usual places. A scratch config
// (CLOUDSYNC_CONFIG or CLOUDSYNC_STATE_DIR set, as in tests) gets its own
// program copy and service name, so testing never touches the real setup.
type placement struct {
	name     string            // "cloudsync", or "cloudsync-<id>" for a scratch config
	exe      string            // the installed program
	stateDir string            // where `run` keeps its lock and status
	env      map[string]string // passed on to the background process (scratch configs)
}

func place() (placement, error) {
	stateDir, err := config.StateDir()
	if err != nil {
		return placement{}, err
	}
	if stateDir, err = filepath.Abs(stateDir); err != nil {
		return placement{}, err
	}
	p := placement{name: "cloudsync", stateDir: stateDir, env: map[string]string{}}
	if os.Getenv("CLOUDSYNC_CONFIG") == "" && os.Getenv("CLOUDSYNC_STATE_DIR") == "" {
		p.exe, err = defaultExe()
		return p, err
	}
	sum := sha256.Sum256([]byte(stateDir))
	p.name = "cloudsync-" + hex.EncodeToString(sum[:4])
	p.exe = filepath.Join(stateDir, "bin", exeName())
	for _, k := range []string{"CLOUDSYNC_CONFIG", "CLOUDSYNC_STATE_DIR", "XDG_DATA_HOME"} {
		if v := os.Getenv(k); v != "" {
			if v, err = filepath.Abs(v); err != nil {
				return placement{}, err
			}
			p.env[k] = v
		}
	}
	return p, nil
}

// exitNeedsOwner is the exit status for something only the owner can fix
// (sign-in, Drive folder). systemd doesn't restart `run` after it:
// restarting would only repeat the problem, and its notification.
const exitNeedsOwner = 3

// service starts and stops the background `cloudsync run` the way this
// system does it: a systemd user service on Linux, a Startup entry on
// Windows.
type service interface {
	registered() bool // set to start at login
	register() error  // start at login from now on
	unregister() error
	launch() error // start now
	halt() error   // stop now, letting it upload its last changes first
	logHint() string
}

// installProgram copies this program to p.exe (and, on Windows, its
// windowless twin), keeping the copy it replaces as ".previous".
func installProgram(p placement, self string) error {
	if err := os.MkdirAll(filepath.Dir(p.exe), 0o755); err != nil {
		return err
	}
	for _, f := range programFiles(p) {
		if _, err := os.Stat(f); err == nil {
			if err := os.Rename(f, f+".previous"); err != nil {
				return fmt.Errorf("keeping the previous copy of %s: %w", f, err)
			}
		}
	}
	return writeProgram(p, self)
}

// restorePrevious puts back the copy installProgram replaced.
func restorePrevious(p placement) error {
	for _, f := range programFiles(p) {
		if err := os.Rename(f+".previous", f); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies src to dst through a temporary file, so dst is never
// half-written.
func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// selfPath is this program's own file.
func selfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && strings.EqualFold(filepath.Clean(ra), filepath.Clean(rb))
}

// installedVersion asks the installed program for its version.
func installedVersion(exe string) string {
	out, err := exec.Command(exe, "version").Output()
	if err != nil {
		return "unknown"
	}
	if f := strings.Fields(string(out)); len(f) == 2 {
		return f[1]
	}
	return "unknown"
}

// waitRunning waits until a `cloudsync run` holds its lock and is still
// holding it a few seconds later, so a build that fails straight away
// isn't taken for a working one.
func waitRunning(p placement) bool {
	deadline := time.Now().Add(20 * time.Second)
	for !runIsRunning(p.stateDir) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	return runIsRunning(p.stateDir)
}

// waitStopped waits until no `cloudsync run` holds its lock.
func waitStopped(p placement, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for runIsRunning(p.stateDir) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
	return true
}

// readyToRun checks that `cloudsync run` can work for this config: signed
// in, and every folder's first sync done (it needs the owner's go-ahead).
func readyToRun(ctx context.Context) error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	if _, err := e.drive(ctx); err != nil {
		return err
	}
	db, err := peekState()
	if err != nil {
		return err
	}
	if db != nil {
		defer db.Close()
	}
	for _, root := range e.cfg.Roots {
		if done, err := firstSyncDone(ctx, db, root.Name); err != nil {
			return err
		} else if !done {
			return fmt.Errorf("%s hasn't had its first sync yet: run `cloudsync plan`, then `cloudsync sync`, then `cloudsync start`", root.Name)
		}
	}
	return nil
}

func noArgs(name string, args []string) error {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fl.Arg(0))
	}
	return nil
}

func runStart(ctx context.Context, args []string) error {
	if err := noArgs("start", args); err != nil {
		return err
	}
	if err := readyToRun(ctx); err != nil {
		return err
	}
	p, err := place()
	if err != nil {
		return err
	}
	svc, err := newService(p)
	if err != nil {
		return err
	}
	self, err := selfPath()
	if err != nil {
		return err
	}
	w := os.Stdout

	_, statErr := os.Stat(p.exe)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(p.exe), 0o755); err != nil {
			return err
		}
		if err := writeProgram(p, self); err != nil {
			return fmt.Errorf("installing CloudSync to %s: %w", p.exe, err)
		}
		fmt.Fprintf(w, "Installed CloudSync %s to %s.\n", buildVersion(), p.exe)
	case statErr != nil:
		return statErr
	case !samePath(self, p.exe):
		if a, _ := fileHash(self); a != "" {
			if b, _ := fileHash(p.exe); a != b {
				fmt.Fprintf(w, "Note: this build (%s) differs from the installed one (%s); `cloudsync update` switches to it.\n",
					buildVersion(), installedVersion(p.exe))
			}
		}
	}

	if !svc.registered() {
		if err := svc.register(); err != nil {
			return fmt.Errorf("setting CloudSync to start at login: %w", err)
		}
		fmt.Fprintln(w, "CloudSync now starts automatically when you log in.")
	}
	if runIsRunning(p.stateDir) {
		fmt.Fprintln(w, "It's already running. Check on it with `cloudsync status`.")
		return nil
	}
	if err := svc.launch(); err != nil {
		return fmt.Errorf("starting CloudSync: %w", err)
	}
	if !waitRunning(p) {
		return fmt.Errorf("CloudSync didn't start; its log says why: %s", svc.logHint())
	}
	fmt.Fprintf(w, "It's running now.\nCheck on it any time:  cloudsync status\nSee its log:           %s\n", svc.logHint())
	return nil
}

func runStop(ctx context.Context, args []string) error {
	if err := noArgs("stop", args); err != nil {
		return err
	}
	p, err := place()
	if err != nil {
		return err
	}
	svc, err := newService(p)
	if err != nil {
		return err
	}
	w := os.Stdout
	wasRegistered := svc.registered()
	if runIsRunning(p.stateDir) || wasRegistered {
		fmt.Fprintln(w, "Stopping CloudSync; it uploads its last changes first...")
		if err := svc.halt(); err != nil {
			return fmt.Errorf("stopping CloudSync: %w", err)
		}
	}
	if wasRegistered {
		if err := svc.unregister(); err != nil {
			return fmt.Errorf("removing CloudSync from login: %w", err)
		}
	}
	if !waitStopped(p, 60*time.Second) {
		return errors.New("a `cloudsync run` is still going, probably in a terminal; stop it there with Ctrl-C")
	}
	if !wasRegistered {
		fmt.Fprintln(w, "CloudSync wasn't set to start at login; nothing else to do.")
		return nil
	}
	fmt.Fprintln(w, "Stopped CloudSync; it won't start at login any more.")
	fmt.Fprintln(w, "Your files, settings and sign-in are kept. To turn it back on: cloudsync start")
	return nil
}

func runUpdate(ctx context.Context, args []string) error {
	if err := noArgs("update", args); err != nil {
		return err
	}
	p, err := place()
	if err != nil {
		return err
	}
	svc, err := newService(p)
	if err != nil {
		return err
	}
	self, err := selfPath()
	if err != nil {
		return err
	}
	w := os.Stdout
	if _, err := os.Stat(p.exe); errors.Is(err, fs.ErrNotExist) {
		return errors.New("CloudSync isn't installed on this computer yet; run `cloudsync start` to install and start it")
	} else if err != nil {
		return err
	}
	if samePath(self, p.exe) {
		return fmt.Errorf("this is the installed copy (%s); run `update` from the new build instead, e.g. `bin/cloudsync update`", p.exe)
	}
	a, err := fileHash(self)
	if err != nil {
		return err
	}
	if b, err := fileHash(p.exe); err != nil {
		return err
	} else if a == b {
		fmt.Fprintf(w, "Already up to date (%s).\n", buildVersion())
		return nil
	}
	old := installedVersion(p.exe)
	fmt.Fprintf(w, "Updating CloudSync from %s to %s.\n", old, buildVersion())

	running := runIsRunning(p.stateDir)
	if running {
		fmt.Fprintln(w, "Stopping the running copy; it uploads its last changes first...")
		if err := svc.halt(); err != nil {
			return fmt.Errorf("stopping CloudSync: %w", err)
		}
		if !waitStopped(p, 60*time.Second) {
			return errors.New("the running CloudSync didn't stop, so nothing was changed; if it runs in a terminal, stop it there with Ctrl-C")
		}
	}
	if err := installProgram(p, self); err != nil {
		return fmt.Errorf("installing the new version: %w", err)
	}
	if !running && !svc.registered() {
		fmt.Fprintln(w, "Updated. CloudSync isn't set to run in the background; `cloudsync start` turns it on.")
		return nil
	}
	if err := svc.launch(); err == nil && waitRunning(p) {
		fmt.Fprintf(w, "Updated and running %s. The previous version is kept as %s.previous.\n", buildVersion(), p.exe)
		return nil
	}
	// The new build doesn't run: go back to the one that did.
	fmt.Fprintln(w, "The new version didn't start; going back to the previous one...")
	if err := restorePrevious(p); err != nil {
		return fmt.Errorf("the new version didn't start, and putting the previous one back failed too (%v); see %s", err, svc.logHint())
	}
	if err := svc.launch(); err != nil || !waitRunning(p) {
		return fmt.Errorf("the new version didn't start, and neither did the previous one; see %s", svc.logHint())
	}
	return fmt.Errorf("the new version didn't start, so CloudSync is back on %s and running; its log says what went wrong: %s",
		old, svc.logHint())
}
