// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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

const ProfileEvidenceTTL = 30 * time.Minute

func parseEvidenceTime(value, field string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, fmt.Errorf("profile evidence %s is required", field)
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("profile evidence %s: %w", field, err)
	}
	return t.UTC(), nil
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
// already committed row unchanged. Validity is computed from daemon receipt,
// never from producer time or a caller-provided expiry. The observed profile
// revision is an authority fence, not a historical annotation: an observation
// whose revision is no longer the live revision of a non-tombstoned profile
// was produced under a superseded identity and is rejected as stale rather
// than promoted into the current revision.
func (js *Store) RecordProfileEvidence(ctx context.Context, observation ProfileEvidenceObservation) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := recordProfileEvidenceTx(ctx, tx, &observation); err != nil {
		return err
	}
	return tx.Commit()
}

// recordProfileEvidenceTx is RecordProfileEvidence's core. It takes a
// pointer because it derives DaemonReceivedAt/ProducerObservedAt/ExpiresAt
// in place, matching RecordProfileEvidence's original by-value semantics
// (the derived fields are call-local; ApplyClaimObservation's caller in
// claim_observation_apply.go only needs its own local copy consistent for
// the ConvertAuthenticationEntryLeaseToHuman call that follows).
func recordProfileEvidenceTx(ctx context.Context, q dbtx, observation *ProfileEvidenceObservation) error {
	if err := observation.validate(); err != nil {
		return err
	}
	if observation.ProducerObservedAt == "" {
		observation.ProducerObservedAt = store.Now()
	}
	// Receipt authority belongs to this daemon process. Caller timestamps are
	// producer metadata only and cannot extend evidence validity.
	received := time.Now().UTC()
	produced, err := parseEvidenceTime(observation.ProducerObservedAt, "producer_observed_at")
	if err != nil {
		return err
	}
	observation.DaemonReceivedAt = received.Format(time.RFC3339Nano)
	observation.ProducerObservedAt = produced.Format(time.RFC3339Nano)
	observation.ExpiresAt = received.Add(ProfileEvidenceTTL).Format(time.RFC3339Nano)
	var liveProfile int
	if err := q.QueryRowContext(ctx, `SELECT 1 FROM institution_profiles
		WHERE id = ? AND revision = ? AND (tombstoned_at IS NULL OR tombstoned_at = '')`,
		observation.InstitutionProfileID, observation.InstitutionProfileRevision).Scan(&liveProfile); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrProfileEvidenceStale
		}
		return err
	}
	result, err := q.ExecContext(ctx, `
		INSERT OR IGNORE INTO profile_evidence
		 (observation_id, browser_holder_generation, institution_profile_id,
		  institution_profile_revision, verdict, source, producer_observed_at,
		  daemon_received_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		observation.ObservationID, observation.BrowserHolderGeneration,
		observation.InstitutionProfileID, observation.InstitutionProfileRevision,
		string(observation.Verdict), string(observation.Source), observation.ProducerObservedAt,
		observation.DaemonReceivedAt, observation.ExpiresAt)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		var existing ProfileEvidenceObservation
		if err := q.QueryRowContext(ctx, `
			SELECT observation_id, browser_holder_generation, institution_profile_id,
			       institution_profile_revision, verdict, source, producer_observed_at,
			       daemon_received_at, expires_at
			FROM profile_evidence WHERE observation_id=?`, observation.ObservationID).
			Scan(&existing.ObservationID, &existing.BrowserHolderGeneration,
				&existing.InstitutionProfileID, &existing.InstitutionProfileRevision,
				&existing.Verdict, &existing.Source, &existing.ProducerObservedAt,
				&existing.DaemonReceivedAt, &existing.ExpiresAt); err != nil {
			return err
		}
		if existing.BrowserHolderGeneration != observation.BrowserHolderGeneration ||
			existing.InstitutionProfileID != observation.InstitutionProfileID ||
			existing.InstitutionProfileRevision != observation.InstitutionProfileRevision ||
			existing.Verdict != observation.Verdict || existing.Source != observation.Source {
			return ErrConflict
		}
	}
	return nil
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
	cutoff := time.Now().UTC().Add(-ProfileEvidenceTTL).Format(time.RFC3339Nano)
	args := []any{profileID, profileRevision, holderGeneration, cutoff}
	err := scan(js.S.DB().QueryRowContext(ctx, `
		SELECT observation_id, browser_holder_generation, institution_profile_id,
		       institution_profile_revision, verdict, source, producer_observed_at,
		       daemon_received_at, expires_at
		FROM profile_evidence
		WHERE institution_profile_id = ? AND institution_profile_revision = ?
		  AND browser_holder_generation = ? AND daemon_received_at > ?
		  AND verdict NOT IN ('unknown', 'inconclusive')
		ORDER BY CASE WHEN verdict IN ('auth_returned','signed_out') THEN 1 ELSE 0 END DESC,
		         daemon_received_at DESC, observation_id DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		err = scan(js.S.DB().QueryRowContext(ctx, `
			SELECT observation_id, browser_holder_generation, institution_profile_id,
			       institution_profile_revision, verdict, source, producer_observed_at,
			       daemon_received_at, expires_at
			FROM profile_evidence
			WHERE institution_profile_id = ? AND institution_profile_revision = ?
			  AND browser_holder_generation = ? AND daemon_received_at > ?
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
// DependentJobIDs and ClaimMemberJobIDs are sets: callers may report the same
// sibling repeatedly and the current projection remains idempotent. They are
// persisted inside detail_json so this Phase 1 schema remains URL-free.
type HumanGateObservation struct {
	ID                    string          `json:"id"`
	GateType              HumanGateType   `json:"gate_type"`
	ScopeClass            string          `json:"scope_class"`
	ScopeKey              string          `json:"scope_key"`
	InstitutionProfileID  string          `json:"institution_profile_id,omitempty"`
	BindingID             string          `json:"binding_id,omitempty"`
	AuthenticationClaimID string          `json:"authentication_claim_id,omitempty"`
	DependentJobIDs       []string        `json:"dependent_job_ids,omitempty"`
	ClaimMemberJobIDs     []string        `json:"claim_member_job_ids,omitempty"`
	ObservationRevision   int64           `json:"observation_revision"`
	Status                HumanGateStatus `json:"status"`
	DetailJSON            string          `json:"detail_json"`
	CreatedAt             string          `json:"created_at"`
	UpdatedAt             string          `json:"updated_at"`
}

type humanGatePersistedDetail struct {
	DetailJSON            string   `json:"detail_json"`
	AuthenticationClaimID string   `json:"authentication_claim_id,omitempty"`
	DependentJobIDs       []string `json:"dependent_job_ids,omitempty"`
	ClaimMemberJobIDs     []string `json:"claim_member_job_ids,omitempty"`
}

func normalizeHumanGateIDs(ids []string) ([]string, error) {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || len(id) > 128 {
			return nil, errors.New("human gate dependent job ids must be bounded and non-empty")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func (o HumanGateObservation) persistedDetail() (string, error) {
	if o.DetailJSON == "" {
		o.DetailJSON = "{}"
	}
	jobs, err := normalizeHumanGateIDs(o.DependentJobIDs)
	if err != nil {
		return "", err
	}
	claimJobs, err := normalizeHumanGateIDs(o.ClaimMemberJobIDs)
	if err != nil {
		return "", err
	}
	if o.AuthenticationClaimID != "" && len(o.AuthenticationClaimID) > 256 {
		return "", errors.New("human gate authentication claim id is too long")
	}
	if len(jobs) == 0 && len(claimJobs) == 0 && o.AuthenticationClaimID == "" {
		return o.DetailJSON, nil
	}
	raw, err := json.Marshal(humanGatePersistedDetail{
		DetailJSON: o.DetailJSON, AuthenticationClaimID: o.AuthenticationClaimID,
		DependentJobIDs: jobs, ClaimMemberJobIDs: claimJobs,
	})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func decodeHumanGateDetail(raw string, o *HumanGateObservation) error {
	var detail humanGatePersistedDetail
	if err := json.Unmarshal([]byte(raw), &detail); err != nil || detail.DetailJSON == "" {
		o.DetailJSON = raw
		return nil
	}
	o.DetailJSON = detail.DetailJSON
	o.AuthenticationClaimID = detail.AuthenticationClaimID
	o.DependentJobIDs = append([]string(nil), detail.DependentJobIDs...)
	o.ClaimMemberJobIDs = append([]string(nil), detail.ClaimMemberJobIDs...)
	return nil
}

func (o HumanGateObservation) validate() error {
	if o.ID == "" || len(o.ID) > 128 || o.ScopeClass == "" || len(o.ScopeClass) > 128 || o.ScopeKey == "" || len(o.ScopeKey) > 256 {
		return errors.New("human gate requires bounded id and scope")
	}
	normalizedDetail := strings.ReplaceAll(o.DetailJSON, `\/`, "/")
	lowerDetail := strings.ToLower(normalizedDetail)
	if strings.Contains(o.ScopeKey, "://") || strings.Contains(normalizedDetail, "://") ||
		strings.Contains(lowerDetail, `"password"`) || strings.Contains(lowerDetail, `"credential"`) ||
		strings.Contains(lowerDetail, `"cookie"`) || strings.Contains(lowerDetail, `"token"`) {
		return errors.New("human gate observations cannot persist URLs or credentials")
	}
	if !validHumanGateScopeClass(o.ScopeClass) || !o.GateType.valid() || !o.Status.valid() || o.ObservationRevision < 1 {
		return errors.New("invalid human gate scope, type, status, or revision")
	}
	switch o.GateType {
	case HumanGateBrowserHostPermission:
		if o.ScopeClass != string(HumanGateScopeBrowserHost) {
			return errors.New("browser-host permission gate requires browser_host scope")
		}
	case HumanGateDownloadsFolderPermission:
		if o.ScopeClass != string(HumanGateScopePlatform) {
			return errors.New("downloads-folder permission gate requires platform scope")
		}
	}
	if _, err := normalizeHumanGateIDs(o.DependentJobIDs); err != nil {
		return err
	}
	if _, err := normalizeHumanGateIDs(o.ClaimMemberJobIDs); err != nil {
		return err
	}
	if o.AuthenticationClaimID != "" && len(o.AuthenticationClaimID) > 256 {
		return errors.New("human gate authentication claim id is too long")
	}
	return nil
}

func deriveHumanGateJobIDs(ctx context.Context, tx *sql.Tx, observation HumanGateObservation) ([]string, error) {
	var query string
	var arg string
	switch HumanGateScopeClass(observation.ScopeClass) {
	case HumanGateScopeAuthenticationClaim:
		query = `SELECT DISTINCT bc.job_id
			FROM browser_candidates bc
			JOIN institution_profiles p ON p.id = bc.institution_profile_id
			JOIN jobs j ON j.id = bc.job_id
			WHERE p.authentication_claim_id = ? AND p.tombstoned_at IS NULL
			  AND p.revision = bc.institution_profile_revision
			  AND bc.job_attempt_revision = (SELECT MAX(current_bc.job_attempt_revision) FROM browser_candidates current_bc WHERE current_bc.job_id=bc.job_id)
			  AND j.state NOT IN ('ready','imported','unavailable','failed','cancelled')
			  AND bc.status IN ('eligible','claimed','materializing')`
		arg = observation.AuthenticationClaimID
	case HumanGateScopeInstitutionProfile:
		query = `SELECT DISTINCT bc.job_id
			FROM browser_candidates bc
			JOIN institution_profiles p ON p.id = bc.institution_profile_id
			JOIN jobs j ON j.id = bc.job_id
			WHERE bc.institution_profile_id = ? AND p.tombstoned_at IS NULL
			  AND p.revision = bc.institution_profile_revision
			  AND bc.job_attempt_revision = (SELECT MAX(current_bc.job_attempt_revision) FROM browser_candidates current_bc WHERE current_bc.job_id=bc.job_id)
			  AND j.state NOT IN ('ready','imported','unavailable','failed','cancelled')
			  AND bc.status IN ('eligible','claimed','materializing')`
		arg = observation.InstitutionProfileID
	case HumanGateScopeBinding:
		query = `SELECT DISTINCT bc.job_id
			FROM materialization_claims mc
			JOIN browser_candidates bc ON bc.id = mc.candidate_id
			WHERE mc.binding_id = ?`
		arg = observation.ScopeKey
	default:
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// UpsertHumanGateObservation atomically advances one current scope projection.
// A stale revision and a lost-response duplicate are both no-ops. Newer
// observations union dependent siblings and claim members with the current row,
// so one live surface survives reports arriving from many jobs.
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
	derived, err := deriveHumanGateJobIDs(ctx, tx, observation)
	if err != nil {
		return err
	}
	observation.DependentJobIDs = append(observation.DependentJobIDs, derived...)
	if observation.ScopeClass == string(HumanGateScopeAuthenticationClaim) {
		observation.ClaimMemberJobIDs = append(observation.ClaimMemberJobIDs, derived...)
	}
	var idGateType, idScopeClass, idScopeKey, idStatus string
	var idRevision int64
	err = tx.QueryRowContext(ctx, `
		SELECT gate_type, scope_class, scope_key, status, observation_revision
		FROM human_gate_observations WHERE id = ?`, observation.ID).
		Scan(&idGateType, &idScopeClass, &idScopeKey, &idStatus, &idRevision)
	if err == nil {
		if idGateType != string(observation.GateType) || idScopeClass != observation.ScopeClass ||
			idScopeKey != observation.ScopeKey || idStatus != string(observation.Status) ||
			idRevision != observation.ObservationRevision {
			return ErrConflict
		}
		return tx.Commit()
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	var currentRevision int64
	var currentDetail string
	var currentCreatedAt string
	err = tx.QueryRowContext(ctx, `
		SELECT observation_revision, detail_json, created_at
		FROM human_gate_observations
		WHERE gate_type = ? AND scope_class = ? AND scope_key = ?`,
		string(observation.GateType), observation.ScopeClass, observation.ScopeKey).
		Scan(&currentRevision, &currentDetail, &currentCreatedAt)
	if err == nil {
		if observation.ObservationRevision <= currentRevision {
			return tx.Commit()
		}
		var current HumanGateObservation
		_ = decodeHumanGateDetail(currentDetail, &current)
		observation.DependentJobIDs = append(observation.DependentJobIDs, current.DependentJobIDs...)
		observation.ClaimMemberJobIDs = append(observation.ClaimMemberJobIDs, current.ClaimMemberJobIDs...)
		if observation.AuthenticationClaimID == "" {
			observation.AuthenticationClaimID = current.AuthenticationClaimID
		}
		observation.CreatedAt = currentCreatedAt
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	persisted, err := observation.persistedDetail()
	if err != nil {
		return err
	}
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
		string(observation.Status), persisted, observation.CreatedAt, observation.UpdatedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (js *Store) CurrentHumanGateObservations(ctx context.Context, scopeClass, scopeKey string) ([]HumanGateObservation, error) {
	if !validHumanGateScopeClass(scopeClass) || scopeKey == "" {
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
		var detail string
		if err := rows.Scan(&o.ID, &o.GateType, &o.ScopeClass, &o.ScopeKey, &o.InstitutionProfileID, &o.BindingID, &o.ObservationRevision, &o.Status, &detail, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		if err := decodeHumanGateDetail(detail, &o); err != nil {
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

// CommitArtifactWinnerAndProducer atomically commits or reuses the per-attempt
// artifact winner and settles only the exact effect producer. A correlated
// artifact may establish its historical winner after the materialization claim
// expires or is replaced; current claim projection remains separately fenced.
// Passing a nil producer preserves ordinary uncorrelated winner behavior and
// still requires a live claim.
func (js *Store) CommitArtifactWinnerAndProducer(
	ctx context.Context,
	winner ArtifactWinner,
	producer *ArtifactProducerIdentity,
) (ArtifactWinner, bool, bool, error) {
	if err := winner.validate(); err != nil {
		return ArtifactWinner{}, false, false, err
	}
	if producer != nil {
		if err := producer.validate(winner.JobID); err != nil {
			return ArtifactWinner{}, false, false, err
		}
	}
	if winner.CreatedAt == "" {
		winner.CreatedAt = store.Now()
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return ArtifactWinner{}, false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if producer != nil {
		// Artifact producer identity is historical. A file can arrive after the
		// bridge and materialization claim move to a replacement holder, so the
		// current caller generation is only provisional. Prefer the exact
		// producer's durable generation for both a new winner and winner replay.
		permitWhere, permitArgs := identityWhere(producer.effectIdentity(winner.JobID))
		var producerHolder int64
		holderErr := tx.QueryRowContext(ctx,
			`SELECT browser_holder_generation FROM effect_permits WHERE `+permitWhere,
			permitArgs...).Scan(&producerHolder)
		if errors.Is(holderErr, sql.ErrNoRows) {
			legacyWhere, legacyArgs := legacyBlockerIdentityWhere(producer.legacyIdentity(winner.JobID))
			var reconstructedHolder sql.NullInt64
			holderErr = tx.QueryRowContext(ctx,
				`SELECT reconstructed_holder FROM legacy_effect_blockers WHERE `+legacyWhere,
				legacyArgs...).Scan(&reconstructedHolder)
			if holderErr == nil && reconstructedHolder.Valid {
				producerHolder = reconstructedHolder.Int64
			} else if holderErr == nil {
				producerHolder = winner.BrowserHolderGeneration
			}
		}
		if holderErr != nil && !errors.Is(holderErr, sql.ErrNoRows) {
			return ArtifactWinner{}, false, false, holderErr
		}
		if holderErr == nil {
			winner.BrowserHolderGeneration = producerHolder
		}
	}

	var existing ArtifactWinner
	err = tx.QueryRowContext(ctx, `SELECT job_id, job_attempt_revision, candidate_id,
		browser_holder_generation, sha256, created_at
		FROM artifact_winners WHERE job_id=? AND job_attempt_revision=?`,
		winner.JobID, winner.JobAttemptRevision).Scan(
		&existing.JobID, &existing.JobAttemptRevision, &existing.CandidateID,
		&existing.BrowserHolderGeneration, &existing.SHA256, &existing.CreatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var claimMatch int
		if producer != nil {
			// Exact producer correlation proves that validated bytes came from
			// this historical materialization. The current claim row may have
			// replaced its holder generation, so preserve candidate/job/attempt
			// linkage here and verify the producing generation from the durable
			// permit or reconstructed legacy blocker below.
			err = tx.QueryRowContext(ctx, `
				SELECT 1
				  FROM materialization_claims m
				  JOIN browser_candidates c ON c.id=m.candidate_id
				 WHERE m.candidate_id=? AND c.job_id=?
				   AND c.job_attempt_revision=?
				 LIMIT 1`,
				winner.CandidateID, winner.JobID, winner.JobAttemptRevision).Scan(&claimMatch)
		} else {
			now := store.Now()
			err = tx.QueryRowContext(ctx, `
				SELECT 1
				  FROM materialization_claims m
				  JOIN browser_candidates c ON c.id=m.candidate_id
				  JOIN institution_profiles p ON p.id=c.institution_profile_id
				 WHERE p.tombstoned_at IS NULL
				   AND p.revision=c.institution_profile_revision
				   AND m.candidate_id=? AND c.job_id=?
				   AND c.job_attempt_revision=? AND m.browser_holder_generation=?
				   AND m.phase IN ('claimed','bound','route_issued','navigated')
				   AND (m.lease_until IS NULL OR m.lease_until>?)
				 LIMIT 1`,
				winner.CandidateID, winner.JobID, winner.JobAttemptRevision,
				winner.BrowserHolderGeneration, now).Scan(&claimMatch)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return ArtifactWinner{}, false, false, ErrMaterializationStale
		}
		if err != nil {
			return ArtifactWinner{}, false, false, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO artifact_winners
			(job_id,job_attempt_revision,candidate_id,browser_holder_generation,sha256,created_at)
			VALUES(?,?,?,?,?,?)`, winner.JobID, winner.JobAttemptRevision,
			winner.CandidateID, winner.BrowserHolderGeneration, winner.SHA256,
			winner.CreatedAt); err != nil {
			return ArtifactWinner{}, false, false, err
		}
		existing = winner
	case err != nil:
		return ArtifactWinner{}, false, false, err
	}
	won := existing.CandidateID == winner.CandidateID &&
		existing.JobAttemptRevision == winner.JobAttemptRevision &&
		existing.BrowserHolderGeneration == winner.BrowserHolderGeneration &&
		existing.SHA256 == winner.SHA256
	settled := false
	if producer != nil {
		settled, err = settleArtifactProducerTx(ctx, tx, winner.JobID, *producer)
		if err != nil {
			return ArtifactWinner{}, false, false, err
		}
		if !settled {
			// A correlated artifact must release one exact durable producer.
			// Committing a winner without that release would leave occupancy
			// stranded, so roll back both sides of this transaction.
			return ArtifactWinner{}, false, false, ErrEffectPermitStale
		}
	}
	if err := tx.Commit(); err != nil {
		return ArtifactWinner{}, false, false, err
	}
	return existing, won, settled, nil
}

