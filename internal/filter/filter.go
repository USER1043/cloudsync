// Package filter decides which paths CloudSync skips and flags names that
// cannot exist on both Windows and Linux.
package filter

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Issue is an entry that cannot be synced as it is.
type Issue struct {
	Rel    string // slash-separated path relative to the sync root
	Reason string
}

// IgnoreFile is the name of per-folder ignore files. Their rules apply to
// the folder they are in and everything below it.
const IgnoreFile = ".cloudsyncignore"

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

// Matcher applies gitignore-style rules. Rules are checked in order and the
// last one that matches a path decides, so later rules, and rules from
// deeper .cloudsyncignore files, override earlier ones.
//
// Supported syntax, matched case-insensitively:
//   - blank lines and lines starting with # are skipped
//   - "!" at the start re-includes what an earlier rule skipped
//   - a trailing "/" matches folders only
//   - a pattern with a "/" at the start or in the middle is anchored to the
//     folder the rule belongs to; otherwise it matches a name at any depth
//   - "*" matches within one name, "?" one character, "[abc]" one of a set,
//     and "**" any number of folders
//   - "\" is a separator like "/", since Windows names cannot contain it
type Matcher struct {
	rules   []rule
	maxSize int64 // 0 means no limit
}

type rule struct {
	re       *regexp.Regexp
	base     string // folder the rule applies under; "" is the root
	anchored bool
	negate   bool
	dirOnly  bool
}

// New compiles root-level patterns into a Matcher.
func New(patterns []string) (*Matcher, error) {
	m := &Matcher{}
	if err := m.AddRules("", patterns, "ignore pattern"); err != nil {
		return nil, err
	}
	return m, nil
}

// AddRules adds patterns that apply under the folder base ("" for the
// root). source names where they came from, for error messages.
func (m *Matcher) AddRules(base string, patterns []string, source string) error {
	for i, line := range patterns {
		r, ok, err := compile(line)
		if err != nil {
			return fmt.Errorf("%s %d: %w", source, i+1, err)
		}
		if ok {
			r.base = base
			m.rules = append(m.rules, r)
		}
	}
	return nil
}

// AddFile adds the rules of the .cloudsyncignore found in the folder base.
func (m *Matcher) AddFile(base string, content []byte) error {
	return m.AddRules(base, strings.Split(string(content), "\n"), path.Join(base, IgnoreFile)+" line")
}

// Clone returns a copy that can take more rules without changing m.
func (m *Matcher) Clone() *Matcher {
	c := *m
	c.rules = slices.Clone(m.rules)
	return &c
}

// SetMaxSize sets the largest file size that is synced; 0 removes the limit.
func (m *Matcher) SetMaxSize(n int64) { m.maxSize = n }

// TooLarge reports whether a file of this size is over the size limit.
func (m *Matcher) TooLarge(size int64) bool { return m.maxSize > 0 && size > m.maxSize }

// Match reports whether the path rel (slash-separated, relative to the sync
// root) is skipped. It does not look at parent folders: callers stop
// descending into a folder once it matches.
func (m *Matcher) Match(rel string, isDir bool) bool {
	skipped := false
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		sub := rel
		if r.base != "" {
			if !strings.HasPrefix(rel, r.base+"/") {
				continue
			}
			sub = rel[len(r.base)+1:]
		}
		if !r.anchored {
			sub = path.Base(sub)
		}
		if r.re.MatchString(sub) {
			skipped = !r.negate
		}
	}
	return skipped
}

func compile(line string) (rule, bool, error) {
	p := strings.TrimSpace(line)
	if p == "" || strings.HasPrefix(p, "#") {
		return rule{}, false, nil
	}
	var r rule
	if strings.HasPrefix(p, "!") {
		r.negate = true
		p = p[1:]
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if strings.HasSuffix(p, "/") {
		r.dirOnly = true
		p = strings.TrimRight(p, "/")
	}
	if strings.Contains(p, "/") {
		r.anchored = true
		p = strings.TrimPrefix(p, "/")
	}
	if p == "" {
		return rule{}, false, fmt.Errorf("%q matches nothing", line)
	}
	expr, err := globToRegexp(p)
	if err != nil {
		return rule{}, false, fmt.Errorf("%q: %w", line, err)
	}
	if r.re, err = regexp.Compile("(?i)^" + expr + "$"); err != nil {
		return rule{}, false, fmt.Errorf("%q: %w", line, err)
	}
	return r, true, nil
}

func globToRegexp(p string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				i++
				if i+1 < len(p) && p[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?") // "**/": zero or more folders
				} else {
					b.WriteString(".*") // trailing "**": everything below
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(p[i+1:], ']')
			if end < 0 {
				return "", errors.New("unclosed [")
			}
			class := p[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String(), nil
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
