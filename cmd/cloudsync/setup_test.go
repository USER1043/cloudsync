package main

import (
	"bytes"
	"debug/pe"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlaceKeepsScratchApart(t *testing.T) {
	t.Setenv("CLOUDSYNC_CONFIG", "")
	t.Setenv("CLOUDSYNC_STATE_DIR", "")
	real, err := place()
	if err != nil {
		t.Fatal(err)
	}
	if real.name != "cloudsync" || len(real.env) != 0 {
		t.Errorf("usual config: %+v", real)
	}

	dir := t.TempDir()
	t.Setenv("CLOUDSYNC_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("CLOUDSYNC_STATE_DIR", filepath.Join(dir, "state"))
	scratch, err := place()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(scratch.name, "cloudsync-") || scratch.exe == real.exe ||
		!strings.HasPrefix(scratch.exe, filepath.Join(dir, "state")) {
		t.Errorf("scratch config shares the real setup: %+v vs %+v", scratch, real)
	}
	if scratch.env["CLOUDSYNC_CONFIG"] != filepath.Join(dir, "config.toml") {
		t.Errorf("scratch env = %v", scratch.env)
	}
	again, _ := place()
	if again.name != scratch.name {
		t.Errorf("name not stable: %q then %q", scratch.name, again.name)
	}
}

func TestInstallAndRestore(t *testing.T) {
	dir := t.TempDir()
	p := placement{name: "cloudsync-test", exe: filepath.Join(dir, "bin", exeName()), stateDir: dir}
	oldBuild := filepath.Join(dir, "old")
	newBuild := filepath.Join(dir, "new")
	os.WriteFile(oldBuild, []byte("old"), 0o755)
	os.WriteFile(newBuild, []byte("new"), 0o755)
	if exeName() != "cloudsync" {
		t.Skip("Windows programs are checked by TestMakeWindowless")
	}
	if err := installProgram(p, oldBuild); err != nil {
		t.Fatal(err)
	}
	if err := installProgram(p, newBuild); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p.exe); string(b) != "new" {
		t.Errorf("installed %q, want new", b)
	}
	if b, _ := os.ReadFile(p.exe + ".previous"); string(b) != "old" {
		t.Errorf("previous %q, want old", b)
	}
	if err := restorePrevious(p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p.exe); string(b) != "old" {
		t.Errorf("after restore %q, want old", b)
	}
	if st, _ := os.Stat(p.exe); st.Mode().Perm()&0o100 == 0 {
		t.Error("installed program isn't executable")
	}
}

// TestMakeWindowless builds a small Windows program and checks that only
// its subsystem changes.
func TestMakeWindowless(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a program")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module hello\n"), 0o644)
	src := filepath.Join(dir, "hello.exe")
	cmd := exec.Command(goTool, "build", "-o", src, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("can't cross-build: %v\n%s", err, out)
	}
	dst := filepath.Join(dir, "hello-background.exe")
	if err := makeWindowless(src, dst); err != nil {
		t.Fatal(err)
	}
	subsystem := func(path string) uint16 {
		f, err := pe.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		return f.OptionalHeader.(*pe.OptionalHeader64).Subsystem
	}
	if got := subsystem(src); got != subsystemConsole {
		t.Fatalf("the build is subsystem %d, want console", got)
	}
	if got := subsystem(dst); got != subsystemGUI {
		t.Errorf("the copy is subsystem %d, want GUI", got)
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	diff := 0
	for i := range a {
		if a[i] != b[i] {
			diff++
		}
	}
	if len(a) != len(b) || diff != 1 {
		t.Errorf("%d bytes differ (sizes %d, %d); want only the subsystem's low byte", diff, len(a), len(b))
	}
	if err := makeWindowless(filepath.Join(dir, "main.go"), filepath.Join(dir, "x.exe")); err == nil {
		t.Error("a non-PE file was accepted")
	}
}

func TestLogFileRotates(t *testing.T) {
	dir := t.TempDir()
	l, err := openLogFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 1<<20)
	for range 6 {
		if _, err := l.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	cur, err := os.Stat(filepath.Join(dir, logFileName))
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(filepath.Join(dir, logFileName+".1"))
	if err != nil {
		t.Fatalf("no rotated log: %v", err)
	}
	if cur.Size() > logMaxBytes || old.Size() > logMaxBytes {
		t.Errorf("sizes %d and %d; each should stay under %d", cur.Size(), old.Size(), logMaxBytes)
	}
}