// SettleArtifactProducer settles an exact producer when no materialization
// candidate exists. This never creates an artifact winner and never guesses
// from job identity alone.
func (js *Store) SettleArtifactProducer(ctx context.Context, jobID string, producer ArtifactProducerIdentity) (bool, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	settled, err := settleArtifactProducerTx(ctx, tx, jobID, producer)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return settled, nil
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

// AuthenticationEntryLeaseState describes durable ownership of one
// authentication claim. A human lease is intentionally distinct from a
// profile's evidence: sharing a claim never shares profile warm facts.
type AuthenticationEntryLeaseState string

const (
	AuthenticationEntryLeaseReserved AuthenticationEntryLeaseState = "reserved"
	AuthenticationEntryLeaseHuman    AuthenticationEntryLeaseState = "human"
	AuthenticationEntryLeaseExpired  AuthenticationEntryLeaseState = "expired"
)

// AuthenticationEntryBindDeadline bounds how long an entry reservation with no
// bound surface may hold its institution. It covers a grant -> open -> bind
// round trip (one browser poll plus a tab creation, ~2s in practice) with two
// orders of magnitude to spare, and nothing else: a reservation nobody binds is
// a permission that was never used, and it otherwise blocks every other paper's
// consult on that institution for the whole action-expiry window. A bound
// sign-in is extended in full by §4.5's human-paced renewals instead.
const AuthenticationEntryBindDeadline = 2 * time.Minute

var (
	ErrAuthenticationEntryLeaseBusy   = errors.New("authentication entry lease busy")
	ErrAuthenticationEntryLeaseStale  = errors.New("authentication entry lease stale")
	ErrAuthenticationEntryLeaseDenied = errors.New("authentication entry lease evidence required")
	// ErrProfileEvidenceStale reports an observation whose institution profile
	// revision is no longer live. The observation is discarded rather than
	// rebound to the current revision.
	ErrProfileEvidenceStale = errors.New("profile evidence revision is stale")
)

type AuthenticationEntryLeaseInput struct {
	AuthenticationClaimID   string
	LeaseID                 string
	OwnerID                 string
	BrowserHolderGeneration int64
	LeaseUntil              time.Time
}

type AuthenticationEntryLease struct {
	AuthenticationClaimID   string                        `json:"authentication_claim_id"`
	LeaseID                 string                        `json:"lease_id"`
	OwnerID                 string                        `json:"owner_id"`
	BrowserHolderGeneration int64                         `json:"browser_holder_generation"`
	State                   AuthenticationEntryLeaseState `json:"state"`
	LeaseUntil              string                        `json:"lease_until,omitempty"`
	HumanOwnerID            string                        `json:"human_owner_id,omitempty"`
	EvidenceObservationID   string                        `json:"evidence_observation_id,omitempty"`
	// OwnerBindingID/OwnerTabHint name the surface currently occupying this
	// lease, set when the owning candidate's institutional_bind_response
	// lands and cleared on lease reassignment or owner_closed (claim-
	// observation-protocol.md §4.1).
	OwnerBindingID string `json:"owner_binding_id,omitempty"`
	OwnerTabHint   *int64 `json:"owner_tab_hint,omitempty"`
	// EntitledAt is set only by a fenced entitled_landing observation
	// (claim_observation_apply.go) on the exact live human occupancy that
	// produced it, and cleared whenever that occupancy ends (a fresh
	// reservation cycle, or owner_closed retiring the lease). It is the
	// durable signal admitAutomaticMaterializationCandidates gates dependent
	// admission on: a merely "human" lease means auth_returned landed
	// somewhere, not that the daemon confirmed entitled content — resuming
	// siblings on state alone would resume them on an IdP bounce or a
	// wrong-work return.
	EntitledAt string `json:"entitled_at,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// authenticationEntryLeaseSelect is the shared column list every reader of
// authentication_entry_leases uses, so a future column addition changes one
// place instead of drifting across Reserve/Get's independent SELECTs.
const authenticationEntryLeaseSelect = `
	SELECT authentication_claim_id, lease_id, owner_id, browser_holder_generation,
	       state, COALESCE(lease_until,''), COALESCE(human_owner_id,''),
	       COALESCE(evidence_observation_id,''), COALESCE(owner_binding_id,''),
	       owner_tab_hint, COALESCE(entitled_at,''), created_at, updated_at
	FROM authentication_entry_leases`

// scanAuthenticationEntryLease reads one row via scan (*sql.Row.Scan or
// *sql.Rows.Scan), matching authenticationEntryLeaseSelect's column order.
func scanAuthenticationEntryLease(scan func(...any) error) (AuthenticationEntryLease, error) {
	var l AuthenticationEntryLease
	var ownerTabHint sql.NullInt64
	if err := scan(&l.AuthenticationClaimID, &l.LeaseID, &l.OwnerID, &l.BrowserHolderGeneration,
		&l.State, &l.LeaseUntil, &l.HumanOwnerID, &l.EvidenceObservationID,
		&l.OwnerBindingID, &ownerTabHint, &l.EntitledAt, &l.CreatedAt, &l.UpdatedAt); err != nil {
		return AuthenticationEntryLease{}, err
	}
	if ownerTabHint.Valid {
		v := ownerTabHint.Int64
		l.OwnerTabHint = &v
	}
	return l, nil
}

func (in AuthenticationEntryLeaseInput) validate() error {
	if strings.TrimSpace(in.AuthenticationClaimID) == "" || len(in.AuthenticationClaimID) > 256 ||
		strings.TrimSpace(in.LeaseID) == "" || len(in.LeaseID) > 128 ||
		strings.TrimSpace(in.OwnerID) == "" || len(in.OwnerID) > 256 ||
		in.BrowserHolderGeneration < 0 || in.LeaseUntil.IsZero() {
		return errors.New("invalid authentication entry lease input")
	}
	return nil
}

// ReserveAuthenticationEntryLease claims one authentication entry for an
// owner. A replay of the same reservation is idempotent; a different live
// reservation receives Busy. A new observed authentication-entry attempt
// supersedes prior human ownership, while expired reservations are replaced
// atomically, including after a daemon restart.
func (js *Store) ReserveAuthenticationEntryLease(ctx context.Context, in AuthenticationEntryLeaseInput) (*AuthenticationEntryLease, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	lease, err := reserveAuthenticationEntryLeaseTx(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return lease, nil
}

// reserveAuthenticationEntryLeaseTx is ReserveAuthenticationEntryLease's core,
// factored out so ApplyClaimObservation (claim_observation_apply.go) can run
// a wall/mfa/challenge renewal inside the same transaction that journals the
// observation, instead of nesting a second top-level transaction.
func reserveAuthenticationEntryLeaseTx(ctx context.Context, q dbtx, in AuthenticationEntryLeaseInput) (*AuthenticationEntryLease, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	untilText := in.LeaseUntil.UTC().Format(time.RFC3339Nano)
	// An entry nobody has bound a surface to yet is not a sign-in in progress:
	// it is a permission to open one, and it only has to outlive the
	// grant -> open -> bind round trip (one browser poll plus a tab creation).
	// Giving it the full human-paced window instead let one unconsumed grant
	// hold its institution's ONLY entry for the whole window, answering every
	// other paper's consult "authentication entry lease is unavailable" — and
	// nothing could retire it early, because the owner-close retirement fences
	// on owner_binding_id, which such an entry does not have. Observed live
	// 2026-08-19: an entry owned by a paper with zero candidates, zero claims
	// and no binding held the operator's institution from 05:14:15Z to
	// 05:44:15Z. Once a surface IS bound, §4.5's human-paced renewals extend
	// it in full on every wall/login/mfa/challenge observation, so a real
	// sign-in is never cut short by this.
	unboundUntil := untilText
	if deadline := now.Add(AuthenticationEntryBindDeadline); in.LeaseUntil.After(deadline) {
		unboundUntil = deadline.Format(time.RFC3339Nano)
	}
	row := q.QueryRowContext(ctx, authenticationEntryLeaseSelect+` WHERE authentication_claim_id = ?`, in.AuthenticationClaimID)
	current, err := scanAuthenticationEntryLease(row.Scan)
	if err == nil {
		var ownerState string
		ownerErr := q.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, current.OwnerID).Scan(&ownerState)
		if ownerErr != nil && !errors.Is(ownerErr, sql.ErrNoRows) {
			return nil, ownerErr
		}
		ownerTerminal := ownerErr == nil && Terminal(ownerState)
		humanRevoked := current.State == AuthenticationEntryLeaseHuman &&
			(current.BrowserHolderGeneration != in.BrowserHolderGeneration || ownerTerminal)
		if current.State == AuthenticationEntryLeaseHuman && !humanRevoked {
			var verdict ProfileEvidenceVerdict
			evidenceErr := q.QueryRowContext(ctx, `
				SELECT pe.verdict
				FROM profile_evidence pe
				JOIN institution_profiles p ON p.id=pe.institution_profile_id
				  AND p.revision=pe.institution_profile_revision
				WHERE p.authentication_claim_id=? AND p.tombstoned_at IS NULL
				  AND pe.browser_holder_generation=? AND pe.daemon_received_at>?
				  AND pe.verdict NOT IN ('unknown','inconclusive')
				ORDER BY CASE WHEN pe.verdict IN ('auth_returned','signed_out') THEN 1 ELSE 0 END DESC,
				         pe.daemon_received_at DESC, pe.observation_id DESC LIMIT 1`,
				in.AuthenticationClaimID, in.BrowserHolderGeneration,
				now.Add(-ProfileEvidenceTTL).Format(time.RFC3339Nano),
			).Scan(&verdict)
			switch {
			case errors.Is(evidenceErr, sql.ErrNoRows):
				humanRevoked = true
			case evidenceErr != nil:
				return nil, evidenceErr
			default:
				humanRevoked = verdict == ProfileEvidenceSignedOut
			}
		}
		reservedExpired := current.State == AuthenticationEntryLeaseReserved &&
			(ownerTerminal || current.LeaseUntil != "" && current.LeaseUntil <= nowText)
		// claim-observation-protocol.md §4.5: expiry alone never authorizes a
		// replacement while an effect permit is unresolved. An in-flight
		// browser-local navigation on the lease's own occupying surface must
		// keep occupying even past a timer.
		if reservedExpired && current.OwnerBindingID != "" {
			var occupied int
			if err := q.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM effect_permits
				WHERE binding_id=? AND effect_kind='institutional' AND status IN ('held','unknown_completion')`,
				current.OwnerBindingID).Scan(&occupied); err != nil {
				return nil, err
			}
			if occupied > 0 {
				reservedExpired = false
			}
		}
		expired := current.State == AuthenticationEntryLeaseExpired || humanRevoked || reservedExpired
		if !expired {
			// A reserved entry belongs to the JOB that is signing in, not to the
			// browser session that happened to arbitrate for it. Renewal
			// therefore keys on the owner job alone and carries the new lease id
			// and holder generation forward, so the sign-in survives a service
			// worker restart: §4.5 requires human-paced renewal precisely
			// because "a login/MFA/challenge prompt routinely outlives the
			// arbitrary action-expiry window", and MV3 sleeps a worker after
			// ~30s idle, so a reconnect mid-login is the common case rather than
			// the edge one. Requiring lease-id and generation equality here made
			// a reconnect leave the institution's only entry neither renewable
			// nor re-reservable — not even by its own owner re-consulting under
			// the new generation, whose lease id is derived from the epoch —
			// until the 30-minute timer expired. Every claim_observation for
			// that login was refused as "the entry is owned elsewhere" in the
			// meantime, which is why the journal held zero rows across weeks of
			// real sign-ins (measured live 2026-08-19).
			//
			// A settled HUMAN sign-in is deliberately NOT renewable this way:
			// humanRevoked above still treats generation churn as revocation,
			// because papio cannot verify a human session survived a browser
			// restart without fresh evidence. Only the reserved (pre-sign-in)
			// phase is holder-agnostic; the sender is separately fenced as the
			// current holder before any of this runs.
			if current.State == AuthenticationEntryLeaseReserved &&
				current.OwnerID == in.OwnerID {
				renewedUntil := untilText
				if current.OwnerBindingID == "" {
					renewedUntil = unboundUntil
				}
				if _, err := q.ExecContext(ctx, `
					UPDATE authentication_entry_leases
					   SET lease_id=?, browser_holder_generation=?, lease_until=?, updated_at=?
					 WHERE authentication_claim_id=?`,
					in.LeaseID, in.BrowserHolderGeneration, renewedUntil, nowText,
					in.AuthenticationClaimID); err != nil {
					return nil, err
				}
				current.LeaseID, current.BrowserHolderGeneration = in.LeaseID, in.BrowserHolderGeneration
				current.LeaseUntil, current.UpdatedAt = renewedUntil, nowText
				return &current, nil
			}
			return nil, ErrAuthenticationEntryLeaseBusy
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE authentication_entry_leases
			   SET lease_id=?, owner_id=?, browser_holder_generation=?, state='reserved',
			       lease_until=?, human_owner_id=NULL, evidence_observation_id=NULL,
			       owner_binding_id=NULL, owner_tab_hint=NULL, entitled_at=NULL,
			       updated_at=?
			 WHERE authentication_claim_id=?`,
			in.LeaseID, in.OwnerID, in.BrowserHolderGeneration, unboundUntil, nowText,
			in.AuthenticationClaimID); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	} else {
		if _, err := q.ExecContext(ctx, `
			INSERT INTO authentication_entry_leases
			  (authentication_claim_id, lease_id, owner_id, browser_holder_generation,
			   state, lease_until, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'reserved', ?, ?, ?)`,
			in.AuthenticationClaimID, in.LeaseID, in.OwnerID, in.BrowserHolderGeneration,
			unboundUntil, nowText, nowText); err != nil {
			return nil, err
		}
	}
	finalRow := q.QueryRowContext(ctx, authenticationEntryLeaseSelect+` WHERE authentication_claim_id=?`, in.AuthenticationClaimID)
	final, err := scanAuthenticationEntryLease(finalRow.Scan)
	if err != nil {
		return nil, err
	}
	return &final, nil
}

// GetAuthenticationEntryLease reads the durable current lease and marks an
// expired reservation before returning it. Human ownership is not erased by
// an expired reservation and requires an explicit stale/fence transition.
func (js *Store) GetAuthenticationEntryLease(ctx context.Context, authenticationClaimID string) (*AuthenticationEntryLease, bool, error) {
	return getAuthenticationEntryLeaseTx(ctx, js.S.DB(), authenticationClaimID)
}

// getAuthenticationEntryLeaseTx is GetAuthenticationEntryLease's core.
// ApplyClaimObservation (claim_observation_apply.go) runs it against its own
// transaction so the dedup/ordering check that must precede any lease
// mutation (§3) and this expiry side effect land in the observation's single
// atomic apply rather than two independent commits.
func getAuthenticationEntryLeaseTx(ctx context.Context, q dbtx, authenticationClaimID string) (*AuthenticationEntryLease, bool, error) {
	if strings.TrimSpace(authenticationClaimID) == "" {
		return nil, false, errors.New("authentication claim is required")
	}
	row := q.QueryRowContext(ctx, authenticationEntryLeaseSelect+` WHERE authentication_claim_id=?`, authenticationClaimID)
	l, err := scanAuthenticationEntryLease(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if l.State == AuthenticationEntryLeaseReserved && l.LeaseUntil != "" && l.LeaseUntil <= store.Now() {
		occupied := false
		if l.OwnerBindingID != "" {
			var n int
			if err := q.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM effect_permits
				WHERE binding_id=? AND effect_kind='institutional' AND status IN ('held','unknown_completion')`,
				l.OwnerBindingID).Scan(&n); err != nil {
				return nil, false, err
			}
			occupied = n > 0
		}
		if occupied {
			return &l, true, nil
		}
		if err := expireAuthenticationEntryLeaseTx(ctx, q, l.AuthenticationClaimID, l.BrowserHolderGeneration, l.LeaseID); err != nil && !errors.Is(err, ErrAuthenticationEntryLeaseStale) {
			return nil, false, err
		}
		// The fenced expiry may have raced a replacement reservation. Re-read
		// authority instead of returning a locally fabricated expired state.
		return getAuthenticationEntryLeaseTx(ctx, q, authenticationClaimID)
	}
	return &l, true, nil
}

