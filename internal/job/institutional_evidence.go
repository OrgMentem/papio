// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"papio/internal/store"
)

// ProfileEvidenceVerdict is a closed observation vocabulary. Positive
// observations are still fenced by the exact holder/profile revisions; the
// other values are deliberately non-authorizing.
type ProfileEvidenceVerdict string

const (
	ProfileEvidenceUnknown      ProfileEvidenceVerdict = "unknown"
	ProfileEvidenceInconclusive ProfileEvidenceVerdict = "inconclusive"
	ProfileEvidenceSignedOut    ProfileEvidenceVerdict = "signed_out"
	ProfileEvidenceWarmVerified ProfileEvidenceVerdict = "warm_verified"
	ProfileEvidenceAuthReturned ProfileEvidenceVerdict = "auth_returned"
)

func (v ProfileEvidenceVerdict) valid() bool {
	switch v {
	case ProfileEvidenceUnknown, ProfileEvidenceInconclusive, ProfileEvidenceSignedOut,
		ProfileEvidenceWarmVerified, ProfileEvidenceAuthReturned:
		return true
	default:
		return false
	}
}

// ProfileEvidenceSource identifies the bounded producer of a browser fact.
type ProfileEvidenceSource string

const (
	ProfileEvidenceProbe           ProfileEvidenceSource = "probe"
	ProfileEvidenceAuthReturn      ProfileEvidenceSource = "auth_return"
	ProfileEvidenceProviderOutcome ProfileEvidenceSource = "provider_outcome"
)

// Descriptive aliases make call sites read naturally without widening the
// wire vocabulary.
const (
	ProfileEvidenceSourceProbe           = ProfileEvidenceProbe
	ProfileEvidenceSourceAuthReturn      = ProfileEvidenceAuthReturn
	ProfileEvidenceSourceProviderOutcome = ProfileEvidenceProviderOutcome
	ProfileEvidenceVerdictWarmVerified   = ProfileEvidenceWarmVerified
	ProfileEvidenceVerdictAuthReturned   = ProfileEvidenceAuthReturned
)

func (s ProfileEvidenceSource) valid() bool {
	switch s {
	case ProfileEvidenceProbe, ProfileEvidenceAuthReturn, ProfileEvidenceProviderOutcome:
		return true
	default:
		return false
	}
}

// ProfileEvidenceObservation is a daemon-received browser observation. It has
// no URL, path, credential, or other provider secret by construction.
type ProfileEvidenceObservation struct {
	ObservationID              string                 `json:"observation_id"`
	BrowserHolderGeneration    int64                  `json:"browser_holder_generation"`
	InstitutionProfileID       string                 `json:"institution_profile_id"`
	InstitutionProfileRevision int64                  `json:"institution_profile_revision"`
	Verdict                    ProfileEvidenceVerdict `json:"verdict"`
	Source                     ProfileEvidenceSource  `json:"source"`
	ProducerObservedAt         string                 `json:"producer_observed_at"`
	DaemonReceivedAt           string                 `json:"daemon_received_at"`
	ExpiresAt                  string                 `json:"expires_at,omitempty"`
}

func (o ProfileEvidenceObservation) validate() error {
	if strings.TrimSpace(o.ObservationID) == "" || len(o.ObservationID) > 128 {
		return errors.New("profile evidence requires a bounded observation id")
	}
	if o.BrowserHolderGeneration < 0 || o.InstitutionProfileRevision < 1 {
		return errors.New("profile evidence revisions must be positive")
	}
	if strings.TrimSpace(o.InstitutionProfileID) == "" || len(o.InstitutionProfileID) > 128 {
		return errors.New("profile evidence requires a bounded profile id")
	}
	if !o.Verdict.valid() {
		return fmt.Errorf("invalid profile evidence verdict %q", o.Verdict)
	}
	if !o.Source.valid() {
		return fmt.Errorf("invalid profile evidence source %q", o.Source)
	}
	return nil
}

