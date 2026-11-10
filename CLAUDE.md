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
- **Files only:** no symlinks, no permissions.
- **Names Windows can't store are translated on Windows only** (`ISSUES.md`
  #1, #2). Drive and Linux keep the true name; Windows stores a lookalike
  stand-in (`a:b` → `a：b`, trailing space → `␣`, `con.txt` → `con＿.txt`,
  a case clash → `notes (case 2).txt`), recorded per item in the state DB.
  Plans, the state DB and Drive always use true paths; only `abs()` on
  Windows sees stand-ins. Names no system can hold (`/`, `.`, `..`, exact
  Drive duplicates, invalid UTF-8) are still skipped and reported.
- **Git repos are skipped entirely** (any folder with `.git`); code goes
  through GitHub. Dependency and cache folders are ignored by default.
- **Shutdown with pending uploads (revised 2026-11-10, Phase 3 step 4):**
  the moment shutdown starts, upload a small *unsynced-files list* so the
  other OS warns at its next start which files are stale; then keep
  uploading until the system stops CloudSync. Unsent edits stay on disk and
  upload at that OS's next start (nothing to queue: a local change is
  whatever differs from the state DB). Details under "Phase 3 plan".
  - **Fedora never blocks shutdown.** Tested 2026-11-10 on Plasma 6.8.2: with
    a logind "block" inhibitor held, KDE's Shut Down silently does nothing
    (logind answers `CanPowerOff` with "challenge"; no message, no "shut
    down anyway"). Instead a logind **delay** inhibitor (max 5 s,
    `InhibitDelayMaxUSec`) gives time to upload the list, then the SIGTERM
    flush gets the user unit's stop time (Fedora: 45 s default, 1 min for
    `user@1000.service`; Wi-Fi is still up).
  - **Windows blocks** ("CloudSync is preventing shutdown: uploading N
    files", `ShutdownBlockReasonCreate` + `WM_QUERYENDSESSION`), **but not
    when offline**: waiting can't help (owner's decision).
  - **Sleep is never blocked** (a laptop in a bag must sleep).
  - **The list checks itself:** each entry records size and modified time
    (uploads keep the local mtime), so the reader warns only about files
    whose Drive copy still differs; a late-finishing upload causes no false
    alarm. Not sent when nothing is pending or when offline.
- **Phone:** the Google Drive iOS app. Native Google Docs/Sheets/Slides are
  skipped (there is no file behind them).
- **Each user brings their own Google OAuth client** (Desktop app type). An
  app with full `drive` scope can't be offered publicly without Google's paid
  review. The owner's app is published (status "In production",
  unverified), with homepage and privacy policy served from `docs/` on GitHub Pages.

## Status

### Where we left off (2026-11-10): give the owner this when they ask

Phase 3 (background service), steps 3–5 committed and pushed (`e6c2d72`,
`de002b2`, `3d3a173`); **step 6 (start at login: `cloudsync start`,
`stop`, `update`) built and live-tested on scratch 2026-11-10, and
switched on for real on Fedora the same day** (the owner approved after
`plan` showed 1 file to upload): `~/.local/bin/cloudsync`,
`cloudsync.service` enabled and active; its catch-up took 2 min (mostly
the Drive listing), uploaded that file, and it now watches `~/My_Files`.
Fedora syncs automatically from here on. Updated to `3736c6e` with `bin/cloudsync update` right after the step 6
commit (the first real use of `update`: stopped with its flush, swapped,
restarted, caught up: all 1,426 files in sync).

