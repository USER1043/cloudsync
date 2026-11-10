// Package engine carries out syncs: first syncs planned by package plan, and
// later ones planned by package reconcile.
package engine

import (
	"cmp"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/localfs"
	"github.com/USER1043/cloudsync/internal/plan"
	"github.com/USER1043/cloudsync/internal/reconcile"
	"github.com/USER1043/cloudsync/internal/state"
	"github.com/USER1043/cloudsync/internal/trash"
	"github.com/USER1043/cloudsync/internal/winname"
)

// Remote is the part of Google Drive the engine uses. *gdrive.Client
// implements it.
type Remote interface {
	EnsureFolder(ctx context.Context, parentID, name string) (string, error)
	CreateFolder(ctx context.Context, parentID, name string) (string, error)
	Upload(ctx context.Context, parentID, name, localPath string, modTime time.Time, md5sum string, progress func(sent int64)) (gdrive.File, error)
	Update(ctx context.Context, id, localPath string, modTime time.Time, md5sum, want string, progress func(sent int64)) (gdrive.File, error)
	Download(ctx context.Context, id string, w io.Writer) error
	Trash(ctx context.Context, id string) error
	HasChildren(ctx context.Context, id string) (bool, error)
}

// Options configures one root's sync.
type Options struct {
	Root    string    // root name; the key for this root in the state DB
	Local   string    // local folder
	RootID  string    // Drive folder ID of the root
	Label   string    // OS name used in conflict copy names, e.g. "Windows"
	Workers int       // parallel transfers and folder creations; 8 if zero
	Log     io.Writer // one line per folder created and per transfer

	// Names holds the stand-in names this computer uses (Windows only; nil
	// on Linux). It must be the Map the scan was made with. Plans and the
	// state DB use true paths; Names turns them into local ones.
	Names *winname.Map

	// Trash moves a local file to the OS trash; trash.Move if nil. It is
	// called from parallel workers.
	Trash func(path string) error

	// Progress, if set, is called when transfers start, as bytes move (at
	// most every 200ms) and after each transfer finishes. Calls never overlap.
	Progress func(Progress)
}

const reportEvery = 200 * time.Millisecond

// Progress reports how far the transfers have got. Bytes are the bytes moved
// by transfers in flight plus the planned size of finished ones, failed ones
// included, so they reach TotalBytes at the end.
type Progress struct {
	Done, Total       int
	Bytes, TotalBytes int64
}

// Result summarises a sync. Failed lists files that could not be synced;
// running the sync again retries them.
type Result struct {
	Uploaded, Downloaded, InSync, Conflicts, Folders int
	Trashed, TrashedInDrive                          int // files moved to the local trash and to Drive's
	FoldersRemoved                                   int // empty folders removed here or in Drive
	Bytes                                            int64
	Failed                                           []filter.Issue
	Waiting                                          []filter.Issue // left for the next run (reconcile.Wait)
}

// FirstSync carries out a plan from plan.FirstSync. It never deletes or
// overwrites a file: conflicts keep both copies, and a download is skipped if
// a local file has appeared at its path. It returns an error only when it
// cannot go on at all; individual file failures are listed in the Result.
func FirstSync(ctx context.Context, r Remote, db *state.DB, opt Options, scan *localfs.Result, tree *gdrive.Tree, p *plan.Plan) (*Result, error) {
	remote := make(map[string]string, len(tree.Files))
	for _, f := range tree.Files {
		remote[f.Rel] = f.ID
	}
	acts := make([]reconcile.Action, 0, len(p.Items))
	for _, it := range p.Items {
		a := reconcile.Action{Rel: it.Rel, Size: it.Size, Note: it.Note, DriveID: remote[it.Rel]}
		switch it.Kind {
		case plan.Upload:
			a.Op, a.DriveID = reconcile.Upload, ""
		case plan.Download:
			a.Op = reconcile.Download
		case plan.Conflict:
			a.Op = reconcile.Conflict
		case plan.InSync:
			a.Op = reconcile.Record
		}
		acts = append(acts, a)
	}
	// Every local folder goes to Drive and every Drive folder comes here.
	localDirs := make([]string, len(tree.Folders))
	for i, f := range tree.Folders {
		localDirs[i] = f.Rel
	}
	return execute(ctx, r, db, opt, scan, tree, acts, scan.Dirs, localDirs)
}

