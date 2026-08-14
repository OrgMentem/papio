package job

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"papio/internal/store"
)

// EffectKind is the closed set of daemon-authorized irreversible effects.
type EffectKind string

const (
	EffectKindGenericDrive  EffectKind = "generic_drive"
	EffectKindDirectGet     EffectKind = "direct_get"
	EffectKindPDFGrab       EffectKind = "pdf_grab"
	EffectKindTerms         EffectKind = "terms"
	EffectKindInstitutional EffectKind = "institutional"
	GenericDrive                       = EffectKindGenericDrive
	DirectGet                          = EffectKindDirectGet
	PDFGrab                            = EffectKindPDFGrab
	Terms                              = EffectKindTerms
	Institutional                      = EffectKindInstitutional
)

// EffectPermitStatus is durable permit state.
type EffectPermitStatus string

const (
	EffectPermitHeld              EffectPermitStatus = "held"
	EffectPermitUnknownCompletion EffectPermitStatus = "unknown_completion"
	EffectPermitSettled           EffectPermitStatus = "settled"
	Held                                             = EffectPermitHeld
	UnknownCompletion                                = EffectPermitUnknownCompletion
	Settled                                          = EffectPermitSettled
)

var (
	ErrEffectPermitBusy          = errors.New("effect permit busy")
	ErrEffectPermitStale         = errors.New("effect permit stale")
	ErrArtifactProducerAmbiguous = errors.New("artifact producer correlation is ambiguous")
)

type EffectPermit struct {
	ID                      string
	JobID                   string
	JobAttemptRevision      int64
	BrowserHolderGeneration int64
	SafetyDomainID          string
	Kind                    EffectKind
	SlotIndex               int
	DriveAttemptID          string
	Ordinal                 *int64
	Strategy                string
	Revision                string
	ClaimID                 string
	BindingID               string
	EffectOrdinal           *int64
	GrabID                  string
	TermsOccurrenceID       string
	InstitutionalRequestID  string
	Status                  EffectPermitStatus
	LeaseUntil              *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time

	// SettlementDisposition is populated only on the value returned by
	// SettleEffectPermit. It is derived while holding the settlement
	// transaction, never from a pre-read in the browser bridge.
	CurrentAtSettlement bool
	OperatorOverridden  bool
}

type EffectPermitIdentity struct {
	JobID                  string
	Kind                   EffectKind
	DriveAttemptID         string
	Ordinal                int64
	Strategy               string
	Revision               string
	GrabID                 string
	TermsOccurrenceID      string
	ClaimID                string
	BindingID              string
	EffectOrdinal          int64
	InstitutionalRequestID string
}

// ArtifactProducerIdentity is the persisted, URL-free identity of the browser
// effect that produced one validated file. Ordinals are pointers so a corrupt
// durable event cannot turn an omitted ordinal into the valid value zero.
type ArtifactProducerIdentity struct {
	Kind                   EffectKind `json:"effect_kind"`
	DriveAttemptID         string     `json:"drive_attempt_id,omitempty"`
	Ordinal                *int64     `json:"ordinal,omitempty"`
	Strategy               string     `json:"strategy,omitempty"`
	Revision               string     `json:"revision,omitempty"`
	ClaimID                string     `json:"claim_id,omitempty"`
	BindingID              string     `json:"binding_id,omitempty"`
	EffectOrdinal          *int64     `json:"effect_ordinal,omitempty"`
	InstitutionalRequestID string     `json:"institutional_request_id,omitempty"`
}

func (p ArtifactProducerIdentity) validate(jobID string) error {
	switch p.Kind {
	case GenericDrive, DirectGet:
		if !nonempty(jobID) || !nonempty(p.DriveAttemptID) || p.Ordinal == nil ||
			*p.Ordinal < 0 || !nonempty(p.Strategy) || !nonempty(p.Revision) {
			return errors.New("artifact drive producer identity is incomplete")
		}
		if p.Kind == DirectGet && p.Strategy != "direct_get" {
			return errors.New("artifact direct producer requires direct_get strategy")
		}
		if p.Kind == GenericDrive && p.Strategy == "direct_get" {
			return errors.New("artifact generic producer cannot use direct_get strategy")
		}
		if p.ClaimID != "" || p.BindingID != "" || p.EffectOrdinal != nil || p.InstitutionalRequestID != "" {
			return errors.New("artifact drive producer carries institutional identity")
		}
	case Institutional:
		if !nonempty(jobID) || !nonempty(p.ClaimID) || !nonempty(p.BindingID) ||
			p.EffectOrdinal == nil || *p.EffectOrdinal < 1 || !nonempty(p.InstitutionalRequestID) {
			return errors.New("artifact institutional producer identity is incomplete")
		}
		if p.DriveAttemptID != "" || p.Ordinal != nil || p.Strategy != "" || p.Revision != "" {
			return errors.New("artifact institutional producer carries drive identity")
		}
	default:
		return errors.New("artifact producer kind cannot produce a job-scoped artifact")
	}
	return nil
}

func (p ArtifactProducerIdentity) effectIdentity(jobID string) EffectPermitIdentity {
	identity := EffectPermitIdentity{
		JobID: jobID, Kind: p.Kind,
		DriveAttemptID: p.DriveAttemptID, Strategy: p.Strategy, Revision: p.Revision,
		ClaimID: p.ClaimID, BindingID: p.BindingID,
		InstitutionalRequestID: p.InstitutionalRequestID,
	}
	if p.Ordinal != nil {
		identity.Ordinal = *p.Ordinal
	}
	if p.EffectOrdinal != nil {
		identity.EffectOrdinal = *p.EffectOrdinal
	}
	return identity
}

func (p ArtifactProducerIdentity) legacyIdentity(jobID string) LegacyEffectBlockerInput {
	identity := p.effectIdentity(jobID)
	return LegacyEffectBlockerInput{
		Kind: identity.Kind, JobID: identity.JobID,
		DriveAttemptID: identity.DriveAttemptID, Ordinal: identity.Ordinal,
		Strategy: identity.Strategy, Revision: identity.Revision,
		ClaimID: identity.ClaimID, BindingID: identity.BindingID,
		EffectOrdinal: identity.EffectOrdinal,
	}
}

func artifactProducerEqual(a, b ArtifactProducerIdentity) bool {
	if a.Kind != b.Kind || a.DriveAttemptID != b.DriveAttemptID ||
		a.Strategy != b.Strategy || a.Revision != b.Revision ||
		a.ClaimID != b.ClaimID || a.BindingID != b.BindingID ||
		a.InstitutionalRequestID != b.InstitutionalRequestID {
		return false
	}
	if (a.Ordinal == nil) != (b.Ordinal == nil) ||
		(a.EffectOrdinal == nil) != (b.EffectOrdinal == nil) {
		return false
	}
	if a.Ordinal != nil && *a.Ordinal != *b.Ordinal {
		return false
	}
	return a.EffectOrdinal == nil || *a.EffectOrdinal == *b.EffectOrdinal
}

type EffectPermitEvent struct {
	Kind   string
	Detail map[string]any
}
type EffectPermitAcquireInput struct {
	Identity                EffectPermitIdentity
	JobAttemptRevision      int64
	BrowserHolderGeneration int64
	SafetyDomainID          string
	LeaseUntil              time.Time
	Authorization           EffectPermitEvent
}
type EffectPermitAcquireOutcome string

