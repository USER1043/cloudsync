package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

func exeName() string { return "cloudsync" }

// defaultExe is where `cloudsync start` installs the program: ~/.local/bin,
// which is on the PATH of most Linux desktops.
func defaultExe() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin", exeName()), nil
}

func programFiles(p placement) []string { return []string{p.exe} }

func writeProgram(p placement, self string) error { return copyFile(self, p.exe, 0o755) }

// systemdUser runs `cloudsync run` as a systemd user service tied to the
// graphical session: it starts once the desktop is up (so the keyring and
// notifications are there) and stops at logout or shutdown, when systemd
// sends SIGTERM and waits for the last uploads.
type systemdUser struct {
	p    placement
	unit string // e.g. "cloudsync.service"
	path string // the unit file
}

func newService(p placement) (service, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	unit := p.name + ".service"
	return &systemdUser{p: p, unit: unit, path: filepath.Join(dir, "systemd", "user", unit)}, nil
}

// unitFile is the service definition. TimeoutStopSec stays inside Fedora's
// 1 min for the whole user session; the flush gives up after 40 s.
func unitFile(p placement) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[Unit]
Description=CloudSync: keeps folders in sync with Google Drive
Documentation=https://github.com/USER1043/cloudsync
PartOf=graphical-session.target
After=graphical-session.target

[Service]
ExecStart=%s run
`, systemdQuote(p.exe))
	keys := make([]string, 0, len(p.env))
	for k := range p.env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(k+"="+p.env[k]))
	}
	fmt.Fprintf(&b, `Restart=on-failure
RestartSec=30
# Stopped for something only you can fix (sign-in, Drive folder): don't loop.
RestartPreventExitStatus=%d
TimeoutStopSec=45

[Install]
WantedBy=graphical-session.target
`, exitNeedsOwner)
	return b.String()
}

// systemdQuote quotes a word for a unit file, where "%" and "\" are
// special too.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

func (s *systemdUser) systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(out.String()); msg != "" {
			return fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), msg)
		}
		return fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func (s *systemdUser) registered() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

func (s *systemdUser) register() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, []byte(unitFile(s.p)), 0o644); err != nil {
		return err
	}
	if err := s.systemctl("daemon-reload"); err != nil {
		return err
	}
	return s.systemctl("enable", s.unit)
}

func (s *systemdUser) unregister() error {
	if err := s.systemctl("disable", s.unit); err != nil {
		return err
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return s.systemctl("daemon-reload")
}

func (s *systemdUser) launch() error {
	// A unit that stopped because it needed the owner is "failed"; clear
	// that so it starts like new.
	_ = s.systemctl("reset-failed", s.unit)
	return s.systemctl("start", s.unit)
}

// halt stops the service; systemctl waits until it has exited, flush and
// all.
func (s *systemdUser) halt() error {
	if !s.registered() {
		return nil
	}
	return s.systemctl("stop", s.unit)
}

func (s *systemdUser) logHint() string { return "journalctl --user -u " + s.unit }

// refresh has nothing to do: the unit file is written by register.
func (s *systemdUser) refresh() error { return nil }

// There is no tray on Linux: run shows the notifications itself.
const trayOwnsNotices = false

func watchControl(context.Context, placement, func(ctlOp)) {}

// watchStopRequests isn't needed on Linux: systemd stops `run` with
// SIGTERM.
func watchStopRequests(context.Context, placement, func()) {}
