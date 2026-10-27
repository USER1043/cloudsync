package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathFor(t *testing.T) {
	legacy := Root{Name: "MyFiles", Windows: `C:\MyFiles`, Linux: "~/My_Files"}
	if got := pathFor("linux", legacy); got != "~/My_Files" {
		t.Errorf("legacy linux path = %q", got)
	}
	if got := pathFor("windows", legacy); got != `C:\MyFiles` {
		t.Errorf("legacy windows path = %q", got)
	}
	both := legacy
	both.Path = "/data/files"
	if got := pathFor("linux", both); got != "/data/files" {
		t.Errorf("path should win over the per-OS fields, got %q", got)
	}
	if got := pathFor("linux", Root{Windows: `C:\x`}); got != "" {
		t.Errorf("no linux path: got %q", got)
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExpandPath("~/My_Files")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "My_Files"); got != want {
		t.Errorf("ExpandPath(~/My_Files) = %q, want %q", got, want)
	}
	if got, _ := ExpandPath("relative"); !filepath.IsAbs(got) {
		t.Errorf("relative path was not made absolute: %q", got)
	}
}

func TestLoadMissing(t *testing.T) {
	t.Setenv("CLOUDSYNC_CONFIG", "")
	if _, err := Load(t.TempDir()); !errors.Is(err, ErrNoConfig) {
		t.Errorf("err = %v, want ErrNoConfig", err)
	}
}

func TestLoadFile(t *testing.T) {
	t.Setenv("CLOUDSYNC_CONFIG", "")
	dir := t.TempDir()
	body := `
ignore = ["*.iso"]
max_file_size = "1.5 GB"

[[root]]
name   = "College"
remote = "/CloudSync/College/"
path   = 'D:\College'
`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Roots) != 1 || c.Roots[0].Path != `D:\College` || len(c.Ignore) != 1 {
		t.Fatalf("unexpected config %+v", c)
	}
	if got := strings.Join(c.Roots[0].RemoteSegments(), "|"); got != "CloudSync|College" {
		t.Errorf("RemoteSegments = %q", got)
	}
	if c.MaxBytes() != 3<<29 {
		t.Errorf("MaxBytes = %d, want 1.5 GB", c.MaxBytes())
	}
}

func TestWriteRoundTrip(t *testing.T) {
	t.Setenv("CLOUDSYNC_CONFIG", "")
	dir := t.TempDir()
	in := Config{
		Ignore:      []string{"*.iso", `/College/Old/`, "it's.tmp"},
		MaxFileSize: "500MB",
		Roots:       []Root{{Name: "MyFiles", Remote: "CloudSync/MyFiles", Path: `C:\Bob's Files\My "Docs"`}},
	}
	p, err := Write(dir, in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Load(dir)
	if err != nil {
		b, _ := os.ReadFile(p)
		t.Fatalf("loading what Write wrote: %v\n%s", err, b)
	}
	if out.Roots[0] != in.Roots[0] || strings.Join(out.Ignore, "|") != strings.Join(in.Ignore, "|") || out.MaxBytes() != 500<<20 {
		t.Errorf("round trip changed the config:\n in  %+v\n out %+v", in, out)
	}

	// Without a size limit the example stays commented out.
	in.MaxFileSize, in.Ignore = "", nil
	if _, err := Write(dir, in); err != nil {
		t.Fatal(err)
	}
	if out, err = Load(dir); err != nil || out.MaxBytes() != 0 || len(out.Ignore) != 0 {
		t.Errorf("config without limits: %+v, %v", out, err)
	}
}

func TestParseSize(t *testing.T) {
	good := map[string]int64{"": 0, "2048": 2048, "500MB": 500 << 20, "500 mb": 500 << 20, "1.5GB": 3 << 29, "10kb": 10 << 10, "1TB": 1 << 40, "7B": 7}
	for in, want := range good {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"10XB", "-1MB", "MB", "0", "lots"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) succeeded, want error", in)
		}
	}
}

func TestConfigEnvOverride(t *testing.T) {
	t.Setenv("CLOUDSYNC_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	if _, err := Load(t.TempDir()); !errors.Is(err, ErrNoConfig) {
		t.Errorf("err = %v, want ErrNoConfig", err)
	}
}

func TestStateDirOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "state")
	t.Setenv("CLOUDSYNC_STATE_DIR", want)
	if got, err := StateDir(); err != nil || got != want {
		t.Errorf("StateDir = %q, %v; want %q", got, err, want)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]Config{
		"no roots":       {},
		"duplicate name": {Roots: []Root{{Name: "a", Remote: "x", Path: "p"}, {Name: "a", Remote: "y", Path: "p"}}},
		"backslash":      {Roots: []Root{{Name: "a", Remote: `CloudSync\a`, Path: "p"}}},
		"whole drive":    {Roots: []Root{{Name: "a", Remote: "/", Path: "p"}}},
		"no path":        {Roots: []Root{{Name: "a", Remote: "x"}}},
		"bad size":       {MaxFileSize: "big", Roots: []Root{{Name: "a", Remote: "x", Path: "p"}}},
	}
	for name, c := range cases {
		if err := c.validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
