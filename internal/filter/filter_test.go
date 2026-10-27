package filter

import "testing"

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

func TestMatcher(t *testing.T) {
	m, err := New(append(DefaultIgnore, "*.ISO", "Downloads/", "# comment", ""))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		dir  bool
		want bool
	}{
		{"node_modules", true, true},
		{"node_modules", false, false}, // folder-only pattern
		{"Node_Modules", true, true},   // case-insensitive
		{"~$Report.docx", false, true},
		{"thumbs.db", false, true},
		{"disk.iso", false, true},
		{"downloads", true, true},
		{"notes.pdf", false, false},
		{"Build", true, false},
		{".metadata", true, true},
		{".notes.pdf.cloudsync-partial", false, true},
	}
	for _, c := range cases {
		var got bool
		if c.dir {
			got = m.IgnoreDir(c.name)
		} else {
			got = m.IgnoreFile(c.name)
		}
		if got != c.want {
			t.Errorf("ignore(%q, dir=%v) = %v, want %v", c.name, c.dir, got, c.want)
		}
	}
}

func TestMatcherRejectsBadPatterns(t *testing.T) {
	for _, p := range []string{"a/b", "[unclosed"} {
		if _, err := New([]string{p}); err == nil {
			t.Errorf("New(%q) succeeded, want error", p)
		}
	}
}
