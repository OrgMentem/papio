// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"papio/internal/store"
	"papio/internal/work"
)

// CreateOnceRequestForWork records convergence atomically and never recreates a
// terminal attempt for this request key. Ordinary submit retains its retry rules.
func (js *Store) CreateOnceRequestForWork(ctx context.Context, requestID string, w work.Work, zotioKey, collection string, pol Policy, rawIDs map[string]string, who Attribution) (CreateResult, error) {
	if requestID == "" {
		return CreateResult{}, errors.New("once-only submission requires a request id")
	}
	return js.createRequest(ctx, requestID, w, zotioKey, collection, pol, rawIDs, who, false, true, "", 0, true)
}
func onceFingerprint(w work.Work, pol Policy, zotioKey, collection string) string {
	data, _ := json.Marshal(struct {
		Work                                work.Work
		Version, Resolver, Item, Collection string
		AutoImport                          bool
	}{w, pol.DesiredVersion, pol.Resolver, zotioKey, collection, pol.AutoImport})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func compatibleOncePolicy(ctx context.Context, tx *sql.Tx, id string, pol Policy, zotioKey, collection string) (bool, error) {
	var raw string
	var key, coll sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT j.policy_json,w.zotio_item_key,w.collection_key FROM jobs j JOIN work_requests w ON w.id=j.work_request_id WHERE j.id=?`, id).Scan(&raw, &key, &coll); err != nil {
		return false, err
	}
	var existing Policy
	if err := json.Unmarshal([]byte(raw), &existing); err != nil {
		return false, err
	}
	return existing.DesiredVersion == pol.DesiredVersion && existing.AutoImport == pol.AutoImport && existing.Resolver == pol.Resolver && key.String == zotioKey && coll.String == collection, nil
}
func commitOnceReceipt(ctx context.Context, tx *sql.Tx, requestID, fingerprint, jobID string, existing bool) (CreateResult, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO submission_receipts(request_id,fingerprint,job_id,created_at) VALUES(?,?,?,?)`, requestID, fingerprint, jobID, store.Now()); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{JobID: jobID, Existing: existing}, tx.Commit()
}