const (
	EffectPermitAcquired     EffectPermitAcquireOutcome = "acquired"
	EffectPermitDuplicate    EffectPermitAcquireOutcome = "duplicate"
	EffectPermitBusyOutcome  EffectPermitAcquireOutcome = "busy"
	EffectPermitStaleOutcome EffectPermitAcquireOutcome = "stale"
	Acquired                                            = EffectPermitAcquired
	Duplicate                                           = EffectPermitDuplicate
	Busy                                                = EffectPermitBusyOutcome
	Stale                                               = EffectPermitStaleOutcome
)

type EffectPermitNavigationFence struct {
	ClaimID, BindingID          string
	RouteIssuanceOrdinal, TabID int64
}

type EffectPermitSettleInput struct {
	Identity       EffectPermitIdentity
	RequiredEvents []EffectPermitEvent
	// CurrentAttemptRevision and CurrentBrowserHolderGeneration make
	// CurrentEvents a commit-time projection. The exact result is always
	// settled, but these events are inserted only when the attempt and holder
	// fences still match inside the same transaction.
	CurrentAttemptRevision         int64
	CurrentBrowserHolderGeneration int64
	CurrentEvents                  []EffectPermitEvent
	Navigation                     *EffectPermitNavigationFence
}
type EffectPermitSettleOutcome string

const (
	EffectPermitApplied         EffectPermitSettleOutcome = "applied"
	EffectPermitSettleDuplicate EffectPermitSettleOutcome = "duplicate"
	EffectPermitSettleStale     EffectPermitSettleOutcome = "stale"
	Applied                                               = EffectPermitApplied
)

type EffectPermitObservation struct {
	PermitID                string
	BrowserHolderGeneration int64
	SettledProof            bool
	Dispatched              bool
	CorrelatedDownload      bool
	Acknowledged            bool
}

func (k EffectKind) valid() bool {
	return k == GenericDrive || k == DirectGet || k == PDFGrab || k == Terms || k == Institutional
}
func nonempty(v string) bool { return strings.TrimSpace(v) != "" && len(v) <= 256 }

func (i EffectPermitIdentity) validate() error {
	if !i.Kind.valid() {
		return errors.New("effect permit identity requires valid kind")
	}
	switch i.Kind {
	case GenericDrive, DirectGet:
		if !nonempty(i.JobID) {
			return errors.New("effect permit identity requires job and valid kind")
		}
		if !nonempty(i.DriveAttemptID) || i.Ordinal < 0 || !nonempty(i.Strategy) || !nonempty(i.Revision) {
			return errors.New("drive effect identity is incomplete")
		}
		if i.Kind == DirectGet && i.Strategy != "direct_get" {
			return errors.New("direct get requires direct_get strategy")
		}
		if i.Kind == GenericDrive && i.Strategy == "direct_get" {
			return errors.New("generic drive cannot use direct_get strategy")
		}
	case PDFGrab:
		if nonempty(i.JobID) {
			return errors.New("pdf grab identity must be jobless")
		}
		if !nonempty(i.GrabID) {
			return errors.New("pdf grab identity requires grab id")
		}
	case Terms:
		if !nonempty(i.JobID) {
			return errors.New("effect permit identity requires job and valid kind")
		}
		if !nonempty(i.TermsOccurrenceID) {
			return errors.New("terms identity requires occurrence id")
		}
	case Institutional:
		if !nonempty(i.JobID) {
			return errors.New("effect permit identity requires job and valid kind")
		}
		if !nonempty(i.ClaimID) || !nonempty(i.BindingID) || i.EffectOrdinal < 1 || !nonempty(i.InstitutionalRequestID) {
			return errors.New("institutional identity is incomplete")
		}
	}
	return nil
}

// ArtifactProducerForArtifact recovers a producer persisted before adoption.
// Filename alone is not authority: the validated SHA-256 must match, and all
// matching durable observations must name the same complete producer tuple.
func (js *Store) ArtifactProducerForArtifact(ctx context.Context, jobID, filename, sha256 string) (*ArtifactProducerIdentity, error) {
	if !nonempty(jobID) || strings.TrimSpace(filename) == "" || len(sha256) != 64 {
		return nil, ErrEffectPermitStale
	}
	if _, err := hex.DecodeString(sha256); err != nil {
		return nil, ErrEffectPermitStale
	}
	rows, err := js.S.DB().QueryContext(ctx, `
		SELECT detail_json
		  FROM events
		 WHERE job_id=? AND kind='browser.download_complete'
		 ORDER BY seq DESC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found *ArtifactProducerIdentity
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var detail struct {
			Filename string          `json:"filename"`
			SHA256   string          `json:"sha256"`
			Producer json.RawMessage `json:"producer"`
		}
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			return nil, err
		}
		if detail.Filename != filename || detail.SHA256 != sha256 {
			continue
		}
		if len(detail.Producer) == 0 || strings.TrimSpace(string(detail.Producer)) == "null" {
			return nil, fmt.Errorf("%w: matching download observation has no producer", ErrArtifactProducerAmbiguous)
		}
		var producer ArtifactProducerIdentity
		if err := json.Unmarshal(detail.Producer, &producer); err != nil {
			return nil, fmt.Errorf("%w: matching download observation has malformed producer: %w", ErrArtifactProducerAmbiguous, err)
		}
		if err := producer.validate(jobID); err != nil {
			return nil, fmt.Errorf("%w: matching download observation has incomplete producer: %w", ErrArtifactProducerAmbiguous, err)
		}
		if found != nil && !artifactProducerEqual(*found, producer) {
			return nil, ErrArtifactProducerAmbiguous
		}
		copy := producer
		found = &copy
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return found, nil
}
func (in EffectPermitAcquireInput) validate() error {
	if err := in.Identity.validate(); err != nil {
		return err
	}
	if in.Identity.Kind == Institutional {
		return errors.New("institutional permits require institutional acquire")
	}
	if in.Identity.Kind == PDFGrab {
		return errors.New("pdf grab permits require allocation transaction")
	}
	if in.JobAttemptRevision < 1 || in.BrowserHolderGeneration < 0 || !nonempty(in.SafetyDomainID) {
		return errors.New("effect permit acquire fence is incomplete")
	}
	if in.Authorization.Kind == "" {
		return errors.New("effect permit authorization event is required")
	}
	return nil
}

func eventJSON(e EffectPermitEvent) (string, error) {
	if strings.TrimSpace(e.Kind) == "" {
		return "", errors.New("effect permit event kind is required")
	}
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	b, err := json.Marshal(e.Detail)
	return string(b), err
}
func appendPermitEvent(ctx context.Context, tx *sql.Tx, jobID, now string, e EffectPermitEvent) error {
	detail, err := eventJSON(e)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events(job_id, at, kind, detail_json) VALUES(?, ?, ?, ?)`, jobID, now, e.Kind, detail)
	return err
}

