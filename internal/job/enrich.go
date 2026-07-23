// Copyright 2026 OrgMentem. Licensed under MIT.

package job

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// EnrichWorkRequestMetadata fills missing request metadata without replacing
// request-supplied values. It returns whether the work request changed.
func (js *Store) EnrichWorkRequestMetadata(ctx context.Context, workRequestID, title string, authors []string, year int) (bool, error) {
	title = strings.TrimSpace(title)
	authorsJSON, err := json.Marshal(authors)
	if err != nil {
		return false, fmt.Errorf("encoding work request authors: %w", err)
	}
	hasTitle := title != ""
	hasAuthors := len(authors) > 0
	hasYear := year > 0

	result, err := js.S.DB().ExecContext(ctx, `
		UPDATE work_requests
		SET title = CASE
				WHEN ? AND (title IS NULL OR TRIM(title) = '') THEN ?
				ELSE title
			END,
			authors_json = CASE
				WHEN ? AND (authors_json IS NULL OR TRIM(authors_json) IN ('', '[]', 'null')) THEN ?
				ELSE authors_json
			END,
			year = CASE
				WHEN ? AND (year IS NULL OR year = 0) THEN ?
				ELSE year
			END
		WHERE id = ?
			AND (
				(? AND (title IS NULL OR TRIM(title) = ''))
				OR (? AND (authors_json IS NULL OR TRIM(authors_json) IN ('', '[]', 'null')))
				OR (? AND (year IS NULL OR year = 0))
			)`,
		hasTitle, title,
		hasAuthors, string(authorsJSON),
		hasYear, year,
		workRequestID,
		hasTitle, hasAuthors, hasYear,
	)
	if err != nil {
		return false, fmt.Errorf("enriching work request metadata: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking work request metadata update: %w", err)
	}
	return changed > 0, nil
}
