//go:build !linux && !windows

package shutdown

// none is used where CloudSync has no way to hear of a shutdown; the
// daemon's usual stop signal still flushes.
type none struct{}

func start(Hooks) (backend, error) { return none{}, nil }

func (none) recheck() {}
func (none) close()   {}
