package reconcile

// Guard decides whether a run's deletes may go ahead without asking the
// owner. It is an interface so the rule can change, for instance to one
// worked out from the state DB's delete log, without touching Reconcile.
type Guard interface {
	// Hold reports whether deleting deletes files of a root that had files
	// files at the last sync needs the owner's go-ahead.
	Hold(deletes, files int) bool
}

// Threshold holds deletes of more than Files files, or of more than Fraction
// of the root's files.
type Threshold struct {
	Files    int
	Fraction float64
}

// DefaultGuard is the starting rule agreed with the owner: more than 50
// files or more than 10% of the root. The delete log exists to tune it.
var DefaultGuard Guard = Threshold{Files: 50, Fraction: 0.10}

// Hold implements Guard.
func (t Threshold) Hold(deletes, files int) bool {
	return deletes > t.Files || float64(deletes) > t.Fraction*float64(files)
}

// isDelete reports whether a trashes something.
func isDelete(a Action) bool { return a.Op == TrashLocal || a.Op == TrashRemote }

// Deletes counts the files the plan trashes here and in Drive. Folders are
// not counted: a folder is only removed once the files in it are gone.
func (p *Plan) Deletes() (local, remote int) {
	for _, a := range p.Actions {
		switch {
		case a.Dir:
		case a.Op == TrashLocal:
			local++
		case a.Op == TrashRemote:
			remote++
		}
	}
	return local, remote
}

// Held reports whether g holds the plan's deletes.
func (p *Plan) Held(g Guard) bool {
	l, r := p.Deletes()
	return l+r > 0 && g.Hold(l+r, p.BaseFiles)
}

// SplitDeletes returns the plan without its deletes, and the deletes. When
// the guard holds a plan, the rest runs at once and the deletes wait for the
// owner: approved, they run as they are; cancelled, each is turned around
// with Undo.
func (p *Plan) SplitDeletes() (rest *Plan, deletes []Action) {
	rest = &Plan{BaseFiles: p.BaseFiles}
	for _, a := range p.Actions {
		if isDelete(a) {
			deletes = append(deletes, a)
		} else {
			rest.Actions = append(rest.Actions, a)
		}
	}
	return rest, deletes
}

// Undo turns a delete the owner cancelled into the action that puts the
// file back on the side it was deleted from: a file deleted in Drive is
// uploaded again, a file deleted here is downloaded again. Other actions are
// returned as they are.
func Undo(a Action) Action {
	note := "delete cancelled; restoring"
	switch {
	case a.Op == TrashLocal && a.Dir:
		return Action{Rel: a.Rel, Op: MkdirRemote, Dir: true, Note: note}
	case a.Op == TrashRemote && a.Dir:
		return Action{Rel: a.Rel, Op: MkdirLocal, Dir: true, DriveID: a.DriveID, Note: note}
	case a.Op == TrashLocal:
		return Action{Rel: a.Rel, Op: Upload, Size: a.Size, Note: note}
	case a.Op == TrashRemote:
		return Action{Rel: a.Rel, Op: Download, Size: a.Size, DriveID: a.DriveID, Note: note}
	}
	return a
}