// ExpireAuthenticationEntryLease releases a reserved lease only when its
// holder and lease id still match. A human owner is never silently replaced
// by an expiry callback.
func (js *Store) ExpireAuthenticationEntryLease(ctx context.Context, authenticationClaimID string, holderGeneration int64, leaseID string) error {
	return expireAuthenticationEntryLeaseTx(ctx, js.S.DB(), authenticationClaimID, holderGeneration, leaseID)
}

func expireAuthenticationEntryLeaseTx(ctx context.Context, q dbtx, authenticationClaimID string, holderGeneration int64, leaseID string) error {
	if authenticationClaimID == "" || leaseID == "" || holderGeneration < 0 {
		return errors.New("authentication entry lease expiry requires exact fence")
	}
	res, err := q.ExecContext(ctx, `
		UPDATE authentication_entry_leases
		   SET state='expired', lease_until=NULL, updated_at=?
		 WHERE authentication_claim_id=? AND lease_id=? AND browser_holder_generation=?
		   AND state='reserved'`,
		store.Now(), authenticationClaimID, leaseID, holderGeneration)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrAuthenticationEntryLeaseStale
	}
	return nil
}

// ConvertAuthenticationEntryLeaseToHuman promotes a reserved authentication
// lease only after the exact current auth-return observation is durably
// present and decisive. Signed-out, unknown, and inconclusive observations
// cannot create a human owner.
func (js *Store) ConvertAuthenticationEntryLeaseToHuman(ctx context.Context, authenticationClaimID, leaseID, ownerID string, holderGeneration int64, evidence ProfileEvidenceObservation) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := convertAuthenticationEntryLeaseToHumanTx(ctx, tx, authenticationClaimID, leaseID, ownerID, holderGeneration, evidence); err != nil {
		return err
	}
	return tx.Commit()
}

