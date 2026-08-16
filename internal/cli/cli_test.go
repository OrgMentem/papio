// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/discovery"
	"papio/internal/ipc"
	"papio/internal/protocol"
	"papio/internal/work"
)

func TestNormalizeIdentifiersAcceptsCommonDOIAndArXivForms(t *testing.T) {
	ids, err := normalizeIdentifiers([]string{"https://doi.org/10.48550/arXiv.2601.12345"}, "", "", "", "", "")
	if err != nil || ids.DOI != "10.48550/arxiv.2601.12345" {
		t.Fatalf("DOI normalization = %+v, %v", ids, err)
	}
	ids, err = normalizeIdentifiers([]string{"arXiv:2601.12345v2"}, "", "", "", "", "")
	if err != nil || ids.ArXiv != "2601.12345v2" {
		t.Fatalf("arXiv normalization = %+v, %v", ids, err)
	}
}

func TestNormalizeIdentifiersRejectsAmbiguousOrMixedInputs(t *testing.T) {
	if _, err := normalizeIdentifiers([]string{"not-an-id"}, "", "", "", "", ""); err == nil {
		t.Fatal("ambiguous identifier accepted")
	}
	if _, err := normalizeIdentifiers([]string{"10.1000/example"}, "10.1000/other", "", "", "", ""); err == nil {
		t.Fatal("positional plus explicit identifier accepted")
	}
}

// The positional argument is the only place papio guesses which scheme the user
// meant, so each accepted bare shape is pinned here, as is the refusal that
// keeps a ten-digit string from silently becoming a PMID.
func TestNormalizeIdentifiersInfersUnprefixedShapes(t *testing.T) {
	for _, test := range []struct {
		name, in string
		want     protocol.Identifiers
	}{
		{"prefixed doi", "doi:10.1002/Example.", protocol.Identifiers{DOI: "10.1002/example"}},
		{"prefixed pmid", "pmid:15676839", protocol.Identifiers{PMID: "15676839"}},
		{"prefixed isbn", "isbn:978-1-4613-3087-5", protocol.Identifiers{ISBN: "9781461330875"}},
		{"bare pmid", "15676839", protocol.Identifiers{PMID: "15676839"}},
		{"bare openalex", "W2036177018", protocol.Identifiers{OpenAlex: "W2036177018"}},
		{"bare arxiv new style", "2301.08745", protocol.Identifiers{ArXiv: "2301.08745"}},
		{"bare arxiv with version", "2301.08745v2", protocol.Identifiers{ArXiv: "2301.08745v2"}},
		{"bare arxiv old style", "math/0211159", protocol.Identifiers{ArXiv: "math/0211159"}},
		// What a user copies out of a browser address bar: openalex.org's web
		// UI serves /works/<id>, not the bare canonical id form.
		{"pasted openalex web url", "https://openalex.org/works/W2036177018", protocol.Identifiers{OpenAlex: "W2036177018"}},
		{"openalex api url", "https://api.openalex.org/works/W2036177018", protocol.Identifiers{OpenAlex: "W2036177018"}},
		{"canonical openalex id url", "https://openalex.org/W2036177018", protocol.Identifiers{OpenAlex: "W2036177018"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ids, err := normalizeIdentifiers([]string{test.in}, "", "", "", "", "")
			if err != nil {
				t.Fatalf("normalizeIdentifiers(%q) = %v", test.in, err)
			}
			if *ids != test.want {
				t.Fatalf("normalizeIdentifiers(%q) = %+v, want %+v", test.in, *ids, test.want)
			}
		})
	}
}

// A ten-digit string is simultaneously a valid ISBN-10 and a valid PMID, so the
// bare form must refuse it rather than pick one. Guarding this: widening the
// PMID width here would silently acquire the wrong work for every bare ISBN-10.
func TestNormalizeIdentifiersRefusesIdentifiersThatNameTwoSchemes(t *testing.T) {
	for _, in := range []string{
		"0306406152",        // ISBN-10 digits, also within PMID width
		"1234567890",        // ten digits, scheme genuinely ambiguous
		"Wnt",               // starts with W but is not an OpenAlex id
		"9781461330875",     // bare ISBN-13 still needs isbn:/--isbn
		"works/W2036177018", // the /works/ segment is stripped inside a URL only, never from a bare argument
		"not-an-id",
	} {
		ids, err := normalizeIdentifiers([]string{in}, "", "", "", "", "")
		if err == nil {
			t.Errorf("normalizeIdentifiers(%q) = %+v, want a cannot-infer error", in, ids)
			continue
		}
		if !strings.Contains(err.Error(), "cannot infer identifier type") {
			t.Errorf("normalizeIdentifiers(%q) error = %q, want the cannot-infer guidance", in, err)
		}
	}
}

