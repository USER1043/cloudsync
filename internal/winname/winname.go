// Package winname lets Windows hold files whose names it cannot store.
//
// A file's true name is the one in Drive and on Linux. On Windows, a name
// with a character Windows forbids, a trailing dot or space, or a reserved
// device name is stored under a lookalike stand-in, and a name that differs
// from a sibling only by letter case gets a " (case N)" suffix. A Map
// remembers every stand-in, so the true name is looked up, never guessed
// back from the local one: a file someone really named "a：b" stays "a：b".
package winname

import (
	"fmt"
	"path"
	"strings"
	"sync"
)

// fullWidth maps each character Windows forbids to its full-width lookalike.
var fullWidth = map[rune]rune{
	'<': '＜', '>': '＞', ':': '：', '"': '＂', '/': '／',
	'\\': '＼', '|': '｜', '?': '？', '*': '＊',
}

const (
	trailingSpace = '␣' // U+2423 OPEN BOX: a space Windows would strip
	trailingDot   = '․' // U+2024 ONE DOT LEADER: a dot Windows would strip
	reservedMark  = '＿' // U+FF3F, added after a reserved device name
)

var reservedNames = func() map[string]bool {
	m := map[string]bool{"con": true, "prn": true, "aux": true, "nul": true, "conin$": true, "conout$": true}
	for _, d := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
		m["com"+d] = true
		m["lpt"+d] = true
	}
	return m
}()

// Valid reports whether Windows can store name as it is.
func Valid(name string) bool { return name != "" && Encode(name) == name }

// Encode returns the stand-in Windows uses for name, or name itself when
// Windows can store it. Control characters become their Unicode pictures
// (U+2401 to U+241F) and forbidden characters their full-width lookalikes;
// trailing spaces and dots become ␣ and ․; a reserved device name gets ＿
// after it ("con.txt" becomes "con＿.txt"). The stand-in has the same
// length in UTF-16, except for the one added ＿.
func Encode(name string) string {
	s := strings.Map(func(r rune) rune {
		if r > 0 && r < 0x20 {
			return 0x2400 + r
		}
		if fw, ok := fullWidth[r]; ok {
			return fw
		}
		return r
	}, name)

	end := len(s)
	for end > 0 && (s[end-1] == ' ' || s[end-1] == '.') {
		end--
	}
	if end < len(s) {
		tail := strings.Map(func(r rune) rune {
			if r == ' ' {
				return trailingSpace
			}
			return trailingDot
		}, s[end:])
		s = s[:end] + tail
	}

	base, ext, hasExt := strings.Cut(s, ".")
	if reservedNames[strings.ToLower(strings.TrimRight(base, " "))] {
		s = base + string(reservedMark)
		if hasExt {
			s += "." + ext
		}
	}
	return s
}

// caseName returns name with " (case n)" before its extension.
func caseName(name string, n int) string {
	ext := path.Ext(name)
	if ext == name { // ".bashrc" is a name, not an extension
		ext = ""
	}
	return fmt.Sprintf("%s (case %d)%s", strings.TrimSuffix(name, ext), n, ext)
}

// Map records, for one sync root, which local name each true path has on
// Windows. Paths are slash-separated and relative to the root. A nil *Map is
// valid and maps every name to itself, which is what Linux uses.
type Map struct {
	mu sync.Mutex
	// standIn holds the local name of every entry whose local name differs
	// from its true name, keyed by true path. It is what the state DB stores.
	standIn map[string]string
	// taken holds, per folder (true path), every local name in use, lower-cased
	// because Windows ignores case, mapped to the true name using it.
	taken map[string]map[string]string
}

// New returns a Map with stand-ins loaded from the state DB, keyed by true
// path.
func New(standIns map[string]string) *Map {
	m := &Map{standIn: make(map[string]string, len(standIns)), taken: make(map[string]map[string]string)}
	for rel, local := range standIns {
		m.standIn[rel] = local
		m.take(rel, local)
	}
	return m
}

// Clone returns an independent copy, for previewing assignments.
func (m *Map) Clone() *Map {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := New(m.standIn)
	for dir, names := range m.taken {
		for lower, name := range names {
			c.takenIn(dir)[lower] = name
		}
	}
	return c
}

func (m *Map) takenIn(dir string) map[string]string {
	t := m.taken[dir]
	if t == nil {
		t = make(map[string]string)
		m.taken[dir] = t
	}
	return t
}

func (m *Map) take(rel, local string) {
	dir, name := split(rel)
	m.takenIn(dir)[strings.ToLower(local)] = name
}

// Local returns the local path of the true path rel.
func (m *Map) Local(rel string) string {
	if m == nil {
		return rel
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.local(rel)
}

func (m *Map) local(rel string) string {
	if len(m.standIn) == 0 || rel == "" {
		return rel
	}
	parts := strings.Split(rel, "/")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p
		if s, ok := m.standIn[strings.Join(parts[:i+1], "/")]; ok {
			out[i] = s
		}
	}
	return strings.Join(out, "/")
}

// StandIn returns the local name of the true path rel if it has a stand-in,
// or "".
func (m *Map) StandIn(rel string) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.standIn[rel]
}

// Found records a local entry seen by a scan: local is its name on disk,
// inside the folder whose true path is dir. It returns the entry's true
// name: the one the stand-in belongs to, or local itself.
func (m *Map) Found(dir, local string) string {
	if m == nil {
		return local
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, lower := m.takenIn(dir), strings.ToLower(local)
	if name, ok := t[lower]; ok && strings.ToLower(m.standIn[join(dir, name)]) == lower {
		return name
	}
	t[lower] = local
	return local
}

// Assign returns the local path for the true path rel, choosing a stand-in
// for its last name if Windows cannot store it or a sibling already uses it.
// The parent folder must have been assigned or found first. Assigning the
// same path again returns the same result.
func (m *Map) Assign(rel string) string {
	if m == nil {
		return rel
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.standIn[rel]; ok {
		return m.local(rel)
	}
	dir, name := split(rel)
	t := m.takenIn(dir)
	if t[strings.ToLower(name)] == name { // found on disk under its true name
		return m.local(rel)
	}
	local := Encode(name)
	for n := 2; ; n++ {
		owner, used := t[strings.ToLower(local)]
		if !used || owner == name {
			break
		}
		local = caseName(Encode(name), n)
	}
	t[strings.ToLower(local)] = name
	if local != name {
		m.standIn[rel] = local
	}
	return m.local(rel)
}

func split(rel string) (dir, name string) {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
