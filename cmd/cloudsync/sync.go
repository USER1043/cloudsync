package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/engine"
	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/state"
)

const (
	metaChangesToken = "drive_changes_token"
	metaFirstSync    = "first_sync_done:" // + root name
	metaRootID       = "root_folder_id:"  // + root name
)

func runSync(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("sync", flag.ContinueOnError)
	yes := fl.Bool("yes", false, "start without asking for confirmation")
	if err := fl.Parse(args); err != nil {
		return err
	}
	e, err := loadEnv()
	if err != nil {
		return err
	}
	m, err := matcher(e.cfg)
	if err != nil {
		return err
	}
	d, err := e.drive(ctx)
	if err != nil {
		return err
	}
	db, closeState, err := openState()
	if err != nil {
		return err
	}
	defer closeState()

	// Remember where the Drive change feed stands before touching anything,
	// so the continuous sync can later pick up every change made after this.
	if _, ok, err := db.Meta(ctx, metaChangesToken); err != nil {
		return err
	} else if !ok {
		tok, err := d.StartPageToken(ctx)
		if err != nil {
			return err
		}
		if err := db.SetMeta(ctx, metaChangesToken, tok); err != nil {
			return err
		}
	}

	w := os.Stdout
	printConfigSource(w, e.cfg)
	for _, root := range e.cfg.Roots {
		if err := syncRoot(ctx, w, root, m, d, db, *yes); err != nil {
			return fmt.Errorf("root %q: %w", root.Name, err)
		}
	}
	return nil
}

func syncRoot(ctx context.Context, w io.Writer, root config.Root, m *filter.Matcher, d *gdrive.Client, db *state.DB, yes bool) error {
	if at, ok, err := db.Meta(ctx, metaFirstSync+root.Name); err != nil {
		return err
	} else if ok {
		fmt.Fprintf(w, "\n== %s: first sync already completed (%s).\nContinuous two-way sync arrives in Phase 3.\n", root.Name, at)
		return nil
	}

	names, err := loadNames(ctx, db, root.Name)
	if err != nil {
		return err
	}
	in, err := inspect(ctx, root, m, d, names)
	if err != nil {
		return err
	}
	printReport(w, in, true, listLimit)

	acc, err := d.Account(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(w)
	printQuota(w, acc, in.uploadBytes())
	if acc.Limit > 0 && in.uploadBytes() > acc.Limit-acc.Usage {
		return fmt.Errorf("not enough Drive storage; free up space or remove files from %s", in.local)
	}
	if !yes && !confirm(w, fmt.Sprintf("\nStart the first sync of %s? Nothing will be deleted or overwritten. [y/N] ", root.Name)) {
		fmt.Fprintln(w, "Cancelled; nothing was changed.")
		return nil
	}

	rootID := in.rootID
	if rootID == "" {
		if rootID, err = d.EnsurePath(ctx, root.RemoteSegments()); err != nil {
			return err
		}
	}
	if err := db.SetMeta(ctx, metaRootID+root.Name, rootID); err != nil {
		return err
	}

	start := time.Now()
	fmt.Fprintln(w)
	opt := engine.Options{Root: root.Name, Local: in.local, RootID: rootID, Label: osLabel(), Log: w, Names: names}
	if f, ok := w.(*os.File); ok {
		if bar := newProgressBar(f); bar != nil {
			opt.Log, opt.Progress = bar, bar.Update
		}
	}
	res, err := engine.FirstSync(ctx, d, db, opt, in.scan, in.tree, in.plan)
	if bar, ok := opt.Log.(*progressBar); ok {
		bar.Finish()
	}
	if res != nil {
		fmt.Fprintf(w, "\nUploaded %d, downloaded %d, already in sync %d, conflicts %d (both copies kept), folders created %d.\n",
			res.Uploaded, res.Downloaded, res.InSync, res.Conflicts, res.Folders)
		fmt.Fprintf(w, "Transferred %s in %s.\n", humanBytes(res.Bytes), time.Since(start).Round(time.Second))
	}
	if err != nil {
		return fmt.Errorf("sync stopped: %w (run `cloudsync sync` again to continue)", err)
	}
	if len(res.Failed) > 0 {
		lines := make([]string, len(res.Failed))
		for i, f := range res.Failed {
			lines[i] = fmt.Sprintf("%s: %s", f.Rel, f.Reason)
		}
		printList(w, "Failed", lines, -1)
		return fmt.Errorf("%d files could not be synced; fix the reasons above and run `cloudsync sync` again", len(res.Failed))
	}
	if err := db.SetMeta(ctx, metaFirstSync+root.Name, time.Now().Format(time.RFC3339)); err != nil {
		return err
	}
	fmt.Fprintf(w, "First sync of %s complete.\n", root.Name)
	return nil
}

// openState opens the state database and takes a lock so only one CloudSync
// process syncs at a time.
func openState() (*state.DB, func(), error) {
	dir, err := config.StateDir()
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	lock := flock.New(filepath.Join(dir, "cloudsync.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, nil, fmt.Errorf("locking %s: %w", dir, err)
	}
	if !locked {
		return nil, nil, fmt.Errorf("another CloudSync is already running (lock file %s)", lock.Path())
	}
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		lock.Unlock()
		return nil, nil, err
	}
	return db, func() { db.Close(); lock.Unlock() }, nil
}

func confirm(w io.Writer, prompt string) bool {
	fmt.Fprint(w, prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// osLabel names this OS in conflict copies, e.g. "notes (conflict Windows ...)".
func osLabel() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	}
	return runtime.GOOS
}
