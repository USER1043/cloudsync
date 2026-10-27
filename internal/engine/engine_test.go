package engine

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/localfs"
	"github.com/USER1043/cloudsync/internal/plan"
	"github.com/USER1043/cloudsync/internal/state"
)

// fakeDrive is an in-memory Drive.
type fakeDrive struct {
	mu    sync.Mutex
	n     int
	nodes map[string]*node
}

type node struct {
	parent, name string
	folder       bool
	data         []byte
	mod          time.Time
}

func newFakeDrive() *fakeDrive {
	return &fakeDrive{nodes: map[string]*node{"root": {folder: true}}}
}

func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func (f *fakeDrive) add(n *node) string {
	f.n++
	id := fmt.Sprintf("id%d", f.n)
	f.nodes[id] = n
	return id
}

func (f *fakeDrive) EnsureFolder(_ context.Context, parentID, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, n := range f.nodes {
		if n.folder && n.parent == parentID && n.name == name {
			return id, nil
		}
	}
	return f.add(&node{parent: parentID, name: name, folder: true}), nil
}

func (f *fakeDrive) Upload(_ context.Context, parentID, name, localPath string, mod time.Time, _ string, progress func(int64)) (gdrive.File, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return gdrive.File{}, err
	}
	if progress != nil {
		progress(int64(len(data)) / 2)
		progress(int64(len(data)))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.add(&node{parent: parentID, name: name, data: data, mod: mod})
	return gdrive.File{ID: id, Rel: name, MD5: md5hex(data), Size: int64(len(data)), ModTime: mod}, nil
}

func (f *fakeDrive) Download(_ context.Context, id string, w io.Writer) error {
	f.mu.Lock()
	n := f.nodes[id]
	f.mu.Unlock()
	_, err := w.Write(n.data)
	return err
}

