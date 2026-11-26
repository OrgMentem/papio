// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package artifact_test

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/artifact"
	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

// A flush failure inside Promote must abort the publication transaction: the
// acceptance edge is not recorded and the prepared-publication journal row
// survives, so recovery (not a half-committed ready job) owns the bytes. Once
// the flush works again the same journal row finalizes.
func TestPromoteFlushFailureKeepsPublicationJournal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fileErr  error
		dirErr   error
		destSeen bool
	}{
		{name: "file flush", fileErr: errors.New("injected file flush failure")},
		{name: "directory flush", dirErr: errors.New("injected directory flush failure"), destSeen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dataDir := storetest.DataDir(t)
			s, err := store.Open(ctx, dataDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			js := &job.Store{S: s}
			artifacts, err := artifact.New(dataDir)
			if err != nil {
				t.Fatal(err)
			}

			jobID, err := js.CreateRequest(ctx, "wr_artifact_flush", work.Work{
				DOI: "10.1002/example", Title: "An Example Paper", Authors: []string{"Author, A."}, Year: 2020,
			}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "institute", FetchMaxBytes: 1 << 20},
				nil, job.PrincipalUnknown)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range [][2]string{
				{job.StateQueued, job.StateResolving},
				{job.StateResolving, job.StateFetching},
				{job.StateFetching, job.StateValidating},
			} {
				if err := js.Transition(ctx, jobID, step[0], step[1], nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := js.InsertCandidates(ctx, jobID, []job.Candidate{{
				JobID: jobID, Source: "test", URLRedacted: "https://example.test/pdf", URLKey: "wr_artifact_flush",
				Version: "unknown", AccessBasis: "open_access", ReuseLicense: "unknown", Rank: 1,
			}}); err != nil {
				t.Fatal(err)
			}
			candidate, err := js.NextPendingCandidate(ctx, jobID)
			if err != nil || candidate == nil {
				t.Fatalf("candidate = %+v, %v", candidate, err)
			}

			quarantine, err := artifacts.QuarantineDir(jobID)
			if err != nil {
				t.Fatal(err)
			}
			temp := filepath.Join(quarantine, "download.tmp")
			body := []byte("%PDF-1.4 journal fixture body")
			if err := os.WriteFile(temp, body, 0o600); err != nil {
				t.Fatal(err)
			}
			sha, size, err := artifact.HashFile(temp)
			if err != nil {
				t.Fatal(err)
			}
			dest, err := artifacts.ArtifactPath(sha)
			if err != nil {
				t.Fatal(err)
			}
			candidateID := candidate.ID
			input := job.PublicationInput{
				ID: "publication_artifact_flush", JobID: jobID, CandidateID: &candidateID,
				Role: job.PublicationRoleMain, SHA256: sha, QuarantinePath: temp,
				Artifact: job.Artifact{
					SHA256: sha, SizeBytes: size, MIME: "application/pdf", PageCount: 1,
					Path: dest, IdentityResult: "pass",
				},
				FromState: job.StateValidating, ToState: job.StateReady,
			}
			if err := js.PreparePublication(ctx, input); err != nil {
				t.Fatal(err)
			}
			promote := func() (job.PromotionResult, error) {
				path, created, err := artifacts.Promote(temp, sha)
				return job.PromotionResult{Path: path, Created: created}, err
			}

			restore := artifact.FailFlushesForTest(t, tc.fileErr, tc.dirErr)
			injected := cmp.Or(tc.fileErr, tc.dirErr)
			if _, err := js.FinalizePublication(ctx, input.ID, promote); err == nil || !errors.Is(err, injected) {
				t.Fatalf("finalize error = %v, want the injected flush failure", err)
			}

			prepared, err := js.PreparedPublications(ctx, jobID)
			if err != nil || len(prepared) != 1 || prepared[0].ID != input.ID {
				t.Fatalf("journal after failed flush = %+v, %v; want the one prepared row", prepared, err)
			}
			if edge, err := js.HasPublicationEdge(ctx, jobID, sha, job.PublicationRoleMain); err != nil || edge {
				t.Fatalf("acquisition edge = %t, %v; want none after a failed flush", edge, err)
			}
			row, err := js.Get(ctx, jobID)
			if err != nil || row.State != job.StateValidating || row.ArtifactSHA256 != "" {
				t.Fatalf("job after failed flush = %+v, %v; want validating without an artifact", row, err)
			}
			if _, err := os.Stat(temp); err != nil {
				t.Fatalf("quarantine file lost: %v", err)
			}
			if _, err := os.Stat(dest); tc.destSeen != (err == nil) {
				t.Fatalf("destination stat = %v, want present=%t", err, tc.destSeen)
			}

			restore()
			result, err := js.FinalizePublication(ctx, input.ID, promote)
			if err != nil || result.Path != dest {
				t.Fatalf("retry finalize = %+v, %v", result, err)
			}
			if result.Created == tc.destSeen {
				t.Fatalf("retry created=%t, want %t", result.Created, !tc.destSeen)
			}
			if prepared, err := js.PreparedPublications(ctx, jobID); err != nil || len(prepared) != 0 {
				t.Fatalf("journal after retry = %+v, %v; want consumed", prepared, err)
			}
			if edge, err := js.HasPublicationEdge(ctx, jobID, sha, job.PublicationRoleMain); err != nil || !edge {
				t.Fatalf("acquisition edge after retry = %t, %v", edge, err)
			}
			if err := artifacts.Verify(sha); err != nil {
				t.Fatalf("verify: %v", err)
			}
		})
	}
}
