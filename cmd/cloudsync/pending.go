package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/reconcile"
	"github.com/USER1043/cloudsync/internal/state"
)

// pendingList is the note a computer leaves in Drive when it shuts down
// before everything is uploaded: the files Drive doesn't have yet, so the
// other OS can warn that its copies of them are out of date. Unsent changes
// themselves stay on disk and upload at that computer's next start.
type pendingList struct {
	OS       string        `json:"os"`
	Computer string        `json:"computer"`       // the writer's computerID
	Host     string        `json:"host,omitempty"` // the writer's host name, for people
	Root     string        `json:"root"`
	At       time.Time     `json:"at"`
	Files    []pendingFile `json:"files"`
}

// pendingFile is one change Drive is missing: a new or edited file, with
// its size and modification time (uploads keep the local time, so Drive's
// copy matching both means it arrived after all), or a deleted one.
type pendingFile struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size,omitempty"`
	Modified time.Time `json:"modified,omitzero"`
	Deleted  bool      `json:"deleted,omitempty"`
}

// metaSegments is the Drive folder holding CloudSync's own files about a
// root: ".cloudsync" next to the root folder, so it is never synced.
func metaSegments(root config.Root) []string {
	segs := root.RemoteSegments()
	return append(slices.Clone(segs[:len(segs)-1]), ".cloudsync")
}

const metaComputerID = "computer_id"

// computerID returns this computer's ID, made the first time it is asked
// for. Lists are named by it rather than by OS alone, so two computers with
// the same OS never take each other's list for their own. Each computer has
// its own state DB, and so its own ID.
func computerID(ctx context.Context, db *state.DB) (string, error) {
	if id, ok, err := db.Meta(ctx, metaComputerID); err != nil || ok {
		return id, err
	}
	b := make([]byte, 4)
	rand.Read(b)
	id := hex.EncodeToString(b)
	return id, db.SetMeta(ctx, metaComputerID, id)
}

// pendingName names a computer's list for root, e.g.
// "pending-linux-3f9a1c2b-MyFiles.json".
func pendingName(sys, computer, root string) string {
	return "pending-" + strings.ToLower(sys) + "-" + computer + "-" + root + ".json"
}

// isPendingName reports whether a file in the .cloudsync folder is a list.
// Which root it is about is read from inside, not guessed from the name.
func isPendingName(name string) bool {
	return strings.HasPrefix(name, "pending-") && strings.HasSuffix(name, ".json")
}

// pendingFrom lists the changes a live plan would still send to Drive, as
// computer's list. Deletes held for the owner aren't pending uploads and
// are left out.
func pendingFrom(l *later, computer string, now time.Time) pendingList {
	host, _ := os.Hostname()
	p := pendingList{OS: osLabel(), Computer: computer, Host: host, Root: l.in.root.Name, At: now}
	mtimes := make(map[string]time.Time, len(l.in.scan.Files))
	for _, f := range l.in.scan.Files {
		mtimes[f.Rel] = f.ModTime
	}
	for _, a := range l.run.Actions {
		switch {
		case a.Dir:
		case a.Op == reconcile.Upload:
			p.Files = append(p.Files, pendingFile{Path: a.Rel, Size: a.Size, Modified: mtimes[a.Rel]})
		case a.Op == reconcile.TrashRemote:
			p.Files = append(p.Files, pendingFile{Path: a.Rel, Deleted: true})
		}
	}
	return p
}

func (p pendingList) encode() ([]byte, error) { return json.MarshalIndent(p, "", "  ") }

func decodePending(data []byte) (pendingList, error) {
	var p pendingList
	err := json.Unmarshal(data, &p)
	return p, err
}

// stale returns the entries Drive still doesn't match, judged by tree, the
// Drive listing of the root: a file whose Drive copy is missing or differs
// in size or time, or a deleted file Drive still has.
func (p pendingList) stale(tree *gdrive.Tree) []pendingFile {
	files := make(map[string]gdrive.File, len(tree.Files))
	for _, f := range tree.Files {
		files[f.Rel] = f
	}
	var out []pendingFile
	for _, e := range p.Files {
		f, ok := files[e.Path]
		switch {
		case e.Deleted && !ok:
		case e.Deleted:
			out = append(out, e)
		// Drive keeps times to the millisecond.
		case ok && f.Size == e.Size && f.ModTime.UnixMilli() == e.Modified.UnixMilli():
		default:
			out = append(out, e)
		}
	}
	return out
}
