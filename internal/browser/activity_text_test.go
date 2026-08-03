package browser

import "testing"

// The activity feed is the operator's window into what papio did; a raw
// event kind leaking through (e.g. "action.reminder") reads as a bug, which
// is exactly what the first feedback round reported. Pin the friendly
// mappings for every kind the daemon writes today plus the fallback.
func TestKindTextCoversWrittenEventKinds(t *testing.T) {
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
		{"browser.error", map[string]any{"code": "download_failed"}, "Browser reported an error (download_failed)"},
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
		if got := kindText(tc.kind, tc.detail); got != tc.want {
			t.Errorf("kindText(%q, %v) = %q, want %q", tc.kind, tc.detail, got, tc.want)
		}
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
