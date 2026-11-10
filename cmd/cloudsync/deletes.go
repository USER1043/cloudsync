package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/USER1043/cloudsync/internal/reconcile"
)

// runDeletes shows the deletes the big-delete guard is holding, or answers
// them. Approving or cancelling runs a full sync, so every held delete is
// checked again against the current state of both sides first: a file
// edited or restored since is never trashed.
func runDeletes(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("deletes", flag.ContinueOnError)
	all := fl.Bool("all", false, "list every held delete instead of the first 20")
	yes := fl.Bool("yes", false, "start without asking for confirmation")
	// The answer may come before or after the flags.
	var answer string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		answer, args = args[0], args[1:]
	}
	if err := fl.Parse(args); err != nil {
		return err
	}
	if answer == "" {
		answer = fl.Arg(0)
	} else if fl.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fl.Arg(0))
	}
	decision := reconcile.Undecided
	switch answer {
	case "":
	case "approve":
		decision = reconcile.Approve
	case "cancel":
		decision = reconcile.Cancel
	default:
		return fmt.Errorf("unknown argument %q: use `cloudsync deletes`, `cloudsync deletes approve` or `cloudsync deletes cancel`", answer)
	}
	e, err := loadEnv()
	if err != nil {
		return err
	}
	w := os.Stdout

	if decision == reconcile.Undecided {
		db, err := peekState()
		if err != nil {
			return err
		}
		if db == nil {
			fmt.Fprintln(w, "No deletes are waiting for you.")
			return nil
		}
		defer db.Close()
		limit := listLimit
		if *all {
			limit = -1
		}
		any := false
		for _, root := range e.cfg.Roots {
			ev, items, ok, err := db.PendingDeletes(ctx, root.Name)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			any = true
			fmt.Fprintf(w, "\n== %s: %d deletes held on %s since %s\n", root.Name, ev.Local+ev.Remote, ev.OS,
				time.Unix(ev.At, 0).Format("2006-01-02 15:04"))
			var here, drive []string
			for _, it := range items {
				rel := it.Rel
				if it.Dir {
					rel += "/"
				}
				if it.Here {
					here = append(here, rel)
				} else {
					drive = append(drive, rel)
				}
			}
			printList(w, "Deleted in Drive; would move to "+trashName()+" here", here, limit)
			printList(w, "Deleted here; would move to Drive's trash", drive, limit)
		}
		if !any {
			fmt.Fprintln(w, "No deletes are waiting for you.")
			return nil
		}
		fmt.Fprintf(w, "\nTo let them go ahead: cloudsync deletes approve\nTo put the files back instead: cloudsync deletes cancel\n"+
			"Either way, everything else is synced at the same time.\n")
		return nil
	}

	m, err := matcher(e.cfg)
	if err != nil {
		return err
	}
	d, err := e.drive(ctx)
	if err != nil {
		return err
	}
	db, closeState, err := openState(ctx, w)
	if err != nil {
		return err
	}
	defer closeState()
	printConfigSource(w, e.cfg)
	any := false
	for _, root := range e.cfg.Roots {
		if _, _, ok, err := db.PendingDeletes(ctx, root.Name); err != nil {
			return err
		} else if !ok {
			continue
		}
		any = true
		names, err := loadNames(ctx, db, root.Name)
		if err != nil {
			return err
		}
		in, err := inspect(ctx, root, m, d, names)
		if err != nil {
			return fmt.Errorf("root %q: %w", root.Name, err)
		}
		if err := syncLater(ctx, w, in, d, db, decision, *yes); err != nil {
			return fmt.Errorf("root %q: %w", root.Name, err)
		}
	}
	if !any {
		fmt.Fprintln(w, "No deletes are waiting for you.")
	}
	return nil
}
