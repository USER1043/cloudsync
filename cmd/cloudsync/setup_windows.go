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

// args are what the background copy runs with. A Startup entry can't set
// environment variables, so a scratch config is passed as flags.
func (s *startupEntry) args() []string {
	args := []string{"run", "--log-file"}
	if v := s.p.env["CLOUDSYNC_CONFIG"]; v != "" {
		args = append(args, "--config", v)
	}
	if v := s.p.env["CLOUDSYNC_STATE_DIR"]; v != "" {
		args = append(args, "--state-dir", v)
	}
	return args
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
	cmd := syscall.EscapeArg(backgroundExe(s.p))
	for _, a := range s.args() {
		cmd += " " + syscall.EscapeArg(a)
	}
	return k.SetStringValue(s.valueName(), cmd)
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

func (s *startupEntry) launch() error {
	cmd := exec.Command(backgroundExe(s.p), s.args()...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// stopEventName names the event a running `cloudsync run` waits on: set
// it, and it uploads its last changes and exits.
func stopEventName(p placement) string { return `Local\` + p.name + "-stop" }

func (s *startupEntry) halt() error {
	name, err := windows.UTF16PtrFromString(stopEventName(s.p))
	if err != nil {
		return err
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil // nothing running in the background
	}
	if err != nil {
		return fmt.Errorf("asking CloudSync to stop: %w", err)
	}
	defer windows.CloseHandle(h)
	return windows.SetEvent(h)
}

func (s *startupEntry) logHint() string { return filepath.Join(s.p.stateDir, logFileName) }

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
