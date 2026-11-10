// Package gdrive wraps the Google Drive v3 calls CloudSync needs.
package gdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
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
	Trashed  bool // set only by Update, for a file found in the trash or gone
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
		r, err = c.srv.Files.List().Q(q).Spaces("drive").Fields("files(" + fileFields + ")").PageSize(100).Context(ctx).Do()
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("looking up %q in Drive: %w", name, err)
	}
	// Compare names exactly here rather than trust the query to, so "Docs"
	// and "docs" in one folder stay two folders.
	var match []*drive.File
	for _, f := range r.Files {
		if f.Name == name {
			match = append(match, f)
		}
	}
	switch {
	case len(match) == 0:
		return nil, nil
	case folder && len(match) > 1:
		return nil, fmt.Errorf("Drive has more than one folder named %q in the same place; rename one so the path is unambiguous", name)
	}
	f := toFile(match[0], name)
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
	return c.folder(ctx, parentID, name, true)
}

// CreateFolder is EnsureFolder for a parent known to have no folder called
// name, such as one created moments ago: it saves the lookup, one request
// in two. Retries still look first, so a create that went through but whose
// reply was lost is not made twice.
func (c *Client) CreateFolder(ctx context.Context, parentID, name string) (string, error) {
	return c.folder(ctx, parentID, name, false)
}

