// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package zotio owns papio's narrow, credential-free subprocess boundary to
// Zotio. Zotio remains authoritative for Zotero reads, writes, deduplication,
// and attachment upload semantics.
package zotio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"papio/internal/config"
)

const (
	MinimumVersion = "0.10.0"
	maxStdoutBytes = 8 << 20
	maxStderrBytes = 64 << 10
)

var (
	versionRE = regexp.MustCompile(`^zotio ([0-9]+\.[0-9]+\.[0-9]+)(?:[-+][^ ]+)?$`)
	keyRE     = regexp.MustCompile(`^[A-Z0-9]{8}$`)
)

// RequiredCapabilities is the exact public Zotio surface Phase 4 consumes.
// Preflight fails before acquisition or mutation when an installed Zotio is old
// or missing one of these commands.
var RequiredCapabilities = map[string]string{
	"items missing-pdf":       "read",
	"items get":               "read",
	"attachments add":         "write",
	"import scan":             "read",
	"import resolve":          "read",
	"import apply":            "write",
	"items add-to-collection": "write",
	"sync":                    "sync",
}

// ExecFunc is injected by tests. Production uses an argv-only os/exec call;
// papio never constructs a shell command.
type ExecFunc func(context.Context, ...string) ([]byte, error)

// Client invokes one configured Zotio executable with bounded output and time.
type Client struct {
	Executable string
	Timeout    time.Duration
	Exec       ExecFunc
}

// Capability is the subset of Zotio's machine registry needed for preflight.
type Capability struct {
	Path        string   `json:"path"`
	Operation   string   `json:"operation"`
	WriteTarget string   `json:"write_target,omitempty"`
	Requires    []string `json:"requires,omitempty"`
}

// PreflightResult records the executable and registry contract actually checked.
type PreflightResult struct {
	Executable   string       `json:"executable"`
	Version      string       `json:"version"`
	Capabilities []Capability `json:"capabilities"`
}

// MissingPDFItem is one row from Zotio's synced missing-PDF queue.
type MissingPDFItem struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	DOI       string `json:"doi,omitempty"`
	ItemType  string `json:"item_type,omitempty"`
	DateAdded string `json:"date_added,omitempty"`
}

// Item is the bibliographic subset used when a queue row has no identifier.
type Item struct {
	Key         string
	Title       string
	DOI         string
	Authors     []string
	Year        int
	Collections []string
}

type itemEnvelope struct {
	Results struct {
		Data struct {
			Key         string    `json:"key"`
			Title       string    `json:"title"`
			DOI         string    `json:"DOI"`
			Date        string    `json:"date"`
			Creators    []creator `json:"creators"`
			Collections []string  `json:"collections"`
		} `json:"data"`
		Meta struct {
			ParsedDate string `json:"parsedDate"`
		} `json:"meta"`
	} `json:"results"`
}

