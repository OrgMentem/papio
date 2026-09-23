// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"fmt"

	"papio/internal/config"
	"papio/internal/job"
)

// RetryPublisher records an explicit route change. Once the DOI attempt has
// been used, an upgraded refusing adapter can restore the original route.
func (b *Bridge) RetryPublisher(ctx context.Context, actionID, revision int64) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.institutionalMaterializationAvailable() || b.arbitration.holderSession() == nil ||
		b.now().Sub(b.arbitration.holderSession().LastSyncAt) > sessionStaleAfter {
		return "", fmt.Errorf("%w: publisher retry requires a connected browser with institutional materialization support", job.ErrConflict)
	}
	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return "", err
	}
	found := false
	for _, action := range actions {
		if action.ID != actionID {
			continue
		}
		found = true
		row, err := b.jobs.Get(ctx, action.JobID)
		if err != nil {
			return "", err
		}
		if mode, offerable := b.offerableAccessMode(*row); !offerable || mode != config.ModeDelegated {
			return "", fmt.Errorf("%w: publisher retry requires delegated access mode", job.ErrConflict)
		}
		if _, ok := b.cfg.InstitutionFor(row.Policy.Resolver); !ok {
			return "", fmt.Errorf("%w: restore the job's resolver profile before retrying its publisher route", job.ErrConflict)
		}
	}
	if !found {
		return "", fmt.Errorf("%w: action is no longer open; list actions again", job.ErrConflict)
	}
	id, err := b.jobs.RetryPublisherHandoff(ctx, actionID, revision, b.arbitration.holderSession().AdapterVersions)
	if err != nil {
		return "", err
	}
	delete(b.offered, id)
	delete(b.queuedOffers, id)
	delete(b.materializationTracked, id)
	b.focusPending[id] = true
	row, err := b.jobs.Get(ctx, id)
	if err == nil {
		_, err = b.prepareMaterializationCandidate(ctx, *row)
	}
	if err != nil {
		return "", fmt.Errorf("%w: route retry recorded for %s; use actions open --job %s to resume: %v", job.ErrConflict, id, id, err)
	}
	return id, nil
}
