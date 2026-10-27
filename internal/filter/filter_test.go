package filter

import (
	"slices"
	"testing"
)

func TestCheckName(t *testing.T) {
	ok := []string{"notes.pdf", "Sem 5", ".bashrc", "con-notes.txt", "a.b.c", "résumé.docx"}
	for _, n := range ok {
		if r := CheckName(n); r != "" {
			t.Errorf("CheckName(%q) = %q, want portable", n, r)
		}
	}
	bad := []string{"", "a:b", "what?.txt", "x*y", `back\slash`, "slash/name", "trailing.", "trailing ", "CON", "con.txt", "Lpt1.log", "nul ", "tab\tname"}
	for _, n := range bad {
		if CheckName(n) == "" {
			t.Errorf("CheckName(%q) = portable, want a reason", n)
		}
	}
}

type matchCase struct {
	rel  string
	dir  bool
	want bool
}

func check(t *testing.T, m *Matcher, cases []matchCase) {
	t.Helper()
	for _, c := range cases {
		if got := m.Match(c.rel, c.dir); got != c.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", c.rel, c.dir, got, c.want)
		}
	}
}

func TestGitignoreRules(t *testing.T) {
	m, err := New(append(slices.Clone(DefaultIgnore),
		"*.iso", "/College/Old/", "Sem 6/Lab/*.class", "**/drafts", "docs/**", "!keep.iso", `Notes\Private/`,
		"# a comment", "",
	))
	if err != nil {
		t.Fatal(err)
	}
	check(t, m, []matchCase{
		{"node_modules", true, true},
		{"a/b/Node_Modules", true, true}, // any depth, any case
		{"node_modules", false, false},   // folder-only rule
		{"Office/~$Report.docx", false, true},
		{".notes.pdf.cloudsync-partial", false, true},
		{"disk.ISO", false, true},
		{"deep/dir/disk.iso", false, true},
		{"keep.iso", false, false}, // re-included by !keep.iso
		{"College/Old", true, true},
		{"Other/College/Old", true, false}, // anchored to the root
		{"College/Old", false, false},      // folder-only rule
		{"Sem 6/Lab/Main.class", false, true},
		{"Sem 6/Lab/sub/Main.class", false, false}, // * stays within one folder
		{"x/y/drafts", true, true},
		{"drafts", false, true},
		{"docs/a/b.txt", false, true},
		{"docs", true, false},
		{"Notes/Private", true, true}, // backslash works as a separator
		{"Build", true, false},
		{"notes.pdf", false, false},
	})
}

func TestIgnoreFileScope(t *testing.T) {
	m, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddFile("College", []byte("# lecture recordings\r\n*.mp4\r\n/Sem5/big/\r\n!intro.mp4\r\n")); err != nil {
		t.Fatal(err)
	}
	check(t, m, []matchCase{
		{"College/a.mp4", false, true},
		{"College/Sem2/b.mp4", false, true},
		{"a.mp4", false, false},          // outside College
		{"Collegex/a.mp4", false, false}, // prefix of the name only
		{"College/Sem5/big", true, true},
		{"College/x/Sem5/big", true, false}, // anchored to College
		{"College/intro.mp4", false, false},
	})
}

func TestCloneIsIndependent(t *testing.T) {
	m, _ := New([]string{"*.iso"})
	c := m.Clone()
	c.AddFile("", []byte("*.mp4"))
	if m.Match("a.mp4", false) || !c.Match("a.mp4", false) {
		t.Error("rules added to a clone leaked into the original, or were lost")
	}
}

func TestTooLarge(t *testing.T) {
	m, _ := New(nil)
	if m.TooLarge(1 << 40) {
		t.Error("no limit set, but a file was too large")
	}
	m.SetMaxSize(1000)
	if !m.TooLarge(1001) || m.TooLarge(1000) {
		t.Error("limit of 1000 bytes applied wrongly")
	}
}

func TestBadPatterns(t *testing.T) {
	for _, p := range []string{"[unclosed", "/", "!"} {
		if _, err := New([]string{p}); err == nil {
			t.Errorf("New(%q) succeeded, want error", p)
		}
	}
}
