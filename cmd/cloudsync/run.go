package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/engine"
	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/notify"
	"github.com/USER1043/cloudsync/internal/reconcile"
	"github.com/USER1043/cloudsync/internal/shutdown"
	"github.com/USER1043/cloudsync/internal/state"
	"github.com/USER1043/cloudsync/internal/watch"
)

const (
	quiet        = 3 * time.Second  // a live pass waits for the folder to be still this long
	maxDelay     = 30 * time.Second // but no longer than this after the first change, if changes never stop
	rescanEvery  = time.Hour        // a pass even without changes, in case the watcher missed one
	flushTimeout = 40 * time.Second // the last pass at exit gives up after this, inside Fedora's 45 s stop timeout
	minRetry     = 30 * time.Second // after a failure, wait this long, doubling each time...
	maxRetry     = 5 * time.Minute  // ...up to this

	offlineNotice = 30 * time.Minute // tell the owner once Drive has been out of reach this long
	troubleNotice = 10 * time.Minute // ... or failing for any other reason this long
)

var errNoFirstSync = errors.New("this folder hasn't had its first sync yet. Run `cloudsync sync` once first " +
	"(it shows what it will do and asks), then `cloudsync start`")

// daemon is `cloudsync run`: a catch-up with Drive at start, then live
// passes that upload local changes as they happen.
type daemon struct {
	cfg      config.Config
	m        *filter.Matcher
	d        *gdrive.Client
	log      io.Writer
	w        *watch.Watcher
	sessions []*session // complete before live passes or shutdown hooks run

	guard    *shutdown.Guard    // nil if shutdowns can't be watched
	stop     context.CancelFunc // ends the daemon, which then flushes
	done     chan struct{}      // closed when the daemon has finished
	stopping chan struct{}      // a shutdown has started
	ctl      chan ctlOp         // requests from the tray (Windows): sync now, pause, resume
	failing  atomic.Bool        // the last pass failed; Windows doesn't hold a shutdown for it

	id string // computerID; set by the first catch-up

	notes *notify.Notifier // nil if notifications aren't available
	st    *statusKeeper    // for `cloudsync status`

	// When the current run of failures began, by kind; zero if none. Used
	// only by the catch-up and the live loop, one after the other.
	offlineSince, troubleSince time.Time

	mu     sync.Mutex
	listed map[*session]bool // sessions whose pending list was saved at this shutdown
}

func runRun(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("run", flag.ContinueOnError)
	toFile := fl.Bool("log-file", false, "write the log to "+logFileName+" in the state folder instead of the terminal")
	// For the Windows background copy, whose Startup entry can't set
	// environment variables: the same as CLOUDSYNC_CONFIG and
	// CLOUDSYNC_STATE_DIR.
	cfgFile := fl.String("config", "", "use this config file")
	stateDir := fl.String("state-dir", "", "use this state folder")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fl.Arg(0))
	}
	if *cfgFile != "" {
		os.Setenv("CLOUDSYNC_CONFIG", *cfgFile)
	}
	if *stateDir != "" {
		os.Setenv("CLOUDSYNC_STATE_DIR", *stateDir)
	}
	e, err := loadEnv()
	if err != nil {
		return err
	}
	m, err := matcher(e.cfg)
	if err != nil {
		return err
	}
	dir, err := config.StateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lockRun(dir)
	if err != nil {
		return err
	}
	defer unlock()
	var out io.Writer = os.Stdout
	if *toFile {
		lf, err := openLogFile(dir)
		if err != nil {
			return err
		}
		defer lf.Close()
		out = lf
	}
	d, err := e.drive(ctx)
	if err != nil {
		return err
	}
	w, err := watch.New()
	if err != nil {
		return err
	}
	defer w.Close()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	dm := &daemon{
		cfg: e.cfg, m: m, d: d, log: newLogWriter(out), w: w,
		stop: stop, done: make(chan struct{}), stopping: make(chan struct{}, 1), ctl: make(chan ctlOp, 8), listed: map[*session]bool{},
		st: newStatusKeeper(dir),
	}
	defer close(dm.done)
	dm.logf("CloudSync %s started (config %s).", buildVersion(), e.cfg.Path())
	if p, err := place(); err == nil {
		// `cloudsync stop` and `update` on Windows; Linux uses SIGTERM.
		watchStopRequests(ctx, p, stop)
		// The tray's Sync now, Pause and Resume (Windows).
		watchControl(ctx, p, dm.control)
	}
	// On Windows the tray process shows the notifications, from the
	// warnings kept in the status file.
	if !trayOwnsNotices {
		if dm.notes, err = notify.New(); err != nil {
			dm.logf("Desktop notifications aren't available (%v); warnings go to this log and `cloudsync status` only.", err)
		} else {
			defer dm.notes.Close()
		}
	}
	dm.st.set("starting", "")

	err = dm.catchUpAll(ctx)
	if err == nil {
		g, gerr := shutdown.Start(shutdown.Hooks{Starting: dm.shutdownStarting, Holding: dm.holding, Ending: dm.ending})
		if gerr != nil {
			dm.logf("Can't watch for shutdowns (%v): changes made just before one may upload only at the next start.", gerr)
		} else {
			dm.guard = g
			defer g.Close()
		}
		err = dm.live(ctx)
	}
	switch {
	case ctx.Err() != nil && (err == nil || errors.Is(err, context.Canceled)):
		dm.st.set("stopped", "")
		dm.logf("Stopped.")
		return nil
	case fatal(err):
		// Only the owner can fix these; say so where they'll see it.
		msg := describe(err)
		if classify(err) == kindSignIn {
			msg += ", then `cloudsync start`"
		}
		dm.st.set("stopped: needs you", msg)
		dm.alert(notify.Message{Key: "stopped", Title: "CloudSync stopped syncing", Body: msg, Critical: true})
	default:
		dm.st.set("stopped", describe(err))
	}
	return err
}

