// Package plan compares a local scan with Drive and works out what a sync
// would do. It never changes anything itself.
package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/localfs"
)

// Kind is what the sync does with one path.
type Kind int

const (
	Upload   Kind = iota // only on this computer
	Download             // only in Drive
	InSync               // same content on both sides
	Conflict             // different content on both sides; both copies are kept
)

func (k Kind) String() string {
	return [...]string{"upload", "download", "in sync", "conflict"}[k]
}

// Item is one path in the plan. Size is the local size for uploads and
// conflicts, the Drive size for downloads, and 0 for files already in sync.
type Item struct {
	Rel      string // local path, or the Drive path for downloads
	DriveRel string // matching Drive path; differs from Rel only by letter case, and is empty for uploads
	Kind     Kind
	Size     int64
	Note     string
}

// Plan is the ordered list of items.
type Plan struct{ Items []Item }

// Total counts the items and bytes of one Kind.
type Total struct {
	Count int
	Bytes int64
}

// Totals sums the plan by Kind.
func (p *Plan) Totals() map[Kind]Total {
	t := make(map[Kind]Total)
	for _, it := range p.Items {
		tot := t[it.Kind]
		tot.Count++
		tot.Bytes += it.Size
		t[it.Kind] = tot
	}
	return t
}

// Of returns the items of one Kind.
func (p *Plan) Of(k Kind) []Item {
	var out []Item
	for _, it := range p.Items {
		if it.Kind == k {
			out = append(out, it)
		}
	}
	return out
}

// Hasher returns the hex MD5 of the local file at a relative path.
type Hasher func(rel string) (string, error)

// FirstSync builds the union-merge plan for a root's first sync. Nothing is
// deleted: a file on only one side is copied to the other, identical files
// are left alone, and files that differ become conflicts where both copies
// are kept. Local files are hashed only when Drive has a file of the same
// path and size.
func FirstSync(local []localfs.File, remote []gdrive.File, hash Hasher) *Plan {
	byRel := make(map[string]gdrive.File, len(remote))
	byLower := make(map[string]string, len(remote))
	for _, f := range remote {
		byRel[f.Rel] = f
		byLower[strings.ToLower(f.Rel)] = f.Rel
	}

	p := &Plan{}
	add := func(rel, driveRel string, k Kind, size int64, note string) {
		p.Items = append(p.Items, Item{Rel: rel, DriveRel: driveRel, Kind: k, Size: size, Note: note})
	}
	matched := make(map[string]bool)
	for _, lf := range local {
		rf, ok := byRel[lf.Rel]
		if !ok {
			// Windows paths ignore case, so a case-only difference is the same file there.
			if other, ok := byLower[strings.ToLower(lf.Rel)]; ok {
				matched[other] = true
				add(lf.Rel, other, Conflict, lf.Size, fmt.Sprintf("Drive has %q, which differs only by letter case", other))
			} else {
				add(lf.Rel, "", Upload, lf.Size, "")
			}
			continue
		}
		matched[lf.Rel] = true
		switch {
		case rf.Size != lf.Size:
			add(lf.Rel, rf.Rel, Conflict, lf.Size, "different size")
		case rf.MD5 == "":
			add(lf.Rel, rf.Rel, Conflict, lf.Size, "Drive has no checksum to compare")
		default:
			sum, err := hash(lf.Rel)
			switch {
			case err != nil:
				add(lf.Rel, rf.Rel, Conflict, lf.Size, "could not read local file: "+err.Error())
			case sum == rf.MD5:
				add(lf.Rel, rf.Rel, InSync, 0, "")
			default:
				add(lf.Rel, rf.Rel, Conflict, lf.Size, "different content")
			}
		}
	}
	for _, rf := range remote {
		if !matched[rf.Rel] {
			add(rf.Rel, rf.Rel, Download, rf.Size, "")
		}
	}
	sort.Slice(p.Items, func(i, j int) bool { return p.Items[i].Rel < p.Items[j].Rel })
	return p
}
