package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/USER1043/cloudsync/internal/engine"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/reconcile"
	"github.com/USER1043/cloudsync/internal/state"
)

// later is one root's plan for a sync after its first one.
type later struct {
	in      *inspection
	rootID  string
	plan    *reconcile.Plan    // everything Reconcile found
	run     *reconcile.Plan    // what runs now, after the big-delete guard
	hold    []reconcile.Action // deletes the guard holds for the owner
	held    map[string]reconcile.Op
	pending bool               // an earlier run's deletes are waiting for the owner
	decided reconcile.Decision // the owner's answer to them, for this run

	// live marks a pass of `cloudsync run` after its catch-up, which sees
	// only local changes. It leaves the deletes held earlier (heldItems)
	// alone and adds any it holds itself to them.
	live      bool
	heldItems []state.HeldDelete
}

// rootError is a problem with a root's Drive folder that only the owner can
// sort out. CloudSync stops rather than guess.
type rootError struct{ msg string }

func (e *rootError) Error() string { return e.msg }

func rootMissing(where string) error {
	return &rootError{fmt.Sprintf("the Drive folder %s is missing or in Drive's trash. If it was deleted by mistake, restore it "+
		"from Drive's trash (drive.google.com, Trash) and run this again. CloudSync stops rather than upload everything again", where)}
}

// planLater compares in with the state DB's record of the last sync and
// applies the big-delete guard, with d as the owner's answer to deletes held
// earlier. It changes nothing.
func planLater(ctx context.Context, in *inspection, db *state.DB, d reconcile.Decision) (*later, error) {
	stored, _, err := db.Meta(ctx, metaRootID+in.root.Name)
	if err != nil {
		return nil, err
	}
	where := "My Drive/" + strings.Join(in.root.RemoteSegments(), "/")
	switch {
	case in.rootID == "":
		return nil, rootMissing(where)
	case stored != "" && stored != in.rootID:
		return nil, &rootError{fmt.Sprintf("the Drive folder %s is not the one this computer synced with (it was replaced or recreated). "+
			"CloudSync stops rather than guess; restore the original folder from Drive's trash, or ask for help resetting this root", where)}
	}

	base, err := db.Items(ctx, in.root.Name)
	if err != nil {
		return nil, err
	}
	l := &later{in: in, rootID: in.rootID, decided: d, held: map[string]reconcile.Op{}}
	l.plan = reconcile.Reconcile(reconcile.Input{
		Local: in.scan.Files, LocalDirs: in.scan.Dirs,
		Remote: in.tree.Files, RemoteDirs: in.tree.Folders,
		Base: base,
		Skip: reconcile.Skips(in.scan, in.tree, in.driveIgnored),
		Hash: in.hash,
	})
	_, items, pending, err := db.PendingDeletes(ctx, in.root.Name)
	if err != nil {
		return nil, err
	}
	l.pending = pending
	for _, it := range items {
		op := reconcile.TrashRemote
		if it.Here {
			op = reconcile.TrashLocal
		}
		l.held[it.Rel] = op
	}
	if !pending {
		d = reconcile.Undecided
	}
	l.run, l.hold = reconcile.Gate(l.plan, reconcile.DefaultGuard, l.held, d)
	return l, nil
}

// uploadBytes is what the sync would add to Drive.
func (l *later) uploadBytes() int64 {
	t := l.run.Totals()
	return t[reconcile.Upload].Bytes + t[reconcile.Conflict].Bytes
}

// trashName is where local deletes go on this system.
func trashName() string {
	if runtime.GOOS == "windows" {
		return "the Recycle Bin"
	}
	return "the Trash"
}

