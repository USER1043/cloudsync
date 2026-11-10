package engine

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/localfs"
	"github.com/USER1043/cloudsync/internal/plan"
	"github.com/USER1043/cloudsync/internal/reconcile"
	"github.com/USER1043/cloudsync/internal/state"
	"github.com/USER1043/cloudsync/internal/winname"
)

// fakeDrive is an in-memory Drive.
type fakeDrive struct {
	mu    sync.Mutex
	n     int
	nodes map[string]*node

	lookups, creates int      // folder requests, as the real Drive would see them
	dupes            int      // CreateFolder calls where the folder already existed
	uploads          []string // uploaded names, in the order they started
	updates          int      // Update calls that replaced content
}

type node struct {
	parent, name string
	folder       bool
	data         []byte
	mod          time.Time
	trashed      bool
	native       bool // a Google Doc: listed in Tree.Native, never synced
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
	f.lookups++
	if id := f.findFolder(parentID, name); id != "" {
		return id, nil
	}
	f.creates++
	return f.add(&node{parent: parentID, name: name, folder: true}), nil
}

func (f *fakeDrive) CreateFolder(_ context.Context, parentID, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findFolder(parentID, name) != "" {
		f.dupes++ // the real Drive would now hold two folders of that name
	}
	f.creates++
	return f.add(&node{parent: parentID, name: name, folder: true}), nil
}

