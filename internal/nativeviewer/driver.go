// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package nativeviewer drives one bounded Firefox PDF-viewer Save operation.
// Saved means the native Save action was dispatched; callers must validate and
// adopt the resulting bytes independently. URLs travel only on the child's stdin.
package nativeviewer

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Request struct {
	URL      string
	Filename string
	Deadline time.Time
}

type Status string

const (
	Pending Status = "pending"
	Saved   Status = "saved"
)

type Driver interface {
	Prepare(context.Context, Request) (Session, error)
}

// Session retains one prepared native surface. Close is required and idempotent.
// Cancellation of an Advance call terminates the session. Prepare's context
// bounds preparation only; Deadline and Close govern the returned session.
type Session interface {
	Advance(context.Context) (Status, error)
	Close() error
}

const maxLifetime = 2 * time.Minute

var filenamePattern = regexp.MustCompile(`^papio-viewer-[a-z0-9_-]+\.pdf$`)

var (
	errRequest   = errors.New("native viewer: invalid request")
	errProtocol  = errors.New("native viewer: invalid helper response")
	errTransport = errors.New("native viewer: helper unavailable")
	errClosed    = errors.New("native viewer: session closed")
	errRejected  = errors.New("native viewer: native surface refused")
)

// NewDriver returns nil unless this machine can execute the explicitly selected
// helper. It does not launch the helper, inspect browsers, or request permission.
func NewDriver(path string) Driver {
	if runtime.GOOS != "darwin" || path == "" {
		return nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil
	}
	return &processDriver{path: path, rpcTimeout: 15 * time.Second}
}

func validateRequest(r Request, now time.Time) error {
	if !utf8.ValidString(r.URL) || strings.ContainsAny(r.URL, " \\") {
		return errRequest
	}
	for _, ch := range r.URL {
		if ch < 32 || ch >= 127 && ch <= 159 {
			return errRequest
		}
	}
	// net/url leaves malformed query escapes untouched; validate those too.
	for i := 0; i < len(r.URL); i++ {
		if r.URL[i] == '%' {
			if i+2 >= len(r.URL) {
				return errRequest
			}
			if _, err := strconv.ParseUint(r.URL[i+1:i+3], 16, 8); err != nil {
				return errRequest
			}
			i += 2
		}
	}
	u, err := url.Parse(r.URL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Hostname() == "" || u.User != nil || u.Opaque != "" || len(r.URL) > 8192 ||
		len(r.Filename) > 128 || !filenamePattern.MatchString(r.Filename) ||
		r.Deadline.IsZero() || !r.Deadline.After(now) || r.Deadline.After(now.Add(maxLifetime)) {
		return errRequest
	}
	if strings.HasPrefix(u.Host, "[") && net.ParseIP(u.Hostname()) == nil {
		return errRequest
	}
	if port := u.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return errRequest
		}
	}
	return nil
}
