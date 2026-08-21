// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"fmt"
	"strings"
)

// ActivityText renders one event as a short human-readable summary. It is the
// single source for every operator surface — the CLI's `papio activity` line
// view and the browser bridge's activity_response `text` field — so humans and
// agents read the same story regardless of surface. Unknown kinds fall through
// verbatim rather than erroring: the feed is display-only and best-effort.
func ActivityText(kind string, detail map[string]any) string {
	filename := activityDetailString(detail, "filename")
	switch kind {
	case "browser.download_started":
		if filename != "" {
			return clampActivityText(fmt.Sprintf("Download started (%s)", filename))
		}
		return "Download started"
	case "browser.download_complete":
		if filename != "" {
			if size, ok := activityDetailInt64(detail, "size_bytes"); ok {
				return clampActivityText(fmt.Sprintf("Download complete (%s, %s)", filename, formatActivityBytes(size)))
			}
			return clampActivityText(fmt.Sprintf("Download complete (%s)", filename))
		}
		return "Download complete"
	case "browser.adoption_deferred":
		if filename != "" {
			return clampActivityText(fmt.Sprintf("Download needs attention (%s)", filename))
		}
		return "Download needs attention"
	case "browser.auth_pending":
		return "Institution login required"
	case "browser.auth_returned":
		return "Institution login returned"
	case "browser.session_evidence":
		return "Institution session verified — re-offering blocked work"
	case "browser.delivery_context":
		route := activityDetailString(detail, "route")
		evidence := strings.ReplaceAll(activityDetailString(detail, "session_evidence"), "_", " ")
		if route != "" && evidence != "" {
			return clampActivityText(fmt.Sprintf("Access route recorded (%s, %s session)", route, evidence))
		}
		return "Access route recorded"
	case "browser.handoff_offered":
		return "Institution access handoff offered"
	case "browser.handoff_failed":
		if outcome := activityDetailString(detail, "outcome"); outcome != "" {
			return clampActivityText(fmt.Sprintf("Institution handoff failed (%s)", outcome))
		}
		return "Institution handoff failed"
	case "browser.handoff_epochs_reset":
		return "Drive attempt count reset by repair"
	case "browser.job_accept":
		return "Job accepted"
	case "browser.job_reject":
		return "Job rejected"
	case "browser.oa_handoff_fallback":
		return "Fell back to open-access handoff"
	case "browser.error":
		if code := activityDetailString(detail, "code"); code != "" {
			return clampActivityText(fmt.Sprintf("Browser reported an error (%s)", code))
		}
		return "Browser reported an error"
	case "browser.page_capture":
		return "Diagnostic page captured"
	case "browser.provider_outcome":
		if outcome := activityDetailString(detail, "outcome"); outcome != "" {
			return clampActivityText(fmt.Sprintf("Provider outcome: %s", strings.ReplaceAll(outcome, "_", " ")))
		}
		return "Provider outcome reported"
	case "browser.no_entitlement_requeue":
		return "No entitlement here — requeued for other routes"
	case "browser.handoff_reoffered":
		return "Handoff re-offered (institution session live)"
	case "job.transition":
		to := strings.ReplaceAll(activityDetailString(detail, "to"), "_", " ")
		if to == "" {
			return "Job state changed"
		}
		if reason := activityDetailString(detail, "reason"); reason != "" {
			return clampActivityText(fmt.Sprintf("Moved to %s (%s)", to, strings.ReplaceAll(reason, "_", " ")))
		}
		return clampActivityText("Moved to " + to)
	case "job.superseded":
		return "Superseded by a newer request"
	case "action.reminder":
		if age, ok := activityDetailInt64(detail, "age_seconds"); ok && age > 0 {
			return clampActivityText(fmt.Sprintf("Still waiting on you (open for %s)", formatActivityAge(age)))
		}
		return "Still waiting on you"
	case "acquisition.component_added":
		if role := activityDetailString(detail, "role"); role != "" {
			return clampActivityText(fmt.Sprintf("Added %s component", strings.ReplaceAll(role, "_", " ")))
		}
		return "Component added"
	case "zotio.auto_import":
		switch activityDetailString(detail, "status") {
		case "applied":
			return "Imported into Zotero"
		case "skipped":
			return "Zotero import skipped"
		default:
			return "Zotero import attempted"
		}
	case "zotio.collection_filing":
		return "Filed into Zotero collection"
	case "zotio.enrich":
		return "Zotero metadata enriched"
	case "hook.on_ready":
		if status := activityDetailString(detail, "status"); status != "" {
			return clampActivityText(fmt.Sprintf("On-ready hook ran (%s)", status))
		}
		return "On-ready hook ran"
	case "notify.attempted":
		if count, ok := activityDetailInt64(detail, "count"); ok && count > 1 {
			return clampActivityText(fmt.Sprintf("Notification attempted for %d items", count))
		}
		return "Notification attempted"
	case "notify.held":
		if reason := strings.ReplaceAll(activityDetailString(detail, "reason"), "_", " "); reason != "" {
			return clampActivityText(fmt.Sprintf("Notification held (%s)", reason))
		}
		return "Notification held"
	case "notify.digest":
		if count, ok := activityDetailInt64(detail, "count"); ok && count > 1 {
			return clampActivityText(fmt.Sprintf("Notification digest queued for %d items", count))
		}
		return "Notification digest queued"
	default:
		return clampActivityText(kind)
	}
}

