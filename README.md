# CloudSync

A personal background service that keeps one folder identical on a dual-boot
laptop (Windows and Fedora) by syncing it through Google Drive. The files are
also reachable from a phone through the Google Drive app.

**Status:** Phase 1. Sign-in and a read-only dry run (`cloudsync plan`).
Nothing is uploaded yet.

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
| `cloudsync logout` | Revokes the token with Google and removes it |

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
