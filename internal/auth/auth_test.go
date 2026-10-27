package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

type memStore struct{ saved []*oauth2.Token }

func (m *memStore) Load() (*oauth2.Token, error) {
	if len(m.saved) == 0 {
		return nil, ErrNotLoggedIn
	}
	return m.saved[len(m.saved)-1], nil
}
func (m *memStore) Save(t *oauth2.Token) error { m.saved = append(m.saved, t); return nil }
func (m *memStore) Delete() error              { m.saved = nil; return nil }

type seqSource struct {
	toks []*oauth2.Token
	i    int
}

func (s *seqSource) Token() (*oauth2.Token, error) {
	t := s.toks[s.i]
	if s.i < len(s.toks)-1 {
		s.i++
	}
	return t, nil
}

func TestSavingSourceSavesOnlyNewTokens(t *testing.T) {
	store := &memStore{}
	a := &oauth2.Token{AccessToken: "a"}
	b := &oauth2.Token{AccessToken: "b"}
	src := &savingSource{base: &seqSource{toks: []*oauth2.Token{a, b, b}}, store: store, last: "a"}
	for range 3 {
		if _, err := src.Token(); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.saved) != 1 || store.saved[0].AccessToken != "b" {
		t.Errorf("saved %v, want exactly the refreshed token b", store.saved)
	}
}

// tokenServer stands in for Google's token endpoint and checks that PKCE
// and the authorization code arrive.
func tokenServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_in":3600}`)
	}))
}

func testConfig(tokenURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: "id",
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/auth", TokenURL: tokenURL, AuthStyle: oauth2.AuthStyleInParams},
		Scopes:   []string{DriveScope},
	}
}

// fakeBrowser plays Google's consent page: it reads the redirect URI and
// state from the auth URL and calls back with a code.
func fakeBrowser(t *testing.T, tamper func(q url.Values)) func(string) error {
	return func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("access_type") != "offline" {
			t.Errorf("auth URL missing PKCE or offline access: %s", authURL)
		}
		cb := url.Values{"code": {"the-code"}, "state": {q.Get("state")}}
		if tamper != nil {
			tamper(cb)
		}
		resp, err := http.Get(q.Get("redirect_uri") + "?" + cb.Encode())
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
}

func TestLogin(t *testing.T) {
	ts := tokenServer(t)
	defer ts.Close()
	tok, err := Login(context.Background(), testConfig(ts.URL), fakeBrowser(t, nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" {
		t.Errorf("got token %+v", tok)
	}
}

func TestLoginRejectsWrongState(t *testing.T) {
	ts := tokenServer(t)
	defer ts.Close()
	open := fakeBrowser(t, func(q url.Values) { q.Set("state", "forged") })
	_, err := Login(context.Background(), testConfig(ts.URL), open, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Errorf("err = %v, want a state mismatch", err)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadConfig(dir); !errors.Is(err, ErrNoClientConfig) {
		t.Errorf("missing file: err = %v, want ErrNoClientConfig", err)
	}
	body := `{"installed":{"client_id":"id.apps.googleusercontent.com","client_secret":"s",
"auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token",
"redirect_uris":["http://localhost"]}}`
	if err := os.WriteFile(filepath.Join(dir, clientFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientID != "id.apps.googleusercontent.com" || cfg.Scopes[0] != DriveScope {
		t.Errorf("unexpected config %+v", cfg)
	}
}

func TestLogoutRevokesAndDeletes(t *testing.T) {
	var revoked string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		revoked = r.Form.Get("token")
	}))
	defer ts.Close()
	old := revokeURL
	revokeURL = ts.URL
	defer func() { revokeURL = old }()

	store := &memStore{saved: []*oauth2.Token{{AccessToken: "at", RefreshToken: "rt"}}}
	if err := Logout(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if revoked != "rt" {
		t.Errorf("revoked %q, want the refresh token", revoked)
	}
	if len(store.saved) != 0 {
		t.Error("token still stored after logout")
	}
	if err := Logout(context.Background(), &memStore{}); err != nil {
		t.Errorf("logout when signed out: %v", err)
	}
}
