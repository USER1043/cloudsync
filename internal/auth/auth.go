// Package auth signs in to Google with OAuth and keeps the token in the
// OS credential store (Windows Credential Manager, or KWallet / GNOME
// Keyring through the Secret Service API on Linux).
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// DriveScope grants full Drive access, so files added from the phone are
// visible too.
const DriveScope = "https://www.googleapis.com/auth/drive"

const clientFile = "client_secret.json"

var (
	// ErrNotLoggedIn means no token is stored yet.
	ErrNotLoggedIn = errors.New("not signed in; run `cloudsync login`")
	// ErrNoClientConfig means client_secret.json is missing.
	ErrNoClientConfig = errors.New("OAuth client not set up")
)

// LoadConfig reads the OAuth Desktop client from client_secret.json in dir.
func LoadConfig(dir string) (*oauth2.Config, error) {
	p := filepath.Join(dir, clientFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: save your Google OAuth Desktop client JSON as %s", ErrNoClientConfig, p)
	}
	if err != nil {
		return nil, err
	}
	cfg, err := google.ConfigFromJSON(b, DriveScope)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	return cfg, nil
}

// TokenStore persists the OAuth token.
type TokenStore interface {
	Load() (*oauth2.Token, error)
	Save(*oauth2.Token) error
	Delete() error
}

const (
	keyringService = "cloudsync"
	keyringUser    = "google-token"
)

// KeyringStore keeps the token in the OS credential store.
type KeyringStore struct{}

func (KeyringStore) Load() (*oauth2.Token, error) {
	s, err := keyring.Get(keyringService, keyringUser)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNotLoggedIn
	}
	if err != nil {
		return nil, fmt.Errorf("reading token from the credential store: %w", err)
	}
	var t oauth2.Token
	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return nil, fmt.Errorf("stored token is corrupt (run `cloudsync login` again): %w", err)
	}
	return &t, nil
}

func (KeyringStore) Save(t *oauth2.Token) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if err := keyring.Set(keyringService, keyringUser, string(b)); err != nil {
		return fmt.Errorf("saving token to the credential store: %w", err)
	}
	return nil
}

func (KeyringStore) Delete() error {
	if err := keyring.Delete(keyringService, keyringUser); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

const successPage = `<!doctype html><meta charset="utf-8"><title>CloudSync</title>
<body style="font-family:system-ui,sans-serif;padding:48px 16px;text-align:center">
<h1>Signed in to CloudSync</h1><p>You can close this tab and return to the terminal.</p></body>`

// Login runs the OAuth loopback flow with PKCE. It opens Google's consent
// page with open and waits up to five minutes for Google to redirect back
// to a one-off local port.
func Login(ctx context.Context, cfg *oauth2.Config, open func(url string) error, out io.Writer) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("starting local sign-in listener: %w", err)
	}
	defer ln.Close()

	c := *cfg
	c.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	state, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	authURL := c.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent"), // always return a refresh token
	)

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			var res result
			switch {
			case q.Get("state") != state:
				res.err = errors.New("sign-in response had an unexpected state value; try again")
			case q.Get("error") != "":
				res.err = fmt.Errorf("Google refused sign-in: %s", q.Get("error"))
			case q.Get("code") == "":
				res.err = errors.New("sign-in response had no authorization code")
			default:
				res.code = q.Get("code")
			}
			if res.err != nil {
				http.Error(w, "CloudSync sign-in failed: "+res.err.Error(), http.StatusBadRequest)
			} else {
				io.WriteString(w, successPage)
			}
			select {
			case results <- res:
			default:
			}
		}),
	}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(out, "Opening your browser to sign in to Google.\nIf it does not open, visit this URL:\n\n  %s\n\n", authURL)
	if err := open(authURL); err != nil {
		fmt.Fprintf(out, "(could not open the browser: %v)\n", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for sign-in: %w", ctx.Err())
	case res := <-results:
		if res.err != nil {
			return nil, res.err
		}
		tok, err := c.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("exchanging the sign-in code: %w", err)
		}
		if tok.RefreshToken == "" {
			return nil, errors.New("Google returned no refresh token; remove CloudSync at myaccount.google.com/permissions and sign in again")
		}
		return tok, nil
	}
}

// Client returns an HTTP client that authorizes requests with the stored
// token, refreshing it when it expires and saving refreshed tokens back.
func Client(ctx context.Context, cfg *oauth2.Config, store TokenStore) (*http.Client, error) {
	tok, err := store.Load()
	if err != nil {
		return nil, err
	}
	src := &savingSource{base: cfg.TokenSource(ctx, tok), store: store, last: tok.AccessToken}
	return oauth2.NewClient(ctx, src), nil
}

// savingSource writes each newly refreshed token to the store so the next
// run starts with a valid access token.
type savingSource struct {
	mu    sync.Mutex
	base  oauth2.TokenSource
	store TokenStore
	last  string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err != nil {
		return nil, fmt.Errorf("refreshing Google sign-in (run `cloudsync login` if this persists): %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.AccessToken != s.last {
		if err := s.store.Save(t); err != nil {
			return nil, err
		}
		s.last = t.AccessToken
	}
	return t, nil
}

var revokeURL = "https://oauth2.googleapis.com/revoke" // replaced in tests

// Logout revokes the refresh token with Google and removes it from the
// store. The local token is removed even if revoking fails.
func Logout(ctx context.Context, store TokenStore) error {
	tok, err := store.Load()
	if errors.Is(err, ErrNotLoggedIn) {
		return nil
	}
	var revokeErr error
	if err == nil && tok.RefreshToken != "" {
		revokeErr = revoke(ctx, revokeURL, tok.RefreshToken)
	}
	if err := store.Delete(); err != nil {
		return err
	}
	if revokeErr != nil {
		return fmt.Errorf("signed out on this computer, but revoking with Google failed (remove CloudSync at myaccount.google.com/permissions): %w", revokeErr)
	}
	return nil
}

func revoke(ctx context.Context, endpoint, token string) error {
	body := strings.NewReader(url.Values{"token": {token}}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoke returned %s", resp.Status)
	}
	return nil
}
