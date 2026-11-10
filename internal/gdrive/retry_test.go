package gdrive

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"

	"golang.org/x/oauth2"
)

func TestSignInRejected(t *testing.T) {
	// The oauth2 transport wraps a refused refresh in a *url.Error, which is
	// also a net.Error; it must not look like a network outage.
	revoked := &url.Error{Op: "Get", URL: "https://www.googleapis.com/drive/v3/files",
		Err: &oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, ErrorCode: "invalid_grant"}}
	offline := &url.Error{Op: "Get", URL: "https://www.googleapis.com/drive/v3/files",
		Err: &net.DNSError{Err: "no such host", Name: "www.googleapis.com", IsNotFound: true}}
	googleDown := &url.Error{Op: "Post", URL: "https://oauth2.googleapis.com/token",
		Err: &oauth2.RetrieveError{Response: &http.Response{StatusCode: 503}}}

	for _, c := range []struct {
		name                string
		err                 error
		rejected, transient bool
	}{
		{"revoked", revoked, true, false},
		{"offline", offline, false, true},
		{"token server down", googleDown, false, true},
		{"other", errors.New("boom"), false, false},
	} {
		if got := SignInRejected(c.err); got != c.rejected {
			t.Errorf("%s: SignInRejected = %v", c.name, got)
		}
		if got := Temporary(c.err); got != c.transient {
			t.Errorf("%s: Temporary = %v", c.name, got)
		}
	}
}
