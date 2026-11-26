// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

// heartbeatJobStore opens a real job store holding one queued job.
func heartbeatJobStore(t *testing.T) (*job.Store, string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	jobs := &job.Store{S: db}
	id, err := jobs.CreateRequest(ctx, "request_heartbeat", work.Work{DOI: "10.1000/heartbeat", Title: "Heartbeat", Year: 2020}, "", "",
		job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1024}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	return jobs, id
}

// heartbeatOnly hides everything but the LeaseStore methods, so the scheduler
// cannot hear of a cancellation from the store and only its heartbeat can
// notice one.
type heartbeatOnly struct{ LeaseStore }

// runHeartbeatScheduler runs a scheduler over leases until the returned stop.
// stop first gives Run a moment to fail on its own: a worker that mishandles
// an abandoned job returns an error from Run within milliseconds of the
// processor ending, which stopping the scheduler at once would hide.
func runHeartbeatScheduler(t *testing.T, leases LeaseStore, heartbeat, lease time.Duration, processor Processor) (stop func()) {
	t.Helper()
	scheduler, err := NewScheduler(leases, processor, SchedulerConfig{
		Owner: "worker", LeaseDuration: lease, HeartbeatInterval: heartbeat, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(runCtx) }()
	return func() {
		select {
		case err := <-done:
			cancel()
			t.Errorf("Run = %v before it was stopped; an abandoned job is not a daemon failure", err)
			return
		case <-time.After(300 * time.Millisecond):
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run = %v; want nil (an abandoned job is not a daemon failure)", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("scheduler did not stop")
		}
	}
}

// blockingProcessor moves its job to resolving, reports that it started, and
// then waits for its context, reporting why that ended. It reports a failure if
// nothing ends it within five seconds.
func blockingProcessor(jobs *job.Store, started chan<- struct{}, finished chan<- error) Processor {
	return ProcessorFunc(func(ctx context.Context, row *job.Row) error {
		if err := jobs.Transition(ctx, row.ID, job.StateQueued, job.StateResolving, nil); err != nil {
			return err
		}
		close(started)
		select {
		case <-ctx.Done():
			finished <- context.Cause(ctx)
			return ctx.Err()
		case <-time.After(5 * time.Second):
			finished <- errors.New("processor kept running after the job was cancelled")
			return nil
		}
	})
}

// TestSchedulerHeartbeatStopsProcessorWhenJobCancelled pins
// papio-bcd62aa64125e723: a cancel committed while the processor is working
// clears the lease, and the scheduler must cancel the processor's context with
// the lost-lease cause instead of letting it carry on with provider,
// filesystem and publication work for a job the user already ended. The store
// here tells the scheduler nothing, so the heartbeat is what notices.
func TestSchedulerHeartbeatStopsProcessorWhenJobCancelled(t *testing.T) {
	jobs, id := heartbeatJobStore(t)
	started := make(chan struct{})
	finished := make(chan error, 1)
	stop := runHeartbeatScheduler(t, heartbeatOnly{jobs}, 5*time.Millisecond, time.Second, blockingProcessor(jobs, started, finished))
	defer stop()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not start the job")
	}
	if err := jobs.Cancel(context.Background(), id, job.TerminalReasonCancelledByUser); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case cause := <-finished:
		if !errors.Is(cause, job.ErrLeaseLost) {
			t.Fatalf("processor stopped with cause %v; want %v", cause, job.ErrLeaseLost)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("processor context was never cancelled after the job was cancelled")
	}
}

// TestSchedulerCancelStopsProcessorBeforeTheNextHeartbeat: a cancellation
// committed in this process stops the processor at once. The heartbeat here is
// five seconds apart, so only the store's own notice can end the processor
// inside the bound.
func TestSchedulerCancelStopsProcessorBeforeTheNextHeartbeat(t *testing.T) {
	jobs, id := heartbeatJobStore(t)
	started := make(chan struct{})
	finished := make(chan error, 1)
	stop := runHeartbeatScheduler(t, jobs, 5*time.Second, 30*time.Second, blockingProcessor(jobs, started, finished))
	defer stop()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not start the job")
	}
	if err := jobs.Cancel(context.Background(), id, job.TerminalReasonCancelledByUser); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case cause := <-finished:
		if !errors.Is(cause, job.ErrLeaseLost) {
			t.Fatalf("processor stopped with cause %v; want %v", cause, job.ErrLeaseLost)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("processor kept running until the next heartbeat after the job was cancelled")
	}
}

