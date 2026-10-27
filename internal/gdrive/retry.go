package gdrive

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"google.golang.org/api/googleapi"
)

const maxAttempts = 6

// Retry runs op until it succeeds, fails with an error that retrying won't
// fix, or has been tried maxAttempts times. It waits 1s, 2s, 4s... between
// attempts (with jitter, capped at 32s), as Google asks clients to do when
// rate-limited.
func Retry(ctx context.Context, op func() error) error {
	delay := time.Second
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || attempt == maxAttempts || !retryable(err) || ctx.Err() != nil {
			return err
		}
		wait := delay + rand.N(delay/2)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		delay = min(delay*2, 32*time.Second)
	}
}

func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		case http.StatusForbidden:
			for _, e := range ge.Errors {
				if e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded" {
					return true
				}
			}
		}
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}
