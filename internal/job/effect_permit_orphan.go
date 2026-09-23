package job

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"papio/internal/store"
)

// An orphaned permit is an unknown_completion permit whose browser worker is
// gone: a dev reload, an extension update or a browser crash took the result
// with it. ADR-0022 keeps such a permit occupying the global effect lane,
// because silence and elapsed time never prove what the provider saw. These
// rules settle one only from evidence the daemon can observe itself. They
// apply to direct_get alone: its whole effect is one browser download steered
// into the job's adoption directory, so that directory and the job's download
// events are a complete record of what the effect did. Every other kind can
// change provider state without writing a file, and keeps waiting for its
// exact result or an operator.
const (
	// OrphanPermitAttributedEvent records that a settled PDF in the job's
	// adoption directory is attributed to the orphaned permit before the
	// ordinary sweep adopts it; adoption then settles the permit exactly.
	OrphanPermitAttributedEvent = "effect_permit.orphan_attributed"
	// OrphanPermitResolvedEvent records every settlement these rules make,
	// with the evidence it rests on.
	OrphanPermitResolvedEvent = "effect_permit.orphan_resolved"
)

// OrphanPermitRule names the evidence a settlement rests on.
type OrphanPermitRule string

const (
	// The effect's download completed, and its bytes are not a paper for
	// this job: not a PDF, so they were moved out of the adoption directory.
	OrphanPermitEffectCompletedNoPaper OrphanPermitRule = "effect_completed_no_paper"
	// The effect's download completed as a PDF, but the job is terminal and
	// will never adopt it.
	OrphanPermitEffectCompletedJobTerminal OrphanPermitRule = "effect_completed_job_terminal"
	// The worker's holder generation is superseded, the lease has expired,
	// and neither the job's directories nor its events show any effect.
	OrphanPermitNoEffectObserved OrphanPermitRule = "no_effect_observed"
)

// orphanPermitClockSkew widens "since the permit" to cover filesystem
// timestamp granularity and small clock steps. Widening only adds evidence
// of an effect, so it can only make these rules refuse more often.
const orphanPermitClockSkew = 2 * time.Second

// ErrOrphanPermitUnproven reports evidence that does not satisfy a rule. The
// permit stays unresolved, and doctor keeps reporting it.
var ErrOrphanPermitUnproven = errors.New("orphaned effect permit evidence does not prove its outcome")

// orphanPermitEvidenceKinds are the job events that show a browser download
// or a direct-route outcome happened. Any of them at or after the permit is
// evidence of an effect, so no_effect_observed must refuse.
var orphanPermitEvidenceKinds = []string{
	"browser.download_started",
	"browser.download_complete",
	"browser.delivery_context",
	"browser.adoption_deferred",
	"browser.provider_direct_get_result",
	OrphanPermitAttributedEvent,
}

// OrphanEvidenceSince is the earliest file time or event time that counts as
// this permit's consequence.
func (p *EffectPermit) OrphanEvidenceSince() time.Time {
	return p.CreatedAt.Add(-orphanPermitClockSkew)
}

// OrphanRecoverable reports whether p is a permit these rules may consider.
// A held permit never is: its worker may still be running.
func (p *EffectPermit) OrphanRecoverable() bool {
	return p != nil && p.Status == UnknownCompletion && p.Kind == DirectGet &&
		p.JobID != "" && p.Ordinal != nil
}

// OrphanPermitFile is one settled file the daemon observed for the permit's
// job, attributed to the permit by time: the global lane admits no other
// effect while this permit occupies it.
type OrphanPermitFile struct {
	Filename   string
	SizeBytes  int64
	SHA256     string
	ModifiedAt time.Time
	PDF        bool
	MovedAside bool
}

// OrphanPermitDirectories is what inspecting every adoption and rejected
// directory for the job showed. A directory that could not be read is not
// evidence of absence, so a caller that could not read one must not ask for
// a no-effect settlement.
type OrphanPermitDirectories struct {
	Inspected          int
	EntriesSincePermit int
	InProgress         int
}

