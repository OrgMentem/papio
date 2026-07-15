package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"papio/internal/job"
)

// LeaseStore is the durable scheduler-facing portion of job.Store. job.Store
// uses one configured SQLite connection, so multiple Scheduler workers may call
// it concurrently without introducing a second database writer.
type LeaseStore interface {
	ClaimNext(context.Context, string, time.Duration) (*job.Row, error)
	Heartbeat(context.Context, string, string, time.Duration) error
	Release(context.Context, string, string) error
	RecoverStale(context.Context) ([]string, error)
	CloseStaleHumanActions(context.Context) error
}

// terminalQuarantineSweeper is optional so scheduler unit-test stores and
// alternate implementations remain focused on leasing. job.Store implements it
// to remove only terminal jobs' abandoned downloads.
type terminalQuarantineSweeper interface {
	SweepTerminalQuarantine(context.Context) error
}

// Processor performs the application work for one leased job.
type Processor interface {
	Process(context.Context, *job.Row) error
}

// ProcessorFunc adapts a function into a Processor.
type ProcessorFunc func(context.Context, *job.Row) error

// Process implements Processor.
func (f ProcessorFunc) Process(ctx context.Context, row *job.Row) error { return f(ctx, row) }

// MaintenanceRunner performs one bounded best-effort periodic maintenance
// pass. Its errors never terminate acquisition workers.
type MaintenanceRunner interface {
	RunDue(context.Context) error
}

// SchedulerConfig controls worker, lease, polling, and periodic maintenance behavior.
type SchedulerConfig struct {
	Owner               string
	Workers             int
	LeaseDuration       time.Duration
	HeartbeatInterval   time.Duration
	PollInterval        time.Duration
	Maintenance         MaintenanceRunner
	MaintenanceInterval time.Duration
}

// Scheduler claims durable jobs and processes them while renewing their lease.
type Scheduler struct {
	Store     LeaseStore
	Processor Processor
	Config    SchedulerConfig
}

// NewScheduler validates configuration and returns a scheduler ready for Run.
func NewScheduler(store LeaseStore, processor Processor, cfg SchedulerConfig) (*Scheduler, error) {
	if store == nil {
		return nil, errors.New("scheduler store is required")
	}
	if processor == nil {
		return nil, errors.New("scheduler processor is required")
	}
	if cfg.Owner == "" {
		return nil, errors.New("scheduler owner is required")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 30 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = cfg.LeaseDuration / 3
	}
	if cfg.HeartbeatInterval <= 0 {
		return nil, errors.New("scheduler heartbeat interval must be positive")
	}
	// Less than half a lease leaves room for a delayed database operation and
	// prevents an otherwise healthy worker from regularly losing ownership.
	if cfg.HeartbeatInterval >= cfg.LeaseDuration/2 {
		return nil, errors.New("scheduler heartbeat must be less than half the lease duration")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	if cfg.Maintenance != nil && cfg.MaintenanceInterval <= 0 {
		cfg.MaintenanceInterval = time.Minute
	}
	return &Scheduler{Store: store, Processor: processor, Config: cfg}, nil
}

// Run recovers expired work once, then runs workers until ctx is cancelled.
// Cancellation is a normal shutdown and returns nil; store or processor errors
// are returned to the daemon supervisor.
func (s *Scheduler) Run(ctx context.Context) error {
	if s == nil || s.Store == nil || s.Processor == nil {
		return errors.New("scheduler is not initialized")
	}
	if ctx.Err() != nil {
		return nil
	}
	if _, err := s.Store.RecoverStale(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("recover stale jobs: %w", err)
	}
	// This repairs historical terminal actions. It is deliberately best-effort:
	// queued work recovery remains available if cleanup is temporarily blocked.
	_ = s.Store.CloseStaleHumanActions(ctx)
	s.sweepTerminalQuarantine(ctx)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		s.maintenance(runCtx)
	}()

	errs := make(chan error, s.Config.Workers)
	var workers sync.WaitGroup
	for i := 0; i < s.Config.Workers; i++ {
		owner := fmt.Sprintf("%s-%s-%d", s.Config.Owner, job.NewID("run"), i+1)
		workers.Add(1)
		go func(owner string) {
			defer workers.Done()
			if err := s.worker(runCtx, owner); err != nil && (!errors.Is(err, context.Canceled) || runCtx.Err() == nil) {
				select {
				case errs <- err:
				case <-runCtx.Done():
				}
			}
		}(owner)
	}
	finished := make(chan struct{})
	go func() { workers.Wait(); close(finished) }()

	select {
	case <-ctx.Done():
		cancel()
		<-finished
		<-maintenanceDone
		return nil
	case err := <-errs:
		cancel()
		<-finished
		<-maintenanceDone
		return err
	case <-finished:
		cancel()
		<-maintenanceDone
		// A worker sends its error before it terminates, so a closed finished
		// channel may race with an already queued fatal error. Preserve the
		// causative failure rather than replacing it with a generic message.
		select {
		case err := <-errs:
			return err
		default:
		}
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("all scheduler workers stopped")
	}
}

func (s *Scheduler) maintenance(ctx context.Context) {
	if s.Config.Maintenance != nil {
		_ = s.Config.Maintenance.RunDue(ctx)
	}
	s.sweepTerminalQuarantine(ctx)
	if s.Config.Maintenance == nil {
		return
	}
	ticker := time.NewTicker(s.Config.MaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.Config.Maintenance.RunDue(ctx)
			s.sweepTerminalQuarantine(ctx)
		}
	}
}

// sweepTerminalQuarantine is best-effort maintenance. A cleanup failure must
// never stop acquisition or conceal a successfully recovered job.
func (s *Scheduler) sweepTerminalQuarantine(ctx context.Context) {
	sweeper, ok := s.Store.(terminalQuarantineSweeper)
	if !ok {
		return
	}
	_ = sweeper.SweepTerminalQuarantine(ctx)
}
func (s *Scheduler) worker(ctx context.Context, owner string) error {
	for {
		row, err := s.Store.ClaimNext(ctx, owner, s.Config.LeaseDuration)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("claim job: %w", err)
		}
		if row == nil {
			if err := waitContext(ctx, s.Config.PollInterval); err != nil {
				return err
			}
			continue
		}
		if err := s.processLease(ctx, row, owner); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

func (s *Scheduler) processLease(ctx context.Context, row *job.Row, owner string) error {
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	heartbeatErr := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(s.Config.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				if err := s.Store.Heartbeat(jobCtx, row.ID, owner, s.Config.LeaseDuration); err != nil {
					// Process completion cancels jobCtx while a heartbeat may be
					// in flight. Only cancellation-derived errors are normal here;
					// a store failure must still reach the daemon supervisor.
					if jobCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
						return
					}
					select {
					case heartbeatErr <- err:
					default:
					}
					cancelJob()
					return
				}
			}
		}
	}()

	processErr := s.Processor.Process(jobCtx, row)
	cancelJob()
	<-stopped
	select {
	case err := <-heartbeatErr:
		if processErr == nil || (errors.Is(processErr, context.Canceled) && ctx.Err() == nil) {
			processErr = fmt.Errorf("heartbeat job %s: %w", row.ID, err)
		}
	default:
	}
	if ctx.Err() == nil {
		releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		releaseErr := s.Store.Release(releaseCtx, row.ID, owner)
		cancelRelease()
		if processErr == nil && releaseErr != nil {
			processErr = fmt.Errorf("release job %s: %w", row.ID, releaseErr)
		}
	}
	return processErr
}

func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
