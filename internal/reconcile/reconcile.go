// Package reconcile works out what a sync after the first one must do. It
// compares three views of every path: the local folder, Google Drive, and the
// base, which is the state DB's record of how the path was when last synced.
// The base tells an edit on one side from an edit on the other, and a file
// deleted on one side from a file that is new on the other.
//
// Reconcile is pure: it reads only its inputs and changes nothing.
package reconcile

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/localfs"
	"github.com/USER1043/cloudsync/internal/state"
)

// Op is what the sync does with one path.
type Op int

const (
	Upload      Op = iota // new or edited here; replaces DriveID, or a new Drive file if DriveID is ""
	Download              // new or edited in Drive
	TrashLocal            // deleted in Drive: move the local copy to the OS trash
	TrashRemote           // deleted here: move the Drive copy (DriveID) to Drive's trash
	Conflict              // edited differently on both sides: keep both copies
	MkdirLocal            // folder to create here
	MkdirRemote           // folder to create in Drive
	Record                // already the same on both sides: update the state DB only
	Forget                // gone from both sides: drop it from the state DB
	Wait                  // can't be decided now (see Note): left alone until the next run
)

func (o Op) String() string {
	return [...]string{"upload", "download", "trash here", "trash in Drive", "conflict",
		"create folder here", "create folder in Drive", "record", "forget", "wait"}[o]
}

// Action is one thing to do.
//
// For a folder, TrashLocal and TrashRemote mean "remove the folder once it is
// empty": the files to trash are listed as actions of their own, and anything
// else inside (ignored files, Google Docs) keeps the folder where it is.
type Action struct {
	Rel     string // true path, the same in Drive, the state DB and the scan
	Op      Op
	Dir     bool
	Size    int64  // bytes the action moves or trashes
	DriveID string // the Drive file it acts on: for downloads, trashes in Drive and uploads that replace a file
	Note    string // why, for anything other than the obvious
}

// Plan is the list of actions, sorted by path. Paths with nothing to do have
// no action.
type Plan struct {
	Actions   []Action
	BaseFiles int // files in the base, the root's size for the big-delete guard
}

// Hasher returns the hex MD5 of the local file at a true path.
type Hasher func(rel string) (string, error)

// Input is the three views of one root.
type Input struct {
	Local      []localfs.File // scan result (localfs.Result.Files)
	LocalDirs  []string       // localfs.Result.Dirs
	Remote     []gdrive.File  // Drive files after the ignore rules (gdrive.Tree.Filter)
	RemoteDirs []gdrive.File  // Drive folders after the ignore rules
	Base       []state.Item   // the state DB's items for this root

	// Skip reports paths to leave alone entirely, such as files over the
	// size limit, files the scan couldn't read, git repositories and names
	// that can't be synced. They are missing from one side's list for a
	// reason other than deletion, so without Skip they would be trashed on
	// the other side. A path is also skipped when any folder above it is.
	// Ignored paths need no Skip: they are on neither list and their base
	// records are left alone the same way.
	Skip func(rel string) bool

	Hash Hasher
}

// Reconcile compares local, Drive and base and returns what to do. The rules,
// for a file:
//
//   - changed on one side only: copy it to the other side;
//   - changed on both sides: the same content is recorded, different content
//     is a conflict and both copies are kept;
//   - deleted on one side and unchanged on the other: trash it there too;
//   - deleted on one side and edited on the other: the edit wins and is
//     copied back;
//   - new on one side: copy it; new on both: as "changed on both sides";
//   - deleted on both: forget it.
//
// A local file is unchanged if its size and modification time match the
// base; if only the time differs it is hashed, so a file that was merely
// touched is not uploaded. A Drive file is unchanged if its MD5 matches the
// base. A path that is a file on one side and a folder on the other is left
// alone, with everything under it, and reported as Wait.
func Reconcile(in Input) *Plan {
	r := &rec{in: in, hashes: map[string]hashResult{}}
	r.index()
	for _, rel := range r.union(keys(r.lf), keys(r.rf), keys(r.bf)) {
		if a, ok := r.file(rel); ok {
			r.actions = append(r.actions, a)
		}
	}
	// Folders deepest first, so a folder's decision can depend on what
	// happens inside it.
	dirs := r.union(keys(r.ld), keys(r.rd), keys(r.bd))
	slices.SortFunc(dirs, func(a, b string) int {
		return cmp.Or(cmp.Compare(strings.Count(b, "/"), strings.Count(a, "/")), cmp.Compare(a, b))
	})
	for _, rel := range dirs {
		if a, ok := r.dir(rel); ok {
			r.actions = append(r.actions, a)
		}
	}
	for _, rel := range r.clashes {
		r.actions = append(r.actions, Action{Rel: rel, Op: Wait, Note: "a file on one side and a folder on the other; rename one of them"})
	}
	slices.SortFunc(r.actions, func(a, b Action) int { return cmp.Compare(a.Rel, b.Rel) })
	return &Plan{Actions: r.actions, BaseFiles: len(r.bf)}
}

