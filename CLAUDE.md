# CloudSync: context for Claude

This file carries the project's context between machines. The owner develops
on a **dual-boot laptop (Windows 11 + Fedora KDE)**. Work started in a Claude
session on Windows; continue here.

## Goal

One folder that is identical on both OSes of the dual-boot laptop, synced
through Google Drive, also reachable from the owner's iPhone through the
Google Drive app.

| | Windows | Fedora | Google Drive |
|---|---|---|---|
| Synced folder | `C:\MyFiles` | `~/My_Files` | `My Drive/CloudSync/MyFiles` |
| Config | `%AppData%\cloudsync\config.toml` | `~/.config/cloudsync/config.toml` | |
| State DB | `%LocalAppData%\cloudsync\state.db` | `~/.local/state/cloudsync/state.db` | |

Only one OS runs at a time, so "sync" means: catch up at login, stay live
while running, and make sure changes are uploaded before shutdown.

**Why Google Drive:** the owner's 1 TB OneDrive is a college account whose
admins block app registration and third-party consent (and it expires after
graduation). iCloud has no public Drive API. Google Drive has an official API
with a change feed. The free tier is **15 GB, shared with Gmail and Photos**:
design for that limit, including quota checks.

## Decisions already made (don't re-ask)

- **Mirror semantics.** Creating, editing, renaming or deleting a file syncs;
  reading does not. Change detection uses size, mtime and MD5, never access events.
- **Deletes propagate,** but always to the OS trash or Drive's trash, never a
  hard delete, with a big-delete guard (pause and ask).
- **First sync on each computer is a union merge:** copy files that exist on
  one side only, leave identical files alone (by checksum), and **keep both**
  copies when content differs (`name (conflict Linux 2026-10-27 1432).ext`).
  It never deletes anything.
- **Files only:** no symlinks, no permissions. Names that can't exist on
  Windows are skipped and reported.
- **Git repos are skipped entirely** (any folder with `.git`); code goes
  through GitHub. Dependency and cache folders are ignored by default.
- **Shutdown with pending uploads:** block shutdown with a message ("uploading
  N files"). If the user chooses "shut down anyway", keep the queue for the
  next boot of that OS **and** upload a small *unsynced-files list* so the
  other OS warns at login which files are stale.
- **Phone:** the Google Drive iOS app. Native Google Docs/Sheets/Slides are
  skipped (there is no file behind them).
- **Each user brings their own Google OAuth client** (Desktop app type). An
  app with full `drive` scope can't be offered publicly without Google's paid
  review. The owner's app is published (status "In production",
  unverified), with homepage and privacy policy served from `docs/` on GitHub Pages.

## Status

| Phase | State |
|---|---|
| 1: `login`, `whoami`, `plan` dry run | done |
| 2: `sync` first sync, state DB, retries, progress bar | done |
| Setup: `init`, gitignore-style rules, `.cloudsyncignore`, `max_file_size`, README guide | done |
| Windows first sync of `C:\MyFiles` | **done and verified**: 629 files, 377.5 MB, all MD5-matched |
| Fedora: build, `login`, `init`, first sync of `~/My_Files` | **next** |
| 3: continuous two-way sync daemon | not started |

After a root's first sync, `cloudsync sync` only reports "already completed".
Until Phase 3, later edits don't sync, and files coming from Fedora reach
Drive and the phone but not Windows.

## Fedora checklist (do this first)

1. Run `go test ./...` and `go vet ./...` on Linux. The symlink test only
   really runs here. Build with `go build -o bin/ ./cmd/cloudsync`.
2. Check that `~/.config/cloudsync/client_secret.json` exists (the owner copies
   it over by hand; never read or print its contents).
3. Run `bin/cloudsync login` and check that the token lands in KWallet through
   the Secret Service API (`go-keyring`). If the Secret Service is
   unavailable, report it rather than adding a plaintext fallback.
4. Run `bin/cloudsync init --local ~/My_Files` and pick `CloudSync/MyFiles`.
   The root name must be `MyFiles`.
5. Show the owner `bin/cloudsync plan` and wait for their go-ahead, then run
   `bin/cloudsync sync` in a real terminal (Konsole) so the progress bar shows.
   Expect downloads of the Windows files, uploads of Fedora-only files, maybe
   conflicts, and maybe Linux-only names that Windows rejects.

## Architecture

Go 1.26, no CGO (cross-compiles with `GOOS=linux`). Packages:

