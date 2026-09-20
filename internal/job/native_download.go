// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"papio/internal/store"
)

var ErrNativeDownloadReserved = errors.New("native download attempt already reserved")

// NativeDownloadReservation records a one-shot observation under an existing
// effect permit. It grants no effect, and contains no external path or URL.
type NativeDownloadReservation struct {
	ReservationID      string                   `json:"reservation_id"`
	PermitID           string                   `json:"permit_id"`
	JobID              string                   `json:"-"`
	Producer           ArtifactProducerIdentity `json:"producer"`
	JobAttemptRevision int64                    `json:"job_attempt_revision"`
	HolderGeneration   int64                    `json:"holder_generation"`
	BindingSHA256      string                   `json:"binding_sha256"`
	ArmedAtMS          int64                    `json:"armed_at_ms"`
	ExpiresAtMS        int64                    `json:"expires_at_ms"`
}

type NativeDownloadAdmission struct {
	NativeDownloadReservation
	DownloadID        int64  `json:"download_id"`
	StartedAtMS       int64  `json:"started_at_ms"`
	ObservationSHA256 string `json:"observation_sha256"`
	Filename          string `json:"filename"`
	SHA256            string `json:"sha256"`
	SizeBytes         int64  `json:"size_bytes"`
}

func nativeSHA(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == 64 && strings.ToLower(s) == s && err == nil
}

func (r NativeDownloadReservation) valid() bool {
	return r.ReservationID != "" && r.PermitID != "" && r.JobID != "" && nativeSHA(r.BindingSHA256) &&
		r.Producer.validate(r.JobID) == nil && r.Producer.Kind == GenericDrive && r.Producer.Strategy == "generic" &&
		r.JobAttemptRevision > 0 && r.HolderGeneration > 0 && r.ArmedAtMS > 0 && r.ExpiresAtMS > r.ArmedAtMS
}

// nativeDownloadCurrentTx checks durable authority in the very transaction
// that reserves/adopts the observation. The bridge additionally checks live
// session, delegated config, and document binding under its session lock.
func nativeDownloadCurrentTx(ctx context.Context, tx *sql.Tx, r NativeDownloadReservation, now time.Time) error {
	if !r.valid() || now.UnixMilli() >= r.ExpiresAtMS {
		return ErrEffectPermitStale
	}
	p, err := scanPermit(tx.QueryRowContext(ctx, permitSelect+` WHERE id=?`, r.PermitID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEffectPermitStale
	}
	if err != nil {
		return err
	}
	if p.JobID != r.JobID || p.Status != Held || p.LeaseUntil == nil || !p.LeaseUntil.After(now) ||
		p.JobAttemptRevision != r.JobAttemptRevision || p.BrowserHolderGeneration != r.HolderGeneration ||
		p.Kind != GenericDrive || p.DriveAttemptID != r.Producer.DriveAttemptID || p.Ordinal == nil || *p.Ordinal != *r.Producer.Ordinal || p.Strategy != r.Producer.Strategy || p.Revision != r.Producer.Revision {
		return ErrEffectPermitStale
	}
	attempt, err := authorizedJobAttemptTx(ctx, tx, r.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEffectPermitStale
	}
	if err != nil {
		return err
	}
	if attempt != r.JobAttemptRevision {
		return ErrEffectPermitStale
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT holder_generation FROM daemon_authority_key WHERE singleton=1`).Scan(&generation); err != nil {
		return err
	}
	if generation != r.HolderGeneration {
		return ErrEffectPermitStale
	}
	// Offered identity is the durable epoch head; a permit left held after a
	// superseding offer must not import bytes for the replacement attempt.
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT detail_json FROM events WHERE job_id=? AND kind='browser.provider_drive_epoch_offered' ORDER BY seq DESC LIMIT 1`, r.JobID).Scan(&raw); err != nil {
		return ErrEffectPermitStale
	}
	var offered struct {
		DriveAttemptID string `json:"drive_attempt_id"`
		Ordinal        *int64 `json:"ordinal"`
		Strategy       string `json:"strategy"`
		Revision       string `json:"revision"`
		Domain         string `json:"safety_domain"`
	}
	if json.Unmarshal([]byte(raw), &offered) != nil || offered.DriveAttemptID != p.DriveAttemptID || offered.Ordinal == nil || *offered.Ordinal != *p.Ordinal || offered.Strategy != p.Strategy || offered.Revision != p.Revision || offered.Domain != p.SafetyDomainID {
		return ErrEffectPermitStale
	}
	var started, finished int
	err = tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(kind='browser.provider_drive_epoch_started'),0),
		COALESCE(SUM(kind IN ('browser.provider_drive_epoch_result','browser.provider_drive_epoch_superseded')),0)
		FROM events WHERE job_id=? AND json_extract(detail_json,'$.drive_attempt_id')=?
		AND json_extract(detail_json,'$.ordinal')=? AND json_extract(detail_json,'$.strategy')=? AND json_extract(detail_json,'$.revision')=?`, r.JobID, p.DriveAttemptID, *p.Ordinal, p.Strategy, p.Revision).Scan(&started, &finished)
	if err != nil {
		return err
	}
	if started < 1 || finished != 0 {
		return ErrEffectPermitStale
	}
	return nil
}

func nativeEventTx(ctx context.Context, tx *sql.Tx, jobID, kind string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events(job_id,at,kind,detail_json) VALUES(?,?,?,?)`, jobID, store.Now(), kind, string(b))
	return err
}