// RecordProfileEvidence durably records one observation. Lost responses are
// safe: the observation id is the idempotency key and a duplicate leaves the
// already committed row unchanged.
func (js *Store) RecordProfileEvidence(ctx context.Context, observation ProfileEvidenceObservation) error {
	if err := observation.validate(); err != nil {
		return err
	}
	if observation.ProducerObservedAt == "" {
		observation.ProducerObservedAt = store.Now()
	}
	if observation.DaemonReceivedAt == "" {
		observation.DaemonReceivedAt = store.Now()
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO profile_evidence
		 (observation_id, browser_holder_generation, institution_profile_id,
		  institution_profile_revision, verdict, source, producer_observed_at,
		  daemon_received_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''))`,
		observation.ObservationID, observation.BrowserHolderGeneration,
		observation.InstitutionProfileID, observation.InstitutionProfileRevision,
		string(observation.Verdict), string(observation.Source), observation.ProducerObservedAt,
		observation.DaemonReceivedAt, observation.ExpiresAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CurrentProfileEvidence projects the newest non-expired decisive observation
// for the exact holder/profile fence. Unknown and inconclusive observations
// are retained but never replace a decisive projection; when no decisive fact
// exists, the newest non-decisive fact is returned so callers can fail closed.
func (js *Store) CurrentProfileEvidence(ctx context.Context, profileID string, profileRevision, holderGeneration int64) (ProfileEvidenceObservation, bool, error) {
	if profileID == "" || profileRevision < 1 || holderGeneration < 0 {
		return ProfileEvidenceObservation{}, false, errors.New("current profile evidence requires exact fence")
	}
	var o ProfileEvidenceObservation
	scan := func(row *sql.Row) error {
		return row.Scan(&o.ObservationID, &o.BrowserHolderGeneration, &o.InstitutionProfileID,
			&o.InstitutionProfileRevision, &o.Verdict, &o.Source, &o.ProducerObservedAt,
			&o.DaemonReceivedAt, &o.ExpiresAt)
	}
	args := []any{profileID, profileRevision, holderGeneration, store.Now()}
	err := scan(js.S.DB().QueryRowContext(ctx, `
		SELECT observation_id, browser_holder_generation, institution_profile_id,
		       institution_profile_revision, verdict, source, producer_observed_at,
		       daemon_received_at, COALESCE(expires_at, '')
		FROM profile_evidence
		WHERE institution_profile_id = ? AND institution_profile_revision = ?
		  AND browser_holder_generation = ?
		  AND (expires_at IS NULL OR expires_at = '' OR expires_at > ?)
		  AND verdict NOT IN ('unknown', 'inconclusive')
		ORDER BY daemon_received_at DESC, observation_id DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		err = scan(js.S.DB().QueryRowContext(ctx, `
			SELECT observation_id, browser_holder_generation, institution_profile_id,
			       institution_profile_revision, verdict, source, producer_observed_at,
			       daemon_received_at, COALESCE(expires_at, '')
			FROM profile_evidence
			WHERE institution_profile_id = ? AND institution_profile_revision = ?
			  AND browser_holder_generation = ?
			  AND (expires_at IS NULL OR expires_at = '' OR expires_at > ?)
			ORDER BY daemon_received_at DESC, observation_id DESC LIMIT 1`, args...))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ProfileEvidenceObservation{}, false, nil
	}
	if err != nil {
		return ProfileEvidenceObservation{}, false, err
	}
	return o, true, nil
}

// HumanGateType is the closed typed attention vocabulary from ADR-0022.
type HumanGateType string

const (
	HumanGateLogin                     HumanGateType = "human_gate.login"
	HumanGateMFA                       HumanGateType = "human_gate.mfa"
	HumanGateCaptchaOrSecurity         HumanGateType = "human_gate.captcha_or_security"
	HumanGateBrowserHostPermission     HumanGateType = "human_gate.browser_host_permission"
	HumanGateDownloadsFolderPermission HumanGateType = "human_gate.downloads_folder_permission"
	HumanGateTermsRequired             HumanGateType = "human_gate.terms_required"
	HumanGateContractualDeclaration    HumanGateType = "human_gate.contractual_declaration"
	HumanGateIdentityAmbiguous         HumanGateType = "human_gate.identity_ambiguous"
)

