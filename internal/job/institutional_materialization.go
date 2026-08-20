package job

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	defer func() { _ = tx.Rollback() }()

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
	defer func() { _ = tx.Rollback() }()
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
	defer func() { _ = tx.Rollback() }()
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
	return getInstitutionProfileTx(ctx, js.S.DB(), id)
}

func getInstitutionProfileTx(ctx context.Context, q dbtx, id string) (*InstitutionProfile, error) {
	var p InstitutionProfile
	err := q.QueryRowContext(ctx, `
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
	defer func() { _ = tx.Rollback() }()
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
	return getBrowserCandidateTx(ctx, js.S.DB(), id)
}

func getBrowserCandidateTx(ctx context.Context, q dbtx, id string) (*BrowserCandidate, error) {
	c, err := scanBrowserCandidate(q.QueryRowContext(ctx, browserCandidateSelect()+` WHERE id=?`, id))
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

// TerminalMaterializationJobIDs returns terminal jobs which still own a live
// browser materialization claim. Bridge poll uses this durable set to emit
// cancel after a daemon restart: the in-memory offered/materializationTracked
// maps are gone, but the extension may still hold the tab and its browser-local
// job state.
func (js *Store) TerminalMaterializationJobIDs(ctx context.Context) ([]string, error) {
	rows, err := js.S.DB().QueryContext(ctx, `
		SELECT DISTINCT c.job_id
		  FROM materialization_claims m
		  JOIN browser_candidates c ON c.id=m.candidate_id
		  JOIN jobs j ON j.id=c.job_id
		 WHERE j.state IN ('ready','imported','unavailable','failed','cancelled')
		   AND m.phase IN ('claimed','bound','route_issued','navigated')
		 ORDER BY c.job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
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

// StartNextMaterializationAttemptForSpentCandidate records the explicit retry
// decision for a job whose CURRENT attempt is provably finished and
// undelivered, so the next candidate can mint. It exists because the
// one-navigation-per-attempt invariant above has an escape hatch nothing could
// reach.
//
// A candidate whose claim navigated keeps the candidate owned until an artifact
// winner closes it (ReconcileMaterializationClaims' comment states this
// deliberately, and TestSettledInstitutionalPermitKeepsExpiredClaimOwnedUntilWinner
// pins it) - the guard against repeating an irreversible provider navigation.
// The other way out is a new attempt, since MaterializationAttemptRevision
// counts job.retry_requested events. But Retry only accepts retry_wait/failed/
// unavailable and RepairAwaitingHuman requires no open actions, so a paper
// parked awaiting a sign-in that never completed could reach neither: measured
// live 2026-08-20, three papers pinned by navigated claims from dead holder
// generations, re-offered every poll and answered 'busy' about once a second,
// with `papio actions open` unable to produce a surface ever again.
//
// The operator asking again IS the retry decision, so the explicit-open path
// records it here rather than inventing a second retirement rule for spent
// claims. Every fence stays: an unsettled permit (an effect possibly in
// flight), a live claim on a fresh attempt, or an artifact winner all make this
// a no-op, and the decision lands in the event log where an attempt bump
// belongs.
func (js *Store) StartNextMaterializationAttemptForSpentCandidate(ctx context.Context, jobID string) (bool, error) {
	if strings.TrimSpace(jobID) == "" {
		return false, nil
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	var attempt int64
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 + COUNT(*) FROM events WHERE job_id=? AND kind='job.retry_requested'`,
		jobID).Scan(&attempt); err != nil {
		return false, err
	}
	spent, err := spentMaterializationCandidateTx(ctx, tx, jobID, attempt, now)
	if err != nil {
		return false, err
	}
	if !spent {
		return false, tx.Commit()
	}
	detail, _ := json.Marshal(map[string]any{"reason": "explicit_open_spent_attempt", "attempt": attempt})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, 'job.retry_requested', ?)`,
		jobID, now, string(detail)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SpentMaterializationCandidate reports whether a job's current attempt has a
// candidate that is owned but finished: its claim navigated (or issued a route)
// with no unsettled effect, its diagnostic lease is over, and no artifact
// winner ever closed it. Nothing the extension can be offered will advance such
// a candidate - a claim request against it can only answer busy - so the offer
// loop stops offering it, and only an explicit operator ask starts the next
// attempt.
func (js *Store) SpentMaterializationCandidate(ctx context.Context, jobID string) (bool, error) {
	if strings.TrimSpace(jobID) == "" {
		return false, nil
	}
	attempt, err := js.MaterializationAttemptRevision(ctx, jobID)
	if err != nil {
		return false, err
	}
	return spentMaterializationCandidateTx(ctx, js.S.DB(), jobID, attempt, store.Now())
}

func spentMaterializationCandidateTx(ctx context.Context, q dbtx, jobID string, attempt int64, now string) (bool, error) {
	var spent int
	// The subquery mirrors CurrentBrowserCandidateForJob's selection exactly,
	// because the question is whether the candidate the offer loop WOULD hand
	// out is spent. Asking it of any candidate on the attempt is wrong: a
	// second row for the same attempt (a legacy or seeded one) would let a
	// finished sibling suppress a live candidate's offers.
	err := q.QueryRowContext(ctx, `SELECT 1 FROM (
		SELECT c.id AS candidate_id, c.status AS status, c.job_attempt_revision AS attempt
		  FROM browser_candidates c
		  JOIN institution_profiles p ON p.id=c.institution_profile_id
		 WHERE c.job_id=? AND c.job_attempt_revision=?
		   AND c.status IN ('eligible','claimed','materializing')
		   AND p.tombstoned_at IS NULL
		   AND p.revision=c.institution_profile_revision
		 ORDER BY c.created_at, c.id LIMIT 1) cur
		WHERE cur.status IN ('claimed','materializing')
		  AND EXISTS (SELECT 1 FROM materialization_claims m
		    WHERE m.candidate_id=cur.candidate_id
		      AND m.phase IN ('route_issued','navigated','settled','abandoned')
		      AND m.lease_until IS NOT NULL AND m.lease_until <= ?
		      AND NOT EXISTS (SELECT 1 FROM effect_permits p
		        WHERE p.claim_id=m.id AND p.status IN ('held','unknown_completion')))
		  AND NOT EXISTS (SELECT 1 FROM materialization_claims live
		    WHERE live.candidate_id=cur.candidate_id
		      AND live.phase IN ('claimed','bound','route_issued','navigated')
		      AND (live.lease_until IS NULL OR live.lease_until > ?))
		  AND NOT EXISTS (SELECT 1 FROM artifact_winners w
		    WHERE w.candidate_id=cur.candidate_id AND w.job_attempt_revision=cur.attempt)`,
		jobID, attempt, now, now).Scan(&spent)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
	defer func() { _ = tx.Rollback() }()

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

	// Expired claims no longer occupy the one-live-claim slot unless an
	// institutional effect was authorized for the claim. The durable permit
	// remains the at-most-once history after settlement; until an artifact
	// winner closes the claim, retiring it could mint a fresh claim and repeat
	// the provider navigation.
	var expiringBindingID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT binding_id FROM materialization_claims
		WHERE candidate_id=? AND phase IN ('claimed','bound','route_issued','navigated')
		  AND lease_until IS NOT NULL AND lease_until <= ?
		  AND NOT EXISTS (SELECT 1 FROM effect_permits p
		    WHERE p.claim_id=materialization_claims.id)`,
		in.CandidateID, now).Scan(&expiringBindingID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE materialization_claims SET phase='abandoned', updated_at=?
		WHERE candidate_id=? AND phase IN ('claimed','bound','route_issued','navigated')
		  AND lease_until IS NOT NULL AND lease_until <= ?
		  AND NOT EXISTS (SELECT 1 FROM effect_permits p
		    WHERE p.claim_id=materialization_claims.id)`,
		now, in.CandidateID, now); err != nil {
		return nil, err
	}
	if expiringBindingID.Valid {
		if err := consumeCloseAuthorizationsTx(ctx, tx, []string{expiringBindingID.String}, now); err != nil {
			return nil, err
		}
	}
	// The same artifact_winners anti-join the two sweeps carry. Without it a
	// winner-bearing candidate that reconciliation deliberately left parked in
	// 'claimed' is flipped back to 'eligible' by the very next claim request,
	// and a fresh claim — and therefore a fresh route issuance — mints the
	// second irreversible provider effect the anti-join exists to prevent.
	if _, err := tx.ExecContext(ctx, `UPDATE browser_candidates SET status='eligible', updated_at=?
		WHERE id=? AND status='claimed'
		  AND NOT EXISTS (SELECT 1 FROM materialization_claims
		    WHERE candidate_id=?
		      AND phase IN ('claimed','bound','route_issued','navigated')
		      AND ((lease_until IS NULL OR lease_until > ?)
		        OR EXISTS (SELECT 1 FROM effect_permits p
		          WHERE p.claim_id=materialization_claims.id)))
		  AND NOT EXISTS (SELECT 1 FROM artifact_winners
		    WHERE candidate_id=browser_candidates.id
		      AND job_attempt_revision=browser_candidates.job_attempt_revision)`,
		now, in.CandidateID, in.CandidateID, now); err != nil {
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
	return materializationClaimByBindingIDTx(ctx, js.S.DB(), bindingID)
}

func materializationClaimByBindingIDTx(ctx context.Context, q dbtx, bindingID string) (*MaterializationClaim, error) {
	if strings.TrimSpace(bindingID) == "" {
		return nil, nil
	}
	c, err := claimScan(q.QueryRowContext(ctx, claimSelect+` WHERE binding_id=?`, bindingID))
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

// CandidateForAttempt returns this job attempt's browser candidate regardless
// of whether a claim is live now, or nil when the attempt was never
// institutional. Delivery uses it for two things: to decide that an attempt
// owes an artifact winner at all, and to attribute that winner when the claim
// that produced the bytes has already expired or been abandoned. Treating a
// missing live claim as "legacy, no fence required" let late institutional
// bytes attach with nothing having won them.
//
// The most recently created candidate wins attribution when an attempt has
// several: later candidates supersede earlier ones within one attempt.
func (js *Store) CandidateForAttempt(ctx context.Context, jobID string, jobAttemptRevision int64) (*BrowserCandidate, error) {
	if strings.TrimSpace(jobID) == "" || jobAttemptRevision < 1 {
		return nil, nil
	}
	// Attribute to the candidate that actually drove, not the newest one. An
	// authority edit mid-handoff mints a second candidate for the same attempt
	// while the original's tab is still open, so recency alone would credit the
	// bytes to a candidate that never issued a route — and leave the real
	// producer outside the artifact_winners anti-join. Rank by how far each
	// candidate's claim got first, and fall back to recency only for candidates
	// that never claimed.
	var id string
	err := js.S.DB().QueryRowContext(ctx, `SELECT c.id FROM browser_candidates c
		LEFT JOIN materialization_claims m ON m.candidate_id = c.id
		WHERE c.job_id = ? AND c.job_attempt_revision = ?
		ORDER BY CASE m.phase
			WHEN 'settled' THEN 5 WHEN 'navigated' THEN 4 WHEN 'route_issued' THEN 3
			WHEN 'bound' THEN 2 WHEN 'claimed' THEN 1 ELSE 0 END DESC,
			c.created_at DESC, c.id DESC LIMIT 1`,
		jobID, jobAttemptRevision).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return js.GetBrowserCandidate(ctx, id)
}

// ReleaseUnconsumedMaterializationClaim gives a claim back when the daemon
// itself refuses to let that job open a sign-in surface. Claiming a candidate
// and arbitrating the institution's authentication entry are two round trips,
// and the claim comes first: it flips the candidate to 'claimed', which is
// exactly the state the scheduler treats as "someone is working on this". So a
// consult answered `park`/`focus_owner` left a claim with no surface — tab 0, no
// route, no effect — holding its candidate out of the scheduler's reach for the
// claim's full lease, with nothing to retire it and nothing to retry. Observed
// live 2026-08-19: claim_009d4edb minted 05:35:26Z for
// job_eb16f955653ac52f89355d19bd, park one second later, still 'claimed' with
// tab 0 half an hour on while its paper reported itself as waiting for the
// operator.
//
// Returns whether anything was released. Only an unconsumed claim qualifies
// (phase 'claimed', tab 0, both ordinals 0, no effect permit), so a claim that
// already produced a surface or an irreversible provider effect is untouched —
// and the candidate is returned to 'eligible' under the same artifact_winners
// anti-join every other release here carries, so a winner-bearing candidate is
// never re-armed for a second route issuance.
func (js *Store) ReleaseUnconsumedMaterializationClaim(ctx context.Context, candidateID string) (bool, error) {
	if strings.TrimSpace(candidateID) == "" {
		return false, errors.New("materialization claim release requires a candidate")
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	res, err := tx.ExecContext(ctx, `UPDATE materialization_claims SET phase='abandoned', updated_at=?
		WHERE candidate_id=? AND phase='claimed' AND tab_id=0
		  AND route_issuance_ordinal=0 AND effect_ordinal=0
		  AND NOT EXISTS (SELECT 1 FROM effect_permits p WHERE p.claim_id=materialization_claims.id)`,
		now, candidateID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE browser_candidates SET status='eligible', updated_at=?
		WHERE id=? AND status='claimed'
		  AND NOT EXISTS (SELECT 1 FROM materialization_claims
		    WHERE candidate_id=? AND phase IN ('claimed','bound','route_issued','navigated'))
		  AND NOT EXISTS (SELECT 1 FROM artifact_winners
		    WHERE candidate_id=browser_candidates.id
		      AND job_attempt_revision=browser_candidates.job_attempt_revision)`,
		now, candidateID, candidateID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// BindMaterialization acknowledges the physical resource for a claim. Binding
// IDs are minted at claim creation. A live claim may replace its tab while
// bound or route_issued; navigated and settled tab fences are immutable.
func (js *Store) BindMaterialization(ctx context.Context, claimID, bindingID string, holderGeneration, profileRevision, tabID int64) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := bindMaterializationTx(ctx, tx, claimID, bindingID, holderGeneration, profileRevision, tabID); err != nil {
		return err
	}
	return tx.Commit()
}

// BindMaterializationWithLeaseOwner is institutionalBind's atomic entry
// point (internal/browser/bridge.go): the physical-resource acknowledgement
// and the authentication-entry lease's owner-binding side channel
// (claim-observation-protocol.md §4.1) commit or roll back together. A
// fenced no-op on the lease side (expired/reassigned/wrong holder) fails the
// whole bind with ErrMaterializationStale — matching BindMaterialization's
// own stale outcome — instead of leaving a bound scaffold whose lease never
// recorded it, which would let a later navigate_existing/focus_owner arm
// while nothing durable names this surface as the owner. authenticationClaimID
// empty means the candidate's institution profile carries no claim (nothing
// to record); the bind proceeds as a plain BindMaterialization.
func (js *Store) BindMaterializationWithLeaseOwner(ctx context.Context, claimID, bindingID string, holderGeneration, profileRevision, tabID int64, authenticationClaimID, ownerJobID string) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := bindMaterializationTx(ctx, tx, claimID, bindingID, holderGeneration, profileRevision, tabID); err != nil {
		return err
	}
	if authenticationClaimID != "" {
		res, err := setAuthenticationEntryLeaseOwnerBindingTx(ctx, tx, authenticationClaimID, ownerJobID, holderGeneration, bindingID, tabID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			// Distinguish "no lease was ever reserved for this claim" (an
			// explicit focus-driven bind that never went through
			// authentication-claim arbitration — nothing to fence against,
			// a benign no-op exactly like the old best-effort side channel)
			// from "a lease row exists but did not fence-match" (expired,
			// reassigned, or a different holder generation/owner — a real
			// fence failure that must fail the whole bind rather than leave
			// a bound scaffold no lease names as owned).
			var exists int
			existsErr := tx.QueryRowContext(ctx,
				`SELECT 1 FROM authentication_entry_leases WHERE authentication_claim_id=?`,
				authenticationClaimID).Scan(&exists)
			if existsErr != nil && !errors.Is(existsErr, sql.ErrNoRows) {
				return existsErr
			}
			if existsErr == nil {
				return ErrMaterializationStale
			}
		}
	}
	return tx.Commit()
}

func bindMaterializationTx(ctx context.Context, q dbtx, claimID, bindingID string, holderGeneration, profileRevision, tabID int64) error {
	if claimID == "" || bindingID == "" || tabID < 0 {
		return errors.New("materialization binding requires claim, binding ID, and nonnegative tab ID")
	}
	now := store.Now()
	res, err := q.ExecContext(ctx, `UPDATE materialization_claims
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
	res, err = q.ExecContext(ctx, `UPDATE materialization_claims
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
	err = q.QueryRowContext(ctx, `SELECT 1
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
	defer func() { _ = tx.Rollback() }()

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
	defer func() { _ = tx.Rollback() }()
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
	if err := consumeCloseAuthorizationsTx(ctx, tx, []string{bindingID}, now); err != nil {
		return err
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

// ReconcileMaterializationClaims fences expired claims in one transaction and
// returns the rows it retired. Unexpired claims, including those from an older
// holder generation, are never stolen by reconciliation.
func (js *Store) ReconcileMaterializationClaims(ctx context.Context, now time.Time) ([]MaterializationClaim, error) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, claimSelect+` WHERE phase IN ('claimed','bound','route_issued','navigated')
		AND lease_until IS NOT NULL AND lease_until <= ?
		AND NOT EXISTS (SELECT 1 FROM effect_permits p WHERE p.claim_id=materialization_claims.id)
		ORDER BY id`, stamp)
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
	if _, err := tx.ExecContext(ctx, `UPDATE materialization_claims SET phase='abandoned', updated_at=?
		WHERE phase IN ('claimed','bound','route_issued','navigated')
		  AND lease_until IS NOT NULL AND lease_until <= ?
		  AND NOT EXISTS (SELECT 1 FROM effect_permits p WHERE p.claim_id=materialization_claims.id)`, stamp, stamp); err != nil {
		return nil, err
	}
	retiredBindings := make([]string, len(expired))
	for i, c := range expired {
		retiredBindings[i] = c.BindingID
	}
	if err := consumeCloseAuthorizationsTx(ctx, tx, retiredBindings, stamp); err != nil {
		return nil, err
	}
	// A retired binding's surface is gone; its institution must not stay held.
	if err := releaseAuthenticationEntryLeasesForBindingsTx(ctx, tx, retiredBindings, stamp); err != nil {
		return nil, err
	}
	// Claims protected by any authorized institutional effect remain live
	// after its permit settles and after their diagnostic lease expires. The
	// candidate stays owned until the artifact winner closes the claim.
	if _, err := tx.ExecContext(ctx, `UPDATE browser_candidates SET status='eligible', updated_at=?
		WHERE status IN ('claimed','materializing')
		  AND NOT EXISTS (SELECT 1 FROM materialization_claims WHERE candidate_id=browser_candidates.id
		    AND phase IN ('claimed','bound','route_issued','navigated'))
		  AND NOT EXISTS (SELECT 1 FROM artifact_winners
		    WHERE candidate_id=browser_candidates.id
		      AND job_attempt_revision=browser_candidates.job_attempt_revision)`, stamp); err != nil {
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
	defer func() { _ = tx.Rollback() }()

	var retiredBindings []string
	staleRows, err := tx.QueryContext(ctx, `SELECT binding_id FROM materialization_claims
		WHERE browser_holder_generation<>?
		  AND phase IN ('claimed','bound','route_issued','navigated')
		  AND (lease_until IS NULL OR lease_until > ?)
		  AND NOT EXISTS (SELECT 1 FROM effect_permits p WHERE p.claim_id=materialization_claims.id)`,
		currentGeneration, now)
	if err != nil {
		return 0, err
	}
	for staleRows.Next() {
		var bindingID string
		if err := staleRows.Scan(&bindingID); err != nil {
			_ = staleRows.Close()
			return 0, err
		}
		retiredBindings = append(retiredBindings, bindingID)
	}
	if err := staleRows.Err(); err != nil {
		_ = staleRows.Close()
		return 0, err
	}
	_ = staleRows.Close()
	res, err := tx.ExecContext(ctx, `UPDATE materialization_claims
		SET phase='abandoned', updated_at=?
		WHERE browser_holder_generation<>?
		  AND phase IN ('claimed','bound','route_issued','navigated')
		  AND (lease_until IS NULL OR lease_until > ?)
		  AND NOT EXISTS (SELECT 1 FROM effect_permits p WHERE p.claim_id=materialization_claims.id)`,
		now, currentGeneration, now)
	if err != nil {
		return 0, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := consumeCloseAuthorizationsTx(ctx, tx, retiredBindings, now); err != nil {
		return 0, err
	}
	// The entry lease is deliberately NOT released here, and the asymmetry with
	// the expiry path is the point. A generation fence means the browser
	// session changed, not that the sign-in died: §4.5 keys a reserved entry on
	// the owner JOB precisely so a human sign-in survives a service-worker
	// restart or a reconnect. Releasing it here cut off exactly that case -
	// TestClaimObservationSurvivesAReconnectSinceArbitration caught it. Expiry
	// is different: no renewal arrived for the claim's own lease, which is real
	// death of the surface.
	if _, err := tx.ExecContext(ctx, `UPDATE browser_candidates
		SET status='eligible', updated_at=?
		WHERE status IN ('claimed','materializing')
		  AND NOT EXISTS (
			SELECT 1 FROM materialization_claims
			WHERE candidate_id=browser_candidates.id
			  AND phase IN ('claimed','bound','route_issued','navigated')
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM artifact_winners
			WHERE candidate_id=browser_candidates.id
			  AND job_attempt_revision=browser_candidates.job_attempt_revision
		  )`, now); err != nil {
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
