// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/resolver"
	"papio/internal/work"
)

func TestSchedulerRestartRecoversInterruptedFetchAndValidation(t *testing.T) {
	for _, blockedStage := range []string{"fetch", "validation"} {
		t.Run(blockedStage, func(t *testing.T) {
			svc, jobs := newTestService(t)
			svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
				Source: "fixture", URL: "https://example.test/restart.pdf",
				Version: resolver.VersionPublished, AccessBasis: resolver.AccessOpen,
				ReuseLicense: "cc-by", ExpectedMIME: "application/pdf", Direct: true,
				IdentityConfidence: 1,
			}}}, Policy: config.Source{Enabled: true}}}
			var fetchCalls, validationCalls atomic.Int32
			entered := make(chan struct{})
			var enterOnce sync.Once
			svc.Fetch = func(ctx context.Context, candidate resolver.Candidate, path string) (fetch.Result, error) {
				call := fetchCalls.Add(1)
				if blockedStage == "fetch" && call == 1 {
					enterOnce.Do(func() { close(entered) })
					<-ctx.Done()
					return fetch.Result{}, ctx.Err()
				}
				body := pdfBytes(candidate.URL)
				if err := os.WriteFile(path, body, 0o600); err != nil {
					return fetch.Result{}, err
				}
				sum := sha256.Sum256(body)
				return fetch.Result{TempPath: path, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(body)), SniffedMIME: "application/pdf", ContentType: "application/pdf", HTTPStatus: 200, FinalHost: "example.test"}, nil
			}
			svc.Validate = func(ctx context.Context, _, _ string, _ work.Work) (pdf.ValidationReport, error) {
				call := validationCalls.Add(1)
				if blockedStage == "validation" && call == 1 {
					enterOnce.Do(func() { close(entered) })
					<-ctx.Done()
					return pdf.ValidationReport{}, ctx.Err()
				}
				return passValidation()(ctx, "", "", work.Work{})
			}
			id, err := svc.Submit(context.Background(), doiRequest("wr_scheduler_restart_"+blockedStage))
			if err != nil {
				t.Fatal(err)
			}

			first, err := daemon.NewScheduler(jobs, svc, daemon.SchedulerConfig{Owner: "first", Workers: 1, LeaseDuration: 100 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond, PollInterval: 5 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			firstCtx, stopFirst := context.WithCancel(context.Background())
			firstDone := make(chan error, 1)
			go func() { firstDone <- first.Run(firstCtx) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatalf("first scheduler never reached %s", blockedStage)
			}
			stopFirst()
			if err := <-firstDone; err != nil {
				t.Fatalf("first scheduler shutdown: %v", err)
			}
			interrupted, err := jobs.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			wantInterrupted := job.StateFetching
			if blockedStage == "validation" {
				wantInterrupted = job.StateValidating
			}
			if interrupted.State != wantInterrupted {
				t.Fatalf("interrupted state = %s, want %s", interrupted.State, wantInterrupted)
			}

			second, err := daemon.NewScheduler(jobs, svc, daemon.SchedulerConfig{Owner: "second", Workers: 1, LeaseDuration: 100 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond, PollInterval: 5 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			secondCtx, stopSecond := context.WithCancel(context.Background())
			secondDone := make(chan error, 1)
			go func() { secondDone <- second.Run(secondCtx) }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				row, err := jobs.Get(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				if row.State == job.StateReady {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("replacement scheduler stopped at %s", row.State)
				}
				time.Sleep(10 * time.Millisecond)
			}
			stopSecond()
			if err := <-secondDone; err != nil {
				t.Fatalf("second scheduler shutdown: %v", err)
			}
			var artifacts, candidates int
			if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM artifacts`).Scan(&artifacts); err != nil {
				t.Fatal(err)
			}
			if err := jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM candidates WHERE job_id = ?`, id).Scan(&candidates); err != nil {
				t.Fatal(err)
			}
			if artifacts != 1 || candidates != 1 {
				t.Fatalf("restart duplicates: artifacts=%d candidates=%d", artifacts, candidates)
			}
		})
	}
}
