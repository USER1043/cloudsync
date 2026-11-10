package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"syscall"

	"google.golang.org/api/googleapi"

	"github.com/USER1043/cloudsync/internal/gdrive"
)

// googleStatus is where Google reports outages of its services.
const googleStatus = "https://www.google.com/appsstatus/dashboard/"

// errKind sorts failures by who can fix them and how.
type errKind int

const (
	kindOther     errKind = iota
	kindOffline           // no network, or Google can't be reached: retry later
	kindGoogle            // Google's servers are failing: retry later, not the owner's setup
	kindRateLimit         // Google asks CloudSync to slow down: retry quietly
	kindSignIn            // the sign-in is missing, expired or revoked: `cloudsync login`
	kindQuota             // Drive is full
	kindRoot              // the root's Drive folder is missing or replaced
	kindLocal             // a problem with a file on this computer
)

// quotaError is Drive being too full for what CloudSync has to upload.
type quotaError struct{ need, free int64 }

func (e *quotaError) Error() string {
	return fmt.Sprintf("Google Drive is full: this needs %s but only %s is free. Free up space in Drive, Gmail or Photos "+
		"(they share the storage), or remove files from the synced folder", humanBytes(e.need), humanBytes(e.free))
}

// Windows error numbers (winerror.h) for files CloudSync can't use.
const (
	winSharingViolation = 32
	winLockViolation    = 33
	winPathTooLong      = 206
)

// classify sorts err into an errKind.
func classify(err error) errKind {
	var (
		re *rootError
		qe *quotaError
		ge *googleapi.Error
		pe *fs.PathError
		ue *url.Error
		oe *net.OpError
		de *net.DNSError
	)
	switch {
	case err == nil:
		return kindOther
	case errors.As(err, &re):
		return kindRoot
	case errors.As(err, &qe):
		return kindQuota
	case gdrive.SignInRejected(err) || signInMissing(err):
		return kindSignIn
	case errors.As(err, &ge):
		switch {
		case ge.Code == http.StatusTooManyRequests || hasReason(ge, "rateLimitExceeded", "userRateLimitExceeded"):
			return kindRateLimit
		case hasReason(ge, "storageQuotaExceeded", "quotaExceeded"):
			return kindQuota
		case ge.Code >= 500:
			return kindGoogle
		}
		return kindOther
	// Local before network: a file error can arrive wrapped in the HTTP
	// request that was reading the file, and a bare syscall.Errno also
	// satisfies net.Error.
	case errors.As(err, &pe), errors.Is(err, syscall.ENOSPC):
		return kindLocal
	case errors.As(err, &ue), errors.As(err, &oe), errors.As(err, &de),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET):
		return kindOffline
	}
	return kindOther
}

func hasReason(ge *googleapi.Error, reasons ...string) bool {
	for _, e := range ge.Errors {
		for _, r := range reasons {
			if e.Reason == r {
				return true
			}
		}
	}
	return false
}

// describe says what went wrong and what to do about it, in plain words.
// Anything it doesn't recognise keeps its original message, so nothing is
// hidden; recognised network errors drop the request's long URL.
func describe(err error) string {
	if err == nil {
		return ""
	}
	var (
		re *rootError
		qe *quotaError
		ge *googleapi.Error
		pe *fs.PathError
	)
	switch classify(err) {
	case kindRoot:
		errors.As(err, &re)
		return re.msg
	case kindQuota:
		if errors.As(err, &qe) {
			return qe.Error()
		}
		return "Google Drive is full. Free up space in Drive, Gmail or Photos (they share the storage)"
	case kindSignIn:
		if gdrive.SignInRejected(err) {
			return "Google no longer accepts CloudSync's sign-in (it expired or was revoked). Run `cloudsync login`"
		}
		return err.Error()
	case kindRateLimit:
		return "Google is limiting how fast CloudSync can go; it slows down and tries again"
	case kindGoogle:
		errors.As(err, &ge)
		return fmt.Sprintf("Google Drive is having problems (error %d from Google), not your setup. "+
			"CloudSync tries again later; Google's status page: %s", ge.Code, googleStatus)
	case kindOffline:
		return describeNetwork(err)
	case kindLocal:
		return describeLocal(err, pe)
	}
	return err.Error()
}

func describeNetwork(err error) string {
	var (
		dns *net.DNSError
		op  *net.OpError
		ne  net.Error
	)
	switch {
	case errors.As(err, &dns):
		return fmt.Sprintf("no internet connection (can't look up %s)", dns.Name)
	case errors.As(err, &ne) && ne.Timeout():
		return "the connection to Google Drive timed out; the network may be slow or down"
	case errors.As(err, &op) && op.Op == "dial":
		return fmt.Sprintf("can't connect to Google Drive: no internet, or a firewall or proxy is blocking it (%v)", op.Err)
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET):
		return "the connection to Google Drive dropped"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Sprintf("can't reach Google Drive (%v)", err)
}

func describeLocal(err error, pe *fs.PathError) string {
	path := "a file"
	if errors.As(err, &pe) {
		path = pe.Path
	}
	var errno syscall.Errno
	isErrno := errors.As(err, &errno)
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return "this computer's disk is full; free up some space and CloudSync tries again"
	case errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf("permission denied for %s; check that you can open it, or move it out of the synced folder", path)
	case runtime.GOOS == "windows" && isErrno && (errno == winSharingViolation || errno == winLockViolation):
		return fmt.Sprintf("%s is open in another program; close it and CloudSync tries again", path)
	case runtime.GOOS == "windows" && isErrno && errno == winPathTooLong:
		return fmt.Sprintf("the path is too long for Windows: %s; shorten a folder or file name", path)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("%s disappeared while syncing (moved or deleted meanwhile); the next pass catches up", path)
	}
	return err.Error()
}
