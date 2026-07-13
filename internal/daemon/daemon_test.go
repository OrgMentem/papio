package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/work"
)

type fakeLeaseStore struct {
	mu         sync.Mutex
	next       *job.Row
	claimed    bool
	recovered  int
	released   int
	owners     []string
	heartbeats chan struct{}
}

func (s *fakeLeaseStore) ClaimNext(_ context.Context, owner string, _ time.Duration) (*job.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed || s.next == nil {
		return nil, nil
	}
	s.claimed = true
	s.owners = append(s.owners, owner)
	return s.next, nil
}

func (s *fakeLeaseStore) Heartbeat(context.Context, string, string, time.Duration) error {
	select {
	case s.heartbeats <- struct{}{}:
	default:
	}
	return nil
}

func (s *fakeLeaseStore) Release(context.Context, string, string) error {
	s.mu.Lock()
	s.released++
	s.mu.Unlock()
	return nil
}

func (s *fakeLeaseStore) RecoverStale(context.Context) ([]string, error) {
	s.mu.Lock()
	s.recovered++
	s.mu.Unlock()
	return nil, nil
}

func (s *fakeLeaseStore) counts() (recovered, released int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recovered, s.released
}

func TestAutostarterUsesExecutableCommandSeamOnce(t *testing.T) {
	var starts int
	var calls [][]string
	starter := &Autostarter{
		SocketPath:    filepath.Join(t.TempDir(), "papio.sock"),
		StartTimeout:  time.Second,
		RetryInterval: time.Millisecond,
		Executable:    func() (string, error) { return "/test/papio", nil },
		Command: func(name string, args ...string) *exec.Cmd {
			calls = append(calls, append([]string{name}, args...))
			return exec.Command(name, args...)
		},
		Start: func(_ context.Context, cmd *exec.Cmd) error {
			if cmd.Stdin == nil || cmd.Stdout == nil || cmd.Stderr == nil {
				t.Error("daemon stdio was not detached")
			}
			starts++
			return nil
		},
		Ready: func(context.Context, string) error {
			if starts == 0 {
				return errors.New("not ready")
			}
			return nil
		},
	}
	if err := starter.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := starter.Ensure(context.Background()); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if starts != 1 || len(calls) != 1 {
		t.Fatalf("starts=%d command calls=%d, want one", starts, len(calls))
	}
	want := []string{"/test/papio", "daemon", "--socket", starter.SocketPath}
	if len(calls[0]) != len(want) {
		t.Fatalf("command args = %#v, want %#v", calls[0], want)
	}
	for i := range want {
		if calls[0][i] != want[i] {
			t.Fatalf("command args = %#v, want %#v", calls[0], want)
		}
	}
}

