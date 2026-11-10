package watch

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"golang.org/x/sys/windows"
)

// treeBackend watches each root's whole tree through one directory handle,
// so no subfolder is held open and folders can still be renamed.
type treeBackend struct {
	changed func()
	failed  func(error)

	mu    sync.Mutex
	roots map[string]windows.Handle
}

func newBackend(changed func(), failed func(error)) (backend, error) {
	return &treeBackend{changed: changed, failed: failed, roots: map[string]windows.Handle{}}, nil
}

const notifyMask = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
	windows.FILE_NOTIFY_CHANGE_SIZE | windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_CREATION

// reader is one root's directory handle and the read waiting on it.
type reader struct {
	h   windows.Handle
	ov  windows.Overlapped
	buf []byte
}

// listen asks Windows for the next changes under the root. It returns at
// once; Windows records changes from this call on, and the result is
// collected through r.ov.
func (r *reader) listen() error {
	err := windows.ReadDirectoryChanges(r.h, &r.buf[0], uint32(len(r.buf)), true, notifyMask, nil, &r.ov, 0)
	if errors.Is(err, windows.ERROR_IO_PENDING) {
		return nil
	}
	return err
}

func (r *reader) close() {
	windows.CloseHandle(r.h)
	windows.CloseHandle(r.ov.HEvent)
}

func (b *treeBackend) watch(root string, _ []string) error {
	root = filepath.Clean(root)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.roots[root]; ok {
		return nil
	}
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return fmt.Errorf("watching %s: %w", root, err)
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return fmt.Errorf("watching %s: %w", root, err)
	}
	r := &reader{h: h, buf: make([]byte, 64<<10)}
	r.ov.HEvent = ev
	// Listen before returning, so a change made right after Watch is seen.
	if err := r.listen(); err != nil {
		r.close()
		return fmt.Errorf("watching %s: %w", root, err)
	}
	b.roots[root] = h
	go b.read(root, r)
	return nil
}

// read waits for changes under root until the watch is closed or fails.
// Which files changed doesn't matter, so the records aren't parsed; an empty
// result means the buffer overflowed, which is a change too.
func (b *treeBackend) read(root string, r *reader) {
	for {
		var n uint32
		err := windows.GetOverlappedResult(r.h, &r.ov, &n, true)
		b.mu.Lock()
		current := b.roots[root] == r.h
		if current && err == nil {
			err = r.listen()
		}
		if !current || err != nil {
			if current {
				// Let the next Watch call open the folder again.
				delete(b.roots, root)
			}
			b.mu.Unlock()
			r.close()
			if current && !errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
				b.changed()
				b.failed(fmt.Errorf("watching %s: %w", root, err))
			}
			return
		}
		b.mu.Unlock()
		b.changed()
	}
}

// close stops every watch. Each reader closes its own handle once its
// cancelled read returns, so a handle is never closed under a waiting read.
func (b *treeBackend) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for root, h := range b.roots {
		delete(b.roots, root)
		windows.CancelIoEx(h, nil)
	}
	return nil
}
