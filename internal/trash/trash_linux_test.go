package trash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMoveToHomeTrash(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data")) // never the real trash
	can := filepath.Join(dir, "data", "Trash")

	write := func(rel, body string) string {
		p := filepath.Join(dir, "sync", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	first := write("notes 100%.txt", "one")
	if err := Move(first); err != nil {
		t.Fatal(err)
	}
	second := write("sub/notes 100%.txt", "two") // same name: must not overwrite the first
	if err := Move(second); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Dir(write("folder/inside.txt", "three"))
	if err := Move(folder); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{first, second, folder} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s is still in place", p)
		}
	}
	for name, body := range map[string]string{
		"notes 100%.txt":    "one",
		"notes 100%.txt.2":  "two",
		"folder/inside.txt": "three",
	} {
		b, err := os.ReadFile(filepath.Join(can, "files", name))
		if err != nil || string(b) != body {
			t.Errorf("trash/files/%s = %q, %v; want %q", name, b, err, body)
		}
	}
	info, err := os.ReadFile(filepath.Join(can, "info", "notes 100%.txt.2.trashinfo"))
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "Path=" + strings.ReplaceAll(second, " 100%", "%20100%25") + "\n"
	if !strings.HasPrefix(string(info), "[Trash Info]\n") || !strings.Contains(string(info), wantPath) ||
		!strings.Contains(string(info), "\nDeletionDate=") {
		t.Errorf("trashinfo =\n%s\nwant a [Trash Info] record with %q and a DeletionDate", info, wantPath)
	}
}

func TestMoveMissingFile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := Move(filepath.Join(t.TempDir(), "nope")); !os.IsNotExist(err) {
		t.Errorf("Move of a missing file: %v, want not-exist", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(os.Getenv("XDG_DATA_HOME"), "Trash", "info")); len(entries) != 0 {
		t.Errorf("a failed move left %d trash records", len(entries))
	}
}

func TestMountTop(t *testing.T) {
	top, err := mountTop(t.TempDir())
	if err != nil || !filepath.IsAbs(top) {
		t.Errorf("mountTop = %q, %v", top, err)
	}
}