// Sync carries out a plan from reconcile.Reconcile, in this order: state-only
// records, folders to create, then transfers and file trashes in parallel
// (largest first), then empty folders to remove, deepest first. If the
// big-delete guard holds the plan, pass the rest from SplitDeletes.
//
// Every change is checked against what the plan saw, so nothing made since is
// lost: a local file edited after the scan is neither overwritten nor
// trashed, and an upload that would replace a Drive file edited meanwhile
// (on the phone, say) becomes a conflict that keeps both. Trashed files go to
// the OS trash or Drive's trash, never deleted outright.
func Sync(ctx context.Context, r Remote, db *state.DB, opt Options, scan *localfs.Result, tree *gdrive.Tree, p *reconcile.Plan) (*Result, error) {
	var remoteDirs, localDirs []string
	for _, a := range p.Actions {
		switch a.Op {
		case reconcile.MkdirRemote:
			remoteDirs = append(remoteDirs, a.Rel)
		case reconcile.MkdirLocal:
			localDirs = append(localDirs, a.Rel)
		}
	}
	return execute(ctx, r, db, opt, scan, tree, p.Actions, remoteDirs, localDirs)
}

// execute runs acts. remoteDirs are folders to make in Drive and localDirs
// folders to make here, besides the Drive parents uploads need.
func execute(ctx context.Context, r Remote, db *state.DB, opt Options, scan *localfs.Result, tree *gdrive.Tree,
	acts []reconcile.Action, remoteDirs, localDirs []string) (*Result, error) {
	if opt.Workers <= 0 {
		// Large files are limited by the uplink, which one transfer nearly
		// fills; small files by about 2 s per request, which more parallel
		// requests hide, up to Google's per-user write limit.
		opt.Workers = 8
	}
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	if opt.Trash == nil {
		opt.Trash = trash.Move
	}
	s := &run{
		ctx: ctx, r: r, db: db, opt: opt, now: time.Now(),
		local:    make(map[string]localfs.File, len(scan.Files)),
		remote:   make(map[string]gdrive.File, len(tree.Files)),
		folders:  map[string]string{"": opt.RootID},
		copies:   make(map[string]string),
		inflight: make(map[string]int64),
	}
	for _, f := range scan.Files {
		s.local[f.Rel] = f
	}
	for _, f := range tree.Files {
		s.remote[f.Rel] = f
	}
	for _, f := range tree.Folders {
		s.folders[f.Rel] = f.ID
	}

	if err := s.makeFolders(acts, remoteDirs, localDirs); err != nil {
		return &s.res, err
	}

	// Choose every new local name before the transfers run in parallel, so
	// the choice doesn't depend on which transfer happens to go first.
	var jobs, emptyDirs []reconcile.Action
	for _, a := range acts {
		switch {
		case a.Op == reconcile.MkdirLocal || a.Op == reconcile.MkdirRemote:
			continue // done by makeFolders
		case a.Op == reconcile.Record:
			if err := s.recordSame(a); err != nil {
				return &s.res, err
			}
			continue
		case a.Op == reconcile.Forget:
			if err := s.db.Delete(ctx, opt.Root, a.Rel); err != nil {
				return &s.res, err
			}
			continue
		case a.Op == reconcile.Wait:
			s.res.Waiting = append(s.res.Waiting, filter.Issue{Rel: a.Rel, Reason: a.Note})
			continue
		case a.Dir: // a folder to remove once empty
			emptyDirs = append(emptyDirs, a)
			continue
		case a.Op == reconcile.Download:
			opt.Names.Assign(a.Rel)
		case a.Op == reconcile.Conflict:
			s.copies[a.Rel] = s.conflictName(a.Rel)
			opt.Names.Assign(s.copies[a.Rel])
		}
		jobs = append(jobs, a)
		s.totalBytes += moved(a)
	}
	// Largest first, so big files run while small ones fill the gaps rather
	// than one big file running alone at the end.
	slices.SortStableFunc(jobs, func(a, b reconcile.Action) int { return cmp.Compare(moved(b), moved(a)) })
	s.total = len(jobs)
	s.report()

	ch := make(chan reconcile.Action)
	var wg sync.WaitGroup
	for range opt.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range ch {
				s.do(a)
			}
		}()
	}