type creator struct {
	Type      string `json:"creatorType"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Name      string `json:"name"`
}

// New creates a client from validated papio configuration.
func New(cfg config.Zotio) *Client {
	return &Client{
		Executable: cfg.Executable,
		Timeout:    time.Duration(cfg.TimeoutSeconds) * time.Second,
	}
}

// Preflight verifies the semantic version and typed capability registry.
func (c *Client) Preflight(ctx context.Context) (*PreflightResult, error) {
	versionOut, err := c.run(ctx, "version", "--agent")
	if err != nil {
		return nil, fmt.Errorf("zotio version preflight: %w", err)
	}
	version, err := parseVersion(string(versionOut), c.Executable)
	if err != nil {
		return nil, err
	}
	if compareVersion(version, MinimumVersion) < 0 {
		return nil, fmt.Errorf("zotio %s at %s is older than papio requires (>= %s) — update your zotio installation, then retry", version, c.Executable, MinimumVersion)
	}

	capabilityOut, err := c.run(ctx, "capabilities")
	if err != nil {
		return nil, fmt.Errorf("zotio capability preflight: %w", err)
	}
	var capabilities []Capability
	if err := json.Unmarshal(capabilityOut, &capabilities); err != nil {
		return nil, fmt.Errorf("decoding zotio capabilities: %w", err)
	}
	seen := make(map[string]Capability, len(capabilities))
	for _, capability := range capabilities {
		seen[capability.Path] = capability
	}
	for path, operation := range RequiredCapabilities {
		capability, ok := seen[path]
		if !ok {
			return nil, fmt.Errorf("zotio %s at %s is missing capability %q required by papio — update zotio at %s, then retry", version, c.Executable, path, c.Executable)
		}
		if capability.Operation != operation {
			return nil, fmt.Errorf("zotio %s at %s reports operation %q for capability %q, but papio requires %q — update zotio at %s, then retry", version, c.Executable, capability.Operation, path, operation, c.Executable)
		}
	}
	if capability := seen["attachments add"]; capability.WriteTarget != "web_api" {
		return nil, fmt.Errorf("zotio %s at %s reports write target %q for capability %q, but papio requires %q — update zotio at %s, then retry", version, c.Executable, capability.WriteTarget, capability.Path, "web_api", c.Executable)
	}

	return &PreflightResult{
		Executable:   c.Executable,
		Version:      version,
		Capabilities: requiredSubset(seen),
	}, nil
}

// MissingPDF returns Zotio's synced missing-PDF queue, optionally filtered to an exact collection key.
func (c *Client) MissingPDF(ctx context.Context, collection string, limit int) ([]MissingPDFItem, error) {
	if collection != "" && !keyRE.MatchString(collection) {
		return nil, fmt.Errorf("invalid Zotero collection key %q", collection)
	}
	if limit < 1 || limit > 500 {
		return nil, fmt.Errorf("limit must be in 1..500")
	}
	args := []string{"--agent", "items", "missing-pdf", "--limit", strconv.Itoa(limit)}
	if collection != "" {
		args = append(args, "--collection", collection)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("reading Zotio missing-PDF queue: %w", err)
	}
	var items []MissingPDFItem
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("decoding Zotio missing-PDF queue: %w", err)
	}
	for i := range items {
		items[i].Key = strings.TrimSpace(items[i].Key)
		items[i].Title = strings.TrimSpace(items[i].Title)
		items[i].DOI = strings.TrimSpace(items[i].DOI)
		if !keyRE.MatchString(items[i].Key) {
			return nil, fmt.Errorf("Zotio queue row %d has invalid item key %q", i, items[i].Key)
		}
	}
	return items, nil
}

// MissingPDFKeys reads missing-PDF state for exact parent keys. It avoids
// inferring ownership from an arbitrary page of a large library-wide queue.
func (c *Client) MissingPDFKeys(ctx context.Context, keys []string) ([]MissingPDFItem, error) {
	if len(keys) == 0 || len(keys) > 50 {
		return nil, fmt.Errorf("missing-PDF key lookup requires 1..50 item keys")
	}
	clean := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if !keyRE.MatchString(key) {
			return nil, fmt.Errorf("invalid Zotero item key %q", key)
		}
		clean = append(clean, key)
	}
	out, err := c.run(ctx, "--agent", "items", "missing-pdf", "--keys", strings.Join(clean, ","))
	if err != nil {
		return nil, fmt.Errorf("reading Zotio missing-PDF queue: %w", err)
	}
	var items []MissingPDFItem
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("decoding Zotio missing-PDF queue: %w", err)
	}
	for i := range items {
		items[i].Key = strings.TrimSpace(items[i].Key)
		if !keyRE.MatchString(items[i].Key) {
			return nil, fmt.Errorf("Zotio queue row %d has invalid item key %q", i, items[i].Key)
		}
	}
	return items, nil
}

// GetItem returns enough metadata to identify a queue row without a DOI.
func (c *Client) GetItem(ctx context.Context, key string) (*Item, error) {
	if !keyRE.MatchString(key) {
		return nil, fmt.Errorf("invalid Zotero item key %q", key)
	}
	out, err := c.run(ctx, "--agent", "items", "get", key)
	if err != nil {
		return nil, fmt.Errorf("reading Zotio item %s: %w", key, err)
	}
	var envelope itemEnvelope
	if err := json.Unmarshal(out, &envelope); err != nil {
		return nil, fmt.Errorf("decoding Zotio item %s: %w", key, err)
	}
	data := envelope.Results.Data
	item := &Item{
		Key:         strings.TrimSpace(data.Key),
		Title:       strings.TrimSpace(data.Title),
		DOI:         strings.TrimSpace(data.DOI),
		Collections: append([]string(nil), data.Collections...),
	}
	if item.Key == "" {
		item.Key = key
	}
	if item.Key != key {
		return nil, fmt.Errorf("Zotio returned item key %q for %q", item.Key, key)
	}
	item.Authors = creatorNames(data.Creators)
	item.Year = firstYear(data.Date)
	if item.Year == 0 {
		item.Year = firstYear(envelope.Results.Meta.ParsedDate)
	}
	return item, nil
}

// Sync refreshes Zotio's local mirror before DOI-based import classification.
// The command emits NDJSON progress events; success requires one terminal
// summary with no errored resources.
func (c *Client) Sync(ctx context.Context) error {
	out, err := c.run(ctx, "--agent", "sync")
	if err != nil {
		return fmt.Errorf("syncing Zotio library: %w", err)
	}
	var summary struct {
		Event   string `json:"event"`
		Errored int    `json:"errored"`
	}
	found := false
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event struct {
			Event   string `json:"event"`
			Errored int    `json:"errored"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("decoding Zotio sync event: %w", err)
		}
		if event.Event == "sync_summary" {
			summary = event
			found = true
		}
	}
	if !found {
		return errors.New("Zotio sync did not report a summary")
	}
	if summary.Errored != 0 {
		return fmt.Errorf("Zotio sync reported %d errored resources", summary.Errored)
	}
	return nil
}