// alert shows a notification and keeps it for `cloudsync status`.
func (dm *daemon) alert(m notify.Message) {
	if _, err := dm.notes.Send(m); err != nil {
		dm.logf("Couldn't show a notification: %v", err)
	}
	dm.st.warn(m)
}

// resolve takes down the notification under key once its problem is gone.
func (dm *daemon) resolve(key string) {
	dm.notes.Clear(key)
	dm.st.resolve(key)
}

// trouble records a failed attempt that CloudSync will retry: it updates
// the status, and tells the owner once the problem has lasted long enough
// to matter. Brief outages stay quiet.
func (dm *daemon) trouble(err error) {
	now := time.Now()
	msg := describe(err)
	switch classify(err) {
	case kindRateLimit:
		dm.st.set("slowed down by Google", msg)
	case kindQuota:
		dm.st.set("paused: Drive is full", msg)
		dm.alert(notify.Message{Key: "quota", Title: "Google Drive is full", Body: msg, Critical: true})
	case kindOffline:
		if dm.offlineSince.IsZero() {
			dm.offlineSince = now
		}
		dm.st.set("offline", msg)
		if d := now.Sub(dm.offlineSince); d >= offlineNotice {
			dm.alert(notify.Message{Key: "offline", Title: "CloudSync can't reach Google Drive",
				Body: fmt.Sprintf("Not since %s (%s). Changes made here are kept and sync once the connection is back.",
					dm.offlineSince.Format("15:04"), msg)})
		}
	default:
		if dm.troubleSince.IsZero() {
			dm.troubleSince = now
		}
		title := "CloudSync keeps failing"
		if classify(err) == kindGoogle {
			title = "Google Drive is having problems"
			dm.st.set("waiting for Google Drive", msg)
		} else {
			dm.st.set("failing", msg)
		}
		if now.Sub(dm.troubleSince) >= troubleNotice {
			dm.alert(notify.Message{Key: "trouble", Title: title, Body: msg + ". CloudSync keeps trying."})
		}
	}
}

// recovered clears what trouble set, after an attempt that worked.
func (dm *daemon) recovered() {
	dm.offlineSince, dm.troubleSince = time.Time{}, time.Time{}
	for _, key := range []string{"offline", "trouble", "quota"} {
		dm.resolve(key)
	}
	dm.st.set("in sync", "")
}