func (c *Client) folder(ctx context.Context, parentID, name string, lookFirst bool) (string, error) {
	var id string
	attempt := 0
	err := Retry(ctx, func() error {
		attempt++
		// Looking also covers a retry after a create whose reply was lost.
		if lookFirst || attempt > 1 {
			f, err := c.findChild(ctx, parentID, name, true)
			if err != nil {
				return err
			}
			if f != nil {
				id = f.ID
				return nil
			}
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

// ErrChanged reports that a Drive file is no longer what CloudSync last saw:
// edited, trashed or deleted by someone else, such as the phone.
var ErrChanged = errors.New("changed in Drive since it was last checked")

// Update replaces the content of the Drive file id with the local file at
// localPath, keeping the file's ID, links and version history. It first
// checks that Drive's copy still has the checksum want (the one CloudSync
// last synced), so a newer edit made elsewhere is never overwritten: if not,
// it returns the file as it is now, with Trashed set if it is gone, and an
// error wrapping ErrChanged. md5sum is the local checksum; a retry that finds
// it already in place is done.
func (c *Client) Update(ctx context.Context, id, localPath string, modTime time.Time, md5sum, want string, progress func(sent int64)) (File, error) {
	var out File
	err := Retry(ctx, func() error {
		cur, err := c.get(ctx, id)
		if err != nil {
			return err
		}
		switch {
		case !cur.Trashed && cur.MD5 == md5sum:
			out = cur // an earlier attempt went through
			return nil
		case cur.Trashed || cur.MD5 != want:
			out = cur
			return fmt.Errorf("updating %q: %w", cur.Rel, ErrChanged)
		}
		f, err := os.Open(localPath)
		if err != nil {
			return err
		}
		defer f.Close()
		meta := &drive.File{ModifiedTime: modTime.UTC().Format(time.RFC3339Nano)}
		res, err := c.srv.Files.Update(id, meta).Media(f, googleapi.ChunkSize(chunkSize)).Fields(fileFields).
			Context(withProgress(ctx, progress)).Do()
		if err != nil {
			return err
		}
		out = toFile(res, res.Name)
		return nil
	})
	return out, err
}

// get returns the file id, with Trashed set if it is in the trash or no
// longer exists. Rel is just the file's name.
func (c *Client) get(ctx context.Context, id string) (File, error) {
	f, err := c.srv.Files.Get(id).Fields(fileFields + ", trashed").Context(ctx).Do()
	if isNotFound(err) {
		return File{ID: id, Trashed: true}, nil
	}
	if err != nil {
		return File{}, err
	}
	out := toFile(f, f.Name)
	out.Trashed = f.Trashed
	return out, nil
}

// Stat returns the file or folder id, with Trashed set if it is in the trash
// or no longer exists.
func (c *Client) Stat(ctx context.Context, id string) (File, error) {
	var f File
	err := Retry(ctx, func() error {
		var err error
		f, err = c.get(ctx, id)
		return err
	})
	return f, err
}

// Trash moves the file or folder id to Drive's trash, from which the owner
// can restore it for 30 days. Trashing a folder trashes everything inside,
// so callers check HasChildren first. A file that is already gone is fine.
func (c *Client) Trash(ctx context.Context, id string) error {
	err := Retry(ctx, func() error {
		_, err := c.srv.Files.Update(id, &drive.File{Trashed: true}).Fields("id").Context(ctx).Do()
		if isNotFound(err) {
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("moving to Drive's trash: %w", err)
	}
	return nil
}

// HasChildren reports whether the Drive folder id holds anything that is not
// in the trash, including items CloudSync doesn't sync, such as Google Docs.
func (c *Client) HasChildren(ctx context.Context, id string) (bool, error) {
	var any bool
	err := Retry(ctx, func() error {
		q := fmt.Sprintf("'%s' in parents and trashed = false", escape(id))
		l, err := c.srv.Files.List().Q(q).Spaces("drive").PageSize(1).Fields("files(id)").Context(ctx).Do()
		if err != nil {
			return err
		}
		any = len(l.Files) > 0
		return nil
	})
	return any, err
}

func isNotFound(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == http.StatusNotFound
}

// Tree is the contents of a Drive folder, recursively.
type Tree struct {
	Files   []File         // regular files CloudSync can sync
	Folders []File         // every subfolder
	Native  []File         // Google Docs, Sheets, Slides and shortcuts, which have no file to download
	Issues  []filter.Issue // names that cannot be synced, and exact duplicates
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
		seen := make(map[string]bool)
		for _, f := range children {
			rel := joinRel(d.rel, f.Name)
			if reason := filter.CheckName(f.Name); reason != "" {
				t.Issues = append(t.Issues, filter.Issue{Rel: rel, Reason: reason})
				continue
			}
			// Drive allows two items with the same name in one folder; no file
			// system does. Names that differ only by case are fine.
			if seen[f.Name] {
				t.Issues = append(t.Issues, filter.Issue{Rel: rel, Reason: "another item in this Drive folder has exactly the same name"})
				continue
			}
			seen[f.Name] = true

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

// Filter returns the tree without what m skips: ignored files and folders,
// everything inside ignored folders, and files over the size limit. It also
// returns the paths it left out, folders ending in "/".
func (t *Tree) Filter(m *filter.Matcher) (*Tree, []string) {
	out := &Tree{Issues: t.Issues}
	var skipped []string
	ignoredDirs := make(map[string]bool)
	inIgnored := func(rel string) bool {
		for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
			if ignoredDirs[dir] {
				return true
			}
		}
		return false
	}

	folders := slices.Clone(t.Folders)
	slices.SortFunc(folders, func(a, b File) int { return strings.Compare(a.Rel, b.Rel) }) // parents first
	for _, f := range folders {
		switch {
		case inIgnored(f.Rel):
			ignoredDirs[f.Rel] = true
		case m.Match(f.Rel, true):
			ignoredDirs[f.Rel] = true
			skipped = append(skipped, f.Rel+"/")
		default:
			out.Folders = append(out.Folders, f)
		}
	}
	for _, f := range t.Files {
		switch {
		case inIgnored(f.Rel):
		case m.Match(f.Rel, false) || m.TooLarge(f.Size):
			skipped = append(skipped, f.Rel)
		default:
			out.Files = append(out.Files, f)
		}
	}
	for _, f := range t.Native {
		if !inIgnored(f.Rel) && !m.Match(f.Rel, false) {
			out.Native = append(out.Native, f)
		}
	}
	return out, skipped
}

// SubFolders lists the names of the folders directly inside the folder at
// segments under My Drive, or nil if that folder does not exist.
func (c *Client) SubFolders(ctx context.Context, segments []string) ([]string, error) {
	id, err := c.FindFolder(ctx, segments)
	if err != nil || id == "" {
		return nil, err
	}
	children, err := c.listChildren(ctx, id)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range children {
		if f.MimeType == folderMime {
			names = append(names, f.Name)
		}
	}
	slices.Sort(names)
	return names, nil
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
