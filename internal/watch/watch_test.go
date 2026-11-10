package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitFor(t *testing.T, w *Watcher, what string) {
	t.Helper()
	select {
	case <-w.C():
	case err := <-w.Errors():
		t.Fatalf("%s: watcher error: %v", what, err)
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no change reported", what)
	}
}

// drain empties C after changes that the test isn't checking.
func drain(w *Watcher) {
	for {
		select {
		case <-w.C():
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}

func TestWatch(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Watch(root, []string{sub, filepath.Join(root, "gone")}); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, "new file in the root")
	drain(w)

	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, "new file in a subfolder")
	drain(w)

	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("bb"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, "edited file")
	drain(w)

	if err := os.Remove(filepath.Join(sub, "b.txt")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, w, "deleted file")
	drain(w)

	// Folders dropped from the list are no longer watched, and a folder
	// removed from disk is no problem.
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	drain(w)
	if err := w.Watch(root, nil); err != nil {
		t.Fatalf("Watch after removing a folder: %v", err)
	}
}

func TestChangedMerges(t *testing.T) {
	w := &Watcher{c: make(chan struct{}, 1), errs: make(chan error, 1)}
	w.changed()
	w.changed() // must not block
	<-w.C()
	select {
	case <-w.C():
		t.Fatal("two changes before a receive should be one signal")
	default:
	}
}