// afterSync tells the owner what a finished sync of root left for them:
// held deletes, conflict copies and files that keep failing.
func (dm *daemon) afterSync(ctx context.Context, root string, res *engine.Result, db *state.DB) {
	key := "held:" + root
	if ev, _, ok, err := db.PendingDeletes(ctx, root); err == nil && ok {
		n := ev.Local + ev.Remote
		verb := "are"
		if n == 1 {
			verb = "is"
		}
		dm.alert(notify.Message{Key: key, Title: "Deletes held for your review", Critical: true,
			Body: fmt.Sprintf("%s deleted at once in %s %s waiting for your answer. Run `cloudsync deletes` to review, "+
				"then approve or cancel.", count(n, "file", "files"), root, verb)})
	} else if err == nil {
		dm.resolve(key)
	}

	if c := res.Conflicted; len(c) > 0 {
		body := fmt.Sprintf("%s was changed both here and in Drive. Your version is next to it as a \"(conflict %s ...)\" copy; "+
			"compare the two and delete the one you don't want.", c[0], osLabel())
		if len(c) > 1 {
			body = fmt.Sprintf("%s were changed both here and in Drive: %s. Your version of each is next to it as a "+
				"\"(conflict %s ...)\" copy; compare them and delete the ones you don't want.", count(len(c), "file", "files"), listed(c, 3), osLabel())
		}
		dm.alert(notify.Message{Key: "conflict:" + root, Title: "Edited in two places: both versions kept", Body: body})
	}

	key = "failed:" + root
	if len(res.Failed) == 0 {
		dm.resolve(key)
		return
	}
	// When Drive is out of reach every file fails; that is trouble's to
	// report, not a list of files.
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if ctx.Err() != nil || dm.d.Ping(pctx) != nil {
		return
	}
	lines := make([]string, len(res.Failed))
	for i, f := range res.Failed {
		lines[i] = f.Rel + ": " + f.Reason
	}
	dm.alert(notify.Message{Key: key, Title: "Some files can't be synced",
		Body: listed(lines, 3) + ". CloudSync keeps trying."})
}

// listed joins up to n items, then says how many more there are.
func listed(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}

// fatal reports whether err needs the owner: retrying won't fix it.
func fatal(err error) bool {
	var re *rootError
	return errors.As(err, &re) || errors.Is(err, errNoFirstSync) || gdrive.SignInRejected(err) || signInMissing(err)
}