// convertAuthenticationEntryLeaseToHumanTx is ConvertAuthenticationEntryLeaseToHuman's
// core, run by ApplyClaimObservation inside the observation's own transaction
// (auth_returned's evidence insert, lease promotion, and journal record must
// commit or roll back together).
func convertAuthenticationEntryLeaseToHumanTx(ctx context.Context, q dbtx, authenticationClaimID, leaseID, ownerID string, holderGeneration int64, evidence ProfileEvidenceObservation) error {
	if authenticationClaimID == "" || leaseID == "" || ownerID == "" || holderGeneration < 0 ||
		evidence.ObservationID == "" || evidence.Verdict != ProfileEvidenceAuthReturned ||
		evidence.Source != ProfileEvidenceAuthReturn {
		return errors.New("authentication entry lease conversion requires exact auth-return evidence")
	}
	now := store.Now()
	var observed ProfileEvidenceObservation
	err := q.QueryRowContext(ctx, `
		SELECT observation_id, browser_holder_generation, institution_profile_id,
		       institution_profile_revision, verdict, source, producer_observed_at,
		       daemon_received_at, expires_at
		FROM profile_evidence WHERE observation_id=?`,
		evidence.ObservationID).Scan(&observed.ObservationID, &observed.BrowserHolderGeneration,
		&observed.InstitutionProfileID, &observed.InstitutionProfileRevision, &observed.Verdict,
		&observed.Source, &observed.ProducerObservedAt, &observed.DaemonReceivedAt, &observed.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAuthenticationEntryLeaseDenied
	}
	if err != nil {
		return err
	}
	if observed.BrowserHolderGeneration != holderGeneration ||
		observed.Verdict != evidence.Verdict ||
		observed.Source != evidence.Source ||
		observed.Verdict != ProfileEvidenceAuthReturned ||
		observed.Source != ProfileEvidenceAuthReturn ||
		observed.DaemonReceivedAt <= time.Now().UTC().Add(-ProfileEvidenceTTL).Format(time.RFC3339Nano) {
		return ErrAuthenticationEntryLeaseDenied
	}
	var profileMatchesClaim int
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM institution_profiles
		WHERE id=? AND revision=? AND authentication_claim_id=?
		  AND (tombstoned_at IS NULL OR tombstoned_at='')`,
		observed.InstitutionProfileID, observed.InstitutionProfileRevision, authenticationClaimID,
	).Scan(&profileMatchesClaim); err != nil {
		return err
	}
	if profileMatchesClaim != 1 {
		return ErrAuthenticationEntryLeaseDenied
	}
	var currentObservationID string
	if err := q.QueryRowContext(ctx, `
		SELECT observation_id FROM profile_evidence
		WHERE institution_profile_id=? AND institution_profile_revision=?
		  AND browser_holder_generation=? AND daemon_received_at>?
		  AND verdict NOT IN ('unknown','inconclusive')
		ORDER BY daemon_received_at DESC, observation_id DESC LIMIT 1`,
		observed.InstitutionProfileID, observed.InstitutionProfileRevision, holderGeneration,
		time.Now().UTC().Add(-ProfileEvidenceTTL).Format(time.RFC3339Nano),
	).Scan(&currentObservationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAuthenticationEntryLeaseDenied
		}
		return err
	}
	if currentObservationID != observed.ObservationID {
		return ErrAuthenticationEntryLeaseDenied
	}
	res, err := q.ExecContext(ctx, `
		UPDATE authentication_entry_leases
		   SET state='human', human_owner_id=?, evidence_observation_id=?,
		       lease_until=NULL, entitled_at=NULL, updated_at=?
		 WHERE authentication_claim_id=? AND lease_id=? AND owner_id=?
		   AND browser_holder_generation=? AND state='reserved'
		   AND (lease_until IS NULL OR lease_until > ?)`,
		ownerID, observed.ObservationID, now, authenticationClaimID, leaseID, ownerID,
		holderGeneration, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrAuthenticationEntryLeaseStale
	}
	return nil
}

// SetAuthenticationEntryLeaseOwnerBinding records the physical surface
// currently occupying a live authentication-entry lease
// (claim-observation-protocol.md §4.1). Called when the owning candidate's
// institutional_bind_response lands. Best-effort and non-authoritative,
// matching this package's other side-channel bookkeeping: a mismatched
// fence (the lease was reassigned, expired, or is held by a different
// owner/generation) is a silent no-op rather than an error, because a stale
// bind side effect must never resurrect or steal a lease.
func (js *Store) SetAuthenticationEntryLeaseOwnerBinding(ctx context.Context, authenticationClaimID, ownerID string, holderGeneration int64, bindingID string, tabID int64) error {
	_, err := setAuthenticationEntryLeaseOwnerBindingTx(ctx, js.S.DB(), authenticationClaimID, ownerID, holderGeneration, bindingID, tabID)
	return err
}

// setAuthenticationEntryLeaseOwnerBindingTx is SetAuthenticationEntryLeaseOwnerBinding's
// core. institutionalBind (internal/browser/bridge.go) runs it inside the
// same transaction as BindMaterialization so a fenced no-op here fails the
// whole bind instead of leaving a bound scaffold with no recorded lease
// owner (BindMaterializationWithLeaseOwner, institutional_materialization.go).
func setAuthenticationEntryLeaseOwnerBindingTx(ctx context.Context, q dbtx, authenticationClaimID, ownerID string, holderGeneration int64, bindingID string, tabID int64) (sql.Result, error) {
	if strings.TrimSpace(authenticationClaimID) == "" || strings.TrimSpace(ownerID) == "" ||
		holderGeneration < 0 || strings.TrimSpace(bindingID) == "" || tabID < 0 {
		return nil, errors.New("authentication entry lease owner binding requires exact fence")
	}
	return q.ExecContext(ctx, `
		UPDATE authentication_entry_leases
		   SET owner_binding_id=?, owner_tab_hint=?, updated_at=?
		 WHERE authentication_claim_id=? AND owner_id=? AND browser_holder_generation=?
		   AND state IN ('reserved','human')`,
		bindingID, tabID, store.Now(), authenticationClaimID, ownerID, holderGeneration)
}

// ClearAuthenticationEntryLeaseOwnerBinding clears owner_binding_id/
// owner_tab_hint for the exact binding an owner_closed observation names
// (claim-observation-protocol.md §2.2.1). The lease itself stays whatever
// state its own expiry/promotion rules already put it in — closing a tab is
// not evidence about the sign-in outcome. Silent no-op when the binding no
// longer matches (already reassigned or already cleared).
func (js *Store) ClearAuthenticationEntryLeaseOwnerBinding(ctx context.Context, authenticationClaimID, bindingID string) error {
	if strings.TrimSpace(authenticationClaimID) == "" || strings.TrimSpace(bindingID) == "" {
		return errors.New("authentication entry lease owner binding clear requires exact fence")
	}
	_, err := js.S.DB().ExecContext(ctx, `
		UPDATE authentication_entry_leases
		   SET owner_binding_id=NULL, owner_tab_hint=NULL, updated_at=?
		 WHERE authentication_claim_id=? AND owner_binding_id=?`,
		store.Now(), authenticationClaimID, bindingID)
	return err
}

// MarkAuthenticationEntryLeaseEntitled durably records that a fenced
// entitled_landing observation applied to the exact live human occupancy
// named by leaseID/ownerID/holderGeneration (claim_observation_apply.go).
// admitAutomaticMaterializationCandidates gates dependent admission on this
// column rather than state='human' alone, because a merely-human lease only
// proves auth_returned landed somewhere — not that the daemon confirmed
// entitled content rather than an IdP bounce or a wrong-work return.
func (js *Store) MarkAuthenticationEntryLeaseEntitled(ctx context.Context, authenticationClaimID, leaseID, ownerID string, holderGeneration int64, at string) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := markAuthenticationEntryLeaseEntitledTx(ctx, tx, authenticationClaimID, leaseID, ownerID, holderGeneration, at); err != nil {
		return err
	}
	return tx.Commit()
}

func markAuthenticationEntryLeaseEntitledTx(ctx context.Context, q dbtx, authenticationClaimID, leaseID, ownerID string, holderGeneration int64, at string) error {
	if strings.TrimSpace(authenticationClaimID) == "" || strings.TrimSpace(leaseID) == "" ||
		strings.TrimSpace(ownerID) == "" || holderGeneration < 0 || strings.TrimSpace(at) == "" {
		return errors.New("authentication entry lease entitlement requires exact fence")
	}
	res, err := q.ExecContext(ctx, `
		UPDATE authentication_entry_leases
		   SET entitled_at=?, updated_at=?
		 WHERE authentication_claim_id=? AND lease_id=? AND owner_id=? AND human_owner_id=?
		   AND browser_holder_generation=? AND state='human'`,
		at, at, authenticationClaimID, leaseID, ownerID, ownerID, holderGeneration)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrAuthenticationEntryLeaseStale
	}
	return nil
}

// RetireAuthenticationEntryLeaseAfterOwnerClose is owner_closed's lease-side
// effect (claim-observation-protocol.md §2.2.1, claim_observation_apply.go).
// Unlike ClearAuthenticationEntryLeaseOwnerBinding (which only drops the
// occupancy pointer and leaves the lease's own state/entitlement alone), this
// fully retires the claim's current occupancy: owner_binding_id/owner_tab_hint
// and entitled_at are cleared, and the lease itself is dropped to 'expired'
// so a later authentication_claim_request grants a brand new reservation
// cycle instead of the claim parking forever. Fenced by the exact
// owner_binding_id the closing observation names; already reassigned or
// already-cleared bindings are a silent no-op, matching
// ClearAuthenticationEntryLeaseOwnerBinding's contract.
func (js *Store) RetireAuthenticationEntryLeaseAfterOwnerClose(ctx context.Context, authenticationClaimID, bindingID string, now time.Time) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := retireAuthenticationEntryLeaseAfterOwnerCloseTx(ctx, tx, authenticationClaimID, bindingID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func retireAuthenticationEntryLeaseAfterOwnerCloseTx(ctx context.Context, q dbtx, authenticationClaimID, bindingID string, now time.Time) error {
	if strings.TrimSpace(authenticationClaimID) == "" || strings.TrimSpace(bindingID) == "" {
		return errors.New("authentication entry lease owner-close retirement requires exact fence")
	}
	_, err := q.ExecContext(ctx, `
		UPDATE authentication_entry_leases
		   SET state='expired', lease_until=NULL, owner_binding_id=NULL,
		       owner_tab_hint=NULL, entitled_at=NULL, updated_at=?
		 WHERE authentication_claim_id=? AND owner_binding_id=?
		   AND state IN ('reserved','human')`,
		now.UTC().Format(time.RFC3339Nano), authenticationClaimID, bindingID)
	return err
}
