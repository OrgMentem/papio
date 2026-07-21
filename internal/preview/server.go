// Copyright 2026 OrgMentem. Licensed under MIT.

// Package preview serves a short-lived, capability-bound preview of one
// quarantined PDF on the local loopback interface.
package preview

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const defaultTTL = 10 * time.Minute

var errClosed = errors.New("preview server is shut down")

type capability struct {
	actionID int64
	path     string
	sha256   string
	size     int64
	expires  time.Time
	verified bool
}

// Server owns the loopback-only HTTP server and its in-memory capabilities.
// It does not listen until Start or Issue is called.
type Server struct {
	mu       sync.Mutex
	listener net.Listener
	http     *http.Server
	closed   bool
	byToken  map[string]*capability
	byAction map[int64]string
}

// New constructs a preview server without opening a listening socket.
func New() *Server {
	return &Server{
		byToken:  make(map[string]*capability),
		byAction: make(map[int64]string),
	}
}

// Start opens the literal IPv4 loopback listener. It is safe to call more than
// once. Issue starts the server automatically, so callers normally need not
// call Start themselves.
func (s *Server) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	if s.listener != nil {
		return nil
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.listener = listener
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second}
	go func(server *http.Server, l net.Listener) {
		_ = server.Serve(l)
	}(s.http, listener)
	return nil
}

// Issue returns a capability URL which serves only path while it remains
// unexpired. A new capability for an action revokes that action's prior one.
func (s *Server) Issue(actionID int64, path, sha256 string, size int64, ttl time.Duration) (string, error) {
	if path == "" || size < 0 || !validSHA256(sha256) {
		return "", errors.New("invalid preview capability binding")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("invalid preview capability path")
	}
	if err := s.Start(context.Background()); err != nil {
		return "", err
	}
	if ttl == 0 {
		ttl = defaultTTL
	}

	token, err := newToken()
	if err != nil {
		return "", err
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.listener == nil {
		return "", errClosed
	}
	if s.byToken == nil {
		s.byToken = make(map[string]*capability)
		s.byAction = make(map[int64]string)
	}
	s.sweepExpiredLocked(now)
	if prior, ok := s.byAction[actionID]; ok {
		delete(s.byToken, prior)
	}
	s.byToken[token] = &capability{
		actionID: actionID,
		path:     path,
		sha256:   strings.ToLower(sha256),
		size:     size,
		expires:  now.Add(ttl),
	}
	s.byAction[actionID] = token
	return "http://" + s.listener.Addr().String() + "/p/" + token, nil
}

// Revoke removes any preview capability issued for actionID.
func (s *Server) Revoke(actionID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token, ok := s.byAction[actionID]; ok {
		delete(s.byToken, token)
		delete(s.byAction, actionID)
	}
}

// Shutdown stops the listener and permanently rejects further capabilities.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.byToken = make(map[string]*capability)
	s.byAction = make(map[int64]string)
	server := s.http
	s.mu.Unlock()
	if server == nil {
		return nil
	}
	return server.Shutdown(ctx)
}

// ServeHTTP serves one capability-bound PDF. No route exposes a filesystem
// path or any other daemon resource.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Host != s.host() {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	token, ok := capabilityToken(r.URL.Path)
	if !ok {
		writeNotFound(w)
		return
	}

	s.mu.Lock()
	s.sweepExpiredLocked(time.Now())
	entry, ok := s.byToken[token]
	if !ok {
		s.mu.Unlock()
		writeNotFound(w)
		return
	}
	var file *os.File
	var info os.FileInfo
	if !entry.verified {
		file, info, ok = s.verifyLocked(entry)
		if !ok {
			s.revokeLocked(entry.actionID)
			s.mu.Unlock()
			w.WriteHeader(http.StatusGone)
			return
		}
		entry.verified = true
	}
	path := entry.path
	s.mu.Unlock()

	if file == nil {
		var err error
		file, err = os.Open(path)
		if err != nil {
			writeNotFound(w)
			return
		}
		info, err = file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			writeNotFound(w)
			return
		}
	}
	defer file.Close()

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.ServeContent(w, r, "preview.pdf", info.ModTime(), file)
}

func (s *Server) host() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Server) sweepExpiredLocked(now time.Time) {
	for token, entry := range s.byToken {
		if !entry.expires.After(now) {
			delete(s.byToken, token)
			delete(s.byAction, entry.actionID)
		}
	}
}

func (s *Server) revokeLocked(actionID int64) {
	if token, ok := s.byAction[actionID]; ok {
		delete(s.byToken, token)
		delete(s.byAction, actionID)
	}
}

func (s *Server) verifyLocked(entry *capability) (*os.File, os.FileInfo, bool) {
	file, err := os.Open(entry.path)
	if err != nil {
		return nil, nil, false
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != entry.size {
		_ = file.Close()
		return nil, nil, false
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil || hex.EncodeToString(hash.Sum(nil)) != entry.sha256 {
		_ = file.Close()
		return nil, nil, false
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, nil, false
	}
	return file, info, true
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func newToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func capabilityToken(path string) (string, bool) {
	const prefix = "/p/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(path, prefix)
	if token == "" || strings.Contains(token, "/") {
		return "", false
	}
	return token, true
}

func writeNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "not found\n")
}