const permitSelect = `SELECT id, job_id, job_attempt_revision, browser_holder_generation, safety_domain_id,
 effect_kind, slot_index, COALESCE(drive_attempt_id,''), ordinal, COALESCE(strategy,''), COALESCE(revision,''),
 COALESCE(claim_id,''), COALESCE(binding_id,''), effect_ordinal, COALESCE(grab_id,''),
 COALESCE(terms_occurrence_id,''), COALESCE(institutional_request_id,''), status, lease_until, created_at, updated_at FROM effect_permits`

func scanPermit(s interface{ Scan(...any) error }) (*EffectPermit, error) {
	var p EffectPermit
	var ord, eff sql.NullInt64
	var jobID sql.NullString
	var lease, created, updated sql.NullString
	var kind, status string
	if err := s.Scan(&p.ID, &jobID, &p.JobAttemptRevision, &p.BrowserHolderGeneration, &p.SafetyDomainID, &kind, &p.SlotIndex, &p.DriveAttemptID, &ord, &p.Strategy, &p.Revision, &p.ClaimID, &p.BindingID, &eff, &p.GrabID, &p.TermsOccurrenceID, &p.InstitutionalRequestID, &status, &lease, &created, &updated); err != nil {
		return nil, err
	}
	p.JobID = jobID.String
	p.Kind, p.Status = EffectKind(kind), EffectPermitStatus(status)
	if ord.Valid {
		v := ord.Int64
		p.Ordinal = &v
	}
	if eff.Valid {
		v := eff.Int64
		p.EffectOrdinal = &v
	}
	var err error
	if created.String != "" {
		p.CreatedAt, err = time.Parse(time.RFC3339Nano, created.String)
		if err != nil {
			return nil, err
		}
	}
	if updated.String != "" {
		p.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated.String)
		if err != nil {
			return nil, err
		}
	}
	if lease.Valid && lease.String != "" {
		t, e := time.Parse(time.RFC3339Nano, lease.String)
		if e != nil {
			return nil, e
		}
		p.LeaseUntil = &t
	}
	return &p, nil
}

func identityWhere(i EffectPermitIdentity) (string, []any) {
	switch i.Kind {
	case GenericDrive, DirectGet:
		return `job_id=? AND effect_kind IN ('generic_drive','direct_get') AND drive_attempt_id=? AND ordinal=? AND strategy=? AND revision=?`, []any{i.JobID, i.DriveAttemptID, i.Ordinal, i.Strategy, i.Revision}
	case PDFGrab:
		return `effect_kind='pdf_grab' AND grab_id=?`, []any{i.GrabID}
	case Terms:
		return `effect_kind='terms' AND job_id=? AND terms_occurrence_id=?`, []any{i.JobID, i.TermsOccurrenceID}
	case Institutional:
		return `effect_kind='institutional' AND job_id=? AND claim_id=? AND binding_id=? AND effect_ordinal=? AND institutional_request_id=?`, []any{i.JobID, i.ClaimID, i.BindingID, i.EffectOrdinal, i.InstitutionalRequestID}
	}
	return "0", nil
}

