package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/USER1043/cloudsync/internal/config"
)

// driveParent is where `init` suggests and looks for synced folders in Drive.
const driveParent = "CloudSync"

func runInit(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("init", flag.ContinueOnError)
	local := fl.String("local", "", "folder to sync on this computer")
	remote := fl.String("remote", "", "folder in My Drive to sync it with, e.g. CloudSync/MyFiles")
	maxSize := fl.String("max-file-size", "", `skip files larger than this, e.g. "500MB"`)
	force := fl.Bool("force", false, "replace an existing config")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if _, err := config.ParseSize(*maxSize); err != nil {
		return fmt.Errorf("--max-file-size: %w", err)
	}

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	if p := config.FilePath(dir); fileExists(p) && !*force {
		return fmt.Errorf("%s already exists; edit it, or run `cloudsync init --force` to start over", p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	// Drive is optional here. When signed in, init can offer the folders
	// another computer already syncs, so both end up on the same one.
	var existing []string
	if d, err := driveClient(ctx, dir); err == nil {
		if existing, err = d.SubFolders(ctx, []string{driveParent}); err != nil {
			return err
		}
	} else if signInMissing(err) {
		fmt.Println("Not signed in to Google, so folders already in Drive can't be shown (run `cloudsync login` first to see them).")
	} else {
		return err
	}

	root, err := setup(os.Stdout, bufio.NewReader(os.Stdin), home, *local, *remote, existing)
	if err != nil {
		return err
	}
	p, err := config.Write(dir, config.Config{MaxFileSize: *maxSize, Roots: []config.Root{root}})
	if err != nil {
		return err
	}
	fmt.Printf("\nSaved %s\n  %s  <->  My Drive/%s\n", p, root.Path, root.Remote)
	fmt.Println("\nNext:\n  cloudsync plan    see what the first sync will do (changes nothing)\n  cloudsync sync    run it")
	return nil
}

// setup works out the root to save, asking on w/in for whatever local and
// remote leave empty. existing lists folders already in My Drive/CloudSync.
func setup(w io.Writer, in *bufio.Reader, home, local, remote string, existing []string) (config.Root, error) {
	if local == "" {
		local = ask(w, in, "Folder to sync on this computer", filepath.Join(home, "CloudSync"))
	}
	local, err := config.ExpandPath(local)
	if err != nil {
		return config.Root{}, err
	}
	if err := checkLocal(local, home); err != nil {
		return config.Root{}, err
	}
	if !fileExists(local) {
		if err := os.MkdirAll(local, 0o755); err != nil {
			return config.Root{}, err
		}
		fmt.Fprintf(w, "Created %s\n", local)
	}

	if remote == "" {
		def := driveParent + "/" + filepath.Base(local)
		if len(existing) > 0 {
			fmt.Fprintf(w, "\nFolders already in My Drive/%s (another computer may sync one of them):\n", driveParent)
			for i, name := range existing {
				fmt.Fprintf(w, "  %d) %s/%s\n", i+1, driveParent, name)
			}
			fmt.Fprintln(w, "Pick a number to share one of them, or type a new folder path.")
			if len(existing) == 1 {
				def = "1"
			}
		}
		remote = ask(w, in, "Google Drive folder", def)
		if n, err := strconv.Atoi(remote); err == nil && n >= 1 && n <= len(existing) {
			remote = driveParent + "/" + existing[n-1]
		}
	}
	segs := strings.FieldsFunc(strings.ReplaceAll(remote, `\`, "/"), func(r rune) bool { return r == '/' })
	if len(segs) == 0 {
		return config.Root{}, errors.New("the Drive folder must be a folder inside My Drive, e.g. CloudSync/MyFiles")
	}
	remote = strings.Join(segs, "/")
	for _, s := range existing {
		if remote == driveParent+"/"+s {
			fmt.Fprintln(w, "That Drive folder already has files. The first sync merges them with this folder: nothing is deleted, and files that differ keep both copies.")
		}
	}
	return config.Root{Name: segs[len(segs)-1], Remote: remote, Path: local}, nil
}

// checkLocal refuses folders that would sync far more than intended.
func checkLocal(local, home string) error {
	switch {
	case filepath.Dir(local) == local:
		return fmt.Errorf("%s is a whole drive; pick a folder inside it", local)
	case strings.EqualFold(filepath.Clean(local), filepath.Clean(home)):
		return fmt.Errorf("%s is your whole home folder; pick a folder inside it", local)
	}
	return nil
}

// ask prints a prompt with a default and returns the answer, or the
// default when the answer is empty.
func ask(w io.Writer, in *bufio.Reader, prompt, def string) string {
	fmt.Fprintf(w, "%s [%s]: ", prompt, def)
	line, _ := in.ReadString('\n')
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return def
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
