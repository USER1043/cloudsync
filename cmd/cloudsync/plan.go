package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/filter"
	"github.com/USER1043/cloudsync/internal/gdrive"
	"github.com/USER1043/cloudsync/internal/localfs"
	"github.com/USER1043/cloudsync/internal/plan"
	"github.com/USER1043/cloudsync/internal/state"
	"github.com/USER1043/cloudsync/internal/winname"
)

const listLimit = 20

// matcher combines the built-in ignore rules with the config's rules and
// size limit. .cloudsyncignore files are added during the scan.
func matcher(cfg config.Config) (*filter.Matcher, error) {
	m, err := filter.New(filter.DefaultIgnore)
	if err != nil {
		return nil, err
	}
	if err := m.AddRules("", cfg.Ignore, "ignore rule"); err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.Path(), err)
	}
	m.SetMaxSize(cfg.MaxBytes())
	return m, nil
}

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
	m, err := matcher(e.cfg)
	if err != nil {
		return err
	}
	w := os.Stdout
	printConfigSource(w, e.cfg)

	d, err := e.drive(ctx)
	if err != nil {
		if !signInMissing(err) {
			return err
		}
		fmt.Fprintf(w, "Drive: not checked (%v)\nEvery local file counts as an upload.\n", err)
	}

	var upload int64
	db, err := peekState()
	if err != nil {
		return err
	}
	if db != nil {
		defer db.Close()
	}
	for _, root := range e.cfg.Roots {
		names, err := loadNames(ctx, db, root.Name)
		if err != nil {
			return err
		}
		in, err := inspect(ctx, root, m, d, names)
		if err != nil {
			return fmt.Errorf("root %q: %w", root.Name, err)
		}
		printReport(w, in, d != nil, limit)
		upload += in.uploadBytes()
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

func printConfigSource(w io.Writer, cfg config.Config) {
	fmt.Fprintln(w, "Config:", cfg.Path())
}

// loadNames returns the stand-in names root uses on this computer: a Map
// loaded from db on Windows (empty if db is nil), and nil on other systems,
// which store every name as it is.
func loadNames(ctx context.Context, db *state.DB, root string) (*winname.Map, error) {
	if runtime.GOOS != "windows" {
		return nil, nil
	}
	if db == nil {
		return winname.New(nil), nil
	}
	standIns, err := db.StandIns(ctx, root)
	if err != nil {
		return nil, err
	}
	return winname.New(standIns), nil
}

// peekState opens the state database without the sync lock, for reading
// stand-in names, or returns nil if there is none yet.
func peekState() (*state.DB, error) {
	if runtime.GOOS != "windows" {
		return nil, nil
	}
	dir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "state.db")
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return state.Open(p)
}

// inspection is one root's local scan, Drive listing and first-sync plan.
type inspection struct {
	root         config.Root
	local        string
	names        *winname.Map // stand-in names on Windows; nil elsewhere
	rootID       string       // "" if the Drive folder does not exist yet or Drive was not checked
	scan         *localfs.Result
	tree         *gdrive.Tree // Drive listing with ignored items removed
	driveIgnored []string     // Drive paths the ignore rules and size limit left out
	plan         *plan.Plan
}

// inspect scans a root and, when d is not nil, lists its Drive folder and
// works out the first-sync plan. It changes nothing.
func inspect(ctx context.Context, root config.Root, m *filter.Matcher, d *gdrive.Client, names *winname.Map) (*inspection, error) {
	local, err := root.LocalPath()
	if err != nil {
		return nil, err
	}
	in := &inspection{root: root, local: local, names: names, tree: &gdrive.Tree{}}
	if d != nil {
		if in.rootID, err = d.FindFolder(ctx, root.RemoteSegments()); err != nil {
			return nil, err
		}
		if in.rootID != "" {
			if in.tree, err = d.ListTree(ctx, in.rootID); err != nil {
				return nil, err
			}
		}
	}
	if in.scan, err = localfs.Scan(local, m, names); err != nil {
		return nil, err
	}
	// The scan's matcher includes the local .cloudsyncignore files, so Drive
	// gets the same rules.
	in.tree, in.driveIgnored = in.tree.Filter(in.scan.Matcher)
	in.plan = plan.FirstSync(in.scan.Files, in.tree.Files, func(rel string) (string, error) {
		return localfs.MD5File(filepath.Join(local, filepath.FromSlash(names.Local(rel))))
	})
	return in, nil
}

