# Issue log

Problems found while building and running CloudSync, and how each was
solved. Add an entry when an issue is found and update it when it is
solved; don't delete solved entries. This repo is public, so use generic
examples, never real file names or paths from the synced folder.

Earlier gotchas (Drive's millisecond `modifiedTime`, `go-api` buffering
small uploads, Windows locking the running `.exe`) are recorded in
`CLAUDE.md` under "Gotchas".

## Next steps

In this order. Discuss the design with the owner before coding.

1. **First release (#13):** push a tag, install `cloudsync-setup.exe` on
   Windows, then turn on `cloudsync start` for the real root from a normal
   terminal (see #9).
2. **Check by eye on Windows,** since these can't be tested from a script:
   - the tray icon: its menu (every item), colours and tooltip (#14), and
     its notifications;
   - the shutdown screen ("CloudSync is preventing shutdown") with an
     upload in progress, and "shut down anyway";
   - that CloudSync starts after signing out and in again.
3. **Still untested on Windows:** `update` going back to the previous
   version when the new build doesn't start; behaviour when offline; the
   Recycle Bin refusing a file.
4. **Kept folders listed in every plan** (#12).
5. **Rename detection** (#6): move instead of upload + trash.
6. **`client_secret.json` permissions** (#4).
7. **Adaptive big-delete guard:** tune the threshold from the delete log
   once there is data. It must never loosen the guard silently.
8. **Optional, only if the owner asks:** a Drive change-feed check every
   ~10 min, on resume from sleep and on network return.

## Issues

| # | Issue | Status |
|---|---|---|
| 1 | Names that Windows can't store are skipped | fixed; verified on Windows |
| 2 | Names that differ only in case are skipped | fixed; verified on Windows |
| 3 | Fedora's Go is older than `go.mod` requires | fixed |
| 4 | `client_secret.json` can be world-readable after copying | open |
| 5 | Uploads slower than the connection allows | fixed |
| 6 | A moved file is re-uploaded and its old copy trashed | open (by design until rename detection) |
| 7 | An expired or revoked sign-in was retried like a network outage | fixed |
| 8 | A computer took another's shutdown list for its own and deleted it | fixed |
| 9 | On Windows, commands run by the Claude desktop app use a different state folder | worked around |
| 10 | The Windows watcher could miss a change made right after it started | fixed |
| 11 | Two tests fail on Windows because they create names Windows can't store | fixed |
| 12 | A folder kept because it holds unsynced files is listed for removal in every plan | open |

## 1. Names that Windows can't store are skipped

- **Found:** 2026-10-27, `cloudsync plan` on Fedora.
- **Symptom:** the plan lists them under "Cannot sync until renamed or
  fixed", e.g. `paper Title:Subtitle.pdf` (contains `:`) or a folder named
  `Hackathon ` (trailing space). A skipped folder takes everything inside
  it along. These items exist only on Linux and never reach Drive, the
  phone or Windows.
- **Cause:** `filter.CheckName` rejects any name that is invalid on Windows
  (`< > : " / \ | ? *`, control characters, trailing dot or space, reserved
  device names such as `CON` and `aux.txt`), and `localfs.Scan` and the
  Drive tree skip those entries. That was the original "skip and report"
  decision.
- **Decision (2026-10-27, agreed with the owner):** translate names on
  Windows only, the way Git for Windows, Cygwin and WSL do. Rejected
  alternatives: keep skipping (files silently miss Windows and the phone)
  and auto-renaming on Linux (CloudSync would rename the owner's files
  behind their back and break paths that scripts use).
  - Linux and Drive keep the true name (Drive accepts all of these, and the
    iPhone app shows it as is).
  - The Windows copy gets a stand-in: full-width lookalikes for forbidden
    characters (`:` → `：`, `?` → `？`, …), a visible marker for a trailing
    dot or space, and a suffix for reserved names (`CON.txt` → `CON＿.txt`).
  - The Windows state DB stores each mapping (Drive name ↔ local name), so
    decoding never guesses, even when a real name already contains `：`.
    Editing or renaming the stand-in on Windows uploads under the true name.
  - Plan and status output lists mapped names, so it never happens
    silently.
  - Linux stops skipping these names.
  - Pure mapping functions, with table tests for round-trips, clashes with
    real full-width names, reserved names and case clashes (#2).
- **Built (2026-10-27):** new package `internal/winname` (`Encode`, `Map`);
  `localfs.Scan` reports true paths through the Map; `filter.CheckName`
  now rejects only names no system can hold; `ListTree` keeps case-only
  siblings and skips only exact duplicates; `findChild` compares names
  exactly instead of trusting Drive's query; the planner matches paths
  exactly; the engine resolves local paths through the Map, picks all new
  stand-ins before transfers start, and saves each in `items.local_name`
  (state schema v2, migrates v1) once the file exists; `plan` lists the
  stand-ins.
- **Bugs caught on the way:** the planner's hash function joined the local
  folder with the true path, which on Windows would look for `a:b.pdf`
  instead of `a：b.pdf`; and `Assign` gave a second stand-in to a folder a
  scan had already found under its true name.
- **Verified:** table tests in `winname`, `localfs`, `state` (including a
  v1 → v2 upgrade) and `engine` (Windows mode on Linux: fresh sync, re-run
  in sync, new file in a stand-in folder). Live on Fedora against
  `CloudSync-Test/NamesFedora`: Drive kept `Trail `, `a:b.txt`,
  `what?.txt`, `etc.`, `con.txt`, and `Docs` and `docs` as two folders,
  exactly; a re-plan matched all 8 files as in sync.
- **Verified on Windows (2026-11-10):** live against a scratch Drive
  folder, `a:b.txt`, `con.txt` and `A.TXT` (next to `a.txt`) arrived as
  `a：b.txt`, `con＿.txt` and `A (case 2).TXT`; `plan` listed the stand-ins
  first.
- **Known edge:** if Drive has both `a:b` and a real `a：b` in one folder,
  whichever sorts first gets `a：b` on Windows and the other gets
  `(case 2)`, although it isn't a case clash.
- **Still skipped after the fix:** names no system can hold: Drive names
  containing `/`, `.` or `..`, two Drive items with exactly the same name
  in one folder, and Linux names that aren't valid UTF-8.

## 2. Names that differ only in case are skipped

- **Found:** 2026-10-27, while reviewing `CheckName` for #1.
- **Symptom:** `Notes.txt` and `notes.txt` in the same Linux folder (or
  Drive folder) are fine on Linux and Drive, but Windows' case-insensitive
  file system can hold only one. `localfs.Scan` and `ListTree` skip the
  second one ("differs only by letter case"), and the first-sync planner
  treats a case-only difference between local and Drive as a conflict.
- **Decision:** part of #1. Both names sync to Drive and Linux unchanged;
  on Windows, the second one gets a stand-in such as `notes (case 2).txt`,
  recorded in the state DB. The planner matches paths exactly.
- **Fixed** with #1, which lists what was built and how it was tested.
  Folders clash too: `Docs` and `docs` become `Docs` and `docs (case 2)`
  on Windows.

## 3. Fedora's Go is older than `go.mod` requires

- **Found:** 2026-10-27, first build on Fedora.
- **Symptom:** `go: go.mod requires go >= 1.27.3 (running go 1.26.10;
  GOTOOLCHAIN=local)`.
- **Cause:** `go.mod` was created with Go 1.27.3 on Windows. Fedora's
  `golang` package is 1.26.10 and sets `GOTOOLCHAIN=local`, which stops Go
  from fetching the newer toolchain itself.
- **Workaround:** prefix commands with `GOTOOLCHAIN=go1.27.3` (Go downloads
  the official, checksum-verified toolchain into its cache), or run
  `go env -w GOTOOLCHAIN=auto` once. All tests pass with 1.27.3 on Linux.
- **Fix (2026-11-05):** ran `go env -w GOTOOLCHAIN=auto` once. Fedora's Go
  now switches to the version `go.mod` asks for (1.27.3, also the latest
  release), so plain `go build` and `go test` work with no prefix. On a
  fresh Fedora machine, run the same command before the first build.

## 4. `client_secret.json` can be world-readable after copying

- **Found:** 2026-10-27, Fedora setup.
- **Symptom:** after a manual copy the file had mode `644`; it was fixed by
  hand with `chmod 600`.
- **Cause:** CloudSync never checks the file's permissions.
- **Possible fix:** on Unix, have `login` warn about (or tighten) a
  `client_secret.json` that group or others can read.

## 5. Uploads slower than the connection allows

- **Found:** 2026-10-28, after the Fedora first sync (1.6 GB up) felt slow.
- **Measured** with a throwaway benchmark against `CloudSync-Test`: large
  files are bound by the uplink (~18 Mbit/s for one transfer, ~21 for four
  at once; chunk size made no measurable difference). Small files take
  about 2 s per request (0.5 files/s one at a time, 2.5 with four, 3.65
  with sixteen, apparently near Google's per-user write limit).
- **Cause:** besides the uplink, which nothing can fix: folders were
  created one at a time before any transfer, each with a lookup and a
  create (two ~2 s requests); only four transfers ran at once, too few to
  hide per-request latency; and files ran in plan order, so a big file
  could end up running alone at the end.
- **Fix (2026-11-03):**
  - `gdrive.CreateFolder`: creates without the lookup, for a parent made
    in the same run (it must be empty). A retry still looks first, so a
    create whose reply was lost isn't made twice.
  - The engine creates folders a depth at a time, each depth in parallel,
    using `CreateFolder` under folders it just made.
  - 8 parallel transfers by default instead of 4.
  - Largest files first.
- **Verified:** tests with the fake Drive (lookups skipped only under new
  folders, no duplicates, largest-first order) and with a fake Drive HTTP
  server (a lost create reply is retried without a duplicate); race
  detector clean. Live against `CloudSync-Test`, the same tree of 52
  folders and 73 files (72 × 4 KB, 1 × 16 MB) took **122 s with the old
  build and 33 s with the new one**; a re-plan found all 73 in sync and no
  duplicate folders.

## 6. A moved file is re-uploaded and its old copy trashed

- **Found:** 2026-11-10, first dry run of a later sync on the real root.
- **Symptom:** after folders are reorganized locally, the plan shows each
  moved file twice: an upload at the new path and a trash in Drive at the
  old one. On the real root about 30 of 37 Drive deletes were moves.
- **Cause:** the reconciler compares paths only; it has no idea a file at a
  new path is the same file as one gone from an old path.
- **Impact:** correct end result (Drive matches the folder; old copies sit in
  Drive's trash for 30 days), but moved files are uploaded again, and a big
  reorganization trips the big-delete guard. The other OS then downloads the
  new paths and trashes the old copies locally.
- **Possible fix:** rename detection, already planned as a Phase 3
  follow-up: match a deleted path with a new one of the same size and MD5
  (locally) or the same Drive file ID (remotely), and move instead of
  upload + trash (`files.update` with new parents in Drive, a rename here).
- **Also in live mode (2026-11-10):** `cloudsync run` uploads the moved
  files and trashes the old copies a few seconds after a folder is renamed.
  Renaming a folder of more than 50 files (or 10% of the root) trips the
  guard, which then waits for `cloudsync deletes approve`.

## 7. An expired or revoked sign-in was retried like a network outage

- **Found:** 2026-11-10, while building `cloudsync run`'s error handling.
- **Symptom (by code review):** when Google refuses the stored refresh
  token, each Drive call is retried 6 times over about 30 s before failing,
  and a long-running sync would keep retrying a sign-in that can't work.
- **Cause:** the oauth2 transport returns the refusal
  (`*oauth2.RetrieveError`) inside a `*url.Error`, and `*url.Error` is a
  `net.Error`, which `gdrive.Retry` treats as a network problem.
- **Fix:** `gdrive.SignInRejected` recognizes the refusal (any token-endpoint
  answer below 500) and `Retry` gives up on it at once; `cloudsync run` stops
  and says to run `cloudsync login`. `gdrive.Temporary` exposes the
  remaining "retry later" cases. Tested with wrapped errors in
  `retry_test.go`.

## 8. A computer took another's shutdown list for its own and deleted it

- **Found:** 2026-11-10, real shutdown test with two scratch computers on
  one Fedora machine.
- **Symptom:** computer A shut down with a file not uploaded and saved its
  unsynced-files list to Drive, but B gave no warning at its next start,
  and the list was gone afterwards.
- **Cause:** the list was named by OS (`pending-linux-<root>.json`). B is
  Linux too, so it took A's list for its own: it skipped it when looking
  for other computers' lists and, after a clean catch-up, removed it. The
  owner's Fedora + Windows pair wouldn't hit this, but two computers with
  the same OS would.
- **Fix:** each computer gets a random ID, kept in its state DB
  (`computer_id`), and lists are named `pending-<os>-<id>-<root>.json`. A
  reader looks at every `pending-*.json` in the `.cloudsync` folder, skips
  its own, and checks the root named inside the list. Re-tested live: B
  warned, A's next start uploaded the file and removed the list, and B
  then gave no warning.

## 9. On Windows, commands run by the Claude desktop app use a different state folder

- **Found:** 2026-11-10, Windows catch-up.
- **Symptom:** `cloudsync plan` run by Claude showed a normal later sync,
  but `cloudsync sync` in a terminal offered a *first* sync that would have
  uploaded the old-layout files again. It was cancelled at the prompt.
- **Cause:** the Claude desktop app is a packaged Windows app. For commands
  Claude runs through its own tools, Windows redirects writes under
  `%LocalAppData%` to
  `%LocalAppData%\Packages\<Claude package>\LocalCache\Local\`. The first
  Windows sync had been run that way, so the state DB existed only in the
  redirected folder. A terminal, and anything started at login, sees the
  real `%LocalAppData%\cloudsync`, which was empty. `%AppData%` (the
  config) and the keyring are not affected.
- **Worked around:** copied `state.db` into the real folder from a
  terminal; `plan` there then matched, and the catch-up ran from the
  terminal.
- **Rule:** on Windows, run every `cloudsync` command that touches state
  from a real terminal, never through the Claude app's own tools.
- **Possible fix:** none needed in CloudSync; a warning when the state
  folder is redirected would be possible but is not planned.

## 10. The Windows watcher could miss a change made right after it started

- **Found:** 2026-11-10, `TestWatch` failing once in a full test run on
  Windows.
- **Cause:** `Watch` opened the folder and returned, and a goroutine made
  the first `ReadDirectoryChangesW` call later. Windows records changes
  only from that first call, so a change in between was lost until the
  hourly rescan.
- **Fix:** the handle is opened for overlapped I/O and the first read is
  issued before `Watch` returns; the goroutine collects results and
  listens again. Each reader closes its own handle after its cancelled
  read returns.
- **Verified:** `TestWatch` passed 30 runs in a row; live under
  `cloudsync run`, a new file, an edit, a folder rename and a delete were
  each picked up within seconds.

## 11. Two tests fail on Windows because they create names Windows can't store

- **Found:** 2026-11-10, first `go test ./...` on Windows since #1.
- **Cause:** `TestScanKeepsWindowsOnlyNames` and
  `TestUploadsNamesWindowsCannotStore` write files such as `Paper:1.pdf`
  to check what Linux does with them.
- **Fix:** both skip on Windows. The Windows behaviour is covered by
  `TestWindowsStandIns` and `TestScanTranslatesStandIns`.

## 12. A folder kept because it holds unsynced files is listed for removal in every plan

- **Found:** 2026-11-10, after approving the Windows catch-up's held
  deletes.
- **Symptom:** a folder that was deleted on the other side but still holds
  files CloudSync doesn't sync here (a git repo, ignored files) is rightly
  kept (`kept  Old/ (deleted on the other side, but still holds files
  CloudSync doesn't sync here)`). But every later `plan` and `sync`
  lists it again under "Trash here", and keeps it again.
- **Cause:** `engine.removeFolders` leaves the folder's record in the
  state DB when it keeps the folder, so the reconciler sees "in the base,
  gone from Drive, still here" each time and plans the removal again.
- **Impact:** nothing is lost, but the plan is misleading, and the folders
  count as changes on every run.
- **Possible fix:** forget the folder's record when it is kept (it is then
  a local-only folder with nothing to sync), or have `reconcile.Skips`
  cover folders whose only contents are unsynced. Decide with the owner.

## 13. No installer: the program was only copied by `cloudsync start`, and wasn't on the Windows PATH

- **Found:** 2026-11-11, while turning on `cloudsync start` on Windows.
- **Symptom:** the program sits in `%LocalAppData%\Programs\CloudSync`,
  which isn't on the PATH, so every command had to be typed as
  `& "$env:LOCALAPPDATA\Programs\CloudSync\cloudsync.exe" stop`. Installing
  was also hidden inside `start`, and there was nothing to release on
  GitHub.
- **Solved (written, not yet tried):** a release workflow builds
  `cloudsync-setup.exe` (Inno Setup, adds the PATH entry) and a Linux
  tarball with `install.sh` (see `DECISIONS.md`, "Running in the
  background"). `start` stays as it was, with no PATH change.
- **To check:** push a first tag, then run the setup on Windows: PATH works
  in a new terminal, updating over a running CloudSync stops and restarts
  it, and uninstalling removes the PATH entry. The Inno script has never
  been compiled; CI is its first build.

## 14. The Windows tray icon had no menu and vanished with the sync

- **Found:** 2026-11-12.
- **Symptom:** the taskbar icon only carried balloons. It lived inside `cloudsync run`, so stopping the sync removed it, and there was no way to see
  the status, pause, or start the sync from it.
- **Solved (written and smoke-tested, not yet tried by eye):** the icon is now
  its own process, `cloudsync tray` (see `DECISIONS.md`), with a menu and a
  colour and tooltip that follow the status. The sync gained Sync now / Pause /
  Resume, driven by named events. `start` brings back a missing tray or sync;
  `stop` and `update` close both.
- **Checked:** unit tests for the menu, status mapping, warning diff and
  lock; the tray starts, holds its lock, a second copy exits, and `stop`
  closes it. **To check by eye:** the menu items, icon colours (the icons are
  drawn at runtime with `CreateIconFromResourceEx`; if one shows as the
  default application icon, that call failed), balloons from the tray, Exit
  then `start`, and the Start Menu shortcut after a release build.
