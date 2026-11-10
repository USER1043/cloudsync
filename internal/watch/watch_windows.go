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
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return fmt.Errorf("watching %s: %w", root, err)
	}
	b.roots[root] = h
	go b.read(root, h)
	return nil
}

// read waits for changes under root until the handle is closed or fails.
// Which files changed doesn't matter, so the records aren't parsed; an empty
// result means the buffer overflowed, which is a change too.
func (b *treeBackend) read(root string, h windows.Handle) {
	buf := make([]byte, 64<<10)
	for {
		var n uint32
		err := windows.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)), true, notifyMask, &n, nil, 0)
		if err != nil {
			b.mu.Lock()
			current := b.roots[root] == h
			if current {
				// Let the next Watch call open the folder again.
				delete(b.roots, root)
				windows.CloseHandle(h)
			}
			b.mu.Unlock()
			if current && !errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
				b.changed()
				b.failed(fmt.Errorf("watching %s: %w", root, err))
			}
			return
		}
		b.changed()
	}
}

func (b *treeBackend) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for root, h := range b.roots {
		delete(b.roots, root)
		windows.CancelIoEx(h, nil)
		windows.CloseHandle(h)
	}
	return nil
}
