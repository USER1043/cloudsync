package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func exeName() string { return "cloudsync.exe" }

// defaultExe is where `cloudsync start` installs the program, the usual
// per-user place for programs: %LocalAppData%\Programs\CloudSync.
func defaultExe() (string, error) {
	dir := os.Getenv("LOCALAPPDATA")
	if dir == "" {
		return "", errors.New("LOCALAPPDATA is not set")
	}
	return filepath.Join(dir, "Programs", "CloudSync", exeName()), nil
}

// backgroundExe is the windowless copy that runs at login: the same
// program, marked so Windows opens no console window for it.
func backgroundExe(p placement) string {
	return strings.TrimSuffix(p.exe, ".exe") + "-background.exe"
}

func programFiles(p placement) []string { return []string{p.exe, backgroundExe(p)} }

func writeProgram(p placement, self string) error {
	if err := copyFile(self, p.exe, 0o755); err != nil {
		return err
	}
	return makeWindowless(self, backgroundExe(p))
}

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// startupEntry runs the background copy at login through the user's
// Startup entries (no administrator rights needed; listed in Task
// Manager's Startup apps). Windows doesn't restart it after a crash; the
// next login does.
type startupEntry struct{ p placement }

func newService(p placement) (service, error) { return &startupEntry{p: p}, nil }

// valueName is the Startup entry's name, as Task Manager shows it.
func (s *startupEntry) valueName() string {
	if s.p.name == "cloudsync" {
		return "CloudSync"
	}
	return s.p.name
}

// trayOwnsNotices is true where a separate tray process shows the
// notifications, from the warnings `run` keeps in its status file.
const trayOwnsNotices = true

// flags select a scratch config. A Startup entry can't set environment
// variables, so they are passed as flags.
func (s *startupEntry) flags() []string {
	var args []string
	if v := s.p.env["CLOUDSYNC_CONFIG"]; v != "" {
		args = append(args, "--config", v)
	}
	if v := s.p.env["CLOUDSYNC_STATE_DIR"]; v != "" {
		args = append(args, "--state-dir", v)
	}
	return args
}

// runArgs are what the background sync runs with.
func (s *startupEntry) runArgs() []string {
	return append([]string{"run", "--log-file"}, s.flags()...)
}

// trayArgs are what the tray runs with. It is what starts at login: it
// starts the sync itself, and stays when the sync is stopped.
func (s *startupEntry) trayArgs() []string {
	return append([]string{"tray"}, s.flags()...)
}

// loginCommand is the command line the Startup entry holds.
func (s *startupEntry) loginCommand() string {
	cmd := syscall.EscapeArg(backgroundExe(s.p))
	for _, a := range s.trayArgs() {
		cmd += " " + syscall.EscapeArg(a)
	}
	return cmd
}

func (s *startupEntry) registered() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(s.valueName())
	return err == nil
}

func (s *startupEntry) register() error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(s.valueName(), s.loginCommand())
}

// refresh rewrites the Startup entry if it holds an older command, such as
// the one from before the tray existed, which ran the sync directly.
func (s *startupEntry) refresh() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	cur, _, err := k.GetStringValue(s.valueName())
	k.Close()
	if err != nil || cur == s.loginCommand() {
		return nil
	}
	return s.register()
}

func (s *startupEntry) unregister() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(s.valueName()); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// spawn starts the background copy with args, detached from this process.
func (s *startupEntry) spawn(args []string) error {
	cmd := exec.Command(backgroundExe(s.p), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// launchRun starts the sync, unless it is running.
func (s *startupEntry) launchRun() error {
	if runIsRunning(s.p.stateDir) {
		return nil
	}
	return s.spawn(s.runArgs())
}

// launchTray starts the tray icon, unless it is running.
func (s *startupEntry) launchTray() error {
	if trayIsRunning(s.p.stateDir) {
		return nil
	}
	return s.spawn(s.trayArgs())
}

// launch starts the tray and the sync. Either is left alone if it already
// runs, so this is what brings back whichever the owner exited.
func (s *startupEntry) launch() error {
	return errors.Join(s.launchTray(), s.launchRun())
}

// Events the background processes wait on, by name: a command line or the
// tray sets one to ask for something. Stop makes `cloudsync run` upload its
// last changes and exit; the others are the tray's Sync now, Pause and
// Resume; trayexit closes the tray.
func stopEventName(p placement) string { return controlEventName(p, "stop") }

func controlEventName(p placement, what string) string { return `Local\` + p.name + "-" + what }

// setEvent sets the named event. It reports nil if nothing is waiting on it,
// because then there is nothing to ask.
func setEvent(name string) error {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, n)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("asking CloudSync to do that: %w", err)
	}
	defer windows.CloseHandle(h)
	return windows.SetEvent(h)
}

// haltRun asks the sync to upload its last changes and exit; the tray stays.
func (s *startupEntry) haltRun() error { return setEvent(stopEventName(s.p)) }

// halt stops the sync and the tray.
func (s *startupEntry) halt() error {
	return errors.Join(s.haltRun(), setEvent(controlEventName(s.p, "trayexit")))
}

func (s *startupEntry) logHint() string { return filepath.Join(s.p.stateDir, logFileName) }

// watchEvent calls fn each time the named event is set, until ctx ends. A
// manual event is for a one-time request: fn runs once. A second process
// creating the same name just opens the existing event.
func watchEvent(ctx context.Context, name string, manual bool, fn func()) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return
	}
	var m uint32
	if manual {
		m = 1
	}
	h, err := windows.CreateEvent(nil, m, 0, n)
	if err != nil {
		return
	}
	go func() {
		defer windows.CloseHandle(h)
		for ctx.Err() == nil {
			if ev, _ := windows.WaitForSingleObject(h, 500); ev == windows.WAIT_OBJECT_0 {
				fn()
				if manual {
					return
				}
			}
		}
	}()
}

// watchControl turns the tray's requests into ops for the live loop.
func watchControl(ctx context.Context, p placement, send func(ctlOp)) {
	for what, op := range map[string]ctlOp{"syncnow": ctlSyncNow, "pause": ctlPause, "resume": ctlResume} {
		watchEvent(ctx, controlEventName(p, what), false, func() { send(op) })
	}
}

// watchStopRequests calls stop when `cloudsync stop` or `update` sets this
// placement's stop event. Windows has no SIGTERM for a windowless program.
func watchStopRequests(ctx context.Context, p placement, stop func()) {
	name, err := windows.UTF16PtrFromString(stopEventName(p))
	if err != nil {
		return
	}
	h, err := windows.CreateEvent(nil, 1, 0, name)
	if err != nil {
		return
	}
	go func() {
		defer windows.CloseHandle(h)
		if ev, _ := windows.WaitForSingleObject(h, windows.INFINITE); ev == windows.WAIT_OBJECT_0 {
			stop()
		}
	}()
}
