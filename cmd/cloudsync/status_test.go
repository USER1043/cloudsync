package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/USER1043/cloudsync/internal/notify"
)

func TestStatusKeeper(t *testing.T) {
	dir := t.TempDir()
	k := newStatusKeeper(dir)
	k.set("in sync", "")
	k.warn(notify.Message{Key: "held:R", Title: "Deletes held", Body: "63 files"})
	k.warn(notify.Message{Key: "conflict:R", Title: "Conflict", Body: "a.txt"})
	k.warn(notify.Message{Key: "held:R", Title: "Deletes held", Body: "70 files"}) // replaces the first
	k.resolve("conflict:R")
	k.set("offline", "no internet connection")

	data, err := os.ReadFile(filepath.Join(dir, statusFile))
	if err != nil {
		t.Fatal(err)
	}
	var st runStatus
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.PID != os.Getpid() || st.State != "offline" || st.Detail != "no internet connection" {
		t.Errorf("status = %+v", st)
	}
	if len(st.Warnings) != 1 || st.Warnings[0].Body != "70 files" {
		t.Errorf("warnings = %+v; want only the newer held-deletes one", st.Warnings)
	}
	if _, err := os.Stat(filepath.Join(dir, statusFile+".tmp")); err == nil {
		t.Error("temporary status file left behind")
	}
}

func TestRunLock(t *testing.T) {
	dir := t.TempDir()
	if runIsRunning(dir) {
		t.Fatal("running before anything locked")
	}
	unlock, err := lockRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !runIsRunning(dir) {
		t.Error("not seen as running while locked")
	}
	if _, err := lockRun(dir); err == nil {
		t.Error("a second run was allowed to start")
	}
	unlock()
	if runIsRunning(dir) {
		t.Error("still running after unlock")
	}
}

func TestPrintRunState(t *testing.T) {
	now := time.Date(2026, 11, 10, 20, 0, 0, 0, time.Local)
	st := runStatus{Started: now.Add(-2 * time.Hour), Since: now.Add(-40 * time.Minute), Updated: now.Add(-time.Minute),
		State: "offline", Detail: "no internet connection (can't look up www.googleapis.com)"}
	var b bytes.Buffer
	printRunState(&b, st, true, now)
	out := b.String()
	for _, want := range []string{"running in the background (since 18:00 (2 hours ago)): offline since 19:20 (40 minutes ago)", "no internet connection"} {
		if !strings.Contains(out, want) {
			t.Errorf("running output %q lacks %q", out, want)
		}
	}
	b.Reset()
	printRunState(&b, st, false, now)
	if out := b.String(); !strings.Contains(out, "not running") || !strings.Contains(out, "cloudsync start") {
		t.Errorf("stopped output = %q", out)
	}
}

func TestWhen(t *testing.T) {
	now := time.Date(2026, 11, 10, 20, 0, 0, 0, time.Local)
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{now.Add(-20 * time.Second), "19:59 (just now)"},
		{now.Add(-5 * time.Minute), "19:55 (5 minutes ago)"},
		{now.Add(-time.Hour), "19:00 (1 hour ago)"},
		{now.Add(-26 * time.Hour), "9 Nov 18:00 (26 hours ago)"},
		{now.Add(-72 * time.Hour), "7 Nov 20:00 (3 days ago)"},
	} {
		if got := when(c.t, now); got != c.want {
			t.Errorf("when(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}

func TestTroubleWaitsBeforeTelling(t *testing.T) {
	// No notifier: alerts still land in the status file's warnings.
	dm := &daemon{st: newStatusKeeper(t.TempDir())}
	offline := &url.Error{Op: "Get", URL: "https://www.googleapis.com/x", Err: &net.DNSError{Err: "no such host", Name: "www.googleapis.com"}}
	keys := func() []string {
		var k []string
		for _, w := range dm.st.st.Warnings {
			k = append(k, w.Key)
		}
		return k
	}

	dm.trouble(offline)
	if dm.st.st.State != "offline" || len(keys()) != 0 {
		t.Fatalf("after a first failure: state %q, warnings %q; want offline and quiet", dm.st.st.State, keys())
	}
	dm.offlineSince = time.Now().Add(-offlineNotice - time.Minute)
	dm.trouble(offline)
	if got := keys(); len(got) != 1 || got[0] != "offline" {
		t.Fatalf("after %s offline: warnings %q", offlineNotice, got)
	}

	google := &googleapi.Error{Code: 503}
	dm.trouble(google)
	if dm.st.st.State != "waiting for Google Drive" || len(keys()) != 1 {
		t.Errorf("first Google failure: state %q, warnings %q", dm.st.st.State, keys())
	}
	dm.troubleSince = time.Now().Add(-troubleNotice)
	dm.trouble(google)
	if got := keys(); len(got) != 2 || got[0] != "trouble" {
		t.Errorf("after %s of Google failures: warnings %q", troubleNotice, got)
	}

	// Drive full needs the owner at once.
	dm.trouble(&quotaError{need: 2 << 30, free: 1 << 20})
	if got := keys(); got[0] != "quota" {
		t.Errorf("Drive full: warnings %q", got)
	}

	dm.recovered()
	if got := keys(); len(got) != 0 || dm.st.st.State != "in sync" || !dm.offlineSince.IsZero() {
		t.Errorf("after recovering: state %q, warnings %q", dm.st.st.State, got)
	}
}
