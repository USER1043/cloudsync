# Decisions

Design decisions agreed with the owner. They are settled: don't re-ask,
and change one only after discussing it. Problems and open work are in
`ISSUES.md`; how the code is laid out is in `docs/ARCHITECTURE.md`.

## Backend

- **Google Drive.** The owner's 1 TB OneDrive is a college account whose
  admins block app registration and third-party consent, and it expires
  after graduation. iCloud has no public Drive API. Google Drive has an
  official API with a change feed.
- **Design for the free tier: 15 GB, shared with Gmail and Photos,**
  including quota checks before uploads.
- **Each user brings their own Google OAuth client** (Desktop app type). An
  app with full `drive` scope can't be offered publicly without Google's
  paid review. The owner's app is published (status "In production",
  unverified), with homepage and privacy policy served from `docs/` on
  GitHub Pages.
- **Phone:** the Google Drive iOS app. Native Google Docs/Sheets/Slides are
  skipped (there is no file behind them).

## What syncs

- **Mirror semantics.** Creating, editing, renaming or deleting a file
  syncs; reading does not. Change detection uses size, mtime and MD5, never
  access events.
- **Files only:** no symlinks, no permissions.
- **Git repos are skipped entirely** (any folder with `.git`); code goes
  through GitHub. Dependency and cache folders are ignored by default.
- **Names Windows can't store are translated on Windows only.** Drive and
  Linux keep the true name; Windows stores a lookalike stand-in (`a:b` →
  `a：b`, trailing space → `␣`, `con.txt` → `con＿.txt`, a case clash →
  `notes (case 2).txt`), recorded per item in the state DB. Plans, the
  state DB and Drive always use true paths; only `abs()` on Windows sees
  stand-ins. Names no system can hold (`/`, `.`, `..`, exact Drive
  duplicates, invalid UTF-8) are skipped and reported.

## First sync

- **The first sync on each computer is a union merge:** copy files that
  exist on one side only, leave identical files alone (by checksum), and
  **keep both** copies when content differs
  (`name (conflict Linux 2026-10-27 1432).ext`). It never deletes anything.
- **Fedora's folder layout is the final one.**

## Deletes

- **Deletes propagate,** but always to the OS trash or Drive's trash, never
  a hard delete.
- **Big-delete guard:** hold all deletes of a run, and ask, when it would
  trash **more than 50 files or more than 10% of the root's files**. The
  numbers are a starting guess: the guard is a pluggable policy, and every
  run's delete count, the root's size and the owner's answer are logged in
  the state DB, so the threshold can be tuned from real use. An adaptive
  policy must be designed with the owner and must never loosen the guard
  silently.
- **An edit beats a delete.** A conflict keeps both copies.

## When syncing happens

- **Drive is checked once per boot** (when `cloudsync run` starts), and
  when the owner runs `cloudsync sync` by hand. There is no periodic Drive
  poll: phone edits arrive at the next boot. The owner rarely edits on the
  phone and usually shuts the laptop down, so a laptop that only sleeps not
  re-checking is accepted. The README documents the trade-off ("When Drive
  is checked"). If wanted later: a change-feed check every ~10 min, on
  resume from sleep and on network return.
- **Local edits upload live:** a watcher signal, then about 3 s of quiet
  (30 s at most), then a full local rescan compared with the state DB;
  files changed in the last 2 s wait for the next pass. Also an hourly
  rescan and one on watcher overflow.

## Shutdown

- The moment shutdown starts, upload a small *unsynced-files list* so the
  other OS warns at its next start which files are stale; then keep
  uploading until the system stops CloudSync. Unsent edits stay on disk and
  upload at that OS's next start (nothing to queue: a local change is
  whatever differs from the state DB).
- **Fedora never blocks shutdown.** With a logind "block" inhibitor held,
  KDE's Shut Down silently does nothing (no message, no "shut down
  anyway"). Instead a logind **delay** inhibitor (max 5 s) gives time to
  upload the list, then the SIGTERM flush gets the user unit's stop time
  (about 40 s; skipped at once when offline).
