package gdrive

import "testing"

func TestEscape(t *testing.T) {
	cases := map[string]string{
		"MyFiles":      "MyFiles",
		"Prajan's":     `Prajan\'s`,
		`back\slash`:   `back\\slash`,
		`it's \ mixed`: `it\'s \\ mixed`,
	}
	for in, want := range cases {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJoinRel(t *testing.T) {
	if got := joinRel("", "a.pdf"); got != "a.pdf" {
		t.Errorf("joinRel at root = %q", got)
	}
	if got := joinRel("Sem5/DBMS", "a.pdf"); got != "Sem5/DBMS/a.pdf" {
		t.Errorf("joinRel nested = %q", got)
	}
}