feed:
	for _, a := range jobs {
		select {
		case ch <- a:
		case <-ctx.Done():
			break feed
		}
	}
	close(ch)
	wg.Wait()
	if ctx.Err() == nil {
		s.removeFolders(emptyDirs)
	}
	return &s.res, ctx.Err()
}

// moved is the bytes an action transfers; trashing moves none.
func moved(a reconcile.Action) int64 {
	if a.Op == reconcile.TrashLocal || a.Op == reconcile.TrashRemote {
		return 0
	}
	return a.Size
}

type run struct {
	ctx context.Context
	r   Remote
	db  *state.DB
	opt Options
	now time.Time

	local   map[string]localfs.File
	remote  map[string]gdrive.File
	folders map[string]string // rel -> Drive folder ID; "" is the root. Read-only once transfers start.

	namesMu sync.Mutex        // guards copies once transfers start
	copies  map[string]string // conflict rel -> rel of the local version's copy

	mu         sync.Mutex // guards res, the progress counters and writes to opt.Log
	res        Result
	done       int
	total      int
	doneBytes  int64
	totalBytes int64
	inflight   map[string]int64 // bytes moved so far by each running transfer
	lastReport time.Time
}

// report calls opt.Progress. Callers hold mu once workers have started.
func (s *run) report() {
	if s.opt.Progress == nil {
		return
	}
	bytes := s.doneBytes
	for _, n := range s.inflight {
		bytes += n
	}
	s.lastReport = time.Now()
	s.opt.Progress(Progress{Done: s.done, Total: s.total, Bytes: bytes, TotalBytes: s.totalBytes})
}

// tracker returns a callback that records the bytes moved for one planned
// transfer of size bytes. It never goes backwards (a retry restarts from
// zero) or past the planned size, and it reports at most every reportEvery.
func (s *run) tracker(rel string, size int64) func(int64) {
	return func(n int64) {
		s.mu.Lock()
		defer s.mu.Unlock()
		n = min(n, size)
		if n <= s.inflight[rel] {
			return
		}
		s.inflight[rel] = n
		if time.Since(s.lastReport) >= reportEvery {
			s.report()
		}
	}
}

// makeFolders creates, before any transfer starts, the Drive folders in
// remoteDirs and those that uploads need, and the local folders in
// localDirs, and records them all.
func (s *run) makeFolders(acts []reconcile.Action, remoteDirs, localDirs []string) error {
	need := make(map[string]bool)
	addWithParents := func(rel string) {
		for ; rel != "." && rel != ""; rel = path.Dir(rel) {
			need[rel] = true
		}
	}
	for _, d := range remoteDirs {
		addWithParents(d)
	}
	for _, a := range acts {
		if a.Op == reconcile.Upload || a.Op == reconcile.Conflict {
			addWithParents(path.Dir(a.Rel))
		}
	}
	// Create the missing Drive folders a depth at a time, the folders of each
	// depth in parallel. Nothing can exist yet inside a folder created in this
	// run, so its subfolders skip the lookup.
	var levels [][]string
	for rel := range need {
		if _, ok := s.folders[rel]; ok {
			continue
		}
		depth := strings.Count(rel, "/")
		for len(levels) <= depth {
			levels = append(levels, nil)
		}
		levels[depth] = append(levels[depth], rel)
	}
	created := make(map[string]bool)
	for _, level := range levels {
		sort.Strings(level)
		if err := s.createFolders(level, created); err != nil {
			return err
		}
	}

	local := slices.Sorted(slices.Values(localDirs)) // parents first
	for _, rel := range local {
		s.opt.Names.Assign(rel)
		if err := os.MkdirAll(s.abs(rel), 0o755); err != nil {
			return err
		}
		need[rel] = true
	}
	for _, rel := range slices.Sorted(maps.Keys(need)) {
		if err := s.record(state.Item{Root: s.opt.Root, Rel: rel, DriveID: s.folders[rel], IsDir: true, SyncedAt: s.now.Unix()}); err != nil {
			return err
		}
	}
	return nil
}

