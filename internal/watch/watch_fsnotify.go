//go:build !windows

package watch

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// notifyBackend watches every folder separately, the only way inotify can.
type notifyBackend struct {
	fw *fsnotify.Watcher

	mu    sync.Mutex
	roots map[string]map[string]bool // root -> folders watched for it
}

func newBackend(changed func(), failed func(error)) (backend, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("starting the folder watcher: %w", err)
	}
	b := &notifyBackend{fw: fw, roots: map[string]map[string]bool{}}
	go func() {
		for {
			select {
			case ev, ok := <-fw.Events:
				if !ok {
					return
				}
				// Permission and timestamp changes alone change no content.
				if ev.Op != fsnotify.Chmod {
					changed()
				}
			case err, ok := <-fw.Errors:
				if !ok {
					return
				}
				// Too many changes at once: some were lost, so rescan.
				changed()
				if !errors.Is(err, fsnotify.ErrEventOverflow) {
					failed(err)
				}
			}
		}
	}()
	return b, nil
}

func (b *notifyBackend) watch(root string, dirs []string) error {
	want := map[string]bool{filepath.Clean(root): true}
	for _, d := range dirs {
		want[filepath.Clean(d)] = true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	have := b.roots[root]
	if have == nil {
		have = map[string]bool{}
		b.roots[root] = have
	}
	for d := range have {
		if !want[d] {
			_ = b.fw.Remove(d) // fails if the folder is gone, which drops the watch anyway
			delete(have, d)
		}
	}
	var first error
	for d := range want {
		if have[d] {
			continue
		}
		switch err := b.fw.Add(d); {
		case err == nil:
			have[d] = true
		case errors.Is(err, fs.ErrNotExist):
		case first == nil:
			first = fmt.Errorf("watching %s: %w", d, err)
		}
	}
	return first
}

func (b *notifyBackend) close() error { return b.fw.Close() }
