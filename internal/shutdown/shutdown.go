// Package shutdown tells `cloudsync run` that the computer is shutting
// down, as early as the system allows, so it can leave a note in Drive of
// the files it hasn't uploaded yet and use the time left to upload them.
//
// Linux: a logind "delay" inhibitor and its PrepareForShutdown signal. The
// shutdown waits for CloudSync at most InhibitDelayMaxSec (5 s by default)
// and is never blocked: KDE Plasma gives no message for a blocked shutdown,
// its Shut Down button just does nothing. Systemd's SIGTERM follows, which
// the daemon handles as usual.
//
// Windows: a hidden top-level window answering WM_QUERYENDSESSION. While
// Hooks.Holding gives a reason, Windows holds the shutdown and shows it
// ("CloudSync is preventing shutdown: ...") with a "Shut down anyway"
// button. WM_ENDSESSION then calls Hooks.Ending.
//
// Sleep is never delayed or blocked.
package shutdown

import "context"

// Hooks are CloudSync's answers to a shutdown. Any may be nil.
type Hooks struct {
	// Starting is called once when a shutdown starts (on Windows, also a
	// sign-out), before programs are told to stop. ctx ends shortly before
	// the system stops waiting: about 4 s on Linux.
	Starting func(ctx context.Context)

	// Holding (Windows) returns why the shutdown should wait, such as
	// "Uploading 3 files to Google Drive", or "" to let it go ahead. It is
	// asked when a shutdown starts and again after each Recheck while the
	// shutdown is held.
	Holding func() string

	// Ending (Windows) is called when the session ends for certain. The
	// process may be stopped at any time after it returns.
	Ending func()
}

// Guard watches for shutdowns until closed.
type Guard struct{ b backend }

type backend interface {
	recheck()
	close()
}

// Start begins watching for shutdowns.
func Start(h Hooks) (*Guard, error) {
	if h.Starting == nil {
		h.Starting = func(context.Context) {}
	}
	if h.Holding == nil {
		h.Holding = func() string { return "" }
	}
	if h.Ending == nil {
		h.Ending = func() {}
	}
	b, err := start(h)
	if err != nil {
		return nil, err
	}
	return &Guard{b: b}, nil
}

// Recheck asks Hooks.Holding again if a shutdown is being held, and lets it
// go ahead once there is no reason left. Call it when the reason may have
// changed, such as after each sync. It doesn't wait.
func (g *Guard) Recheck() { g.b.recheck() }

// Close stops watching.
func (g *Guard) Close() { g.b.close() }
