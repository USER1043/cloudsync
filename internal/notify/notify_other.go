//go:build !linux && !windows

package notify

import "errors"

func newBackend() (backend, error) {
	return nil, errors.New("desktop notifications aren't supported on this system")
}