// TestSchedulerCancelOfAnotherJobLeavesProcessorRunning: the notice names a
// job, and only that job's processor stops.
func TestSchedulerCancelOfAnotherJobLeavesProcessorRunning(t *testing.T) {
	jobs, _ := heartbeatJobStore(t)
	started := make(chan struct{})
	finished := make(chan error, 1)
	stop := runHeartbeatScheduler(t, jobs, 5*time.Second, 30*time.Second, blockingProcessor(jobs, started, finished))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not start the job")
	}
	// The one worker is busy, so this job stays queued until it is cancelled.
	other, err := jobs.CreateRequest(context.Background(), "request_other", work.Work{DOI: "10.1000/other", Title: "Other", Year: 2021}, "", "",
		job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1024}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if err := jobs.Cancel(context.Background(), other, job.TerminalReasonCancelledByUser); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case cause := <-finished:
		t.Fatalf("processor stopped (%v) when another job was cancelled", cause)
	case <-time.After(200 * time.Millisecond):
	}
	stop()
}

// TestSchedulerHeartbeatLeavesProcessorRunningAfterItsOwnTransition keeps the
// other half of the contract: a processor that reaches ready keeps working
// (auto-import, ready hook) with a live context while heartbeats observe the
// lease its own transition released.
func TestSchedulerHeartbeatLeavesProcessorRunningAfterItsOwnTransition(t *testing.T) {
	jobs, _ := heartbeatJobStore(t)
	result := make(chan error, 1)
	stop := runHeartbeatScheduler(t, jobs, 5*time.Millisecond, time.Second, ProcessorFunc(func(ctx context.Context, row *job.Row) error {
		if err := jobs.Transition(ctx, row.ID, job.StateQueued, job.StateResolving, nil); err != nil {
			return err
		}
		if err := jobs.Transition(ctx, row.ID, job.StateResolving, job.StateReady, nil); err != nil {
			return err
		}
		// Many heartbeat intervals pass after the releasing transition.
		time.Sleep(150 * time.Millisecond)
		result <- ctx.Err()
		return nil
	}))
	defer stop()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("processor context after its own ready transition = %v; want live", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("processor did not finish")
	}
}

// TestSchedulerCancelStopsEveryProcessorOfAJob: a worker can park a job and keep
// working on its tail while another worker claims the job once it resumes, so
// one job can have two processors, and a cancellation must stop both. A
// finished processor removes only its own registration.
func TestSchedulerCancelStopsEveryProcessorOfAJob(t *testing.T) {
	scheduler := &Scheduler{}
	register := func(jobID string) (*activeProcessor, context.Context) {
		ctx, cancel := context.WithCancelCause(context.Background())
		t.Cleanup(func() { cancel(nil) })
		return scheduler.startActive(jobID, cancel), ctx
	}
	first, firstCtx := register("job_1")
	second, secondCtx := register("job_1")
	_, otherCtx := register("job_2")

	scheduler.stopCancelled("job_1")
	for name, ctx := range map[string]context.Context{"first": firstCtx, "second": secondCtx} {
		if cause := context.Cause(ctx); !errors.Is(cause, job.ErrLeaseLost) {
			t.Errorf("%s processor stopped with cause %v; want %v", name, cause, job.ErrLeaseLost)
		}
	}
	if otherCtx.Err() != nil {
		t.Errorf("a cancellation of job_1 stopped job_2's processor: %v", otherCtx.Err())
	}

	// The first processor finishing leaves the second registered.
	scheduler.finishActive("job_1", first)
	thirdCtx, thirdCancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { thirdCancel(nil) })
	third := scheduler.startActive("job_1", thirdCancel)
	scheduler.finishActive("job_1", second)
	scheduler.stopCancelled("job_1")
	if cause := context.Cause(thirdCtx); !errors.Is(cause, job.ErrLeaseLost) {
		t.Fatalf("a processor registered after another finished stopped with cause %v; want %v", cause, job.ErrLeaseLost)
	}
	scheduler.finishActive("job_1", third)
	scheduler.activeMu.Lock()
	defer scheduler.activeMu.Unlock()
	if _, left := scheduler.active["job_1"]; left {
		t.Fatal("job_1 stays registered after every processor of it finished")
	}
}
