package delivery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A cancellation decided from an earlier read must lose to a submission that
// already happened. submitToProvider sends the irreversible provider request
// while the row is still offered and only then CAS-records submitted, so an
// unconditional cancel landing in that window reports success to the operator
// while the institution keeps fulfilling the request. This is the interleave
// deliveryCancel now survives.
func TestStaleCancelLosesToARecordedSubmission(t *testing.T) {
	svc, clock := testServiceClock(t)
	ctx := context.Background()
	testJob(t, svc, "cancelrace1")
	// RecordSubmission requires the owning job to be resolving: only that
	// state legitimately owns an in-flight submission.
	if _, err := svc.store.DB().ExecContext(ctx, `UPDATE jobs SET state = 'resolving' WHERE id = ?`, "cancelrace1"); err != nil {
		t.Fatal(err)
	}
	created, err := svc.Create(ctx, CreateRequest{
		JobID:              "cancelrace1",
		InstitutionProfile: "default",
		Provider:           "illiad",
		RequestClass:       "digital_journal_article",
		WorkIdentity:       "doi:10.1000/cancelrace1",
		GateProfileDigest:  "digest",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The cancelling caller's snapshot, read while the row was still offered.
	stale, err := svc.Get(ctx, created.ID)
	if err != nil || stale == nil {
		t.Fatalf("get: %v, %v", stale, err)
	}
	if stale.State != StateOffered {
		t.Fatalf("fixture state = %q, want offered", stale.State)
	}

	// The provider request went out and was recorded.
	won, err := svc.RecordSubmission(ctx, created.ID, "555", clock.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("RecordSubmission lost its CAS, want the submission recorded")
	}

	applied, err := svc.UpdateStateFrom(ctx, created.ID, stale.State, StateCancelled)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("stale cancellation applied, want it to lose: the provider request is live")
	}

	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSubmitted {
		t.Fatalf("state = %q, want submitted preserved", got.State)
	}
	if got.ProviderReference != "555" {
		t.Fatalf("provider_reference = %q, want the recorded transaction preserved", got.ProviderReference)
	}
}

// UpdateState is keyed by id alone, so it silently overwrites whatever a
// concurrent writer committed between a caller's read and its own write.
// Every production transition is decided from a prior read, so every one of
// them belongs on UpdateStateFrom. This guard exists because the unconditional
// method cannot be deleted - fixtures across several packages use it for
// seeding - and a comment saying "do not call this" is not enforcement.
func TestProductionUsesExpectedStateTransitions(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dev", "node_modules", "extension", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The declaration and its own package's callers are the point of the
		// method; the guard is about callers elsewhere.
		if filepath.Dir(path) == filepath.Join(root, "internal", "delivery") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, ".UpdateState(") {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+":"+itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("production callers of the unconditional UpdateState: %v\nuse UpdateStateFrom: the row can move between the read that decided this transition and the write that applies it", offenders)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