// createFolders creates the Drive folders rels, whose parents all exist
// already, opt.Workers at a time. It records them in s.folders and in
// created, the folders made in this run.
func (s *run) createFolders(rels []string, created map[string]bool) error {
	ids := make([]string, len(rels))
	errs := make([]error, len(rels))
	sem := make(chan struct{}, s.opt.Workers)
	var wg sync.WaitGroup
	for i, rel := range rels {
		parent, name := splitRel(rel)
		create := s.r.EnsureFolder
		if created[parent] {
			create = s.r.CreateFolder
		}
		parentID := s.folders[parent]
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			ids[i], errs[i] = create(s.ctx, parentID, name)
		}()
	}
	wg.Wait()
	for i, rel := range rels {
		if errs[i] != nil {
			return errs[i]
		}
		s.folders[rel] = ids[i]
		created[rel] = true
		s.res.Folders++
		fmt.Fprintf(s.opt.Log, "folder    %s\n", rel)
	}
	return nil
}

// record saves an item in the state DB along with its stand-in name, if it
// has one.
func (s *run) record(it state.Item) error {
	it.LocalName = s.opt.Names.StandIn(it.Rel)
	return s.db.Put(s.ctx, it)
}

// recordSame records a path that already matches on both sides.
func (s *run) recordSame(a reconcile.Action) error {
	if a.Dir {
		return s.record(state.Item{Root: s.opt.Root, Rel: a.Rel, DriveID: a.DriveID, IsDir: true, SyncedAt: s.now.Unix()})
	}
	lf, rf := s.local[a.Rel], s.remote[a.Rel]
	s.res.InSync++
	return s.record(state.Item{
		Root: s.opt.Root, Rel: a.Rel, DriveID: rf.ID, Size: lf.Size, MD5: rf.MD5,
		LocalMTime: lf.ModTime.UnixNano(), SyncedAt: s.now.Unix(),
	})
}

