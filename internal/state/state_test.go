package state

import (
	"context"
	"database/sql"
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

func TestStandIns(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	for _, it := range []Item{
		{Root: "MyFiles", Rel: "Hackathon ", IsDir: true, LocalName: "Hackathon␣"},
		{Root: "MyFiles", Rel: "Hackathon /a:b.pdf", LocalName: "a：b.pdf"},
		{Root: "MyFiles", Rel: "plain.txt"},
		{Root: "Other", Rel: "x?", LocalName: "x？"},
	} {
		if err := db.Put(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.StandIns(ctx, "MyFiles")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["Hackathon "] != "Hackathon␣" || got["Hackathon /a:b.pdf"] != "a：b.pdf" {
		t.Errorf("StandIns = %q", got)
	}
}

func TestUpgradeFromSchema1(t *testing.T) {
	// A database written by the first release, as on the owner's Windows.
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "state.db")
	old, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE items (root TEXT NOT NULL, rel TEXT NOT NULL, drive_id TEXT NOT NULL, is_dir INTEGER NOT NULL,
			size INTEGER NOT NULL, md5 TEXT NOT NULL, local_mtime INTEGER NOT NULL, synced_at INTEGER NOT NULL,
			PRIMARY KEY (root, rel))`,
		`INSERT INTO items VALUES ('MyFiles', 'a.pdf', 'id1', 0, 3, 'abc', 42, 7)`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	it, ok, err := db.Get(ctx, "MyFiles", "a.pdf")
	if err != nil || !ok || it.DriveID != "id1" || it.LocalName != "" {
		t.Fatalf("after upgrade Get = %+v, %v, %v", it, ok, err)
	}
	if err := db.Put(ctx, Item{Root: "MyFiles", Rel: "b:c", LocalName: "b：c"}); err != nil {
		t.Fatal(err)
	}
	var v int
	db.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
}
