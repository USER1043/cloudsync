package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

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

// folderServer is a fake Drive API that holds folders. With loseReply set,
// the first create goes through but answers 503, like a reply lost on the way.
type folderServer struct {
	mu             sync.Mutex
	names          []string
	lists, creates int
	loseReply      bool
}

func (s *folderServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		s.lists++
		var files []string
		for i, n := range s.names {
			files = append(files, fmt.Sprintf(`{"id":"f%d","name":%q,"mimeType":%q}`, i, n, folderMime))
		}
		fmt.Fprintf(w, `{"files":[%s]}`, strings.Join(files, ","))
	case http.MethodPost:
		s.creates++
		var f struct{ Name string }
		json.NewDecoder(r.Body).Decode(&f)
		s.names = append(s.names, f.Name)
		if s.loseReply {
			s.loseReply = false
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":503,"message":"backend error"}}`)
			return
		}
		fmt.Fprintf(w, `{"id":"f%d"}`, len(s.names)-1)
	}
}

func folderClient(t *testing.T, s *folderServer) *Client {
	t.Helper()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	srv, err := drive.NewService(context.Background(), option.WithHTTPClient(ts.Client()), option.WithEndpoint(ts.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	return &Client{srv: srv}
}

func TestCreateFolderSkipsLookup(t *testing.T) {
	ctx := context.Background()
	s := &folderServer{}
	c := folderClient(t, s)
	if id, err := c.CreateFolder(ctx, "p", "Sem5"); err != nil || id != "f0" {
		t.Fatalf("CreateFolder = %q, %v", id, err)
	}
	if s.lists != 0 || s.creates != 1 {
		t.Errorf("CreateFolder made %d lookups and %d creates, want 0 and 1", s.lists, s.creates)
	}
	if id, err := c.EnsureFolder(ctx, "p", "Sem5"); err != nil || id != "f0" || s.lists != 1 || s.creates != 1 {
		t.Errorf("EnsureFolder of an existing folder = %q, %v with %d lookups, %d creates; want f0 found by one lookup",
			id, err, s.lists, s.creates)
	}
}

func TestCreateFolderLostReply(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for backoff")
	}
	s := &folderServer{loseReply: true}
	c := folderClient(t, s)
	id, err := c.CreateFolder(context.Background(), "p", "Sem5")
	if err != nil || id != "f0" {
		t.Fatalf("CreateFolder = %q, %v; want the folder the lost reply was for", id, err)
	}
	if s.creates != 1 || len(s.names) != 1 {
		t.Errorf("Drive has %v after %d creates; the retry made a duplicate", s.names, s.creates)
	}
}

// fileServer is a fake Drive API holding one file, "x". Updates set its
// checksum to newMD5; gone makes every request answer 404.
type fileServer struct {
	mu              sync.Mutex
	md5             string
	newMD5          string
	trashed, gone   bool
	updates, trashs int
}

func (s *fileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if s.gone {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":404,"message":"File not found: x."}}`)
		return
	}
	switch {
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/upload/"):
		s.updates++
		s.md5 = s.newMD5
	case r.Method == http.MethodPatch:
		var f struct{ Trashed bool }
		json.NewDecoder(r.Body).Decode(&f)
		s.trashed = s.trashed || f.Trashed
		s.trashs++
	}
	fmt.Fprintf(w, `{"id":"x","name":"a.txt","md5Checksum":%q,"trashed":%v}`, s.md5, s.trashed)
}

func fileClient(t *testing.T, s *fileServer) *Client {
	t.Helper()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	srv, err := drive.NewService(context.Background(), option.WithHTTPClient(ts.Client()), option.WithEndpoint(ts.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	return &Client{srv: srv}
}

func TestUpdate(t *testing.T) {
	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("edited here"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	update := func(s *fileServer) (File, error) {
		return fileClient(t, s).Update(ctx, "x", local, time.Now(), "new", "old", nil)
	}

	t.Run("unchanged in Drive", func(t *testing.T) {
		s := &fileServer{md5: "old", newMD5: "new"}
		f, err := update(s)
		if err != nil || s.updates != 1 || f.MD5 != "new" || f.ID != "x" {
			t.Errorf("Update = %+v, %v after %d updates; want one update", f, err, s.updates)
		}
	})
	t.Run("already updated by an earlier attempt", func(t *testing.T) {
		s := &fileServer{md5: "new"}
		if _, err := update(s); err != nil || s.updates != 0 {
			t.Errorf("err = %v after %d updates; want done without uploading", err, s.updates)
		}
	})
	t.Run("edited elsewhere", func(t *testing.T) {
		s := &fileServer{md5: "phone edit", newMD5: "new"}
		f, err := update(s)
		if !errors.Is(err, ErrChanged) || s.updates != 0 || f.MD5 != "phone edit" {
			t.Errorf("Update = %+v, %v after %d updates; want ErrChanged and Drive's copy untouched", f, err, s.updates)
		}
	})
	t.Run("trashed elsewhere", func(t *testing.T) {
		s := &fileServer{md5: "old", trashed: true}
		f, err := update(s)
		if !errors.Is(err, ErrChanged) || !f.Trashed || s.updates != 0 {
			t.Errorf("Update = %+v, %v; want ErrChanged with Trashed", f, err)
		}
	})
	t.Run("deleted elsewhere", func(t *testing.T) {
		f, err := update(&fileServer{gone: true})
		if !errors.Is(err, ErrChanged) || !f.Trashed {
			t.Errorf("Update = %+v, %v; want ErrChanged with Trashed", f, err)
		}
	})
}

func TestTrash(t *testing.T) {
	ctx := context.Background()
	s := &fileServer{}
	if err := fileClient(t, s).Trash(ctx, "x"); err != nil || !s.trashed {
		t.Errorf("Trash: err = %v, trashed = %v", err, s.trashed)
	}
	if err := fileClient(t, &fileServer{gone: true}).Trash(ctx, "x"); err != nil {
		t.Errorf("Trash of a file that is already gone: %v", err)
	}
}
