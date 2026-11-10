package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

func TestDescribe(t *testing.T) {
	get := func(err error) error {
		return fmt.Errorf("listing Drive folder %q: %w", "/docs", &url.Error{Op: "Get", URL: "https://www.googleapis.com/drive/v3/files?q=long", Err: err})
	}
	for _, c := range []struct {
		name string
		err  error
		kind errKind
		want string // part of the description
		not  string // must not appear
	}{
		{"no DNS", get(&net.DNSError{Err: "no such host", Name: "www.googleapis.com", IsNotFound: true}),
			kindOffline, "no internet connection (can't look up www.googleapis.com)", "?q=long"},
		{"refused", get(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}),
			kindOffline, "firewall or proxy", "?q=long"},
		{"dropped", fmt.Errorf("uploading: %w", io.ErrUnexpectedEOF), kindOffline, "dropped", ""},
		{"Google 503", fmt.Errorf("x: %w", &googleapi.Error{Code: 503}), kindGoogle, "not your setup", ""},
		{"rate limit", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}},
			kindRateLimit, "slows down", ""},
		{"Drive full (Google)", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "storageQuotaExceeded"}}},
			kindQuota, "Free up space", ""},
		{"Drive full (check)", fmt.Errorf("root: %w", &quotaError{need: 2 << 30, free: 1 << 30}), kindQuota, "only 1.0 GB is free", ""},
		{"sign-in revoked", get(&oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, ErrorCode: "invalid_grant"}),
			kindSignIn, "cloudsync login", ""},
		{"root gone", fmt.Errorf("root %q: %w", "MyFiles", rootMissing("My Drive/CloudSync/MyFiles")), kindRoot, "restore it", ""},
		{"permission", &fs.PathError{Op: "open", Path: "/home/u/My_Files/a.pdf", Err: fs.ErrPermission},
			kindLocal, "permission denied for /home/u/My_Files/a.pdf", ""},
		{"disk full", &fs.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}, kindLocal, "disk is full", ""},
		{"vanished", &fs.PathError{Op: "stat", Path: "/x/b.txt", Err: fs.ErrNotExist}, kindLocal, "disappeared", ""},
		{"unknown", errors.New("something odd"), kindOther, "something odd", ""},
	} {
		if got := classify(c.err); got != c.kind {
			t.Errorf("%s: kind %d, want %d", c.name, got, c.kind)
		}
		d := describe(c.err)
		if !strings.Contains(d, c.want) || (c.not != "" && strings.Contains(d, c.not)) {
			t.Errorf("%s: describe = %q; want it to contain %q", c.name, d, c.want)
		}
	}
}