// ReserveNativeDownload consumes the one arm for this permit even if the
// worker/daemon subsequently loses its private filesystem baseline.
func (js *Store) ReserveNativeDownload(ctx context.Context, r NativeDownloadReservation, now time.Time) error {
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := nativeDownloadCurrentTx(ctx, tx, r, now); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='browser.native_download_reserved' AND json_extract(detail_json,'$.permit_id')=?`, r.JobID, r.PermitID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrNativeDownloadReserved
	}
	if err := nativeEventTx(ctx, tx, r.JobID, "browser.native_download_reserved", r); err != nil {
		return err
	}
	return tx.Commit()
}

// AdmitNativeDownload consumes a reservation and records the exact artifact
// producer BEFORE the staging file can enter the job's swept directory.
// No external source path, response body or page data enters these events.
func (js *Store) AdmitNativeDownload(ctx context.Context, a NativeDownloadAdmission, now time.Time) error {
	if !a.valid() || a.DownloadID < 1 || a.StartedAtMS < a.ArmedAtMS || a.StartedAtMS > now.UnixMilli() || !nativeSHA(a.ObservationSHA256) || !nativeSHA(a.SHA256) || a.SizeBytes < 1 || a.Filename != a.ReservationID+".pdf" || strings.ContainsAny(a.ReservationID, "/\\\x00:") {
		return ErrEffectPermitStale
	}
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := nativeDownloadCurrentTx(ctx, tx, a.NativeDownloadReservation, now); err != nil {
		return err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT detail_json FROM events WHERE job_id=? AND kind='browser.native_download_reserved' AND json_extract(detail_json,'$.reservation_id')=?`, a.JobID, a.ReservationID).Scan(&raw); err != nil {
		return ErrEffectPermitStale
	}
	var stored NativeDownloadReservation
	if json.Unmarshal([]byte(raw), &stored) != nil {
		return ErrEffectPermitStale
	}
	want, _ := json.Marshal(a.NativeDownloadReservation)
	got, _ := json.Marshal(stored)
	if string(want) != string(got) {
		return ErrEffectPermitStale
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id=? AND kind='browser.native_download_admitted' AND json_extract(detail_json,'$.reservation_id')=?`, a.JobID, a.ReservationID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrNativeDownloadReserved
	}
	if err := nativeEventTx(ctx, tx, a.JobID, "browser.native_download_admitted", a); err != nil {
		return err
	}
	if err := nativeEventTx(ctx, tx, a.JobID, "browser.download_complete", map[string]any{"download_id": a.DownloadID, "filename": a.Filename, "size_bytes": a.SizeBytes, "sha256": a.SHA256, "producer": a.Producer}); err != nil {
		return err
	}
	return tx.Commit()
}
