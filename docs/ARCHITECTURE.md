# Architecture

Go 1.27.3 (`go.mod`), no CGO, so it cross-compiles (`GOOS=linux`,
`GOOS=windows`). The design decisions behind this layout are in
`DECISIONS.md`.

## How a sync works

1. `localfs.Scan` walks the local folder and the Drive folder is listed
   (`gdrive`), both filtered by the same rules (`filter`).
2. A planner turns the two listings into actions. A root's first sync uses
   `plan.FirstSync` (union merge). Every later sync uses
   `reconcile.Reconcile`, a three-way comparison of the scan, the Drive
   tree and the base (what the state DB recorded at the last sync).
3. `engine` carries the actions out and records each result in the state
   DB (`state`).

**The root name is the state DB key; never rename a root.** The Drive
change-feed start token is saved before the first sync touches anything.

## Packages

### `cmd/cloudsync`: the command line

Commands: `login`, `logout`, `whoami`, `init`, `plan`, `sync`, `deletes`,
`run`, `status`, `start`, `stop`, `update`, `version`.

| File | Role |
|---|---|
| `plan.go`, `sync.go` | `plan` and `sync`. They use `plan.FirstSync` until a root's first sync is done, then `twoway.go`. `openState` takes the sync lock and waits for another process's lock; `peekState` reads without it |
| `twoway.go` | Later syncs: `planLater` (root-folder checks, `reconcile.Reconcile` with `Skips`, `Gate`), `syncLater`/`runLater` (delete log, held deletes, `engine.Sync`) |
| `deletes.go` | `deletes`, `deletes approve`, `deletes cancel`. Approve and cancel run a full sync with that answer, so held deletes are re-checked first |
| `run.go` | `cloudsync run`: catch-up per root with retries, then a loop: `internal/watch` signal → `schedule` (3 s quiet, 30 s cap) → live pass; hourly rescan; flush at exit; `fatal` vs retried errors. Also `alert`/`resolve`, `trouble`/`recovered` (quiet until offline 30 min or failing 10 min), `afterSync` (held deletes, conflicts, failing files), `shutdownStarting`, `holding`, `flush` (40 s, skipped at once if `Ping` fails) |
| `live.go` | `planLive` rescans and reconciles against the state DB standing in for Drive (`treeFromBase`), so a live pass only uploads or trashes in Drive. It skips held deletes, files changed in the last 2 s and the `session`'s unsynced Drive paths; its held deletes join the pending question (`later.live`) |
| `pending.go` | The unsynced-files list saved at shutdown; `catchUp` warns about other computers' stale lists (`warnStale`) and removes this computer's own |
| `errors.go` | `classify`/`describe` turn errors into plain words with a fix (offline, Google failing, rate limit, sign-in, Drive full, root folder, local file problems). Used for logs, `main`'s errors and `engine.Options.Explain` |
| `status.go` | `statusKeeper` writes `status.json` in the state folder (state, detail, recent warnings); `run.lock` marks `run` as running (one instance); `cloudsync status` |
| `setup.go` | `start`/`stop`/`update`, `place` (usual vs scratch placement), the `service` interface |
| `setup_linux.go` | systemd user unit |
| `setup_windows.go` | Startup entry, windowless copy, stop event, `run --log-file/--config/--state-dir` |
| `pe.go` | `makeWindowless`: patches the PE subsystem, like `editbin` |
| `logfile.go` | Rotating log, 5 MB + 1 old |
| `version.go` | Git commit as the version (`buildVersion`) |
| `progress.go` | Terminal progress bar |

### `internal/config`

`config.toml` load/write/validate, `ParseSize`, per-OS dirs. No built-in
default root: a missing config returns `ErrNoConfig`. `CLOUDSYNC_CONFIG`
and `CLOUDSYNC_STATE_DIR` override the config file and state folder.

### `internal/auth`

OAuth loopback + PKCE, token in the OS keyring (`go-keyring`), refreshed
tokens saved back.

### `internal/gdrive`

Drive v3:

- Folders: `EnsureFolder` looks first; `CreateFolder` skips the lookup and
  looks only on a retry.
- Listing: `ListTree`, `Tree.Filter`, `HasChildren`, `Stat`.
- Transfers: chunked resumable uploads (8 MiB chunks), downloads, `Update`
  in place (refuses with `ErrChanged` if Drive's copy isn't the last-synced
  checksum), `Trash`. `countingTransport` counts upload progress.
