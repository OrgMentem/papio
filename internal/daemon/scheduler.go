package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
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

// publicationRecoverer restores durable publication work before stale-job
// recovery can rewind its transition state or quarantine cleanup can remove
// journal-owned bytes.
type publicationRecoverer interface {
	RecoverPreparedPublications(context.Context) error
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

// MaintenanceRunners runs several maintenance runners in order on each pass, so
// one daemon composes independent periodic tasks (watchlists, import retry)
// behind the scheduler's single Maintenance seam. Every runner runs even if an
// earlier one errors; the first error is returned for the caller, though
// maintenance errors are best-effort and never stop acquisition workers.
type MaintenanceRunners []MaintenanceRunner

// RunDue runs each non-nil runner once and returns the first error, if any.
func (rs MaintenanceRunners) RunDue(ctx context.Context) error {
	names := rs.runnerNames()
	errs := make(map[string]error, len(names))
	ordered := make([]string, 0, len(names))
	i := 0
	for _, r := range rs {
		if r == nil {
			continue
		}
		name := names[i]
		ordered = append(ordered, name)
		errs[name] = r.RunDue(ctx)
		i++
	}
	for _, name := range ordered {
		if err := errs[name]; err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// RunDueAll runs each non-nil runner once and reports every runner's outcome
// by name, so operators can see which maintenance keeps failing without
// stopping acquisition workers. Nil runners are skipped; a nil runner would
// report an empty name and hide the failure.
func (rs MaintenanceRunners) RunDueAll(ctx context.Context) map[string]error {
	names := rs.runnerNames()
	errs := make(map[string]error, len(names))
	i := 0
	for _, r := range rs {
		if r == nil {
			continue
		}
		errs[names[i]] = r.RunDue(ctx)
		i++
	}
	return errs
}

// runnerNames lists the disambiguated diagnostic name of each non-nil runner
// in order, so RunDue and RunDueAll agree on identity.
func (rs MaintenanceRunners) runnerNames() []string {
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		if r == nil {
			continue
		}
		names = append(names, runnerName(r))
	}
	return disambiguateRunnerNames(names)
}

// runnerName names one maintenance runner for diagnostics. Pointer receivers
// report their concrete type; disambiguateRunnerNames makes repeats unique.
func runnerName(r MaintenanceRunner) string {
	name := fmt.Sprintf("%T", r)
	if name == "" || name == "<nil>" {
		return "unknown"
	}
	return name
}

// disambiguateRunnerNames appends a "#n" suffix to a repeated runner type so a
// name always identifies exactly one runner in one pass.
func disambiguateRunnerNames(names []string) []string {
	seen := make(map[string]int, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		seen[name]++
		if seen[name] == 1 {
			out = append(out, name)
			continue
		}
		out = append(out, fmt.Sprintf("%s#%d", name, seen[name]))
	}
	return out
}

// BackgroundRunner is a long-lived best-effort loop that runs beside
// maintenance, such as a watcher that sleeps until an external event. Run
// must return once ctx ends; the scheduler waits for it before Run returns,
// so anything it started is gone by then.
type BackgroundRunner interface {
	Run(context.Context)
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
	// Background loops start with maintenance and stop with it.
	Background []BackgroundRunner
}

// Scheduler claims durable jobs and processes them while renewing their lease.
type Scheduler struct {
	Store     LeaseStore
	Processor Processor
	Config    SchedulerConfig

	maintenanceMu  sync.Mutex
	maintenanceErr map[string]error
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
	if recovery, ok := s.Processor.(publicationRecoverer); ok {
		if err := recovery.RecoverPreparedPublications(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("recover prepared publications: %w", err)
		}
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
		var background sync.WaitGroup
		for _, runner := range s.Config.Background {
			if runner == nil {
				continue
			}
			background.Add(1)
			go func() {
				defer background.Done()
				runner.Run(runCtx)
			}()
		}
		s.maintenance(runCtx)
		background.Wait()
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
		s.runMaintenancePass(ctx)
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
			s.runMaintenancePass(ctx)
			s.sweepTerminalQuarantine(ctx)
		}
	}
}

// runMaintenancePass runs one maintenance pass and remembers per-runner
// failures for diagnostics. Failures are best-effort: they are logged and
// surfaced through MaintenanceFailures/Status, never returned to stop
// acquisition workers.
func (s *Scheduler) runMaintenancePass(ctx context.Context) {
	multi, ok := s.Config.Maintenance.(MaintenanceRunners)
	if !ok {
		if err := s.Config.Maintenance.RunDue(ctx); err != nil {
			log.Printf("papio: maintenance failed: %v", err)
			s.setMaintenanceFailure("maintenance", err)
		} else {
			s.clearMaintenanceFailures()
		}
		return
	}
	errs := multi.RunDueAll(ctx)
	failed := false
	for _, name := range multi.runnerNames() {
		if err := errs[name]; err != nil {
			failed = true
			log.Printf("papio: maintenance %s failed: %v", name, err)
			s.setMaintenanceFailure(name, err)
		} else {
			s.clearMaintenanceFailure(name)
		}
	}
	if !failed {
		s.clearMaintenanceFailures()
	}
}

// setMaintenanceFailure remembers one runner's latest maintenance failure.
func (s *Scheduler) setMaintenanceFailure(name string, err error) {
	if s == nil || err == nil {
		return
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if s.maintenanceErr == nil {
		s.maintenanceErr = make(map[string]error)
	}
	s.maintenanceErr[name] = err
}

// clearMaintenanceFailure drops one runner's remembered failure after it
// succeeds, so a recovered runner stops looking degraded while a still-failing
// sibling stays named.
func (s *Scheduler) clearMaintenanceFailure(name string) {
	if s == nil {
		return
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	delete(s.maintenanceErr, name)
}

// clearMaintenanceFailures drops every remembered maintenance failure after a
// clean pass. It runs on success, so a recovered runner stops looking
// degraded.
func (s *Scheduler) clearMaintenanceFailures() {
	if s == nil {
		return
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	s.maintenanceErr = nil
}

// MaintenanceFailures reports the latest failure per maintenance runner since
// the last clean pass. An empty map means the last pass was clean or no pass
// has run yet.
func (s *Scheduler) MaintenanceFailures() map[string]error {
	if s == nil {
		return nil
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if len(s.maintenanceErr) == 0 {
		return nil
	}
	out := make(map[string]error, len(s.maintenanceErr))
	for name, err := range s.maintenanceErr {
		out[name] = err
	}
	return out
}

// MaintenanceStatus summarizes maintenance health for daemon diagnostics: one
// line per failing runner, in name order for stable output. Empty means the
// last pass was clean. Main owns the bootstrap/doctor seam; this is the data
// it can expose: runner names come from runnerName (%T per runner, "#2" for
// repeats) and per-runner errors persist until a clean pass clears them.
func (s *Scheduler) MaintenanceStatus() string {
	failures := s.MaintenanceFailures()
	if len(failures) == 0 {
		return ""
	}
	names := make([]string, 0, len(failures))
	for name := range failures {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("maintenance %s failed: %v", name, failures[name]))
	}
	return "degraded: " + joinLines(lines)
}

// joinLines joins status lines without pulling in strings for one call.
func joinLines(lines []string) string {
	out := ""
	for i, line := range lines {
		if i > 0 {
			out += "; "
		}
		out += line
	}
	return out
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
