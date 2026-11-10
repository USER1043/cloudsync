package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/localfs"
	"github.com/USER1043/cloudsync/internal/reconcile"
	"github.com/USER1043/cloudsync/internal/state"
)

// liveFixture is a root whose last sync the state DB records, ready for
// local changes.
type liveFixture struct {
	t     *testing.T
	ctx   context.Context
	local string
	db    *state.DB
	s     *session
	m     *filter.Matcher
	old   time.Time // modification time of files written by synced
}

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m, err := filter.New(filter.DefaultIgnore)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	return &liveFixture{
		t: t, ctx: context.Background(), local: local, db: db, m: m,
		s:   &session{root: config.Root{Name: "R"}, local: local, rootID: "root", unsynced: map[string]bool{}},
		old: time.Now().Add(-time.Hour).Truncate(time.Second),
	}
}

// write creates the local file rel, modified at mt.
func (f *liveFixture) write(rel, content string, mt time.Time) string {
	f.t.Helper()
	p := filepath.Join(f.local, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// synced writes rel and records it as synced; with keep false the local
// file is removed again, as if deleted since.
func (f *liveFixture) synced(rel, content string, keep bool) {
	f.t.Helper()
	p := f.write(rel, content, f.old)
	sum, err := localfs.MD5File(p)
	if err != nil {
		f.t.Fatal(err)
	}
	it := state.Item{Root: "R", Rel: rel, DriveID: "id-" + rel, Size: int64(len(content)), MD5: sum, LocalMTime: f.old.UnixNano()}
	if err := f.db.Put(f.ctx, it); err != nil {
		f.t.Fatal(err)
	}
	if !keep {
		if err := os.Remove(p); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *liveFixture) dir(rel string, keep bool) {
	f.t.Helper()
	if keep {
		if err := os.MkdirAll(filepath.Join(f.local, filepath.FromSlash(rel)), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := f.db.Put(f.ctx, state.Item{Root: "R", Rel: rel, DriveID: "id-" + rel, IsDir: true}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *liveFixture) plan(settleFor time.Duration) (*later, bool) {
	f.t.Helper()
	l, recent, err := planLive(f.ctx, f.s, f.m, f.db, time.Now(), settleFor)
	if err != nil {
		f.t.Fatal(err)
	}
	return l, recent
}

func opsOf(acts []reconcile.Action) []string {
	var out []string
	for _, a := range acts {
		s := fmt.Sprintf("%v %s", a.Op, a.Rel)
		if a.DriveID != "" {
			s += " " + a.DriveID
		}
		out = append(out, s)
	}
	return out
}

func TestPlanLive(t *testing.T) {
	f := newLiveFixture(t)
	f.dir("sub", true)
	f.dir("gone", false)
	f.synced("same.txt", "same", true)
	f.synced("touched.txt", "touched", true)
	f.synced("edited.txt", "old", true)
	f.synced("deleted.txt", "deleted", false)
	f.synced("sub/held.txt", "held", false)
	f.synced("gone/x.txt", "x", false)
	f.write("edited.txt", "new content", f.old.Add(time.Minute))
	f.write("touched.txt", "touched", f.old.Add(time.Minute))
	f.write("sub/new.txt", "new", f.old)
	f.write("drive-doc.txt", "local file where Drive has an unsynced item", f.old)
	f.write("recent.txt", "still being written", time.Now())
	f.s.unsynced["drive-doc.txt"] = true
	if _, err := f.db.HoldDeletes(f.ctx, state.DeleteEvent{At: 1, Root: "R", OS: "Linux", Files: 6, Remote: 1},
		[]state.HeldDelete{{Rel: "sub/held.txt", DriveID: "id-sub/held.txt", Size: 4}}); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(filepath.Join(f.local, "gone")); err != nil {
		t.Fatal(err)
	}

	l, recent := f.plan(settle)
	if !recent {
		t.Error("recent.txt was not reported as still being written")
	}
	want := []string{
		"upload edited.txt id-edited.txt",
		"upload sub/new.txt",
		"record touched.txt id-touched.txt",
	}
	if got := opsOf(l.run.Actions); !slices.Equal(got, want) {
		t.Errorf("run =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// 2 file deletes (sub/held.txt is held already) of 6 files is over 10%.
	want = []string{"trash in Drive deleted.txt id-deleted.txt", "trash in Drive gone id-gone", "trash in Drive gone/x.txt id-gone/x.txt"}
	if got := opsOf(l.hold); !slices.Equal(got, want) {
		t.Errorf("hold = %q, want %q", got, want)
	}
	if !l.live || !l.pending || len(l.heldItems) != 1 {
		t.Errorf("live=%v pending=%v heldItems=%v; want the earlier held delete carried along", l.live, l.pending, l.heldItems)
	}

	// The final pass at exit doesn't wait for files to settle.
	l, recent = f.plan(0)
	if recent || !slices.Contains(opsOf(l.run.Actions), "upload recent.txt") {
		t.Errorf("final pass: recent=%v run=%q; want recent.txt uploaded", recent, opsOf(l.run.Actions))
	}
}

// A live pass never acts on Drive's side: with the base standing in for
// Drive, there is nothing to download or trash here.
func TestPlanLiveOnlyUploads(t *testing.T) {
	f := newLiveFixture(t)
	for i := range 20 {
		f.synced(fmt.Sprintf("f%02d.txt", i), "x", true)
	}
	f.synced("deleted.txt", "x", false)
	l, _ := f.plan(settle)
	if got := opsOf(l.run.Actions); !slices.Equal(got, []string{"trash in Drive deleted.txt id-deleted.txt"}) {
		t.Errorf("run = %q", got)
	}
	for _, a := range l.plan.Actions {
		switch a.Op {
		case reconcile.Download, reconcile.TrashLocal, reconcile.Conflict, reconcile.MkdirLocal:
			t.Errorf("live plan has %v %s", a.Op, a.Rel)
		}
	}
}

// Deletes the guard holds in a live pass join those held earlier, under the
// one pending question, and the earlier answer isn't touched.
func TestLiveHoldMerges(t *testing.T) {
	f := newLiveFixture(t)
	for i := range 100 {
		f.synced(fmt.Sprintf("keep%03d.txt", i), "k", true)
	}
	for i := range 60 {
		f.synced(fmt.Sprintf("del%02d.txt", i), "d", false)
	}
	f.synced("held.txt", "h", false)
	if _, err := f.db.HoldDeletes(f.ctx, state.DeleteEvent{At: 1, Root: "R", OS: "Linux", Files: 161, Remote: 1},
		[]state.HeldDelete{{Rel: "held.txt", DriveID: "id-held.txt", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	l, _ := f.plan(settle)
	if len(l.hold) != 60 || l.run.Changes() {
		t.Fatalf("hold %d, run %q; want the 60 new deletes held and nothing else", len(l.hold), opsOf(l.run.Actions))
	}
	// Nothing runs in Drive, so a nil client is never called.
	if _, err := runLater(f.ctx, &bytes.Buffer{}, l, (*gdrive.Client)(nil), f.db); err != nil {
		t.Fatal(err)
	}
	ev, items, ok, err := f.db.PendingDeletes(f.ctx, "R")
	if err != nil || !ok {
		t.Fatalf("PendingDeletes: ok=%v err=%v", ok, err)
	}
	if len(items) != 61 || ev.Remote != 61 || ev.Local != 0 {
		t.Errorf("pending: %d items, %d remote, %d local; want 61 in Drive", len(items), ev.Remote, ev.Local)
	}
	log, err := f.db.DeleteLog(f.ctx, "R")
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 {
		t.Errorf("delete log has %d events, want the one pending question", len(log))
	}
}

func TestNewSession(t *testing.T) {
	in := &inspection{
		root: config.Root{Name: "R"}, local: "/l", rootID: "root",
		tree: &gdrive.Tree{
			Files:   []gdrive.File{{Rel: "synced.txt"}, {Rel: "failed.txt"}, {Rel: "trashed.txt"}},
			Folders: []gdrive.File{{Rel: "dir"}},
			Native:  []gdrive.File{{Rel: "Notes"}},
			Issues:  []filter.Issue{{Rel: "dup.txt"}},
		},
	}
	run := &reconcile.Plan{Actions: []reconcile.Action{{Rel: "trashed.txt", Op: reconcile.TrashRemote}}}
	base := []state.Item{{Rel: "synced.txt"}, {Rel: "dir", IsDir: true}}
	s := newSession(in, run, base)
	var got []string
	for rel := range s.unsynced {
		got = append(got, rel)
	}
	slices.Sort(got)
	if want := []string{"Notes", "dup.txt", "failed.txt"}; !slices.Equal(got, want) {
		t.Errorf("unsynced = %q, want %q", got, want)
	}
}

func TestSchedule(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var s schedule
	if !s.next().IsZero() {
		t.Fatal("empty schedule has a pass due")
	}
	s.change(t0)
	if got := s.next(); !got.Equal(t0.Add(quiet)) {
		t.Errorf("after one change: due %v, want %v", got.Sub(t0), quiet)
	}
	// Changes that never stop still get a pass maxDelay after the first.
	for d := time.Second; d < 2*maxDelay; d += time.Second {
		s.change(t0.Add(d))
	}
	if got := s.next(); !got.Equal(t0.Add(maxDelay)) {
		t.Errorf("non-stop changes: due %v, want %v", got.Sub(t0), maxDelay)
	}
	// An earlier requested time wins; a later one doesn't delay the pass.
	s.at(t0.Add(time.Second))
	if got := s.next(); !got.Equal(t0.Add(time.Second)) {
		t.Errorf("at earlier: due %v", got.Sub(t0))
	}
	s = schedule{}
	s.at(t0.Add(time.Minute))
	s.change(t0)
	if got := s.next(); !got.Equal(t0.Add(quiet)) {
		t.Errorf("change before a later at: due %v", got.Sub(t0))
	}
}

func TestLogWriter(t *testing.T) {
	var buf bytes.Buffer
	w := newLogWriter(&buf)
	fmt.Fprint(w, "one\n\n  \ntw")
	fmt.Fprint(w, "o\n")
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " one") || !strings.HasSuffix(lines[1], " two") {
		t.Fatalf("log = %q", buf.String())
	}
	if _, err := time.Parse("2006-01-02 15:04:05", lines[0][:19]); err != nil {
		t.Errorf("line has no timestamp: %q", lines[0])
	}
}
