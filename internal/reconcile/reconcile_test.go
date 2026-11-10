package reconcile

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/localfs"
	"github.com/USER1043/cloudsync/internal/state"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// The helpers describe one path's three views. Content is a short string
// whose "MD5" is the string itself, and whose size is its length.

func lfile(rel, content string, mtime time.Time) localfs.File {
	return localfs.File{Rel: rel, Size: int64(len(content)), ModTime: mtime}
}

func rfile(rel, id, content string) gdrive.File {
	return gdrive.File{Rel: rel, ID: id, Size: int64(len(content)), MD5: content}
}

func base(rel, id, content string, mtime time.Time) state.Item {
	return state.Item{Rel: rel, DriveID: id, Size: int64(len(content)), MD5: content, LocalMTime: mtime.UnixNano()}
}

func bdir(rel string) state.Item { return state.Item{Rel: rel, IsDir: true} }

func rdir(rel, id string) gdrive.File { return gdrive.File{Rel: rel, ID: id} }

// hasher returns the local content given for each path.
func hasher(content map[string]string) (Hasher, map[string]int) {
	calls := map[string]int{}
	return func(rel string) (string, error) {
		calls[rel]++
		c, ok := content[rel]
		if !ok {
			return "", errors.New("file is open in another program")
		}
		return c, nil
	}, calls
}

