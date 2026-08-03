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
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"papio/internal/job"
)

const defaultTTL = 10 * time.Minute

var errClosed = errors.New("preview server is shut down")

// Citation is the requested work identity shown above a preview.
type Citation struct {
	Title   string
	Authors []string
	Year    int
}

// IssueInput binds one preview capability to the exact open review action and
// quarantined file it may resolve.
type IssueInput struct {
	ActionID         int64
	Path             string
	SHA256           string
	Size             int64
	ExpectedRevision int64
	Citation         Citation
	TTL              time.Duration
}

// ReviewResolver is the existing durable identity-review resolution boundary.
type ReviewResolver interface {
	ResolveReviewCAS(context.Context, job.ResolveReviewInput) (job.ReviewResolution, error)
}

type capability struct {
	actionID         int64
	path             string
	sha256           string
	size             int64
	expectedRevision int64
	citation         Citation
	expires          time.Time
	verified         bool
	resolving        bool
	recorded         bool
}

// Server owns the loopback-only HTTP server and its in-memory capabilities.
// It does not listen until Start or Issue is called.
type Server struct {
	mu       sync.Mutex
	listener net.Listener
	http     *http.Server
	closed   bool
	resolver ReviewResolver
	byToken  map[string]*capability
	byAction map[int64]string
}

// New constructs a preview server without opening a listening socket.
// Verdicts fail closed when resolver is nil.
func New(resolver ReviewResolver) *Server {
	return &Server{
		resolver: resolver,
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

// Issue returns a capability URL which serves one review shell while it
// remains unexpired. A new capability for an action revokes that action's
// prior one.
func (s *Server) Issue(input IssueInput) (string, error) {
	if input.ActionID <= 0 || input.Path == "" || input.Size < 0 || input.ExpectedRevision <= 0 || !validSHA256(input.SHA256) {
		return "", errors.New("invalid preview capability binding")
	}
	info, err := os.Stat(input.Path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("invalid preview capability path")
	}
	if err := s.Start(context.Background()); err != nil {
		return "", err
	}
	if input.TTL == 0 {
		input.TTL = defaultTTL
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
	if prior, ok := s.byAction[input.ActionID]; ok {
		delete(s.byToken, prior)
	}
	s.byToken[token] = &capability{
		actionID:         input.ActionID,
		path:             input.Path,
		sha256:           strings.ToLower(input.SHA256),
		size:             input.Size,
		expectedRevision: input.ExpectedRevision,
		citation:         input.Citation,
		expires:          now.Add(input.TTL),
	}
	s.byAction[input.ActionID] = token
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

// ServeHTTP serves only the shell and resources belonging to one capability.
// No route exposes a filesystem path or any other daemon resource.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Host != s.host() {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	token, resource, ok := capabilityRoute(r.URL.Path)
	if !ok {
		writeNotFound(w)
		return
	}

	switch resource {
	case "":
		if !allowMethod(w, r, http.MethodGet, http.MethodHead) {
			return
		}
		s.serveShell(w, r, token)
	case "file":
		if !allowMethod(w, r, http.MethodGet, http.MethodHead) {
			return
		}
		s.servePDF(w, r, token)
	case "script.js":
		if !allowMethod(w, r, http.MethodGet, http.MethodHead) {
			return
		}
		s.serveAsset(w, r, token, "text/javascript; charset=utf-8", previewScript)
	case "style.css":
		if !allowMethod(w, r, http.MethodGet, http.MethodHead) {
			return
		}
		s.serveAsset(w, r, token, "text/css; charset=utf-8", previewStyle)
	case "verdict":
		if !allowMethod(w, r, http.MethodPost) {
			return
		}
		s.serveVerdict(w, r, token)
	default:
		writeNotFound(w)
	}
}

func (s *Server) serveShell(w http.ResponseWriter, r *http.Request, token string) {
	s.mu.Lock()
	s.sweepExpiredLocked(time.Now())
	entry, ok := s.byToken[token]
	if !ok {
		s.mu.Unlock()
		writeNotFound(w)
		return
	}
	data := shellData{
		Question:   reviewQuestion(entry.citation),
		FilePath:   "/p/" + token + "/file",
		ScriptPath: "/p/" + token + "/script.js",
		StylePath:  "/p/" + token + "/style.css",
		Recorded:   entry.recorded,
	}
	s.mu.Unlock()
	writeShell(w, r, http.StatusOK, data)
}

func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, token, contentType, body string) {
	s.mu.Lock()
	s.sweepExpiredLocked(time.Now())
	_, ok := s.byToken[token]
	s.mu.Unlock()
	if !ok {
		writeNotFound(w)
		return
	}
	w.Header().Set("Content-Type", contentType)
	setPrivateHeaders(w)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, body)
	}
}