func (t HumanGateType) valid() bool {
	switch t {
	case HumanGateLogin, HumanGateMFA, HumanGateCaptchaOrSecurity, HumanGateBrowserHostPermission,
		HumanGateDownloadsFolderPermission, HumanGateTermsRequired, HumanGateContractualDeclaration,
		HumanGateIdentityAmbiguous:
		return true
	default:
		return false
	}
}

type HumanGateStatus string

const (
	HumanGateOpen      HumanGateStatus = "open"
	HumanGateResolved  HumanGateStatus = "resolved"
	HumanGateCancelled HumanGateStatus = "cancelled"
)

func (s HumanGateStatus) valid() bool {
	switch s {
	case HumanGateOpen, HumanGateResolved, HumanGateCancelled:
		return true
	default:
		return false
	}
}

type HumanGateScopeClass string

const (
	HumanGateScopeAuthenticationClaim HumanGateScopeClass = "authentication_claim"
	HumanGateScopeInstitutionProfile  HumanGateScopeClass = "institution_profile"
	HumanGateScopeBrowserHost         HumanGateScopeClass = "browser_host"
	HumanGateScopePlatform            HumanGateScopeClass = "platform"
	HumanGateScopeBinding             HumanGateScopeClass = "binding"
)

func validHumanGateScopeClass(s string) bool {
	switch HumanGateScopeClass(s) {
	case HumanGateScopeAuthenticationClaim, HumanGateScopeInstitutionProfile,
		HumanGateScopeBrowserHost, HumanGateScopePlatform, HumanGateScopeBinding:
		return true
	default:
		return false
	}
}

// HumanGateObservation is the current typed attention projection for a scope.
type HumanGateObservation struct {
	ID                   string          `json:"id"`
	GateType             HumanGateType   `json:"gate_type"`
	ScopeClass           string          `json:"scope_class"`
	ScopeKey             string          `json:"scope_key"`
	InstitutionProfileID string          `json:"institution_profile_id,omitempty"`
	BindingID            string          `json:"binding_id,omitempty"`
	ObservationRevision  int64           `json:"observation_revision"`
	Status               HumanGateStatus `json:"status"`
	DetailJSON           string          `json:"detail_json"`
	CreatedAt            string          `json:"created_at"`
	UpdatedAt            string          `json:"updated_at"`
}

func (o HumanGateObservation) validate() error {
	if o.ID == "" || len(o.ID) > 128 || o.ScopeClass == "" || len(o.ScopeClass) > 128 || o.ScopeKey == "" || len(o.ScopeKey) > 256 {
		return errors.New("human gate requires bounded id and scope")
	}
	if !validHumanGateScopeClass(o.ScopeClass) || !o.GateType.valid() || !o.Status.valid() || o.ObservationRevision < 1 {
		return errors.New("invalid human gate scope, type, status, or revision")
	}
	return nil
}