- **Step 6 (decided with the owner 2026-11-10):** commands `start` (was
  "install"; installs the program once, registers it to start at login,
  starts it; idempotent; never replaces the installed copy), `stop` (was
  "uninstall"; stops after the flush and removes the login entry, keeps the
  program file) and `update` (a separate command, run from the new build:
  refuses if not installed or run from the installed copy; "already up to
  date" if identical; stops, keeps `.previous`, starts and checks it runs,
  else restores the previous one). Versions come from git (`buildVersion`).
  Linux: `~/.local/bin/cloudsync` + systemd user unit tied to
  `graphical-session.target`, `Restart=on-failure`, exit status 3 (needs
  the owner) not restarted. Windows (owner agreed): per-user **Startup
  entry** (HKCU Run) instead of Task Scheduler (no admin), a windowless
  copy (PE subsystem patched, like `editbin`), a log file, stop via a named
  event; untested on Windows. Scratch configs get their own unit name and
  program copy (`place`). Live-tested on scratch: start, start again,
  update (same build, new build, broken build → fallback, no restart loop),
  live upload under the service, stop, stop again.
- **Step 5 (decided with the owner 2026-11-10):** notifications only for
  what needs the owner or that they'd want to know (stale files from
  another computer, held deletes, conflicts, files failing for a local
  reason, Drive full, sign-in, root folder, offline **30+ min**, Google
  failing **10+ min**); never for routine syncs, retries or rate limits.
  Same message at most once an hour; resolved problems take their
  notification down. Linux: freedesktop notifications on the session bus.
  Windows: a **tray icon** (owner agreed) whose balloons show as
  notifications; untested on Windows. `cloudsync status` added (owner
  agreed). Live-tested with A as a systemd user unit: conflict, held
  deletes and an unreadable file notified with no errors (the owner saw
  them in KDE and confirmed they looked right); `status` showed
  them and they cleared once fixed; a second `run` is refused.

- **Works now:** two-way `cloudsync plan`/`sync`, the big-delete guard with
  `cloudsync deletes` (`approve`/`cancel`), and `cloudsync run`: catch-up
  with Drive at start, then local changes uploaded ~3 s after they settle,
  a flush on Ctrl-C/SIGTERM/SIGHUP, offline retries, other commands usable
  alongside. Live-tested 2026-11-10 on scratch Drive folders.
- **Step 4 (built 2026-11-10, uncommitted):** `internal/shutdown` (logind
  delay inhibitor + `PrepareForShutdown` on Linux; hidden window with
  `ShutdownBlockReasonCreate` on Windows, holding only while uploads are
  pending and Drive is reachable), `cmd/cloudsync/pending.go` (the
  unsynced-files list) and its use in `run.go`: `shutdownStarting` saves the
  list, `flush` (40 s, skipped at once if `Ping` fails) uploads and then
  removes it, `catchUp` warns about other systems' stale lists
  (`warnStale`) and removes this system's own; `gdrive/small.go`
  (`ReadSmall`, `WriteSmall`, `RemoveSmall`, `Ping`). Unit tests pass;
  checked under a systemd user service: keyring works, logind lists the
  inhibitor as "shutdown"/"delay", stop flushes.
- **Real shutdown test passed (2026-11-10)** with scratch computers A and B
  in `~/.local/share/cloudsync-shutdown-test` (deleted afterwards at the
  owner's request; recreate scratch configs as needed; Drive folder
  `CloudSync-Test/TwoWay-1110`, which still exists), A running as a transient systemd
  user unit, a 150 MB upload in progress: "shutting down" logged, the list
  saved within 2.4 s, the small file already up, the 40 s flush stopped
  cleanly inside Fedora's 45 s, and A's next start uploaded the rest. The
  test found `ISSUES.md` #8 (lists named by OS; now by computer ID), fixed
  and re-tested: B warned, A removed its list, B then gave no warning.
  The Windows half (window, hold reason) is untested until Windows.
- **Real `MyFiles`:** first two-way sync on Fedora done and verified
  2026-11-10 (1,426 files in sync); `cloudsync run` not tried on it yet.
- **Not yet:** the Windows catch-up (rebuild `C:\MyFiles`' state, let the
  new build reconcile the old layout; see "Windows follow-up" below), and
  testing every Windows-only part there: watcher, Recycle Bin, shutdown
  screen, tray notifications, Startup entry, windowless copy, log file.

