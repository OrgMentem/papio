// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"papio/internal/api"
	"papio/internal/job"
	"papio/internal/pdf"
)

// adapterRepairDOIRE bounds the only page-independent label a repair may carry
// into generated TypeScript and operator instructions. It matches plan.ts's
// DOI evidence shape, restricted to printable ASCII so no control byte can
// change meaning between Go, JavaScript and a shell.
var adapterRepairDOIRE = regexp.MustCompile(`^10\.[0-9]{4,9}/[\x21-\x7e]{1,200}$`)

// adapterRepairRecovery is the durable evidence that a validated fallback
// acquired the work whose page defeated the declarative adapter. Every field
// is copied from the daemon's job history and artifact row; nothing here is
// supplied by the repair tool, the decision backend or the candidate patch.
//
// FixtureSHA256 on the repair manifest names the sanitized page capture; the
// Artifact digest names the adopted PDF. They are different documents.
type adapterRepairRecovery struct {
	JobID          string                `json:"job_id"`
	WorkDOI        string                `json:"work_doi"`
	CaptureSeq     int64                 `json:"capture_event_seq"`
	FailureSeq     int64                 `json:"failure_event_seq"`
	FailureOutcome string                `json:"failure_outcome"`
	ReadySeq       int64                 `json:"ready_event_seq"`
	Delivery       string                `json:"delivery"`
	Route          string                `json:"route"`
	AgentDecisions int                   `json:"agent_decisions"`
	ExplicitOpens  int                   `json:"explicit_opens"`
	Artifact       adapterRepairArtifact `json:"artifact"`
}

type adapterRepairArtifact struct {
	SHA256         string `json:"sha256"`
	SizeBytes      int64  `json:"size_bytes"`
	PageCount      int    `json:"page_count"`
	IdentityResult string `json:"identity_result"`
}

// repairRouteTemporal marks a link whose only failure record is an agent
// decision reservation with no completion receipt: capture, reservation and
// ready are ordered in one job, but nothing shows the fallback acquired it.
const repairRouteTemporal = "temporal_correlation"

// correlates reports whether the link is fallback evidence strong enough to
// stand in for daemon correlation at the revision gate.
func (r *adapterRepairRecovery) correlates() bool {
	return r != nil && r.Route != repairRouteTemporal
}

const (
	repairDeliveryBrowser    = "browser_download"
	repairDeliveryNative     = "native_download"
	repairDeliveryNativeSave = "native_viewer_save"
)

type repairEvent struct {
	seq    int64
	kind   string
	detail map[string]any
}

func repairEvents(raw []map[string]any) ([]repairEvent, error) {
	events := make([]repairEvent, 0, len(raw))
	for _, event := range raw {
		kind, _ := event["kind"].(string)
		seq, ok := repairEventSeq(event["seq"])
		if kind == "" || !ok {
			return nil, errors.New("job history contains an event without a kind or sequence number")
		}
		detail, _ := event["detail"].(map[string]any)
		events = append(events, repairEvent{seq: seq, kind: kind, detail: detail})
	}
	return events, nil
}

// repairEventSeq accepts the JSON number the IPC decoder produces and the
// integer the in-process store returns; a fractional or missing value is not
// an ordering the linkage can rely on.
func repairEventSeq(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		if v < 1 || v != math.Trunc(v) || v > math.MaxInt64/2 {
			return 0, false
		}
		return int64(v), true
	case int64:
		return v, v > 0
	case int:
		return int64(v), v > 0
	default:
		return 0, false
	}
}

func repairDetailString(detail map[string]any, key string) string {
	value, _ := detail[key].(string)
	return value
}

