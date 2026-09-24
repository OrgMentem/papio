// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"papio/internal/store"
)

// ArtifactProducerEvent is the one event a successful main-artifact promotion
// records: who produced the bytes and which human interventions the attempt
// needed. It is written inside the publication transaction, so an accepted
// artifact without one is an artifact promoted before this record existed.
const ArtifactProducerEvent = "artifact.producer"

// Producer names who produced the promoted bytes. The vocabulary is closed.
type Producer string

const (
	// ProducerAdapter is the extension's own generic drive: it downloaded a
	// page-declared PDF inside a daemon-issued drive epoch and no article
	// agent decision was taken in that drive.
	ProducerAdapter Producer = "adapter"
	// ProducerAgent is a generic drive in which the article agent chose the
	// control that produced the download.
	ProducerAgent Producer = "agent"
	// ProducerViewerCapture is a signed PDF viewer's one response that the
	// extension saved in a tab it armed for the job: Firefox's stream capture
	// or Chrome's download rule. AdapterID names the adapter that matched the
	// paper's page, when one did; who clicked View PDF is not recorded.
	ProducerViewerCapture Producer = "viewer_capture"
	// ProducerDaemonFetch is bytes whose URL the daemon chose: a resolver
	// candidate the daemon fetched itself, or a browser direct_get of a
	// daemon-selected public route.
	ProducerDaemonFetch Producer = "daemon_fetch"
	// ProducerManual is bytes a person supplied: a PDF grab of a document the
	// operator had open, or a browser download that arrived after the
	// provider adapter had declared it could not proceed.
	ProducerManual Producer = "manual"
	// ProducerUnknown is a browser download with no durable evidence of who
	// clicked. A browser download outside a drive epoch is reported without
	// the adapter that matched the page, so an adapter click and a human click
	// look identical to the daemon.
	ProducerUnknown Producer = "unknown"
)

// Producers lists the closed vocabulary in reporting order.
var Producers = []Producer{ProducerAdapter, ProducerAgent, ProducerViewerCapture, ProducerDaemonFetch, ProducerManual, ProducerUnknown}

// Intervention names one kind of human involvement the job's own events
// record for the attempt that produced the artifact.
type Intervention string

const (
	// InterventionOpen is a handoff opened by anyone but the pacer.
	InterventionOpen Intervention = "open"
	// InterventionSignIn is an institutional sign-in the browser observed.
	InterventionSignIn Intervention = "sign_in"
	// InterventionTerms is a provider terms page an adapter stopped at.
	InterventionTerms Intervention = "terms"
	// InterventionChallenge is a provider bot challenge the browser hit.
	InterventionChallenge Intervention = "challenge"
	// InterventionReview is a human action resolved during the attempt,
	// such as an accepted identity review.
	InterventionReview Intervention = "review"
	// InterventionManualFile is bytes a person supplied (ProducerManual).
	InterventionManualFile Intervention = "manual_file"
)

// Interventions lists the closed vocabulary in reporting order.
var Interventions = []Intervention{InterventionOpen, InterventionSignIn, InterventionTerms, InterventionChallenge, InterventionReview, InterventionManualFile}

// PacerPrincipal is the handoff.opened principal of the daemon's own paced
// opener. It is the one opener that is not a human intervention.
const PacerPrincipal = "pacer"

// ViewerCaptureEvent records that the extension is about to save a signed
// viewer's one response for the job. It arrives before the file can land in
// the adoption directory, so it precedes the promotion it attributes.
const ViewerCaptureEvent = "browser.viewer_capture"

// ArtifactProducerRecord is the detail of one artifact.producer event.
type ArtifactProducerRecord struct {
	Producer        Producer `json:"producer"`
	AdapterID       string   `json:"adapter_id,omitempty"`
	AdapterVersion  string   `json:"adapter_version,omitempty"`
	AgentDecisionID string   `json:"agent_decision_id,omitempty"`
	// EffectKind is the browser effect tuple that bound the download, when
	// one did: generic_drive, direct_get or institutional.
	EffectKind string `json:"effect_kind,omitempty"`
	// Basis names the evidence the producer was derived from.
	Basis  string `json:"basis"`
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
	// OpenedBy is the principal of the attempt's latest handoff.opened.
	OpenedBy      string         `json:"opened_by,omitempty"`
	Interventions []Intervention `json:"interventions"`
}

// Attended reports whether the record names any intervention besides sign-in.
func (r ArtifactProducerRecord) Attended() bool {
	for _, intervention := range r.Interventions {
		if intervention != InterventionSignIn {
			return true
		}
	}
	return false
}

