//go:build !linux && !windows

package trash

import (
	"errors"
	"runtime"
)

func move(string) error {
	return errors.New("moving files to the trash is not supported on " + runtime.GOOS)
}
