# Issue log

Problems found while building and running CloudSync, and how each was
solved. Add an entry when an issue is found and update it when it is
solved; don't delete solved entries. This repo is public, so use generic
examples, never real file names or paths from the synced folder.

Earlier gotchas (Drive's millisecond `modifiedTime`, `go-api` buffering
small uploads, Windows locking the running `.exe`) are recorded in
`CLAUDE.md` under "Gotchas found so far".

| # | Issue | Status |
|---|---|---|
| 1 | Names that Windows can't store are skipped | fixed; Windows side to verify |
| 2 | Names that differ only in case are skipped | fixed; Windows side to verify |
| 3 | Fedora's Go is older than `go.mod` requires | fixed |
| 4 | `client_secret.json` can be world-readable after copying | open |
| 5 | Uploads slower than the connection allows | fixed |
| 6 | A moved file is re-uploaded and its old copy trashed | open (by design until rename detection) |

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
- **To verify on Windows:** rebuild there (the old binary refuses the v2
  state DB), then check the stand-ins appear as listed by `plan`. Windows
  only downloads again once Phase 3 exists.
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