// RunJSON invokes a Zotio machine command and requires one JSON document.
// Zotio deliberately exits non-zero for a valid mutation envelope whose
// result is incomplete. Preserve that envelope alongside the process error so
// callers can record the known outcome rather than leave an ambiguous retry
// reservation.
func (c *Client) RunJSON(ctx context.Context, args ...string) (json.RawMessage, error) {
	out, runErr := c.run(ctx, args...)
	var value any
	if err := json.Unmarshal(out, &value); err != nil {
		if runErr != nil {
			return nil, runErr
		}
		return nil, fmt.Errorf("decoding Zotio JSON output: %w", err)
	}
	return json.RawMessage(bytes.TrimSpace(out)), runErr
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	if c.Exec != nil {
		return c.Exec(ctx, args...)
	}
	if strings.TrimSpace(c.Executable) == "" {
		return nil, errors.New("zotio executable is not configured")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	path, err := exec.LookPath(c.Executable)
	if err != nil {
		return nil, fmt.Errorf("locating %q: %w", c.Executable, err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr boundedBuffer
	stdout.max = maxStdoutBytes
	stderr.max = maxStderrBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	// context.Canceled (parent/daemon shutdown) and context.DeadlineExceeded
	// (real timeout) both surface through ctx.Err(); they must not collapse into
	// one retryable "timed out" outcome. Preserve stdout on either path so
	// RunJSON can still recover a mutation envelope the CLI already wrote before
	// being killed, avoiding an ambiguous in-flight reservation.
	if cause := ctx.Err(); cause != nil {
		if errors.Is(cause, context.Canceled) {
			return stdout.Bytes(), fmt.Errorf("zotio command canceled: %w", cause)
		}
		return stdout.Bytes(), fmt.Errorf("zotio command timed out after %s: %w", timeout, cause)
	}
	if stdout.overflow {
		return nil, fmt.Errorf("zotio stdout exceeds %d bytes", maxStdoutBytes)
	}
	if stderr.overflow {
		return nil, fmt.Errorf("zotio stderr exceeds %d bytes", maxStderrBytes)
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("zotio %s: %s", commandName(args), detail)
	}
	return stdout.Bytes(), nil
}

type boundedBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.max - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return original, nil
	}
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return original, nil
}

func parseVersion(out, executable string) (string, error) {
	match := versionRE.FindStringSubmatch(strings.TrimSpace(out))
	if match == nil {
		return "", fmt.Errorf("unexpected zotio version output %q from configured executable %q; it may not be zotio — configure zotio at %s, then retry", strings.TrimSpace(out), executable, executable)
	}
	return match[1], nil
}

func compareVersion(left, right string) int {
	parse := func(value string) [3]int {
		var parts [3]int
		for i, raw := range strings.SplitN(value, ".", 3) {
			parts[i], _ = strconv.Atoi(raw)
		}
		return parts
	}
	a, b := parse(left), parse(right)
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

func requiredSubset(seen map[string]Capability) []Capability {
	paths := []string{"items missing-pdf", "items get", "attachments add", "items add-to-collection", "import scan", "import resolve", "import apply", "sync"}
	out := make([]Capability, 0, len(paths))
	for _, path := range paths {
		out = append(out, seen[path])
	}
	return out
}

func creatorNames(creators []creator) []string {
	collect := func(authorsOnly bool) []string {
		out := make([]string, 0, len(creators))
		for _, creator := range creators {
			if authorsOnly && creator.Type != "author" {
				continue
			}
			name := strings.TrimSpace(creator.Name)
			if name == "" {
				name = strings.TrimSpace(strings.TrimSpace(creator.FirstName) + " " + strings.TrimSpace(creator.LastName))
			}
			if name != "" {
				out = append(out, name)
			}
		}
		return out
	}
	if authors := collect(true); len(authors) != 0 {
		return authors
	}
	return collect(false)
}

func firstYear(value string) int {
	for i := 0; i+4 <= len(value); i++ {
		year, err := strconv.Atoi(value[i : i+4])
		if err == nil && year >= 1000 && year <= 2100 {
			return year
		}
	}
	return 0
}

func commandName(args []string) string {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return "command"
}
