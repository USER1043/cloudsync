// Package gdrive wraps the Google Drive v3 calls CloudSync needs.
package gdrive

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"github.com/USER1043/cloudsync/internal/filter"
)

const (
	folderMime       = "application/vnd.google-apps.folder"
	googleAppsPrefix = "application/vnd.google-apps."
)

// Client talks to the signed-in user's Google Drive.
type Client struct{ srv *drive.Service }

// New creates a Client that sends requests through hc, which must add the
// OAuth token (see auth.Client).
func New(ctx context.Context, hc *http.Client) (*Client, error) {
	srv, err := drive.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("creating Drive client: %w", err)
	}
	return &Client{srv: srv}, nil
}

// Account is the signed-in user and their storage quota. Limit is 0 when
// the account has unlimited storage.
type Account struct {
	Name, Email  string
	Limit, Usage int64
}

// Account returns who is signed in and how much storage they have.
func (c *Client) Account(ctx context.Context) (Account, error) {
	a, err := c.srv.About.Get().
		Fields("user(displayName,emailAddress),storageQuota(limit,usage)").
		Context(ctx).Do()
	if err != nil {
		return Account{}, fmt.Errorf("reading Drive account: %w", err)
	}
	var acc Account
	if a.User != nil {
		acc.Name, acc.Email = a.User.DisplayName, a.User.EmailAddress
	}
	if a.StorageQuota != nil {
		acc.Limit, acc.Usage = a.StorageQuota.Limit, a.StorageQuota.Usage
	}
	return acc, nil
}

// FindFolder resolves folder names under My Drive to a folder ID. It
// returns "" if any folder along the path does not exist yet.
func (c *Client) FindFolder(ctx context.Context, segments []string) (string, error) {
	parent := "root"
	for _, name := range segments {
		q := fmt.Sprintf("name = '%s' and '%s' in parents and mimeType = '%s' and trashed = false",
			escape(name), parent, folderMime)
		r, err := c.srv.Files.List().Q(q).Spaces("drive").Fields("files(id)").PageSize(2).Context(ctx).Do()
		if err != nil {
			return "", fmt.Errorf("looking up Drive folder %q: %w", name, err)
		}
		switch len(r.Files) {
		case 0:
			return "", nil
		case 1:
			parent = r.Files[0].Id
		default:
			return "", fmt.Errorf("Drive has more than one folder named %q in the same place; rename one so the path is unambiguous", name)
		}
	}
	return parent, nil
}

// File is a file stored in Drive.
type File struct {
	ID       string
	Rel      string // slash-separated path relative to the root folder
	MimeType string
	MD5      string
	Size     int64
}

// Tree is the contents of a Drive folder, recursively.
type Tree struct {
	Files  []File         // regular files CloudSync can sync
	Native []File         // Google Docs, Sheets, Slides and shortcuts, which have no file to download
	Issues []filter.Issue // names that cannot exist on Windows, or that clash by case
}

// ListTree lists every non-trashed item under folderID.
func (c *Client) ListTree(ctx context.Context, folderID string) (*Tree, error) {
	type dir struct{ id, rel string }
	t := &Tree{}
	queue := []dir{{id: folderID}}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		seen := make(map[string]string) // lower-cased name -> name
		q := fmt.Sprintf("'%s' in parents and trashed = false", d.id)
		err := c.srv.Files.List().Q(q).Spaces("drive").PageSize(1000).
			Fields("nextPageToken, files(id, name, mimeType, size, md5Checksum)").
			Pages(ctx, func(l *drive.FileList) error {
				for _, f := range l.Files {
					rel := joinRel(d.rel, f.Name)
					if reason := filter.CheckName(f.Name); reason != "" {
						t.Issues = append(t.Issues, filter.Issue{Rel: rel, Reason: reason})
						continue
					}
					lower := strings.ToLower(f.Name)
					if other, ok := seen[lower]; ok {
						t.Issues = append(t.Issues, filter.Issue{Rel: rel,
							Reason: fmt.Sprintf("same name as %q in this Drive folder (ignoring case)", other)})
						continue
					}
					seen[lower] = f.Name

					file := File{ID: f.Id, Rel: rel, MimeType: f.MimeType, MD5: f.Md5Checksum, Size: f.Size}
					switch {
					case f.MimeType == folderMime:
						queue = append(queue, dir{id: f.Id, rel: rel})
					case strings.HasPrefix(f.MimeType, googleAppsPrefix):
						t.Native = append(t.Native, file)
					default:
						t.Files = append(t.Files, file)
					}
				}
				return nil
			})
		if err != nil {
			return nil, fmt.Errorf("listing Drive folder %q: %w", "/"+d.rel, err)
		}
	}
	return t, nil
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// escape quotes a value for a Drive query string literal.
func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}