func (s *Server) serveVerdict(w http.ResponseWriter, r *http.Request, token string) {
	if strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0] != "application/json" {
		writePlainError(w, http.StatusUnsupportedMediaType, "application/json required\n")
		return
	}
	var request struct {
		Verdict string `json:"verdict"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1025))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writePlainError(w, http.StatusBadRequest, "invalid verdict\n")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writePlainError(w, http.StatusBadRequest, "invalid verdict\n")
		return
	}
	if request.Verdict != "accept" && request.Verdict != "reject" {
		writePlainError(w, http.StatusBadRequest, "invalid verdict\n")
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
	if entry.recorded || entry.resolving {
		data := recordedShellData(token)
		s.mu.Unlock()
		writeShell(w, r, http.StatusConflict, data)
		return
	}
	if request.Verdict == "accept" && !entry.verified {
		s.mu.Unlock()
		writePlainError(w, http.StatusTooEarly, "PDF has not loaded yet\n")
		return
	}
	resolver := s.resolver
	if resolver == nil {
		s.mu.Unlock()
		writePlainError(w, http.StatusServiceUnavailable, "review resolution is unavailable\n")
		return
	}
	entry.resolving = true
	input := job.ResolveReviewInput{
		ActionID: entry.actionID, Verdict: request.Verdict,
		ExpectedRevision: entry.expectedRevision, ExpectedSHA256: entry.sha256,
	}
	s.mu.Unlock()

	resolution, err := resolver.ResolveReviewCAS(r.Context(), input)

	s.mu.Lock()
	entry.resolving = false
	if err == nil {
		// Applied, already-applied, and conflict are all terminal for this
		// capability. Never let a stale page submit a different verdict.
		entry.recorded = true
	}
	s.mu.Unlock()
	if err != nil {
		writePlainError(w, http.StatusServiceUnavailable, "review could not be recorded\n")
		return
	}
	status := http.StatusOK
	if resolution.Outcome != job.ReviewApplied {
		status = http.StatusConflict
	}
	writeShell(w, r, status, recordedShellData(token))
}

func (s *Server) servePDF(w http.ResponseWriter, r *http.Request, token string) {
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
	w.Header().Set("Content-Disposition", `inline; filename="preview.pdf"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	setPrivateHeaders(w)
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

func capabilityRoute(path string) (token, resource string, ok bool) {
	const prefix = "/p/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) == 0 || parts[0] == "" || len(parts[0]) != 43 {
		return "", "", false
	}
	if len(parts) == 1 {
		return parts[0], "", true
	}
	if len(parts) == 2 && parts[1] != "" {
		return parts[0], parts[1], true
	}
	return "", "", false
}

func allowMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, method := range methods {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	w.WriteHeader(http.StatusMethodNotAllowed)
	return false
}

func setPrivateHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func setShellHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; frame-src 'self'; object-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'")
	setPrivateHeaders(w)
}

func writePlainError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	setPrivateHeaders(w)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message)
}

func writeNotFound(w http.ResponseWriter) {
	writePlainError(w, http.StatusNotFound, "not found\n")
}

type shellData struct {
	Question   string
	FilePath   string
	ScriptPath string
	StylePath  string
	Recorded   bool
}

func recordedShellData(token string) shellData {
	return shellData{
		ScriptPath: "/p/" + token + "/script.js",
		StylePath:  "/p/" + token + "/style.css",
		Recorded:   true,
	}
}

func reviewQuestion(citation Citation) string {
	title := strings.TrimSpace(citation.Title)
	if title == "" || citation.Year <= 0 {
		return "Is this the file you requested?"
	}
	authors := make([]string, 0, len(citation.Authors))
	for _, author := range citation.Authors {
		if author = strings.TrimSpace(author); author != "" {
			authors = append(authors, author)
		}
	}
	if len(authors) == 0 {
		return "Is this the file you requested?"
	}
	return "Is this “" + title + "” (" + strings.Join(authors, ", ") + ", " + strconv.Itoa(citation.Year) + ")?"
}

func writeShell(w http.ResponseWriter, r *http.Request, status int, data shellData) {
	setShellHeaders(w)
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_ = shellTemplate.Execute(w, data)
	}
}

var shellTemplate = template.Must(template.New("preview").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>PDF identity review</title>
<link rel="stylesheet" href="{{.StylePath}}">
</head>
<body>
<header>
{{if .Recorded}}
<p class="recorded" role="status">Recorded — you can close this tab.</p>
<div class="actions"><button disabled>Yes, correct file</button><button disabled>No, wrong file</button></div>
{{else}}
<p>{{.Question}}</p>
<div class="actions"><button type="button" data-verdict="accept">Yes, correct file</button><button type="button" data-verdict="reject">No, wrong file</button></div>
<p id="status" role="status" aria-live="polite"></p>
{{end}}
</header>
{{if not .Recorded}}<embed src="{{.FilePath}}" type="application/pdf" title="Quarantined PDF preview">{{end}}
{{if not .Recorded}}<script src="{{.ScriptPath}}" defer></script>{{end}}
</body>
</html>
`))

const previewScript = `(() => {
  const buttons = Array.from(document.querySelectorAll('[data-verdict]'));
  const status = document.getElementById('status');
  const setDisabled = disabled => buttons.forEach(button => { button.disabled = disabled; });
  buttons.forEach(button => button.addEventListener('click', async () => {
    setDisabled(true);
    status.textContent = 'Recording…';
    try {
      const response = await fetch(location.pathname + '/verdict', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({verdict: button.dataset.verdict})
      });
      if (response.ok || response.status === 409) {
        location.reload();
        return;
      }
      status.textContent = response.status === 425 ? 'Wait for the PDF to load, then try again.' : 'Could not record the verdict. Try again.';
    } catch (_) {
      status.textContent = 'Could not record the verdict. Try again.';
    }
    setDisabled(false);
  }));
})();
`

const previewStyle = `
:root { color-scheme: light dark; font-family: system-ui, sans-serif; }
* { box-sizing: border-box; }
html, body { width: 100%; height: 100%; margin: 0; overflow: hidden; }
header { position: fixed; inset: 0 0 auto 0; height: 4.5rem; z-index: 1; display: flex; align-items: center; gap: 1rem; padding: .65rem 1rem; background: Canvas; border-bottom: 1px solid GrayText; }
header p { margin: 0; min-width: 0; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 600; }
.actions { display: flex; flex: none; gap: .6rem; }
button { padding: .55rem .8rem; font: inherit; }
embed { position: fixed; inset: 4.5rem 0 0 0; width: 100%; height: calc(100% - 4.5rem); border: 0; }
.recorded { text-align: center; }
#status { max-width: 18rem; font-size: .85rem; font-weight: 400; }
`
