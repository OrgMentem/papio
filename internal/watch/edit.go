// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// EditInput changes operational settings only. Identity, search seeds, mode,
// scheduling history, digest entries, and failure recovery stay unchanged.
type EditInput struct {
	ID           int64   `json:"id"`
	Label        *string `json:"label,omitempty"`
	Collection   *string `json:"collection,omitempty"`
	CadenceHours *int    `json:"cadence_hours,omitempty"`
	PerRunCap    *int    `json:"per_run_cap,omitempty"`
	YearFrom     *int    `json:"year_from,omitempty"`
	YearTo       *int    `json:"year_to,omitempty"`
	OAOnly       *bool   `json:"oa_only,omitempty"`
}

func (s *Store) Edit(ctx context.Context, input EditInput) (*Watch, error) {
	if s == nil || s.S == nil {
		return nil, errors.New("watch store is not configured")
	}
	if input.ID <= 0 {
		return nil, errors.New("watch id must be positive")
	}
	if input.Label == nil && input.Collection == nil && input.CadenceHours == nil && input.PerRunCap == nil && input.YearFrom == nil && input.YearTo == nil && input.OAOnly == nil {
		return nil, errors.New("at least one operational setting is required")
	}
	tx, err := s.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanWatch(tx.QueryRowContext(ctx, watchSelect+` WHERE id = ?`, input.ID))
	if err != nil {
		return nil, err
	}
	next := CreateInput{Label: current.Label, Kind: current.Kind, Mode: current.Mode, Query: current.Query, Filters: current.Filters, Collection: current.Collection, CadenceHours: current.CadenceHours, PerRunCap: current.PerRunCap}
	if input.Label != nil {
		next.Label = *input.Label
	}
	if input.Collection != nil {
		next.Collection = *input.Collection
	}
	if input.CadenceHours != nil {
		next.CadenceHours = *input.CadenceHours
	}
	if input.PerRunCap != nil {
		if *input.PerRunCap == 0 {
			return nil, errors.New("watch per_run_cap must be 1-50")
		}
		next.PerRunCap = *input.PerRunCap
	}
	if input.YearFrom != nil {
		next.Filters.YearFrom = *input.YearFrom
	}
	if input.YearTo != nil {
		next.Filters.YearTo = *input.YearTo
	}
	if input.OAOnly != nil {
		next.Filters.OAOnly = *input.OAOnly
	}
	next, err = normalizeCreateInput(next)
	if err != nil {
		return nil, err
	}
	filters, err := json.Marshal(next.Filters)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE watches SET label=?, collection=?, cadence_hours=?, per_run_cap=?, filters_json=? WHERE id=?`, next.Label, next.Collection, next.CadenceHours, next.PerRunCap, string(filters), input.ID); err != nil {
		return nil, fmt.Errorf("editing watch: %w", err)
	}
	updated, err := scanWatch(tx.QueryRowContext(ctx, watchSelect+` WHERE id = ?`, input.ID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}
