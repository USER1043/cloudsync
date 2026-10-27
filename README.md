# CloudSync

A personal background service that keeps one folder identical on a dual-boot
laptop (Windows and Fedora) by syncing it through Google Drive. The files are
also reachable from a phone through the Google Drive app.

**Status:** Phase 2. Sign-in, dry run, and the one-time first sync on each
OS. Continuous two-way sync (background service, deletes, shutdown
handling) is Phase 3.

## Setup

1. Create an OAuth **Desktop app** client in Google Cloud Console with the
   Google Drive API enabled, and save its JSON as
   - Windows: `%AppData%\cloudsync\client_secret.json`
   - Linux: `~/.config/cloudsync/client_secret.json`
2. Optional: copy [`config.example.toml`](config.example.toml) to
   `config.toml` in the same folder to change the synced folders.
3. Build (Go 1.26+):

   ```bash
   go build -o bin/ ./cmd/cloudsync
   ```

## Commands

| Command | What it does |
|---|---|
| `cloudsync login` | Opens Google sign-in in the browser; stores the token in Windows Credential Manager or KWallet |
| `cloudsync whoami` | Shows the signed-in account and Drive storage |
| `cloudsync plan [--all]` | Dry run: what the first sync would upload, download or flag. Changes nothing |
| `cloudsync sync [--yes]` | First sync: uploads local-only files, downloads Drive-only files, and keeps both copies of files that differ. Never deletes or overwrites; safe to re-run after an interruption |
| `cloudsync logout` | Revokes the token with Google and removes it |

### First sync details

- Every transfer is verified against Drive's MD5 checksum. Downloads go to a
  `.name.cloudsync-partial` file and are moved into place only once verified.
- Files keep their modified time in both directions.
- A conflict renames the local file to `name (conflict Windows 2026-10-27 1432).ext`,
  uploads it, and downloads Drive's version under the original name.
- What was synced is recorded in `state.db` under `%LocalAppData%\cloudsync`
  (Windows) or `~/.local/state/cloudsync` (Linux) for the continuous sync.

### Testing against a scratch folder

`CLOUDSYNC_CONFIG` points at a different `config.toml` and
`CLOUDSYNC_STATE_DIR` at a different state folder, so a test root can be
synced without touching the real one.

## What is never synced

- Folders containing `.git`. Code goes through GitHub.
- Dependency folders and temp files: `node_modules/`, `.venv/`, `venv/`,
  `__pycache__/`, `.gradle/`, Eclipse's `.metadata/`, `~$*`, `*.tmp`, `*.swp`, `Thumbs.db`,
  `desktop.ini`, `.DS_Store`.
- Symlinks and special files, and names that can't exist on both Windows and
  Linux. `plan` lists these so they can be renamed.
- Google Docs, Sheets and Slides, which have no file to download.

## License

[MIT](LICENSE)
