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
<title>PDF identity review — papio</title>
<link rel="stylesheet" href="{{.StylePath}}">
</head>
<body>
<header class="review-bar">
<span class="brand-name" aria-label="papio"><em>papio</em></span>
<div class="review-copy">
{{if .Recorded}}
<p class="confirmation" role="status"><span class="check" aria-hidden="true">✓</span><span class="confirmation-copy">Recorded — review complete</span></p>
<p id="status">You can close this tab.</p>
{{else}}
<p class="question">{{.Question}}</p>
<p id="status" role="status" aria-live="polite"></p>
{{end}}
</div>
<div class="actions">
{{if .Recorded}}
<button class="primary" disabled>Yes, correct file</button><button disabled>No, wrong file</button>
{{else}}
<button class="primary" type="button" data-verdict="accept">Yes, correct file</button><button type="button" data-verdict="reject">No, wrong file</button>
{{end}}
</div>
</header>
{{if .Recorded}}
<main id="preview-area" class="preview-area preview-area--muted"><div class="fallback-panel">Review recorded. This preview is no longer interactive.</div></main>
{{else}}
<main id="preview-area" class="preview-area"><embed src="{{.FilePath}}" type="application/pdf" title="Quarantined PDF preview"></main>
<script src="{{.ScriptPath}}" defer></script>
{{end}}
</body>
</html>
`))

const previewScript = `(() => {
  const buttons = Array.from(document.querySelectorAll('[data-verdict]'));
  const reviewCopy = document.querySelector('.review-copy');
  const previewArea = document.getElementById('preview-area');
  const setDisabled = disabled => buttons.forEach(button => { button.disabled = disabled; });

  const showMessage = (message, hint, recorded) => {
    const lead = document.createElement('p');
    lead.className = recorded ? 'confirmation' : 'error';
    lead.setAttribute('role', 'status');
    if (recorded) {
      const check = document.createElement('span');
      check.className = 'check';
      check.setAttribute('aria-hidden', 'true');
      check.textContent = '✓';
      lead.append(check);
    }
    const copy = document.createElement('span');
    copy.className = 'confirmation-copy';
    copy.textContent = message;
    lead.append(copy);
    const status = document.createElement('p');
    status.id = 'status';
    status.textContent = hint;
    reviewCopy.replaceChildren(lead, status);
  };

  const responseMessage = async response => {
    const body = (await response.text()).trim();
    if (!body) return 'Could not record the verdict.';
    if (response.headers.get('Content-Type')?.startsWith('text/html')) {
      const document = new DOMParser().parseFromString(body, 'text/html');
      return document.querySelector('.confirmation-copy')?.textContent?.trim() || 'Could not record the verdict.';
    }
    return body;
  };

  const showBlockedClose = () => {
    const status = document.getElementById('status');
    if (status) status.textContent = 'You can close this tab.';
    previewArea.classList.add('preview-area--muted');
    if (!previewArea.querySelector('.fallback-panel')) {
      const panel = document.createElement('div');
      panel.className = 'fallback-panel';
      panel.textContent = 'Review recorded. This preview can now be closed.';
      previewArea.append(panel);
    }
  };

  buttons.forEach(button => button.addEventListener('click', async () => {
    const originalLabel = button.textContent;
    setDisabled(true);
    button.textContent = 'Recording…';
    try {
      const response = await fetch(location.pathname + '/verdict', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({verdict: button.dataset.verdict})
      });
      if (response.ok) {
        const phrase = button.dataset.verdict === 'accept' ? 'marked correct' : 'marked wrong file';
        button.textContent = originalLabel;
        showMessage('Recorded — ' + phrase, 'Closing this tab…', true);
        setTimeout(() => {
          window.close();
          setTimeout(showBlockedClose, 600);
        }, 1200);
        return;
      }
      const message = await responseMessage(response);
      button.textContent = originalLabel;
      showMessage(message, response.status === 409 ? 'You can close this tab.' : '', response.status === 409);
    } catch (_) {
      button.textContent = originalLabel;
      showMessage('Could not record the verdict.', '', false);
    }
  }));
})();
`

const previewStyle = `
:root {
  color-scheme: light;
  --color-ink: #182231;
  --color-brand-ink: #2b2d42;
  --color-brand-accent: #d94f3d;
  --color-muted: #607080;
  --color-border: #dce3ea;
  --color-control-border: #b8c5d1;
  --color-page: #f4f7f9;
  --color-surface: #fdfefe;
  --color-surface-hover: #eef3f6;
  --color-primary: #12549b;
  --color-primary-border: #8db9eb;
  --color-primary-surface: #eaf3ff;
  --color-primary-hover: #dcecff;
  --color-on-primary: #fdfefe;
  --radius-control: 6px;
  --radius-card: 8px;
  color: var(--color-ink);
  font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
@media (prefers-color-scheme: dark) {
  :root {
    color-scheme: dark;
    --color-ink: #e5edf4;
    --color-brand-ink: #f0edf3;
    --color-brand-accent: #ef6a57;
    --color-muted: #a2b1be;
    --color-border: #3e4f61;
    --color-control-border: #526579;
    --color-page: #141b23;
    --color-surface: #1c2631;
    --color-surface-hover: #263340;
    --color-primary: #a8d0ff;
    --color-primary-border: #4f85bc;
    --color-primary-surface: #183956;
    --color-primary-hover: #214967;
    --color-on-primary: #101a24;
  }
}
* { box-sizing: border-box; }
html, body { width: 100%; height: 100%; margin: 0; overflow: hidden; background: var(--color-page); }
.review-bar {
  position: fixed;
  inset: 0 0 auto 0;
  z-index: 2;
  min-height: 4.75rem;
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto;
  align-items: center;
  gap: 1rem;
  padding: .65rem 1rem;
  background: var(--color-surface);
  border-bottom: 1px solid var(--color-border);
}
.brand-name {
  color: var(--color-brand-ink);
  font-size: 1.375rem;
  font-weight: 700;
  letter-spacing: .01em;
  line-height: 1.2;
}
.brand-name em {
  text-decoration-color: var(--color-brand-accent);
  text-decoration-line: underline;
  text-decoration-thickness: 2px;
  text-underline-offset: 4px;
}
.review-copy { min-width: 0; }
.review-copy p { margin: 0; }
.question {
  overflow: hidden;
  color: var(--color-ink);
  font-size: .875rem;
  font-weight: 600;
  text-overflow: ellipsis;
  white-space: nowrap;
}
#status {
  min-height: 1rem;
  margin-top: .15rem;
  color: var(--color-muted);
  font-size: .75rem;
  line-height: 1.25;
}
#status:empty { display: none; }
.actions { display: flex; flex: none; gap: .5rem; }
button {
  appearance: none;
  min-height: 36px;
  padding: 4px 12px;
  border: 1px solid var(--color-control-border);
  border-radius: var(--radius-control);
  background: var(--color-surface);
  color: var(--color-ink);
  cursor: pointer;
  font: inherit;
  font-size: .75rem;
  white-space: nowrap;
}
button:hover:not(:disabled) { background: var(--color-surface-hover); }
button:focus-visible { outline: 2px solid var(--color-primary); outline-offset: 2px; }
button:disabled { cursor: not-allowed; opacity: .55; }
button.primary {
  border-color: var(--color-primary);
  background: var(--color-primary);
  color: var(--color-on-primary);
}
button.primary:hover:not(:disabled) {
  border-color: var(--color-primary-hover);
  background: var(--color-primary-hover);
  color: var(--color-ink);
}
.confirmation { display: flex; align-items: center; gap: .45rem; color: var(--color-ink); font-size: .875rem; font-weight: 650; }
.check { flex: none; color: var(--color-brand-accent); font-size: 1.1rem; font-weight: 800; }
.error { color: var(--color-brand-accent); font-size: .875rem; font-weight: 650; }
.preview-area {
  position: fixed;
  inset: 4.75rem 0 0 0;
  background: var(--color-page);
}
.preview-area embed { width: 100%; height: 100%; border: 0; transition: opacity 180ms ease-out; }
.preview-area--muted embed { opacity: .12; pointer-events: none; }
.fallback-panel {
  position: absolute;
  inset: 1rem;
  z-index: 1;
  display: grid;
  place-items: center;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-card);
  background: var(--color-primary-surface);
  color: var(--color-muted);
  font-size: .875rem;
  text-align: center;
}
`