// A work carries several identifiers at once, and the daemon stores every one
// it is given, so flags compose instead of excluding one another.
func TestNormalizeIdentifiersComposesSeveralFlags(t *testing.T) {
	ids, err := normalizeIdentifiers(nil, "https://doi.org/10.1000/Example", "12345", "arXiv:2601.12345v2", "", "W2741809807")
	if err != nil {
		t.Fatalf("multiple identifier flags rejected: %v", err)
	}
	if ids.DOI != "10.1000/example" || ids.PMID != "12345" || ids.ArXiv != "2601.12345v2" || ids.OpenAlex != "W2741809807" {
		t.Fatalf("composed identifiers = %+v", ids)
	}
	if ids.ISBN != "" {
		t.Fatalf("unset flag populated an identifier: %+v", ids)
	}
}

func TestSearchCommandAllowsSnowballWithoutQuery(t *testing.T) {
	command := newSearchCommand(&options{})
	if err := command.Flags().Set("cites", "10.1000/seed"); err != nil {
		t.Fatal(err)
	}
	if err := command.Args(command, nil); err != nil {
		t.Fatalf("snowball search without query rejected: %v", err)
	}
	if err := command.Flags().Set("cites", ""); err != nil {
		t.Fatal(err)
	}
	if err := command.Args(command, nil); err == nil {
		t.Fatal("search without query or a snowball DOI succeeded")
	}
	for _, name := range []string{"cites", "cited-by", "related-to", "new-only"} {
		flag := command.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("missing --%s flag", name)
		}
	}
	if got := command.Flags().Lookup("cited-by").Usage; !strings.Contains(got, "backward references") || !strings.Contains(got, "cited_by:") {
		t.Fatalf("cited-by help = %q", got)
	}
}

func TestSearchCommandRendersConfidentMatchMarker(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "discovery.search" {
			t.Fatalf("method = %q, want discovery.search", method)
		}
		*result.(*[]discovery.DiscoveredWork) = []discovery.DiscoveredWork{{
			Work:       work.Work{Year: 2024, Authors: []string{"Ada Lovelace"}, Title: "Attention Is All You Need"},
			CitedBy:    10,
			MatchScore: 1,
			MatchKind:  discovery.MatchExactTitle,
		}}
		return nil
	})
	root.SetArgs([]string{"search", "Attention Is All You Need"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, " | EXACT | ") {
		t.Fatalf("output missing EXACT match marker: %q", got)
	}
	if strings.Contains(got, "no confident title match") {
		t.Fatalf("banner printed despite a confident row: %q", got)
	}
}

func TestSearchCommandPrintsNoConfidentMatchBanner(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*[]discovery.DiscoveredWork) = []discovery.DiscoveredWork{
			{Work: work.Work{Year: 2019, Title: "A Survey of Deep Learning Methods"}, MatchKind: discovery.MatchWeak},
			{Work: work.Work{Year: 2020, Title: "Another Unrelated Review Paper"}, MatchKind: discovery.MatchWeak},
		}
		return nil
	})
	root.SetArgs([]string{"search", "Attention Is All You Need"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, " | WEAK | ") {
		t.Fatalf("output missing WEAK match marker: %q", got)
	}
	want := `no confident title match for "Attention Is All You Need" — showing the closest results anyway` + "\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("output = %q, want it to end with banner %q", got, want)
	}
}

func TestSearchCommandCitationSnowballSuppressesBannerAndLoudMarkers(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*[]discovery.DiscoveredWork) = []discovery.DiscoveredWork{
			{Work: work.Work{Year: 2021, Title: "Some Citing Paper"}, MatchKind: discovery.MatchUnscored},
		}
		return nil
	})
	root.SetArgs([]string{"search", "--cites", "10.1000/seed"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "no confident title match") {
		t.Fatalf("banner printed for a citation snowball with no query: %q", got)
	}
	if strings.Contains(got, "EXACT") || strings.Contains(got, "PHRASE") || strings.Contains(got, "TOKENS") || strings.Contains(got, "WEAK") {
		t.Fatalf("loud match marker printed for an unscored row: %q", got)
	}
	if !strings.Contains(got, " | — | ") {
		t.Fatalf("output missing quiet unscored marker: %q", got)
	}
}