// put stores a file at rel under rootID, creating folders on the way.
func (f *fakeDrive) put(t *testing.T, rootID, rel, body string, mod time.Time) {
	t.Helper()
	parts := strings.Split(rel, "/")
	parent := rootID
	for _, dir := range parts[:len(parts)-1] {
		parent, _ = f.EnsureFolder(context.Background(), parent, dir)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.add(&node{parent: parent, name: parts[len(parts)-1], data: []byte(body), mod: mod})
}

// tree lists everything under rootID, like gdrive.Client.ListTree.
func (f *fakeDrive) tree(rootID string) *gdrive.Tree {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &gdrive.Tree{}
	var walk func(id, rel string)
	walk = func(id, rel string) {
		for cid, n := range f.nodes {
			if n.parent != id || cid == "root" {
				continue
			}
			r := n.name
			if rel != "" {
				r = rel + "/" + n.name
			}
			if n.folder {
				t.Folders = append(t.Folders, gdrive.File{ID: cid, Rel: r})
				walk(cid, r)
			} else {
				t.Files = append(t.Files, gdrive.File{ID: cid, Rel: r, MD5: md5hex(n.data), Size: int64(len(n.data)), ModTime: n.mod})
			}
		}
	}
	walk(rootID, "")
	return t
}

func (f *fakeDrive) content(rootID string) map[string]string {
	out := make(map[string]string)
	for _, file := range f.tree(rootID).Files {
		out[file.Rel] = string(f.nodes[file.ID].data)
	}
	return out
}

type fixture struct {
	t      *testing.T
	drive  *fakeDrive
	rootID string
	local  string
	db     *state.DB
}

func newFixture(t *testing.T) *fixture {
	d := newFakeDrive()
	rootID, _ := d.EnsureFolder(context.Background(), "root", "CloudSync")
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &fixture{t: t, drive: d, rootID: rootID, local: t.TempDir(), db: db}
}

func (fx *fixture) write(rel, body string) {
	p := filepath.Join(fx.local, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fx.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixture) read(rel string) string {
	b, err := os.ReadFile(filepath.Join(fx.local, filepath.FromSlash(rel)))
	if err != nil {
		fx.t.Fatal(err)
	}
	return string(b)
}

// sync plans and runs a first sync, the way `cloudsync sync` does.
func (fx *fixture) sync() (*Result, *plan.Plan) {
	fx.t.Helper()
	m, _ := filter.New(filter.DefaultIgnore)
	scan, err := localfs.Scan(fx.local, m)
	if err != nil {
		fx.t.Fatal(err)
	}
	tree := fx.drive.tree(fx.rootID)
	p := plan.FirstSync(scan.Files, tree.Files, func(rel string) (string, error) {
		return localfs.MD5File(filepath.Join(fx.local, filepath.FromSlash(rel)))
	})
	var last Progress
	calls := 0
	res, err := FirstSync(context.Background(), fx.drive, fx.db,
		Options{Root: "MyFiles", Local: fx.local, RootID: fx.rootID, Label: "Windows",
			Progress: func(pr Progress) {
				if pr.Bytes < last.Bytes || pr.Bytes > pr.TotalBytes {
					fx.t.Errorf("progress went from %+v to %+v", last, pr)
				}
				last = pr
				calls++
			}}, scan, tree, p)
	if err != nil {
		fx.t.Fatal(err)
	}
	if calls < last.Total+1 || last.Done != last.Total || last.Bytes != last.TotalBytes {
		fx.t.Errorf("progress: %d calls, last %+v; want a start call plus one per transfer, ending complete", calls, last)
	}
	return res, p
}

func TestUploadsEverythingToEmptyDrive(t *testing.T) {
	fx := newFixture(t)
	fx.write("a.txt", "alpha")
	fx.write("Sem5/DBMS/notes.pdf", "pdf bytes")
	if err := os.MkdirAll(filepath.Join(fx.local, "Sem8"), 0o755); err != nil { // empty folder
		t.Fatal(err)
	}

	res, _ := fx.sync()
	if res.Uploaded != 2 || res.Folders != 3 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	want := map[string]string{"a.txt": "alpha", "Sem5/DBMS/notes.pdf": "pdf bytes"}
	if got := fx.drive.content(fx.rootID); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Drive has %v, want %v", got, want)
	}
	if len(fx.drive.tree(fx.rootID).Folders) != 3 {
		t.Error("empty folder Sem8 was not created in Drive")
	}
	files, dirs, _ := fx.db.Count(context.Background(), "MyFiles")
	if files != 2 || dirs != 3 {
		t.Errorf("state has %d files and %d dirs, want 2 and 3", files, dirs)
	}
	it, _, _ := fx.db.Get(context.Background(), "MyFiles", "a.txt")
	if it.MD5 != md5hex([]byte("alpha")) || it.DriveID == "" {
		t.Errorf("state for a.txt = %+v", it)
	}

	// Running again finds everything in sync and transfers nothing.
	res, _ = fx.sync()
	if res.InSync != 2 || res.Uploaded != 0 || res.Folders != 0 {
		t.Errorf("second run = %+v, want everything in sync", res)
	}
}

func TestDownloadsEverythingToEmptyFolder(t *testing.T) {
	fx := newFixture(t)
	mod := time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)
	fx.drive.put(t, fx.rootID, "Sem5/OS.pptx", "slides", mod)
	fx.drive.put(t, fx.rootID, "readme.txt", "hi", mod)
	fx.drive.EnsureFolder(context.Background(), fx.rootID, "EmptyInDrive")

	res, _ := fx.sync()
	if res.Downloaded != 2 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := fx.read("Sem5/OS.pptx"); got != "slides" {
		t.Errorf("OS.pptx = %q", got)
	}
	fi, err := os.Stat(filepath.Join(fx.local, "readme.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mod) {
		t.Errorf("modified time = %v, want Drive's %v", fi.ModTime(), mod)
	}
	if _, err := os.Stat(filepath.Join(fx.local, "EmptyInDrive")); err != nil {
		t.Error("empty Drive folder was not created locally")
	}
	matches, _ := filepath.Glob(filepath.Join(fx.local, "Sem5", "*.cloudsync-partial"))
	if len(matches) != 0 {
		t.Errorf("leftover partial files: %v", matches)
	}
}

func TestConflictKeepsBothCopies(t *testing.T) {
	fx := newFixture(t)
	fx.write("report.docx", "my version")
	fx.drive.put(t, fx.rootID, "report.docx", "their version!", time.Now())
	fx.write("same.txt", "identical")
	fx.drive.put(t, fx.rootID, "same.txt", "identical", time.Now())

	res, _ := fx.sync()
	if res.Conflicts != 1 || res.InSync != 1 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := fx.read("report.docx"); got != "their version!" {
		t.Errorf("report.docx = %q, want Drive's version", got)
	}
	copies, _ := filepath.Glob(filepath.Join(fx.local, "report (conflict Windows *).docx"))
	if len(copies) != 1 {
		t.Fatalf("conflict copies = %v, want one", copies)
	}
	if b, _ := os.ReadFile(copies[0]); string(b) != "my version" {
		t.Errorf("conflict copy = %q, want the local version", b)
	}
	if got := fx.drive.content(fx.rootID)[filepath.Base(copies[0])]; got != "my version" {
		t.Errorf("Drive copy of the conflict file = %q", got)
	}
}

func TestDownloadNeverOverwrites(t *testing.T) {
	fx := newFixture(t)
	fx.drive.put(t, fx.rootID, "late.txt", "from drive", time.Now())
	m, _ := filter.New(nil)
	scan, _ := localfs.Scan(fx.local, m)
	tree := fx.drive.tree(fx.rootID)
	p := plan.FirstSync(scan.Files, tree.Files, nil)

	fx.write("late.txt", "created after planning") // appears between plan and sync
	res, err := FirstSync(context.Background(), fx.drive, fx.db, Options{Root: "MyFiles", Local: fx.local, RootID: fx.rootID}, scan, tree, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 1 || fx.read("late.txt") != "created after planning" {
		t.Errorf("failed = %v, local = %q; want the download refused", res.Failed, fx.read("late.txt"))
	}
}

func TestConflictName(t *testing.T) {
	fx := newFixture(t)
	s := &run{opt: Options{Local: fx.local, Label: "Linux"}, now: time.Date(2026, 10, 27, 14, 32, 0, 0, time.Local),
		remote: map[string]gdrive.File{"Sem5/a (conflict Linux 2026-10-27 1432).pdf": {}}}
	if got, want := s.conflictName("Sem5/a.pdf"), "Sem5/a (conflict Linux 2026-10-27 1432) 2.pdf"; got != want {
		t.Errorf("conflictName = %q, want %q", got, want)
	}
	if got, want := s.conflictName("README"), "README (conflict Linux 2026-10-27 1432)"; got != want {
		t.Errorf("conflictName = %q, want %q", got, want)
	}
}
