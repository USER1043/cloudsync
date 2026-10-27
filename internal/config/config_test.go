package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalPathFor(t *testing.T) {
	r := Root{Name: "MyFiles", Windows: `C:\MyFiles`, Linux: "~/My_Files"}
	home := filepath.Join("home", "u")

	got, err := localPathFor("linux", home, r)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "My_Files"); got != want {
		t.Errorf("linux path = %q, want %q", got, want)
	}

	got, err = localPathFor("windows", home, r)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Clean(`C:\MyFiles`); got != want {
		t.Errorf("windows path = %q, want %q", got, want)
	}

	if _, err := localPathFor("linux", home, Root{Name: "x", Windows: `C:\x`}); err == nil {
		t.Error("expected an error for a root with no linux path")
	}
}

func TestLoadMissingUsesDefaults(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Path() != "" || len(c.Roots) != 1 || c.Roots[0].Remote != "CloudSync/MyFiles" {
		t.Errorf("got %+v, want built-in defaults", c)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	body := `
ignore = ["*.iso"]

[[root]]
name    = "College"
remote  = "/CloudSync/College/"
windows = 'D:\College'
linux   = "~/College"
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Roots) != 1 || c.Roots[0].Name != "College" || len(c.Ignore) != 1 {
		t.Fatalf("unexpected config %+v", c)
	}
	if got := strings.Join(c.Roots[0].RemoteSegments(), "|"); got != "CloudSync|College" {
		t.Errorf("RemoteSegments = %q", got)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]Config{
		"no roots":       {},
		"duplicate name": {Roots: []Root{{Name: "a", Remote: "x"}, {Name: "a", Remote: "y"}}},
		"backslash":      {Roots: []Root{{Name: "a", Remote: `CloudSync\a`}}},
		"whole drive":    {Roots: []Root{{Name: "a", Remote: "/"}}},
	}
	for name, c := range cases {
		if err := c.validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
