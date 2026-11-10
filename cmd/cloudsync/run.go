package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/engine"
	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/reconcile"
	"github.com/USER1043/cloudsync/internal/watch"
)

const (
	quiet        = 3 * time.Second  // a live pass waits for the folder to be still this long
	maxDelay     = 30 * time.Second // but no longer than this after the first change, if changes never stop
	rescanEvery  = time.Hour        // a pass even without changes, in case the watcher missed one
	flushTimeout = time.Minute      // the last pass at exit gives up after this
	minRetry     = 30 * time.Second // after a failure, wait this long, doubling each time...
	maxRetry     = 5 * time.Minute  // ...up to this
)

var errNoFirstSync = errors.New("this folder hasn't had its first sync yet. Run `cloudsync sync` once first " +
	"(it shows what it will do and asks), then start `cloudsync run`")

// daemon is `cloudsync run`: a catch-up with Drive at start, then live
// passes that upload local changes as they happen.
type daemon struct {
	cfg      config.Config
	m        *filter.Matcher
	d        *gdrive.Client
	log      io.Writer
	w        *watch.Watcher
	sessions []*session
}

func runRun(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("run", flag.ContinueOnError)
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fl.Arg(0))
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
	w, err := watch.New()
	if err != nil {
		return err
	}
	defer w.Close()
	dm := &daemon{cfg: e.cfg, m: m, d: d, log: newLogWriter(os.Stdout), w: w}
	dm.logf("CloudSync %s started (config %s).", version, e.cfg.Path())

	err = dm.catchUpAll(ctx)
	if err == nil {
		err = dm.live(ctx)
	}
	switch {
	case ctx.Err() != nil && (err == nil || errors.Is(err, context.Canceled)):
		dm.logf("Stopped.")
		return nil
	case gdrive.SignInRejected(err):
		return fmt.Errorf("Google no longer accepts CloudSync's sign-in (it expired or was revoked). "+
			"Run `cloudsync login`, then start `cloudsync run` again (%w)", err)
	}
	return err
}

// fatal reports whether err needs the owner: retrying won't fix it.
func fatal(err error) bool {
	var re *rootError
	return errors.As(err, &re) || errors.Is(err, errNoFirstSync) || gdrive.SignInRejected(err) || signInMissing(err)
}

// explain describes a failure that CloudSync will retry. For a network
// failure, the cause is enough; the request's long URL is noise.
func explain(err error) string {
	if !gdrive.Temporary(err) {
		return err.Error()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Sprintf("can't reach Google Drive (offline, or Google is having problems): %v", err)
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
					root.Name, explain(err), delay)
				failed = append(failed, root)
			}
		}
		if len(failed) == 0 {
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
	dm.logf("%s: catching up with Drive...", root.Name)
	names, err := loadNames(ctx, db, root.Name)
	if err != nil {
		return nil, err
	}
	in, err := inspect(ctx, root, dm.m, dm.d, names)
	if err != nil {
		return nil, err
	}
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
	if len(res.Failed) > 0 {
		dm.logf("%s: %s could not be synced (listed above); CloudSync tries again at the next start, or run `cloudsync sync`.",
			root.Name, count(len(res.Failed), "file", "files"))
	}
	base, err := db.Items(ctx, root.Name)
	if err != nil {
		return nil, err
	}
	s := newSession(in, l.run, base)
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
		case <-dm.w.C():
			if failures == 0 { // otherwise the retry picks it up
				sched.change(time.Now())
			}
		case err := <-dm.w.Errors():
			dm.logf("Watching for changes: %v. Changes there are picked up by the next rescan.", err)
		case <-hourly.C:
			if failures == 0 {
				sched.at(time.Now())
			}
		case <-timer.C:
			sched = schedule{}
			soon, failed, err := dm.pass(ctx, settle)
			switch {
			case ctx.Err() != nil:
				continue // flush
			case err != nil && fatal(err):
				return err
			case err != nil || failed:
				delay := min(minRetry<<min(failures, 4), maxRetry)
				failures++
				if err != nil {
					dm.logf("Sync failed: %s. Trying again in %s; your changes are kept until then.", explain(err), delay)
				} else {
					dm.logf("Trying the failed files again in %s.", delay)
				}
				sched.at(time.Now().Add(delay))
			default:
				failures = 0
				if soon {
					// A file was still being written; look again once it settles.
					sched.at(time.Now().Add(settle + 500*time.Millisecond))
				}
			}
		}
	}
}

// flush runs one last pass when CloudSync is asked to stop, so changes made
// just before are uploaded. A second Ctrl-C (or stop signal) skips it.
func (dm *daemon) flush() error {
	dm.logf("Stopping: uploading the last changes first (Ctrl-C again to quit now)...")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	if _, failed, err := dm.pass(ctx, 0); err != nil || failed {
		dm.logf("Not everything was uploaded; the rest syncs at the next start.")
	}
	return nil
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
	start := time.Now()
	res, err := runLater(ctx, dm.log, l, dm.d, db)
	if err != nil {
		return false, false, err
	}
	dm.report(s.root.Name, res, start)
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
		return fmt.Errorf("Google Drive is full: this needs %s but %s is free. Free up space in Drive, Gmail or Photos "+
			"(they share the storage)", humanBytes(upload), humanBytes(max(acc.Limit-acc.Usage, 0)))
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