- **Windows blocks** ("CloudSync is preventing shutdown: uploading N
  files"), **but not when offline**: waiting can't help.
- **Sleep is never blocked** (a laptop in a bag must sleep).
- **Unsynced-files list:**
  `<root's parent>/.cloudsync/pending-<os>-<computer id>-<root>.json` in
  Drive, outside the root folder. The computer ID is random and kept in the
  state DB, so two computers with the same OS don't mix up their lists.
  Each entry records path, size and modified time (or "deleted"), so the
  reader warns only about files whose Drive copy still differs. Not sent
  when nothing is pending or when offline. The writer deletes it after its
  next clean catch-up or a complete flush.

## Running in the background

- **Commands:** `start` installs the program once, registers it to start at
  login and starts it; it is idempotent and never replaces the installed
  copy. `stop` stops after the flush and removes the login entry, keeping
  the program file. `update` is run from the new build: it refuses if not
  installed or if run from the installed copy, says "already up to date" if
  identical, and otherwise stops, keeps `.previous`, starts and checks it
  runs, else restores the previous one. Versions come from git.
- **Linux:** `~/.local/bin/cloudsync` and a systemd **user** unit tied to
  `graphical-session.target`, `Restart=on-failure`; exit status 3 (needs
  the owner) is not restarted.
- **Windows:** a per-user **Startup entry** (HKCU Run), not Task Scheduler
  (no admin needed) and not a Session-0 service (it can't reach the user's
  keyring or show notifications). It runs a windowless copy of the program,
  logs to a file, and stops through a named event.
- **Windows tray is its own process** (`cloudsync tray`, the windowless copy
  again), and it is what the Startup entry runs. It owns the taskbar icon, so
  the icon survives stopping the sync and can start it again; the sync
  (`cloudsync run`) is a child it launches. It reads `status.json` and the
  run lock to show state, and asks the sync to sync now, pause or resume
  through named events (like the stop event). Menu: status, Sync now,
  Pause/Resume, Stop/Start sync, Open folder, Open log, Start at login, Exit.
  *Pause* keeps the process but holds uploads (a shutdown or stop still
  uploads); *Stop sync* ends the process after its flush but keeps the tray
  and login entry; *Exit* stops the sync, then the tray, but keeps start at
  login. `cloudsync start` brings back whichever is missing; `stop` and
  `update` close both, since both hold the program file. A second tray
  exits at once (a lock file). At login the tray starts the sync, so Stop
  sync lasts until the next login.
- **Scratch configs** get their own unit or entry name and program copy,
  so testing never touches the real setup.
- **Installing is separate from `start`.** Releases are built by GitHub
  Actions when a `v*` tag is pushed: a Windows installer
  (`cloudsync-setup.exe`, Inno Setup, per-user, no admin) and a Linux
  tarball with `install.sh`. The installer puts the program in the same
  place `start` uses, adds it to the PATH (Windows; `~/.local/bin` is
  already on it on Linux), and offers to run `start` on its last page,
  unchecked by default because `start` needs the sign-in and first sync.
  `start` still installs the program itself when it isn't installed (a
  build from source), but never touches the PATH. The Windows build ships
  the windowless background copy as a second build (`-H=windowsgui`).
  The installers are not code-signed, so Windows SmartScreen warns on first
  run.

## Notifications and errors

- **Notify only for what needs the owner or that they'd want to know:**
  stale files from another computer, held deletes, conflicts, files failing
  for a local reason, Drive full, sign-in, root folder, offline 30+ min,
  Google failing 10+ min. Never for routine syncs, retries or rate limits.
  The same message at most once an hour; a resolved problem takes its
  notification down.
- **Linux:** freedesktop notifications on the session bus, from `run`.
  **Windows:** the tray process shows balloons for the warnings `run` keeps
  in `status.json` (it has no icon of its own), and takes them down when
  the warning is gone.
- **Errors explain themselves,** each with its cause and fix. Offline:
  changes wait and retry when the network is back. Sign-in expired or
  revoked: "run `cloudsync login`". Drive full: pause uploads and say which
  services share the 15 GB. Root folder trashed or missing in Drive: stop
  and say so, never re-upload blindly. Rate limits: silent backoff.
  Repeated Google server errors: say "Google Drive is having problems, not
  your setup", link Google's status page, and retry later.
- If the Secret Service or keyring is unavailable, report it; never add a
  plaintext token fallback.
