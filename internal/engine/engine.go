// Package engine carries out syncs planned by package plan.
package engine

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/USER1043/cloudsync/internal/state"
	"github.com/USER1043/cloudsync/internal/winname"
)

// Remote is the part of Google Drive the engine uses. *gdrive.Client
// implements it.
type Remote interface {
	EnsureFolder(ctx context.Context, parentID, name string) (string, error)
	Upload(ctx context.Context, parentID, name, localPath string, modTime time.Time, md5sum string, progress func(sent int64)) (gdrive.File, error)
	Download(ctx context.Context, id string, w io.Writer) error
}

// Options configures one root's sync.
type Options struct {
	Root    string    // root name; the key for this root in the state DB
	Local   string    // local folder
	RootID  string    // Drive folder ID of the root
	Label   string    // OS name used in conflict copy names, e.g. "Windows"
	Workers int       // parallel transfers; 4 if zero
	Log     io.Writer // one line per folder created and per transfer

	// Names holds the stand-in names this computer uses (Windows only; nil
	// on Linux). It must be the Map the scan was made with. Plans and the
	// state DB use true paths; Names turns them into local ones.
	Names *winname.Map

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
	Bytes                                            int64
	Failed                                           []filter.Issue
}

// FirstSync carries out a plan from plan.FirstSync. It never deletes or
// overwrites a file: conflicts keep both copies, and a download is skipped if
// a local file has appeared at its path. It returns an error only when it
// cannot go on at all; individual file failures are listed in the Result.
func FirstSync(ctx context.Context, r Remote, db *state.DB, opt Options, scan *localfs.Result, tree *gdrive.Tree, p *plan.Plan) (*Result, error) {
	if opt.Workers <= 0 {
		opt.Workers = 4
	}
	if opt.Log == nil {
		opt.Log = io.Discard
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

	if err := s.makeFolders(scan, tree, p); err != nil {
		return &s.res, err
	}

	// Choose every new local name before the transfers run in parallel, so
	// the choice doesn't depend on which transfer happens to go first.
	var jobs []plan.Item
	for _, it := range p.Items {
		switch it.Kind {
		case plan.InSync:
			if err := s.recordInSync(it); err != nil {
				return &s.res, err
			}
			continue
		case plan.Download:
			opt.Names.Assign(it.Rel)
		case plan.Conflict:
			s.copies[it.Rel] = s.conflictName(it.Rel)
			opt.Names.Assign(s.copies[it.Rel])
		}
		jobs = append(jobs, it)
		s.totalBytes += it.Size
	}
	s.total = len(jobs)
	s.report()

	ch := make(chan plan.Item)
	var wg sync.WaitGroup
	for range opt.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				s.do(it)
			}
		}()
	}
feed:
	for _, it := range jobs {
		select {
		case ch <- it:
		case <-ctx.Done():
			break feed
		}
	}
	close(ch)
	wg.Wait()
	return &s.res, ctx.Err()
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
	copies  map[string]string // conflict rel -> rel of the local version's copy. Read-only once transfers start.

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
// transfer. It never goes backwards (a retry restarts from zero) or past the
// planned size, and it reports at most every reportEvery.
func (s *run) tracker(it plan.Item) func(int64) {
	return func(n int64) {
		s.mu.Lock()
		defer s.mu.Unlock()
		n = min(n, it.Size)
		if n <= s.inflight[it.Rel] {
			return
		}
		s.inflight[it.Rel] = n
		if time.Since(s.lastReport) >= reportEvery {
			s.report()
		}
	}
}