// producerEvent is one row of the attempt's event stream, decoded once.
type producerEvent struct {
	seq    int64
	kind   string
	detail map[string]any
}

// producerEventKinds are the only kinds the derivation reads. Filtering in SQL
// keeps notify.* and action.* traffic, which dominates a parked job's stream,
// out of the promotion transaction.
var producerEventKinds = []string{
	"job.transition", "handoff.opened",
	"browser.auth_pending", "browser.auth_returned",
	"browser.provider_outcome", "browser.error", "browser.challenge_cleared",
	"browser.provider_drive_epoch_started", "browser.agent_decision_completed",
	"browser.download_complete", "human_action.resolve", ViewerCaptureEvent,
}

// adapterStopOutcomes are provider outcomes after which the adapter does not
// click again on its own. A download that follows one, in the same visit and
// with no drive, was finished by a person. terms_acceptance_required and
// human_auth_required are excluded: after the person acts the page reloads and
// the adapter may well click the PDF itself.
var adapterStopOutcomes = map[string]bool{
	"ui_changed": true, "native_viewer_download_required": true,
	"no_entitlement": true, "wrong_work": true,
}

// recordArtifactProducerTx derives and appends the artifact.producer event for
// a main artifact promoted to ready. It runs inside the publication
// transaction so the record and the acceptance commit together, and it takes
// the ready transition's own timestamp: ProducerStats pairs the two on it.
// A derivation that cannot read its evidence records unknown rather than
// refusing the promotion: attribution is an audit fact, and a validated paper
// must not be held back for it. Only the record's own write can fail the
// promotion, and that write shares the transaction's fate anyway.
func recordArtifactProducerTx(ctx context.Context, tx *sql.Tx, input PublicationInput, now string) error {
	record, err := deriveArtifactProducerTx(ctx, tx, input.JobID, input.CandidateID, input.SHA256)
	if err != nil {
		log.Printf("papio: deriving artifact producer for job %s: %v", input.JobID, err)
		record = ArtifactProducerRecord{
			Producer: ProducerUnknown, Basis: "derivation_failed", SHA256: input.SHA256,
			Interventions: []Intervention{},
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, ?, ?)`,
		input.JobID, now, ArtifactProducerEvent, string(encoded))
	return err
}

func deriveArtifactProducerTx(ctx context.Context, tx *sql.Tx, jobID string, candidateID *int64, sha string) (ArtifactProducerRecord, error) {
	source := ""
	if candidateID != nil {
		if err := tx.QueryRowContext(ctx,
			`SELECT source FROM candidates WHERE id = ? AND job_id = ?`, *candidateID, jobID).Scan(&source); err != nil {
			return ArtifactProducerRecord{}, err
		}
	}
	// The attempt begins after the latest retry request: that is the
	// boundary job_attempt_revision counts (1 + retry_requested events).
	var windowStart int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE job_id = ? AND kind = 'job.retry_requested'`,
		jobID).Scan(&windowStart); err != nil {
		return ArtifactProducerRecord{}, err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(producerEventKinds)), ",")
	args := make([]any, 0, len(producerEventKinds)+1)
	args = append(args, jobID)
	for _, kind := range producerEventKinds {
		args = append(args, kind)
	}
	// #nosec G202 -- only generated "?" placeholders enter the query text; the
	// job ID and event kinds remain bound arguments.
	rows, err := tx.QueryContext(ctx, `SELECT seq, kind, detail_json FROM events
		WHERE job_id = ? AND kind IN (`+placeholders+`) ORDER BY seq ASC`, args...)
	if err != nil {
		return ArtifactProducerRecord{}, err
	}
	var events []producerEvent
	for rows.Next() {
		var event producerEvent
		var raw string
		if err := rows.Scan(&event.seq, &event.kind, &raw); err != nil {
			_ = rows.Close()
			return ArtifactProducerRecord{}, err
		}
		// A malformed detail reads as empty: it cannot bind a producer, and
		// refusing the promotion over an old audit row would be worse.
		_ = json.Unmarshal([]byte(raw), &event.detail)
		events = append(events, event)
	}
	if err := rows.Close(); err != nil {
		return ArtifactProducerRecord{}, err
	}
	if err := rows.Err(); err != nil {
		return ArtifactProducerRecord{}, err
	}
	var grabbed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pdf_grabs WHERE job_id = ? AND state = 'job_created'`, jobID).Scan(&grabbed); err != nil {
		return ArtifactProducerRecord{}, err
	}
	permitDrive := func(permitID string) (string, int64, bool) {
		var attempt sql.NullString
		var ordinal sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT drive_attempt_id, ordinal FROM effect_permits WHERE id = ? AND job_id = ?`,
			permitID, jobID).Scan(&attempt, &ordinal)
		if err != nil || !attempt.Valid || !ordinal.Valid {
			return "", 0, false
		}
		return attempt.String, ordinal.Int64, true
	}
	return deriveArtifactProducer(producerInputs{
		source: source, sha: sha, windowStart: windowStart, events: events,
		grabbed: grabbed > 0, permitDrive: permitDrive,
	}), nil
}

