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

// schemaVersion 2 added items.local_name. A CloudSync that knows only
// version 1 refuses the database rather than ignore the stand-in names.
const schemaVersion = 2

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
	local_name  TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (root, rel)
);`

// migrations[v] upgrades a database from schema v to v+1.
var migrations = map[int]string{
	1: `ALTER TABLE items ADD COLUMN local_name TEXT NOT NULL DEFAULT ''`,
}

// DB is the state database.
type DB struct{ db *sql.DB }

// Item is one file or folder as it was when last synced.
type Item struct {
	Root       string // root name from the config
	Rel        string // true path: slash-separated, relative to the root, as in Drive
	LocalName  string // the stand-in name on Windows (package winname); "" if the local name is the true name
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
	for ; v > 0 && v < schemaVersion; v++ {
		if _, err := db.Exec(migrations[v]); err != nil {
			return fail(fmt.Errorf("upgrading from schema %d: %w", v, err))
		}
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
		INSERT INTO items (root, rel, drive_id, is_dir, size, md5, local_mtime, synced_at, local_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (root, rel) DO UPDATE SET
			drive_id = excluded.drive_id, is_dir = excluded.is_dir, size = excluded.size,
			md5 = excluded.md5, local_mtime = excluded.local_mtime, synced_at = excluded.synced_at,
			local_name = excluded.local_name`,
		it.Root, it.Rel, it.DriveID, isDir, it.Size, it.MD5, it.LocalMTime, it.SyncedAt, it.LocalName)
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
		SELECT drive_id, is_dir, size, md5, local_mtime, synced_at, local_name
		FROM items WHERE root = ? AND rel = ?`, root, rel).
		Scan(&it.DriveID, &isDir, &it.Size, &it.MD5, &it.LocalMTime, &it.SyncedAt, &it.LocalName)
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

// StandIns returns the stand-in name of every item of root that has one,
// keyed by true path, for winname.New.
func (d *DB) StandIns(ctx context.Context, root string) (map[string]string, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT rel, local_name FROM items WHERE root = ? AND local_name != ''`, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var rel, local string
		if err := rows.Scan(&rel, &local); err != nil {
			return nil, err
		}
		out[rel] = local
	}
	return out, rows.Err()
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
