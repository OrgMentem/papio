// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/job"
)

// Failure of the state write must also roll back the replacement prompt.
func TestRejectPathsAreAtomic(t *testing.T) {
	for _, name := range []string{"supplied", "adopted-moved", "adopted-unmoved"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			svc, jobs := newTestService(t)
			id := createLiveJob(t, jobs, "wr_reject_atomic_"+name,
				[2]string{job.StateQueued, job.StateResolving},
				[2]string{job.StateResolving, job.StateFetching})
			if _, err := jobs.S.DB().ExecContext(ctx, `
				CREATE TRIGGER reject_park_failure BEFORE UPDATE OF state ON jobs
				WHEN NEW.state IN ('awaiting_human', 'needs_review')
				BEGIN SELECT RAISE(ABORT, 'injected park failure'); END`); err != nil {
				t.Fatal(err)
			}
			reject := func() error {
				if name == "supplied" {
					return svc.rejectSuppliedPDF(ctx, id, job.Access(false, ""))
				}
				dir := filepath.Join(svc.Config.EffectiveAdoptionRoot(), id)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "paper.pdf")
				if err := os.WriteFile(path, pdfBytes("rejected"), 0o600); err != nil {
					t.Fatal(err)
				}
				if name == "adopted-unmoved" {
					if err := os.WriteFile(filepath.Join(svc.Config.EffectiveAdoptionRoot(), "rejected"), []byte("block mkdir"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return svc.rejectAdoptedDownload(ctx, id, path, job.Access(false, ""))
			}
			if err := reject(); err == nil || !strings.Contains(err.Error(), "injected park failure") {
				t.Fatalf("reject error = %v, want injected park failure", err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != job.StateFetching {
				t.Fatalf("job state = %s, want fetching", row.State)
			}
			if kinds := openActionKinds(t, jobs, id); kinds["manual_download"] {
				t.Fatalf("stale replacement prompt left open: %v", kinds)
			}
		})
	}
}
