package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/USER1043/cloudsync/internal/auth"
	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/localfs"
	"github.com/USER1043/cloudsync/internal/plan"
)

const listLimit = 20

func runPlan(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("plan", flag.ContinueOnError)
	all := fl.Bool("all", false, "list every entry instead of the first 20 per section")
	if err := fl.Parse(args); err != nil {
		return err
	}
	limit := listLimit
	if *all {
		limit = -1
	}

	e, err := loadEnv()
	if err != nil {
		return err
	}
	m, err := filter.New(append(slices.Clone(filter.DefaultIgnore), e.cfg.Ignore...))
	if err != nil {
		return err
	}
	w := os.Stdout
	if e.cfg.Path() == "" {
		fmt.Fprintln(w, "Config: built-in defaults (copy config.example.toml to customise)")
	} else {
		fmt.Fprintln(w, "Config:", e.cfg.Path())
	}

	d, err := e.drive(ctx)
	if err != nil {
		if !errors.Is(err, auth.ErrNotLoggedIn) && !errors.Is(err, auth.ErrNoClientConfig) {
			return err
		}
		fmt.Fprintf(w, "Drive: not checked (%v)\nEvery local file counts as an upload.\n", err)
	}

	var upload int64
	for _, root := range e.cfg.Roots {
		n, err := planRoot(ctx, w, root, m, d, limit)
		if err != nil {
			return fmt.Errorf("root %q: %w", root.Name, err)
		}
		upload += n
	}

	fmt.Fprintln(w)
	if d != nil {
		a, err := d.Account(ctx)
		if err != nil {
			return err
		}
		printQuota(w, a, upload)
	} else {
		fmt.Fprintf(w, "Total to upload: %s (Google's free tier is 15 GB, shared with Gmail and Photos)\n", humanBytes(upload))
	}
	fmt.Fprintln(w, "\nDry run only: nothing was uploaded, downloaded or changed.")
	return nil
}

// planRoot scans one root, compares it with Drive when d is not nil, and
// prints the result. It returns the bytes the first sync would upload.
func planRoot(ctx context.Context, w io.Writer, root config.Root, m *filter.Matcher, d *gdrive.Client, limit int) (int64, error) {
	local, err := root.LocalPath()
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(w, "\n== %s\nLocal: %s\nDrive: My Drive/%s", root.Name, local, strings.Join(root.RemoteSegments(), "/"))

	tree := &gdrive.Tree{}
	if d != nil {
		id, err := d.FindFolder(ctx, root.RemoteSegments())
		if err != nil {
			return 0, err
		}
		if id == "" {
			fmt.Fprint(w, " (does not exist yet; the first sync creates it)")
		} else if tree, err = d.ListTree(ctx, id); err != nil {
			return 0, err
		}
	}
	fmt.Fprintln(w)

	scan, err := localfs.Scan(local, m)
	if err != nil {
		return 0, err
	}
	p := plan.FirstSync(scan.Files, tree.Files, func(rel string) (string, error) {
		return localfs.MD5File(filepath.Join(local, filepath.FromSlash(rel)))
	})

	printFolders(w, scan.Files)

	t := p.Totals()
	fmt.Fprintln(w, "\nFirst sync would:")
	fmt.Fprintf(w, "  upload     %7d files  %s\n", t[plan.Upload].Count, humanBytes(t[plan.Upload].Bytes))
	fmt.Fprintf(w, "  download   %7d files  %s\n", t[plan.Download].Count, humanBytes(t[plan.Download].Bytes))
	fmt.Fprintf(w, "  in sync    %7d files\n", t[plan.InSync].Count)
	fmt.Fprintf(w, "  conflicts  %7d files  (both copies kept)\n", t[plan.Conflict].Count)

	printLargest(w, p.Of(plan.Upload), 10)
	printList(w, "Git repos skipped (sync these through GitHub)", scan.Repos, limit)
	printList(w, "Ignored by rules", scan.Ignored, limit)

	var issues []string
	for _, is := range scan.Issues {
		issues = append(issues, fmt.Sprintf("%s: %s", is.Rel, is.Reason))
	}
	for _, is := range tree.Issues {
		issues = append(issues, fmt.Sprintf("[Drive] %s: %s", is.Rel, is.Reason))
	}
	printList(w, "Cannot sync until renamed or fixed", issues, limit)

	var native []string
	for _, f := range tree.Native {
		native = append(native, f.Rel)
	}
	printList(w, "Google Docs/Sheets/Slides in Drive (skipped, no file to download)", native, limit)
	printList(w, "Conflicts", itemLines(p.Of(plan.Conflict)), limit)
	printList(w, "Downloads", itemLines(p.Of(plan.Download)), limit)

	return t[plan.Upload].Bytes + t[plan.Conflict].Bytes, nil
}

// printFolders shows how the synced files split across top-level folders.
func printFolders(w io.Writer, files []localfs.File) {
	type row struct {
		name  string
		count int
		bytes int64
	}
	byName := make(map[string]*row)
	for _, f := range files {
		name, _, nested := strings.Cut(f.Rel, "/")
		if !nested {
			name = "(files directly in the root)"
		}
		r := byName[name]
		if r == nil {
			r = &row{name: name}
			byName[name] = r
		}
		r.count++
		r.bytes += f.Size
	}
	if len(byName) == 0 {
		return
	}
	rows := make([]*row, 0, len(byName))
	for _, r := range byName {
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(a, b *row) int { return cmp.Compare(b.bytes, a.bytes) })
	fmt.Fprintln(w, "\nSize by top-level folder (files that would sync):")
	for _, r := range rows {
		fmt.Fprintf(w, "  %10s  %7d files  %s\n", humanBytes(r.bytes), r.count, r.name)
	}
}

func printLargest(w io.Writer, items []plan.Item, n int) {
	if len(items) == 0 {
		return
	}
	items = slices.Clone(items)
	slices.SortFunc(items, func(a, b plan.Item) int { return cmp.Compare(b.Size, a.Size) })
	fmt.Fprintln(w, "\nLargest uploads:")
	for _, it := range items[:min(n, len(items))] {
		fmt.Fprintf(w, "  %10s  %s\n", humanBytes(it.Size), it.Rel)
	}
}

func printList(w io.Writer, title string, lines []string, limit int) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s (%d):\n", title, len(lines))
	for i, l := range lines {
		if limit >= 0 && i == limit {
			fmt.Fprintf(w, "  ... and %d more (use --all)\n", len(lines)-limit)
			break
		}
		fmt.Fprintf(w, "  %s\n", l)
	}
}

func itemLines(items []plan.Item) []string {
	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = it.Rel
		if it.Note != "" {
			lines[i] += " (" + it.Note + ")"
		}
	}
	return lines
}

func printQuota(w io.Writer, a gdrive.Account, upload int64) {
	fmt.Fprintf(w, "Google account: %s <%s>\n", a.Name, a.Email)
	if a.Limit <= 0 {
		fmt.Fprintf(w, "Storage: no limit; the first sync uploads %s\n", humanBytes(upload))
		return
	}
	free := a.Limit - a.Usage
	verdict := "fits"
	if upload > free {
		verdict = fmt.Sprintf("does NOT fit, %s short", humanBytes(upload-free))
	}
	fmt.Fprintf(w, "Storage: %s free of %s; the first sync uploads %s, which %s\n",
		humanBytes(free), humanBytes(a.Limit), humanBytes(upload), verdict)
}