type hashResult struct {
	sum string
	err error
}

type rec struct {
	in      Input
	lf      map[string]localfs.File
	rf      map[string]gdrive.File
	bf      map[string]state.Item
	ld      map[string]bool
	rd      map[string]gdrive.File
	bd      map[string]state.Item
	clash   map[string]bool
	clashes []string
	hashes  map[string]hashResult
	actions []Action
}

func (r *rec) index() {
	r.lf = make(map[string]localfs.File, len(r.in.Local))
	for _, f := range r.in.Local {
		r.lf[f.Rel] = f
	}
	r.rf = make(map[string]gdrive.File, len(r.in.Remote))
	for _, f := range r.in.Remote {
		r.rf[f.Rel] = f
	}
	r.ld = make(map[string]bool, len(r.in.LocalDirs))
	for _, d := range r.in.LocalDirs {
		r.ld[d] = true
	}
	r.rd = make(map[string]gdrive.File, len(r.in.RemoteDirs))
	for _, d := range r.in.RemoteDirs {
		r.rd[d.Rel] = d
	}
	r.bf, r.bd = map[string]state.Item{}, map[string]state.Item{}
	for _, it := range r.in.Base {
		if it.IsDir {
			r.bd[it.Rel] = it
		} else {
			r.bf[it.Rel] = it
		}
	}
	r.clash = map[string]bool{}
	for rel := range r.lf {
		if _, ok := r.rd[rel]; ok {
			r.clash[rel] = true
		}
	}
	for rel := range r.rf {
		if r.ld[rel] {
			r.clash[rel] = true
		}
	}
	for rel := range r.clash {
		r.clashes = append(r.clashes, rel)
	}
}

// union returns the paths in any of the lists, sorted, once each, leaving
// out skipped paths.
func (r *rec) union(lists ...[]string) []string {
	out := slices.Concat(lists...)
	slices.Sort(out)
	return slices.DeleteFunc(slices.Compact(out), r.skipped)
}

func keys[V any](m map[string]V) []string { return slices.Collect(maps.Keys(m)) }

// skipped reports whether rel or a folder above it is skipped or a clash.
func (r *rec) skipped(rel string) bool {
	for p := rel; ; {
		if r.clash[p] || (r.in.Skip != nil && r.in.Skip(p)) {
			return true
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return false
		}
		p = p[:i]
	}
}

func (r *rec) hash(rel string) (string, error) {
	h, ok := r.hashes[rel]
	if !ok {
		h.sum, h.err = r.in.Hash(rel)
		r.hashes[rel] = h
	}
	return h.sum, h.err
}

// localChanged compares the local file with the base. touched means the
// modification time moved but the content is the same.
func (r *rec) localChanged(l localfs.File, b state.Item) (changed, touched bool, err error) {
	if l.Size != b.Size {
		return true, false, nil
	}
	if l.ModTime.UnixNano() == b.LocalMTime {
		return false, false, nil
	}
	sum, err := r.hash(l.Rel)
	if err != nil {
		return false, false, err
	}
	if sum != b.MD5 {
		return true, false, nil
	}
	return false, true, nil
}

// remoteChanged compares the Drive file with the base.
func remoteChanged(f gdrive.File, b state.Item) bool {
	if f.MD5 != "" && b.MD5 != "" {
		return f.MD5 != b.MD5
	}
	return f.ID != b.DriveID || f.Size != b.Size
}