func activityDetailString(detail map[string]any, key string) string {
	if detail == nil {
		return ""
	}
	value, ok := detail[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func activityDetailInt64(detail map[string]any, key string) (int64, bool) {
	if detail == nil {
		return 0, false
	}
	value, ok := detail[key]
	if !ok {
		return 0, false
	}
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		if number >= 0 && number <= float64(^uint64(0)>>1) && number == float64(int64(number)) {
			return int64(number), true
		}
	}
	return 0, false
}

func formatActivityBytes(size int64) string {
	if size < 0 {
		return ""
	}
	const unit = 1024
	value := float64(size)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	index := 0
	for value >= unit && index < len(units)-1 {
		value /= unit
		index++
	}
	if index == 0 {
		return fmt.Sprintf("%d B", size)
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}

// formatActivityAge renders a duration in seconds as a coarse human unit.
func formatActivityAge(seconds int64) string {
	switch {
	case seconds < 90:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 90*60:
		return fmt.Sprintf("%dm", (seconds+30)/60)
	case seconds < 36*3600:
		return fmt.Sprintf("%dh", (seconds+1800)/3600)
	default:
		return fmt.Sprintf("%dd", (seconds+43200)/86400)
	}
}

// StripTerminalControls removes every code point that a terminal emulator
// could interpret as part of a control sequence rather than display text:
// C0 controls (r < 0x20), DEL (0x7f), and the C1 block (0x80-0x9f). C1 matters
// because a UTF-8 xterm decodes U+009B and U+009D as CSI and OSC introducers
// respectively — the same escape-injection primitive as ESC, just reachable
// without an ESC byte in the input. This is the one choke point for any
// string that reaches an operator's terminal or another untrusted-input
// surface; route new callers through it rather than writing a second filter.
func StripTerminalControls(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, value)
}

// clampActivityText bounds one summary line; the browser protocol carries it
// inside a frame with a hard size cap and the CLI prints it on one row.
//
// It runs every value through StripTerminalControls before truncating.
// `papio activity` writes this string straight to the operator's terminal,
// and the one attacker-influenced field that reaches it — a browser-reported
// download filename, whose protocol regex forbids only the path separators —
// could otherwise carry ESC (or a raw C1 byte) and inject ANSI/OSC escape
// sequences into that terminal. Stripping at this single choke point also
// keeps any detail field that becomes untrusted later safe by default,
// rather than re-opening the hole silently.
//
// The --json path deliberately does NOT go through this function: it is the
// machine-readable, authoritative form and must preserve the filename byte
// for byte, including any control bytes, so a consumer can recover the exact
// on-disk name. It is terminal-safe by a different mechanism — the CLI's
// printJSON escapes DEL and the C1 block as \uXXXX, which every conformant
// JSON parser decodes back to the original code point, so the value survives
// losslessly while the bytes reaching a terminal cannot introduce a CSI or
// OSC sequence. Do not route --json through StripTerminalControls to close
// that gap: escaping preserves the filename, stripping corrupts it.
func clampActivityText(value string) string {
	value = StripTerminalControls(value)
	runes := []rune(value)
	if len(runes) <= 160 {
		return value
	}
	return string(runes[:157]) + "..."
}