// makeFolders creates, before any transfer starts, every Drive folder that
// uploads need and every local folder that exists in Drive.
func (s *run) makeFolders(scan *localfs.Result, tree *gdrive.Tree, p *plan.Plan) error {
	need := make(map[string]bool)
	for _, d := range scan.Dirs {
		need[d] = true
	}
	for _, it := range p.Items {
		if it.Kind == plan.Upload || it.Kind == plan.Conflict {
			for dir := path.Dir(it.Rel); dir != "."; dir = path.Dir(dir) {
				need[dir] = true
			}
		}
	}
	rels := make([]string, 0, len(need))
	for rel := range need {
		rels = append(rels, rel)
	}
	sort.Strings(rels) // a parent is a prefix of its children, so it sorts first
	for _, rel := range rels {
		if _, ok := s.folders[rel]; ok {
			continue
		}
		parent, name := splitRel(rel)
		id, err := s.r.EnsureFolder(s.ctx, s.folders[parent], name)
		if err != nil {
			return err
		}
		s.folders[rel] = id
		s.res.Folders++
		fmt.Fprintf(s.opt.Log, "folder    %s\n", rel)
	}

	local := slices.Clone(tree.Folders)
	slices.SortFunc(local, func(a, b gdrive.File) int { return strings.Compare(a.Rel, b.Rel) }) // parents first
	for _, f := range local {
		s.opt.Names.Assign(f.Rel)
		if err := os.MkdirAll(s.abs(f.Rel), 0o755); err != nil {
			return err
		}
	}
	for rel, id := range s.folders {
		if rel == "" {
			continue
		}
		if err := s.record(state.Item{Root: s.opt.Root, Rel: rel, DriveID: id, IsDir: true, SyncedAt: s.now.Unix()}); err != nil {
			return err
		}
	}
	return nil
}

// record saves an item in the state DB along with its stand-in name, if it
// has one.
func (s *run) record(it state.Item) error {
	it.LocalName = s.opt.Names.StandIn(it.Rel)
	return s.db.Put(s.ctx, it)
}

func (s *run) recordInSync(it plan.Item) error {
	lf, rf := s.local[it.Rel], s.remote[it.Rel]
	s.res.InSync++
	return s.record(state.Item{
		Root: s.opt.Root, Rel: it.Rel, DriveID: rf.ID, Size: lf.Size, MD5: rf.MD5,
		LocalMTime: lf.ModTime.UnixNano(), SyncedAt: s.now.Unix(),
	})
}

func (s *run) do(it plan.Item) {
	var (
		n    int64
		err  error
		verb string
	)
	track := s.tracker(it)
	switch it.Kind {
	case plan.Upload:
		verb = "upload"
		n, err = s.upload(it.Rel, track)
	case plan.Download:
		verb = "download"
		n, err = s.download(it.Rel, track)
	case plan.Conflict:
		verb = "conflict"
		n, err = s.conflict(it, track)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.done++
	delete(s.inflight, it.Rel)
	s.doneBytes += it.Size
	defer s.report()
	if err != nil {
		s.res.Failed = append(s.res.Failed, filter.Issue{Rel: it.Rel, Reason: err.Error()})
		fmt.Fprintf(s.opt.Log, "[%d/%d] FAILED %s %s: %v\n", s.done, s.total, verb, it.Rel, err)
		return
	}
	switch it.Kind {
	case plan.Upload:
		s.res.Uploaded++
	case plan.Download:
		s.res.Downloaded++
	case plan.Conflict:
		s.res.Conflicts++
	}
	s.res.Bytes += n
	fmt.Fprintf(s.opt.Log, "[%d/%d] %-8s %s\n", s.done, s.total, verb, it.Rel)
}

// upload sends the local file at rel to Drive, checks Drive's checksum
// against the local one and records the result. track receives bytes sent.
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

// download fetches the Drive file at rel into a temporary file, verifies its
// checksum, sets its modified time and only then moves it into place. track
// receives bytes received.
func (s *run) download(rel string, track func(int64)) (int64, error) {
	rf, ok := s.remote[rel]
	if !ok {
		return 0, fmt.Errorf("%s is not in the Drive listing", rel)
	}
	abs := s.abs(rel)
	if _, err := os.Lstat(abs); err == nil {
		return 0, errors.New("a local file now exists at this path; both copies were left untouched")
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
// and uploaded, and Drive's version is downloaded under the original name.
func (s *run) conflict(it plan.Item, track func(int64)) (int64, error) {
	copyRel := s.copies[it.Rel]
	if err := os.Rename(s.abs(it.Rel), s.abs(copyRel)); err != nil {
		return 0, err
	}
	down, err := s.download(it.Rel, track)
	if err != nil {
		return 0, fmt.Errorf("your version was kept as %s, but downloading Drive's version failed: %w", copyRel, err)
	}
	up, err := s.upload(copyRel, track)
	if err != nil {
		return down, fmt.Errorf("Drive's version was downloaded; uploading your copy %s failed: %w", copyRel, err)
	}
	return down + up, nil
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