// summary renders a plan as "rel=op" lines, with the Drive ID where it
// matters, for compact comparisons.
func summary(p *Plan) string {
	var b strings.Builder
	for _, a := range p.Actions {
		b.WriteString(a.Rel + "=" + a.Op.String())
		if a.DriveID != "" && (a.Op == Upload || a.Op == Download || a.Op == TrashRemote) {
			b.WriteString("(" + a.DriveID + ")")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestReconcileFiles(t *testing.T) {
	t1 := t0.Add(time.Hour)
	tests := []struct {
		name   string
		local  []localfs.File
		remote []gdrive.File
		base   []state.Item
		hash   map[string]string // local content, for files that get hashed
		want   string
	}{
		{name: "unchanged everywhere",
			local: []localfs.File{lfile("a", "v1", t0)}, remote: []gdrive.File{rfile("a", "id", "v1")},
			base: []state.Item{base("a", "id", "v1", t0)}, want: ""},
		{name: "edited here",
			local: []localfs.File{lfile("a", "v2 longer", t1)}, remote: []gdrive.File{rfile("a", "id", "v1")},
			base: []state.Item{base("a", "id", "v1", t0)}, want: "a=upload(id)\n"},
		{name: "edited here, same size",
			local: []localfs.File{lfile("a", "v2", t1)}, remote: []gdrive.File{rfile("a", "id", "v1")},
			base: []state.Item{base("a", "id", "v1", t0)}, hash: map[string]string{"a": "v2"}, want: "a=upload(id)\n"},
		{name: "touched here, content unchanged",
			local: []localfs.File{lfile("a", "v1", t1)}, remote: []gdrive.File{rfile("a", "id", "v1")},
			base: []state.Item{base("a", "id", "v1", t0)}, hash: map[string]string{"a": "v1"}, want: "a=record\n"},
		{name: "edited in Drive",
			local: []localfs.File{lfile("a", "v1", t0)}, remote: []gdrive.File{rfile("a", "id", "v2")},
			base: []state.Item{base("a", "id", "v1", t0)}, want: "a=download(id)\n"},
		{name: "replaced in Drive by a new file with the same content",
			local: []localfs.File{lfile("a", "v1", t0)}, remote: []gdrive.File{rfile("a", "id2", "v1")},
			base: []state.Item{base("a", "id", "v1", t0)}, want: "a=record\n"},
		{name: "edited on both sides, differently",
			local: []localfs.File{lfile("a", "v2", t1)}, remote: []gdrive.File{rfile("a", "id", "v3")},
			base: []state.Item{base("a", "id", "v1", t0)}, hash: map[string]string{"a": "v2"}, want: "a=conflict\n"},
		{name: "edited on both sides, the same way",
			local: []localfs.File{lfile("a", "v2", t1)}, remote: []gdrive.File{rfile("a", "id", "v2")},
			base: []state.Item{base("a", "id", "v1", t0)}, hash: map[string]string{"a": "v2"}, want: "a=record\n"},
		{name: "deleted in Drive",
			local: []localfs.File{lfile("a", "v1", t0)},
			base:  []state.Item{base("a", "id", "v1", t0)}, want: "a=trash here\n"},
		{name: "deleted in Drive, edited here: edit wins",
			local: []localfs.File{lfile("a", "v2 longer", t1)},
			base:  []state.Item{base("a", "id", "v1", t0)}, want: "a=upload\n"},
		{name: "deleted here",
			remote: []gdrive.File{rfile("a", "id", "v1")},
			base:   []state.Item{base("a", "id", "v1", t0)}, want: "a=trash in Drive(id)\n"},
		{name: "deleted here, edited in Drive: edit wins",
			remote: []gdrive.File{rfile("a", "id", "v2")},
			base:   []state.Item{base("a", "id", "v1", t0)}, want: "a=download(id)\n"},
		{name: "deleted on both sides",
			base: []state.Item{base("a", "id", "v1", t0)}, want: "a=forget\n"},
		{name: "new here",
			local: []localfs.File{lfile("a", "v1", t0)}, want: "a=upload\n"},
		{name: "new in Drive",
			remote: []gdrive.File{rfile("a", "id", "v1")}, want: "a=download(id)\n"},
		{name: "new on both sides, same content",
			local: []localfs.File{lfile("a", "v1", t0)}, remote: []gdrive.File{rfile("a", "id", "v1")},
			hash: map[string]string{"a": "v1"}, want: "a=record\n"},
		{name: "new on both sides, different content",
			local: []localfs.File{lfile("a", "v1", t0)}, remote: []gdrive.File{rfile("a", "id", "v2")},
			hash: map[string]string{"a": "v1"}, want: "a=conflict\n"},
		{name: "unreadable local file waits",
			local: []localfs.File{lfile("a", "v2", t1)}, remote: []gdrive.File{rfile("a", "id", "v1")},
			base: []state.Item{base("a", "id", "v1", t0)}, want: "a=wait\n"},
		{name: "Drive file without MD5 compared by ID and size",
			local:  []localfs.File{lfile("a", "v1", t0)},
			remote: []gdrive.File{{Rel: "a", ID: "id", Size: 2}},
			base:   []state.Item{base("a", "id", "v1", t0)}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := hasher(tc.hash)
			p := Reconcile(Input{Local: tc.local, Remote: tc.remote, Base: tc.base, Hash: h})
			if got := summary(p); got != tc.want {
				t.Errorf("got\n%swant\n%s", got, tc.want)
			}
		})
	}
}

func TestReconcileHashesOnlyWhenNeeded(t *testing.T) {
	h, calls := hasher(map[string]string{"touched": "v1", "both": "v2"})
	Reconcile(Input{
		Local: []localfs.File{
			lfile("same", "v1", t0),           // size and time match: no hash
			lfile("resized", "v2 longer", t0), // size differs: no hash
			lfile("touched", "v1", t0.Add(1)), // time differs: hashed once
			lfile("both", "v2", t0.Add(1)),    // hashed once, used twice
			lfile("newsize", "v1", t0),        // new on both, sizes differ: no hash
		},
		Remote: []gdrive.File{
			rfile("same", "1", "v1"), rfile("resized", "2", "v1"), rfile("touched", "3", "v1"),
			rfile("both", "4", "v2"), rfile("newsize", "5", "v1 longer"),
		},
		Base: []state.Item{
			base("same", "1", "v1", t0), base("resized", "2", "v1", t0),
			base("touched", "3", "v1", t0), base("both", "4", "v1", t0),
		},
		Hash: h,
	})
	want := map[string]int{"touched": 1, "both": 1}
	if len(calls) != len(want) || calls["touched"] != 1 || calls["both"] != 1 {
		t.Errorf("hash calls = %v, want %v", calls, want)
	}
}

func TestReconcileFolders(t *testing.T) {
	tests := []struct {
		name       string
		local      []localfs.File
		localDirs  []string
		remote     []gdrive.File
		remoteDirs []gdrive.File
		base       []state.Item
		want       string
	}{
		{name: "new folder here",
			localDirs: []string{"d"}, want: "d=create folder in Drive\n"},
		{name: "new folder in Drive",
			remoteDirs: []gdrive.File{rdir("d", "D")}, want: "d=create folder here\n"},
		{name: "folder on both sides, first seen",
			localDirs: []string{"d"}, remoteDirs: []gdrive.File{rdir("d", "D")}, want: "d=record\n"},
		{name: "folder deleted in Drive with its files",
			local: []localfs.File{lfile("d/a", "v1", t0)}, localDirs: []string{"d"},
			base: []state.Item{bdir("d"), base("d/a", "a", "v1", t0)},
			want: "d=trash here\nd/a=trash here\n"},
		{name: "folder deleted in Drive, new file here keeps it",
			local:     []localfs.File{lfile("d/a", "v1", t0), lfile("d/e/new", "x", t0)},
			localDirs: []string{"d", "d/e"},
			base:      []state.Item{bdir("d"), base("d/a", "a", "v1", t0)},
			want:      "d=create folder in Drive\nd/a=trash here\nd/e=create folder in Drive\nd/e/new=upload\n"},
		{name: "folder deleted here with its files",
			remote: []gdrive.File{rfile("d/a", "a", "v1")}, remoteDirs: []gdrive.File{rdir("d", "D")},
			base: []state.Item{bdir("d"), base("d/a", "a", "v1", t0)},
			want: "d=trash in Drive(D)\nd/a=trash in Drive(a)\n"},
		{name: "folder deleted here, edited file in Drive keeps it",
			remote: []gdrive.File{rfile("d/a", "a", "v2")}, remoteDirs: []gdrive.File{rdir("d", "D")},
			base: []state.Item{bdir("d"), base("d/a", "a", "v1", t0)},
			want: "d=create folder here\nd/a=download(a)\n"},
		{name: "folder deleted on both sides",
			base: []state.Item{bdir("d")}, want: "d=forget\n"},
		{name: "file here, folder in Drive: left alone",
			local:      []localfs.File{lfile("x", "v1", t0)},
			remote:     []gdrive.File{rfile("x/inner", "i", "v1")},
			remoteDirs: []gdrive.File{rdir("x", "X")},
			want:       "x=wait\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := hasher(nil)
			p := Reconcile(Input{Local: tc.local, LocalDirs: tc.localDirs, Remote: tc.remote,
				RemoteDirs: tc.remoteDirs, Base: tc.base, Hash: h})
			if got := summary(p); got != tc.want {
				t.Errorf("got\n%swant\n%s", got, tc.want)
			}
		})
	}
}

func TestReconcileSkip(t *testing.T) {
	// "big" went over the size limit and "repo" became a git repository:
	// both are missing from the scan but must not be trashed in Drive.
	h, _ := hasher(nil)
	p := Reconcile(Input{
		Remote:     []gdrive.File{rfile("big", "b", "v1"), rfile("repo/x", "x", "v1"), rfile("gone", "g", "v1")},
		RemoteDirs: []gdrive.File{rdir("repo", "R")},
		Base: []state.Item{base("big", "b", "v1", t0), bdir("repo"), base("repo/x", "x", "v1", t0),
			base("gone", "g", "v1", t0)},
		Skip: func(rel string) bool { return rel == "big" || rel == "repo" },
		Hash: h,
	})
	if got, want := summary(p), "gone=trash in Drive(g)\n"; got != want {
		t.Errorf("got\n%swant\n%s", got, want)
	}
}

func TestGuard(t *testing.T) {
	tests := []struct {
		deletes, files int
		hold           bool
	}{
		{0, 1000, false},
		{50, 1000, false},
		{51, 1000, true}, // more than 50 files
		{10, 100, false}, // exactly 10%
		{11, 100, true},  // more than 10%
		{1, 5, true},     // small roots hold early
		{3, 0, true},     // nothing in the base yet
	}
	for _, tc := range tests {
		if got := DefaultGuard.Hold(tc.deletes, tc.files); got != tc.hold {
			t.Errorf("Hold(%d, %d) = %v, want %v", tc.deletes, tc.files, got, tc.hold)
		}
	}
}

func TestSplitAndUndo(t *testing.T) {
	var local []localfs.File
	var base_ []state.Item
	for _, n := range []string{"a", "b", "c"} {
		local = append(local, lfile("d/"+n, "v1", t0))
		base_ = append(base_, base("d/"+n, n, "v1", t0))
	}
	base_ = append(base_, bdir("d"))
	h, _ := hasher(nil)
	p := Reconcile(Input{
		Local: append(local, lfile("new", "x", t0)), LocalDirs: []string{"d"},
		Base: base_, Hash: h,
	})
	if l, r := p.Deletes(); l != 3 || r != 0 {
		t.Fatalf("Deletes() = %d, %d; want 3, 0", l, r)
	}
	if !p.Held(DefaultGuard) {
		t.Error("3 of 3 files deleted should be held")
	}
	rest, deletes := p.SplitDeletes()
	if got, want := summary(rest), "new=upload\n"; got != want {
		t.Errorf("rest:\n%swant\n%s", got, want)
	}
	var undone []Action
	for _, a := range deletes {
		undone = append(undone, Undo(a))
	}
	got := summary(&Plan{Actions: undone})
	want := "d=create folder in Drive\nd/a=upload\nd/b=upload\nd/c=upload\n"
	if got != want {
		t.Errorf("undone:\n%swant\n%s", got, want)
	}
}

func TestGate(t *testing.T) {
	// 60 files deleted in Drive, plus their folder and one upload.
	var local []localfs.File
	var bases []state.Item
	for i := range 60 {
		rel := fmt.Sprintf("old/f%02d", i)
		local = append(local, lfile(rel, "v1", t0))
		bases = append(bases, base(rel, rel, "v1", t0))
	}
	for i := range 940 { // the rest of a 1,000-file root, unchanged
		rel := fmt.Sprintf("keep/f%03d", i)
		local = append(local, lfile(rel, "v1", t0))
		bases = append(bases, base(rel, rel, "v1", t0))
	}
	bases = append(bases, bdir("old"))
	h, _ := hasher(nil)
	in := Input{Local: append(local, lfile("new", "x", t0)), LocalDirs: []string{"old"}, Base: bases, Hash: h}
	in.Remote = make([]gdrive.File, 0, 940)
	for i := range 940 {
		rel := fmt.Sprintf("keep/f%03d", i)
		in.Remote = append(in.Remote, rfile(rel, rel, "v1"))
	}
	p := Reconcile(in)

	run, hold := Gate(p, DefaultGuard, nil, Undecided)
	if got := summary(run); got != "new=upload\n" || len(hold) != 61 {
		t.Fatalf("undecided: run\n%sheld %d; want only the upload to run and 60 files + 1 folder held", got, len(hold))
	}
	held := make(map[string]Op)
	for _, a := range hold {
		held[a.Rel] = a.Op
	}

	run, hold = Gate(p, DefaultGuard, held, Approve)
	if l, _ := run.Deletes(); l != 60 || len(hold) != 0 {
		t.Errorf("approved: %d deletes run, %d held; want 60 and 0", l, len(hold))
	}
	run, hold = Gate(p, DefaultGuard, held, Cancel)
	if l, _ := run.Deletes(); l != 0 || len(hold) != 0 || run.Totals()[Upload].Count != 61 || len(run.Of(MkdirRemote)) != 1 {
		t.Errorf("cancelled: %d deletes, %d held, totals %v; want the 60 files and their folder restored to Drive", l, len(hold), run.Totals())
	}

	// A few deletes under the threshold run without asking.
	small := Reconcile(Input{Local: local[:3], Base: bases[:3], Hash: h, Remote: nil})
	small.BaseFiles = 1000
	if run, hold := Gate(small, DefaultGuard, nil, Undecided); len(hold) != 0 || len(run.Actions) != 3 {
		t.Errorf("3 of 1000: held %d, run %d; want all 3 run", len(hold), len(run.Actions))
	}
}

func TestSkips(t *testing.T) {
	scan := &localfs.Result{
		Repos:    []string{"code/proj"},
		TooLarge: []localfs.File{{Rel: "video.mp4"}},
		Issues:   []filter.Issue{{Rel: "locked.xlsx"}},
	}
	tree := &gdrive.Tree{
		Issues: []filter.Issue{{Rel: "dupe.txt"}},
		Native: []gdrive.File{{Rel: "Plan"}},
	}
	skip := Skips(scan, tree, []string{"phone-big.mov", "node_modules/"})
	for _, rel := range []string{"code/proj", "video.mp4", "locked.xlsx", "dupe.txt", "Plan", "phone-big.mov", "node_modules"} {
		if !skip(rel) {
			t.Errorf("Skips(%q) = false", rel)
		}
	}
	if skip("notes.txt") {
		t.Error("Skips(notes.txt) = true")
	}
}