func normalizeRepairDOI(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// validatedRepairArtifact reads the digest row for size and page count. Its
// identity_result is shared by every job that accepted the same bytes, so the
// job's own ready transition decides whether a person overrode identity; this
// check only refuses a row that does not even claim a pass.
func validatedRepairArtifact(row *job.Row, artifact *job.Artifact) (adapterRepairArtifact, error) {
	if artifact == nil {
		return adapterRepairArtifact{}, errors.New("job has no validated artifact row")
	}
	if !validSHA256(artifact.SHA256) || artifact.SHA256 != row.ArtifactSHA256 {
		return adapterRepairArtifact{}, errors.New("artifact row does not match the job's accepted artifact digest")
	}
	if artifact.MIME != "application/pdf" {
		return adapterRepairArtifact{}, fmt.Errorf("artifact is %q, not application/pdf", artifact.MIME)
	}
	if artifact.PageCount < 1 {
		return adapterRepairArtifact{}, errors.New("artifact has no recorded page count")
	}
	if artifact.IdentityResult != pdf.IdentityPass {
		return adapterRepairArtifact{}, fmt.Errorf("artifact identity result is %q, not %q", artifact.IdentityResult, pdf.IdentityPass)
	}
	return adapterRepairArtifact{
		SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes,
		PageCount: artifact.PageCount, IdentityResult: artifact.IdentityResult,
	}, nil
}

// acceptedJobState is a job whose validated artifact stands; a Zotero import
// after ready keeps the accepted digest and its ready event.
func acceptedJobState(state string) bool {
	return state == job.StateReady || state == job.StateImported
}

var (
	errReadyMissing    = errors.New("the job did not accept its artifact through validation")
	errReadyOverridden = errors.New("the artifact was accepted by a human identity override, not by validation")
)

// readyTransitionAfter returns the latest validating-to-ready transition after
// seq that accepted exactly the job's current artifact. A human identity
// override is the job-scoped record of a reviewer label and never counts.
func readyTransitionAfter(events []repairEvent, after int64, sha string) (int64, error) {
	var ready int64
	overridden := false
	for _, event := range events {
		if event.seq > after && event.kind == "job.transition" &&
			repairDetailString(event.detail, "from") == job.StateValidating &&
			repairDetailString(event.detail, "to") == job.StateReady &&
			repairDetailString(event.detail, "sha256") == sha && event.seq > ready {
			ready = event.seq
			overridden = repairDetailString(event.detail, "reason") == "human_identity_override"
		}
	}
	if ready == 0 {
		return 0, errReadyMissing
	}
	if overridden {
		return 0, errReadyOverridden
	}
	return ready, nil
}

// deliveryBetween names the strongest delivery evidence in (after, before).
// Native admissions are exact job-bound receipts; a browser download start is
// the extension's report of a download it associated with the job.
func deliveryBetween(events []repairEvent, after, before int64) string {
	delivery := ""
	for _, event := range events {
		if event.seq <= after || event.seq >= before {
			continue
		}
		switch event.kind {
		case "browser.native_viewer_save_admitted":
			delivery = repairDeliveryNativeSave
		case "browser.native_download_admitted":
			if delivery != repairDeliveryNativeSave {
				delivery = repairDeliveryNative
			}
		case "browser.download_started":
			if delivery == "" {
				delivery = repairDeliveryBrowser
			}
		}
	}
	return delivery
}

func countRepairEvents(events []repairEvent, after, before int64, match func(repairEvent) bool) int {
	count := 0
	for _, event := range events {
		if event.seq > after && event.seq < before && match(event) {
			count++
		}
	}
	return count
}

func isAgentDecisionEvent(event repairEvent) bool {
	return event.kind == "browser.agent_decision_requested" || event.kind == "browser.agent_decision_completed"
}

// linkAdapterRepairRecovery proves, from the daemon's own record, that the
// capture being repaired is the declarative failure of one job and that the
// same job then reached ready with a validated PDF through a browser delivery.
// It refuses rather than guessing when any link is missing.
func linkAdapterRepairRecovery(capture adapterRepairCapture, jobID string, detail api.JobDetail, artifact *job.Artifact) (adapterRepairRecovery, error) {
	row := detail.Job
	if row == nil || row.ID != jobID {
		return adapterRepairRecovery{}, fmt.Errorf("daemon returned no job %q", jobID)
	}
	if !acceptedJobState(row.State) || row.ArtifactSHA256 == "" {
		return adapterRepairRecovery{}, fmt.Errorf("recovery job %s is %s, not ready with an accepted artifact", jobID, row.State)
	}
	doi := normalizeRepairDOI(row.Work.DOI)
	if !adapterRepairDOIRE.MatchString(doi) {
		return adapterRepairRecovery{}, errors.New("recovery labelling requires a DOI-identified work")
	}
	events, err := repairEvents(detail.Events)
	if err != nil {
		return adapterRepairRecovery{}, err
	}

	capturePath := filepath.Clean(capture.Path)
	var captureSeq int64
	for _, event := range events {
		if event.kind != "browser.page_capture" || filepath.Clean(repairDetailString(event.detail, "path")) != capturePath {
			continue
		}
		if repairDetailString(event.detail, "adapter_id") != capture.Provider ||
			repairDetailString(event.detail, "adapter_version") != capture.AdapterVersion ||
			repairDetailString(event.detail, "scenario") != capture.Scenario {
			return adapterRepairRecovery{}, errors.New("recovery job recorded this capture with different adapter or scenario metadata")
		}
		captureSeq = event.seq
		break
	}
	if captureSeq == 0 {
		return adapterRepairRecovery{}, fmt.Errorf("recovery job %s did not record this capture", jobID)
	}

	// The declarative failure is recorded one of two ways. A parked attempt
	// reports ui_changed for the adapter version. When the agent fallback is
	// available the extension starts it instead of reporting, so the first
	// daemon-reserved decision after the capture is the failure record.
	var failureSeq int64
	failure, requestID := "", ""
	for _, event := range events {
		if event.seq <= captureSeq {
			continue
		}
		if event.kind == "browser.provider_outcome" &&
			repairDetailString(event.detail, "outcome") == "ui_changed" &&
			repairDetailString(event.detail, "adapter_id") == capture.Provider &&
			repairDetailString(event.detail, "adapter_version") == capture.AdapterVersion {
			failureSeq, failure = event.seq, "ui_changed"
			break
		}
		if event.kind == "browser.agent_decision_requested" {
			failureSeq, failure = event.seq, "agent_fallback"
			requestID = repairDetailString(event.detail, "request_id")
			break
		}
	}
	if failureSeq == 0 {
		return adapterRepairRecovery{}, errors.New("recovery job has no ui_changed outcome or agent fallback after the capture")
	}

	readySeq, err := readyTransitionAfter(events, failureSeq, row.ArtifactSHA256)
	if err != nil {
		return adapterRepairRecovery{}, fmt.Errorf("after the declarative failure, %w", err)
	}
	delivery := deliveryBetween(events, failureSeq, readySeq)
	if delivery == "" {
		// A resolver fetch or artifact reuse proves nothing about the page.
		return adapterRepairRecovery{}, errors.New("recovery job reached ready without a browser delivery after the declarative failure")
	}
	validated, err := validatedRepairArtifact(row, artifact)
	if err != nil {
		return adapterRepairRecovery{}, err
	}

	decisions := countRepairEvents(events, failureSeq, readySeq, func(event repairEvent) bool {
		return event.kind == "browser.agent_decision_completed" && repairDetailString(event.detail, "outcome") == "decision"
	})
	// A reservation alone is not fallback evidence: the daemon writes it
	// before inference, so the ready that follows may owe nothing to the
	// fallback. Only the same request's completion receipt makes the pair a
	// fallback; without it the link is temporal and cannot unlock a revision.
	fallbackCompleted := failure != "agent_fallback" || requestID != "" &&
		countRepairEvents(events, failureSeq, readySeq, func(event repairEvent) bool {
			return event.kind == "browser.agent_decision_completed" && repairDetailString(event.detail, "request_id") == requestID
		}) > 0
	route := "unattributed"
	switch {
	case !fallbackCompleted:
		route = repairRouteTemporal
	case decisions > 0:
		route = "agent_decision"
	case delivery == repairDeliveryNativeSave:
		route = repairDeliveryNativeSave
	}
	opens := countRepairEvents(events, 0, math.MaxInt64, func(event repairEvent) bool { return event.kind == "handoff.opened" })
	return adapterRepairRecovery{
		JobID: jobID, WorkDOI: doi, CaptureSeq: captureSeq, FailureSeq: failureSeq,
		FailureOutcome: failure, ReadySeq: readySeq, Delivery: delivery, Route: route,
		AgentDecisions: decisions, ExplicitOpens: opens, Artifact: validated,
	}, nil
}

func adapterRepairRecoveryReport(recovery *adapterRepairRecovery) string {
	if recovery == nil {
		return "\n## validated fallback evidence\n\nNone linked. Pass `--recovery-job <job-id>` for a job that recorded this capture, failed declaratively, and then reached ready. canary.md is written only for a linked proposal.\n"
	}
	route := "The durable history does not attribute the delivery to an agent decision or native save; it may have been a manual download."
	switch recovery.Route {
	case repairRouteTemporal:
		route = "The failure record is an agent decision reservation with no completion receipt, so this link is temporal correlation only: it labels the regression but does not unlock the revision."
	case "agent_decision":
		route = fmt.Sprintf("%d agent decision(s) completed between the failure and the delivery.", recovery.AgentDecisions)
	case repairDeliveryNativeSave:
		route = "A deterministic native viewer save produced the delivery."
	}
	return fmt.Sprintf(`
## validated fallback evidence

Job `+"`%s`"+` recorded this capture (event %d), then recorded the declarative failure as `+"`%s`"+` (event %d), and accepted artifact `+"`%s`"+` (event %d) through a %s: %d pages, %d bytes, identity result `+"`%s`"+`. %s Explicit Open actions in the job: %d.

This supports: the captured page belongs to the work `+"`%s`"+`, and the operator's session could obtain a validated PDF of that work from it. The generated regression uses that DOI as its label instead of the page's own identity evidence, and it requires a different DOI to refuse.

This does not support: which control produced the file (agent choices are opaque control ids and observations are not retained), that the proposed selector is the control the fallback used, equivalent authority on other pages, or access in another session. Only a model-free declarative canary can establish the repair.
`, recovery.JobID, recovery.CaptureSeq, recovery.FailureOutcome, recovery.FailureSeq,
		recovery.Artifact.SHA256, recovery.ReadySeq, strings.ReplaceAll(recovery.Delivery, "_", " "),
		recovery.Artifact.PageCount, recovery.Artifact.SizeBytes, recovery.Artifact.IdentityResult,
		route, recovery.ExplicitOpens, recovery.WorkDOI)
}

const (
	adapterRepairManifestName   = "repair.json"
	adapterRepairManifestSchema = "papio-adapter-repair/1"
	// adapterRepairOtherDOI is the refusal probe the generated test uses.
	adapterRepairOtherDOI = "10.0000/papio-repair-different-work"
)

// adapterRepairManifest is the machine-readable record of one repair
// workspace: the proposal, the capture digest and the recovery evidence a
// canary is judged against, so the judgement never re-derives the proposal.
type adapterRepairManifest struct {
	Schema              string                 `json:"schema"`
	CreatedAt           time.Time              `json:"created_at"`
	Provider            string                 `json:"provider"`
	Scenario            string                 `json:"scenario"`
	FixtureSHA256       string                 `json:"fixture_sha256"`
	FixtureName         string                 `json:"fixture_name"`
	CurrentVersion      string                 `json:"current_adapter_version"`
	NextRevision        string                 `json:"next_revision"`
	IndependentEvidence bool                   `json:"independent_evidence"`
	Outcome             string                 `json:"outcome"`
	Patches             []string               `json:"patches"`
	Recovery            *adapterRepairRecovery `json:"recovery,omitempty"`
}

func writeAdapterRepairJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func adapterRepairCanaryInstructions(manifest adapterRepairManifest, workspaceRelative string) string {
	recovery := manifest.Recovery
	fixture := manifest.FixtureName + ".html"
	return fmt.Sprintf(`# Model-free declarative canary for %[1]s %[2]s

Recovery job `+"`%[3]s`"+` proves the captured page can yield `+"`%[4]s`"+` (%[5]d pages). It does not prove this patch. Only this canary can promote it, and only the operator's real browser run counts.

Use an isolated checkout, a separate config with a fresh empty data directory, and the development extension. Keep Zotero automatic imports paused (`+"`[zotio] auto_import_paused = true`"+`). Record every Open, sign-in and other intervention. Do not click a PDF control, use Send PDF, or save from a viewer: the daemon history cannot tell a manual download from the adapter's, so the verdict depends on this record.

## 1. Disable the agent fallback for the canary daemon

Remove any `+"`[agent]`"+` table from the canary config. Start its daemon with the variable present but empty; an empty value overrides every stored TypeSafe credential:

`+"```sh"+`
papio --config <canary-config> daemon stop
PAPIO_TYPESAFE_API_KEY= papio --config <canary-config> doctor
papio --config <canary-config> config agent status
`+"```"+`

The status must show no enabled backend. The verdict refuses any canary job that records an agent decision or a native save.

## 2. Reproduce the failure with the current adapter %[6]s

With the unpatched development bundle loaded, submit a fresh job and open its handoff:

`+"```sh"+`
papio --config <canary-config> acquire --doi %[7]s --force --deny-source unpaywall --deny-source europepmc --deny-source openalex_content --deny-source core --deny-source crossref_tdm --deny-source arxiv --json
papio --config <canary-config> actions open --job <canary-job> --dry-run
papio --config <canary-config> actions open --job <canary-job>
`+"```"+`

Add the institution's `+"`--resolver <profile>`"+` when needed. Wait until `+"`papio --config <canary-config> jobs show <canary-job>`"+` shows `+"`browser.provider_outcome`"+` `+"`ui_changed`"+` from `+"`%[1]s`"+` `+"`%[6]s`"+` and an open `+"`manual_download`"+` action.

## 3. Load the candidate; the daemon re-drives the same job

In the isolated checkout:

`+"```sh"+`
git apply %[8]s %[9]s
mkdir -p extension/fixtures/%[1]s
cp %[10]s %[11]s
(cd extension && bun run typecheck && bun test && bun run build)
papio --config <canary-config> browser reload
`+"```"+`

The reload must report a new holder session id. The daemon then moves the parked job back to resolving with `+"`adapter_upgrade_repair`"+` and `+"`new_adapter_version`"+` `+"`%[2]s`"+`. That transition is the only attestation the verdict accepts that the candidate ran. If the job parks for institutional handoff again, reopen it with `+"`papio --config <canary-config> actions open --job <canary-job>`"+` and record that Open.

## 4. Decide from the artifact

`+"```sh"+`
papio --config <canary-config> jobs show <canary-job> --json
papio --config <canary-config> artifacts get <canary-job> --json
`+"```"+`

- promote: after the upgrade, the history shows a browser download, then a validating-to-ready transition not marked `+"`human_identity_override`"+`, with no agent decision, native admission or other candidate outcome, and the artifact is `+"`%[4]s`"+` with %[5]d pages. Inspect the first page and page count of the artifact yourself, then keep and commit the fixture, test and revision from the isolated checkout.
- rollback: the candidate reported ui_changed or wrong_work, delivered the wrong work or a different page count, or its document needed identity review or a human override. In the isolated checkout run `+"`git apply -R`"+` with both patches above, remove the copied fixture, then rebuild and reload the unpatched bundle.
- inconclusive: agent decisions, native delivery, no ui_changed from the current adapter before the upgrade, no upgrade attestation, any other candidate outcome, another work, a job created before this workspace, or a job still in progress. Decide nothing.

Never reset or reuse a job to repeat the canary. Submit a new one; recovery job `+"`%[3]s`"+` and its store stay untouched.
`, manifest.Provider, manifest.NextRevision, recovery.JobID, recovery.WorkDOI, recovery.Artifact.PageCount,
		manifest.CurrentVersion, shellQuote(recovery.WorkDOI),
		shellQuote(workspaceRelative+"/adapters.test.ts.patch"), shellQuote(workspaceRelative+"/types.ts.patch"),
		shellQuote(workspaceRelative+"/extension/fixtures/"+manifest.Provider+"/"+fixture),
		shellQuote("extension/fixtures/"+manifest.Provider+"/"+fixture))
}

// shellQuote makes one POSIX shell word. Recovered identifiers come from
// provider metadata, so copy-paste instructions must never let them expand.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