func printLater(w io.Writer, l *later, limit int) {
	in := l.in
	fmt.Fprintf(w, "\n== %s\nLocal: %s\nDrive: My Drive/%s\n", in.root.Name, in.local, strings.Join(in.root.RemoteSegments(), "/"))

	t := l.run.Totals()
	switch {
	case !l.run.Changes() && len(l.hold) == 0:
		fmt.Fprintf(w, "\nEverything is in sync (%d files).\n", l.plan.BaseFiles)
	case !l.run.Changes():
		fmt.Fprintln(w, "\nNothing else changed since the last sync.")
	default:
		fmt.Fprintln(w, "\nChanges since the last sync:")
		row := func(label string, op reconcile.Op, note string) {
			if tot := t[op]; tot.Count > 0 {
				fmt.Fprintf(w, "  %-15s %6s  %-9s %s\n", label, files(tot.Count), humanBytes(tot.Bytes), note)
			}
		}
		row("upload", reconcile.Upload, "new or edited here")
		row("download", reconcile.Download, "new or edited in Drive")
		row("trash here", reconcile.TrashLocal, "deleted in Drive; moved to "+trashName())
		row("trash in Drive", reconcile.TrashRemote, "deleted here; moved to Drive's trash")
		row("conflicts", reconcile.Conflict, "edited on both sides; both copies kept")
		if n := len(l.run.Of(reconcile.MkdirLocal)) + len(l.run.Of(reconcile.MkdirRemote)); n > 0 {
			fmt.Fprintf(w, "  %-15s %6d\n", "new folders", n)
		}
	}

	lines := func(acts []reconcile.Action) []string {
		out := make([]string, len(acts))
		for i, a := range acts {
			out[i] = a.Rel
			if a.Dir {
				out[i] += "/"
			}
			if a.Note != "" {
				out[i] += " (" + a.Note + ")"
			}
		}
		return out
	}
	printList(w, "Uploads", lines(l.run.Of(reconcile.Upload)), limit)
	printList(w, "Downloads", lines(l.run.Of(reconcile.Download)), limit)
	printList(w, "Trash here", lines(l.run.Of(reconcile.TrashLocal)), limit)
	printList(w, "Trash in Drive", lines(l.run.Of(reconcile.TrashRemote)), limit)
	printList(w, "Conflicts", lines(l.run.Of(reconcile.Conflict)), limit)
	printList(w, "Waiting until the next sync", lines(l.run.Of(reconcile.Wait)), limit)

	var issues []string
	for _, is := range in.scan.Issues {
		issues = append(issues, fmt.Sprintf("%s: %s", is.Rel, is.Reason))
	}
	for _, is := range in.tree.Issues {
		issues = append(issues, fmt.Sprintf("[Drive] %s: %s", is.Rel, is.Reason))
	}
	printList(w, "Cannot sync until renamed or fixed", issues, limit)

	if l.pending && l.decided == reconcile.Undecided {
		fmt.Fprintf(w, "\nDeletes held from an earlier sync are still waiting for you: see `cloudsync deletes`.\n")
	}
	if len(l.hold) > 0 {
		here, drive := 0, 0
		for _, a := range l.hold {
			switch {
			case a.Dir:
			case a.Op == reconcile.TrashLocal:
				here++
			default:
				drive++
			}
		}
		fmt.Fprintf(w, "\nBig-delete guard: %d deletes held (%d here, %d in Drive) because they are more than 50 files "+
			"or 10%% of the %d files in %s. Everything else syncs now.\n", here+drive, here, drive, l.plan.BaseFiles, in.root.Name)
		printList(w, "Held deletes", lines(l.hold), limit)
		fmt.Fprintln(w, "Review them with `cloudsync deletes`, then run `cloudsync deletes approve` or `cloudsync deletes cancel`.")
	}
}

