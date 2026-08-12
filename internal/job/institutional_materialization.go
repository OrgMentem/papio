package job

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"papio/internal/store"
)

// ErrMaterializationConflict reports a compare-and-swap that lost to a newer
// durable state. ErrMaterializationStale is returned when a caller's holder or
// authority fence no longer names the current claim.
var (
	ErrMaterializationConflict = errors.New("materialization state conflict")
	ErrMaterializationStale    = errors.New("materialization state stale")
	ErrMaterializationBusy     = errors.New("materialization claim busy")
)

// InstitutionAuthorityKey returns the singleton daemon-private HMAC key,
// creating it transactionally on first use. The key is returned only to
// daemon code that derives opaque authority identities; it is never serialized
// into protocol, event, diagnostic, or configuration data.
func (js *Store) InstitutionAuthorityKey(ctx context.Context) ([]byte, error) {
	var generated [32]byte
	if _, err := rand.Read(generated[:]); err != nil {
		return nil, err
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// INSERT OR IGNORE is deliberately the first statement: concurrent
	// processes cannot hold a stale read snapshot while upgrading to a writer.
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO daemon_authority_key(singleton, hmac_key, created_at)
		VALUES (1, ?, ?)`, generated[:], store.Now()); err != nil {
		return nil, err
	}
	var key []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT hmac_key FROM daemon_authority_key WHERE singleton=1`,
	).Scan(&key); err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid daemon authority key length")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return append([]byte(nil), key...), nil
}

