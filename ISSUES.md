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
| 3 | Fedora's Go is older than `go.mod` requires | worked around |
| 4 | `client_secret.json` can be world-readable after copying | open |

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
- **Solved when:** Fedora ships Go ≥ 1.27.3, or we decide to lower the `go`
  line in `go.mod`.

## 4. `client_secret.json` can be world-readable after copying

- **Found:** 2026-10-27, Fedora setup.
- **Symptom:** after a manual copy the file had mode `644`; it was fixed by
  hand with `chmod 600`.
- **Cause:** CloudSync never checks the file's permissions.
- **Possible fix:** on Unix, have `login` warn about (or tighten) a
  `client_secret.json` that group or others can read.