func (s *run) do(a reconcile.Action) {
	var (
		n   int64
		err error
	)
	did := a.Op
	track := s.tracker(a.Rel, moved(a))
	switch a.Op {
	case reconcile.Upload:
		if a.DriveID != "" {
			n, did, err = s.update(a.Rel, a.DriveID, track)
		} else {
			n, err = s.upload(a.Rel, track)
		}
	case reconcile.Download:
		n, err = s.download(a.Rel, s.remote[a.Rel], track)
	case reconcile.Conflict:
		n, err = s.conflict(a.Rel, s.remote[a.Rel], track)
	case reconcile.TrashLocal:
		err = s.trashLocal(a.Rel)
	case reconcile.TrashRemote:
		err = s.trashRemote(a.Rel, a.DriveID)
	default:
		err = fmt.Errorf("unexpected action %v", a.Op)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.done++
	delete(s.inflight, a.Rel)
	s.doneBytes += moved(a)
	defer s.report()
	if err != nil {
		s.res.Failed = append(s.res.Failed, filter.Issue{Rel: a.Rel, Reason: err.Error()})
		if s.ctx.Err() != nil {
			// Interrupted, not broken: the next run picks it up.
			fmt.Fprintf(s.opt.Log, "[%d/%d] stopped  %s %s\n", s.done, s.total, verb(a), a.Rel)
			return
		}
		fmt.Fprintf(s.opt.Log, "[%d/%d] FAILED %s %s: %v\n", s.done, s.total, verb(a), a.Rel, err)
		return
	}
	switch did {
	case reconcile.Upload:
		s.res.Uploaded++
	case reconcile.Download:
		s.res.Downloaded++
	case reconcile.Conflict:
		s.res.Conflicts++
	case reconcile.TrashLocal:
		s.res.Trashed++
	case reconcile.TrashRemote:
		s.res.TrashedInDrive++
	}
	s.res.Bytes += n
	fmt.Fprintf(s.opt.Log, "[%d/%d] %-8s %s\n", s.done, s.total, verb(reconcile.Action{Op: did, DriveID: a.DriveID}), a.Rel)
}

// verb names an action in the log.
func verb(a reconcile.Action) string {
	switch a.Op {
	case reconcile.Upload:
		if a.DriveID != "" {
			return "update"
		}
		return "upload"
	case reconcile.Download:
		return "download"
	case reconcile.Conflict:
		return "conflict"
	case reconcile.TrashLocal:
		return "trash"
	case reconcile.TrashRemote:
		return "trash in Drive"
	}
	return a.Op.String()
}

// upload sends the local file at rel to Drive as a new file, checks Drive's
// checksum against the local one and records the result. track receives
// bytes sent.
func (s *run) upload(rel string, track func(int64)) (int64, error) {
	abs := s.abs(rel)
	before, err := os.Stat(abs)
	if err != nil {
		return 0, err
	}
	sum, err := localfs.MD5File(abs)
	if err != nil {
		return 0, err
	}
	parent, name := splitRel(rel)
	f, err := s.r.Upload(s.ctx, s.folders[parent], name, abs, before.ModTime(), sum, track)
	if err != nil {
		return 0, err
	}
	return s.uploaded(rel, abs, before, sum, f)
}

// update replaces the content of the Drive file id with the local file at
// rel, keeping its ID and version history. If Drive's copy changed since the
// last sync, nothing is overwritten: an edit there becomes a conflict that
// keeps both, and a delete there loses to the edit here, which is uploaded
// as a new file. It returns what it ended up doing.
func (s *run) update(rel, id string, track func(int64)) (int64, reconcile.Op, error) {
	base, _, err := s.db.Get(s.ctx, s.opt.Root, rel)
	if err != nil {
		return 0, reconcile.Upload, err
	}
	abs := s.abs(rel)
	before, err := os.Stat(abs)
	if err != nil {
		return 0, reconcile.Upload, err
	}
	sum, err := localfs.MD5File(abs)
	if err != nil {
		return 0, reconcile.Upload, err
	}
	f, err := s.r.Update(s.ctx, id, abs, before.ModTime(), sum, base.MD5, track)
	switch {
	case errors.Is(err, gdrive.ErrChanged) && f.Trashed:
		n, err := s.upload(rel, track)
		return n, reconcile.Upload, err
	case errors.Is(err, gdrive.ErrChanged):
		f.Rel = rel
		n, err := s.conflict(rel, f, track)
		return n, reconcile.Conflict, err
	case err != nil:
		return 0, reconcile.Upload, err
	}
	n, err := s.uploaded(rel, abs, before, sum, f)
	return n, reconcile.Upload, err
}

// uploaded checks a finished upload of the local file abs, as it was before
// the upload, and records it.
func (s *run) uploaded(rel, abs string, before fs.FileInfo, sum string, f gdrive.File) (int64, error) {
	if f.MD5 != "" && f.MD5 != sum {
		return 0, fmt.Errorf("Drive's checksum %s does not match the local %s", f.MD5, sum)
	}
	after, err := os.Stat(abs)
	if err != nil {
		return 0, err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return 0, errors.New("the file changed while uploading; run sync again")
	}
	return before.Size(), s.record(state.Item{
		Root: s.opt.Root, Rel: rel, DriveID: f.ID, Size: before.Size(), MD5: sum,
		LocalMTime: before.ModTime().UnixNano(), SyncedAt: time.Now().Unix(),
	})
}

// unchanged reports whether the local file fi is still as the scan saw it.
func (s *run) unchanged(rel string, fi fs.FileInfo) bool {
	lf, ok := s.local[rel]
	return ok && fi.Mode().IsRegular() && fi.Size() == lf.Size && fi.ModTime().Equal(lf.ModTime)
}

// trashLocal moves the local file at rel to the OS trash, unless it was
// edited after the scan: then it stays, and the next run uploads it.
func (s *run) trashLocal(rel string) error {
	abs := s.abs(rel)
	fi, err := os.Lstat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s.db.Delete(s.ctx, s.opt.Root, rel) // already gone
	case err != nil:
		return err
	case !s.unchanged(rel, fi):
		return errors.New("edited here after the scan; kept, and the next sync uploads it")
	}
	if err := s.opt.Trash(abs); err != nil {
		return err
	}
	return s.db.Delete(s.ctx, s.opt.Root, rel)
}

