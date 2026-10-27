// Package gdrive wraps the Google Drive v3 calls CloudSync needs.
package gdrive

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/USER1043/cloudsync/internal/filter"
)

const (
	folderMime       = "application/vnd.google-apps.folder"
	googleAppsPrefix = "application/vnd.google-apps."
	fileFields       = "id, name, mimeType, size, md5Checksum, modifiedTime"
	// Files larger than this are uploaded in resumable chunks of this size,
	// so a dropped connection retries one chunk instead of the whole file.
	chunkSize = 8 << 20
)

// Client talks to the signed-in user's Google Drive.
type Client struct{ srv *drive.Service }

// New creates a Client that sends requests through hc, which must add the
// OAuth token (see auth.Client).
func New(ctx context.Context, hc *http.Client) (*Client, error) {
	counted := *hc
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	counted.Transport = countingTransport{base: base}
	srv, err := drive.NewService(ctx, option.WithHTTPClient(&counted))
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
	var a *drive.About
	err := Retry(ctx, func() (err error) {
		a, err = c.srv.About.Get().
			Fields("user(displayName,emailAddress),storageQuota(limit,usage)").
			Context(ctx).Do()
		return err
	})
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

// StartPageToken returns the current position in the Drive change feed.
// Changes made after this point can be listed later from the token.
func (c *Client) StartPageToken(ctx context.Context) (string, error) {
	var tok string
	err := Retry(ctx, func() error {
		r, err := c.srv.Changes.GetStartPageToken().Context(ctx).Do()
		if err != nil {
			return err
		}
		tok = r.StartPageToken
		return nil
	})
	return tok, err
}

// File is a file or folder stored in Drive.
type File struct {
	ID       string
	Rel      string // slash-separated path relative to the root folder
	MimeType string
	MD5      string
	Size     int64
	ModTime  time.Time
}

func toFile(f *drive.File, rel string) File {
	mt, _ := time.Parse(time.RFC3339, f.ModifiedTime)
	return File{ID: f.Id, Rel: rel, MimeType: f.MimeType, MD5: f.Md5Checksum, Size: f.Size, ModTime: mt}
}

// findChild returns the item called name directly inside parentID, or nil.
// It looks only at folders when folder is true and only at files otherwise.
func (c *Client) findChild(ctx context.Context, parentID, name string, folder bool) (*File, error) {
	op := "!="
	if folder {
		op = "="
	}
	q := fmt.Sprintf("name = '%s' and '%s' in parents and mimeType %s '%s' and trashed = false",
		escape(name), parentID, op, folderMime)
	var r *drive.FileList
	err := Retry(ctx, func() (err error) {
		r, err = c.srv.Files.List().Q(q).Spaces("drive").Fields("files(" + fileFields + ")").PageSize(2).Context(ctx).Do()
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("looking up %q in Drive: %w", name, err)
	}
	switch {
	case len(r.Files) == 0:
		return nil, nil
	case folder && len(r.Files) > 1:
		return nil, fmt.Errorf("Drive has more than one folder named %q in the same place; rename one so the path is unambiguous", name)
	}
	f := toFile(r.Files[0], name)
	return &f, nil
}

// FindFolder resolves folder names under My Drive to a folder ID. It
// returns "" if any folder along the path does not exist yet.
func (c *Client) FindFolder(ctx context.Context, segments []string) (string, error) {
	parent := "root"
	for _, name := range segments {
		f, err := c.findChild(ctx, parent, name, true)
		if err != nil || f == nil {
			return "", err
		}
		parent = f.ID
	}
	return parent, nil
}

// EnsurePath is FindFolder, creating any folders that are missing.
func (c *Client) EnsurePath(ctx context.Context, segments []string) (string, error) {
	parent := "root"
	for _, name := range segments {
		id, err := c.EnsureFolder(ctx, parent, name)
		if err != nil {
			return "", err
		}
		parent = id
	}
	return parent, nil
}

// EnsureFolder returns the ID of the folder called name inside parentID,
// creating it if needed.
func (c *Client) EnsureFolder(ctx context.Context, parentID, name string) (string, error) {
	var id string
	err := Retry(ctx, func() error {
		// Looking first also covers a retry after a create whose reply was lost.
		f, err := c.findChild(ctx, parentID, name, true)
		if err != nil {
			return err
		}
		if f != nil {
			id = f.ID
			return nil
		}
		nf, err := c.srv.Files.Create(&drive.File{Name: name, MimeType: folderMime, Parents: []string{parentID}}).
			Fields("id").Context(ctx).Do()
		if err != nil {
			return err
		}
		id = nf.Id
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("creating Drive folder %q: %w", name, err)
	}
	return id, nil
}

// Upload creates a file called name in parentID from the local file at
// localPath, with Drive's modified time set to modTime. md5sum is the local
// checksum; a retry that finds a file with that name and checksum already in
// place reuses it instead of uploading a duplicate. progress, if not nil,
// receives the bytes sent so far.
func (c *Client) Upload(ctx context.Context, parentID, name, localPath string, modTime time.Time, md5sum string, progress func(sent int64)) (File, error) {
	var out File
	attempt := 0
	err := Retry(ctx, func() error {
		attempt++
		if attempt > 1 {
			existing, err := c.findChild(ctx, parentID, name, false)
			if err != nil {
				return err
			}
			if existing != nil && existing.MD5 == md5sum {
				out = *existing
				return nil
			}
		}
		f, err := os.Open(localPath)
		if err != nil {
			return err
		}
		defer f.Close()
		meta := &drive.File{Name: name, Parents: []string{parentID}, ModifiedTime: modTime.UTC().Format(time.RFC3339Nano)}
		res, err := c.srv.Files.Create(meta).Media(f, googleapi.ChunkSize(chunkSize)).Fields(fileFields).
			Context(withProgress(ctx, progress)).Do()
		if err != nil {
			return err
		}
		out = toFile(res, name)
		return nil
	})
	return out, err
}

// Download writes the content of file id to w. Callers retry with Retry,
// since a failed download leaves w partly written.
func (c *Client) Download(ctx context.Context, id string, w io.Writer) error {
	resp, err := c.srv.Files.Get(id).Context(ctx).Download()
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

// Tree is the contents of a Drive folder, recursively.
type Tree struct {
	Files   []File         // regular files CloudSync can sync
	Folders []File         // every subfolder
	Native  []File         // Google Docs, Sheets, Slides and shortcuts, which have no file to download
	Issues  []filter.Issue // names that cannot exist on Windows, or that clash by case
}

// ListTree lists every non-trashed item under folderID.
func (c *Client) ListTree(ctx context.Context, folderID string) (*Tree, error) {
	type dir struct{ id, rel string }
	t := &Tree{}
	queue := []dir{{id: folderID}}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		children, err := c.listChildren(ctx, d.id)
		if err != nil {
			return nil, fmt.Errorf("listing Drive folder %q: %w", "/"+d.rel, err)
		}
		seen := make(map[string]string) // lower-cased name -> name
		for _, f := range children {
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

			file := toFile(f, rel)
			switch {
			case f.MimeType == folderMime:
				t.Folders = append(t.Folders, file)
				queue = append(queue, dir{id: f.Id, rel: rel})
			case strings.HasPrefix(f.MimeType, googleAppsPrefix):
				t.Native = append(t.Native, file)
			default:
				t.Files = append(t.Files, file)
			}
		}
	}
	return t, nil
}

func (c *Client) listChildren(ctx context.Context, folderID string) ([]*drive.File, error) {
	var out []*drive.File
	err := Retry(ctx, func() error {
		out = out[:0]
		q := fmt.Sprintf("'%s' in parents and trashed = false", folderID)
		return c.srv.Files.List().Q(q).Spaces("drive").PageSize(1000).
			Fields("nextPageToken, files("+fileFields+")").
			Pages(ctx, func(l *drive.FileList) error {
				out = append(out, l.Files...)
				return nil
			})
	})
	return out, err
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
