package localfs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/USER1043/cloudsync/internal/filter"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	write(t, root, "College/Sem5/notes.pdf", "pdf")
	write(t, root, "readme.txt", "hello")
	write(t, root, "Code/app/.git/HEAD", "ref: refs/heads/main")
	write(t, root, "Code/app/main.go", "package main")
	write(t, root, "Code/wt/.git", "gitdir: elsewhere") // worktree marker file
	write(t, root, "Code/site/node_modules/x/index.js", "x")
	write(t, root, "Code/site/index.html", "<p>")
	write(t, root, "College/~$draft.docx", "lock")
	write(t, root, "College/"+strings.Repeat("n", 235)+".txt", "long")

	m, err := filter.New(filter.DefaultIgnore)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Scan(root, m)
	if err != nil {
		t.Fatal(err)
	}

	var files []string
	for _, f := range res.Files {
		files = append(files, f.Rel)
	}
	wantFiles := []string{"Code/site/index.html", "College/Sem5/notes.pdf", "readme.txt"}
	if !slices.Equal(files, wantFiles) {
		t.Errorf("files = %v, want %v", files, wantFiles)
	}
	if want := []string{"Code/app", "Code/wt"}; !slices.Equal(res.Repos, want) {
		t.Errorf("repos = %v, want %v", res.Repos, want)
	}
	if want := []string{"Code/site/node_modules/", "College/~$draft.docx"}; !slices.Equal(res.Ignored, want) {
		t.Errorf("ignored = %v, want %v", res.Ignored, want)
	}
	if len(res.Issues) != 1 || !strings.Contains(res.Issues[0].Reason, "characters long") {
		t.Errorf("issues = %v, want one long-path issue", res.Issues)
	}
	if res.Files[2].Size != 5 {
		t.Errorf("readme.txt size = %d, want 5", res.Files[2].Size)
	}
}

func TestScanSymlink(t *testing.T) {
	root := t.TempDir()
	write(t, root, "real.txt", "x")
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	m, _ := filter.New(nil)
	res, err := Scan(root, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || len(res.Issues) != 1 || res.Issues[0].Rel != "link.txt" {
		t.Errorf("files=%v issues=%v, want real.txt synced and link.txt flagged", res.Files, res.Issues)
	}
}

func TestCheckPathCaseCollision(t *testing.T) {
	names := make(map[string]map[string]string)
	if r := checkPath(names, "Sem5/Notes.pdf", "Notes.pdf"); r != "" {
		t.Fatalf("first name flagged: %s", r)
	}
	if r := checkPath(names, "Sem5/notes.pdf", "notes.pdf"); !strings.Contains(r, "letter case") {
		t.Errorf("case collision not flagged, got %q", r)
	}
	if r := checkPath(names, "Sem6/notes.pdf", "notes.pdf"); r != "" {
		t.Errorf("same name in another folder flagged: %s", r)
	}
}

func TestScanMissingRoot(t *testing.T) {
	m, _ := filter.New(nil)
	if _, err := Scan(filepath.Join(t.TempDir(), "nope"), m); err == nil {
		t.Error("expected an error for a missing root")
	}
}

func TestMD5File(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.txt", "hello")
	got, err := MD5File(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "5d41402abc4b2a76b9719d911017c592"; got != want {
		t.Errorf("MD5File = %s, want %s", got, want)
	}
}