// uploadBytes is what the first sync would add to Drive: uploads plus the
// local copies of conflicting files.
func (in *inspection) uploadBytes() int64 {
	t := in.plan.Totals()
	return t[plan.Upload].Bytes + t[plan.Conflict].Bytes
}

func printReport(w io.Writer, in *inspection, driveChecked bool, limit int) {
	fmt.Fprintf(w, "\n== %s\nLocal: %s\nDrive: My Drive/%s", in.root.Name, in.local, strings.Join(in.root.RemoteSegments(), "/"))
	if driveChecked && in.rootID == "" {
		fmt.Fprint(w, " (does not exist yet; the first sync creates it)")
	}
	fmt.Fprintln(w)

	printFolders(w, in.scan.Files)

	t := in.plan.Totals()
	fmt.Fprintln(w, "\nFirst sync would:")
	fmt.Fprintf(w, "  upload     %7d files  %s\n", t[plan.Upload].Count, humanBytes(t[plan.Upload].Bytes))
	fmt.Fprintf(w, "  download   %7d files  %s\n", t[plan.Download].Count, humanBytes(t[plan.Download].Bytes))
	fmt.Fprintf(w, "  in sync    %7d files\n", t[plan.InSync].Count)
	fmt.Fprintf(w, "  conflicts  %7d files  (both copies kept)\n", t[plan.Conflict].Count)

	printLargest(w, in.plan.Of(plan.Upload), 10)
	printList(w, "Git repos skipped (sync these through GitHub)", in.scan.Repos, limit)
	printList(w, "Ignored by rules", in.scan.Ignored, limit)
	var large []string
	for _, f := range in.scan.TooLarge {
		large = append(large, fmt.Sprintf("%s (%s)", f.Rel, humanBytes(f.Size)))
	}
	printList(w, "Over max_file_size", large, limit)
	printList(w, "Ignored in Drive (not downloaded)", in.driveIgnored, limit)

	var issues []string
	for _, is := range in.scan.Issues {
		issues = append(issues, fmt.Sprintf("%s: %s", is.Rel, is.Reason))
	}
	for _, is := range in.tree.Issues {
		issues = append(issues, fmt.Sprintf("[Drive] %s: %s", is.Rel, is.Reason))
	}
	printList(w, "Cannot sync until renamed or fixed", issues, limit)
	if in.names != nil {
		printList(w, "Stand-in names on this computer (true name in Drive -> name here)", in.standIns(), limit)
	} else {
		printList(w, "Shown with a stand-in name on Windows (true name -> name on Windows)", in.standIns(), limit)
	}

	var native []string
	for _, f := range in.tree.Native {
		native = append(native, f.Rel)
	}
	printList(w, "Google Docs/Sheets/Slides in Drive (skipped, no file to download)", native, limit)
	printList(w, "Conflicts", itemLines(in.plan.Of(plan.Conflict)), limit)
	printList(w, "Downloads", itemLines(in.plan.Of(plan.Download)), limit)
}

// standIns lists every synced entry whose name Windows stores as a stand-in.
// On Windows they are the names this sync uses; elsewhere, the names a
// Windows computer syncing with an empty folder would use.
func (in *inspection) standIns() []string {
	names := in.names.Clone()
	if names == nil {
		names = winname.New(nil)
	}
	var dirs, files []string
	dirs = append(dirs, in.scan.Dirs...)
	for _, f := range in.tree.Folders {
		dirs = append(dirs, f.Rel)
	}
	for _, f := range in.scan.Files {
		files = append(files, f.Rel)
	}
	for _, f := range in.tree.Files {
		files = append(files, f.Rel)
	}
	// Folders first and in order, so parents are assigned before children.
	slices.Sort(dirs)
	slices.Sort(files)
	var lines []string
	for _, rel := range slices.Concat(slices.Compact(dirs), slices.Compact(files)) {
		names.Assign(rel)
		if s := names.StandIn(rel); s != "" {
			lines = append(lines, fmt.Sprintf("%q -> %q", rel, s))
		}
	}
	return lines
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