type producerInputs struct {
	source      string
	sha         string
	windowStart int64
	events      []producerEvent
	grabbed     bool
	permitDrive func(permitID string) (attempt string, ordinal int64, ok bool)
}

// deriveArtifactProducer classifies one promotion from durable evidence only.
// Every branch that lacks evidence lands on unknown; nothing is inferred from
// a provider host, a filename, or which actions happened to be open.
func deriveArtifactProducer(in producerInputs) ArtifactProducerRecord {
	record := ArtifactProducerRecord{Source: in.source, SHA256: in.sha, Producer: ProducerUnknown}
	var window []producerEvent
	if start := slices.IndexFunc(in.events, func(event producerEvent) bool { return event.seq > in.windowStart }); start >= 0 {
		window = in.events[start:]
	}
	switch {
	case in.source != "browser":
		record.Producer, record.Basis = ProducerDaemonFetch, "resolver_candidate"
	case in.grabbed:
		record.Producer, record.Basis = ProducerManual, "pdf_grab"
	default:
		classifyBrowserDownload(&record, in, window)
	}
	record.OpenedBy, record.Interventions = interventionsFor(record.Producer, window)
	return record
}

func classifyBrowserDownload(record *ArtifactProducerRecord, in producerInputs, window []producerEvent) {
	producer, basis := boundArtifactProducer(in.events, window, in.sha)
	if producer == nil {
		// Two download tuples for the same bytes stay ambiguous: a capture
		// record cannot say which of them saved the file.
		if capture, ok := viewerCaptureBeforePromotion(window); ok && basis != "ambiguous_producer" {
			record.Producer, record.Basis = ProducerViewerCapture, "viewer_capture"
			record.AdapterID = detailString(capture.detail, "adapter_id")
			record.AdapterVersion = detailString(capture.detail, "adapter_version")
			return
		}
		if outcome, ok := adapterStoppedBeforeDownload(window); ok {
			record.Producer, record.Basis = ProducerManual, "adapter_stopped"
			record.AdapterID = detailString(outcome.detail, "adapter_id")
			record.AdapterVersion = detailString(outcome.detail, "adapter_version")
			return
		}
		record.Basis = basis
		return
	}
	record.EffectKind = string(producer.Kind)
	record.Basis = basis
	switch {
	case producer.Kind == DirectGet:
		record.Producer = ProducerDaemonFetch
	case producer.Kind == GenericDrive && producer.Ordinal != nil:
		if decision := agentDecisionInDrive(window, producer.DriveAttemptID, *producer.Ordinal, in.permitDrive); decision != "" {
			record.Producer, record.AgentDecisionID = ProducerAgent, decision
			return
		}
		// The drive tuple names the extension's generic strategy; the
		// specific generic candidate (citation meta or article link) is not
		// reported to the daemon.
		record.Producer, record.AdapterID = ProducerAdapter, producer.Strategy
	default:
		// An institutional tuple proves the download happened in the tab
		// papio's claim opened, not whether an adapter or a person clicked.
		record.Producer = ProducerUnknown
	}
}

// boundArtifactProducer finds the effect tuple for the promoted bytes. A
// download_complete naming the exact digest is authoritative; otherwise the
// attempt's latest browser download is used when it carries a tuple and no
// later validation rejected bytes (which would make it the wrong download).
func boundArtifactProducer(all, window []producerEvent, sha string) (*ArtifactProducerIdentity, string) {
	var exact *ArtifactProducerIdentity
	for _, event := range all {
		if event.kind != "browser.download_complete" || detailString(event.detail, "sha256") != sha {
			continue
		}
		producer, ok := decodeProducerIdentity(event.detail)
		if !ok {
			continue
		}
		if exact != nil && !artifactProducerEqual(*exact, producer) {
			return nil, "ambiguous_producer"
		}
		exact = &producer
	}
	if exact != nil {
		return exact, "download_digest"
	}
	for i := len(window) - 1; i >= 0; i-- {
		event := window[i]
		if event.kind == "job.transition" && detailString(event.detail, "from") == StateValidating &&
			detailString(event.detail, "to") != StateReady {
			return nil, "no_download_record"
		}
		if event.kind != "browser.download_complete" {
			continue
		}
		producer, ok := decodeProducerIdentity(event.detail)
		if !ok {
			return nil, "no_effect_tuple"
		}
		return &producer, "attempt_download"
	}
	return nil, "no_download_record"
}

