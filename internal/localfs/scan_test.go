package localfs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/winname"
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
	res, err := Scan(root, m, nil)
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
	if want := []string{"Code", "Code/site", "College", "College/Sem5"}; !slices.Equal(res.Dirs, want) {
		t.Errorf("dirs = %v, want %v", res.Dirs, want)
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

func TestScanIgnoreFilesAndSizeLimit(t *testing.T) {
	root := t.TempDir()
	write(t, root, filter.IgnoreFile, "/Videos/\n")
	write(t, root, "Videos/lecture.mp4", "v")
	write(t, root, "Sub/"+filter.IgnoreFile, "*.log\n")
	write(t, root, "Sub/run.log", "log")
	write(t, root, "Sub/keep.txt", "k")
	write(t, root, "Other/run.log", "not covered by Sub's rules")
	write(t, root, "big.bin", strings.Repeat("x", 2000))

	m, _ := filter.New(nil)
	m.SetMaxSize(1000)
	res, err := Scan(root, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, f := range res.Files {
		files = append(files, f.Rel)
	}
	// Ignore files themselves sync, so the rules apply on every computer.
	wantFiles := []string{filter.IgnoreFile, "Other/run.log", "Sub/" + filter.IgnoreFile, "Sub/keep.txt"}
	if !slices.Equal(files, wantFiles) {
		t.Errorf("files = %v, want %v", files, wantFiles)
	}
	if want := []string{"Sub/run.log", "Videos/"}; !slices.Equal(res.Ignored, want) {
		t.Errorf("ignored = %v, want %v", res.Ignored, want)
	}
	if len(res.TooLarge) != 1 || res.TooLarge[0].Rel != "big.bin" {
		t.Errorf("too large = %v, want big.bin", res.TooLarge)
	}
	if !res.Matcher.Match("Sub/x.log", false) || m.Match("Sub/x.log", false) {
		t.Error("result matcher should carry Sub's rules without changing the caller's matcher")
	}
}

func TestScanBadIgnoreFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Sub/"+filter.IgnoreFile, "[unclosed\n")
	m, _ := filter.New(nil)
	if _, err := Scan(root, m, nil); err == nil || !strings.Contains(err.Error(), "Sub/.cloudsyncignore line 1") {
		t.Errorf("err = %v, want it to name the broken rule", err)
	}
}

func TestScanSymlink(t *testing.T) {
	root := t.TempDir()
	write(t, root, "real.txt", "x")
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	m, _ := filter.New(nil)
	res, err := Scan(root, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || len(res.Issues) != 1 || res.Issues[0].Rel != "link.txt" {
		t.Errorf("files=%v issues=%v, want real.txt synced and link.txt flagged", res.Files, res.Issues)
	}
}

func TestScanKeepsWindowsOnlyNames(t *testing.T) {
	// Linux can hold names Windows can't, and names that differ only by
	// case. They all sync; Windows gives them stand-ins later.
	root := t.TempDir()
	for _, rel := range []string{"Hackathon /plan.pdf", "Paper:1.pdf", "Notes.txt", "notes.txt", "con.txt"} {
		write(t, root, rel, "x")
	}
	m, _ := filter.New(nil)
	res, err := Scan(root, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, f := range res.Files {
		rels = append(rels, f.Rel)
	}
	slices.Sort(rels)
	if want := []string{"Hackathon /plan.pdf", "Notes.txt", "Paper:1.pdf", "con.txt", "notes.txt"}; !slices.Equal(rels, want) || len(res.Issues) != 0 {
		t.Errorf("files = %q, issues = %v; want %q and no issues", rels, res.Issues, want)
	}
}

func TestScanTranslatesStandIns(t *testing.T) {
	// On Windows, stand-ins on disk come back as their true names, while a
	// file really named with a full-width character keeps its name.
	root := t.TempDir()
	write(t, root, "Hackathon␣/Paper：1.pdf", "x")
	write(t, root, "Hackathon␣/Real：Name.pdf", "x")
	write(t, root, "Hackathon␣/skip.iso", "x")
	names := winname.New(map[string]string{
		"Hackathon ":             "Hackathon␣",
		"Hackathon /Paper:1.pdf": "Paper：1.pdf",
	})
	m, _ := filter.New([]string{"/Hackathon /*.iso"}) // rules use true paths
	res, err := Scan(root, m, names)
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, f := range res.Files {
		rels = append(rels, f.Rel)
	}
	slices.Sort(rels)
	if want := []string{"Hackathon /Paper:1.pdf", "Hackathon /Real：Name.pdf"}; !slices.Equal(rels, want) {
		t.Errorf("files = %q, want %q", rels, want)
	}
	if !slices.Equal(res.Dirs, []string{"Hackathon "}) || !slices.Equal(res.Ignored, []string{"Hackathon /skip.iso"}) {
		t.Errorf("dirs = %q, ignored = %q", res.Dirs, res.Ignored)
	}
	// The scan told the map which local names are taken.
	if got := names.Assign("Hackathon /real：name.pdf"); got != "Hackathon␣/real：name (case 2).pdf" {
		t.Errorf("Assign after scan = %q", got)
	}
}

func TestScanMissingRoot(t *testing.T) {
	m, _ := filter.New(nil)
	if _, err := Scan(filepath.Join(t.TempDir(), "nope"), m, nil); err == nil {
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
