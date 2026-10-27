package winname

import (
	"testing"
	"unicode/utf16"
)

func TestEncode(t *testing.T) {
	cases := map[string]string{
		// Already fine on Windows: unchanged.
		"notes.pdf":     "notes.pdf",
		"Sem 5":         "Sem 5",
		".bashrc":       ".bashrc",
		"con-notes.txt": "con-notes.txt",
		"résumé.docx":   "résumé.docx",
		" leading":      " leading",
		"a：b":           "a：b", // a real full-width colon is left alone

		// Forbidden characters become full-width lookalikes.
		"Title:Subtitle.pdf": "Title：Subtitle.pdf",
		"what?.txt":          "what？.txt",
		`a<b>c"d|e*f\g`:      `a＜b＞c＂d｜e＊f＼g`,
		"tab\tname":          "tab␉name",

		// Trailing spaces and dots get visible markers; inner ones stay.
		"Hackathon ":   "Hackathon␣",
		"etc.":         "etc․",
		"a. .":         "a․␣․",
		"...":          "․․․",
		"v1.2 final .": "v1.2 final␣․",

		// Reserved device names, with or without an extension, in any case.
		"CON":        "CON＿",
		"con.txt":    "con＿.txt",
		"Lpt1.log":   "Lpt1＿.log",
		"aux.tar.gz": "aux＿.tar.gz",
		"COM¹":       "COM¹＿",
		"conout$":    "conout$＿",
		"nul ":       "nul␣", // the marker already makes it a different name
	}
	for in, want := range cases {
		got := Encode(in)
		if got != want {
			t.Errorf("Encode(%q) = %q, want %q", in, got, want)
		}
		if !Valid(got) {
			t.Errorf("Encode(%q) = %q, which is still not valid", in, got)
		}
		if Valid(in) != (got == in) {
			t.Errorf("Valid(%q) = %v, but Encode changed it to %q", in, Valid(in), got)
		}
	}
	if Valid("") {
		t.Error(`Valid("") = true`)
	}
}

func TestEncodeKeepsLength(t *testing.T) {
	for _, in := range []string{"Title:Subtitle.pdf", "Hackathon ", "a. .", "tab\tname"} {
		if a, b := len(utf16.Encode([]rune(in))), len(utf16.Encode([]rune(Encode(in)))); a != b {
			t.Errorf("Encode(%q) changed the UTF-16 length from %d to %d", in, a, b)
		}
	}
}

func TestAssign(t *testing.T) {
	m := New(nil)
	steps := []struct{ rel, want string }{
		{"Docs", "Docs"},
		{"Docs/report.pdf", "Docs/report.pdf"},
		{"Hackathon ", "Hackathon␣"},
		{"Hackathon /plan.pdf", "Hackathon␣/plan.pdf"}, // children follow the folder's stand-in
		{"Hackathon /a:b.txt", "Hackathon␣/a：b.txt"},
		{"Notes.txt", "Notes.txt"},
		{"notes.txt", "notes (case 2).txt"}, // first one keeps the name
		{"NOTES.txt", "NOTES (case 3).txt"},
		{"notes.txt", "notes (case 2).txt"}, // assigning again is stable
		{"docs", "docs (case 2)"},           // folders clash with folders too
		{"docs/x.txt", "docs (case 2)/x.txt"},
		{"a:b", "a：b"},
		{"a：b", "a：b (case 2)"}, // a real full-width name clashing with a stand-in
		{".bashrc", ".bashrc"},
		{".BASHRC", ".BASHRC (case 2)"},
		{"con.txt", "con＿.txt"},
	}
	for _, s := range steps {
		if got := m.Assign(s.rel); got != s.want {
			t.Errorf("Assign(%q) = %q, want %q", s.rel, got, s.want)
		}
	}
	if got := m.StandIn("Hackathon "); got != "Hackathon␣" {
		t.Errorf("StandIn(folder) = %q", got)
	}
	if got := m.StandIn("Hackathon /plan.pdf"); got != "" {
		t.Errorf("StandIn of a name that needs none = %q, want empty", got)
	}
	if got := m.Local("Hackathon /new.pdf"); got != "Hackathon␣/new.pdf" {
		t.Errorf("Local = %q", got)
	}
}

func TestFound(t *testing.T) {
	// The state DB knows two stand-ins from an earlier sync.
	m := New(map[string]string{
		"Hackathon ":             "Hackathon␣",
		"Hackathon /Paper:1.pdf": "Paper：1.pdf",
	})
	cases := []struct{ dir, local, want string }{
		{"", "Hackathon␣", "Hackathon "},                 // a stand-in maps back to its true name
		{"Hackathon ", "Paper：1.pdf", "Paper:1.pdf"},     // also inside a stand-in folder
		{"Hackathon ", "PAPER：1.PDF", "Paper:1.pdf"},     // Windows reports names in any case
		{"Hackathon ", "Real：Name.pdf", "Real：Name.pdf"}, // no stand-in recorded: the name is the true name
		{"", "Paper：1.pdf", "Paper：1.pdf"},               // the same stand-in in another folder is unrelated
	}
	for _, c := range cases {
		if got := m.Found(c.dir, c.local); got != c.want {
			t.Errorf("Found(%q, %q) = %q, want %q", c.dir, c.local, got, c.want)
		}
	}
	// A Drive file whose name is taken by a found local file gets a suffix.
	if got := m.Assign("Hackathon /real：name.pdf"); got != "Hackathon␣/real：name (case 2).pdf" {
		t.Errorf("Assign after Found = %q", got)
	}
	// A Drive file that is the found file keeps its name.
	if got := m.Assign("Hackathon /Real：Name.pdf"); got != "Hackathon␣/Real：Name.pdf" {
		t.Errorf("Assign of a found file = %q", got)
	}
}

func TestAssignReusesFoundName(t *testing.T) {
	// An entry the scan found under its own true name keeps it, even if
	// Windows would need a stand-in (only possible in tests on Linux).
	m := New(nil)
	m.Found("", "Hackathon ")
	if got := m.Assign("Hackathon "); got != "Hackathon " {
		t.Errorf("Assign = %q, want the found name", got)
	}
}

func TestClone(t *testing.T) {
	m := New(nil)
	m.Assign("Notes.txt")
	c := m.Clone()
	if got := c.Assign("notes.txt"); got != "notes (case 2).txt" {
		t.Errorf("clone forgot taken names: %q", got)
	}
	if m.StandIn("notes.txt") != "" {
		t.Error("assigning in the clone changed the original")
	}
}

func TestNilMap(t *testing.T) {
	var m *Map
	if m.Assign("a:b") != "a:b" || m.Local("x /y") != "x /y" || m.Found("", "a:b") != "a:b" || m.StandIn("a:b") != "" || m.Clone() != nil {
		t.Error("a nil Map must map every name to itself")
	}
}