- `Retry`: backoff on 429, 5xx, rate-limit 403 and network errors; never
  on a refused sign-in. `Temporary` and `SignInRejected` classify errors.
- `small.go`: `ReadSmall`, `WriteSmall`, `RemoveSmall`, `Ping`.

### `internal/filter`

gitignore-style `Matcher` (last match wins, `!`, anchored `/x/`, `**`,
case-insensitive), `.cloudsyncignore`, size limit, and `CheckName` for
names no system can sync.

### `internal/winname`

Windows stand-in names: `Encode` (pure), and `Map` (true path ↔ local
name, case-insensitive taken names, `Found` for scans, `Assign` for new
entries). A nil `*Map` is the identity, used on Linux.

### `internal/localfs`

`Scan` walks a root, skips repos, applies rules, flags issues, and reports
true paths through a `winname.Map`. `MD5File`.

### `internal/plan`

`FirstSync`: the pure union-merge planner. Exact path matching.

### `internal/reconcile`

The planner for later syncs.

- `Reconcile`: a pure three-way comparison of scan, Drive tree and base
  into actions (upload, download, trash here or in Drive, conflict,
  folders, record, forget, wait).
- `Skip` keeps paths that are missing for other reasons from being
  trashed. `Skips` builds it from the scan and Drive tree (repos, too
  large, unreadable, Drive duplicates, Google Docs, Drive-side filtered).
- `Guard` interface with `DefaultGuard` (more than 50 files or 10%).
  `Gate` applies it with the owner's answer to earlier held deletes
  (approve runs them, cancel `Undo`es them).

### `internal/engine`

Carries out a plan: `FirstSync` from `plan`, `Sync` from `reconcile`; both
run the same actions.

- Folders a depth at a time, each depth in parallel.
- Then 8 parallel transfers, largest first, MD5-verified.
- Atomic downloads (`.name.cloudsync-partial`, then rename), keep-both
  conflicts, progress callbacks.
- `Sync` checks every change against what the plan saw: a local file saved
  after the scan is never overwritten or trashed, and an update that finds
  a newer Drive edit becomes a conflict.
- Trashes go to `Options.Trash` (default `trash.Move`) or Drive's trash;
  folders are removed only when empty.

### `internal/trash`

`Move` to the OS trash. Linux: freedesktop.org Trash (home trash, or the
file system's `.Trash-$uid` across devices/subvolumes). Windows: Recycle
Bin via `SHFileOperationW` (asks before deleting outright what the Recycle
Bin can't take).

### `internal/state`

SQLite (`modernc.org/sqlite`), schema v2 (migrates v1).

| Table | Holds |
|---|---|
| `items` | Per root/rel: drive_id, size, md5, local_mtime, local_name (Windows stand-in) |
| `meta` | Drive changes token, `first_sync_done:<root>`, `root_folder_id:<root>`, `computer_id` |
| `delete_log` | Each run's deletes, root size, whether the guard held them and the owner's answer (auto, pending, approved, cancelled, superseded) |
| `held_deletes` | The pending event's deletes; one pending event per root |

### `internal/watch`

Signals "something changed" under a root, never what: callers rescan, so a
lost or merged event can't lose an edit. Linux: fsnotify (inotify) per
folder, with the folder list refreshed after each pass. Windows: one
recursive `ReadDirectoryChangesW` on the root, so subfolders stay
renamable.

### `internal/shutdown`

Hears a shutdown as early as the system allows. `Hooks.Starting` (save the
unsynced-files list), `Holding` (Windows only: the reason to hold, or ""),
`Ending` (Windows `WM_ENDSESSION`). Linux: a logind **delay** inhibitor and
`PrepareForShutdown`, matched only from logind. Windows: a hidden top-level
window on a locked thread, answering `WM_QUERYENDSESSION` with
`ShutdownBlockReasonCreate`. Sleep is never touched.

### `internal/notify`

Desktop notifications with no repeats: the same key and text at most once
an hour; a new message replaces its key's last one; `Clear` when the
problem is gone; a nil `*Notifier` is a no-op. Linux:
`org.freedesktop.Notifications` on the session bus (critical ones stay
until dismissed). Windows: a tray icon (`Shell_NotifyIconW`) on a hidden
window; balloons show as notifications.
