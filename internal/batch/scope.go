// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package batch

import (
	"crypto/sha256"
	"encoding/hex"
	"papio/internal/protocol"
	"time"
)

// ScopedID separates a durable campaign from same-day ad hoc submissions.
func ScopedID(requests []protocol.WorkRequest, now time.Time, scope string) string {
	id := ID(requests, now)
	if scope == "" {
		return id
	}
	sum := sha256.Sum256([]byte(scope + "\n" + id))
	return "batch-" + hex.EncodeToString(sum[:16])
}
