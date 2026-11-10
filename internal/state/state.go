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
// The delete_log and held_deletes tables were added later without a new
// version: older builds never read them, so they can safely ignore them.
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
);
CREATE TABLE IF NOT EXISTS delete_log (
	id         INTEGER PRIMARY KEY,
	at         INTEGER NOT NULL,
	root       TEXT    NOT NULL,
	os         TEXT    NOT NULL,
	files      INTEGER NOT NULL,
	local      INTEGER NOT NULL,
	remote     INTEGER NOT NULL,
	held       INTEGER NOT NULL,
	decision   TEXT    NOT NULL,
	decided_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS held_deletes (
	event_id INTEGER NOT NULL REFERENCES delete_log (id),
	rel      TEXT    NOT NULL,
	here     INTEGER NOT NULL,
	is_dir   INTEGER NOT NULL,
	drive_id TEXT    NOT NULL,
	size     INTEGER NOT NULL,
	PRIMARY KEY (event_id, rel)
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

// Delete removes the item at root/rel, if there is one.
func (d *DB) Delete(ctx context.Context, root, rel string) error {
	if _, err := d.db.ExecContext(ctx, `DELETE FROM items WHERE root = ? AND rel = ?`, root, rel); err != nil {
		return fmt.Errorf("removing %s from the state database: %w", rel, err)
	}
	return nil
}

// Items returns every item recorded for root, sorted by path: the base that
// later syncs compare against.
func (d *DB) Items(ctx context.Context, root string) ([]Item, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT rel, drive_id, is_dir, size, md5, local_mtime, synced_at, local_name
		FROM items WHERE root = ? ORDER BY rel`, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it := Item{Root: root}
		var isDir int
		if err := rows.Scan(&it.Rel, &it.DriveID, &isDir, &it.Size, &it.MD5, &it.LocalMTime, &it.SyncedAt, &it.LocalName); err != nil {
			return nil, err
		}
		it.IsDir = isDir != 0
		out = append(out, it)
	}
	return out, rows.Err()
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

// Decisions recorded in the delete log.
const (
	DeletesAuto      = "auto"      // under the guard's threshold; went ahead
	DeletesPending   = "pending"   // held; waiting for the owner
	DeletesApproved  = "approved"  // held, then approved by the owner
	DeletesCancelled = "cancelled" // held, then cancelled; the files were restored
	// Superseded: held, then no longer needed before the owner answered,
	// because a later run found nothing over the threshold to hold.
	DeletesSuperseded = "superseded"
)

// DeleteEvent is one run's deletes, logged so the big-delete guard's
// threshold can be tuned from real use.
type DeleteEvent struct {
	ID        int64
	At        int64 // unix seconds
	Root      string
	OS        string // "Linux" or "Windows"
	Files     int    // files in the root at the last sync
	Local     int    // files to trash here
	Remote    int    // files to trash in Drive
	Held      bool   // the guard asked the owner
	Decision  string // one of the Deletes* constants
	DecidedAt int64  // unix seconds; 0 while pending
}

// LogDeletes records ev and returns its ID.
func (d *DB) LogDeletes(ctx context.Context, ev DeleteEvent) (int64, error) {
	held := 0
	if ev.Held {
		held = 1
	}
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO delete_log (at, root, os, files, local, remote, held, decision, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.At, ev.Root, ev.OS, ev.Files, ev.Local, ev.Remote, held, ev.Decision, ev.DecidedAt)
	if err != nil {
		return 0, fmt.Errorf("logging deletes: %w", err)
	}
	return res.LastInsertId()
}

// DecideDeletes records the owner's answer to a held event.
func (d *DB) DecideDeletes(ctx context.Context, id int64, decision string, at int64) error {
	res, err := d.db.ExecContext(ctx, `
		UPDATE delete_log SET decision = ?, decided_at = ? WHERE id = ? AND decision = ?`,
		decision, at, id, DeletesPending)
	if err != nil {
		return fmt.Errorf("logging the delete decision: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("no pending deletes with ID %d", id)
	}
	return nil
}

// DeleteLog returns root's delete log, oldest first.
func (d *DB) DeleteLog(ctx context.Context, root string) ([]DeleteEvent, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, at, root, os, files, local, remote, held, decision, decided_at
		FROM delete_log WHERE root = ? ORDER BY id`, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeleteEvent
	for rows.Next() {
		var ev DeleteEvent
		var held int
		if err := rows.Scan(&ev.ID, &ev.At, &ev.Root, &ev.OS, &ev.Files, &ev.Local, &ev.Remote,
			&held, &ev.Decision, &ev.DecidedAt); err != nil {
			return nil, err
		}
		ev.Held = held != 0
		out = append(out, ev)
	}
	return out, rows.Err()
}

// HeldDelete is one delete the big-delete guard is holding for the owner.
type HeldDelete struct {
	Rel     string
	Here    bool // trash the local copy; false for the Drive copy
	Dir     bool
	DriveID string
	Size    int64
}

// HoldDeletes records the deletes a run held. A root has at most one pending
// event: if one is waiting already, it is updated to this run's counts and
// deletes instead of logging a new one, so the log keeps one entry per
// question asked. It returns the event's ID.
func (d *DB) HoldDeletes(ctx context.Context, ev DeleteEvent, items []HeldDelete) (int64, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM delete_log WHERE root = ? AND decision = ?`, ev.Root, DeletesPending).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx, `
			INSERT INTO delete_log (at, root, os, files, local, remote, held, decision)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
			ev.At, ev.Root, ev.OS, ev.Files, ev.Local, ev.Remote, DeletesPending)
		if err != nil {
			return 0, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE delete_log SET at = ?, os = ?, files = ?, local = ?, remote = ? WHERE id = ?`,
			ev.At, ev.OS, ev.Files, ev.Local, ev.Remote, id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM held_deletes WHERE event_id = ?`, id); err != nil {
			return 0, err
		}
	}
	for _, it := range items {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO held_deletes (event_id, rel, here, is_dir, drive_id, size) VALUES (?, ?, ?, ?, ?, ?)`,
			id, it.Rel, it.Here, it.Dir, it.DriveID, it.Size); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("saving held deletes: %w", err)
	}
	return id, nil
}

// PendingDeletes returns root's pending event and the deletes it holds, and
// false if nothing is waiting for the owner.
func (d *DB) PendingDeletes(ctx context.Context, root string) (DeleteEvent, []HeldDelete, bool, error) {
	ev := DeleteEvent{Root: root, Held: true, Decision: DeletesPending}
	err := d.db.QueryRowContext(ctx, `
		SELECT id, at, os, files, local, remote FROM delete_log WHERE root = ? AND decision = ?`,
		root, DeletesPending).Scan(&ev.ID, &ev.At, &ev.OS, &ev.Files, &ev.Local, &ev.Remote)
	if errors.Is(err, sql.ErrNoRows) {
		return DeleteEvent{}, nil, false, nil
	}
	if err != nil {
		return DeleteEvent{}, nil, false, err
	}
	rows, err := d.db.QueryContext(ctx, `
		SELECT rel, here, is_dir, drive_id, size FROM held_deletes WHERE event_id = ? ORDER BY rel`, ev.ID)
	if err != nil {
		return DeleteEvent{}, nil, false, err
	}
	defer rows.Close()
	var items []HeldDelete
	for rows.Next() {
		var it HeldDelete
		if err := rows.Scan(&it.Rel, &it.Here, &it.Dir, &it.DriveID, &it.Size); err != nil {
			return DeleteEvent{}, nil, false, err
		}
		items = append(items, it)
	}
	return ev, items, true, rows.Err()
}

// ResolveDeletes records decision for root's pending event, if there is one,
// and forgets the deletes it held.
func (d *DB) ResolveDeletes(ctx context.Context, root, decision string, at int64) error {
	ev, _, ok, err := d.PendingDeletes(ctx, root)
	if err != nil || !ok {
		return err
	}
	if err := d.DecideDeletes(ctx, ev.ID, decision, at); err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx, `DELETE FROM held_deletes WHERE event_id = ?`, ev.ID)
	return err
}
