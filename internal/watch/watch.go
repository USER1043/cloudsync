// Package watch tells `cloudsync run` that something changed in a synced
// folder. It deliberately doesn't say what: the caller rescans the folder
// and compares it with the last sync, so a lost, merged or misreported event
// can never lose an edit, and the watcher can stay simple.
//
// Linux watches each folder separately (inotify, through fsnotify), so the
// caller passes the folder list again after every rescan. Windows watches a
// whole tree with one handle on the root (ReadDirectoryChangesW), because
// watching each subfolder would stop the owner from renaming folders in
// Explorer while CloudSync runs.
package watch

// Watcher signals changes under the folders it watches.
type Watcher struct {
	c    chan struct{}
	errs chan error
	b    backend
}

// backend is the system-specific part of a Watcher.
type backend interface {
	watch(root string, dirs []string) error
	close() error
}

// New starts a Watcher that watches nothing yet.
func New() (*Watcher, error) {
	w := &Watcher{c: make(chan struct{}, 1), errs: make(chan error, 16)}
	b, err := newBackend(w.changed, w.failed)
	if err != nil {
		return nil, err
	}
	w.b = b
	return w, nil
}

// C receives a value when something changed since the last receive. Changes
// that happen before the receiver gets to it are merged into one.
func (w *Watcher) C() <-chan struct{} { return w.c }

// Errors receives problems the watcher ran into while watching. None is
// fatal; changes may be missed until the next rescan.
func (w *Watcher) Errors() <-chan error { return w.errs }

// Watch makes w watch root, an absolute path. dirs lists every folder under
// root to watch, as absolute paths; folders not listed any more are dropped.
// On Windows, which watches the whole tree, dirs is ignored. A folder that
// is gone by the time it is added is skipped; the next rescan notices.
func (w *Watcher) Watch(root string, dirs []string) error { return w.b.watch(root, dirs) }

// Close stops watching.
func (w *Watcher) Close() error { return w.b.close() }

// changed records that something changed, without blocking.
func (w *Watcher) changed() {
	select {
	case w.c <- struct{}{}:
	default: // a signal is already waiting
	}
}

// failed reports a problem without blocking; if nobody is reading, it is
// dropped.
func (w *Watcher) failed(err error) {
	select {
	case w.errs <- err:
	default:
	}
}