// legacyEffectBlockerStatusTx performs the exact migration-era identity
// lookup inside the caller's admission transaction. Terms have no legacy
// representation: pre-permit terms effects were never imported as blockers.
// A settled row is still returned to the caller as a tombstone; it must not
// be treated as an empty slot or a successful replay.
func legacyEffectBlockerStatusTx(ctx context.Context, tx *sql.Tx, identity EffectPermitIdentity) (string, error) {
	if identity.Kind == Terms || identity.Kind == PDFGrab {
		return "", nil
	}
	legacy := LegacyEffectBlockerInput{
		Kind: identity.Kind, JobID: identity.JobID,
		DriveAttemptID: identity.DriveAttemptID, Ordinal: identity.Ordinal,
		Strategy: identity.Strategy, Revision: identity.Revision,
		ClaimID: identity.ClaimID, BindingID: identity.BindingID,
		EffectOrdinal: identity.EffectOrdinal,
	}
	where, args := legacyBlockerIdentityWhere(legacy)
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT status FROM legacy_effect_blockers WHERE `+where,
		args...).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return status, nil
}

func exactLegacyBlockerAdmission(status string) (EffectPermitAcquireOutcome, error) {
	switch LegacyEffectBlockerStatus(status) {
	case LegacyEffectBlockerUnresolved:
		return EffectPermitBusyOutcome, ErrEffectPermitBusy
	case LegacyEffectBlockerSettled:
		return EffectPermitStaleOutcome, ErrEffectPermitStale
	default:
		return EffectPermitStaleOutcome, errors.New("legacy effect blocker has invalid status")
	}
}

func authorizedJobAttemptTx(ctx context.Context, tx *sql.Tx, jobID string) (int64, error) {
	var attempt int64
	err := tx.QueryRowContext(ctx,
		`SELECT 1 + (SELECT COUNT(*) FROM events WHERE job_id=? AND kind='job.retry_requested')
		   FROM jobs
		  WHERE id=?
		    AND state='awaiting_human'
		    AND EXISTS (
				SELECT 1 FROM human_actions
				 WHERE human_actions.job_id=jobs.id
				   AND human_actions.kind='openurl_handoff'
				   AND human_actions.status='open'
			)`,
		jobID, jobID).Scan(&attempt)
	return attempt, err
}

func (js *Store) AcquireEffectPermit(ctx context.Context, in EffectPermitAcquireInput) (*EffectPermit, EffectPermitAcquireOutcome, error) {
	if err := in.validate(); err != nil {
		return nil, EffectPermitStaleOutcome, err
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, EffectPermitStaleOutcome, err
	}

	defer func() { _ = tx.Rollback() }()
	where, args := identityWhere(in.Identity)
	existing, err := scanPermit(tx.QueryRowContext(ctx, permitSelect+` WHERE `+where, args...))
	if err == nil {
		if existing.JobAttemptRevision != in.JobAttemptRevision ||
			existing.BrowserHolderGeneration != in.BrowserHolderGeneration ||
			existing.SafetyDomainID != in.SafetyDomainID {
			// A different holder, attempt, or domain may report an exact
			// historical result, but it may never reacquire start authority.
			if commitErr := tx.Commit(); commitErr != nil {
				return nil, EffectPermitStaleOutcome, commitErr
			}
			return existing, EffectPermitStaleOutcome, ErrEffectPermitStale
		}
		outcome := EffectPermitDuplicate
		if existing.Status == Held {
			if existing.Kind == GenericDrive || existing.Kind == DirectGet || existing.Kind == Terms {
				currentAttempt, attemptErr := authorizedJobAttemptTx(ctx, tx, existing.JobID)
				if errors.Is(attemptErr, sql.ErrNoRows) {
					if commitErr := tx.Commit(); commitErr != nil {
						return nil, EffectPermitStaleOutcome, commitErr
					}
					return existing, EffectPermitStaleOutcome, ErrEffectPermitStale
				}
				if attemptErr != nil {
					return nil, EffectPermitStaleOutcome, attemptErr
				}
				if currentAttempt != in.JobAttemptRevision {
					if commitErr := tx.Commit(); commitErr != nil {
						return nil, EffectPermitStaleOutcome, commitErr
					}
					return existing, EffectPermitStaleOutcome, ErrEffectPermitStale
				}
			}
			// Exact held replay is the same authorization delivery. This is
			// required when commit succeeded but the first response was lost.
			outcome = EffectPermitAcquired
		}
		if err := tx.Commit(); err != nil {
			return nil, EffectPermitStaleOutcome, err
		}
		return existing, outcome, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, EffectPermitStaleOutcome, err
	}
	if in.LeaseUntil.IsZero() || !in.LeaseUntil.After(time.Now().UTC()) {
		return nil, EffectPermitStaleOutcome, errors.New("effect permit lease must be in the future")
	}
	currentAttempt, attemptErr := authorizedJobAttemptTx(ctx, tx, in.Identity.JobID)
	if errors.Is(attemptErr, sql.ErrNoRows) {
		return nil, EffectPermitStaleOutcome, ErrEffectPermitStale
	} else if attemptErr != nil {
		return nil, EffectPermitStaleOutcome, attemptErr
	} else if currentAttempt != in.JobAttemptRevision {
		return nil, EffectPermitStaleOutcome, ErrEffectPermitStale
	}
	// A historical blocker is an exact identity fence, not merely another
	// global occupancy row. Unresolved blocks this start; settled remains a
	// cleanup-only tombstone and can never authorize it again.
	blockerStatus, blockerErr := legacyEffectBlockerStatusTx(ctx, tx, in.Identity)
	if blockerErr != nil {
		return nil, EffectPermitStaleOutcome, blockerErr
	}
	if blockerStatus != "" {
		outcome, admissionErr := exactLegacyBlockerAdmission(blockerStatus)
		return nil, outcome, admissionErr
	}
	var blocker int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM legacy_effect_blockers WHERE status='unresolved' LIMIT 1`).Scan(&blocker); err == nil {
		return nil, EffectPermitBusyOutcome, ErrEffectPermitBusy
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, EffectPermitStaleOutcome, err
	}
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM effect_permits WHERE status IN ('held','unknown_completion') LIMIT 1`).Scan(&live); err == nil {
		return nil, EffectPermitBusyOutcome, ErrEffectPermitBusy
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, EffectPermitStaleOutcome, err
	}
	var domain int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM effect_permits WHERE safety_domain_id=? AND status IN ('held','unknown_completion') LIMIT 1`, in.SafetyDomainID).Scan(&domain); err == nil {
		return nil, EffectPermitBusyOutcome, ErrEffectPermitBusy
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, EffectPermitStaleOutcome, err
	}
	now := store.Now()
	id := NewID("permit")
	lease := in.LeaseUntil.UTC().Format(time.RFC3339Nano)
	i := in.Identity
	var ordinal any
	if i.Kind == GenericDrive || i.Kind == DirectGet {
		ordinal = i.Ordinal
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO effect_permits(id,job_id,job_attempt_revision,browser_holder_generation,safety_domain_id,effect_kind,slot_index,drive_attempt_id,ordinal,strategy,revision,claim_id,binding_id,effect_ordinal,grab_id,terms_occurrence_id,institutional_request_id,status,lease_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'held',?,?,?)`, id, i.JobID, in.JobAttemptRevision, in.BrowserHolderGeneration, in.SafetyDomainID, string(i.Kind), 0, nullIf(i.DriveAttemptID), ordinal, nullIf(i.Strategy), nullIf(i.Revision), nullIf(i.ClaimID), nullIf(i.BindingID), nullableInt64(nil), nullIf(i.GrabID), nullIf(i.TermsOccurrenceID), nullIf(i.InstitutionalRequestID), lease, now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			existing, _ := scanPermit(tx.QueryRowContext(ctx, permitSelect+` WHERE `+where, args...))
			if existing != nil {
				if existing.JobAttemptRevision != in.JobAttemptRevision ||
					existing.BrowserHolderGeneration != in.BrowserHolderGeneration ||
					existing.SafetyDomainID != in.SafetyDomainID {
					_ = tx.Commit()
					return existing, EffectPermitStaleOutcome, ErrEffectPermitStale
				}
				_ = tx.Commit()
				return existing, EffectPermitDuplicate, nil
			}
			return nil, EffectPermitBusyOutcome, ErrEffectPermitBusy
		}
		return nil, EffectPermitStaleOutcome, err
	}
	if err := appendPermitEvent(ctx, tx, i.JobID, now, in.Authorization); err != nil {
		return nil, EffectPermitStaleOutcome, err
	}
	if err := tx.Commit(); err != nil {
		return nil, EffectPermitStaleOutcome, err
	}
	p, err := js.GetEffectPermit(ctx, id)
	return p, EffectPermitAcquired, err
}
func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
func nullIf(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func (js *Store) GetEffectPermit(ctx context.Context, id string) (*EffectPermit, error) {
	if id == "" {
		return nil, nil
	}
	p, e := scanPermit(js.S.DB().QueryRowContext(ctx, permitSelect+` WHERE id=?`, id))
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	return p, e
}

// GetEffectPermitByIdentity performs a read-only lookup using the same
// kind-specific identity predicate used by acquire and settle. Bridge callers
// must use this accessor rather than reaching into the permit table directly.
func (js *Store) GetEffectPermitByIdentity(ctx context.Context, identity EffectPermitIdentity) (*EffectPermit, error) {
	if err := identity.validate(); err != nil {
		return nil, err
	}
	where, args := identityWhere(identity)
	p, err := scanPermit(js.S.DB().QueryRowContext(ctx, permitSelect+` WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func appendUniqueArtifactEvent(ctx context.Context, tx *sql.Tx, jobID, now string, event EffectPermitEvent) error {
	if strings.TrimSpace(jobID) == "" {
		return nil
	}
	detail, err := eventJSON(event)
	if err != nil {
		return err
	}
	var exists int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM events WHERE job_id=? AND kind=? AND detail_json=? LIMIT 1`,
		jobID, event.Kind, detail).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return appendPermitEvent(ctx, tx, jobID, now, event)
}

func artifactPermitResultEvent(permit *EffectPermit) (EffectPermitEvent, error) {
	if permit == nil {
		return EffectPermitEvent{}, ErrEffectPermitStale
	}
	base := map[string]any{
		"safety_domain":     permit.SafetyDomainID,
		"artifact_observed": true,
		"cleanup_only":      true,
	}
	switch permit.Kind {
	case GenericDrive:
		if permit.Ordinal == nil {
			return EffectPermitEvent{}, ErrEffectPermitStale
		}
		base["drive_attempt_id"] = permit.DriveAttemptID
		base["ordinal"] = *permit.Ordinal
		base["strategy"] = permit.Strategy
		base["revision"] = permit.Revision
		base["outcome"] = "applied"
		return EffectPermitEvent{Kind: "browser.provider_drive_epoch_result", Detail: base}, nil
	case DirectGet:
		if permit.Ordinal == nil {
			return EffectPermitEvent{}, ErrEffectPermitStale
		}
		base["drive_attempt_id"] = permit.DriveAttemptID
		base["ordinal"] = *permit.Ordinal
		base["route_revision"] = permit.Revision
		base["outcome"] = "success"
		base["landing_class"] = "pdf"
		return EffectPermitEvent{Kind: "browser.provider_direct_get_result", Detail: base}, nil
	case Institutional:
		if permit.EffectOrdinal == nil {
			return EffectPermitEvent{}, ErrEffectPermitStale
		}
		base["claim_id"] = permit.ClaimID
		base["binding_id"] = permit.BindingID
		base["effect_ordinal"] = *permit.EffectOrdinal
		base["institutional_request_id"] = permit.InstitutionalRequestID
		base["outcome"] = "artifact_observed"
		return EffectPermitEvent{Kind: "browser.institutional_effect_result", Detail: base}, nil
	default:
		return EffectPermitEvent{}, errors.New("effect kind cannot settle from a job artifact")
	}
}

// settleArtifactProducerTx settles only the exact producer tuple. It is kept
// transaction-local so an artifact winner and occupancy release cannot split
// across a daemon crash.
func settleArtifactProducerTx(ctx context.Context, tx *sql.Tx, jobID string, producer ArtifactProducerIdentity) (bool, error) {
	if err := producer.validate(jobID); err != nil {
		return false, err
	}
	now := store.Now()
	identity := producer.effectIdentity(jobID)
	where, args := identityWhere(identity)
	permit, permitErr := scanPermit(tx.QueryRowContext(ctx, permitSelect+` WHERE `+where, args...))
	if errors.Is(permitErr, sql.ErrNoRows) {
		permit = nil
		permitErr = nil
	}
	if permitErr != nil {
		return false, permitErr
	}
	if permit != nil && permit.JobID != jobID {
		// The SQL identity predicate is job-scoped, but retain a defensive
		// check before any artifact result or occupancy mutation in case a
		// corrupt row or future predicate change ever crosses that fence.
		return false, ErrEffectPermitStale
	}

	// Legacy blockers are a separate migration-era occupancy projection. An
	// exact live permit and an exact blocker together are ambiguous provenance:
	// fail closed rather than releasing both (or choosing one arbitrarily).
	legacy := producer.legacyIdentity(jobID)
	legacyWhere, legacyArgs := legacyBlockerIdentityWhere(legacy)
	var blockerID, blockerStatus string
	blockerErr := tx.QueryRowContext(ctx,
		`SELECT id, status FROM legacy_effect_blockers WHERE `+legacyWhere,
		legacyArgs...).Scan(&blockerID, &blockerStatus)
	if errors.Is(blockerErr, sql.ErrNoRows) {
		blockerErr = nil
	}
	if blockerErr != nil {
		return false, blockerErr
	}
	if permit != nil && blockerID != "" {
		return false, ErrEffectPermitStale
	}

	if permit != nil {
		if permit.Status == Held || permit.Status == UnknownCompletion {
			event, eventErr := artifactPermitResultEvent(permit)
			if eventErr != nil {
				return false, eventErr
			}
			if eventErr = appendUniqueArtifactEvent(ctx, tx, permit.JobID, now, event); eventErr != nil {
				return false, eventErr
			}
			res, updateErr := tx.ExecContext(ctx,
				`UPDATE effect_permits SET status='settled', updated_at=? WHERE id=? AND status IN ('held','unknown_completion')`,
				now, permit.ID)
			if updateErr != nil {
				return false, updateErr
			}
			n, updateErr := res.RowsAffected()
			if updateErr != nil {
				return false, updateErr
			}
			if n != 1 {
				return false, ErrEffectPermitStale
			}
		} else if permit.Status != Settled {
			return false, ErrEffectPermitStale
		}
		return true, nil
	}
	if blockerID == "" {
		return false, nil
	}
	if blockerStatus != string(LegacyEffectBlockerUnresolved) {
		if blockerStatus == string(LegacyEffectBlockerSettled) {
			return true, nil
		}
		return false, ErrEffectPermitStale
	}
	res, updateErr := tx.ExecContext(ctx,
		`UPDATE legacy_effect_blockers SET status='settled', updated_at=? WHERE id=? AND status='unresolved'`,
		now, blockerID)
	if updateErr != nil {
		return false, updateErr
	}
	n, updateErr := res.RowsAffected()
	if updateErr != nil {
		return false, updateErr
	}
	if n != 1 {
		return false, ErrEffectPermitStale
	}
	detail := map[string]any{
		"blocker_id": blockerID, "effect_kind": producer.Kind,
		"artifact_observed": true, "cleanup_only": true,
	}
	if producer.Ordinal != nil {
		detail["drive_attempt_id"] = producer.DriveAttemptID
		detail["ordinal"] = *producer.Ordinal
		detail["strategy"] = producer.Strategy
		detail["revision"] = producer.Revision
	}
	if producer.EffectOrdinal != nil {
		detail["claim_id"] = producer.ClaimID
		detail["binding_id"] = producer.BindingID
		detail["effect_ordinal"] = *producer.EffectOrdinal
	}
	if eventErr := appendUniqueArtifactEvent(ctx, tx, jobID, now,
		EffectPermitEvent{Kind: "browser.legacy_effect_artifact_result", Detail: detail}); eventErr != nil {
		return false, eventErr
	}
	return true, nil
}

func (js *Store) SettleEffectPermit(ctx context.Context, in EffectPermitSettleInput) (*EffectPermit, EffectPermitSettleOutcome, error) {
	if err := in.Identity.validate(); err != nil {
		return nil, EffectPermitSettleStale, err
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, EffectPermitSettleStale, err
	}
	defer func() { _ = tx.Rollback() }()
	where, args := identityWhere(in.Identity)
	p, err := scanPermit(tx.QueryRowContext(ctx, permitSelect+` WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, EffectPermitSettleStale, ErrEffectPermitStale
	}
	if err != nil {
		return nil, EffectPermitSettleStale, err
	}
	duplicate := p.Status == EffectPermitSettled
	if !duplicate && p.Status != Held && p.Status != UnknownCompletion {
		return nil, EffectPermitSettleStale, ErrEffectPermitStale
	}
	now := store.Now()
	requiredEvents := in.RequiredEvents
	for _, ev := range requiredEvents {
		if !effectResultEventKind(ev.Kind) {
			continue
		}
		raw, marshalErr := eventJSON(ev)
		if marshalErr != nil {
			return nil, EffectPermitSettleStale, marshalErr
		}
		var detail map[string]json.RawMessage
		if unmarshalErr := json.Unmarshal([]byte(raw), &detail); unmarshalErr != nil ||
			!permitResultIdentityMatches(p, ev.Kind, detail) {
			return nil, EffectPermitSettleStale, ErrEffectPermitStale
		}
	}
	var durableResult string
	var hasDurableResult bool
	overridden := false
	if duplicate {
		overridden, err = permitOverrideExists(ctx, tx, p.ID)
		if err != nil {
			return nil, EffectPermitSettleStale, err
		}
		if overridden {
			// Administrative resolution deliberately applies no result,
			// latch, successor, redrive, or claim mutation.
			requiredEvents = nil
		} else {
			for _, ev := range requiredEvents {
				if !effectResultEventKind(ev.Kind) {
					continue
				}
				incoming, e := eventJSON(ev)
				if e != nil {
					return nil, EffectPermitSettleStale, e
				}
				durableResult, hasDurableResult, e = permitResultEventDetail(ctx, tx, p, ev.Kind)
				if e != nil {
					return nil, EffectPermitSettleStale, e
				}
				if hasDurableResult && durableResult != incoming {
					// The first exact result is immutable. A conflicting
					// duplicate cannot repair any result-side projection.
					requiredEvents = nil
				}
				break
			}
		}
	}

	// Re-read the full authority fence while holding the settlement
	// transaction. Exact results remain historical cleanup after the fence is
	// lost, but retry, cancellation, handoff closure, or holder replacement
	// must suppress every current-only event and claim mutation.
	current := !duplicate
	if current && in.Navigation != nil {
		current = p.BrowserHolderGeneration == in.CurrentBrowserHolderGeneration
	}
	expectedAttempt := p.JobAttemptRevision
	if current && in.CurrentAttemptRevision > 0 {
		current = p.JobAttemptRevision == in.CurrentAttemptRevision &&
			p.BrowserHolderGeneration == in.CurrentBrowserHolderGeneration
		expectedAttempt = in.CurrentAttemptRevision
	}
	if current && p.JobID != "" {
		attempt, attemptErr := authorizedJobAttemptTx(ctx, tx, p.JobID)
		switch {
		case errors.Is(attemptErr, sql.ErrNoRows):
			current = false
		case attemptErr != nil:
			return nil, EffectPermitSettleStale, attemptErr
		default:
			current = attempt == expectedAttempt
		}
	}
	if current && in.Navigation != nil {
		n := in.Navigation
		if n.TabID < 0 {
			current = false
		} else {
			res, e := tx.ExecContext(ctx, `UPDATE materialization_claims SET phase='navigated', updated_at=?
				WHERE id=? AND binding_id=? AND browser_holder_generation=? AND route_issuance_ordinal=?
				  AND tab_id=? AND phase IN ('route_issued','navigated')
				  AND (lease_until IS NULL OR lease_until > ?)
				  AND EXISTS (
					SELECT 1 FROM browser_candidates c
					JOIN institution_profiles p ON p.id=c.institution_profile_id
					WHERE c.id=materialization_claims.candidate_id
					  AND c.job_id=? AND c.job_attempt_revision=? AND c.safety_domain_id=?
					  AND p.tombstoned_at IS NULL AND p.revision=c.institution_profile_revision
					  AND c.job_attempt_revision = 1 + (
						SELECT COUNT(*) FROM events e
						 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
					  )
				  )`,
				now, n.ClaimID, n.BindingID, p.BrowserHolderGeneration,
				n.RouteIssuanceOrdinal, n.TabID, now, p.JobID, p.JobAttemptRevision,
				p.SafetyDomainID)
			if e != nil {
				return nil, EffectPermitSettleStale, e
			}
			rows, e := res.RowsAffected()
			if e != nil {
				return nil, EffectPermitSettleStale, e
			}
			current = rows == 1
		}
	}
	p.CurrentAtSettlement = current
	p.OperatorOverridden = overridden

	if p.Kind != PDFGrab {
		for _, ev := range requiredEvents {
			detail, e := eventJSON(ev)
			if e != nil {
				return nil, EffectPermitSettleStale, e
			}
			exists := duplicate && effectResultEventKind(ev.Kind) && hasDurableResult
			if !exists {
				var n int
				e = tx.QueryRowContext(ctx, `SELECT 1 FROM events WHERE job_id=? AND kind=? AND detail_json=? LIMIT 1`, p.JobID, ev.Kind, detail).Scan(&n)
				exists = e == nil
				if errors.Is(e, sql.ErrNoRows) {
					e = nil
				}
			}
			if e != nil {
				return nil, EffectPermitSettleStale, e
			}
			if !exists {
				if e = appendPermitEvent(ctx, tx, p.JobID, now, ev); e != nil {
					return nil, EffectPermitSettleStale, e
				}
			}
		}
		if current && !overridden {
			for _, ev := range in.CurrentEvents {
				detail, e := eventJSON(ev)
				if e != nil {
					return nil, EffectPermitSettleStale, e
				}
				var n int
				e = tx.QueryRowContext(ctx, `SELECT 1 FROM events WHERE job_id=? AND kind=? AND detail_json=? LIMIT 1`, p.JobID, ev.Kind, detail).Scan(&n)
				if errors.Is(e, sql.ErrNoRows) {
					e = nil
					n = 0
				}
				if e != nil {
					return nil, EffectPermitSettleStale, e
				}
				if n == 0 {
					if e = appendPermitEvent(ctx, tx, p.JobID, now, ev); e != nil {
						return nil, EffectPermitSettleStale, e
					}
				}
			}
		}
	}
	outcome := EffectPermitApplied
	if duplicate {
		outcome = EffectPermitSettleDuplicate
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status='settled',updated_at=? WHERE id=? AND status IN ('held','unknown_completion')`, now, p.ID); err != nil {
			return nil, EffectPermitSettleStale, err
		}
		p.Status = EffectPermitSettled
		p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, now)
	}
	if err := tx.Commit(); err != nil {
		return nil, EffectPermitSettleStale, err
	}
	return p, outcome, nil
}

func effectResultEventKind(kind string) bool {
	switch kind {
	case "browser.provider_drive_epoch_result",
		"browser.provider_direct_get_result",
		"browser.terms_effect_result",
		"browser.institutional_effect_result":
		return true
	default:
		return false
	}
}

func permitResultEventDetail(ctx context.Context, tx *sql.Tx, permit *EffectPermit, kind string) (string, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT detail_json FROM events WHERE job_id=? AND kind=? ORDER BY seq`, permit.JobID, kind)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return "", false, err
		}
		var detail map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			return "", false, err
		}
		if permitResultIdentityMatches(permit, kind, detail) {
			return raw, true, nil
		}
	}
	return "", false, rows.Err()
}