| Package | Role |
|---|---|
| `cmd/cloudsync` | CLI: `login`, `logout`, `whoami`, `init`, `plan`, `sync`; terminal progress bar (`progress.go`) |
| `internal/config` | `config.toml` load/write/validate, `ParseSize`, per-OS dirs. No built-in default root: a missing config returns `ErrNoConfig` |
| `internal/auth` | OAuth loopback + PKCE, token in OS keyring, refreshed tokens saved back |
| `internal/gdrive` | Drive v3: find/create folders, list trees, chunked resumable uploads, downloads, `Retry` (backoff on 429/5xx/rate-limit 403), `countingTransport` for upload progress, `Tree.Filter` |
| `internal/filter` | gitignore-style `Matcher` (last match wins, `!`, anchored `/x/`, `**`, case-insensitive), `.cloudsyncignore`, size limit, `CheckName` for Windows-portable names |
| `internal/localfs` | `Scan`: walks a root, skips repos, applies rules, flags issues; `MD5File` |
| `internal/plan` | `FirstSync`: pure union-merge planner |
| `internal/engine` | Carries out a plan: folders first, then 4 parallel transfers, MD5-verified, atomic downloads (`.name.cloudsync-partial` then rename), keep-both conflicts, progress callbacks |
| `internal/state` | SQLite (`modernc.org/sqlite`): `items` (per root/rel: drive_id, size, md5, local_mtime) and `meta` (Drive changes token, `first_sync_done:<root>`, `root_folder_id:<root>`) |

**The root name is the state DB key; never rename the `MyFiles` root.** The
Drive change-feed start token is saved before the first sync touches
anything, so Phase 3 can pick up every later change from it.

## Gotchas found so far

- Drive stores `modifiedTime` to **millisecond** precision, while NTFS keeps
  100 ns. Compare mtimes truncated to ms, or rely on MD5.
- `go-api` reads small upload bodies into memory before sending, which is
  why upload progress is counted at the HTTP transport, not by wrapping the file.
- On Windows the running `bin/cloudsync.exe` can't be overwritten, so don't
  rebuild while a sync runs.
- A throwaway `My Drive/CloudSync-Test` folder (live tests) may still exist.
  The owner can delete it; never sync into it from the real config.
- Live tests against real Drive use `CLOUDSYNC_CONFIG` and
  `CLOUDSYNC_STATE_DIR` pointing at a scratch config, state folder and Drive
  folder (e.g. `CloudSync-Test/...`). Never test against `CloudSync/MyFiles`.

## Phase 3 plan (agreed outline; discuss details with the owner before coding)

- **`cloudsync run` daemon,** started at login: a systemd **user** unit on
  Fedora, and a Task Scheduler "at logon" task on Windows (not a Session-0
  service, which can't reach the user's keyring or show notifications). It
  keeps the single-instance lock (already in `openState`).
- **Catch-up at start:** upload anything queued from the last session, read
  Drive changes since the stored token (`changes.list`, limited to the root's
  subtree; on an expired or invalid token, re-list and diff), scan locally,
  then reconcile.
- **Reconciler:** a pure three-way function of local, base (state DB) and
  remote. Upload, download, trash locally or in Drive, keep both on
  conflict, and an edit beats a delete. Big-delete guard and dry run. Test it
  with a table of cases.
- **Live mode:** fsnotify with about 3 s debounce and a stable-size check;
  a full rescan hourly and on watcher overflow; poll the Drive change feed
  every 1–2 min (phone edits).
- **Shutdown:** on Linux, a logind inhibitor in "block" mode (D-Bus
  `org.freedesktop.login1.Manager.Inhibit`) held only while uploads are
  pending, which KDE shows in its logout screen, plus a flush in `ExecStop`.
  On Windows, `ShutdownBlockReasonCreate` on a hidden window, answering
  `WM_QUERYENDSESSION`.
- **Unsynced-files list:** `CloudSync/.cloudsync/pending-<os>.json`, outside
  any root folder. The other OS reads it at start, sends a notification, and
  it is cleared once those files are uploaded.
- **Also:** desktop notifications (freedesktop D-Bus / Windows toast), a quota
  check that pauses before Drive is full, log file with rotation, rename
  detection (Drive file ID; local inode or file ID plus hash) as a follow-up.

## Working with the owner

- They are a student. Explain decisions plainly, discuss design before
  writing code, and ask before committing, pushing or touching real data.
- Keep code idiomatic and in the existing style. Run `gofmt`, `go vet` and
  `go test ./...` before proposing a commit. The repo uses LF endings
  (`.gitattributes`).
- Never commit or print `client_secret.json`, tokens or `config.toml`
  (`.gitignore` covers them).
