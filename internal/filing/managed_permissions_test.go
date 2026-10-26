// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package filing_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/filing"
)

func TestManagedTransientOpenAndStatFailuresRetry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permission failures")
	}
	for _, failure := range []string{"artifact_open", "target_open", "target_stat"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			svc, id, sha, source := managedFixture(t)
			key := filing.Key(id, svc.ManagedFilingFolder, sha)
			target := filepath.Join(svc.ManagedFilingFolder, key+".pdf")
			if failure != "artifact_open" {
				// Simulate a crash after linking but before the receipt commits.
				staged, err := filing.Stage(ctx, source, svc.ManagedFilingFolder, key, sha)
				if err != nil {
					t.Fatal(err)
				}
				if err := staged.Publish(ctx); err != nil {
					t.Fatal(err)
				}
				staged.Cleanup()
			}
			denied, mode := source, os.FileMode(0o400)
			op := "mkdir"
			switch failure {
			case "target_open":
				denied = target
			case "target_stat":
				denied, mode, op = svc.ManagedFilingFolder, 0o700, "verify"
			}
			t.Cleanup(func() { _ = os.Chmod(denied, mode) })
			var hookErr error
			inject := true
			filing.SetBeforeIOForTest(t, func(got string) {
				if inject && got == op {
					hookErr = os.Chmod(denied, 0)
				}
			})
			got, err := svc.RetryManagedJob(ctx, id, "")
			if restoreErr := os.Chmod(denied, mode); restoreErr != nil {
				t.Fatal(restoreErr)
			}
			inject = false
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			if err != nil || got.State != "failed" || got.ErrorCode != "destination_unavailable" || got.RetryAt == "" || got.ReceiptPath != "" {
				t.Fatalf("transient %s = %+v, %v; want retryable destination failure", failure, got, err)
			}
			rows, _, err := svc.ManagedFilings(ctx, id, 10)
			if err != nil || len(rows) != 1 || rows[0].ErrorCode != "destination_unavailable" || rows[0].RetryAt == "" {
				t.Fatalf("stored retry = %+v, %v", rows, err)
			}
			now := time.Now().Add(2 * time.Minute)
			svc.Now = func() time.Time { return now }
			if err := svc.RetryManagedFiling(ctx); err != nil {
				t.Fatal(err)
			}
			rows, _, err = svc.ManagedFilings(ctx, id, 10)
			if err != nil || len(rows) != 1 || rows[0].State != "filed" || rows[0].Attempts != 2 || rows[0].ReceiptPath != target {
				t.Fatalf("automatic retry = %+v, %v", rows, err)
			}
			if body, err := os.ReadFile(target); err != nil || string(body) != "%PDF-1.4\nmanaged I/O fixture\n%%EOF" {
				t.Fatalf("retried artifact = %q, %v", body, err)
			}
		})
	}
}
