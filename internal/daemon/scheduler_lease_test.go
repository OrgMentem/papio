// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"papio/internal/job"
)

// lostLeaseStore hands out one job and then reports every heartbeat as a
// lost lease, modelling a wall-clock jump that expired the live holder's
// durable lease text.
type lostLeaseStore struct {
	row      *job.Row
	claimed  bool
	released chan struct{}
}

func (s *lostLeaseStore) ClaimNext(context.Context, string, time.Duration) (*job.Row, error) {
	if s.claimed {
		return nil, nil
	}
	s.claimed = true
	return s.row, nil
}

func (s *lostLeaseStore) Heartbeat(context.Context, string, string, time.Duration) error {
	return fmt.Errorf("%w: lease on job_01 lost to another worker", job.ErrConflict)
}

func (s *lostLeaseStore) Release(context.Context, string, string) error {
	select {
	case s.released <- struct{}{}:
	default:
	}
	return nil
}

func (s *lostLeaseStore) RecoverStale(context.Context) ([]string, error) { return nil, nil }
func (s *lostLeaseStore) CloseStaleHumanActions(context.Context) error   { return nil }

// TestSchedulerAbandonsLostLeaseQuietly pins the scheduler half of
// papio-e1e6e896: a heartbeat that reports the lease lost ends only that
// attempt instead of stopping the daemon.
func TestSchedulerAbandonsLostLeaseQuietly(t *testing.T) {
	store := &lostLeaseStore{row: &job.Row{ID: "job_01"}, released: make(chan struct{}, 1)}
	var mu sync.Mutex
	var gotCause error
	scheduler, err := NewScheduler(store, ProcessorFunc(func(ctx context.Context, _ *job.Row) error {
		<-ctx.Done()
		mu.Lock()
		gotCause = context.Cause(ctx)
		mu.Unlock()
		return ctx.Err()
	}), SchedulerConfig{Owner: "worker", LeaseDuration: 30 * time.Millisecond, HeartbeatInterval: time.Millisecond, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(runCtx) }()
	select {
	case <-store.released:
	case <-time.After(2 * time.Second):
		t.Fatal("lost-lease attempt was not abandoned")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v; want nil (a lost lease abandons one job, not the daemon)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop")
	}
	mu.Lock()
	cause := gotCause
	mu.Unlock()
	if !errors.Is(cause, job.ErrLeaseLost) {
		t.Fatalf("abandoned processor cause = %v; want %v", cause, job.ErrLeaseLost)
	}
}
