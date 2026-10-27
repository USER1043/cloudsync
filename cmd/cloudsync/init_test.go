package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USER1043/cloudsync/internal/config"
)

func answers(lines ...string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

func TestSetupWithFlags(t *testing.T) {
	home := t.TempDir()
	local := filepath.Join(home, "Uni Files")
	root, err := setup(io.Discard, answers(), home, local, `\CloudSync\Uni\`, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Root{Name: "Uni", Remote: "CloudSync/Uni", Path: local}
	if root != want {
		t.Errorf("root = %+v, want %+v", root, want)
	}
	if _, err := os.Stat(local); err != nil {
		t.Error("missing local folder was not created")
	}
}

func TestSetupPicksExistingDriveFolder(t *testing.T) {
	home := t.TempDir()
	local := filepath.Join(home, "My_Files")
	// Empty answers take the defaults; with one folder in Drive that is "1".
	root, err := setup(io.Discard, answers(local, ""), home, "", "", []string{"MyFiles"})
	if err != nil {
		t.Fatal(err)
	}
	if root.Name != "MyFiles" || root.Remote != "CloudSync/MyFiles" || root.Path != local {
		t.Errorf("root = %+v, want it linked to CloudSync/MyFiles", root)
	}

	root, err = setup(io.Discard, answers(local, "2"), home, "", "", []string{"Notes", "Work"})
	if err != nil || root.Remote != "CloudSync/Work" {
		t.Errorf("picking 2 gave %+v, %v", root, err)
	}
}

func TestSetupDefaults(t *testing.T) {
	home := t.TempDir()
	root, err := setup(io.Discard, answers("", ""), home, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != filepath.Join(home, "CloudSync") || root.Remote != "CloudSync/CloudSync" {
		t.Errorf("defaults gave %+v", root)
	}
}

func TestSetupRefusesHugeFolders(t *testing.T) {
	home := t.TempDir()
	for _, local := range []string{home, filepath.VolumeName(home) + string(filepath.Separator)} {
		if _, err := setup(io.Discard, answers(), home, local, "CloudSync/x", nil); err == nil {
			t.Errorf("setup accepted %q", local)
		}
	}
}
