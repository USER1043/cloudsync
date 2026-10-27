// Package filter decides which paths CloudSync skips and flags names that
// cannot exist on both Windows and Linux.
package filter

import (
	"fmt"
	"path"
	"strings"
)

// Issue is an entry that cannot be synced as it is.
type Issue struct {
	Rel    string // slash-separated path relative to the sync root
	Reason string
}

// DefaultIgnore is always applied. Folders that are git repositories are
// skipped separately, so these only cover clutter outside repos.
var DefaultIgnore = []string{
	// Dependency folders: huge and regenerable.
	"node_modules/", ".venv/", "venv/", "__pycache__/", ".gradle/",
	// Eclipse workspace cache: rebuilt by Eclipse and rewritten while it runs.
	".metadata/",
	// Editor, OS and Office temp files.
	"~$*", "*.tmp", "*.swp", ".DS_Store", "Thumbs.db", "desktop.ini",
	// CloudSync's own partial downloads.
	".*.cloudsync-partial",
}

// Matcher matches base names against ignore patterns. A pattern ending in
// "/" matches folders only; any other pattern matches files and folders.
// Patterns use path.Match wildcards and are case-insensitive.
type Matcher struct {
	dirOnly []string
	any     []string
}

// New compiles patterns into a Matcher.
func New(patterns []string) (*Matcher, error) {
	m := &Matcher{}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		dir := strings.HasSuffix(p, "/")
		p = strings.TrimSuffix(p, "/")
		if strings.Contains(p, "/") {
			return nil, fmt.Errorf("ignore pattern %q: patterns match a single name, not a path", p)
		}
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("ignore pattern %q: %w", p, err)
		}
		if dir {
			m.dirOnly = append(m.dirOnly, p)
		} else {
			m.any = append(m.any, p)
		}
	}
	return m, nil
}

// IgnoreDir reports whether a folder with this name is skipped along with
// everything inside it.
func (m *Matcher) IgnoreDir(name string) bool {
	n := strings.ToLower(name)
	return matchAny(m.dirOnly, n) || matchAny(m.any, n)
}

// IgnoreFile reports whether a file with this name is skipped.
func (m *Matcher) IgnoreFile(name string) bool {
	return matchAny(m.any, strings.ToLower(name))
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

var reservedNames = func() map[string]bool {
	m := map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}
	for i := 1; i <= 9; i++ {
		m[fmt.Sprintf("com%d", i)] = true
		m[fmt.Sprintf("lpt%d", i)] = true
	}
	return m
}()

// CheckName returns why a file or folder name cannot exist on both Windows
// and Linux, or "" if it is portable.
func CheckName(name string) string {
	if name == "" {
		return "empty name"
	}
	for _, r := range name {
		if r < 0x20 {
			return "contains a control character"
		}
		if strings.ContainsRune(`<>:"/\|?*`, r) {
			return fmt.Sprintf("contains %q, which Windows does not allow", r)
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "ends with a dot or space, which Windows does not allow"
	}
	base, _, _ := strings.Cut(name, ".")
	if reservedNames[strings.ToLower(strings.TrimRight(base, " "))] {
		return "is a reserved device name on Windows"
	}
	return ""
}