// UpsertHumanGateObservation atomically advances one current scope projection.
// A stale revision and a lost-response duplicate are both no-ops.
func (js *Store) UpsertHumanGateObservation(ctx context.Context, observation HumanGateObservation) error {
	if err := observation.validate(); err != nil {
		return err
	}
	now := store.Now()
	if observation.CreatedAt == "" {
		observation.CreatedAt = now
	}
	if observation.UpdatedAt == "" {
		observation.UpdatedAt = now
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO human_gate_observations
		 (id, gate_type, scope_class, scope_key, institution_profile_id, binding_id,
		  observation_revision, status, detail_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?)
		ON CONFLICT(gate_type, scope_class, scope_key) DO UPDATE SET
		 id = excluded.id,
		 institution_profile_id = excluded.institution_profile_id,
		 binding_id = excluded.binding_id, observation_revision = excluded.observation_revision,
		 status = excluded.status, detail_json = excluded.detail_json,
		 updated_at = excluded.updated_at
		WHERE excluded.observation_revision > human_gate_observations.observation_revision`,
		observation.ID, string(observation.GateType), observation.ScopeClass, observation.ScopeKey,
		observation.InstitutionProfileID, observation.BindingID, observation.ObservationRevision,
		string(observation.Status), observation.DetailJSON, observation.CreatedAt, observation.UpdatedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CurrentHumanGateObservations returns the current projection for one exact
// gate scope. Scope class and key are explicit to avoid an unscoped attention
// read being mistaken for an authorization decision.
func (js *Store) CurrentHumanGateObservations(ctx context.Context, scopeClass, scopeKey string) ([]HumanGateObservation, error) {
	if scopeClass == "" || scopeKey == "" {
		return nil, errors.New("gate scope class and key must be supplied")
	}
	q := `SELECT id, gate_type, scope_class, scope_key, COALESCE(institution_profile_id,''), COALESCE(binding_id,''), observation_revision, status, detail_json, created_at, updated_at FROM human_gate_observations WHERE scope_class = ? AND scope_key = ? ORDER BY gate_type`
	rows, err := js.S.DB().QueryContext(ctx, q, scopeClass, scopeKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []HumanGateObservation
	for rows.Next() {
		var o HumanGateObservation
		if err := rows.Scan(&o.ID, &o.GateType, &o.ScopeClass, &o.ScopeKey, &o.InstitutionProfileID, &o.BindingID, &o.ObservationRevision, &o.Status, &o.DetailJSON, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RouteSuppressionReason is a closed reason vocabulary.
type RouteSuppressionReason string

const (
	RouteSuppressionNoEntitlement     RouteSuppressionReason = "no_entitlement"
	RouteSuppressionProviderChallenge RouteSuppressionReason = "provider_challenge"
	RouteSuppressionRateLimited       RouteSuppressionReason = "rate_limited"
	RouteSuppressionAdapterDrift      RouteSuppressionReason = "adapter_drift"
)

func (r RouteSuppressionReason) valid() bool {
	return r == RouteSuppressionNoEntitlement || r == RouteSuppressionProviderChallenge ||
		r == RouteSuppressionRateLimited || r == RouteSuppressionAdapterDrift
}

// RouteSuppressionKey is the complete invalidation and lookup key.
type RouteSuppressionKey struct {
	JobID                      string
	JobAttemptRevision         int64
	InstitutionProfileID       string
	InstitutionProfileRevision int64
	RouteRevision              int64
	SafetyDomainID             string
	AdapterRevision            string
	IdentifierStrategy         string
}

// RouteSuppression is one exact fenced suppression projection.
type RouteSuppression struct {
	RouteSuppressionKey
	ID                    string
	EvidenceObservationID string
	Reason                RouteSuppressionReason
	Active                bool
	CreatedAt             string
	UpdatedAt             string
}

func (s RouteSuppression) validate() error {
	if s.JobID == "" || s.InstitutionProfileID == "" || s.SafetyDomainID == "" || s.IdentifierStrategy == "" || s.AdapterRevision == "" || s.JobAttemptRevision < 1 || s.InstitutionProfileRevision < 1 || s.RouteRevision < 1 || !s.Reason.valid() {
		return errors.New("invalid route suppression key or reason")
	}
	return nil
}

// AddRouteSuppression records one exact active key idempotently.
func (js *Store) AddRouteSuppression(ctx context.Context, suppression RouteSuppression) error {
	if err := suppression.validate(); err != nil {
		return err
	}
	if suppression.ID == "" {
		suppression.ID = NewID("sup")
	}
	now := store.Now()
	if suppression.CreatedAt == "" {
		suppression.CreatedAt = now
	}
	if suppression.UpdatedAt == "" {
		suppression.UpdatedAt = now
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO route_suppressions (id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision, route_revision, safety_domain_id, adapter_revision, identifier_strategy, evidence_observation_id, reason, active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, 1, ?, ?)`, suppression.ID, suppression.JobID, suppression.JobAttemptRevision, suppression.InstitutionProfileID, suppression.InstitutionProfileRevision, suppression.RouteRevision, suppression.SafetyDomainID, suppression.AdapterRevision, suppression.IdentifierStrategy, suppression.EvidenceObservationID, string(suppression.Reason), suppression.CreatedAt, suppression.UpdatedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// InvalidateRouteSuppressions closes every active suppression for a job whose
// complete key differs from current. Exact matches remain active.
func (js *Store) InvalidateRouteSuppressions(ctx context.Context, current RouteSuppressionKey) error {
	if current.JobID == "" {
		return errors.New("suppression invalidation requires a job")
	}
	_, err := js.S.DB().ExecContext(ctx, `UPDATE route_suppressions SET active = 0, updated_at = ? WHERE job_id = ? AND active = 1 AND NOT (job_attempt_revision = ? AND institution_profile_id = ? AND institution_profile_revision = ? AND route_revision = ? AND safety_domain_id = ? AND adapter_revision = ? AND identifier_strategy = ?)`, store.Now(), current.JobID, current.JobAttemptRevision, current.InstitutionProfileID, current.InstitutionProfileRevision, current.RouteRevision, current.SafetyDomainID, current.AdapterRevision, current.IdentifierStrategy)
	return err
}

// ActiveRouteSuppressions returns only exact active matches for the supplied
// key; stale profile/route/adapter revisions cannot leak into eligibility.
func (js *Store) ActiveRouteSuppressions(ctx context.Context, key RouteSuppressionKey) ([]RouteSuppression, error) {
	if key.JobID == "" {
		return nil, errors.New("suppression lookup requires a job")
	}
	rows, err := js.S.DB().QueryContext(ctx, `SELECT id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision, route_revision, safety_domain_id, adapter_revision, identifier_strategy, COALESCE(evidence_observation_id,''), reason, active, created_at, updated_at FROM route_suppressions WHERE job_id = ? AND job_attempt_revision = ? AND institution_profile_id = ? AND institution_profile_revision = ? AND route_revision = ? AND safety_domain_id = ? AND adapter_revision = ? AND identifier_strategy = ? AND active = 1 ORDER BY created_at, id`, key.JobID, key.JobAttemptRevision, key.InstitutionProfileID, key.InstitutionProfileRevision, key.RouteRevision, key.SafetyDomainID, key.AdapterRevision, key.IdentifierStrategy)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RouteSuppression
	for rows.Next() {
		var s RouteSuppression
		var active int
		if err := rows.Scan(&s.ID, &s.JobID, &s.JobAttemptRevision, &s.InstitutionProfileID, &s.InstitutionProfileRevision, &s.RouteRevision, &s.SafetyDomainID, &s.AdapterRevision, &s.IdentifierStrategy, &s.EvidenceObservationID, &s.Reason, &active, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.Active = active != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// ArtifactWinner is the insert-only CAS decision for a job attempt. The
// browser holder generation fences the result to the browser claim that
// actually materialized it.
type ArtifactWinner struct {
	JobID                   string `json:"job_id"`
	JobAttemptRevision      int64  `json:"job_attempt_revision"`
	CandidateID             string `json:"candidate_id"`
	BrowserHolderGeneration int64  `json:"browser_holder_generation"`
	SHA256                  string `json:"sha256"`
	CreatedAt               string `json:"created_at"`
}

func (w ArtifactWinner) validate() error {
	if w.JobID == "" || w.CandidateID == "" || len(w.SHA256) != 64 || w.JobAttemptRevision < 1 || w.BrowserHolderGeneration < 0 {
		return errors.New("invalid artifact winner")
	}
	return nil
}

// ClaimArtifactWinner performs an insert-only winner CAS. The candidate must
// still have a live materialization claim for the exact job attempt and holder
// generation; stale or demoted holders cannot create a winner. A loser
// receives the committed winner for that same attempt, and repeating the
// current winning request is idempotent.
func (js *Store) ClaimArtifactWinner(ctx context.Context, winner ArtifactWinner) (ArtifactWinner, bool, error) {
	if err := winner.validate(); err != nil {
		return ArtifactWinner{}, false, err
	}
	if winner.CreatedAt == "" {
		winner.CreatedAt = store.Now()
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return ArtifactWinner{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var existing ArtifactWinner
	err = tx.QueryRowContext(ctx, `SELECT job_id, job_attempt_revision, candidate_id,
		browser_holder_generation, sha256, created_at
		FROM artifact_winners WHERE job_id = ? AND job_attempt_revision = ?`,
		winner.JobID, winner.JobAttemptRevision).Scan(
		&existing.JobID, &existing.JobAttemptRevision, &existing.CandidateID,
		&existing.BrowserHolderGeneration, &existing.SHA256, &existing.CreatedAt)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return ArtifactWinner{}, false, err
		}
		return existing, existing.CandidateID == winner.CandidateID &&
			existing.BrowserHolderGeneration == winner.BrowserHolderGeneration &&
			existing.SHA256 == winner.SHA256, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ArtifactWinner{}, false, err
	}
	now := store.Now()
	var liveClaim int
	err = tx.QueryRowContext(ctx, `
		SELECT 1
		  FROM materialization_claims m
		  JOIN browser_candidates c ON c.id = m.candidate_id
		  JOIN institution_profiles p ON p.id = c.institution_profile_id
		 WHERE p.tombstoned_at IS NULL
		   AND p.revision = c.institution_profile_revision
		   AND m.candidate_id = ?
		   AND c.job_id = ?
		   AND c.job_attempt_revision = ?
		   AND m.browser_holder_generation = ?
		   AND m.phase IN ('claimed','bound','route_issued','navigated')
		   AND (m.lease_until IS NULL OR m.lease_until > ?)
		 LIMIT 1`,
		winner.CandidateID, winner.JobID, winner.JobAttemptRevision,
		winner.BrowserHolderGeneration, now).Scan(&liveClaim)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactWinner{}, false, ErrMaterializationStale
	}
	if err != nil {
		return ArtifactWinner{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO artifact_winners
		(job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, winner.JobID, winner.JobAttemptRevision,
		winner.CandidateID, winner.BrowserHolderGeneration, winner.SHA256, winner.CreatedAt)
	if err != nil {
		return ArtifactWinner{}, false, err
	}
	err = tx.QueryRowContext(ctx, `SELECT job_id, job_attempt_revision, candidate_id,
		browser_holder_generation, sha256, created_at
		FROM artifact_winners WHERE job_id = ? AND job_attempt_revision = ?`,
		winner.JobID, winner.JobAttemptRevision).Scan(
		&existing.JobID, &existing.JobAttemptRevision, &existing.CandidateID,
		&existing.BrowserHolderGeneration, &existing.SHA256, &existing.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// The job's unique winner belongs to another attempt. Do not return
		// that winner as if it were a loser for this attempt.
		return ArtifactWinner{}, false, ErrMaterializationConflict
	}
	if err != nil {
		return ArtifactWinner{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ArtifactWinner{}, false, err
	}
	return existing, existing.CandidateID == winner.CandidateID &&
		existing.JobAttemptRevision == winner.JobAttemptRevision &&
		existing.BrowserHolderGeneration == winner.BrowserHolderGeneration &&
		existing.SHA256 == winner.SHA256, nil
}

// ArtifactWinner returns the committed winner for one exact job attempt.
func (js *Store) ArtifactWinner(ctx context.Context, jobID string, jobAttemptRevision int64) (ArtifactWinner, bool, error) {
	if jobID == "" || jobAttemptRevision < 1 {
		return ArtifactWinner{}, false, errors.New("artifact winner requires a job and positive attempt")
	}
	var w ArtifactWinner
	err := js.S.DB().QueryRowContext(ctx, `SELECT job_id, job_attempt_revision, candidate_id,
		browser_holder_generation, sha256, created_at
		FROM artifact_winners WHERE job_id = ? AND job_attempt_revision = ?`,
		jobID, jobAttemptRevision).Scan(&w.JobID, &w.JobAttemptRevision, &w.CandidateID,
		&w.BrowserHolderGeneration, &w.SHA256, &w.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactWinner{}, false, nil
	}
	if err != nil {
		return ArtifactWinner{}, false, err
	}
	return w, true, nil
}