func TestSearchCommandJSONIncludesMatchFields(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*[]discovery.DiscoveredWork) = []discovery.DiscoveredWork{
			{Work: work.Work{Year: 2024, Title: "Attention Is All You Need"}, MatchScore: 1, MatchKind: discovery.MatchExactTitle},
		}
		return nil
	})
	root.SetArgs([]string{"--json", "search", "Attention Is All You Need"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search: %v", err)
	}
	var page map[string]any
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatalf("decode JSON: %v (%q)", err, out.String())
	}
	keys := make([]string, 0, len(page))
	for k := range page {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "truncated" || keys[1] != "works" {
		t.Fatalf("page keys = %v, want exactly [truncated works]", keys)
	}
	rows, ok := page["works"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("works = %#v", page["works"])
	}
	row, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("row = %#v", rows[0])
	}
	if row["match_kind"] != discovery.MatchExactTitle {
		t.Fatalf("match_kind = %v, want %q", row["match_kind"], discovery.MatchExactTitle)
	}
	if score, ok := row["match_score"].(float64); !ok || score != 1 {
		t.Fatalf("match_score = %v", row["match_score"])
	}
}

// discovered.Work.Title/Authors are third-party bibliographic metadata
// (internal/discovery normalizes them with only strings.TrimSpace) and
// discovered.Work.DOI passes through work.NormalizeDOI, which does not strip
// control bytes either (doiCoreRE's \S excludes only [\t\n\f\r ] in RE2).
// Before this fix, `papio search` printed all three straight to the
// terminal — the widest-reach surface of the escape-injection class this
// package's other columns already close.
func TestSearchCommandStripsTerminalControlBytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		work    work.Work
		wantRow string
	}{
		{
			name: "escape and osc sequence in title, author, and doi",
			work: work.Work{
				Year: 2026, Authors: []string{"Evil\x1b]0;pwned\x07 Author"},
				Title: "Evil\x1b[31mTitle", DOI: "10.1000/evil\u009b31m",
			},
			wantRow: "2026 | Evil]0;pwned Author | Evil[31mTitle | 10.1000/evil31m | — | — | 0 citations\n",
		},
		{
			name: "printable non-ASCII survives byte-for-byte",
			work: work.Work{
				Year: 2026, Authors: []string{"Café Über"}, Title: "日本語のタイトル", DOI: "10.1000/plain",
			},
			wantRow: "2026 | Café Über | 日本語のタイトル | 10.1000/plain | — | — | 0 citations\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
				if method != "discovery.search" {
					t.Fatalf("method = %q, want discovery.search", method)
				}
				*result.(*[]discovery.DiscoveredWork) = []discovery.DiscoveredWork{{Work: tc.work}}
				return nil
			})
			root.SetArgs([]string{"search", "--cites", "10.1000/seed"})
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("search: %v", err)
			}
			got := out.String()
			if got != tc.wantRow {
				t.Fatalf("stdout = %q, want %q", got, tc.wantRow)
			}
			for _, r := range got {
				if r == '\n' {
					continue
				}
				if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
					t.Errorf("control byte %#U survived in %q", r, got)
				}
			}
		})
	}
}

