package state

import (
	"context"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "sub", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestItems(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	if _, ok, err := db.Get(ctx, "MyFiles", "a.pdf"); err != nil || ok {
		t.Fatalf("Get on empty db = %v, %v", ok, err)
	}
	it := Item{Root: "MyFiles", Rel: "a.pdf", DriveID: "id1", Size: 3, MD5: "abc", LocalMTime: 42, SyncedAt: 7}
	if err := db.Put(ctx, it); err != nil {
		t.Fatal(err)
	}
	it.MD5, it.Size = "def", 4 // upsert replaces
	if err := db.Put(ctx, it); err != nil {
		t.Fatal(err)
	}
	if err := db.Put(ctx, Item{Root: "MyFiles", Rel: "Sem5", DriveID: "id2", IsDir: true}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := db.Get(ctx, "MyFiles", "a.pdf")
	if err != nil || !ok || got != it {
		t.Errorf("Get = %+v, %v, %v; want %+v", got, ok, err, it)
	}
	files, dirs, err := db.Count(ctx, "MyFiles")
	if err != nil || files != 1 || dirs != 1 {
		t.Errorf("Count = %d files, %d dirs, %v", files, dirs, err)
	}
	if files, _, _ := db.Count(ctx, "Other"); files != 0 {
		t.Errorf("other root has %d files", files)
	}
}

func TestMeta(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	if _, ok, err := db.Meta(ctx, "k"); ok || err != nil {
		t.Fatalf("Meta on empty db = %v, %v", ok, err)
	}
	for _, v := range []string{"one", "two"} {
		if err := db.SetMeta(ctx, "k", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok, err := db.Meta(ctx, "k"); v != "two" || !ok || err != nil {
		t.Errorf("Meta = %q, %v, %v", v, ok, err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMeta(ctx, "k", "v")
	db.Close()

	db, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, _, _ := db.Meta(ctx, "k"); v != "v" {
		t.Errorf("after reopen Meta = %q", v)
	}
}
