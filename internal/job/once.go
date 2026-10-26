// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"papio/internal/store"
	"papio/internal/work"
)

// OnceConflictMarker is the stable text of every ErrOnceConflict refusal.
// Seams that only see the error as a string (an IPC RemoteError after the
// daemon boundary, or a plain in-process caller) match on this marker rather
// than restating the literal.
const OnceConflictMarker = "once-only request conflict"

// ErrOnceConflict reports a once-only submission that cannot converge on the
// live acquisition: the caller reused a request id with different work or
// filing intent, or the live job for the request id runs under a different
// acquisition policy. It is a caller-caused conflict, not a store failure,
// so the API reports it as conflict rather than internal.
var ErrOnceConflict = errors.New(OnceConflictMarker)

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

// onceFilingIntentMismatch reports how the once-only request's filing intent
// differs from the live job for the same canonical work. It names fields,
// not values: the values name where the caller files.
func onceFilingIntentMismatch(existing Policy, existingItem, existingCollection string, pol Policy, zotioKey, collection string) string {
	var fields []string
	if existing.AutoImport != pol.AutoImport {
		fields = append(fields, "auto_import")
	}
	if existingItem != zotioKey {
		fields = append(fields, "library_item")
	}
	if existingCollection != collection {
		fields = append(fields, "collection")
	}
	return strings.Join(fields, ", ")
}

// checkOnceCanonicalWork refuses a once-only request that names a different
// filing intent for work already in flight. A join would silently drop the
// new intent: the receipt records only the job id, and import planning still
// reads the original job's collection/auto-import policy. A fork would run
// the acquisition twice. The only safe answer names the live job and refuses.
// Desired version and resolver are acquisition identity: a mismatch there
// keeps the old fall-through that inserts a new job.
func checkOnceCanonicalWork(ctx context.Context, tx *sql.Tx, id string, pol Policy, zotioKey, collection string) error {
	var raw string
	var key, coll sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT j.policy_json,w.zotio_item_key,w.collection_key FROM jobs j JOIN work_requests w ON w.id=j.work_request_id WHERE j.id=?`, id).Scan(&raw, &key, &coll); err != nil {
		return err
	}
	var existing Policy
	if err := json.Unmarshal([]byte(raw), &existing); err != nil {
		return err
	}
	if existing.DesiredVersion != pol.DesiredVersion || existing.Resolver != pol.Resolver {
		return nil
	}
	if mismatch := onceFilingIntentMismatch(existing, key.String, coll.String, pol, zotioKey, collection); mismatch != "" {
		return fmt.Errorf("%w: once-only filing intent differs (%s) from live job %s for the same work", ErrOnceConflict, mismatch, id)
	}
	return nil
}
func commitOnceReceipt(ctx context.Context, tx *sql.Tx, requestID, fingerprint, jobID string, existing bool) (CreateResult, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO submission_receipts(request_id,fingerprint,job_id,created_at) VALUES(?,?,?,?)`, requestID, fingerprint, jobID, store.Now()); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{JobID: jobID, Existing: existing}, tx.Commit()
}
