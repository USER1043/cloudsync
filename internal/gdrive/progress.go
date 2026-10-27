package gdrive

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// progressKey marks a request context that carries an upload progress callback.
type progressKey struct{}

// withProgress makes uploads sent with ctx report their progress to fn.
func withProgress(ctx context.Context, fn func(sent int64)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, fn)
}

// countingTransport reports how many bytes of an upload have been handed to
// the network. It counts the request body as the HTTP client sends it,
// because the Drive library reads small files into memory before sending,
// so counting reads of the file itself would jump to 100% at once.
type countingTransport struct{ base http.RoundTripper }

func (t countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	fn, _ := req.Context().Value(progressKey{}).(func(int64))
	if fn == nil || req.Body == nil || req.Body == http.NoBody {
		return t.base.RoundTrip(req)
	}
	r := *req // a RoundTripper must not modify the caller's request
	r.Body = &countingBody{ReadCloser: req.Body, offset: rangeStart(req.Header.Get("Content-Range")), fn: fn}
	return t.base.RoundTrip(&r)
}

type countingBody struct {
	io.ReadCloser
	offset, n int64
	fn        func(int64)
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	b.fn(b.offset + b.n)
	return n, err
}

// rangeStart returns where a resumable-upload chunk starts in the file, from
// a header like "bytes 8388608-16777215/20971520"; 0 for anything else.
func rangeStart(h string) int64 {
	var start int64
	if _, err := fmt.Sscanf(h, "bytes %d-", &start); err != nil {
		return 0
	}
	return start
}