func TestSchedulerProcessesQueuedJobAndHeartbeats(t *testing.T) {
	leaseStore := &fakeLeaseStore{next: &job.Row{ID: "job_01"}, heartbeats: make(chan struct{}, 1)}
	processed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	scheduler, err := NewScheduler(leaseStore, ProcessorFunc(func(ctx context.Context, row *job.Row) error {
		if row.ID != "job_01" {
			t.Errorf("processed unexpected job %q", row.ID)
		}
		close(processed)
		select {
		case <-leaseStore.heartbeats:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}), SchedulerConfig{
		Owner:             "test-worker",
		LeaseDuration:     90 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond,
		PollInterval:      time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not process queued job")
	}
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
	recovered, released := leaseStore.counts()
	if recovered != 1 {
		t.Fatalf("RecoverStale calls = %d, want 1", recovered)
	}
	if released == 0 {
		t.Fatal("scheduler did not release completed lease")
	}
}

func TestSchedulerRecoversExpiredLease(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	jobs := &job.Store{S: db}
	id, err := jobs.CreateRequest(ctx, "request_01", work.Work{DOI: "10.1000/example", Title: "Example", Year: 2020}, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1024}, nil)
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	row, err := jobs.ClaimNext(ctx, "crashed-worker", 5*time.Millisecond)
	if err != nil || row == nil || row.ID != id {
		t.Fatalf("ClaimNext = %#v, %v", row, err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, map[string]any{"reason": "test"}); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	time.Sleep(15 * time.Millisecond)

	runCtx, cancel := context.WithCancel(context.Background())
	processed := make(chan struct{}, 1)
	scheduler, err := NewScheduler(jobs, ProcessorFunc(func(ctx context.Context, row *job.Row) error {
		if err := jobs.Transition(ctx, row.ID, job.StateResolving, job.StateFailed, map[string]any{"reason": "test"}, job.WithTerminalReason("test")); err != nil {
			return err
		}
		processed <- struct{}{}
		return nil
	}), SchedulerConfig{Owner: "new-worker", LeaseDuration: time.Second, HeartbeatInterval: 200 * time.Millisecond, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(runCtx) }()
	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not reclaim recovered resolving job")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	var owner sql.NullString
	if err := db.DB().QueryRowContext(ctx, `SELECT lease_owner FROM jobs WHERE id = ?`, id).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner.Valid {
		t.Fatalf("stale lease owner = %q, want cleared", owner.String)
	}
}

func TestSchedulerRejectsUnsafeHeartbeat(t *testing.T) {
	_, err := NewScheduler(&fakeLeaseStore{heartbeats: make(chan struct{}, 1)}, ProcessorFunc(func(context.Context, *job.Row) error { return nil }), SchedulerConfig{Owner: "worker", LeaseDuration: time.Second, HeartbeatInterval: 500 * time.Millisecond})
	if err == nil {
		t.Fatal("NewScheduler accepted heartbeat at half lease")
	}
}

func TestSchedulerRunUsesUniqueLeaseOwners(t *testing.T) {
	leaseStore := &fakeLeaseStore{heartbeats: make(chan struct{}, 1)}
	runOnce := func(jobID string) {
		t.Helper()
		leaseStore.mu.Lock()
		leaseStore.next = &job.Row{ID: jobID}
		leaseStore.claimed = false
		leaseStore.mu.Unlock()
		started := make(chan struct{})
		scheduler, err := NewScheduler(leaseStore, ProcessorFunc(func(ctx context.Context, _ *job.Row) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}), SchedulerConfig{Owner: "daemon", LeaseDuration: 50 * time.Millisecond, HeartbeatInterval: 10 * time.Millisecond, PollInterval: time.Millisecond})
		if err != nil {
			t.Fatalf("NewScheduler: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- scheduler.Run(ctx) }()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("scheduler did not claim the job")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("scheduler did not stop")
		}
	}
	runOnce("job_01")
	runOnce("job_02")
	leaseStore.mu.Lock()
	defer leaseStore.mu.Unlock()
	if len(leaseStore.owners) != 2 {
		t.Fatalf("lease owners = %#v, want two", leaseStore.owners)
	}
	if leaseStore.owners[0] == leaseStore.owners[1] {
		t.Fatalf("lease owners = %#v, want unique tokens per scheduler run", leaseStore.owners)
	}
}

type cancellationHeartbeatStore struct {
	mu               sync.Mutex
	row              *job.Row
	heartbeatStarted chan struct{}
}

func (s *cancellationHeartbeatStore) ClaimNext(context.Context, string, time.Duration) (*job.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.row
	s.row = nil
	return row, nil
}

func (s *cancellationHeartbeatStore) Heartbeat(ctx context.Context, _ string, _ string, _ time.Duration) error {
	select {
	case s.heartbeatStarted <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *cancellationHeartbeatStore) Release(context.Context, string, string) error  { return nil }
func (s *cancellationHeartbeatStore) RecoverStale(context.Context) ([]string, error) { return nil, nil }

func TestSchedulerIgnoresHeartbeatCancellationAfterSuccess(t *testing.T) {
	store := &cancellationHeartbeatStore{row: &job.Row{ID: "job_01"}, heartbeatStarted: make(chan struct{}, 1)}
	processed := make(chan struct{})
	scheduler, err := NewScheduler(store, ProcessorFunc(func(context.Context, *job.Row) error {
		select {
		case <-store.heartbeatStarted:
			close(processed)
			return nil
		case <-time.After(time.Second):
			return errors.New("heartbeat did not begin")
		}
	}), SchedulerConfig{Owner: "worker", LeaseDuration: 30 * time.Millisecond, HeartbeatInterval: time.Millisecond, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not process job")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
}

func TestSchedulerCancelledBeforeRecoveryReturnsNil(t *testing.T) {
	store := &fakeLeaseStore{heartbeats: make(chan struct{}, 1)}
	scheduler, err := NewScheduler(store, ProcessorFunc(func(context.Context, *job.Row) error { return nil }), SchedulerConfig{Owner: "worker"})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := scheduler.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	recovered, _ := store.counts()
	if recovered != 0 {
		t.Fatalf("RecoverStale calls = %d, want 0", recovered)
	}
}

func TestAutostarterDoesNotSpawnAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	starts := 0
	starter := &Autostarter{
		SocketPath: filepath.Join(t.TempDir(), "papio.sock"),
		Ready:      func(context.Context, string) error { return errors.New("not ready") },
		Start: func(context.Context, *exec.Cmd) error {
			starts++
			return nil
		},
	}
	if err := starter.Ensure(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ensure error = %v, want context.Canceled", err)
	}
	if starts != 0 {
		t.Fatalf("starts = %d, want 0", starts)
	}
}

func TestAutostarterDefaultCommandOutlivesCallerContext(t *testing.T) {
	started := false
	starter := &Autostarter{
		SocketPath: filepath.Join(t.TempDir(), "papio.sock"),
		Ready: func(context.Context, string) error {
			if started {
				return nil
			}
			return errors.New("not ready")
		},
		Start: func(_ context.Context, cmd *exec.Cmd) error {
			if cmd.Cancel != nil {
				t.Fatal("default daemon command is bound to the caller context")
			}
			started = true
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := starter.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	cancel()
	if !started {
		t.Fatal("daemon command was not started")
	}
}

type heartbeatFailureStore struct {
	row       *job.Row
	claimed   bool
	failed    chan struct{}
	heartbeat error
}

func (s *heartbeatFailureStore) ClaimNext(context.Context, string, time.Duration) (*job.Row, error) {
	if s.claimed {
		return nil, nil
	}
	s.claimed = true
	return s.row, nil
}

func (s *heartbeatFailureStore) Heartbeat(context.Context, string, string, time.Duration) error {
	select {
	case s.failed <- struct{}{}:
	default:
	}
	return s.heartbeat
}

func (s *heartbeatFailureStore) Release(context.Context, string, string) error  { return nil }
func (s *heartbeatFailureStore) RecoverStale(context.Context) ([]string, error) { return nil, nil }

func TestSchedulerReturnsHeartbeatStorageFailure(t *testing.T) {
	want := errors.New("lease store unavailable")
	store := &heartbeatFailureStore{row: &job.Row{ID: "job_01"}, failed: make(chan struct{}, 1), heartbeat: want}
	scheduler, err := NewScheduler(store, ProcessorFunc(func(ctx context.Context, _ *job.Row) error {
		<-ctx.Done()
		return ctx.Err()
	}), SchedulerConfig{Owner: "worker", LeaseDuration: 30 * time.Millisecond, HeartbeatInterval: time.Millisecond, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	err = scheduler.Run(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want heartbeat failure %v", err, want)
	}
}

func TestAutostarterDoesNotStartAfterCommandFactoryCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	starts := 0
	starter := &Autostarter{
		SocketPath: filepath.Join(t.TempDir(), "papio.sock"),
		Ready:      func(context.Context, string) error { return errors.New("not ready") },
		Command: func(name string, args ...string) *exec.Cmd {
			cancel()
			return exec.Command(name, args...)
		},
		Start: func(context.Context, *exec.Cmd) error {
			starts++
			return nil
		},
	}
	if err := starter.Ensure(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ensure error = %v, want context.Canceled", err)
	}
	if starts != 0 {
		t.Fatalf("starts = %d, want 0", starts)
	}
}

type completionHeartbeatFailureStore struct {
	row              *job.Row
	claimed          bool
	heartbeatStarted chan struct{}
	heartbeat        error
}

func (s *completionHeartbeatFailureStore) ClaimNext(context.Context, string, time.Duration) (*job.Row, error) {
	if s.claimed {
		return nil, nil
	}
	s.claimed = true
	return s.row, nil
}

func (s *completionHeartbeatFailureStore) Heartbeat(ctx context.Context, _ string, _ string, _ time.Duration) error {
	select {
	case s.heartbeatStarted <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return s.heartbeat
}

func (s *completionHeartbeatFailureStore) Release(context.Context, string, string) error { return nil }
func (s *completionHeartbeatFailureStore) RecoverStale(context.Context) ([]string, error) {
	return nil, nil
}

func TestSchedulerReportsHeartbeatFailureAfterProcessorCompletion(t *testing.T) {
	want := errors.New("heartbeat storage failure")
	store := &completionHeartbeatFailureStore{
		row:              &job.Row{ID: "job_01"},
		heartbeatStarted: make(chan struct{}, 1),
		heartbeat:        want,
	}
	scheduler, err := NewScheduler(store, ProcessorFunc(func(context.Context, *job.Row) error {
		select {
		case <-store.heartbeatStarted:
			return nil
		case <-time.After(time.Second):
			return errors.New("heartbeat did not start")
		}
	}), SchedulerConfig{Owner: "worker", LeaseDuration: 30 * time.Millisecond, HeartbeatInterval: time.Millisecond, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if err := scheduler.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want heartbeat failure %v", err, want)
	}
}

type claimFailureStore struct{ err error }

func (s claimFailureStore) ClaimNext(context.Context, string, time.Duration) (*job.Row, error) {
	return nil, s.err
}
func (claimFailureStore) Heartbeat(context.Context, string, string, time.Duration) error { return nil }
func (claimFailureStore) Release(context.Context, string, string) error                  { return nil }
func (claimFailureStore) RecoverStale(context.Context) ([]string, error)                 { return nil, nil }

func TestSchedulerReturnsQueuedWorkerFailure(t *testing.T) {
	want := errors.New("claim failure")
	scheduler, err := NewScheduler(claimFailureStore{err: want}, ProcessorFunc(func(context.Context, *job.Row) error { return nil }), SchedulerConfig{Owner: "worker"})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	if err := scheduler.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
}
