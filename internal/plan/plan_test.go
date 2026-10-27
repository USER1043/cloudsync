package plan

import (
	"errors"
	"testing"

	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/localfs"
)

func TestFirstSync(t *testing.T) {
	local := []localfs.File{
		{Rel: "only-local.pdf", Size: 10},
		{Rel: "same.pptx", Size: 5},
		{Rel: "edited.docx", Size: 5},
		{Rel: "resized.png", Size: 7},
		{Rel: "Notes.txt", Size: 3},
		{Rel: "locked.xlsx", Size: 4},
	}
	remote := []gdrive.File{
		{Rel: "only-remote.jpg", Size: 20},
		{Rel: "same.pptx", Size: 5, MD5: "aaa"},
		{Rel: "edited.docx", Size: 5, MD5: "bbb"},
		{Rel: "resized.png", Size: 9, MD5: "ccc"},
		{Rel: "notes.txt", Size: 3, MD5: "ddd"},
		{Rel: "locked.xlsx", Size: 4, MD5: "eee"},
	}
	hashed := map[string]bool{}
	hash := func(rel string) (string, error) {
		hashed[rel] = true
		switch rel {
		case "same.pptx":
			return "aaa", nil
		case "locked.xlsx":
			return "", errors.New("file is open in another program")
		}
		return "different", nil
	}

	p := FirstSync(local, remote, hash)

	want := map[string]Kind{
		"only-local.pdf":  Upload,
		"only-remote.jpg": Download,
		"same.pptx":       InSync,
		"edited.docx":     Conflict,
		"resized.png":     Conflict,
		"Notes.txt":       Upload,   // a different file from notes.txt
		"notes.txt":       Download, // Windows gives one of them a stand-in name
		"locked.xlsx":     Conflict,
	}
	if len(p.Items) != len(want) {
		t.Fatalf("got %d items %+v, want %d", len(p.Items), p.Items, len(want))
	}
	for _, it := range p.Items {
		if k, ok := want[it.Rel]; !ok || k != it.Kind {
			t.Errorf("%s: got %v, want %v", it.Rel, it.Kind, k)
		}
	}
	if hashed["resized.png"] || hashed["only-local.pdf"] {
		t.Error("hashed a file that size alone already decided")
	}

	tot := p.Totals()
	if tot[Upload] != (Total{Count: 2, Bytes: 13}) || tot[Download] != (Total{Count: 2, Bytes: 23}) {
		t.Errorf("totals = %+v", tot)
	}
	if tot[InSync].Bytes != 0 {
		t.Error("in-sync files should transfer no bytes")
	}
	if got := len(p.Of(Conflict)); got != 3 {
		t.Errorf("conflicts = %d, want 3", got)
	}
	for i := 1; i < len(p.Items); i++ {
		if p.Items[i-1].Rel > p.Items[i].Rel {
			t.Fatal("items are not sorted by path")
		}
	}
}

func TestFirstSyncEmptyDrive(t *testing.T) {
	p := FirstSync([]localfs.File{{Rel: "a", Size: 1}, {Rel: "b", Size: 2}}, nil, func(string) (string, error) {
		t.Fatal("nothing should be hashed when Drive is empty")
		return "", nil
	})
	if tot := p.Totals()[Upload]; tot.Count != 2 || tot.Bytes != 3 {
		t.Errorf("upload totals = %+v", tot)
	}
}