// trashRemote moves the Drive file id to Drive's trash.
func (s *run) trashRemote(rel, id string) error {
	if err := s.r.Trash(s.ctx, id); err != nil {
		return err
	}
	return s.db.Delete(s.ctx, s.opt.Root, rel)
}

// removeFolders removes the folders in dirs that are empty now that their
// files are gone, deepest first: here with a plain remove, which only works
// on an empty folder, and in Drive after checking it holds nothing at all.
// A folder that still holds something, such as ignored files or Google Docs,
// stays where it is, and so does its record.
func (s *run) removeFolders(dirs []reconcile.Action) {
	slices.SortFunc(dirs, func(a, b reconcile.Action) int {
		return cmp.Or(cmp.Compare(strings.Count(b.Rel, "/"), strings.Count(a.Rel, "/")), cmp.Compare(a.Rel, b.Rel))
	})
	fail := func(rel string, err error) {
		s.res.Failed = append(s.res.Failed, filter.Issue{Rel: rel + "/", Reason: err.Error()})
		fmt.Fprintf(s.opt.Log, "FAILED remove folder %s: %v\n", rel, err)
	}
	keep := func(rel, where string) {
		fmt.Fprintf(s.opt.Log, "kept      %s/ (deleted on the other side, but still holds files CloudSync doesn't sync %s)\n", rel, where)
	}
	for _, a := range dirs {
		switch a.Op {
		case reconcile.TrashLocal:
			abs := s.abs(a.Rel)
			if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
				keep(a.Rel, "here")
				continue
			}
			if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
				fail(a.Rel, err)
				continue
			}
		case reconcile.TrashRemote:
			has, err := s.r.HasChildren(s.ctx, a.DriveID)
			if err != nil {
				fail(a.Rel, err)
				continue
			}
			if has {
				keep(a.Rel, "in Drive")
				continue
			}
			if err := s.r.Trash(s.ctx, a.DriveID); err != nil {
				fail(a.Rel, err)
				continue
			}
		}
		if err := s.db.Delete(s.ctx, s.opt.Root, a.Rel); err != nil {
			fail(a.Rel, err)
			continue
		}
		s.res.FoldersRemoved++
		fmt.Fprintf(s.opt.Log, "removed   %s/\n", a.Rel)
	}
}

