// Command cloudsync keeps a local folder in sync with Google Drive across
// Windows and Linux.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usageText = `CloudSync keeps a local folder in sync with Google Drive.

Usage:
  cloudsync <command> [flags]

Commands:
  login     Sign in to Google in your browser and store the token securely
  logout    Revoke and remove the stored Google token
  whoami    Show the signed-in account and Drive storage
  init      Choose the folder to sync and its Drive folder; writes config.toml
            --local PATH  --remote CloudSync/NAME  --max-file-size 500MB  --force
  plan      Dry run: show what the first sync would do (changes nothing)
            --all  list every entry instead of the first 20 per section
  sync      Run the first sync: upload, download and keep both copies of
            conflicting files. Never deletes or overwrites. Safe to re-run.
            --yes  start without asking for confirmation
  version   Print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1], os.Args[2:])
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "login":
		return runLogin(ctx)
	case "logout":
		return runLogout(ctx)
	case "whoami":
		return runWhoami(ctx)
	case "init":
		return runInit(ctx, args)
	case "plan":
		return runPlan(ctx, args)
	case "sync":
		return runSync(ctx, args)
	case "version":
		fmt.Println("cloudsync", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return nil
	default:
		return fmt.Errorf("unknown command %q (run `cloudsync help`)", cmd)
	}
}