func permitOverrideExists(ctx context.Context, tx *sql.Tx, permitID string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT detail_json FROM events WHERE kind='effect_permit.override' ORDER BY seq`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var detail struct {
			PermitID string `json:"permit_id"`
		}
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			return false, err
		}
		if detail.PermitID == permitID {
			return true, nil
		}
	}
	return false, rows.Err()
}

func permitResultIdentityMatches(permit *EffectPermit, kind string, detail map[string]json.RawMessage) bool {
	stringField := func(name string) string {
		var value string
		_ = json.Unmarshal(detail[name], &value)
		return value
	}
	intField := func(name string) (int64, bool) {
		var value int64
		err := json.Unmarshal(detail[name], &value)
		return value, err == nil
	}
	switch permit.Kind {
	case GenericDrive:
		ordinal, ok := intField("ordinal")
		return kind == "browser.provider_drive_epoch_result" && ok && permit.Ordinal != nil &&
			stringField("drive_attempt_id") == permit.DriveAttemptID && ordinal == *permit.Ordinal &&
			stringField("strategy") == permit.Strategy && stringField("revision") == permit.Revision
	case DirectGet:
		ordinal, ok := intField("ordinal")
		return kind == "browser.provider_direct_get_result" && ok && permit.Ordinal != nil &&
			stringField("drive_attempt_id") == permit.DriveAttemptID && ordinal == *permit.Ordinal &&
			stringField("route_revision") == permit.Revision
	case Terms:
		return kind == "browser.terms_effect_result" &&
			stringField("permit_id") == permit.ID &&
			stringField("terms_occurrence_id") == permit.TermsOccurrenceID
	case Institutional:
		ordinal, ok := intField("effect_ordinal")
		return kind == "browser.institutional_effect_result" && ok && permit.EffectOrdinal != nil &&
			stringField("claim_id") == permit.ClaimID && stringField("binding_id") == permit.BindingID &&
			ordinal == *permit.EffectOrdinal &&
			stringField("institutional_request_id") == permit.InstitutionalRequestID
	default:
		return false
	}
}

func (js *Store) ReconcileEffectPermit(ctx context.Context, obs EffectPermitObservation) (*EffectPermit, error) {
	if obs.PermitID == "" || obs.BrowserHolderGeneration < 0 {
		return nil, ErrEffectPermitStale
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := scanPermit(tx.QueryRowContext(ctx, permitSelect+` WHERE id=?`, obs.PermitID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEffectPermitStale
	}
	if err != nil {
		return nil, err
	}
	// Reconciliation is correlated to the current holder/request by the
	// bridge, but classification belongs to the historical permit. A
	// replacement holder therefore uses the permit's stored generation and
	// is not rejected merely because its own generation differs.
	if p.Status == Settled {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return p, nil
	}
	next := UnknownCompletion
	if obs.SettledProof {
		next = Settled
	} else if obs.Dispatched || obs.CorrelatedDownload || obs.Acknowledged {
		next = Held
	}
	now := store.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status=?,updated_at=? WHERE id=? AND status IN ('held','unknown_completion')`, string(next), now, p.ID); err != nil {
		return nil, err
	}
	p.Status = next
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, now)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

