//go:build !windows

package main

import (
	"context"
	"errors"
)

func runTray(context.Context, []string) error {
	return errors.New("the tray icon is only available on Windows")
}