// count is "1 file" or "N files".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// catchUpAll syncs every root with Drive, as `cloudsync sync --yes` would,
// retrying a root that fails until it works, ctx ends or the failure needs
// the owner.
func (dm *daemon) catchUpAll(ctx context.Context) error {
	roots := dm.cfg.Roots
	delay := minRetry
	for {
		var failed []config.Root
		for _, root := range roots {
			s, err := dm.catchUp(ctx, root)
			switch {
			case err == nil:
				dm.sessions = append(dm.sessions, s)
			case ctx.Err() != nil:
				return ctx.Err()
			case fatal(err):
				return fmt.Errorf("root %q: %w", root.Name, err)
			default:
				dm.logf("%s: catching up with Drive failed: %s. Trying again in %s; nothing is lost meanwhile.",
					root.Name, describe(err), delay)
				dm.trouble(err)
				failed = append(failed, root)
			}
		}
		if len(failed) == 0 {
			dm.recovered()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		roots, delay = failed, min(2*delay, maxRetry)
	}
}

// catchUp brings root and its Drive folder in sync: a full Drive listing,
// a local scan and a two-way sync. Deletes the big-delete guard holds stay
// held for `cloudsync deletes`.
func (dm *daemon) catchUp(ctx context.Context, root config.Root) (*session, error) {
	db, closeState, err := openState(ctx, dm.log)
	if err != nil {
		return nil, err
	}
	defer closeState()
	if done, err := firstSyncDone(ctx, db, root.Name); err != nil {
		return nil, err
	} else if !done {
		return nil, errNoFirstSync
	}
	if dm.id == "" {
		if dm.id, err = computerID(ctx, db); err != nil {
			return nil, err
		}
	}
	dm.logf("%s: catching up with Drive...", root.Name)
	dm.st.set("catching up with Drive", root.Name)
	names, err := loadNames(ctx, db, root.Name)
	if err != nil {
		return nil, err
	}
	in, err := inspect(ctx, root, dm.m, dm.d, names)
	if err != nil {
		return nil, err
	}
	// Made now, so a shutdown has only the list itself to save.
	metaID, err := dm.d.EnsurePath(ctx, metaSegments(root))
	if err != nil {
		return nil, err
	}
	dm.warnStale(ctx, root, metaID, in.tree)
	l, err := planLater(ctx, in, db, reconcile.Undecided)
	if err != nil {
		return nil, err
	}
	printLater(dm.log, l, listLimit)
	if err := dm.checkQuota(ctx, l.uploadBytes()); err != nil {
		return nil, err
	}
	start := time.Now()
	res, err := runLater(ctx, dm.log, l, dm.d, db)
	if err != nil {
		return nil, err
	}
	dm.report(root.Name, res, start)
	dm.afterSync(ctx, root.Name, res, db)
	if len(res.Failed) > 0 {
		dm.logf("%s: %s could not be synced (listed above); CloudSync tries again at the next start, or run `cloudsync sync`.",
			root.Name, count(len(res.Failed), "file", "files"))
	} else if err := dm.d.RemoveSmall(ctx, metaID, pendingName(osLabel(), dm.id, root.Name)); err != nil {
		// What the last shutdown left unsent is in Drive now, so its list is
		// out of date; the other system would only warn needlessly.
		dm.logf("%s: couldn't remove the old list of files not uploaded at the last shutdown: %v", root.Name, err)
	}
	base, err := db.Items(ctx, root.Name)
	if err != nil {
		return nil, err
	}
	s := newSession(in, l.run, base)
	s.metaID = metaID
	dm.watch(s, in)
	dm.logf("%s: in sync; watching %s for changes.", root.Name, in.local)
	return s, nil
}

// live runs a pass over every root a few seconds after local changes settle,
// every hour regardless, and once more, to upload the last changes, when
// ctx ends.
func (dm *daemon) live(ctx context.Context) error {
	hourly := time.NewTicker(rescanEvery)
	defer hourly.Stop()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	var (
		sched    schedule
		failures int
		shutting bool // a shutdown has started: don't wait for files to settle
		paused   bool // the owner paused syncing from the tray
	)
	// Right away, for changes made while the catch-up ran.
	sched.at(time.Now())
	for {
		if due := sched.next(); due.IsZero() {
			timer.Stop()
		} else {
			timer.Reset(time.Until(due))
		}
		select {
		case <-ctx.Done():
			return dm.flush()
		case <-dm.stopping:
			// A shutdown overrides a pause: nothing may be left behind.
			shutting, paused = true, false
			sched.at(time.Now())
		case op := <-dm.ctl:
			switch op {
			case ctlSyncNow:
				// Also while paused: one pass, then it stays paused.
				dm.logf("Syncing now, as asked.")
				sched.at(time.Now())
			case ctlPause:
				if !paused {
					paused = true
					sched = schedule{}
					dm.logf("Paused, as asked: changes are not uploaded until you resume.")
					dm.st.set(statePaused, "")
				}
			case ctlResume:
				if paused {
					paused = false
					dm.logf("Resumed: catching up with changes made while paused.")
					// A full rescan, so nothing made during the pause is missed.
					sched.at(time.Now())
				}
			}
		case <-dm.w.C():
			if failures == 0 && !paused { // otherwise the retry picks it up
				sched.change(time.Now())
			}
		case err := <-dm.w.Errors():
			dm.logf("Watching for changes: %v. Changes there are picked up by the next rescan.", err)
		case <-hourly.C:
			if failures == 0 && !paused {
				sched.at(time.Now())
			}
		case <-timer.C:
			sched = schedule{}
			settleFor := settle
			if shutting {
				settleFor = 0
			}
			soon, failed, err := dm.pass(ctx, settleFor)
			dm.failing.Store(err != nil || failed)
			if dm.guard != nil {
				dm.guard.Recheck()
			}
			switch {
			case ctx.Err() != nil:
				continue // flush
			case err != nil && fatal(err):
				return err
			case err != nil || failed:
				delay := min(minRetry<<min(failures, 4), maxRetry)
				failures++
				if err != nil {
					dm.logf("Sync failed: %s. Trying again in %s; your changes are kept until then.", describe(err), delay)
					dm.trouble(err)
				} else {
					dm.logf("Trying the failed files again in %s.", delay)
					dm.st.set("retrying files that failed", "")
				}
				if !paused {
					sched.at(time.Now().Add(delay))
				}
			default:
				failures = 0
				dm.recovered()
				if soon && !paused {
					// A file was still being written; look again once it settles.
					sched.at(time.Now().Add(settle + 500*time.Millisecond))
				}
			}
			if paused {
				// A "sync now" while paused: the pass is done, the pause isn't.
				dm.st.set(statePaused, "")
			}
		}
	}
}

// flush runs one last pass when CloudSync is asked to stop, so changes made
// just before are uploaded. A second Ctrl-C (or stop signal) skips it, and
// so does being offline: waiting would only hold up the shutdown.
func (dm *daemon) flush() error {
	dm.logf("Stopping: uploading the last changes first (Ctrl-C again to quit now)...")
	dm.st.set("stopping", "")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	err := dm.d.Ping(pctx)
	pcancel()
	if err != nil {
		dm.logf("Not uploading: %s. Changes not uploaded yet stay here and upload at the next start.", describe(err))
		return nil
	}
	if _, failed, err := dm.pass(ctx, 0); err != nil || failed {
		dm.logf("Not everything was uploaded; the rest syncs at the next start.")
		return nil
	}
	// Everything is in Drive, so a list saved as the shutdown started is
	// out of date.
	dm.mu.Lock()
	var listed []*session
	for s := range dm.listed {
		listed = append(listed, s)
	}
	dm.mu.Unlock()
	for _, s := range listed {
		if err := dm.d.RemoveSmall(ctx, s.metaID, pendingName(osLabel(), dm.id, s.root.Name)); err != nil {
			dm.logf("%s: couldn't remove the list of files not uploaded: %v", s.root.Name, err)
		}
	}
	return nil
}

// shutdownStarting saves, for each root, the list of changes Drive doesn't
// have yet, so the other system can warn about them even if the uploads
// don't finish in time, and starts uploading them without waiting for
// files to settle.
func (dm *daemon) shutdownStarting(ctx context.Context) {
	dm.logf("The computer is shutting down.")
	select {
	case dm.stopping <- struct{}{}:
	default:
	}
	for _, s := range dm.sessions {
		p, err := dm.unsynced(ctx, s)
		if err == nil && len(p.Files) > 0 {
			var data []byte
			if data, err = p.encode(); err == nil {
				err = dm.d.WriteSmall(ctx, s.metaID, pendingName(osLabel(), dm.id, s.root.Name), data)
			}
		}
		n := count(len(p.Files), "file", "files")
		switch {
		case err != nil:
			dm.logf("%s: couldn't save the list of changes not uploaded yet (%s); they upload at the next start.", s.root.Name, describe(err))
		case len(p.Files) > 0:
			dm.mu.Lock()
			dm.listed[s] = true
			dm.mu.Unlock()
			dm.logf("%s: %s not uploaded yet; saved the list in Drive, so other computers warn about what doesn't make it.", s.root.Name, n)
		}
	}
}

// unsynced lists the changes in s that Drive doesn't have yet. It reads
// the state DB without the lock, so it works while a pass is uploading.
func (dm *daemon) unsynced(ctx context.Context, s *session) (pendingList, error) {
	db, err := peekState()
	if err != nil || db == nil {
		return pendingList{}, err
	}
	defer db.Close()
	l, _, err := planLive(ctx, s, dm.m, db, time.Now(), 0)
	if err != nil {
		return pendingList{}, err
	}
	return pendingFrom(l, dm.id, time.Now()), nil
}

// holding is why Windows should hold a shutdown: uploads still to do. It
// doesn't hold one when uploads are failing or Drive can't be reached,
// since waiting can't help then.
func (dm *daemon) holding() string {
	if dm.failing.Load() {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	n := 0
	for _, s := range dm.sessions {
		p, err := dm.unsynced(ctx, s)
		if err != nil {
			return ""
		}
		n += len(p.Files)
	}
	if n == 0 || dm.d.Ping(ctx) != nil {
		return ""
	}
	return "Uploading " + count(n, "file", "files") + " to Google Drive"
}

// ending is Windows' last word: the session ends now. It stops the daemon,
// which flushes, and waits for that so the process isn't cut off first.
func (dm *daemon) ending() {
	dm.stop()
	select {
	case <-dm.done:
	case <-time.After(flushTimeout + 5*time.Second):
	}
}

// warnStale reads the lists other computers left at shutdown and warns
// about changes Drive still doesn't have, judged by tree, the Drive listing.
func (dm *daemon) warnStale(ctx context.Context, root config.Root, metaID string, tree *gdrive.Tree) {
	names, err := dm.d.ListSmall(ctx, metaID)
	if err != nil {
		dm.logf("%s: couldn't look for other computers' lists of files not uploaded: %v", root.Name, err)
		return
	}
	own := pendingName(osLabel(), dm.id, root.Name)
	for _, name := range names {
		if !isPendingName(name) || name == own {
			continue
		}
		data, err := dm.d.ReadSmall(ctx, metaID, name)
		if err != nil {
			dm.logf("%s: couldn't read %s: %v", root.Name, name, err)
			continue
		}
		p, err := decodePending(data)
		if err != nil {
			dm.logf("%s: %s is unreadable: %v", root.Name, name, err)
			continue
		}
		if p.Root != root.Name || p.Computer == dm.id {
			continue
		}
		stale := p.stale(tree)
		if len(stale) == 0 {
			continue
		}
		paths := make([]string, len(stale))
		for i, f := range stale {
			paths[i] = f.Path
			if f.Deleted {
				paths[i] += " (deleted there)"
			}
		}
		if len(paths) > 10 {
			paths = append(paths[:10], fmt.Sprintf("and %d more", len(stale)-10))
		}
		who := p.OS
		if p.Host != "" {
			who += " (" + p.Host + ")"
		}
		dm.logf("%s: WARNING: %s shut down on %s before uploading %s, so these are missing or out of date here until it "+
			"next starts online (editing them here first leaves conflict copies to sort out): %s",
			root.Name, who, p.At.Local().Format("2 Jan 15:04"), count(len(stale), "change", "changes"), strings.Join(paths, ", "))
		dm.alert(notify.Message{Key: "stale:" + root.Name + ":" + p.Computer, Critical: true,
			Title: who + " didn't upload everything",
			Body: fmt.Sprintf("It shut down on %s before uploading %s, so these are missing or out of date here: %s. "+
				"They arrive when it next starts online; editing them here first leaves conflict copies.",
				p.At.Local().Format("2 Jan 15:04"), count(len(stale), "change", "changes"), listed(paths, 5))})
	}
}

// pass runs a live pass over every root, treating files changed in the last
// settleFor as still being written. soon reports such files; failed reports
// files that could not be synced.
func (dm *daemon) pass(ctx context.Context, settleFor time.Duration) (soon, failed bool, err error) {
	for _, s := range dm.sessions {
		sn, f, err := dm.passRoot(ctx, s, settleFor)
		if err != nil {
			return false, false, fmt.Errorf("%s: %w", s.root.Name, err)
		}
		soon, failed = soon || sn, failed || f
	}
	return soon, failed, nil
}

func (dm *daemon) passRoot(ctx context.Context, s *session, settleFor time.Duration) (soon, failed bool, err error) {
	db, closeState, err := openState(ctx, dm.log)
	if err != nil {
		return false, false, err
	}
	defer closeState()
	l, soon, err := planLive(ctx, s, dm.m, db, time.Now(), settleFor)
	if err != nil {
		return false, false, err
	}
	dm.watch(s, l.in)
	if len(l.run.Actions) == 0 && len(l.hold) == 0 {
		// Nothing to do, but a problem may have gone away meanwhile: held
		// deletes answered, a failing file fixed or removed.
		dm.afterSync(ctx, s.root.Name, &engine.Result{}, db)
		return soon, false, nil
	}
	if l.run.Changes() || len(l.hold) > 0 {
		// Uploading into a Drive folder that was trashed meanwhile would
		// quietly put the files in the trash too.
		f, err := dm.d.Stat(ctx, s.rootID)
		if err != nil {
			return false, false, err
		}
		if f.Trashed {
			return false, false, rootMissing("My Drive/" + strings.Join(s.root.RemoteSegments(), "/"))
		}
		if err := dm.checkQuota(ctx, l.uploadBytes()); err != nil {
			return false, false, err
		}
	}
	if l.run.Changes() {
		n := len(pendingFrom(l, "", time.Now()).Files)
		dm.st.set("uploading", count(n, "change", "changes"))
	}
	start := time.Now()
	res, err := runLater(ctx, dm.log, l, dm.d, db)
	if err != nil {
		return false, false, err
	}
	dm.report(s.root.Name, res, start)
	dm.afterSync(ctx, s.root.Name, res, db)
	if len(l.hold) > 0 {
		files := 0
		for _, a := range l.hold {
			if !a.Dir {
				files++
			}
		}
		dm.logf("%s: big-delete guard: holding %s here (more than 50 files or 10%% of the folder at once). "+
			"Review with `cloudsync deletes`, then approve or cancel.", s.root.Name, count(files, "deleted file", "deleted files"))
	}
	return soon, len(res.Failed) > 0, nil
}

// checkQuota fails if uploading upload bytes would overfill Drive.
func (dm *daemon) checkQuota(ctx context.Context, upload int64) error {
	if upload == 0 {
		return nil
	}
	acc, err := dm.d.Account(ctx)
	if err != nil {
		return err
	}
	if acc.Limit > 0 && upload > acc.Limit-acc.Usage {
		return &quotaError{need: upload, free: max(acc.Limit-acc.Usage, 0)}
	}
	return nil
}

// report logs what a sync did, if anything.
func (dm *daemon) report(root string, res *engine.Result, start time.Time) {
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", what, n))
		}
	}
	add(res.Uploaded, "uploaded")
	add(res.Downloaded, "downloaded")
	add(res.Trashed, "moved to "+trashName())
	add(res.TrashedInDrive, "moved to Drive's trash")
	add(res.Conflicts, "conflicts (both copies kept)")
	add(res.Folders, "folders created")
	add(res.FoldersRemoved, "folders removed")
	add(len(res.Failed), "failed")
	if len(parts) == 0 {
		return
	}
	dm.logf("%s: %s; %s in %s.", root, strings.Join(parts, ", "), humanBytes(res.Bytes), time.Since(start).Round(100*time.Millisecond))
}