func (js *Store) ResolveUnknownEffectPermit(ctx context.Context, id, reason string) error {
	if id == "" || strings.TrimSpace(reason) == "" {
		return errors.New("permit id and nonempty reason required")
	}
	tx, e := js.S.DB().BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	var job sql.NullString
	var status string
	e = tx.QueryRowContext(ctx, `SELECT job_id,status FROM effect_permits WHERE id=?`, id).Scan(&job, &status)
	if errors.Is(e, sql.ErrNoRows) {
		return ErrEffectPermitStale
	}
	if e != nil {
		return e
	}
	if status != "unknown_completion" {
		return ErrEffectPermitStale
	}
	// For PDF grab (jobless) the override event carries no job correlation;
	// store NULL job_id. For other kinds job must be present.
	var jobID any
	if job.Valid && job.String != "" {
		jobID = job.String
	} else {
		jobID = nil
	}
	now := store.Now()
	d := map[string]any{"permit_id": id, "reason": reason}
	b, _ := json.Marshal(d)
	if _, e = tx.ExecContext(ctx, `INSERT INTO events(job_id,at,kind,detail_json) VALUES(?,?,?,?)`, jobID, now, "effect_permit.override", string(b)); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE effect_permits SET status='settled',updated_at=? WHERE id=? AND status='unknown_completion'`, now, id); e != nil {
		return e
	}
	return tx.Commit()
}

func (js *Store) LiveEffectPermit(ctx context.Context) (*EffectPermit, error) {
	p, e := scanPermit(js.S.DB().QueryRowContext(ctx, permitSelect+` WHERE status IN ('held','unknown_completion') ORDER BY created_at,id LIMIT 1`))
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	return p, e
}
func (js *Store) DomainEffectPermit(ctx context.Context, domain string) (*EffectPermit, error) {
	if !nonempty(domain) {
		return nil, nil
	}
	p, e := scanPermit(js.S.DB().QueryRowContext(ctx, permitSelect+` WHERE safety_domain_id=? AND status IN ('held','unknown_completion') ORDER BY created_at,id LIMIT 1`, domain))
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	return p, e
}

// InstitutionalEffectPermitAcquireInput carries the complete CAS fence for an institutional effect.
type InstitutionalEffectPermitAcquireInput struct {
	JobID, ClaimID, BindingID, SafetyDomainID, InstitutionalRequestID  string
	JobAttemptRevision, BrowserHolderGeneration, ExpectedEffectOrdinal int64
	LeaseUntil                                                         time.Time
	Authorization                                                      EffectPermitEvent
}

func (in InstitutionalEffectPermitAcquireInput) validate() error {
	if !nonempty(in.JobID) || !nonempty(in.ClaimID) || !nonempty(in.BindingID) || !nonempty(in.SafetyDomainID) || !nonempty(in.InstitutionalRequestID) || in.JobAttemptRevision < 1 || in.BrowserHolderGeneration < 0 || in.ExpectedEffectOrdinal < 0 || in.Authorization.Kind == "" {
		return errors.New("institutional acquire fence is incomplete")
	}
	return nil
}
func (js *Store) AcquireInstitutionalEffectPermit(ctx context.Context, in InstitutionalEffectPermitAcquireInput) (*EffectPermit, EffectPermitAcquireOutcome, error) {
	if e := in.validate(); e != nil {
		return nil, EffectPermitStaleOutcome, e
	}
	tx, e := js.S.DB().BeginTx(ctx, nil)
	if e != nil {
		return nil, EffectPermitStaleOutcome, e
	}
	defer func() { _ = tx.Rollback() }()
	var existingID string
	e = tx.QueryRowContext(ctx, `SELECT id FROM effect_permits WHERE effect_kind='institutional' AND institutional_request_id=?`, in.InstitutionalRequestID).Scan(&existingID)
	if e == nil {
		p, e := scanPermit(tx.QueryRowContext(ctx, permitSelect+` WHERE id=?`, existingID))
		if e != nil {
			return nil, EffectPermitStaleOutcome, e
		}
		if p.JobID != in.JobID || p.ClaimID != in.ClaimID || p.BindingID != in.BindingID ||
			p.SafetyDomainID != in.SafetyDomainID ||
			p.JobAttemptRevision != in.JobAttemptRevision ||
			p.BrowserHolderGeneration != in.BrowserHolderGeneration ||
			p.EffectOrdinal == nil || *p.EffectOrdinal != in.ExpectedEffectOrdinal+1 {
			return nil, EffectPermitStaleOutcome, ErrEffectPermitStale
		}
		if p.Status == Held {
			current, currentErr := institutionalEffectPermitReplayCurrent(ctx, tx, p, in)
			if currentErr != nil {
				return nil, EffectPermitStaleOutcome, currentErr
			}
			if !current {
				return nil, EffectPermitStaleOutcome, ErrEffectPermitStale
			}
		}
		if e = tx.Commit(); e != nil {
			return nil, EffectPermitStaleOutcome, e
		}
		return p, EffectPermitDuplicate, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return nil, EffectPermitStaleOutcome, e
	}
	if in.LeaseUntil.IsZero() || !in.LeaseUntil.After(time.Now().UTC()) {
		return nil, EffectPermitStaleOutcome, errors.New("institutional effect permit lease must be in the future")
	}
	var candidateJob, domain, phase, lease string
	var claimHolder, claimAttempt, ordinal int64
	e = tx.QueryRowContext(ctx, `SELECT c.job_id,c.safety_domain_id,m.browser_holder_generation,c.job_attempt_revision,m.effect_ordinal,m.phase,COALESCE(m.lease_until,'')
		FROM materialization_claims m
		JOIN browser_candidates c ON c.id=m.candidate_id
		JOIN institution_profiles p ON p.id=c.institution_profile_id
		WHERE m.id=? AND m.binding_id=?
		  AND p.tombstoned_at IS NULL
		  AND p.revision=c.institution_profile_revision
		  AND EXISTS (
			SELECT 1 FROM jobs j
			 WHERE j.id=c.job_id
			   AND j.state='awaiting_human'
			   AND EXISTS (
				 SELECT 1 FROM human_actions h
				  WHERE h.job_id=j.id
				    AND h.kind='openurl_handoff'
				    AND h.status='open'
			   )
		  )
		  AND c.job_attempt_revision = 1 + (
			SELECT COUNT(*) FROM events e
			 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
		  )`, in.ClaimID, in.BindingID).Scan(&candidateJob, &domain, &claimHolder, &claimAttempt, &ordinal, &phase, &lease)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, EffectPermitStaleOutcome, ErrEffectPermitStale
	}
	if e != nil {
		return nil, EffectPermitStaleOutcome, e
	}
	if candidateJob != in.JobID || domain != in.SafetyDomainID || claimHolder != in.BrowserHolderGeneration || claimAttempt != in.JobAttemptRevision || phase != "bound" || ordinal != in.ExpectedEffectOrdinal {
		return nil, EffectPermitStaleOutcome, ErrEffectPermitStale
	}
	if lease != "" {
		if t, pe := time.Parse(time.RFC3339Nano, lease); pe != nil || !t.After(time.Now().UTC()) {
			return nil, EffectPermitStaleOutcome, ErrEffectPermitStale
		}
	}
	var live int
	if e = tx.QueryRowContext(ctx, `SELECT 1 FROM legacy_effect_blockers WHERE status='unresolved' LIMIT 1`).Scan(&live); e == nil {
		return nil, EffectPermitBusyOutcome, ErrEffectPermitBusy
	} else if !errors.Is(e, sql.ErrNoRows) {
		return nil, EffectPermitStaleOutcome, e
	}
	legacyIdentity := EffectPermitIdentity{
		Kind: Institutional, JobID: in.JobID, ClaimID: in.ClaimID,
		BindingID: in.BindingID, EffectOrdinal: in.ExpectedEffectOrdinal + 1,
	}
	blockerStatus, blockerErr := legacyEffectBlockerStatusTx(ctx, tx, legacyIdentity)
	if blockerErr != nil {
		return nil, EffectPermitStaleOutcome, blockerErr
	}
	if blockerStatus != "" {
		outcome, admissionErr := exactLegacyBlockerAdmission(blockerStatus)
		return nil, outcome, admissionErr
	}
	if e = tx.QueryRowContext(ctx, `SELECT 1 FROM effect_permits WHERE status IN ('held','unknown_completion') LIMIT 1`).Scan(&live); e == nil {
		return nil, EffectPermitBusyOutcome, ErrEffectPermitBusy
	} else if !errors.Is(e, sql.ErrNoRows) {
		return nil, EffectPermitStaleOutcome, e
	}
	now := store.Now()
	newOrd := in.ExpectedEffectOrdinal + 1
	res, e := tx.ExecContext(ctx, `UPDATE materialization_claims
		SET effect_ordinal=?,route_issuance_ordinal=route_issuance_ordinal+1,phase='route_issued',updated_at=?
		WHERE id=? AND binding_id=? AND browser_holder_generation=?
		  AND phase='bound' AND effect_ordinal=?
		  AND (lease_until IS NULL OR lease_until>?)
		  AND EXISTS (
			SELECT 1
			FROM browser_candidates c
			JOIN institution_profiles p ON p.id=c.institution_profile_id
			WHERE c.id=materialization_claims.candidate_id
			  AND c.job_id=? AND c.safety_domain_id=?
			  AND c.job_attempt_revision=?
			  AND p.tombstoned_at IS NULL
			  AND p.revision=c.institution_profile_revision
			  AND EXISTS (
				SELECT 1 FROM jobs j
				 WHERE j.id=c.job_id
				   AND j.state='awaiting_human'
				   AND EXISTS (
					 SELECT 1 FROM human_actions h
					  WHERE h.job_id=j.id
					    AND h.kind='openurl_handoff'
					    AND h.status='open'
				   )
			  )
			  AND c.job_attempt_revision = 1 + (
				SELECT COUNT(*) FROM events e
				 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
			  )
		  )`,
		newOrd, now, in.ClaimID, in.BindingID, in.BrowserHolderGeneration,
		in.ExpectedEffectOrdinal, now, in.JobID, in.SafetyDomainID, in.JobAttemptRevision)
	if e != nil {
		return nil, EffectPermitStaleOutcome, e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, EffectPermitStaleOutcome, ErrEffectPermitStale
	}
	id := NewID("permit")
	_, e = tx.ExecContext(ctx, `INSERT INTO effect_permits(id,job_id,job_attempt_revision,browser_holder_generation,safety_domain_id,effect_kind,slot_index,claim_id,binding_id,effect_ordinal,institutional_request_id,status,lease_until,created_at,updated_at) VALUES(?,?,?,?,?,?,0,?,?,?,?,'held',?,?,?)`, id, in.JobID, in.JobAttemptRevision, in.BrowserHolderGeneration, in.SafetyDomainID, string(Institutional), in.ClaimID, in.BindingID, newOrd, in.InstitutionalRequestID, in.LeaseUntil.UTC().Format(time.RFC3339Nano), now, now)
	if e != nil {
		return nil, EffectPermitStaleOutcome, e
	}
	if e = appendPermitEvent(ctx, tx, in.JobID, now, in.Authorization); e != nil {
		return nil, EffectPermitStaleOutcome, e
	}
	if e = tx.Commit(); e != nil {
		return nil, EffectPermitStaleOutcome, e
	}
	p, e := js.GetEffectPermit(ctx, id)
	return p, EffectPermitAcquired, e
}

func institutionalEffectPermitReplayCurrent(ctx context.Context, tx *sql.Tx, permit *EffectPermit, in InstitutionalEffectPermitAcquireInput) (bool, error) {
	if permit == nil || permit.EffectOrdinal == nil {
		return false, nil
	}
	var current int
	err := tx.QueryRowContext(ctx, `SELECT 1
		FROM materialization_claims m
		JOIN browser_candidates c ON c.id=m.candidate_id
		JOIN institution_profiles p ON p.id=c.institution_profile_id
		WHERE m.id=? AND m.binding_id=? AND m.browser_holder_generation=?
		  AND m.phase IN ('route_issued','navigated') AND m.effect_ordinal=?
		  AND c.job_id=? AND c.safety_domain_id=? AND c.job_attempt_revision=?
		  AND p.tombstoned_at IS NULL
		  AND p.revision=c.institution_profile_revision
		  AND EXISTS (
			SELECT 1 FROM jobs j
			 WHERE j.id=c.job_id
			   AND j.state='awaiting_human'
			   AND EXISTS (
				 SELECT 1 FROM human_actions h
				  WHERE h.job_id=j.id
				    AND h.kind='openurl_handoff'
				    AND h.status='open'
			   )
		  )
		  AND c.job_attempt_revision = 1 + (
			SELECT COUNT(*) FROM events e
			 WHERE e.job_id=c.job_id AND e.kind='job.retry_requested'
		  )`,
		in.ClaimID, in.BindingID, in.BrowserHolderGeneration, *permit.EffectOrdinal,
		in.JobID, in.SafetyDomainID, in.JobAttemptRevision).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