// download fetches the Drive file rf into a temporary file next to rel,
// verifies its checksum, sets its modified time and only then moves it into
// place. It replaces a local file only if that file is still exactly as the
// scan saw it, so nothing written since is lost. track receives bytes
// received.
func (s *run) download(rel string, rf gdrive.File, track func(int64)) (int64, error) {
	if rf.ID == "" {
		return 0, fmt.Errorf("%s is not in the Drive listing", rel)
	}
	abs := s.abs(rel)
	if fi, err := os.Lstat(abs); err == nil {
		if _, scanned := s.local[rel]; !scanned {
			return 0, errors.New("a local file now exists at this path; both copies were left untouched")
		}
		if !s.unchanged(rel, fi) {
			return 0, errors.New("edited here after the scan; both copies were left untouched for the next sync")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, err
	}
	tmp := filepath.Join(filepath.Dir(abs), "."+filepath.Base(abs)+".cloudsync-partial")
	if err := gdrive.Retry(s.ctx, func() error { return s.fetch(rf, tmp, track) }); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if !rf.ModTime.IsZero() {
		if err := os.Chtimes(tmp, time.Now(), rf.ModTime); err != nil {
			os.Remove(tmp)
			return 0, err
		}
	}
	if err := os.Rename(tmp, abs); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return 0, err
	}
	return rf.Size, s.record(state.Item{
		Root: s.opt.Root, Rel: rel, DriveID: rf.ID, Size: fi.Size(), MD5: rf.MD5,
		LocalMTime: fi.ModTime().UnixNano(), SyncedAt: time.Now().Unix(),
	})
}

func (s *run) fetch(rf gdrive.File, tmp string, track func(int64)) error {
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := md5.New()
	err = s.r.Download(s.ctx, rf.ID, io.MultiWriter(f, h, &counter{fn: track}))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); rf.MD5 != "" && got != rf.MD5 {
		return fmt.Errorf("downloaded checksum %s does not match Drive's %s", got, rf.MD5)
	}
	return nil
}

// counter is an io.Writer that reports the running total written to it.
type counter struct {
	n  int64
	fn func(int64)
}

func (c *counter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	c.fn(c.n)
	return len(p), nil
}

// conflict keeps both versions: the local file is renamed to a conflict copy
// and uploaded, and Drive's version rf is downloaded under the original name.
func (s *run) conflict(rel string, rf gdrive.File, track func(int64)) (int64, error) {
	copyRel := s.copyName(rel)
	if err := os.Rename(s.abs(rel), s.abs(copyRel)); err != nil {
		return 0, err
	}
	down, err := s.download(rel, rf, track)
	if err != nil {
		return 0, fmt.Errorf("your version was kept as %s, but downloading Drive's version failed: %w", copyRel, err)
	}
	up, err := s.upload(copyRel, track)
	if err != nil {
		return down, fmt.Errorf("Drive's version was downloaded; uploading your copy %s failed: %w", copyRel, err)
	}
	return down + up, nil
}

// copyName returns the conflict copy's name for rel: the one chosen before
// transfers started, or, for a conflict found during one, a new one.
func (s *run) copyName(rel string) string {
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	if c, ok := s.copies[rel]; ok {
		return c
	}
	c := s.conflictName(rel)
	s.opt.Names.Assign(c)
	s.copies[rel] = c
	return c
}

// conflictName returns a free name like "notes (conflict Windows 2026-10-27 1432).pdf".
func (s *run) conflictName(rel string) string {
	dir, file := path.Split(rel)
	ext := path.Ext(file)
	base := fmt.Sprintf("%s (conflict %s %s)", strings.TrimSuffix(file, ext), s.opt.Label, s.now.Format("2006-01-02 1504"))
	name := base + ext
	for i := 2; ; i++ {
		_, localErr := os.Lstat(s.guess(dir + name))
		_, inDrive := s.remote[dir+name]
		if errors.Is(localErr, fs.ErrNotExist) && !inDrive {
			return dir + name
		}
		name = fmt.Sprintf("%s %d%s", base, i, ext)
	}
}

// abs returns the local path of the true path rel. A new name must have been
// assigned in opt.Names first.
func (s *run) abs(rel string) string {
	return filepath.Join(s.opt.Local, filepath.FromSlash(s.opt.Names.Local(rel)))
}

// guess returns where rel would be stored locally if its name were
// assigned now, for checking whether a name is free without assigning it.
func (s *run) guess(rel string) string {
	if s.opt.Names == nil {
		return s.abs(rel)
	}
	dir, name := splitRel(rel)
	return filepath.Join(s.opt.Local, filepath.FromSlash(s.opt.Names.Local(dir)), winname.Encode(name))
}

func splitRel(rel string) (parent, name string) {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}
