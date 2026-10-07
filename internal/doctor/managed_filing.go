// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"fmt"
	"path/filepath"

	"papio/internal/config"
	"papio/internal/store"
)

func checkManagedFiling(ctx context.Context, cfg config.Config, db *store.Store, add func(string, string, string, string)) {
	if db == nil {
		add("filing", Skip, "managed filing is checked by the daemon", "")
		return
	}
	destination, err := filepath.Abs(cfg.Filing.Folder)
	if err != nil {
		add("filing", Warn, "managed filing destination is invalid", "inspect [filing] folder")
		return
	}
	var count int
	err = db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs j WHERE j.state IN ('ready', 'imported') AND j.acquisition_disposition = 'active' AND EXISTS (SELECT 1 FROM job_artifacts ja WHERE ja.job_id = j.id AND ja.artifact_sha256 = j.artifact_sha256 AND ja.role = 'main' AND ja.identity_result IN ('pass', 'user_confirmed')) AND NOT EXISTS (SELECT 1 FROM managed_filings f WHERE f.job_id = j.id AND f.destination = ? AND f.state = 'filed')`, destination).Scan(&count)
	if err != nil {
		add("filing", Warn, "managed filing could not be counted", "inspect database permissions")
		return
	}
	if count == 0 {
		add("filing", Pass, "every active validated acquisition has a managed folder receipt", "")
		return
	}
	add("filing", Warn, fmt.Sprintf("%d validated acquisitions lack a managed folder receipt", count), "run `papio jobs filing list`; inspect failed receipts and retry after fixing the destination")
}