// NextMaterializationHolderGeneration allocates the next daemon holder
// generation transactionally. The durable counter prevents a daemon restart
// from reusing a live claim's generation.
func (js *Store) NextMaterializationHolderGeneration(ctx context.Context) (int64, error) {
	const maxGeneration int64 = 1<<53 - 1
	var generated [32]byte
	if _, err := rand.Read(generated[:]); err != nil {
		return 0, err
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO daemon_authority_key(singleton, hmac_key, created_at)
		VALUES (1, ?, ?)`, generated[:], store.Now()); err != nil {
		return 0, err
	}
	update, err := tx.ExecContext(ctx, `
		UPDATE daemon_authority_key SET holder_generation = holder_generation + 1
		WHERE singleton=1 AND holder_generation < ?`, maxGeneration)
	if err != nil {
		return 0, err
	}
	changed, err := update.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed != 1 {
		return 0, errors.New("daemon holder generation exhausted")
	}
	var generation int64
	if err := tx.QueryRowContext(ctx,
		`SELECT holder_generation FROM daemon_authority_key WHERE singleton=1`,
	).Scan(&generation); err != nil {
		return 0, err
	}
	if generation < 1 || generation > maxGeneration {
		return 0, errors.New("daemon holder generation exhausted")
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return generation, nil
}

// InstitutionProfileSpec is the daemon-computed authority for one configured
// institution. AuthenticationClaimID is opaque and is supplied by the daemon's
// identity reconciler; it is never derived from a provider/entity identifier.
type InstitutionProfileSpec struct {
	ConfiguredName        string
	AuthorityDigest       string
	AuthenticationClaimID string
}

// InstitutionProfile is the durable identity and revision of one configured
// institution profile. Tombstoned profiles are never reused.
type InstitutionProfile struct {
	ID                    string
	ConfiguredName        string
	Revision              int64
	AuthorityDigest       string
	AuthenticationClaimID string
	TombstonedAt          string
	CreatedAt             string
	UpdatedAt             string
}

// ReconcileInstitutionProfiles atomically projects the daemon's configured
// profile set. Existing active names retain their opaque IDs; authority changes
// increment their revision. Removed rows are tombstoned and therefore cannot be
// reused by a later profile with the same configured name.
func (js *Store) ReconcileInstitutionProfiles(ctx context.Context, specs []InstitutionProfileSpec) ([]InstitutionProfile, error) {
	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		if strings.TrimSpace(spec.ConfiguredName) == "" || strings.TrimSpace(spec.AuthorityDigest) == "" {
			return nil, errors.New("institution profile requires configured name and authority digest")
		}
		if _, ok := seen[spec.ConfiguredName]; ok {
			return nil, fmt.Errorf("duplicate institution profile %q", spec.ConfiguredName)
		}
		seen[spec.ConfiguredName] = struct{}{}
	}

	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := store.Now()
	rows, err := tx.QueryContext(ctx, `
		SELECT id, configured_name, revision, authority_digest,
		       authentication_claim_id, COALESCE(tombstoned_at,''), created_at, updated_at
		  FROM institution_profiles
		 WHERE tombstoned_at IS NULL`)
	if err != nil {
		return nil, err
	}
	active := make(map[string]InstitutionProfile)
	for rows.Next() {
		var p InstitutionProfile
		if err := rows.Scan(&p.ID, &p.ConfiguredName, &p.Revision, &p.AuthorityDigest,
			&p.AuthenticationClaimID, &p.TombstonedAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		active[p.ConfiguredName] = p
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	out := make([]InstitutionProfile, 0, len(specs))
	for _, spec := range specs {
		if old, ok := active[spec.ConfiguredName]; ok {
			claimID := spec.AuthenticationClaimID
			if claimID == "" {
				claimID = old.AuthenticationClaimID
			}
			if claimID == "" {
				claimID, err = opaqueMaterializationID("auth")
				if err != nil {
					return nil, err
				}
			}
			changed := old.AuthorityDigest != spec.AuthorityDigest || old.AuthenticationClaimID != claimID
			if changed {
				old.Revision++
				old.AuthorityDigest = spec.AuthorityDigest
				old.AuthenticationClaimID = claimID
				old.UpdatedAt = now
				if _, err := tx.ExecContext(ctx, `
					UPDATE institution_profiles
					   SET revision=?, authority_digest=?, authentication_claim_id=?, updated_at=?
					 WHERE id=? AND tombstoned_at IS NULL AND revision=?`,
					old.Revision, old.AuthorityDigest, old.AuthenticationClaimID, now, old.ID, old.Revision-1); err != nil {
					return nil, err
				}
			}
			out = append(out, old)
			delete(active, spec.ConfiguredName)
			continue
		}
		id, err := opaqueMaterializationID("profile")
		if err != nil {
			return nil, err
		}
		claimID := spec.AuthenticationClaimID
		if claimID == "" {
			claimID, err = opaqueMaterializationID("auth")
			if err != nil {
				return nil, err
			}
		}
		p := InstitutionProfile{ID: id, ConfiguredName: spec.ConfiguredName, Revision: 1,
			AuthorityDigest: spec.AuthorityDigest, AuthenticationClaimID: claimID,
			CreatedAt: now, UpdatedAt: now}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO institution_profiles
				(id, configured_name, revision, authority_digest, authentication_claim_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, p.ID, p.ConfiguredName, p.Revision,
			p.AuthorityDigest, p.AuthenticationClaimID, now, now); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	for _, old := range active {
		if _, err := tx.ExecContext(ctx, `
			UPDATE institution_profiles
			   SET tombstoned_at=?, updated_at=?
			 WHERE id=? AND tombstoned_at IS NULL`, now, now, old.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// GetInstitutionProfile loads a profile by opaque ID. Tombstoned rows remain
// readable for audit and stale-fence diagnostics.
func (js *Store) GetInstitutionProfile(ctx context.Context, id string) (*InstitutionProfile, error) {
	var p InstitutionProfile
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT id, configured_name, revision, authority_digest,
		       authentication_claim_id, COALESCE(tombstoned_at,''), created_at, updated_at
		  FROM institution_profiles WHERE id=?`, id).Scan(
		&p.ID, &p.ConfiguredName, &p.Revision, &p.AuthorityDigest,
		&p.AuthenticationClaimID, &p.TombstonedAt, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// InstitutionProfileByConfiguredName returns the current active profile with
// the exact configured name. Tombstoned profiles are intentionally invisible
// to operational callers.
func (js *Store) InstitutionProfileByConfiguredName(ctx context.Context, configuredName string) (*InstitutionProfile, error) {
	var p InstitutionProfile
	err := js.S.DB().QueryRowContext(ctx, `
		SELECT id, configured_name, revision, authority_digest,
		       authentication_claim_id, COALESCE(tombstoned_at,''), created_at, updated_at
		  FROM institution_profiles
		 WHERE configured_name=? AND tombstoned_at IS NULL`, configuredName).Scan(
		&p.ID, &p.ConfiguredName, &p.Revision, &p.AuthorityDigest,
		&p.AuthenticationClaimID, &p.TombstonedAt, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListInstitutionProfiles returns active profiles unless includeTombstoned is
// true. Results are deterministic by configured name then opaque ID.
func (js *Store) ListInstitutionProfiles(ctx context.Context, includeTombstoned bool) ([]InstitutionProfile, error) {
	q := `SELECT id, configured_name, revision, authority_digest, authentication_claim_id,
	             COALESCE(tombstoned_at,''), created_at, updated_at
	        FROM institution_profiles`
	if !includeTombstoned {
		q += ` WHERE tombstoned_at IS NULL`
	}
	q += ` ORDER BY configured_name, id`
	rows, err := js.S.DB().QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstitutionProfile
	for rows.Next() {
		var p InstitutionProfile
		if err := rows.Scan(&p.ID, &p.ConfiguredName, &p.Revision, &p.AuthorityDigest,
			&p.AuthenticationClaimID, &p.TombstonedAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BrowserCandidate is an URL-free, authority-fenced institutional candidate.
type BrowserCandidate struct {
	ID                         string
	JobID                      string
	JobAttemptRevision         int64
	InstitutionProfileID       string
	InstitutionProfileRevision int64
	RouteRevision              int64
	RouteClass                 string
	IdentifierStrategy         string
	PreRouteSafetyKey          string
	SafetyDomainID             string
	AdapterRevision            string
	EffectContractID           string
	Status                     string
	CreatedAt                  string
	UpdatedAt                  string
}

// BrowserCandidateInput supplies immutable authority fields for a candidate.
type BrowserCandidateInput struct {
	ID                         string
	JobID                      string
	JobAttemptRevision         int64
	InstitutionProfileID       string
	InstitutionProfileRevision int64
	RouteRevision              int64
	RouteClass                 string
	IdentifierStrategy         string
	PreRouteSafetyKey          string
	SafetyDomainID             string
	AdapterRevision            string
	EffectContractID           string
	Status                     string
}

func (js *Store) CreateBrowserCandidate(ctx context.Context, in BrowserCandidateInput) (*BrowserCandidate, error) {
	if strings.TrimSpace(in.JobID) == "" || strings.TrimSpace(in.InstitutionProfileID) == "" {
		return nil, errors.New("browser candidate requires job and institution profile")
	}
	if in.ID == "" {
		var err error
		in.ID, err = opaqueMaterializationID("candidate")
		if err != nil {
			return nil, err
		}
	}
	if in.Status == "" {
		in.Status = "eligible"
	}
	now := store.Now()
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM institution_profiles WHERE id=? AND tombstoned_at IS NULL AND revision=?`, in.InstitutionProfileID, in.InstitutionProfileRevision).Scan(&active); err != nil {
		return nil, err
	}
	if active != 1 {
		return nil, ErrMaterializationStale
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO browser_candidates
			(id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
			 route_revision, route_class, identifier_strategy, pre_route_safety_key, safety_domain_id,
			 adapter_revision, effect_contract_id, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ID, in.JobID, in.JobAttemptRevision, in.InstitutionProfileID, in.InstitutionProfileRevision,
		in.RouteRevision, in.RouteClass, in.IdentifierStrategy, in.PreRouteSafetyKey, in.SafetyDomainID,
		in.AdapterRevision, in.EffectContractID, in.Status, now, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &BrowserCandidate{ID: in.ID, JobID: in.JobID, JobAttemptRevision: in.JobAttemptRevision,
		InstitutionProfileID: in.InstitutionProfileID, InstitutionProfileRevision: in.InstitutionProfileRevision,
		RouteRevision: in.RouteRevision, RouteClass: in.RouteClass, IdentifierStrategy: in.IdentifierStrategy,
		PreRouteSafetyKey: in.PreRouteSafetyKey, SafetyDomainID: in.SafetyDomainID, AdapterRevision: in.AdapterRevision,
		EffectContractID: in.EffectContractID, Status: in.Status, CreatedAt: now, UpdatedAt: now}, nil
}

func scanBrowserCandidate(s interface{ Scan(...any) error }) (*BrowserCandidate, error) {
	var c BrowserCandidate
	if err := s.Scan(&c.ID, &c.JobID, &c.JobAttemptRevision, &c.InstitutionProfileID, &c.InstitutionProfileRevision,
		&c.RouteRevision, &c.RouteClass, &c.IdentifierStrategy, &c.PreRouteSafetyKey, &c.SafetyDomainID,
		&c.AdapterRevision, &c.EffectContractID, &c.Status, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

func browserCandidateSelect() string {
	return `SELECT id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
	               route_revision, route_class, identifier_strategy, pre_route_safety_key, safety_domain_id,
	               adapter_revision, effect_contract_id, status, created_at, updated_at FROM browser_candidates`
}

func (js *Store) GetBrowserCandidate(ctx context.Context, id string) (*BrowserCandidate, error) {
	c, err := scanBrowserCandidate(js.S.DB().QueryRowContext(ctx, browserCandidateSelect()+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// EligibleBrowserCandidateForJob returns the oldest current eligible
// institutional candidate for the specified job attempt.
func (js *Store) EligibleBrowserCandidateForJob(ctx context.Context, jobID string, attemptRevision int64) (*BrowserCandidate, error) {
	if strings.TrimSpace(jobID) == "" || attemptRevision < 1 {
		return nil, nil
	}
	c, err := scanBrowserCandidate(js.S.DB().QueryRowContext(ctx, `SELECT browser_candidates.id, browser_candidates.job_id,
		browser_candidates.job_attempt_revision, browser_candidates.institution_profile_id,
		browser_candidates.institution_profile_revision, browser_candidates.route_revision,
		browser_candidates.route_class, browser_candidates.identifier_strategy,
		browser_candidates.pre_route_safety_key, browser_candidates.safety_domain_id,
		browser_candidates.adapter_revision, browser_candidates.effect_contract_id,
		browser_candidates.status, browser_candidates.created_at, browser_candidates.updated_at
		FROM browser_candidates
		JOIN institution_profiles p ON p.id=browser_candidates.institution_profile_id
		WHERE browser_candidates.job_id=? AND browser_candidates.job_attempt_revision=?
		  AND browser_candidates.status='eligible'
		  AND p.tombstoned_at IS NULL
		  AND p.revision=browser_candidates.institution_profile_revision
		ORDER BY browser_candidates.created_at, browser_candidates.id LIMIT 1`,
		jobID, attemptRevision))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// CurrentBrowserCandidateForJob returns the oldest nonterminal current
// candidate, including one already claimed by an in-flight materialization.
func (js *Store) CurrentBrowserCandidateForJob(ctx context.Context, jobID string, attemptRevision int64) (*BrowserCandidate, error) {
	if strings.TrimSpace(jobID) == "" || attemptRevision < 1 {
		return nil, nil
	}
	c, err := scanBrowserCandidate(js.S.DB().QueryRowContext(ctx, `SELECT browser_candidates.id, browser_candidates.job_id,
		browser_candidates.job_attempt_revision, browser_candidates.institution_profile_id,
		browser_candidates.institution_profile_revision, browser_candidates.route_revision,
		browser_candidates.route_class, browser_candidates.identifier_strategy,
		browser_candidates.pre_route_safety_key, browser_candidates.safety_domain_id,
		browser_candidates.adapter_revision, browser_candidates.effect_contract_id,
		browser_candidates.status, browser_candidates.created_at, browser_candidates.updated_at
		FROM browser_candidates
		JOIN institution_profiles p ON p.id=browser_candidates.institution_profile_id
		WHERE browser_candidates.job_id=? AND browser_candidates.job_attempt_revision=?
		  AND browser_candidates.status IN ('eligible','claimed','materializing')
		  AND p.tombstoned_at IS NULL
		  AND p.revision=browser_candidates.institution_profile_revision
		ORDER BY browser_candidates.created_at, browser_candidates.id LIMIT 1`,
		jobID, attemptRevision))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// MaterializationAttemptRevision returns the explicit retry decision epoch
// for a job. A retry_requested event starts the next materialization attempt;
// ordinary transitions do not.
func (js *Store) MaterializationAttemptRevision(ctx context.Context, jobID string) (int64, error) {
	if strings.TrimSpace(jobID) == "" {
		return 0, nil
	}
	var retries int64
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE job_id=? AND kind='job.retry_requested'`,
		jobID).Scan(&retries); err != nil {
		return 0, err
	}
	return retries + 1, nil
}

// SetBrowserCandidateStatus performs a status CAS; immutable authority columns
// are never included in this update.
func (js *Store) SetBrowserCandidateStatus(ctx context.Context, id, expectedStatus, nextStatus string) error {
	now := store.Now()
	res, err := js.S.DB().ExecContext(ctx, `UPDATE browser_candidates SET status=?, updated_at=? WHERE id=? AND status=?`, nextStatus, now, id, expectedStatus)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrMaterializationConflict
	}
	return nil
}

// MaterializationClaim is a fenced daemon claim. Routes are intentionally not
// represented; only their issuance ordinal is durable.
type MaterializationClaim struct {
	ID                      string
	CandidateID             string
	BrowserHolderGeneration int64
	MaterializationKind     string
	BindingID               string
	TabID                   int64
	Phase                   string
	RouteIssuanceOrdinal    int64
	EffectOrdinal           int64
	LeaseUntil              string
	CreatedAt               string
	UpdatedAt               string
}

type MaterializationClaimInput struct {
	CandidateID                string
	BrowserHolderGeneration    int64
	JobAttemptRevision         int64
	InstitutionProfileRevision int64
	RouteRevision              int64
	MaterializationKind        string
	BindingID                  string
	LeaseUntil                 time.Time
}

func claimScan(s interface{ Scan(...any) error }) (*MaterializationClaim, error) {
	var c MaterializationClaim
	if err := s.Scan(&c.ID, &c.CandidateID, &c.BrowserHolderGeneration, &c.MaterializationKind,
		&c.BindingID, &c.TabID, &c.Phase, &c.RouteIssuanceOrdinal, &c.EffectOrdinal, &c.LeaseUntil, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

const claimSelect = `SELECT id, candidate_id, browser_holder_generation, materialization_kind,
	COALESCE(binding_id,''), tab_id, phase, route_issuance_ordinal, effect_ordinal,
	COALESCE(lease_until,''), created_at, updated_at FROM materialization_claims`

func (js *Store) ClaimMaterialization(ctx context.Context, in MaterializationClaimInput) (*MaterializationClaim, error) {
	if in.CandidateID == "" || in.MaterializationKind == "" || in.LeaseUntil.IsZero() {
		return nil, errors.New("materialization claim requires candidate, kind, and lease")
	}
	claimID, err := opaqueMaterializationID("claim")
	if err != nil {
		return nil, err
	}
	bindingID := in.BindingID
	if bindingID == "" {
		bindingID, err = opaqueMaterializationID("binding")
		if err != nil {
			return nil, err
		}
	}
	now := store.Now()
	nowTime := time.Now().UTC()
	if !in.LeaseUntil.After(nowTime) {
		return nil, errors.New("materialization lease must be in the future")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// The candidate's profile is an authority fence, not merely a historical
	// foreign key. A tombstoned profile or a revision drift invalidates the
	// candidate before any claim state is inspected or changed.
	var jobRev, profileRev, routeRev int64
	var candidateStatus string
	err = tx.QueryRowContext(ctx, `SELECT c.job_attempt_revision, c.institution_profile_revision,
		c.route_revision, c.status
		FROM browser_candidates c
		JOIN institution_profiles p ON p.id=c.institution_profile_id
		WHERE c.id=? AND p.tombstoned_at IS NULL
		  AND p.revision=c.institution_profile_revision
		  AND c.job_attempt_revision = 1 + (
			SELECT COUNT(*) FROM events e
			 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
		  )`, in.CandidateID).
		Scan(&jobRev, &profileRev, &routeRev, &candidateStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMaterializationStale
	}
	if err != nil {
		return nil, err
	}
	if jobRev != in.JobAttemptRevision || profileRev != in.InstitutionProfileRevision || routeRev != in.RouteRevision {
		return nil, ErrMaterializationStale
	}

	// Expired claims no longer occupy the one-live-claim slot. This is done in
	// the same transaction as insertion, so two workers cannot both win.
	if _, err := tx.ExecContext(ctx, `UPDATE materialization_claims SET phase='abandoned', updated_at=? WHERE candidate_id=? AND phase IN ('claimed','bound','route_issued','navigated') AND lease_until IS NOT NULL AND lease_until <= ?`, now, in.CandidateID, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE browser_candidates SET status='eligible', updated_at=? WHERE id=? AND status='claimed' AND NOT EXISTS (SELECT 1 FROM materialization_claims WHERE candidate_id=? AND phase IN ('claimed','bound','route_issued','navigated') AND (lease_until IS NULL OR lease_until > ?))`, now, in.CandidateID, in.CandidateID, now); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT status FROM browser_candidates WHERE id=?`, in.CandidateID).Scan(&candidateStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrMaterializationStale
		}
		return nil, err
	}

	// A caller may have lost the response after the insert committed. Return
	// that exact live claim (including its durable binding) instead of making a
	// retry look busy. Authority and revision fences above make this safe.
	existing, err := claimScan(tx.QueryRowContext(ctx, claimSelect+` WHERE candidate_id=?
		AND phase IN ('claimed','bound','route_issued','navigated')
		AND (lease_until IS NULL OR lease_until > ?) ORDER BY id LIMIT 1`, in.CandidateID, now))
	if err == nil {
		if existing.BrowserHolderGeneration == in.BrowserHolderGeneration &&
			existing.MaterializationKind == in.MaterializationKind {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return existing, nil
		}
		return nil, ErrMaterializationBusy
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	if candidateStatus != "eligible" {
		if candidateStatus == "claimed" || candidateStatus == "materializing" {
			return nil, ErrMaterializationBusy
		}
		return nil, ErrMaterializationConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO materialization_claims
		(id, candidate_id, browser_holder_generation, materialization_kind, binding_id, tab_id, phase,
		 route_issuance_ordinal, effect_ordinal, lease_until, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, 'claimed', 0, 0, ?, ?, ?)`, claimID, in.CandidateID,
		in.BrowserHolderGeneration, in.MaterializationKind, bindingID, in.LeaseUntil.UTC().Format(time.RFC3339Nano), now, now); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrMaterializationBusy
		}
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE browser_candidates SET status='claimed', updated_at=? WHERE id=? AND status='eligible'`, now, in.CandidateID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return js.GetMaterializationClaim(ctx, claimID)
}

func (js *Store) GetMaterializationClaim(ctx context.Context, id string) (*MaterializationClaim, error) {
	c, err := claimScan(js.S.DB().QueryRowContext(ctx, claimSelect+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// MaterializationClaimByBindingID resolves the daemon claim owning a physical
// scaffold binding. Binding IDs are unique across claim history.
func (js *Store) MaterializationClaimByBindingID(ctx context.Context, bindingID string) (*MaterializationClaim, error) {
	if strings.TrimSpace(bindingID) == "" {
		return nil, nil
	}
	c, err := claimScan(js.S.DB().QueryRowContext(ctx, claimSelect+` WHERE binding_id=?`, bindingID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// LiveMaterializationClaimForJob resolves the claim currently materializing
// the job's exact attempt, fenced to the holder generation that owns it and
// to a profile revision that is still live. Delivery callbacks use it to bind
// arriving bytes to the browser effect that was actually authorized; a job
// with no institutional claim returns nil and keeps the legacy delivery path.
func (js *Store) LiveMaterializationClaimForJob(ctx context.Context, jobID string, jobAttemptRevision, holderGeneration int64) (*MaterializationClaim, *BrowserCandidate, error) {
	if strings.TrimSpace(jobID) == "" || jobAttemptRevision < 1 || holderGeneration < 0 {
		return nil, nil, nil
	}
	var claimID string
	err := js.S.DB().QueryRowContext(ctx, `SELECT m.id
		FROM materialization_claims m
		JOIN browser_candidates c ON c.id = m.candidate_id
		JOIN institution_profiles p ON p.id = c.institution_profile_id
		WHERE c.job_id = ? AND c.job_attempt_revision = ?
		  AND m.browser_holder_generation = ?
		  AND m.phase IN ('claimed','bound','route_issued','navigated')
		  AND p.tombstoned_at IS NULL
		  AND p.revision = c.institution_profile_revision
		ORDER BY m.id LIMIT 1`, jobID, jobAttemptRevision, holderGeneration).Scan(&claimID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	claim, err := js.GetMaterializationClaim(ctx, claimID)
	if err != nil || claim == nil {
		return nil, nil, err
	}
	candidate, err := js.GetBrowserCandidate(ctx, claim.CandidateID)
	if err != nil || candidate == nil {
		return nil, nil, err
	}
	return claim, candidate, nil
}

// BindMaterialization acknowledges the physical resource for a claim. Binding
// IDs are minted at claim creation. A live claim may replace its tab while
// bound or route_issued; navigated and settled tab fences are immutable.
func (js *Store) BindMaterialization(ctx context.Context, claimID, bindingID string, holderGeneration, profileRevision, tabID int64) error {
	if claimID == "" || bindingID == "" || tabID < 0 {
		return errors.New("materialization binding requires claim, binding ID, and nonnegative tab ID")
	}
	now := store.Now()
	res, err := js.S.DB().ExecContext(ctx, `UPDATE materialization_claims
		SET phase='bound', tab_id=?, updated_at=?
		WHERE id=? AND binding_id=? AND browser_holder_generation=? AND phase='claimed'
		  AND (lease_until IS NULL OR lease_until > ?)
		  AND EXISTS (
			SELECT 1 FROM browser_candidates c
			JOIN institution_profiles p ON p.id=c.institution_profile_id
			WHERE c.id=materialization_claims.candidate_id
			  AND c.institution_profile_revision=?
			  AND p.tombstoned_at IS NULL
			  AND p.revision=c.institution_profile_revision
			  AND c.job_attempt_revision = 1 + (
				SELECT COUNT(*) FROM events e
				 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
			  )
		  )`,
		tabID, now, claimID, bindingID, holderGeneration, now, profileRevision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}

	// A live scaffold can disappear before navigation. Allow the same
	// holder/profile to rebind a bound or route-issued claim to its replacement
	// tab without changing its route phase or issuance ordinal.
	res, err = js.S.DB().ExecContext(ctx, `UPDATE materialization_claims
		SET tab_id=?, updated_at=?
		WHERE id=? AND binding_id=? AND browser_holder_generation=? AND phase IN ('bound','route_issued')
		  AND tab_id<>?
		  AND (lease_until IS NULL OR lease_until > ?)
		  AND EXISTS (
			SELECT 1 FROM browser_candidates c
			JOIN institution_profiles p ON p.id=c.institution_profile_id
			WHERE c.id=materialization_claims.candidate_id
			  AND c.institution_profile_revision=?
			  AND p.tombstoned_at IS NULL
			  AND p.revision=c.institution_profile_revision
			  AND c.job_attempt_revision = 1 + (
				SELECT COUNT(*) FROM events e
				 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
			  )
		  )`,
		tabID, now, claimID, bindingID, holderGeneration, tabID, now, profileRevision)
	if err != nil {
		return err
	}
	if n, err = res.RowsAffected(); err != nil {
		return err
	} else if n == 1 {
		return nil
	}

	var present int
	err = js.S.DB().QueryRowContext(ctx, `SELECT 1
		FROM materialization_claims m
		JOIN browser_candidates c ON c.id=m.candidate_id
		JOIN institution_profiles p ON p.id=c.institution_profile_id
		WHERE m.id=? AND m.binding_id=? AND m.browser_holder_generation=? AND m.phase IN ('bound','route_issued')
		  AND m.tab_id=? AND c.institution_profile_revision=? AND p.tombstoned_at IS NULL
		  AND p.revision=c.institution_profile_revision
		  AND (m.lease_until IS NULL OR m.lease_until > ?)`,
		claimID, bindingID, holderGeneration, tabID, profileRevision, now).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMaterializationStale
	}
	if err != nil {
		return err
	}
	return nil
}

// IssueMaterializationRoute advances the route issuance ordinal for a newly
// bound claim and replays the existing ordinal for an already issued or
// navigated claim. In both cases the candidate's snapshotted institution
// profile revision must still be the active revision of the same profile.
// Routes are intentionally not represented; only their issuance ordinal is
// durable. expectedOrdinal is the last ordinal observed by the caller; the
// returned value is the newly issued or replayed ordinal.
func (js *Store) IssueMaterializationRoute(ctx context.Context, claimID, bindingID string, holderGeneration, expectedOrdinal int64) (int64, error) {
	now := store.Now()
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `UPDATE materialization_claims
		SET phase='route_issued', route_issuance_ordinal=route_issuance_ordinal+1, updated_at=?
		WHERE id=? AND binding_id=? AND browser_holder_generation=?
		  AND phase='bound' AND route_issuance_ordinal=?
		  AND (lease_until IS NULL OR lease_until > ?)
		  AND EXISTS (
			SELECT 1 FROM browser_candidates c
			JOIN institution_profiles p ON p.id=c.institution_profile_id
			WHERE c.id=materialization_claims.candidate_id
			  AND p.tombstoned_at IS NULL
			  AND p.revision=c.institution_profile_revision
			  AND c.job_attempt_revision = 1 + (
				SELECT COUNT(*) FROM events e
				 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
			  )
		  )`, now, claimID, bindingID, holderGeneration, expectedOrdinal, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 1 {
		var ordinal int64
		if err := tx.QueryRowContext(ctx, `SELECT route_issuance_ordinal FROM materialization_claims WHERE id=?`, claimID).Scan(&ordinal); err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return ordinal, nil
	}

	// A response may have been lost after route issuance committed. Replay is
	// permitted only for the exact current ordinal and the same active profile
	// revision; a drift or tombstone therefore cannot leak the old route.
	var ordinal int64
	err = tx.QueryRowContext(ctx, `SELECT m.route_issuance_ordinal
		FROM materialization_claims m
		JOIN browser_candidates c ON c.id=m.candidate_id
		JOIN institution_profiles p ON p.id=c.institution_profile_id
		WHERE m.id=? AND m.binding_id=? AND m.browser_holder_generation=?
		  AND m.phase IN ('route_issued','navigated')
		  AND m.route_issuance_ordinal=?
		  AND (m.lease_until IS NULL OR m.lease_until > ?)
		  AND p.tombstoned_at IS NULL
		  AND p.revision=c.institution_profile_revision
		  AND c.job_attempt_revision = 1 + (
			SELECT COUNT(*) FROM events e
			 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
		  )`, claimID, bindingID,
		holderGeneration, expectedOrdinal, now).Scan(&ordinal)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrMaterializationStale
	}
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return ordinal, nil
}

func (js *Store) AcknowledgeMaterializationNavigation(ctx context.Context, claimID, bindingID string, holderGeneration, routeOrdinal, tabID int64) error {
	if tabID < 0 {
		return ErrMaterializationStale
	}
	now := store.Now()
	res, err := js.S.DB().ExecContext(ctx, `UPDATE materialization_claims SET phase='navigated', updated_at=?
		WHERE id=? AND binding_id=? AND browser_holder_generation=? AND route_issuance_ordinal=?
		  AND tab_id=? AND phase IN ('route_issued','navigated') AND (lease_until IS NULL OR lease_until > ?)
		  AND EXISTS (
			SELECT 1 FROM browser_candidates c
			JOIN institution_profiles p ON p.id=c.institution_profile_id
			WHERE c.id=materialization_claims.candidate_id
			  AND p.tombstoned_at IS NULL
			  AND p.revision=c.institution_profile_revision
			  AND c.job_attempt_revision = 1 + (
				SELECT COUNT(*) FROM events e
				 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
			  )
		  )`, now, claimID, bindingID, holderGeneration, routeOrdinal, tabID, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrMaterializationStale
	}
	return nil
}

// SettleMaterialization closes a materialization only after the matching
// artifact winner has been durably recorded. Replaying the same fenced,
// already-settled claim is idempotent.
func (js *Store) SettleMaterialization(ctx context.Context, claimID, bindingID string, holderGeneration, profileRevision int64) error {
	now := store.Now()
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var candidateID, jobID string
	var jobAttemptRevision int64
	err = tx.QueryRowContext(ctx, `SELECT c.id, c.job_id, c.job_attempt_revision
		FROM materialization_claims m
		JOIN browser_candidates c ON c.id=m.candidate_id
		JOIN institution_profiles p ON p.id=c.institution_profile_id
		WHERE m.id=? AND m.binding_id=? AND m.browser_holder_generation=?
		  AND m.phase IN ('navigated','settled')
		  AND (m.phase='settled' OR m.lease_until IS NULL OR m.lease_until > ?)
		  AND c.institution_profile_revision=?
		  AND p.tombstoned_at IS NULL
		  AND p.revision=c.institution_profile_revision
		  AND c.job_attempt_revision = 1 + (
			SELECT COUNT(*) FROM events e
			 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
		  )`, claimID, bindingID, holderGeneration, now, profileRevision).
		Scan(&candidateID, &jobID, &jobAttemptRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMaterializationStale
	}
	if err != nil {
		return err
	}
	var winnerCandidate string
	var winnerAttempt, winnerGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT candidate_id, job_attempt_revision,
		browser_holder_generation FROM artifact_winners WHERE job_id=?`, jobID).
		Scan(&winnerCandidate, &winnerAttempt, &winnerGeneration); errors.Is(err, sql.ErrNoRows) {
		return ErrMaterializationConflict
	} else if err != nil {
		return err
	}
	if winnerCandidate != candidateID || winnerAttempt != jobAttemptRevision ||
		winnerGeneration != holderGeneration {
		return ErrMaterializationConflict
	}
	res, err := tx.ExecContext(ctx, `UPDATE materialization_claims SET phase='settled', updated_at=?
		WHERE id=? AND binding_id=? AND browser_holder_generation=? AND phase IN ('navigated','settled')`,
		now, claimID, bindingID, holderGeneration)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrMaterializationStale
	}
	res, err = tx.ExecContext(ctx, `UPDATE browser_candidates SET status='succeeded', updated_at=?
		WHERE id=? AND institution_profile_revision=?
		  AND EXISTS (
			SELECT 1 FROM institution_profiles p
			WHERE p.id=browser_candidates.institution_profile_id
			  AND p.tombstoned_at IS NULL
			  AND p.revision=browser_candidates.institution_profile_revision
		  )`, now, candidateID, profileRevision)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrMaterializationStale
	}
	return tx.Commit()
}

// RenewMaterializationClaim extends a live claim's lease only for its original
// holder generation. An expired or superseded claim cannot be revived.
func (js *Store) RenewMaterializationClaim(ctx context.Context, claimID string, holderGeneration int64, leaseUntil time.Time) error {
	if !leaseUntil.After(time.Now().UTC()) {
		return errors.New("materialization lease must be in the future")
	}
	res, err := js.S.DB().ExecContext(ctx, `UPDATE materialization_claims SET lease_until=?, updated_at=?
		WHERE id=? AND browser_holder_generation=? AND phase IN ('claimed','bound','route_issued','navigated')
		  AND (lease_until IS NULL OR lease_until > ?)
		  AND EXISTS (
			SELECT 1 FROM browser_candidates c
			JOIN institution_profiles p ON p.id=c.institution_profile_id
			WHERE c.id=materialization_claims.candidate_id
			  AND p.tombstoned_at IS NULL
			  AND p.revision=c.institution_profile_revision
			  AND c.job_attempt_revision = 1 + (
				SELECT COUNT(*) FROM events e
				 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
			  )
		  )`, leaseUntil.UTC().Format(time.RFC3339Nano),
		store.Now(), claimID, holderGeneration, store.Now())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrMaterializationStale
	}
	return nil
}

// AdvanceMaterializationEffect increments the effect ordinal under the same
// holder and binding fence used for navigation. The ordinal is the only
// durable effect permit identity; effect details remain transient.
func (js *Store) AdvanceMaterializationEffect(ctx context.Context, claimID, bindingID string, holderGeneration, expectedOrdinal int64) (int64, error) {
	now := store.Now()
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE materialization_claims
		SET effect_ordinal=effect_ordinal+1, updated_at=?
		WHERE id=? AND binding_id=? AND browser_holder_generation=? AND phase='navigated'
		  AND effect_ordinal=? AND (lease_until IS NULL OR lease_until > ?)
		  AND EXISTS (
			SELECT 1 FROM browser_candidates c
			JOIN institution_profiles p ON p.id=c.institution_profile_id
			WHERE c.id=materialization_claims.candidate_id
			  AND p.tombstoned_at IS NULL
			  AND p.revision=c.institution_profile_revision
			  AND c.job_attempt_revision = 1 + (
				SELECT COUNT(*) FROM events e
				 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
			  )
		  )`,
		now, claimID, bindingID, holderGeneration, expectedOrdinal, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrMaterializationStale
	}
	var ordinal int64
	if err := tx.QueryRowContext(ctx, `SELECT effect_ordinal FROM materialization_claims WHERE id=?`, claimID).Scan(&ordinal); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return ordinal, nil
}

// ReconcileMaterializationClaims fences expired claims in one transaction and
// returns the rows it retired. Unexpired claims, including those from an older
// holder generation, are never stolen by reconciliation.
func (js *Store) ReconcileMaterializationClaims(ctx context.Context, now time.Time) ([]MaterializationClaim, error) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, claimSelect+` WHERE phase IN ('claimed','bound','route_issued','navigated') AND lease_until IS NOT NULL AND lease_until <= ? ORDER BY id`, stamp)
	if err != nil {
		return nil, err
	}
	var expired []MaterializationClaim
	for rows.Next() {
		var c MaterializationClaim
		if err := rows.Scan(&c.ID, &c.CandidateID, &c.BrowserHolderGeneration, &c.MaterializationKind,
			&c.BindingID, &c.TabID, &c.Phase, &c.RouteIssuanceOrdinal, &c.EffectOrdinal, &c.LeaseUntil, &c.CreatedAt, &c.UpdatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		expired = append(expired, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if _, err := tx.ExecContext(ctx, `UPDATE materialization_claims SET phase='abandoned', updated_at=? WHERE phase IN ('claimed','bound','route_issued','navigated') AND lease_until IS NOT NULL AND lease_until <= ?`, stamp, stamp); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE browser_candidates SET status='eligible', updated_at=?
		WHERE status IN ('claimed','materializing')
		  AND NOT EXISTS (SELECT 1 FROM materialization_claims WHERE candidate_id=browser_candidates.id
		    AND phase IN ('claimed','bound','route_issued','navigated')
		    AND (lease_until IS NULL OR lease_until > ?))`, stamp, stamp); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return expired, nil
}

// AbandonStaleMaterializations transactionally fences every live claim issued
// by an older browser holder generation. Claims from the current generation
// remain untouched. Candidates are made eligible again only when no other live
// claim still owns them; terminal candidate states are never rewritten.
func (js *Store) AbandonStaleMaterializations(ctx context.Context, currentGeneration int64) (int64, error) {
	now := store.Now()
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `UPDATE materialization_claims
		SET phase='abandoned', updated_at=?
		WHERE browser_holder_generation<>?
		  AND phase IN ('claimed','bound','route_issued','navigated')
		  AND (lease_until IS NULL OR lease_until > ?)`,
		now, currentGeneration, now)
	if err != nil {
		return 0, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE browser_candidates
		SET status='eligible', updated_at=?
		WHERE status IN ('claimed','materializing')
		  AND NOT EXISTS (
			SELECT 1 FROM materialization_claims
			WHERE candidate_id=browser_candidates.id
			  AND phase IN ('claimed','bound','route_issued','navigated')
			  AND (lease_until IS NULL OR lease_until > ?)
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM artifact_winners
			WHERE candidate_id=browser_candidates.id
			  AND job_attempt_revision=browser_candidates.job_attempt_revision
		  )`, now, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func opaqueMaterializationID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(raw[:]), nil
}