func orphanPermitTx(ctx context.Context, tx *sql.Tx, permitID, jobID string) (*EffectPermit, error) {
	p, err := scanPermit(tx.QueryRowContext(ctx, permitSelect+` WHERE id=?`, permitID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEffectPermitStale
	}
	if err != nil {
		return nil, err
	}
	if !p.OrphanRecoverable() || (jobID != "" && p.JobID != jobID) {
		return nil, ErrEffectPermitStale
	}
	return p, nil
}

func (f OrphanPermitFile) attributable(p *EffectPermit) bool {
	name := strings.TrimSpace(f.Filename)
	return name != "" && filepath.IsLocal(f.Filename) && filepath.Base(f.Filename) == f.Filename &&
		f.SizeBytes > 0 && !f.ModifiedAt.Before(p.OrphanEvidenceSince())
}

func orphanPermitDetail(p *EffectPermit) map[string]any {
	return map[string]any{
		"permit_id":         p.ID,
		"effect_kind":       string(p.Kind),
		"safety_domain":     p.SafetyDomainID,
		"drive_attempt_id":  p.DriveAttemptID,
		"ordinal":           *p.Ordinal,
		"route_revision":    p.Revision,
		"permit_created_at": p.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (f OrphanPermitFile) detail(into map[string]any) {
	into["filename"] = f.Filename
	into["size_bytes"] = f.SizeBytes
	into["modified_at"] = f.ModifiedAt.UTC().Format(time.RFC3339Nano)
	into["pdf"] = f.PDF
	if f.SHA256 != "" {
		into["sha256"] = f.SHA256
	}
}

func settleOrphanPermitTx(ctx context.Context, tx *sql.Tx, p *EffectPermit, detail map[string]any) error {
	now := store.Now()
	if err := appendPermitEvent(ctx, tx, p.JobID, now, EffectPermitEvent{Kind: OrphanPermitResolvedEvent, Detail: detail}); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE effect_permits SET status='settled', updated_at=? WHERE id=? AND status='unknown_completion'`, now, p.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrEffectPermitStale
	}
	return nil
}

// AttributeOrphanedPermitArtifact returns the exact producer of an orphaned
// direct_get permit for a settled PDF that appeared in its job's adoption
// directory after the permit. The attribution is recorded first; the caller
// then adopts the file through the ordinary sweep, whose producer correlation
// settles the permit. Validation still decides whether the bytes are the paper.
func (js *Store) AttributeOrphanedPermitArtifact(ctx context.Context, permitID, jobID string, file OrphanPermitFile) (*ArtifactProducerIdentity, error) {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := orphanPermitTx(ctx, tx, permitID, jobID)
	if err != nil {
		return nil, err
	}
	if !file.PDF || file.MovedAside || !file.attributable(p) {
		return nil, ErrOrphanPermitUnproven
	}
	detail := orphanPermitDetail(p)
	file.detail(detail)
	if err := appendUniqueArtifactEvent(ctx, tx, p.JobID, store.Now(), EffectPermitEvent{Kind: OrphanPermitAttributedEvent, Detail: detail}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	ordinal := *p.Ordinal
	return &ArtifactProducerIdentity{
		Kind: DirectGet, DriveAttemptID: p.DriveAttemptID, Ordinal: &ordinal,
		Strategy: p.Strategy, Revision: p.Revision,
	}, nil
}

// SettleOrphanedPermitFromFile settles an orphaned direct_get permit whose
// download completed without giving the job a paper: the bytes are not a PDF
// and were moved out of the adoption directory, or the job is terminal.
func (js *Store) SettleOrphanedPermitFromFile(ctx context.Context, permitID, jobID string, rule OrphanPermitRule, file OrphanPermitFile) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := orphanPermitTx(ctx, tx, permitID, jobID)
	if err != nil {
		return err
	}
	if !file.attributable(p) || len(file.SHA256) != 64 {
		return ErrOrphanPermitUnproven
	}
	if _, err := hex.DecodeString(file.SHA256); err != nil {
		return ErrOrphanPermitUnproven
	}
	detail := orphanPermitDetail(p)
	switch rule {
	case OrphanPermitEffectCompletedNoPaper:
		// Bytes left in the adoption directory would make every later
		// download for this job ambiguous, so they must be aside first.
		if file.PDF || !file.MovedAside {
			return ErrOrphanPermitUnproven
		}
		detail["moved_to"] = "rejected"
	case OrphanPermitEffectCompletedJobTerminal:
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, p.JobID).Scan(&state); err != nil {
			return err
		}
		if !Terminal(state) {
			return ErrOrphanPermitUnproven
		}
		detail["job_state"] = state
	default:
		return ErrOrphanPermitUnproven
	}
	detail["rule"] = string(rule)
	file.detail(detail)
	if err := settleOrphanPermitTx(ctx, tx, p, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// SettleOrphanedPermitNoEffect settles an orphaned direct_get permit that
// left no trace: the holder generation that received it is superseded, its
// lease has expired, the caller read every directory the download could have
// written without finding anything since the permit, and no job event since
// the permit shows a download. A download that lands later is still adopted
// by the ordinary sweep; this settlement only frees the lane.
func (js *Store) SettleOrphanedPermitNoEffect(ctx context.Context, permitID string, currentGeneration int64, now time.Time, dirs OrphanPermitDirectories) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := orphanPermitTx(ctx, tx, permitID, "")
	if err != nil {
		return err
	}
	if currentGeneration <= p.BrowserHolderGeneration || p.LeaseUntil == nil || now.Before(*p.LeaseUntil) ||
		dirs.Inspected < 1 || dirs.EntriesSincePermit != 0 || dirs.InProgress != 0 {
		return ErrOrphanPermitUnproven
	}
	since := p.OrphanEvidenceSince()
	args := []any{p.JobID}
	for _, kind := range orphanPermitEvidenceKinds {
		args = append(args, kind)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(orphanPermitEvidenceKinds)), ",")
	// #nosec G202 -- only generated "?" placeholders enter the query text; the
	// job ID and event kinds remain bound arguments.
	rows, err := tx.QueryContext(ctx, `SELECT at FROM events WHERE job_id=? AND kind IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			return err
		}
		// An unreadable time cannot prove the event predates the permit.
		if when, parseErr := time.Parse(time.RFC3339Nano, at); parseErr != nil || !when.Before(since) {
			return ErrOrphanPermitUnproven
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	detail := orphanPermitDetail(p)
	detail["rule"] = string(OrphanPermitNoEffectObserved)
	detail["permit_holder_generation"] = p.BrowserHolderGeneration
	detail["current_holder_generation"] = currentGeneration
	detail["lease_until"] = p.LeaseUntil.UTC().Format(time.RFC3339Nano)
	detail["directories_inspected"] = dirs.Inspected
	detail["entries_since_permit"] = 0
	detail["download_events_since_permit"] = 0
	if err := settleOrphanPermitTx(ctx, tx, p, detail); err != nil {
		return err
	}
	return tx.Commit()
}
