// Package localfs reads a local sync root.
package localfs

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/USER1043/cloudsync/internal/filter"
)

// File is a regular file that CloudSync would sync.
type File struct {
	Rel     string // slash-separated path relative to the sync root
	Size    int64
	ModTime time.Time
}

// Result is everything Scan found under a sync root.
type Result struct {
	Files    []File
	Dirs     []string       // folders that are synced, including empty ones
	Repos    []string       // folders skipped because they are git repositories
	Ignored  []string       // entries skipped by ignore rules; folders end in "/"
	TooLarge []File         // files over the size limit
	Issues   []filter.Issue // entries that cannot be synced as they are

	// Matcher is the scan's matcher plus every .cloudsyncignore found, for
	// applying the same rules to the Drive side.
	Matcher *filter.Matcher
}

// maxRelPath keeps root-relative paths short enough to stay within Windows'
// 260-character limit once the root folder is prefixed.
const maxRelPath = 240

// Scan walks root and sorts every entry into synced files, skipped git
// repositories, ignored entries and issues. Besides m's rules it applies any
// .cloudsyncignore files it finds. It reads metadata and ignore files only.
func Scan(root string, m *filter.Matcher) (*Result, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("sync root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("sync root %s is not a folder", root)
	}

	m = m.Clone()
	if err := loadIgnoreFile(m, root, ""); err != nil {
		return nil, err
	}
	res := &Result{Matcher: m}
	names := make(map[string]map[string]string) // parent rel -> lower-cased name -> name
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if p == root {
			return err
		}
		rel := relPath(root, p)
		if err != nil {
			res.Issues = append(res.Issues, filter.Issue{Rel: rel, Reason: err.Error()})
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		name, isDir := d.Name(), d.IsDir()
		if m.Match(rel, isDir) {
			if isDir {
				res.Ignored = append(res.Ignored, rel+"/")
				return fs.SkipDir
			}
			res.Ignored = append(res.Ignored, rel)
			return nil
		}
		if reason := checkPath(names, rel, name); reason != "" {
			res.Issues = append(res.Issues, filter.Issue{Rel: rel, Reason: reason})
			if isDir {
				return fs.SkipDir
			}
			return nil
		}

		switch {
		case isDir:
			if isRepo(p) {
				res.Repos = append(res.Repos, rel)
				return fs.SkipDir
			}
			res.Dirs = append(res.Dirs, rel)
			// Rules in this folder's .cloudsyncignore apply to everything below it,
			// which WalkDir visits next.
			return loadIgnoreFile(m, p, rel)
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				res.Issues = append(res.Issues, filter.Issue{Rel: rel, Reason: err.Error()})
				return nil
			}
			f := File{Rel: rel, Size: fi.Size(), ModTime: fi.ModTime()}
			if m.TooLarge(f.Size) {
				res.TooLarge = append(res.TooLarge, f)
				return nil
			}
			res.Files = append(res.Files, f)
		case d.Type()&fs.ModeSymlink != 0:
			res.Issues = append(res.Issues, filter.Issue{Rel: rel, Reason: "symlink; only regular files are synced"})
		default:
			res.Issues = append(res.Issues, filter.Issue{Rel: rel, Reason: "special file (junction, device or socket); not synced"})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// checkPath returns why rel cannot be synced, or "". It also records name
// so a later sibling that differs only by case is caught.
func checkPath(names map[string]map[string]string, rel, name string) string {
	if reason := filter.CheckName(name); reason != "" {
		return reason
	}
	if n := len(utf16.Encode([]rune(rel))); n > maxRelPath {
		return fmt.Sprintf("path is %d characters long; it must stay under %d for Windows", n, maxRelPath)
	}
	parent, lower := path.Dir(rel), strings.ToLower(name)
	if names[parent] == nil {
		names[parent] = make(map[string]string)
	}
	if other, ok := names[parent][lower]; ok {
		return fmt.Sprintf("differs from %q only by letter case; Windows cannot hold both", other)
	}
	names[parent][lower] = name
	return ""
}

// loadIgnoreFile adds the rules of dir's .cloudsyncignore, if it has one, to
// m. rel is dir relative to the sync root. A broken rule stops the scan
// rather than risk syncing files the user meant to skip.
func loadIgnoreFile(m *filter.Matcher, dir, rel string) error {
	b, err := os.ReadFile(filepath.Join(dir, filter.IgnoreFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return m.AddFile(rel, b)
}

// isRepo reports whether dir is a git repository or worktree. Worktrees and
// submodules use a .git file rather than a folder, so either counts.
func isRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

func relPath(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// MD5File returns the hex MD5 of a file, the checksum Google Drive reports.
func MD5File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