// The --json branch marshals discovery.DiscoveredWork directly (never
// through firstAuthor/emptyMarker/the Title strip in the text row above), so
// a control byte in third-party metadata must survive verbatim for a
// machine caller to see and act on the raw value.
func TestSearchCommandJSONPreservesControlBytes(t *testing.T) {
	const title = "Evil\x1b[31mTitle"
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*[]discovery.DiscoveredWork) = []discovery.DiscoveredWork{
			{Work: work.Work{Year: 2024, Title: title}, MatchScore: 1, MatchKind: discovery.MatchExactTitle},
		}
		return nil
	})
	root.SetArgs([]string{"--json", "search", "Evil"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search --json: %v", err)
	}
	var page struct {
		Works []discovery.DiscoveredWork `json:"works"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatalf("decode JSON: %v (%q)", err, out.String())
	}
	if len(page.Works) != 1 || page.Works[0].Work.Title != title {
		t.Fatalf("JSON title = %q, want exact bytes %q", page.Works[0].Work.Title, title)
	}
}

// papio-9007c692bea6c968: encoding/json escapes only bytes below 0x20 plus
// quote and backslash, so DEL and the whole C1 block used to reach the
// operator's terminal raw through --json. U+009B and U+009D are CSI and OSC
// to a UTF-8 terminal — the same escape-injection primitive as ESC, reachable
// with no ESC byte in the input. printJSON escapes them, and that escape must
// be BOTH terminal-safe on the wire and lossless after decoding: --json is
// the authoritative machine-readable form, so stripping is not an option.
func TestJSONOutputEscapesTerminalControlBytesLosslessly(t *testing.T) {
	const title = "Evil\u009b31mTitle\u009dpwned\u007fDEL\u0085NEL"
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*[]discovery.DiscoveredWork) = []discovery.DiscoveredWork{
			{Work: work.Work{Year: 2024, Title: title}, MatchScore: 1, MatchKind: discovery.MatchExactTitle},
		}
		return nil
	})
	root.SetArgs([]string{"--json", "search", "Evil"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search --json: %v", err)
	}

	for _, r := range out.String() {
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			t.Errorf("control byte %#U reached the writer raw in %q", r, out.String())
		}
	}

	var page struct {
		Works []discovery.DiscoveredWork `json:"works"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatalf("decode JSON: %v (%q)", err, out.String())
	}
	if len(page.Works) != 1 || page.Works[0].Work.Title != title {
		t.Fatalf("JSON title = %q, want exact bytes %q", page.Works[0].Work.Title, title)
	}
}

// The escape is a scan-and-rewrite over every --json payload the CLI emits,
// so the overwhelmingly common clean case must not allocate a second buffer.
func TestEscapeJSONTerminalControlsLeavesCleanOutputUntouched(t *testing.T) {
	clean := []byte(`{"title":"Café 日本語","truncated":false}`)
	got := escapeJSONTerminalControls(clean)
	if &got[0] != &clean[0] {
		t.Errorf("clean payload was copied: %q", got)
	}
}

func TestVersionFlagMatchesVersionCommand(t *testing.T) {
	var flagOut, flagErr bytes.Buffer
	flagRoot := NewRoot(&flagOut, &flagErr)
	flagRoot.SetArgs([]string{"--version"})
	if err := flagRoot.Execute(); err != nil {
		t.Fatalf("--version: %v (%s)", err, flagErr.String())
	}

	var cmdOut, cmdErr bytes.Buffer
	cmdRoot := NewRoot(&cmdOut, &cmdErr)
	cmdRoot.SetArgs([]string{"version"})
	if err := cmdRoot.Execute(); err != nil {
		t.Fatalf("version: %v (%s)", err, cmdErr.String())
	}

	if flagOut.String() != cmdOut.String() {
		t.Fatalf("--version output = %q, version command output = %q, want identical", flagOut.String(), cmdOut.String())
	}
	want := "papio " + api.Version + "\n"
	if flagOut.String() != want {
		t.Fatalf("--version output = %q, want %q", flagOut.String(), want)
	}
}

func TestNewWorksOnlyFiltersOwnedResultsWithoutRefetching(t *testing.T) {
	works := []discovery.DiscoveredWork{
		{OpenAlexID: "one"},
		{OpenAlexID: "two", Owned: true, OwnedItemKey: "PDF00001"},
		{OpenAlexID: "three"},
	}
	filtered := newWorksOnly(works)
	if len(filtered) != 2 || filtered[0].OpenAlexID != "one" || filtered[1].OpenAlexID != "three" {
		t.Fatalf("filtered works = %+v", filtered)
	}
	if got := ownedSuffix(true); got != " [in library]" {
		t.Fatalf("owned suffix = %q", got)
	}
	if got := newSearchCommand(&options{}).Flags().Lookup("new-only").Usage; !strings.Contains(got, "after --limit") || !strings.Contains(got, "fewer") {
		t.Fatalf("new-only help = %q", got)
	}
}

func TestConfigInitWritesPrivateStructuredConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	var stdout, stderr bytes.Buffer
	root := NewRoot(&stdout, &stderr)
	root.SetArgs([]string{"--config", path, "--json", "config", "init", "--access-mode", "delegated", "--email", "reader@example.test"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config init: %v (%s)", err, stderr.String())
	}
	var output map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("JSON output: %v (%q)", err, stdout.String())
	}
	if output["access_mode"] != "delegated" || output["config_path"] != path {
		t.Fatalf("output = %v", output)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v", info.Mode().Perm())
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.AccessMode != config.ModeDelegated || cfg.Email != "reader@example.test" {
		t.Fatalf("loaded config = %+v, %v", cfg, err)
	}
}

func TestDaemonPingResultDecodesFullStatus(t *testing.T) {
	var result daemonPingResult
	if err := ipc.DecodeResult(json.RawMessage(`{"status":"ok","version":"1.2.3","extension_connected":true,"extension_version":"4.5.6","pending_browser_sessions":2,"browser_session_denied":3,"update_available":true,"latest_version":"1.2.4","zotio_update_available":true,"zotio_latest_version":"5.6.7"}`), &result); err != nil {
		t.Fatalf("decode ping result: %v", err)
	}
	if result.Status != "ok" || result.Version != "1.2.3" || !result.ExtensionConnected || result.ExtensionVersion != "4.5.6" || result.PendingBrowserSessions != 2 || result.BrowserSessionDenied != 3 || !result.UpdateAvailable || result.LatestVersion != "1.2.4" || !result.ZotioUpdateAvailable || result.ZotioLatestVersion != "5.6.7" {
		t.Fatalf("ping result = %+v", result)
	}
}

