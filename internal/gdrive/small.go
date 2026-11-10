package gdrive

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

// The functions here handle CloudSync's own small files in Drive, such as
// the list of files a computer shut down without uploading. They are kept
// outside every synced folder and are never the owner's files, so
// RemoveSmall deletes outright instead of trashing.

// ReadSmall returns the content of the file name directly inside folderID,
// or nil if there is none.
func (c *Client) ReadSmall(ctx context.Context, folderID, name string) ([]byte, error) {
	f, err := c.findChild(ctx, folderID, name, false)
	if err != nil || f == nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = Retry(ctx, func() error {
		buf.Reset()
		return c.Download(ctx, f.ID, &buf)
	})
	if err != nil {
		return nil, fmt.Errorf("reading %q from Drive: %w", name, err)
	}
	return buf.Bytes(), nil
}

// WriteSmall creates or replaces the file name directly inside folderID.
func (c *Client) WriteSmall(ctx context.Context, folderID, name string, data []byte) error {
	f, err := c.findChild(ctx, folderID, name, false)
	if err != nil {
		return err
	}
	media := func() io.Reader { return bytes.NewReader(data) }
	ct := googleapi.ContentType("application/json")
	err = Retry(ctx, func() error {
		var err error
		if f != nil {
			_, err = c.srv.Files.Update(f.ID, &drive.File{}).Media(media(), ct).Fields("id").Context(ctx).Do()
		} else {
			_, err = c.srv.Files.Create(&drive.File{Name: name, Parents: []string{folderID}}).
				Media(media(), ct).Fields("id").Context(ctx).Do()
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("writing %q to Drive: %w", name, err)
	}
	return nil
}

// RemoveSmall deletes the file name directly inside folderID, if there is
// one.
func (c *Client) RemoveSmall(ctx context.Context, folderID, name string) error {
	f, err := c.findChild(ctx, folderID, name, false)
	if err != nil || f == nil {
		return err
	}
	err = Retry(ctx, func() error {
		err := c.srv.Files.Delete(f.ID).Context(ctx).Do()
		if isNotFound(err) {
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("removing %q from Drive: %w", name, err)
	}
	return nil
}

// ListSmall returns the names of the files (not folders) directly inside
// folderID.
func (c *Client) ListSmall(ctx context.Context, folderID string) ([]string, error) {
	children, err := c.listChildren(ctx, folderID)
	if err != nil {
		return nil, fmt.Errorf("listing CloudSync's files in Drive: %w", err)
	}
	var names []string
	for _, f := range children {
		if f.MimeType != folderMime {
			names = append(names, f.Name)
		}
	}
	return names, nil
}

// Ping checks once, without retrying, that Drive can be reached, so a
// caller with little time (at shutdown) can tell quickly that it is offline.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.srv.About.Get().Fields("kind").Context(ctx).Do()
	return err
}
