package gdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"

	"github.com/USER1043/cloudsync/internal/filter"
)

func TestRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&googleapi.Error{Code: http.StatusTooManyRequests}, true},
		{&googleapi.Error{Code: http.StatusServiceUnavailable}, true},
		{&googleapi.Error{Code: http.StatusForbidden, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}}, true},
		{&googleapi.Error{Code: http.StatusForbidden, Errors: []googleapi.ErrorItem{{Reason: "insufficientPermissions"}}}, false},
		{&googleapi.Error{Code: http.StatusNotFound}, false},
		{fmt.Errorf("wrapped: %w", &googleapi.Error{Code: http.StatusBadGateway}), true},
		{context.Canceled, false},
		{errors.New("file is open in another program"), false},
	}
	for _, c := range cases {
		if got := retryable(c.err); got != c.want {
			t.Errorf("retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestRetryStopsOnPermanentError(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), func() error {
		calls++
		return &googleapi.Error{Code: http.StatusNotFound}
	})
	if err == nil || calls != 1 {
		t.Errorf("calls = %d, err = %v; want one call and the error", calls, err)
	}
}

func TestRetryRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for backoff")
	}
	calls := 0
	err := Retry(context.Background(), func() error {
		calls++
		if calls == 1 {
			return &googleapi.Error{Code: http.StatusServiceUnavailable}
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Errorf("calls = %d, err = %v; want success on the second call", calls, err)
	}
}

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

func TestCountingTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	defer srv.Close()
	hc := &http.Client{Transport: countingTransport{base: http.DefaultTransport}}

	send := func(ctx context.Context, contentRange string) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, srv.URL, strings.NewReader("hello world"))
		if contentRange != "" {
			req.Header.Set("Content-Range", contentRange)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	var last int64
	ctx := withProgress(context.Background(), func(n int64) { last = n })
	send(ctx, "")
	if last != 11 {
		t.Errorf("single request: progress = %d, want 11", last)
	}
	send(ctx, "bytes 100-110/200")
	if last != 111 {
		t.Errorf("chunk at offset 100: progress = %d, want 111", last)
	}

	last = -1
	send(context.Background(), "") // no callback: nothing reported
	if last != -1 {
		t.Errorf("request without a callback reported %d", last)
	}
}

func TestRangeStart(t *testing.T) {
	cases := map[string]int64{"": 0, "bytes 0-8388607/20971520": 0, "bytes 8388608-16777215/20971520": 8388608, "bytes */20971520": 0}
	for h, want := range cases {
		if got := rangeStart(h); got != want {
			t.Errorf("rangeStart(%q) = %d, want %d", h, got, want)
		}
	}
}

func TestTreeFilter(t *testing.T) {
	tree := &Tree{
		Folders: []File{{Rel: "A/B"}, {Rel: "A"}, {Rel: "C"}},
		Files: []File{
			{Rel: "A/B/x.pdf", Size: 1}, {Rel: "C/disk.iso", Size: 1},
			{Rel: "huge.bin", Size: 5000}, {Rel: "ok.txt", Size: 1},
		},
		Native: []File{{Rel: "A/doc"}, {Rel: "C/sheet"}},
	}
	m, err := filter.New([]string{"/A/", "*.iso"})
	if err != nil {
		t.Fatal(err)
	}
	m.SetMaxSize(1000)

	got, skipped := tree.Filter(m)
	rels := func(fs []File) (out []string) {
		for _, f := range fs {
			out = append(out, f.Rel)
		}
		return out
	}
	if fmt.Sprint(rels(got.Folders)) != "[C]" || fmt.Sprint(rels(got.Files)) != "[ok.txt]" || fmt.Sprint(rels(got.Native)) != "[C/sheet]" {
		t.Errorf("kept folders %v, files %v, native %v", rels(got.Folders), rels(got.Files), rels(got.Native))
	}
	if fmt.Sprint(skipped) != "[A/ C/disk.iso huge.bin]" {
		t.Errorf("skipped = %v", skipped)
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