// watch points the watcher at the folders of in's scan.
func (dm *daemon) watch(s *session, in *inspection) {
	dirs := make([]string, len(in.scan.Dirs))
	for i, rel := range in.scan.Dirs {
		dirs[i] = filepath.Join(s.local, filepath.FromSlash(in.names.Local(rel)))
	}
	if err := dm.w.Watch(s.local, dirs); err != nil {
		dm.logf("%s: some folders can't be watched (%v); changes there are picked up by the hourly rescan.", s.root.Name, err)
	}
}

func (dm *daemon) logf(format string, args ...any) { fmt.Fprintf(dm.log, format+"\n", args...) }

// schedule is when the next live pass is due: after a change, once the
// folder has been quiet a moment, but at most maxDelay after the first
// change, so a file that is written non-stop doesn't hold everything else
// up; or at a time asked for with at, whichever is sooner.
type schedule struct {
	first, changeDue, atDue time.Time
}

func (s *schedule) change(now time.Time) {
	if s.first.IsZero() {
		s.first = now
	}
	s.changeDue = now.Add(quiet)
	if limit := s.first.Add(maxDelay); s.changeDue.After(limit) {
		s.changeDue = limit
	}
}

func (s *schedule) at(t time.Time) {
	if s.atDue.IsZero() || t.Before(s.atDue) {
		s.atDue = t
	}
}

// next is when the pass is due, or zero if none is.
func (s *schedule) next() time.Time {
	switch {
	case s.changeDue.IsZero():
		return s.atDue
	case s.atDue.IsZero() || s.changeDue.Before(s.atDue):
		return s.changeDue
	}
	return s.atDue
}

// logWriter prefixes each line written to it with the date and time, and
// drops blank lines.
type logWriter struct {
	mu  sync.Mutex
	out io.Writer
	buf []byte
}

func newLogWriter(out io.Writer) *logWriter { return &logWriter{out: out} }

func (l *logWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if line := bytes.TrimRight(l.buf[:i], " \r"); len(line) > 0 {
			fmt.Fprintf(l.out, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
		}
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}
