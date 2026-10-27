// Package state is CloudSync's local record of what has been synced. Later
// syncs compare against it to tell local edits from remote ones.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS items (
	root        TEXT    NOT NULL,
	rel         TEXT    NOT NULL,
	drive_id    TEXT    NOT NULL,
	is_dir      INTEGER NOT NULL,
	size        INTEGER NOT NULL,
	md5         TEXT    NOT NULL,
	local_mtime INTEGER NOT NULL,
	synced_at   INTEGER NOT NULL,
	PRIMARY KEY (root, rel)
);`

// DB is the state database.
type DB struct{ db *sql.DB }

// Item is one file or folder as it was when last synced.
type Item struct {
	Root       string // root name from the config
	Rel        string // slash-separated path relative to the root
	DriveID    string
	IsDir      bool
	Size       int64
	MD5        string
	LocalMTime int64 // local modification time, unix nanoseconds
	SyncedAt   int64 // unix seconds
}

// Open opens or creates the database at path.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite has a single writer anyway, and per-connection
	// pragmas such as busy_timeout then always apply.
	db.SetMaxOpenConns(1)
	fail := func(err error) (*DB, error) {
		db.Close()
		return nil, fmt.Errorf("opening state database %s: %w", path, err)
	}
	for _, p := range []string{"PRAGMA busy_timeout = 5000", "PRAGMA journal_mode = WAL", "PRAGMA synchronous = NORMAL"} {
		if _, err := db.Exec(p); err != nil {
			return fail(err)
		}
	}
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fail(err)
	}
	if v > schemaVersion {
		return fail(fmt.Errorf("written by a newer CloudSync (schema %d)", v))
	}
	if _, err := db.Exec(schema); err != nil {
		return fail(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fail(err)
	}
	return &DB{db: db}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// Put inserts or replaces an item.
func (d *DB) Put(ctx context.Context, it Item) error {
	isDir := 0
	if it.IsDir {
		isDir = 1
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO items (root, rel, drive_id, is_dir, size, md5, local_mtime, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (root, rel) DO UPDATE SET
			drive_id = excluded.drive_id, is_dir = excluded.is_dir, size = excluded.size,
			md5 = excluded.md5, local_mtime = excluded.local_mtime, synced_at = excluded.synced_at`,
		it.Root, it.Rel, it.DriveID, isDir, it.Size, it.MD5, it.LocalMTime, it.SyncedAt)
	if err != nil {
		return fmt.Errorf("recording %s in the state database: %w", it.Rel, err)
	}
	return nil
}

// Get returns the item at root/rel, and false if there is none.
func (d *DB) Get(ctx context.Context, root, rel string) (Item, bool, error) {
	it := Item{Root: root, Rel: rel}
	var isDir int
	err := d.db.QueryRowContext(ctx, `
		SELECT drive_id, is_dir, size, md5, local_mtime, synced_at
		FROM items WHERE root = ? AND rel = ?`, root, rel).
		Scan(&it.DriveID, &isDir, &it.Size, &it.MD5, &it.LocalMTime, &it.SyncedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	it.IsDir = isDir != 0
	return it, true, nil
}

// Count returns how many files and folders are recorded for root.
func (d *DB) Count(ctx context.Context, root string) (files, dirs int, err error) {
	err = d.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(is_dir = 0), 0), COALESCE(SUM(is_dir = 1), 0)
		FROM items WHERE root = ?`, root).Scan(&files, &dirs)
	return files, dirs, err
}

// Meta returns a stored setting, and false if it is not set.
func (d *DB) Meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := d.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetMeta stores a setting.
func (d *DB) SetMeta(ctx context.Context, key, value string) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