// same reports whether the local and Drive files have the same content.
func (r *rec) same(l localfs.File, f gdrive.File) (bool, error) {
	if l.Size != f.Size || f.MD5 == "" {
		return false, nil
	}
	sum, err := r.hash(l.Rel)
	return sum == f.MD5, err
}

func wait(rel string, err error) Action {
	return Action{Rel: rel, Op: Wait, Note: "could not read local file: " + err.Error()}
}

func (r *rec) file(rel string) (Action, bool) {
	l, hasL := r.lf[rel]
	f, hasR := r.rf[rel]
	b, hasB := r.bf[rel]
	up := func(id, note string) (Action, bool) {
		return Action{Rel: rel, Op: Upload, Size: l.Size, DriveID: id, Note: note}, true
	}
	down := func(note string) (Action, bool) {
		return Action{Rel: rel, Op: Download, Size: f.Size, DriveID: f.ID, Note: note}, true
	}
	record := func(note string) (Action, bool) {
		return Action{Rel: rel, Op: Record, DriveID: f.ID, Note: note}, true
	}
	both := func(note string) (Action, bool) {
		same, err := r.same(l, f)
		switch {
		case err != nil:
			return wait(rel, err), true
		case same:
			return record("same content on both sides")
		}
		return Action{Rel: rel, Op: Conflict, Size: l.Size, DriveID: f.ID, Note: note}, true
	}

	switch {
	case hasL && hasR && hasB:
		lc, touched, err := r.localChanged(l, b)
		if err != nil {
			return wait(rel, err), true
		}
		rc := remoteChanged(f, b)
		switch {
		case lc && rc:
			return both("edited on both sides")
		case lc:
			return up(f.ID, "")
		case rc:
			return down("")
		case touched || f.ID != b.DriveID:
			return record("")
		}
		return Action{}, false
	case hasL && hasR:
		return both("new on both sides with different content")
	case hasL && hasB:
		lc, _, err := r.localChanged(l, b)
		switch {
		case err != nil:
			return wait(rel, err), true
		case lc:
			return up("", "deleted in Drive but edited here; the edit wins")
		}
		return Action{Rel: rel, Op: TrashLocal, Size: l.Size}, true
	case hasR && hasB:
		if remoteChanged(f, b) {
			return down("deleted here but edited in Drive; the edit wins")
		}
		return Action{Rel: rel, Op: TrashRemote, Size: f.Size, DriveID: f.ID}, true
	case hasL:
		return up("", "")
	case hasR:
		return down("")
	}
	return Action{Rel: rel, Op: Forget}, true
}

// keeps lists, per side, the ops that need a folder to stay on that side.
var keeps = map[Op][]Op{
	TrashLocal:  {Upload, Conflict, MkdirRemote, Wait},  // folder deleted in Drive
	TrashRemote: {Download, Conflict, MkdirLocal, Wait}, // folder deleted here
}

// keptUnder reports whether anything inside folder rel needs it to stay
// instead of being trashed with trash.
func (r *rec) keptUnder(rel string, trash Op) bool {
	prefix := rel + "/"
	for _, a := range r.actions {
		if strings.HasPrefix(a.Rel, prefix) && slices.Contains(keeps[trash], a.Op) {
			return true
		}
	}
	return false
}

func (r *rec) dir(rel string) (Action, bool) {
	hasL := r.ld[rel]
	f, hasR := r.rd[rel]
	_, hasB := r.bd[rel]
	act := func(op Op, note string) (Action, bool) {
		return Action{Rel: rel, Op: op, Dir: true, DriveID: f.ID, Note: note}, true
	}
	switch {
	case hasL && hasR:
		if !hasB {
			return act(Record, "")
		}
		return Action{}, false
	case hasL && hasB:
		if r.keptUnder(rel, TrashLocal) {
			return act(MkdirRemote, "deleted in Drive but has new or edited files here")
		}
		return act(TrashLocal, "")
	case hasR && hasB:
		if r.keptUnder(rel, TrashRemote) {
			return act(MkdirLocal, "deleted here but has new or edited files in Drive")
		}
		return act(TrashRemote, "")
	case hasL:
		return act(MkdirRemote, "")
	case hasR:
		return act(MkdirLocal, "")
	}
	return act(Forget, "")
}
