//go:build !linux && !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

func exeName() string { return "cloudsync" }

func defaultExe() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin", exeName()), nil
}

func programFiles(p placement) []string { return []string{p.exe} }

func writeProgram(p placement, self string) error { return copyFile(self, p.exe, 0o755) }

func newService(placement) (service, error) {
	return nil, errors.New("starting CloudSync at login isn't supported on this system yet; run `cloudsync run` yourself")
}

func watchStopRequests(context.Context, placement, func()) {}

const trayOwnsNotices = false

func watchControl(context.Context, placement, func(ctlOp)) {}
