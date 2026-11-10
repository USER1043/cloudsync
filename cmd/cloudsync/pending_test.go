package main

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/gdrive"
)

func TestPendingFromLivePlan(t *testing.T) {
	f := newLiveFixture(t)
	f.synced("kept.txt", "same", true)
	// Enough files that one delete stays under the big-delete guard; held
	// deletes wait for the owner and aren't listed.
	for i := range 20 {
		f.synced(fmt.Sprintf("other%02d.txt", i), "x", true)
	}
	f.synced("edited.txt", "old", true)
	f.synced("gone.txt", "bye", false)
	edited := f.old.Add(time.Minute)
	f.write("edited.txt", "new content", edited)
	f.write("dir/new.txt", "new", f.old)
	f.write("busy.txt", "being written right now", time.Now())

	// At shutdown nothing waits to settle, so busy.txt is listed too.
	l, _ := f.plan(0)
	p := pendingFrom(l, "3f9a1c2b", time.Now())
	got := make(map[string]pendingFile)
	for _, e := range p.Files {
		got[e.Path] = e
	}
	if len(got) != 4 {
		t.Fatalf("pending = %+v; want busy.txt, dir/new.txt, edited.txt, gone.txt", p.Files)
	}
	if e := got["edited.txt"]; e.Size != int64(len("new content")) || !e.Modified.Equal(edited) || e.Deleted {
		t.Errorf("edited.txt = %+v", e)
	}
	if e := got["gone.txt"]; !e.Deleted {
		t.Errorf("gone.txt = %+v; want deleted", e)
	}
	if _, ok := got["kept.txt"]; ok {
		t.Error("an unchanged file is listed")
	}
	if p.OS != osLabel() || p.Root != "R" || p.Computer != "3f9a1c2b" {
		t.Errorf("list header = %q %q %q", p.OS, p.Root, p.Computer)
	}
}

func TestPendingStale(t *testing.T) {
	mt := time.Date(2026, 11, 10, 19, 30, 0, 123456789, time.UTC)
	p := pendingList{OS: "Windows", Root: "R", At: mt, Files: []pendingFile{
		{Path: "arrived.txt", Size: 5, Modified: mt},
		{Path: "old.txt", Size: 5, Modified: mt},
		{Path: "missing.txt", Size: 5, Modified: mt},
		{Path: "trashed.txt", Deleted: true},
		{Path: "still-there.txt", Deleted: true},
	}}
	// Round trip through JSON, as it goes through Drive.
	data, err := p.encode()
	if err != nil {
		t.Fatal(err)
	}
	if p, err = decodePending(data); err != nil {
		t.Fatal(err)
	}
	tree := &gdrive.Tree{Files: []gdrive.File{
		{Rel: "arrived.txt", Size: 5, ModTime: mt.Truncate(time.Millisecond)}, // Drive keeps milliseconds
		{Rel: "old.txt", Size: 4, ModTime: mt.Add(-time.Hour)},
		{Rel: "still-there.txt", Size: 1},
	}}
	var got []string
	for _, e := range p.stale(tree) {
		got = append(got, e.Path)
	}
	if want := []string{"old.txt", "missing.txt", "still-there.txt"}; !slices.Equal(got, want) {
		t.Errorf("stale = %q, want %q", got, want)
	}
}

func TestPendingPlaces(t *testing.T) {
	for _, c := range []struct {
		remote string
		want   []string
	}{
		{"CloudSync/MyFiles", []string{"CloudSync", ".cloudsync"}},
		{"MyFiles", []string{".cloudsync"}},
		{"CloudSync-Test/TwoWay-1110", []string{"CloudSync-Test", ".cloudsync"}},
	} {
		if got := metaSegments(config.Root{Remote: c.remote}); !slices.Equal(got, c.want) {
			t.Errorf("metaSegments(%q) = %q, want %q", c.remote, got, c.want)
		}
	}
	name := pendingName("Linux", "3f9a1c2b", "MyFiles")
	if name != "pending-linux-3f9a1c2b-MyFiles.json" || !isPendingName(name) {
		t.Errorf("pendingName = %q", name)
	}
	if isPendingName("notes.txt") {
		t.Error("isPendingName(notes.txt)")
	}
}

func TestComputerID(t *testing.T) {
	f := newLiveFixture(t)
	id, err := computerID(f.ctx, f.db)
	if err != nil || len(id) != 8 {
		t.Fatalf("computerID = %q, %v", id, err)
	}
	if again, _ := computerID(f.ctx, f.db); again != id {
		t.Errorf("second computerID = %q, want the same %q", again, id)
	}
	other := newLiveFixture(t)
	if theirs, _ := computerID(other.ctx, other.db); theirs == id {
		t.Errorf("two state DBs got the same ID %q", id)
	}
}