| Phase | State |
|---|---|
| 1: `login`, `whoami`, `plan` dry run | done |
| 2: `sync` first sync, state DB, retries, progress bar | done |
| Setup: `init`, gitignore-style rules, `.cloudsyncignore`, `max_file_size`, README guide | done |
| Windows first sync of `C:\MyFiles` | **done and verified**: 629 files, 377.5 MB, all MD5-matched |
| Windows-name translation (`internal/winname`, `ISSUES.md` #1, #2) | done on Linux; verify on Windows |
| Fedora first sync of `~/My_Files` | **done and verified** 2026-10-28: nothing failed; re-plan shows 1,341 files in sync, 0 to transfer |
| Faster uploads (`ISSUES.md` #5) | **done** 2026-11-03: test sync of 52 folders + 73 files went from 122 s to 33 s |
| 3: continuous two-way sync daemon (background service) | **in progress**: two-way `plan`/`sync`, `cloudsync deletes`, `cloudsync run` (catch-up + live upload) and shutdown handling done and live-tested 2026-11-10. Next: notifications and errors (step 5), start at login (step 6) |

After a root's first sync, `cloudsync sync` runs a two-way sync by hand
(`cmd/cloudsync/twoway.go`) and `cloudsync run` keeps syncing
(`run.go`, `live.go`). Nothing starts at login yet, and Windows still runs
the old build.

### Fedora first sync (2026-10-28)

The repo is public, so real file and folder names from the synced folder stay
out of this file (they are in Claude's private project memory instead).

- 717 files uploaded, 584 downloaded, 34 already identical, 3 conflicts kept
  as both copies; nothing failed.
- **Fedora's folder layout is the final one.** Before syncing, the owner
  moved the folders Windows had at different paths in Drive into Fedora's
  layout, so shared files were compared instead of duplicated.
- A root `.cloudsyncignore` keeps secrets out of Drive (`.env`, `*.pem`,
  `client_secret*.json`, recovery-code files). It syncs, so Windows gets the
  same rules. Fedora's `config.toml` also ignores two large, re-downloadable
  folders (model weights, a competition dataset).
- **Windows follow-up (decided 2026-11-05: Phase 3 catches it up):**
  `C:\MyFiles` still has the old layout and its state DB records old paths.
  Rebuild there (state schema v2) and let `cloudsync run` reconcile. Without
  rename detection every moved file looks like "deleted at the old path, new
  at the new path", so Windows downloads the new paths (~377 MB), trashes the
  old copies to the Recycle Bin, and the big-delete guard pauses once for the
  owner to approve.

## Next steps (in this order; discuss design with the owner before coding)

1. **Faster uploads (done 2026-11-03, `ISSUES.md` #5).** Measured
   2026-10-28 on the owner's connection (throwaway benchmark against
   `CloudSync-Test/UploadBench`, files deleted afterwards):

   | Test | Result |
   |---|---|
   | 1 large file, 8 MB chunks | 18.3 Mbit/s |
   | 4 large files in sequence, 32 MB chunks | 17.4 Mbit/s |
   | 4 large files at once | 20.9 Mbit/s |
   | 64 × 16 KB files, 1 / 4 / 16 at once | 0.5 / 2.5 / 3.65 files/s |

   Conclusions: large files are bound by the ~20 Mbit/s uplink (one stream
   nearly fills it; chunk size made no measurable difference). Small files
   are bound by ~2 s per request, and parallelism helps up to ~3–4 files/s,
   apparently Google's per-user write limit. Folders are created one at a
   time before any transfer, two requests each (lookup + create), which costs
   minutes on a first sync with hundreds of new folders.

   Built (items 1–3; item 4 kept as is):
   1. Faster folder creation: skip the lookup inside a folder created in the
      same run (it's empty), and create folders of the same depth in
      parallel. Must stay safe on retry.
   2. 8 parallel transfers instead of 4 (`engine.Options.Workers`).
   3. Start the largest files first, so big files overlap with small-file
      latency instead of running alone at the end.
   4. Leave `gdrive.chunkSize` at 8 MiB.

   Not possible: going past the uplink.
2. **Background service:** the Phase 3 `cloudsync run` daemon below
   (systemd user unit on Fedora, logon task on Windows).

## Fedora checklist (done 2026-10-28; kept for a fresh Linux machine)

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

Go 1.27.3 (`go.mod`), no CGO (cross-compiles with `GOOS=linux`). On Fedora,
run `go env -w GOTOOLCHAIN=auto` once so Go fetches 1.27.3 (`ISSUES.md` #3).
Packages:

| Package | Role |
|---|---|
| `cmd/cloudsync` | CLI: `login`, `logout`, `whoami`, `init`, `plan`, `sync`, `deletes`; terminal progress bar (`progress.go`). `plan`/`sync` use `plan.FirstSync` until a root's first sync is done, then `twoway.go`: `planLater` (root-folder checks, `reconcile.Reconcile` with `Skips`, `Gate`), `syncLater`/`runLater` (delete log, held deletes, `engine.Sync`). `deletes approve`/`cancel` run a full sync with that answer, so held deletes are re-checked first. `run.go`: `cloudsync run` (catch-up per root with retries, then a loop: `internal/watch` signal → `schedule` (3 s quiet, 30 s cap) → live pass; hourly rescan; flush at exit; `fatal` vs retried errors). `live.go`: `planLive` rescans and reconciles against the state DB standing in for Drive (`treeFromBase`), so a live pass only uploads or trashes in Drive; it skips held deletes, files changed in the last 2 s and the `session`'s unsynced Drive paths, and its held deletes join the pending question (`later.live`). `openState` waits for another process's lock. `errors.go`: `classify`/`describe` turn errors into plain words with a fix (offline, Google failing, rate limit, sign-in, Drive full, root folder, local file problems); used for logs, `main`'s errors and `engine.Options.Explain`. `status.go`: `statusKeeper` writes `status.json` in the state folder (state, detail, recent warnings), `run.lock` marks `run` as running (one instance), `cloudsync status`. In `run.go`, `alert`/`resolve`, `trouble`/`recovered` (quiet until offline 30 min or failing 10 min), `afterSync` (held deletes, conflicts, failing files) |
| `internal/config` | `config.toml` load/write/validate, `ParseSize`, per-OS dirs. No built-in default root: a missing config returns `ErrNoConfig` |
| `internal/auth` | OAuth loopback + PKCE, token in OS keyring, refreshed tokens saved back |
| `internal/gdrive` | Drive v3: find/create folders (`EnsureFolder` looks first; `CreateFolder` only on retry), list trees, chunked resumable uploads, `Update` in place (refuses with `ErrChanged` if Drive's copy isn't the last-synced checksum), `Trash`, `HasChildren`, downloads, `Retry` (backoff on 429/5xx/rate-limit 403/network; never on a refused sign-in), `Temporary`, `SignInRejected`, `Stat`, `countingTransport` for upload progress, `Tree.Filter` |
| `cmd/cloudsync` (setup) | `setup.go`: `start`/`stop`/`update`, `place` (usual vs scratch placement), `service` interface; `setup_linux.go` (systemd user unit), `setup_windows.go` (Startup entry, windowless copy, stop event, `run --log-file/--config/--state-dir`); `pe.go` (`makeWindowless`); `logfile.go` (rotating log, 5 MB + 1 old); `version.go` (git commit as version) |
| `internal/notify` | Desktop notifications with no repeats (same key and text at most once an hour; a new message replaces its key's last one; `Clear` when the problem is gone; nil `*Notifier` is a no-op). Linux: `org.freedesktop.Notifications` on the session bus (critical ones stay until dismissed). Windows: tray icon (`Shell_NotifyIconW`) on a hidden window; balloons show as notifications |
| `internal/shutdown` | Hears a shutdown as early as the system allows: `Hooks.Starting` (save the unsynced-files list), `Holding` (Windows only: the reason to hold, or ""), `Ending` (Windows `WM_ENDSESSION`). Linux: logind **delay** inhibitor (never block: KDE gives no message for a blocked shutdown) and `PrepareForShutdown`, matched only from logind. Windows: hidden top-level window on a locked thread. Sleep is never touched |
| `internal/watch` | Signals "something changed" under a root, never what (callers rescan, so a lost or merged event can't lose an edit). Linux: fsnotify (inotify) per folder, folder list refreshed after each pass. Windows: one recursive `ReadDirectoryChangesW` on the root, so subfolders stay renamable |
| `internal/filter` | gitignore-style `Matcher` (last match wins, `!`, anchored `/x/`, `**`, case-insensitive), `.cloudsyncignore`, size limit, `CheckName` for names no system can sync |
| `internal/winname` | Windows stand-in names: `Encode` (pure), and `Map` (true path ↔ local name, case-insensitive taken names, `Found` for scans, `Assign` for new entries). A nil `*Map` is the identity, used on Linux |
| `internal/localfs` | `Scan`: walks a root, skips repos, applies rules, flags issues, reports true paths through a `winname.Map`; `MD5File` |
| `internal/plan` | `FirstSync`: pure union-merge planner; exact path matching |
| `internal/engine` | Carries out a plan (`FirstSync` from `plan`, `Sync` from `reconcile`; both run the same reconcile actions): folders a depth at a time (each depth in parallel; `CreateFolder` skips the lookup inside folders made in the same run), then 8 parallel transfers, largest first, MD5-verified, atomic downloads (`.name.cloudsync-partial` then rename), keep-both conflicts, progress callbacks. `Sync` checks every change against what the plan saw: a local file saved after the scan is never overwritten or trashed, and an update that finds a newer Drive edit becomes a conflict. Trashes go to `Options.Trash` (default `trash.Move`) or Drive's trash; folders are removed only when empty |
| `internal/trash` | `Move` to the OS trash: freedesktop.org Trash on Linux (home trash, or the file system's `.Trash-$uid` across devices/subvolumes), Recycle Bin via `SHFileOperationW` on Windows (asks before deleting outright what the Recycle Bin can't take) |
| `internal/state` | SQLite (`modernc.org/sqlite`): `items` (per root/rel: drive_id, size, md5, local_mtime, local_name for Windows stand-ins; schema v2, migrates v1) and `meta` (Drive changes token, `first_sync_done:<root>`, `root_folder_id:<root>`, `computer_id`); `delete_log` (each run's deletes, root size, whether the guard held them and the owner's answer: auto, pending, approved, cancelled, superseded) and `held_deletes` (the pending event's deletes; one pending event per root) |
| `internal/reconcile` | Phase 3 planner: `Reconcile`, a pure three-way comparison of scan, Drive tree and base (state DB) into actions (upload, download, trash here or in Drive, conflict, folders, record, forget, wait); `Skip` keeps paths missing for other reasons (too large, unreadable, repos) from being trashed. `Skips` builds it from the scan and Drive tree (repos, too large, unreadable, Drive duplicates, Google Docs, Drive-side filtered). `Guard` interface with `DefaultGuard` (more than 50 files or 10%); `Gate` applies it with the owner's answer to earlier held deletes (approve runs them, cancel `Undo`es them) |

**The root name is the state DB key; never rename the `MyFiles` root.** The
Drive change-feed start token is saved before the first sync touches
anything, so Phase 3 can pick up every later change from it.

## Issue log

`ISSUES.md` tracks every problem found and how it was solved. Add an entry
as soon as an issue is found, update it when it is solved, and never delete
solved entries. The repo is public: no real file names from the synced folder.

## Gotchas found so far

- A bare `syscall.Errno` (e.g. "no space left on device") satisfies
  `net.Error`, so "is it a network error?" must look for `*url.Error`,
  `*net.OpError` or `*net.DNSError`, and check `*fs.PathError` first.
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

## Phase 3 plan (outline agreed; details below decided 2026-11-05)

Decided with the owner on 2026-11-05:

- **Build for Fedora and Windows together.**
- **Drive is checked once per boot** (when `cloudsync run` starts), and when
  the owner runs `cloudsync sync` by hand. There is no periodic Drive poll:
  phone edits arrive at the next boot. **Reconfirmed 2026-11-10** after the
  trade-off was raised (sleep isn't a boot, so a laptop that only sleeps
  never re-checks): the owner rarely edits on the phone and usually shuts
  the laptop down, so it stays as is. Don't re-ask. The trade-off is
  documented for other users in the README ("When Drive is checked"); the
  possible addition, if wanted later, is a change-feed check every ~10 min,
  on resume from sleep and on network return, running the usual catch-up
  when anything changed.
- **Local edits upload live,** a few seconds after they settle, plus a final
  flush at shutdown.
- **Big-delete guard:** hold all deletes of a run, and ask, when it would
  trash **more than 50 files or more than 10% of the root's files**. The
  numbers are a starting guess: the guard is a pluggable policy, and every
  run's delete count, the root's size and the owner's answer (approved or
  cancelled) are logged in the state DB, so the threshold can be tuned from
  real use and, later, worked out by the app itself. Design that adaptive
  policy with the owner once there is data; it must never loosen the guard
  silently.
- **Errors explain themselves:** each failure to reach Drive gets a message
  with its cause and fix. Offline: changes queue and retry when the network
  is back. Sign-in expired or revoked: "run `cloudsync login`". Drive full:
  pause uploads and say which services share the 15 GB. Root folder trashed
  or missing in Drive: stop and say so, never re-upload blindly. Rate limits:
  silent backoff. Repeated Google server errors: flag "Google Drive is having
  problems, not your setup" with a link to Google's status page, and retry
  later.

Outline:

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
- **Live mode (built 2026-11-10):** a watcher signal, then about 3 s of
  quiet (30 s at most), then a full local rescan compared with the state DB;
  files changed in the last 2 s wait for the next pass. Also an hourly
  rescan and one on watcher overflow. Drive is not polled (see above). A
  full rescan of the owner's folder takes ~45 ms.
- **Shutdown (step 4, design agreed 2026-11-10; see "Decisions already
  made"):** on Linux a logind **delay** inhibitor (D-Bus
  `org.freedesktop.login1.Manager.Inhibit`, mode "delay") and the
  `PrepareForShutdown` signal: upload the unsynced-files list within the
  5 s, release, then the SIGTERM flush (about 40 s; skipped at once when
  offline). On Windows, a hidden top-level window answering
  `WM_QUERYENDSESSION` with `ShutdownBlockReasonCreate` while uploads are
  pending and Drive is reachable, and finishing in `WM_ENDSESSION`.
- **Unsynced-files list:** `<root's parent>/.cloudsync/pending-<os>-<computer id>-<root>.json`
  in Drive (`CloudSync/.cloudsync/pending-linux-3f9a1c2b-MyFiles.json`),
  outside the root folder; the computer ID is random, kept in the state DB
  (`computer_id`), so two computers with the same OS don't mix up their
  lists (`ISSUES.md` #8). Entries: path plus size and modified time, or
  "deleted"; the list also names its OS, host and root. Other computers
  read every `pending-*.json` there at their catch-up and warn about
  entries Drive still doesn't match (a log line now, a notification in
  step 5). The writer deletes it after its next clean catch-up or a
  complete flush.
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