func TestCallWarnsOnceForVersionSkew(t *testing.T) {
	opt, _, stderr := versionWarningTestOptions(api.Version + "-old")
	for range 2 {
		if err := opt.call(context.Background(), "jobs.list", struct{}{}, &struct{}{}); err != nil {
			t.Fatalf("call: %v", err)
		}
	}
	want := "papio: daemon is running " + api.Version + "-old but this CLI is " + api.Version + " — run 'papio daemon stop'; the next command starts the matching daemon\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestCallDoesNotWarnWhenDaemonVersionMatches(t *testing.T) {
	opt, _, stderr := versionWarningTestOptions(api.Version)
	if err := opt.call(context.Background(), "jobs.list", struct{}{}, &struct{}{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestCallSkipsVersionWarningWhenItStartsDaemon(t *testing.T) {
	opt, _, stderr := versionWarningTestOptions(api.Version + "-old")
	started := false
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	opt.newAutostarter = func(socket string) *daemon.Autostarter {
		return &daemon.Autostarter{
			SocketPath: socket,
			LockPath:   filepath.Join(t.TempDir(), "daemon.lock"),
			LogPath:    logPath,
			Executable: func() (string, error) { return "/test/papio", nil },
			Command:    func(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) },
			Start: func(context.Context, *exec.Cmd) error {
				started = true
				return nil
			},
			Ready: func(context.Context, string) error {
				if started {
					return nil
				}
				return errors.New("not ready")
			},
		}
	}
	if err := opt.call(context.Background(), "jobs.list", struct{}{}, &struct{}{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !started {
		t.Fatal("call did not start the daemon")
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestCallVersionWarningLeavesJSONOutputClean(t *testing.T) {
	opt, stdout, stderr := versionWarningTestOptions(api.Version + "-old")
	opt.jsonOutput = true
	if err := opt.call(context.Background(), "jobs.list", struct{}{}, &struct{}{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if err := opt.printResult(map[string]string{"status": "ok"}, "ignored"); err != nil {
		t.Fatalf("print JSON result: %v", err)
	}
	if got := stdout.String(); got != "{\"status\":\"ok\"}\n" {
		t.Fatalf("stdout = %q, want JSON only", got)
	}
	if got := stderr.String(); !strings.Contains(got, "papio: daemon is running ") {
		t.Fatalf("stderr = %q, want version warning", got)
	}
}

func TestCallHintsForAvailableUpdateOncePerDay(t *testing.T) {
	dataDir := t.TempDir()
	newOptions := func() (*options, *bytes.Buffer) {
		opt, _, stderr := versionWarningTestOptions(api.Version)
		opt.configLoader = func(string) (config.Config, error) {
			return config.Config{DataDir: dataDir, Updates: config.Updates{Check: true}}, nil
		}
		opt.rpcCall = func(_ context.Context, _ string, method string, _ any, result any) error {
			if method == "ping" {
				status := result.(*daemonPingResult)
				status.Version = api.Version
				status.UpdateAvailable = true
				status.LatestVersion = "99.0.0"
			}
			return nil
		}
		return opt, stderr
	}
	first, firstStderr := newOptions()
	for range 2 {
		if err := first.call(context.Background(), "jobs.list", struct{}{}, &struct{}{}); err != nil {
			t.Fatalf("first call: %v", err)
		}
	}
	want := "papio: updates available: papio 99.0.0 (you have " + api.Version + ") — run 'papio doctor' for details\n"
	if got := firstStderr.String(); got != want {
		t.Fatalf("first stderr = %q, want %q", got, want)
	}

	second, secondStderr := newOptions()
	if err := second.call(context.Background(), "jobs.list", struct{}{}, &struct{}{}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := secondStderr.String(); got != "" {
		t.Fatalf("second stderr = %q, want empty due to persisted nag", got)
	}
}

func TestCallHintIncludesCachedZotioUpdate(t *testing.T) {
	dataDir := t.TempDir()
	cache := `{"latest_version":"1.1.0","url":"https://example.test/zotio","installed_version":"1.0.0"}`
	if err := os.WriteFile(filepath.Join(dataDir, "update-cache-zotio.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	opt, _, stderr := versionWarningTestOptions(api.Version)
	opt.configLoader = func(string) (config.Config, error) {
		return config.Config{DataDir: dataDir, Updates: config.Updates{Check: true}}, nil
	}
	opt.rpcCall = func(_ context.Context, _ string, method string, _ any, result any) error {
		if method == "ping" {
			status := result.(*daemonPingResult)
			status.Version = api.Version
			status.UpdateAvailable = true
			status.LatestVersion = "99.0.0"
		}
		return nil
	}
	if err := opt.call(context.Background(), "jobs.list", struct{}{}, &struct{}{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	want := "papio: updates available: papio 99.0.0 (you have " + api.Version + "), zotio 1.1.0 (you have 1.0.0) — run 'papio doctor' for details\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func versionWarningTestOptions(daemonVersion string) (*options, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	opt := &options{
		out:    &stdout,
		errOut: &stderr,
		configLoader: func(string) (config.Config, error) {
			return config.Config{DataDir: "/test/data"}, nil
		},
		newAutostarter: func(socket string) *daemon.Autostarter {
			return &daemon.Autostarter{
				SocketPath: socket,
				Ready:      func(context.Context, string) error { return nil },
			}
		},
		rpcCall: func(_ context.Context, _ string, method string, _ any, result any) error {
			if method == "ping" {
				result.(*daemonPingResult).Version = daemonVersion
			}
			return nil
		},
	}
	return opt, &stdout, &stderr
}

// TestSearchNewOnlyPreservesTruncationFromThePreFilterPage pins the
// truncated-honesty fix for --new-only: the daemon caps the page at the
// effective --limit, so a full page whose rows are then ALL filtered out by
// --new-only must still report truncated:true — the cap hid rows, even
// though none of the hidden or shown rows survived the owned-work filter.
func TestSearchNewOnlyPreservesTruncationFromThePreFilterPage(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		if method != "discovery.search" {
			t.Fatalf("method = %q, want discovery.search", method)
		}
		limit := params.(discovery.SearchParams).Limit
		works := make([]discovery.DiscoveredWork, limit)
		for i := range works {
			works[i] = discovery.DiscoveredWork{Owned: true}
		}
		*result.(*[]discovery.DiscoveredWork) = works
		return nil
	})
	root.SetArgs([]string{"--json", "search", "--new-only", "--limit", "3", "quantum gravity"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("search --new-only: %v (%s)", err, errOut.String())
	}
	var page struct {
		Works     []discovery.DiscoveredWork `json:"works"`
		Truncated bool                       `json:"truncated"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Works) != 0 {
		t.Fatalf("works = %d, want 0 — every returned row was owned and --new-only filters them", len(page.Works))
	}
	if !page.Truncated {
		t.Fatal("truncated = false after --new-only emptied a full page — the daemon's pre-filter page hit --limit, so more may exist; want true")
	}
}