// syncLater carries out a sync after the root's first one: the plan the
// guard lets through runs now, and held deletes are saved for the owner.
func syncLater(ctx context.Context, w io.Writer, in *inspection, d *gdrive.Client, db *state.DB, decision reconcile.Decision, yes bool) error {
	l, err := planLater(ctx, in, db, decision)
	if err != nil {
		return err
	}
	printLater(w, l, listLimit)
	if !l.run.Changes() && len(l.hold) == 0 && !(l.pending && decision != reconcile.Undecided) {
		// Nothing to transfer; still record anything only the state DB needs.
		_, err := runLater(ctx, w, l, d, db)
		return err
	}

	acc, err := d.Account(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(w)
	printQuota(w, acc, "this sync", l.uploadBytes())
	if acc.Limit > 0 && l.uploadBytes() > acc.Limit-acc.Usage {
		return fmt.Errorf("not enough Drive storage; free up space in Drive, Gmail or Photos (they share the 15 GB), "+
			"or remove files from %s", in.local)
	}
	if l.run.Changes() && !yes && !confirm(w, fmt.Sprintf("\nApply these changes to %s? Deleted files go to %s or Drive's trash. [y/N] ",
		in.root.Name, trashName())) {
		fmt.Fprintln(w, "Cancelled; nothing was changed.")
		return nil
	}
	start := time.Now()
	res, err := runLater(ctx, w, l, d, db)
	if res != nil && l.run.Changes() {
		fmt.Fprintf(w, "\nUploaded %d, downloaded %d, trashed %d here and %d in Drive, conflicts %d (both copies kept), "+
			"folders created %d, removed %d.\nTransferred %s in %s.\n",
			res.Uploaded, res.Downloaded, res.Trashed, res.TrashedInDrive, res.Conflicts, res.Folders, res.FoldersRemoved,
			humanBytes(res.Bytes), time.Since(start).Round(time.Second))
	}
	if err != nil {
		return err
	}
	if len(res.Failed) > 0 {
		lines := make([]string, len(res.Failed))
		for i, f := range res.Failed {
			lines[i] = fmt.Sprintf("%s: %s", f.Rel, f.Reason)
		}
		printList(w, "Failed", lines, -1)
		return fmt.Errorf("%d files could not be synced; fix the reasons above and run `cloudsync sync` again", len(res.Failed))
	}
	return nil
}

// runLater records the guard's outcome in the delete log and runs l.run.
func runLater(ctx context.Context, w io.Writer, l *later, d *gdrive.Client, db *state.DB) (*engine.Result, error) {
	root := l.in.root.Name
	now := time.Now().Unix()

	// The owner's answer, if any, closes the pending question first; a run
	// that no longer needs to hold anything supersedes it.
	var err error
	switch {
	case l.live:
	case l.pending && l.decided == reconcile.Approve:
		err = db.ResolveDeletes(ctx, root, state.DeletesApproved, now)
	case l.pending && l.decided == reconcile.Cancel:
		err = db.ResolveDeletes(ctx, root, state.DeletesCancelled, now)
	case l.pending && len(l.hold) == 0:
		err = db.ResolveDeletes(ctx, root, state.DeletesSuperseded, now)
	}
	if err != nil {
		return nil, err
	}
	if len(l.hold) > 0 {
		ev := state.DeleteEvent{At: now, Root: root, OS: osLabel(), Files: l.plan.BaseFiles}
		var items []state.HeldDelete
		if l.live {
			items = slices.Clone(l.heldItems)
		}
		for _, a := range l.hold {
			items = append(items, state.HeldDelete{Rel: a.Rel, Here: a.Op == reconcile.TrashLocal, Dir: a.Dir, DriveID: a.DriveID, Size: a.Size})
		}
		for _, it := range items {
			switch {
			case it.Dir:
			case it.Here:
				ev.Local++
			default:
				ev.Remote++
			}
		}
		if _, err := db.HoldDeletes(ctx, ev, items); err != nil {
			return nil, err
		}
	}
	// Deletes the guard let through on its own, without the owner's answer.
	auto := state.DeleteEvent{At: now, Root: root, OS: osLabel(), Files: l.plan.BaseFiles, Decision: state.DeletesAuto}
	for _, a := range l.run.Actions {
		if a.Dir || (a.Op != reconcile.TrashLocal && a.Op != reconcile.TrashRemote) {
			continue
		}
		if op, ok := l.held[a.Rel]; ok && op == a.Op && l.decided == reconcile.Approve {
			continue
		}
		if a.Op == reconcile.TrashLocal {
			auto.Local++
		} else {
			auto.Remote++
		}
	}
	if auto.Local+auto.Remote > 0 {
		if _, err := db.LogDeletes(ctx, auto); err != nil {
			return nil, err
		}
	}

	opt := engine.Options{Root: root, Local: l.in.local, RootID: l.rootID, Label: osLabel(), Log: w, Names: l.in.names, Explain: describe}
	if f, ok := w.(*os.File); ok && l.run.Changes() {
		fmt.Fprintln(w)
		if bar := newProgressBar(f); bar != nil {
			opt.Log, opt.Progress = bar, bar.Update
		}
	}
	res, err := engine.Sync(ctx, d, db, opt, l.in.scan, l.in.tree, l.run)
	if bar, ok := opt.Log.(*progressBar); ok {
		bar.Finish()
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return res, errors.New("sync stopped; run `cloudsync sync` again to continue")
		}
		return res, fmt.Errorf("sync stopped: %w (run `cloudsync sync` again to continue)", err)
	}
	// For `cloudsync status`: a full sync compared with Drive; a live pass
	// only uploaded.
	stamp := time.Now().Format(time.RFC3339)
	switch {
	case !l.live:
		err = db.SetMeta(ctx, metaLastCheck+root, stamp)
	case l.run.Changes():
		err = db.SetMeta(ctx, metaLastUpload+root, stamp)
	}
	return res, err
}

// files is "1 file" or "N files", right-aligned for tables.
func files(n int) string {
	if n == 1 {
		return "1 file "
	}
	return fmt.Sprintf("%d files", n)
}
