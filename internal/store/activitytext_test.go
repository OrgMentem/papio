// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import "testing"

// The activity feed is the operator's window into what papio did; a raw
// event kind leaking through (e.g. "action.reminder") reads as a bug, which
// is exactly what the first feedback round reported. Pin the friendly
// mappings for every kind the daemon writes today plus the fallback.
func TestActivityTextCoversWrittenEventKinds(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		detail map[string]any
		want   string
	}{
		{"job.transition", map[string]any{"to": "awaiting_human", "reason": "login_required"}, "Moved to awaiting human (login required)"},
		{"job.transition", map[string]any{"to": "resolving"}, "Moved to resolving"},
		{"job.transition", nil, "Job state changed"},
		{"job.superseded", nil, "Superseded by a newer request"},
		{"action.reminder", map[string]any{"age_seconds": int64(8 * 3600)}, "Still waiting on you (open for 8h)"},
		{"action.reminder", nil, "Still waiting on you"},
		{"browser.download_complete", map[string]any{"filename": "paper.pdf", "size_bytes": int64(1258291)}, "Download complete (paper.pdf, 1.2 MB)"},
		{"browser.error", map[string]any{"code": "download_not_pdf"}, "Browser reported an error (download_not_pdf)"},
		{"browser.page_capture", nil, "Diagnostic page captured"},
		{"browser.provider_outcome", map[string]any{"outcome": "no_entitlement"}, "Provider outcome: no entitlement"},
		{"browser.no_entitlement_requeue", nil, "No entitlement here — requeued for other routes"},
		{"browser.handoff_reoffered", nil, "Handoff re-offered (institution session live)"},
		{"acquisition.component_added", map[string]any{"role": "supplement"}, "Added supplement component"},
		{"zotio.auto_import", map[string]any{"status": "applied"}, "Imported into Zotero"},
		{"zotio.collection_filing", nil, "Filed into Zotero collection"},
		{"zotio.enrich", nil, "Zotero metadata enriched"},
		{"hook.on_ready", map[string]any{"status": "ok"}, "On-ready hook ran (ok)"},
		// Unknown kinds fall through verbatim rather than erroring.
		{"future.kind", nil, "future.kind"},
	} {
		if got := ActivityText(tc.kind, tc.detail); got != tc.want {
			t.Errorf("ActivityText(%q, %v) = %q, want %q", tc.kind, tc.detail, got, tc.want)
		}
	}
}

// `papio activity` prints this text straight to a terminal, and a
// browser-reported download filename is attacker-influenced: the protocol's
// filenameRE forbids only the path separators, so ESC and every other C0 byte
// reach here. An unescaped ESC lets a provider page, a spoofed
// Content-Disposition, or a compromised browser session inject ANSI/OSC
// sequences into the operator's terminal on the next `papio activity` run.
// C1 controls (U+0080-U+009F) get the same treatment: a UTF-8 xterm decodes
// U+009B/U+009D as CSI/OSC introducers with no ESC byte in the input at all.
func TestActivityTextStripsTerminalControlBytes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		filename string
		want     string
	}{
		{"escape sequence", "a\x1b[31mred.pdf", "Download started (a[31mred.pdf)"},
		{"osc title set", "x\x1b]0;pwned\x07.pdf", "Download started (x]0;pwned.pdf)"},
		{"carriage return overwrite", "safe.pdf\rmalicious", "Download started (safe.pdfmalicious)"},
		{"newline row break", "one\ntwo.pdf", "Download started (onetwo.pdf)"},
		{"nul still stripped", "a\x00b.pdf", "Download started (ab.pdf)"},
		{"del", "a\x7fb.pdf", "Download started (ab.pdf)"},
		{"c1 csi", "a\u009b31mred.pdf", "Download started (a31mred.pdf)"},
		{"c1 osc", "x\u009d0;pwned.pdf", "Download started (x0;pwned.pdf)"},
		{"printable text survives intact", "Ünïcode paper (2026).pdf", "Download started (Ünïcode paper (2026).pdf)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ActivityText("browser.download_started", map[string]any{"filename": tc.filename})
			if got != tc.want {
				t.Errorf("ActivityText = %q, want %q", got, tc.want)
			}
			for _, r := range got {
				if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
					t.Errorf("control byte %#U survived in %q", r, got)
				}
			}
		})
	}
}

// StripTerminalControls is the single choke point every terminal-printed
// surface (clampActivityText, the CLI's compactActivitySummary, the browser
// bridge's activityTitle) routes through. Pin its contract directly: C0,
// DEL, and C1 all removed; ordinary printable text — including non-ASCII —
// passes through byte-for-byte untouched.
func TestStripTerminalControls(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"c0 control", "a\x01\x1bb", "ab"},
		{"del", "a\x7fb", "ab"},
		{"c1 low", "a\u0080b", "ab"},
		// A bare 0x80 byte is not U+0080, it is invalid UTF-8. strings.Map
		// folds it to U+FFFD, which is inert on a terminal. JSON decoding
		// means the wire cannot deliver one anyway; pinned so a future
		// rewrite cannot start passing raw bytes through instead.
		{"invalid utf-8 byte folded, not passed through", "a\x80b", "a\uFFFDb"},
		{"c1 csi", "a\u009bb", "ab"},
		{"c1 osc", "a\u009db", "ab"},
		{"c1 high", "a\u009fb", "ab"},
		{"accented latin preserved", "Café Über Nïño", "Café Über Nïño"},
		{"cjk preserved", "日本語のタイトル", "日本語のタイトル"},
		{"mixed printable and control", "Title\u009b[31m: \x1bDanger\x07", "Title[31m: Danger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := StripTerminalControls(tc.input)
			if got != tc.want {
				t.Errorf("StripTerminalControls(%q) = %q, want %q", tc.input, got, tc.want)
			}
			for _, r := range got {
				if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
					t.Errorf("control byte %#U survived in %q", r, got)
				}
			}
		})
	}
}

func TestFormatActivityAge(t *testing.T) {
	for _, tc := range []struct {
		seconds int64
		want    string
	}{
		{45, "45s"},
		{240, "4m"},
		{8 * 3600, "8h"},
		{3 * 86400, "3d"},
	} {
		if got := formatActivityAge(tc.seconds); got != tc.want {
			t.Errorf("formatActivityAge(%d) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}