func decodeProducerIdentity(detail map[string]any) (ArtifactProducerIdentity, bool) {
	raw, ok := detail["producer"]
	if !ok || raw == nil {
		return ArtifactProducerIdentity{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ArtifactProducerIdentity{}, false
	}
	var producer ArtifactProducerIdentity
	if err := json.Unmarshal(encoded, &producer); err != nil || producer.Kind == "" {
		return ArtifactProducerIdentity{}, false
	}
	return producer, true
}

// agentDecisionInDrive returns the request id of the latest agent decision
// taken under the drive's own effect permit.
func agentDecisionInDrive(window []producerEvent, attempt string, ordinal int64, permitDrive func(string) (string, int64, bool)) string {
	if permitDrive == nil {
		return ""
	}
	decision := ""
	for _, event := range window {
		if event.kind != "browser.agent_decision_completed" || detailString(event.detail, "outcome") != "decision" {
			continue
		}
		permitAttempt, permitOrdinal, ok := permitDrive(detailString(event.detail, "permit_id"))
		if ok && permitAttempt == attempt && permitOrdinal == ordinal {
			decision = detailString(event.detail, "request_id")
		}
	}
	return decision
}

// adapterStoppedBeforeDownload reports the provider outcome that ended the
// visit which delivered the bytes: the latest outcome in the attempt, when no
// handoff was opened and no drive started after it.
func adapterStoppedBeforeDownload(window []producerEvent) (producerEvent, bool) {
	for i := len(window) - 1; i >= 0; i-- {
		switch window[i].kind {
		case "handoff.opened", "browser.provider_drive_epoch_started":
			return producerEvent{}, false
		case "browser.provider_outcome":
			return window[i], adapterStopOutcomes[detailString(window[i].detail, "outcome")]
		}
	}
	return producerEvent{}, false
}

// viewerCaptureBeforePromotion returns the attempt's latest viewer capture
// record when nothing after it names a different source for the bytes: a new
// handoff or drive, a provider outcome (the capture failed and a person was
// asked to download), or a validation that rejected bytes (the capture's
// file was not the paper). A download tuple is checked first by the caller,
// so this runs only when no effect identity bound the bytes.
func viewerCaptureBeforePromotion(window []producerEvent) (producerEvent, bool) {
	for i := len(window) - 1; i >= 0; i-- {
		event := window[i]
		switch event.kind {
		case ViewerCaptureEvent:
			return event, true
		case "handoff.opened", "browser.provider_drive_epoch_started", "browser.provider_outcome":
			return producerEvent{}, false
		case "job.transition":
			if detailString(event.detail, "from") == StateValidating && detailString(event.detail, "to") != StateReady {
				return producerEvent{}, false
			}
		}
	}
	return producerEvent{}, false
}

// interventionsFor returns the attempt's latest opener and its interventions
// in vocabulary order.
func interventionsFor(producer Producer, window []producerEvent) (string, []Intervention) {
	seen := map[Intervention]bool{}
	openedBy := ""
	for _, event := range window {
		switch event.kind {
		case "handoff.opened":
			principal := detailString(event.detail, "principal")
			openedBy = principal
			if principal != PacerPrincipal {
				seen[InterventionOpen] = true
			}
		case "browser.auth_pending", "browser.auth_returned":
			seen[InterventionSignIn] = true
		case "browser.provider_outcome":
			if detailString(event.detail, "outcome") == "terms_acceptance_required" {
				seen[InterventionTerms] = true
			}
		case "browser.error":
			if detailString(event.detail, "code") == "challenge_blocked" {
				seen[InterventionChallenge] = true
			}
		case "browser.challenge_cleared":
			seen[InterventionChallenge] = true
		case "human_action.resolve":
			seen[InterventionReview] = true
		}
	}
	if producer == ProducerManual {
		seen[InterventionManualFile] = true
	}
	out := make([]Intervention, 0, len(seen))
	for _, intervention := range Interventions {
		if seen[intervention] {
			out = append(out, intervention)
		}
	}
	return openedBy, out
}

func detailString(detail map[string]any, key string) string {
	value, _ := detail[key].(string)
	return value
}

// ProducerStats is the producer breakdown of artifact.producer events in a
// period. Every vocabulary entry is present, so a zero is a count, not an
// omission. Unrecorded counts ready transitions in the period that carry no
// producer record — promotions from before the record existed, and cache
// reuse, which promotes nothing.
type ProducerStats struct {
	Since         string               `json:"since"`
	Until         string               `json:"until"`
	Acquired      int                  `json:"acquired"`
	Producers     map[Producer]int     `json:"producers"`
	Unattended    int                  `json:"unattended"`
	SignInOnly    int                  `json:"sign_in_only"`
	Intervened    int                  `json:"intervened"`
	Interventions map[Intervention]int `json:"interventions"`
	OpenedBy      map[string]int       `json:"opened_by"`
	Unrecorded    int                  `json:"unrecorded"`
}

// ProducerStats aggregates artifact.producer events recorded in [since, until).
//
// A zero until leaves the period open at its end, so the count takes every
// event recorded before the query, and Until reports when it ran. "Until now"
// cannot be a bound: the event recorded just before the query can carry the
// same clock reading as the query, and a half-open period then drops it.
// Windows advances the wall clock about once a millisecond, so measured there
// the second of two promotions recorded back to back read equal to the
// default end in 12 of 20 runs.
func (js *Store) ProducerStats(ctx context.Context, since, until time.Time) (ProducerStats, error) {
	openEnd := until.IsZero()
	if openEnd {
		until = time.Now()
	}
	if !until.After(since) {
		return ProducerStats{}, errors.New("producer stats period must end after it starts")
	}
	stats := ProducerStats{
		Since: since.UTC().Format(time.RFC3339Nano), Until: until.UTC().Format(time.RFC3339Nano),
		Producers: map[Producer]int{}, Interventions: map[Intervention]int{}, OpenedBy: map[string]int{},
	}
	for _, producer := range Producers {
		stats.Producers[producer] = 0
	}
	for _, intervention := range Interventions {
		stats.Interventions[intervention] = 0
	}
	// period is the WHERE clause on one events alias. The bounds are bound in
	// store.TimeLayout because events.at is compared as text.
	period := func(alias string) (string, []any) {
		clause, args := alias+".at >= ?", []any{store.FormatTime(since)}
		if !openEnd {
			clause += " AND " + alias + ".at < ?"
			args = append(args, store.FormatTime(until))
		}
		return clause, args
	}
	recorded, recordedArgs := period("e")
	rows, err := js.S.DB().QueryContext(ctx,
		`SELECT e.detail_json FROM events e WHERE e.kind = ? AND `+recorded+` ORDER BY e.seq ASC`,
		append([]any{ArtifactProducerEvent}, recordedArgs...)...)
	if err != nil {
		return ProducerStats{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return ProducerStats{}, err
		}
		var record ArtifactProducerRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return ProducerStats{}, fmt.Errorf("decoding %s event: %w", ArtifactProducerEvent, err)
		}
		if !slices.Contains(Producers, record.Producer) {
			record.Producer = ProducerUnknown
		}
		stats.Acquired++
		stats.Producers[record.Producer]++
		switch {
		case len(record.Interventions) == 0:
			stats.Unattended++
		case !record.Attended():
			stats.SignInOnly++
		default:
			stats.Intervened++
		}
		for _, intervention := range record.Interventions {
			stats.Interventions[intervention]++
		}
		if record.OpenedBy != "" {
			stats.OpenedBy[record.OpenedBy]++
		}
	}
	if err := rows.Err(); err != nil {
		return ProducerStats{}, err
	}
	// Ready transitions in the period that no producer record accompanies.
	promoted, promotedArgs := period("t")
	if err := js.S.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM events t
		 WHERE t.kind = 'job.transition' AND `+promoted+`
		   AND json_extract(t.detail_json, '$.to') = 'ready'
		   AND NOT EXISTS (
		     SELECT 1 FROM events p
		      WHERE p.job_id = t.job_id AND p.kind = ? AND p.at = t.at)`,
		append(promotedArgs, ArtifactProducerEvent)...).Scan(&stats.Unrecorded); err != nil {
		return ProducerStats{}, err
	}
	return stats, nil
}
