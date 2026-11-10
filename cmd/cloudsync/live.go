package main

import (
	"context"
	"slices"
	"time"

	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/localfs"
	"github.com/USER1043/cloudsync/internal/reconcile"
	"github.com/USER1043/cloudsync/internal/state"
)

// settle is how long a file must go unmodified before a live pass uploads
// it; a file changed more recently may still be being written.
const settle = 2 * time.Second

// session is what `cloudsync run` knows about a root once its catch-up with
// Drive has run.
type session struct {
	root   config.Root
	local  string
	rootID string
	metaID string // Drive folder for CloudSync's own files about this root (metaSegments)

	// unsynced holds the Drive paths the catch-up saw but did not sync:
	// Google Docs, duplicate or unusable names, failed downloads, file and
	// folder clashes. A live pass doesn't see Drive, so it leaves those
	// paths alone, rather than upload a local file over something it can't
	// see, until the next full sync.
	unsynced map[string]bool
}

// newSession records what the catch-up of in left unsynced: every item of
// the Drive tree that has no state DB record after it, except those the
// catch-up moved to Drive's trash.
func newSession(in *inspection, run *reconcile.Plan, base []state.Item) *session {
	s := &session{root: in.root, local: in.local, rootID: in.rootID, unsynced: map[string]bool{}}
	known := make(map[string]bool, len(base))
	for _, it := range base {
		known[it.Rel] = true
	}
	for _, a := range run.Of(reconcile.TrashRemote) {
		known[a.Rel] = true
	}
	add := func(rel string) {
		if !known[rel] {
			s.unsynced[rel] = true
		}
	}
	for _, list := range [][]gdrive.File{in.tree.Files, in.tree.Folders, in.tree.Native} {
		for _, f := range list {
			add(f.Rel)
		}
	}
	for _, is := range in.tree.Issues {
		add(is.Rel)
	}
	return s
}

// treeFromBase is Drive as this computer last synced it, according to the
// state DB.
func treeFromBase(base []state.Item) *gdrive.Tree {
	t := &gdrive.Tree{}
	for _, it := range base {
		f := gdrive.File{ID: it.DriveID, Rel: it.Rel, MD5: it.MD5, Size: it.Size}
		if it.IsDir {
			f.MimeType = "application/vnd.google-apps.folder"
			t.Folders = append(t.Folders, f)
		} else {
			t.Files = append(t.Files, f)
		}
	}
	return t
}

// planLive plans a live pass: it rescans the root and compares it with the
// state DB's record of the last sync, standing in for Drive. Drive isn't
// listed between boots, so every difference is a change made here: a new
// or edited file to upload, a deleted one to move to Drive's trash. An edit
// made in Drive meanwhile (on the phone, say) is still caught when the
// upload runs: the engine's in-place update finds it and keeps both copies.
//
// Deletes held for the owner and the session's unsynced paths are left
// alone, as are files modified less than settleFor ago (they may still be
// being written); recent reports whether there were any, so the caller can
// look again soon.
func planLive(ctx context.Context, s *session, m *filter.Matcher, db *state.DB, now time.Time, settleFor time.Duration) (l *later, recent bool, err error) {
	names, err := loadNames(ctx, db, s.root.Name)
	if err != nil {
		return nil, false, err
	}
	base, err := db.Items(ctx, s.root.Name)
	if err != nil {
		return nil, false, err
	}
	scan, err := localfs.Scan(s.local, m, names)
	if err != nil {
		return nil, false, err
	}
	in := &inspection{root: s.root, local: s.local, names: names, rootID: s.rootID, scan: scan}
	in.tree, in.driveIgnored = treeFromBase(base).Filter(scan.Matcher)

	_, heldItems, pending, err := db.PendingDeletes(ctx, s.root.Name)
	if err != nil {
		return nil, false, err
	}
	skip := make(map[string]bool)
	for _, it := range heldItems {
		skip[it.Rel] = true
	}
	inBase := make(map[string]bool, len(base))
	for _, it := range base {
		inBase[it.Rel] = true
	}
	for rel := range s.unsynced {
		if !inBase[rel] {
			skip[rel] = true
		}
	}
	for _, f := range scan.Files {
		// Abs: a file stamped a little in the future is just as fresh.
		if settleFor > 0 && now.Sub(f.ModTime).Abs() < settleFor {
			skip[f.Rel] = true
			recent = true
		}
	}
	skips := reconcile.Skips(scan, in.tree, in.driveIgnored)

	l = &later{in: in, rootID: s.rootID, live: true, pending: pending, heldItems: heldItems}
	l.plan = reconcile.Reconcile(reconcile.Input{
		Local: scan.Files, LocalDirs: scan.Dirs,
		Remote: in.tree.Files, RemoteDirs: in.tree.Folders,
		Base: base,
		Skip: func(rel string) bool { return skip[rel] || skips(rel) },
		Hash: in.hash,
	})
	// With the base standing in for Drive, nothing can come from Drive. Drop
	// anything that would, should a stale record ever suggest it.
	l.plan.Actions = slices.DeleteFunc(l.plan.Actions, func(a reconcile.Action) bool {
		switch a.Op {
		case reconcile.Download, reconcile.TrashLocal, reconcile.Conflict, reconcile.MkdirLocal:
			return true
		}
		return false
	})
	l.run, l.hold = reconcile.Gate(l.plan, reconcile.DefaultGuard, nil, reconcile.Undecided)
	return l, recent, nil
}