func (f *fakeDrive) findFolder(parentID, name string) string {
	for id, n := range f.nodes {
		if n.folder && n.parent == parentID && n.name == name {
			return id
		}
	}
	return ""
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
	f.uploads = append(f.uploads, name)
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

func (f *fakeDrive) Update(_ context.Context, id, localPath string, mod time.Time, md5sum, want string, progress func(int64)) (gdrive.File, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return gdrive.File{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodes[id]
	cur := gdrive.File{ID: id, Rel: n.name, MD5: md5hex(n.data), Size: int64(len(n.data)), ModTime: n.mod, Trashed: n.trashed}
	if n.trashed || cur.MD5 != want {
		return cur, gdrive.ErrChanged
	}
	if progress != nil {
		progress(int64(len(data)))
	}
	f.updates++
	n.data, n.mod = data, mod
	return gdrive.File{ID: id, Rel: n.name, MD5: md5hex(data), Size: int64(len(data)), ModTime: mod}, nil
}

func (f *fakeDrive) Trash(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.nodes[id]; n != nil {
		n.trashed = true
	}
	return nil
}

func (f *fakeDrive) HasChildren(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.nodes {
		if n.parent == id && !n.trashed {
			return true, nil
		}
	}
	return false, nil
}

// find returns the ID of the item at rel under rootID, or "".
func (f *fakeDrive) find(rootID, rel string) string {
	t := f.tree(rootID)
	for _, x := range append(t.Files, t.Folders...) {
		if x.Rel == rel {
			return x.ID
		}
	}
	return ""
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
			if n.parent != id || cid == "root" || n.trashed {
				continue
			}
			r := n.name
			if rel != "" {
				r = rel + "/" + n.name
			}
			switch {
			case n.folder:
				t.Folders = append(t.Folders, gdrive.File{ID: cid, Rel: r})
				walk(cid, r)
			case n.native:
				t.Native = append(t.Native, gdrive.File{ID: cid, Rel: r})
			default:
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

	// windows makes sync act like CloudSync on Windows: names Windows
	// can't store get stand-ins, loaded from the state DB on each run.
	windows bool
	workers int // engine.Options.Workers; 0 for the default

	trashDir string   // the fake OS trash used by resync
	trashed  []string // names moved there, in order
	trashMu  sync.Mutex
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
	var names *winname.Map
	if fx.windows {
		standIns, err := fx.db.StandIns(context.Background(), "MyFiles")
		if err != nil {
			fx.t.Fatal(err)
		}
		names = winname.New(standIns)
	}
	scan, err := localfs.Scan(fx.local, m, names)
	if err != nil {
		fx.t.Fatal(err)
	}
	tree := fx.drive.tree(fx.rootID)
	p := plan.FirstSync(scan.Files, tree.Files, func(rel string) (string, error) {
		return localfs.MD5File(filepath.Join(fx.local, filepath.FromSlash(names.Local(rel))))
	})
	var last Progress
	calls := 0
	res, err := FirstSync(context.Background(), fx.drive, fx.db,
		Options{Root: "MyFiles", Local: fx.local, RootID: fx.rootID, Label: "Windows", Names: names, Workers: fx.workers,
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
	scan, _ := localfs.Scan(fx.local, m, nil)
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

func TestUploadsNamesWindowsCannotStore(t *testing.T) {
	// On Linux, such names go to Drive exactly as they are.
	if runtime.GOOS == "windows" {
		t.Skip("Windows can't store these names")
	}
	fx := newFixture(t)
	fx.write("Hackathon /Paper:1.pdf", "paper")
	fx.write("Notes.txt", "upper")
	fx.write("notes.txt", "lower")

	res, _ := fx.sync()
	if res.Uploaded != 3 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	want := map[string]string{"Hackathon /Paper:1.pdf": "paper", "Notes.txt": "upper", "notes.txt": "lower"}
	if got := fx.drive.content(fx.rootID); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Drive has %v, want %v", got, want)
	}
	if standIns, _ := fx.db.StandIns(context.Background(), "MyFiles"); len(standIns) != 0 {
		t.Errorf("Linux recorded stand-ins: %v", standIns)
	}
}

func TestWindowsStandIns(t *testing.T) {
	fx := newFixture(t)
	fx.windows = true
	now := time.Now()
	fx.drive.put(t, fx.rootID, "Hackathon /Paper:1.pdf", "paper", now)
	fx.drive.put(t, fx.rootID, "Notes.txt", "upper", now)
	fx.drive.put(t, fx.rootID, "notes.txt", "lower", now)
	fx.drive.put(t, fx.rootID, "con.txt", "device", now)
	fx.drive.put(t, fx.rootID, "report.docx", "theirs", now)
	fx.write("Real：Name.pdf", "a real full-width colon")
	fx.write("report.docx", "mine")
	fx.write("Local/only.txt", "local")

	res, _ := fx.sync()
	if res.Downloaded != 4 || res.Conflicts != 1 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	for rel, want := range map[string]string{
		"Hackathon␣/Paper：1.pdf": "paper",
		"Notes.txt":              "upper",
		"notes (case 2).txt":     "lower",
		"con＿.txt":               "device",
		"report.docx":            "theirs",
		"Real：Name.pdf":          "a real full-width colon",
	} {
		if got := fx.read(rel); got != want {
			t.Errorf("local %s = %q, want %q", rel, got, want)
		}
	}
	// The local-only file and the conflict copy reach Drive under true names.
	drive := fx.drive.content(fx.rootID)
	if drive["Real：Name.pdf"] != "a real full-width colon" || drive["Local/only.txt"] != "local" || len(drive) != 8 {
		t.Errorf("Drive has %v", drive)
	}
	standIns, _ := fx.db.StandIns(context.Background(), "MyFiles")
	want := map[string]string{
		"Hackathon ":             "Hackathon␣",
		"Hackathon /Paper:1.pdf": "Paper：1.pdf",
		"notes.txt":              "notes (case 2).txt",
		"con.txt":                "con＿.txt",
	}
	if fmt.Sprint(standIns) != fmt.Sprint(want) {
		t.Errorf("stand-ins = %q, want %q", standIns, want)
	}

	// A second run maps every stand-in back to its true name, so nothing is
	// uploaded twice or downloaded again.
	res, p := fx.sync()
	if res.Uploaded != 0 || res.Downloaded != 0 || res.Conflicts != 0 || res.InSync != len(p.Items) {
		t.Errorf("second run = %+v, plan %+v; want everything in sync", res, p.Items)
	}

	// A new file inside a stand-in folder goes to the true folder in Drive.
	fx.write("Hackathon␣/new.txt", "new")
	if res, _ := fx.sync(); res.Uploaded != 1 || fx.drive.content(fx.rootID)["Hackathon /new.txt"] != "new" {
		t.Errorf("third run = %+v, Drive has %v", res, fx.drive.content(fx.rootID))
	}
}

func TestFolderCreation(t *testing.T) {
	fx := newFixture(t)
	fx.drive.EnsureFolder(context.Background(), fx.rootID, "Existing")
	for _, rel := range []string{"Existing/New/a.txt", "Sem5/DBMS/Lab1/q.sql", "Sem5/DBMS/notes.pdf", "Sem5/OS/x.pdf", "Sem6/y.pdf"} {
		fx.write(rel, "x")
	}
	fx.drive.lookups, fx.drive.creates = 0, 0

	res, _ := fx.sync()
	if res.Uploaded != 5 || res.Folders != 6 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	// Only folders whose parent existed before this run need a lookup:
	// Existing/New, Sem5 and Sem6. Sem5/DBMS, Sem5/DBMS/Lab1 and Sem5/OS
	// sit inside folders created in this run and skip it.
	if fx.drive.lookups != 3 || fx.drive.creates != 6 || fx.drive.dupes != 0 {
		t.Errorf("%d lookups, %d creates, %d duplicates; want 3, 6, 0", fx.drive.lookups, fx.drive.creates, fx.drive.dupes)
	}
	if got := fx.drive.content(fx.rootID); len(got) != 5 || got["Sem5/DBMS/Lab1/q.sql"] != "x" {
		t.Errorf("Drive has %v", got)
	}
}

func TestLargestFilesFirst(t *testing.T) {
	fx := newFixture(t)
	fx.workers = 1 // one at a time, so the start order is the queue order
	for name, size := range map[string]int{"small.txt": 1, "big.bin": 300, "mid.pdf": 20, "tiny.md": 0} {
		fx.write(name, strings.Repeat("x", size))
	}
	fx.sync()
	if got, want := strings.Join(fx.drive.uploads, " "), "big.bin mid.pdf small.txt tiny.md"; got != want {
		t.Errorf("upload order = %s, want %s", got, want)
	}
}

// edit replaces the content of the Drive file at rel, as the phone would.
func (f *fakeDrive) edit(t *testing.T, rootID, rel, body string) {
	t.Helper()
	id := f.find(rootID, rel)
	if id == "" {
		t.Fatalf("no Drive file %s", rel)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[id].data, f.nodes[id].mod = []byte(body), time.Now()
}

func (f *fakeDrive) trash(t *testing.T, rootID, rel string) {
	t.Helper()
	id := f.find(rootID, rel)
	if id == "" {
		t.Fatalf("no Drive item %s", rel)
	}
	f.Trash(context.Background(), id)
}

func (fx *fixture) remove(rel string) {
	if err := os.RemoveAll(filepath.Join(fx.local, filepath.FromSlash(rel))); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixture) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(fx.local, filepath.FromSlash(rel)))
	return err == nil
}

// edit rewrites a local file with a later modification time, as an editor
// that saves within the same second would still leave a newer one.
func (fx *fixture) edit(rel, body string) {
	fx.write(rel, body)
	p := filepath.Join(fx.local, filepath.FromSlash(rel))
	later := time.Now().Add(time.Duration(len(body)+1) * time.Second)
	if err := os.Chtimes(p, later, later); err != nil {
		fx.t.Fatal(err)
	}
}

// resync plans a later sync the way `cloudsync run` will, runs between, if
// set, between planning and running, and runs it with a fake OS trash.
func (fx *fixture) resync(between func()) (*Result, *reconcile.Plan) {
	fx.t.Helper()
	ctx := context.Background()
	m, _ := filter.New(filter.DefaultIgnore)
	scan, err := localfs.Scan(fx.local, m, nil)
	if err != nil {
		fx.t.Fatal(err)
	}
	tree := fx.drive.tree(fx.rootID)
	base, err := fx.db.Items(ctx, "MyFiles")
	if err != nil {
		fx.t.Fatal(err)
	}
	p := reconcile.Reconcile(reconcile.Input{
		Local: scan.Files, LocalDirs: scan.Dirs, Remote: tree.Files, RemoteDirs: tree.Folders, Base: base,
		Hash: func(rel string) (string, error) {
			return localfs.MD5File(filepath.Join(fx.local, filepath.FromSlash(rel)))
		},
	})
	if between != nil {
		between()
	}
	if fx.trashDir == "" {
		fx.trashDir = fx.t.TempDir()
	}
	res, err := Sync(ctx, fx.drive, fx.db, Options{
		Root: "MyFiles", Local: fx.local, RootID: fx.rootID, Label: "Linux",
		Trash: func(p string) error {
			fx.trashMu.Lock() // called by parallel workers
			defer fx.trashMu.Unlock()
			fx.trashed = append(fx.trashed, filepath.Base(p))
			return os.Rename(p, filepath.Join(fx.trashDir, filepath.Base(p)))
		},
	}, scan, tree, p)
	if err != nil {
		fx.t.Fatal(err)
	}
	return res, p
}

// synced makes a fixture whose first sync is done, with files on both sides.
func synced(t *testing.T, files map[string]string) *fixture {
	fx := newFixture(t)
	for rel, body := range files {
		fx.write(rel, body)
	}
	if res, _ := fx.sync(); len(res.Failed) != 0 {
		t.Fatalf("first sync failed: %v", res.Failed)
	}
	return fx
}

func TestSyncNothingChanged(t *testing.T) {
	fx := synced(t, map[string]string{"a.txt": "alpha", "Sem5/b.pdf": "beta"})
	res, p := fx.resync(nil)
	if len(p.Actions) != 0 || res.Uploaded+res.Downloaded+res.Trashed+res.TrashedInDrive != 0 {
		t.Errorf("plan %+v, result %+v; want nothing to do", p.Actions, res)
	}
}

func TestSyncLocalEditUpdatesInPlace(t *testing.T) {
	fx := synced(t, map[string]string{"a.txt": "alpha"})
	id := fx.drive.find(fx.rootID, "a.txt")
	fx.edit("a.txt", "alpha, edited")

	res, _ := fx.resync(nil)
	if res.Uploaded != 1 || len(res.Failed) != 0 || fx.drive.updates != 1 {
		t.Fatalf("result %+v, %d updates", res, fx.drive.updates)
	}
	if got := fx.drive.find(fx.rootID, "a.txt"); got != id {
		t.Errorf("Drive ID changed from %s to %s; the file should be updated in place", id, got)
	}
	if got := fx.drive.content(fx.rootID)["a.txt"]; got != "alpha, edited" {
		t.Errorf("Drive has %q", got)
	}
	if res, p := fx.resync(nil); len(p.Actions) != 0 {
		t.Errorf("after the update, the next run plans %+v (result %+v)", p.Actions, res)
	}
}

func TestSyncDriveEditDownloads(t *testing.T) {
	fx := synced(t, map[string]string{"a.txt": "alpha"})
	fx.drive.edit(t, fx.rootID, "a.txt", "edited on the phone")

	res, _ := fx.resync(nil)
	if res.Downloaded != 1 || len(res.Failed) != 0 || fx.read("a.txt") != "edited on the phone" {
		t.Errorf("result %+v, local %q", res, fx.read("a.txt"))
	}
	if _, p := fx.resync(nil); len(p.Actions) != 0 {
		t.Errorf("after the download, the next run plans %+v", p.Actions)
	}
}

func TestSyncDeletes(t *testing.T) {
	fx := synced(t, map[string]string{"here.txt": "1", "there.txt": "2", "keep.txt": "3"})
	fx.remove("here.txt")                     // deleted on this computer
	fx.drive.trash(t, fx.rootID, "there.txt") // deleted on the phone

	res, _ := fx.resync(nil)
	if res.Trashed != 1 || res.TrashedInDrive != 1 || len(res.Failed) != 0 {
		t.Fatalf("result %+v", res)
	}
	if _, ok := fx.drive.content(fx.rootID)["here.txt"]; ok {
		t.Error("here.txt is still in Drive")
	}
	if fx.exists("there.txt") || fmt.Sprint(fx.trashed) != "[there.txt]" {
		t.Errorf("there.txt: exists %v; trashed %v, want it moved to the OS trash", fx.exists("there.txt"), fx.trashed)
	}
	files, _, _ := fx.db.Count(context.Background(), "MyFiles")
	if files != 1 {
		t.Errorf("state has %d files, want only keep.txt", files)
	}
}

func TestSyncFolderDeletedInDrive(t *testing.T) {
	fx := synced(t, map[string]string{"Old/a.txt": "1", "Old/Sub/b.txt": "2", "x.txt": "3"})
	fx.write("Old/.env", "ignored, never synced") // must not be trashed with the folder
	fx.drive.trash(t, fx.rootID, "Old")

	res, _ := fx.resync(nil)
	if res.Trashed != 2 || len(res.Failed) != 0 {
		t.Fatalf("result %+v", res)
	}
	if fx.exists("Old/Sub") || !fx.exists("Old/.env") {
		t.Errorf("Old/Sub exists %v (want removed), Old/.env exists %v (want kept)", fx.exists("Old/Sub"), fx.exists("Old/.env"))
	}
	if res.FoldersRemoved != 1 {
		t.Errorf("removed %d folders, want only Old/Sub; Old holds an ignored file", res.FoldersRemoved)
	}
}

func TestSyncFolderDeletedHereKeepsGoogleDocs(t *testing.T) {
	fx := synced(t, map[string]string{"Proj/a.txt": "1", "Empty/b.txt": "2"})
	projID := fx.drive.find(fx.rootID, "Proj")
	fx.drive.mu.Lock()
	fx.drive.add(&node{parent: projID, name: "Plan", native: true}) // a Google Doc made on the phone
	fx.drive.mu.Unlock()
	fx.remove("Proj")
	fx.remove("Empty")

	res, _ := fx.resync(nil)
	if res.TrashedInDrive != 2 || res.FoldersRemoved != 1 || len(res.Failed) != 0 {
		t.Fatalf("result %+v", res)
	}
	if fx.drive.find(fx.rootID, "Proj") == "" {
		t.Error("Proj was trashed in Drive along with the Google Doc inside it")
	}
	if fx.drive.find(fx.rootID, "Empty") != "" {
		t.Error("the emptied folder Empty is still in Drive")
	}
}

func TestSyncConflictAfterBothEdited(t *testing.T) {
	fx := synced(t, map[string]string{"r.docx": "v1"})
	fx.edit("r.docx", "laptop edit")
	fx.drive.edit(t, fx.rootID, "r.docx", "phone edit")

	res, _ := fx.resync(nil)
	if res.Conflicts != 1 || len(res.Failed) != 0 || fx.read("r.docx") != "phone edit" {
		t.Fatalf("result %+v, local %q", res, fx.read("r.docx"))
	}
	copies, _ := filepath.Glob(filepath.Join(fx.local, "r (conflict Linux *).docx"))
	if len(copies) != 1 {
		t.Fatalf("conflict copies %v", copies)
	}
	if got := fx.drive.content(fx.rootID)[filepath.Base(copies[0])]; got != "laptop edit" {
		t.Errorf("Drive copy of the laptop edit = %q", got)
	}
}

func TestSyncNeverOverwritesNewerDriveEdit(t *testing.T) {
	// The phone edits the file after Drive was listed, just before the
	// upload: the update must turn into a conflict, not overwrite it.
	fx := synced(t, map[string]string{"r.docx": "v1"})
	fx.edit("r.docx", "laptop edit")
	res, _ := fx.resync(func() { fx.drive.edit(t, fx.rootID, "r.docx", "phone edit") })
	if res.Conflicts != 1 || res.Uploaded != 0 || len(res.Failed) != 0 || fx.drive.updates != 0 {
		t.Fatalf("result %+v, %d updates", res, fx.drive.updates)
	}
	drive := fx.drive.content(fx.rootID)
	if drive["r.docx"] != "phone edit" || fx.read("r.docx") != "phone edit" || len(drive) != 2 {
		t.Errorf("Drive %v, local %q; want the phone's version kept and the laptop's as a copy", drive, fx.read("r.docx"))
	}
}

func TestSyncEditBeatsDeleteMidRun(t *testing.T) {
	// Drive's copy is trashed after the listing: the laptop's edit is
	// uploaded again as a new file.
	fx := synced(t, map[string]string{"r.docx": "v1"})
	fx.edit("r.docx", "laptop edit")
	res, _ := fx.resync(func() { fx.drive.trash(t, fx.rootID, "r.docx") })
	if res.Uploaded != 1 || len(res.Failed) != 0 || fx.drive.content(fx.rootID)["r.docx"] != "laptop edit" {
		t.Errorf("result %+v, Drive %v", res, fx.drive.content(fx.rootID))
	}
}

func TestSyncKeepsFilesEditedAfterScan(t *testing.T) {
	fx := synced(t, map[string]string{"gone.txt": "1", "changed.txt": "2"})
	fx.drive.trash(t, fx.rootID, "gone.txt")
	fx.drive.edit(t, fx.rootID, "changed.txt", "phone edit")

	// Both local files are saved again after the scan.
	res, _ := fx.resync(func() {
		fx.edit("gone.txt", "saved again")
		fx.edit("changed.txt", "saved again too")
	})
	if res.Trashed != 0 || res.Downloaded != 0 || len(res.Failed) != 2 {
		t.Fatalf("result %+v; want both refused", res)
	}
	if fx.read("gone.txt") != "saved again" || fx.read("changed.txt") != "saved again too" || len(fx.trashed) != 0 {
		t.Error("a file saved after the scan was trashed or overwritten")
	}
	// The next run sees the edits: one beats the delete, one conflicts.
	res, _ = fx.resync(nil)
	if res.Uploaded != 1 || res.Conflicts != 1 || len(res.Failed) != 0 {
		t.Errorf("next run %+v", res)
	}
}

func TestSyncNewFilesBothWays(t *testing.T) {
	fx := synced(t, map[string]string{"a.txt": "1"})
	fx.write("New/here.txt", "from laptop")
	fx.drive.put(t, fx.rootID, "Phone/there.jpg", "from phone", time.Now())

	res, _ := fx.resync(nil)
	if res.Uploaded != 1 || res.Downloaded != 1 || res.Folders != 1 || len(res.Failed) != 0 {
		t.Fatalf("result %+v", res)
	}
	if fx.read("Phone/there.jpg") != "from phone" || fx.drive.content(fx.rootID)["New/here.txt"] != "from laptop" {
		t.Error("new files did not cross over")
	}
	if _, p := fx.resync(nil); len(p.Actions) != 0 {
		t.Errorf("next run plans %+v", p.Actions)
	}
}
