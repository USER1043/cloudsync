# CloudSync

Keeps a folder identical across computers by syncing it through Google
Drive. It was built for a dual-boot laptop (Windows and Fedora), where only
one OS runs at a time, and works the same for any set of Windows and Linux
computers. Synced files are also reachable from a phone through the Google
Drive app.

**Status:** Phase 2. Sign-in, setup, dry run and the one-time first sync on
each computer. Continuous two-way sync (a background service that also syncs
deletes and handles shutdown) is Phase 3.

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
```

`init` suggests `~/CloudSync` and `CloudSync/<folder name>` in Drive; both
can be anything. Run `login` before `init` so `init` can show folders that
already exist in Drive.

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
| `cloudsync plan [--all]` | Dry run: what the first sync would upload, download, skip or flag. Changes nothing |
| `cloudsync sync [--yes]` | First sync: uploads local-only files, downloads Drive-only files, and keeps both copies of files that differ. Never deletes or overwrites; safe to re-run after an interruption |
| `cloudsync logout` | Revokes the token with Google and removes it |

### How the first sync works

- Files on only one side are copied to the other. Identical files are left
  alone (compared by checksum, not date).
- If the same file differs on both sides, both copies are kept: Drive's version
  keeps the name and the local one becomes
  `name (conflict Windows 2026-10-27 1432).ext`, which is uploaded too.
- Every transfer is verified against Drive's MD5 checksum. Downloads go to a
  `.name.cloudsync-partial` file and are moved into place only once verified.
  Modified times are kept in both directions.
- Names that can't exist on both Windows and Linux (`a:b.txt`, `CON`, two
  names differing only by case) are skipped and listed by `plan`.

## Where things are kept

| What | Windows | Linux |
|---|---|---|
| `config.toml`, `client_secret.json` | `%AppData%\cloudsync` | `~/.config/cloudsync` |
| Sign-in token | Credential Manager | Secret Service keyring |
| Sync records (`state.db`) | `%LocalAppData%\cloudsync` | `~/.local/state/cloudsync` |

To try CloudSync against a scratch folder without touching your real setup,
point `CLOUDSYNC_CONFIG` at another `config.toml` and `CLOUDSYNC_STATE_DIR`
at another folder.

## License

[MIT](LICENSE)
