# CloudSync

Keeps a folder identical across computers by syncing it through Google
Drive. It was built for a dual-boot laptop (Windows and Fedora), where only
one OS runs at a time, and works the same for any set of Windows and Linux
computers. Synced files are also reachable from a phone through the Google
Drive app.

**Status:** Phase 3 nearly done. Sign-in, setup, dry run, the first sync on
each computer, two-way sync (edits and deletes both ways, with a big-delete
guard), and a background sync that starts at login (`cloudsync start`),
catches up with Drive, uploads local changes as they happen, finishes its
uploads at shutdown and notifies you only when something needs you. The
Windows side of the background sync (Startup entry, tray icon, shutdown
screen) is built but not yet tested on Windows.

## Set it up yourself

You need about 20 minutes, a Google account, and Go 1.26 or newer.

### 1. Build

```bash
git clone https://github.com/USER1043/cloudsync.git
cd cloudsync
go build -o bin/ ./cmd/cloudsync
```

### 2. Create your own Google sign-in client

Google only lets an app with full Drive access serve the public after a paid
security review, so each user creates their own (free) OAuth client. It only
ever talks to your own Drive.

1. In [Google Cloud Console](https://console.cloud.google.com/), create a
   project, then enable the **Google Drive API** under *APIs & Services →
   Library*.
2. Under *Google Auth Platform*, set up the consent screen with user type
   **External**, and add your Google account as a test user.
3. Choose how long sign-ins last:
   - **Publish the app** (*Audience → Publish app*) so you stay signed in.
     Google asks for a homepage and privacy policy URL first. Fork this repo,
     enable GitHub Pages from `main` / `/docs`, and use
     `https://<you>.github.io/cloudsync/` and `.../privacy.html`. Add
     `<you>.github.io` under *Authorised domains*. Don't upload a logo; that
     triggers Google's review.
   - Or stay in **Testing**, which needs no URLs, but Google signs you out
     every 7 days.
4. Under *Clients*, create a client of type **Desktop app** and download its
   JSON. Save it as:
   - Windows: `%AppData%\cloudsync\client_secret.json`
   - Linux: `~/.config/cloudsync/client_secret.json`

   Keep it out of git; the repo's `.gitignore` already does.

### 3. Sign in, choose a folder, sync

```bash
cloudsync login     # opens Google sign-in; "unverified app" is expected: Advanced → Go to CloudSync
cloudsync init      # pick the folder to sync and its Drive folder
cloudsync plan      # dry run: shows exactly what will happen
cloudsync sync      # first sync
cloudsync start     # from now on, sync in the background, starting at every login
```

`init` suggests `~/CloudSync` and `CloudSync/<folder name>` in Drive; both
can be anything. Run `login` before `init` so `init` can show folders that
already exist in Drive.

`start` copies the program to a fixed place (`~/.local/bin/cloudsync` on
Linux, `%LocalAppData%\Programs\CloudSync` on Windows), sets it to start at
login (a systemd user service on Linux; a Startup entry on Windows, which
Task Manager's *Startup apps* lists) and starts it now. You only run it once
per computer. After building a newer version, run `cloudsync update` from
the new build; `cloudsync stop` turns the background sync off.

### 4. On the second computer (or the other OS)

Repeat steps 1 to 3 there, using the same `client_secret.json`. In
`cloudsync init`, pick the Drive folder the first computer created. Its first
sync merges with whatever is already in the local folder (see below).

## Choosing what syncs

Some things are **always skipped**: git repositories (any folder containing
`.git`, since code belongs in git), `node_modules/`, `.venv/`, `venv/`,
`__pycache__/`, `.gradle/`, Eclipse's `.metadata/`, Office lock files `~$*`,
`*.tmp`, `*.swp`, `Thumbs.db`, `desktop.ini`, `.DS_Store`, symlinks, and Google
Docs/Sheets/Slides, which have no file to download.

Add your own rules in two places, written like `.gitignore`:

- `ignore = [...]` in `config.toml`, for rules across the whole folder;
- a `.cloudsyncignore` file in any folder, for rules that apply to that folder
  and below. It syncs too, so the rules apply on every computer.

| Rule | Skips |
|---|---|
| `*.iso` | every `.iso` file, at any depth |
| `Recordings/` | every folder named `Recordings` (trailing `/`: folders only) |
| `/College/Old/` | just that folder (a `/` at the start or middle anchors the rule to the folder it belongs to) |
| `Sem 6/Lab/*.class` | `.class` files directly in that folder |
| `**/drafts` | `drafts` at any depth |
| `docs/**` | everything inside `docs` |
| `!keep.iso` | nothing: it re-includes what an earlier rule skipped |

Rules ignore letter case, `\` works like `/`, and the last matching rule wins.
`max_file_size = "500MB"` in `config.toml` (or `init --max-file-size`) also
skips anything larger. `cloudsync plan` lists everything that is skipped, so
check it after changing rules. A rule never deletes anything: a file that is
already in Drive stays there and simply stops syncing.

## Commands

| Command | What it does |
|---|---|
| `cloudsync login` | Signs in through the browser and stores the token in Windows Credential Manager or the Linux keyring (KWallet / GNOME Keyring) |
| `cloudsync whoami` | Shows the signed-in account and Drive storage |
| `cloudsync init` | Chooses the folder to sync and its Drive folder, and writes `config.toml`. Flags: `--local`, `--remote`, `--max-file-size`, `--force` |
| `cloudsync plan [--all]` | Dry run: what the next sync would upload, download, trash, skip or flag. Changes nothing |
| `cloudsync sync [--yes]` | The first time: uploads local-only files, downloads Drive-only files, and keeps both copies of files that differ; never deletes or overwrites. After that: a two-way sync of edits, new files and deletes (to the Trash / Recycle Bin or Drive's trash), keeping both copies when a file was edited on both sides. Safe to re-run after an interruption |
| `cloudsync deletes [approve\|cancel]` | Shows the deletes the big-delete guard is holding (more than 50 files or 10% of the folder at once); `approve` lets them go ahead, `cancel` puts the files back |
| `cloudsync start` | Keeps CloudSync syncing in the background from now on: installs the program (the first time), starts it at every login, and starts it now. Needs the first sync done |
| `cloudsync stop` | Stops the background sync (after its last uploads) and doesn't start it at login any more. Files, settings and sign-in are kept |
| `cloudsync update` | Run from a newer build: replaces the installed program, restarts the background sync on it, and goes back to the previous version if the new one doesn't start |
| `cloudsync run` | The background sync itself, in the terminal: catches up with Drive at start (as `sync --yes`, holding big deletes for you), then uploads local changes a few seconds after they settle. Ctrl-C uploads the last changes and stops. `start` runs this for you |
| `cloudsync status` | Whether the background sync is going, when Drive was last checked, what's waiting to upload, held deletes, and recent warnings |
| `cloudsync logout` | Revokes the token with Google and removes it |

### When Drive is checked (a deliberate trade-off)

`cloudsync run` sends your changes **up** to Drive within seconds, but it
**checks Drive for changes made elsewhere only when it starts** (normally at
boot or login), or when you run `cloudsync sync` by hand.

- **What this means:** an edit made on your phone, or on another computer,
  while this computer is running shows up here at the next start, not
  straight away. Closing the lid to sleep isn't a start: a laptop that sleeps
  for days without being shut down picks up those changes only at its next
  boot or `cloudsync sync`.
- **Why:** CloudSync was built for a dual-boot laptop that is shut down
  often, where the phone is mostly for reading files, and where only one OS
  runs at a time. There, checking once per boot is enough, and Drive is never
  queried while you work.
- **Nothing is lost either way:** if you edit a file here that was also
  changed in Drive since the last check, CloudSync notices when it uploads
  and keeps both copies (`name (conflict ...)`) instead of overwriting.
- **If that doesn't fit how you work** (you edit on the phone a lot, or you
  mostly sleep the computer instead of shutting it down), run
  `cloudsync sync` when you want Drive's changes now, or restart
  `cloudsync run`. Checking Drive while running (every few minutes, on wake
  from sleep and when the network returns) would be a small change; it isn't
  built because its author doesn't need it.

### Notifications

`cloudsync run` stays quiet while things work. It shows a desktop
notification (KDE/GNOME on Linux; a CloudSync icon in the Windows taskbar)
only for:

- another computer that shut down before uploading everything (which files
  are out of date here);
- deletes held by the big-delete guard, waiting for `cloudsync deletes`;
- a file edited in two places (both versions are kept);
- files that keep failing for a reason on this computer (permissions, a
  file open in another program, a full disk, a path too long for Windows);
- Drive full, or the sign-in expired (`cloudsync login`), or the Drive
  folder missing; CloudSync pauses or stops until you fix it;
- no connection to Google Drive for 30 minutes, or Google's servers failing
  for 10 minutes (with a link to Google's status page; not your setup).

The same message isn't repeated within an hour, and warnings disappear once
their problem is gone. `cloudsync status` lists recent ones.

### At shutdown

- **Fedora (and other Linux):** CloudSync never blocks a shutdown; KDE
  would give no message, its Shut Down button would just seem broken. The
  moment shutdown starts, CloudSync saves a small list in Drive of the
  changes it hasn't uploaded yet, then keeps uploading for up to about 40
  seconds (systemd's stop time), less if you're offline. Press Esc on the
  boot logo to see "A stop job is running for CloudSync".
- **Windows:** while uploads are pending and Drive can be reached, Windows
  shows "CloudSync is preventing shutdown: Uploading N files to Google
  Drive", with *Shut down anyway*. Offline, it doesn't hold the shutdown.
- **Sleep is never blocked or delayed.**
- **Nothing is lost:** whatever didn't make it uploads at that computer's
  next start. Meanwhile, the other computers read the list at their next
  start and warn which files are missing or out of date there. The list
  sits in a `.cloudsync` folder next to the synced folder in Drive and is
  removed once everything is uploaded.

### How the first sync works

- Files on only one side are copied to the other. Identical files are left
  alone (compared by checksum, not date).
- If the same file differs on both sides, both copies are kept: Drive's version
  keeps the name and the local one becomes
  `name (conflict Windows 2026-10-27 1432).ext`, which is uploaded too.
- Every transfer is verified against Drive's MD5 checksum. Downloads go to a
  `.name.cloudsync-partial` file and are moved into place only once verified.
  Modified times are kept in both directions.
- Names Windows can't store (`a:b.txt`, `notes.`, `CON`, a folder ending in
  a space, two names differing only by case) keep their real name in Drive
  and on Linux. On Windows they get a lookalike stand-in (`a：b.txt`,
  `notes․`, `CON＿`, `folder␣`, `notes (case 2).txt`); CloudSync remembers
  which is which, so edits and new files map back to the real name. `plan`
  lists every stand-in.

## Where things are kept

| What | Windows | Linux |
|---|---|---|
| `config.toml`, `client_secret.json` | `%AppData%\cloudsync` | `~/.config/cloudsync` |
| Sign-in token | Credential Manager | Secret Service keyring |
| Sync records (`state.db`), `status.json` | `%LocalAppData%\cloudsync` | `~/.local/state/cloudsync` |
| Installed program (`cloudsync start`) | `%LocalAppData%\Programs\CloudSync` | `~/.local/bin/cloudsync` |
| Background sync's log | `%LocalAppData%\cloudsync\cloudsync.log` | `journalctl --user -u cloudsync` |
| Started at login by | Startup entry `CloudSync` (Task Manager → Startup apps) | `~/.config/systemd/user/cloudsync.service` |

To try CloudSync against a scratch folder without touching your real setup,
point `CLOUDSYNC_CONFIG` at another `config.toml` and `CLOUDSYNC_STATE_DIR`
at another folder. `cloudsync start` then installs a separate copy into that
folder with its own service name, so it never replaces the real one.

## License

[MIT](LICENSE)
