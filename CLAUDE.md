# CloudSync: context for Claude

What Claude needs to know to work on this code. Keep it short and current:
no history, no dated notes.

| For | Read |
|---|---|
| Settled design decisions (don't re-ask) | `DECISIONS.md` |
| Package map and how a sync works | `docs/ARCHITECTURE.md` |
| Next steps, and every problem found and how it was solved | `ISSUES.md` |
| Setup and commands, for users | `README.md` |

## Goal

One folder that is identical on both OSes of the owner's dual-boot laptop
(Windows 11 + Fedora KDE), synced through Google Drive, also reachable from
the owner's iPhone through the Google Drive app.

| | Windows | Fedora | Google Drive |
|---|---|---|---|
| Synced folder | `C:\MyFiles` | `~/My_Files` | `My Drive/CloudSync/MyFiles` |
| Config | `%AppData%\cloudsync\config.toml` | `~/.config/cloudsync/config.toml` | |
| State DB | `%LocalAppData%\cloudsync\state.db` | `~/.local/state/cloudsync/state.db` | |
| Installed program | `%LocalAppData%\Programs\CloudSync` | `~/.local/bin/cloudsync` | |

Only one OS runs at a time, so "sync" means: catch up at login, stay live
while running, and make sure changes are uploaded before shutdown.

## Current state

Replace this section when it changes; don't add to it.

- All phases are built: first sync, two-way `plan`/`sync`, the big-delete
  guard, `cloudsync run`, shutdown handling, notifications, `status`, and
  `start`/`stop`/`update`.
- **Fedora:** in sync and running in the background from login.
- **Windows:** in sync with Drive, in Fedora's layout. Not yet running in
  the background. The background side is a tray icon process (`cloudsync
  tray`) that manages the sync; built and smoke-tested, not yet checked by eye.
- What comes next is listed in `ISSUES.md` under "Next steps".

## Build and test

- `go build -o bin/ ./cmd/cloudsync`
- Before proposing a commit: `gofmt -l .`, `go vet ./...` and
  `go test ./...`, plus `go vet` for the other OS (`GOOS=linux` or
  `GOOS=windows`). Some tests only really run on one OS (symlinks and
  Windows-illegal names on Linux; the watcher backend differs per OS).
- On a fresh Fedora machine run `go env -w GOTOOLCHAIN=auto` once, so Go
  fetches the version `go.mod` asks for.
- **Live tests against real Drive** use `CLOUDSYNC_CONFIG` and
  `CLOUDSYNC_STATE_DIR` pointing at a scratch config, state folder and
  Drive folder under `CloudSync-Test/`. Never test against
  `CloudSync/MyFiles`, and never sync into `CloudSync-Test` from the real
  config.

## Gotchas

- **The root name is the state DB key; never rename the `MyFiles` root.**
- **On Windows, run `cloudsync` from a real terminal, not through the
  Claude desktop app's own tools.** The app is a packaged Windows app, so
  its tool commands see `%LocalAppData%` redirected to a private copy and
  get a different state DB (`ISSUES.md` #9). Don't keep scratch test
  folders under `%LocalAppData%` either.
- Drive stores `modifiedTime` to **millisecond** precision, while NTFS
  keeps 100 ns. Compare mtimes truncated to ms, or rely on MD5.
- A bare `syscall.Errno` (e.g. "no space left on device") satisfies
  `net.Error`, so "is it a network error?" must look for `*url.Error`,
  `*net.OpError` or `*net.DNSError`, and check `*fs.PathError` first.
- `go-api` reads small upload bodies into memory before sending, which is
  why upload progress is counted at the HTTP transport, not by wrapping
  the file.
- On Windows the running `bin/cloudsync.exe` can't be overwritten, so don't
  rebuild while a sync runs.
- Without rename detection, a moved file is an upload at the new path plus
  a trash at the old one, and a big reorganization trips the big-delete
  guard (`ISSUES.md` #6).
- A root `.cloudsyncignore` keeps secrets out of Drive (`.env`, `*.pem`,
  `client_secret*.json`, recovery-code files). It syncs, so every computer
  gets the same rules.

## Working with the owner

- They are a student. Explain decisions plainly, discuss design before
  writing code, and ask before committing, pushing or touching real data.
- Show the owner `cloudsync plan` and wait for their go-ahead before a
  real `cloudsync sync`; run it in a real terminal so the progress bar
  shows.
- Keep code idiomatic and in the existing style. The repo uses LF endings
  (`.gitattributes`).
- **The repo is public.** Never put real file or folder names from the
  synced folder in any file here; use generic examples.
- Never commit or print `client_secret.json`, tokens or `config.toml`
  (`.gitignore` covers them). Never read `client_secret.json`.
- `ISSUES.md`: add an entry as soon as an issue is found, update it when
  it is solved, and never delete solved entries. When a decision is made
  or changed, record it in `DECISIONS.md`; when packages change, update
  `docs/ARCHITECTURE.md`.
