// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package filing

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// ErrDestinationStalled reports that managed-folder I/O stopped making progress
// or that an earlier stalled call still holds the folder. Fail closed: nothing
// was published, and the caller retries on its normal schedule.
var ErrDestinationStalled = errors.New("destination_stalled")

// StallDeadline is how long managed-folder I/O may go without progress before
// the caller stops waiting. The destination is optional and may sit behind a
// macOS TCC consent wall or a hard-mounted network share, where open(2),
// write(2) and fsync(2) block in the kernel and never observe a context (the
// AGENTS.md TCC footgun). Progress, not total time, is measured: every chunk
// of a copy is progress, so a large PDF on a slow disk is not a stall. It is a
// var only so tests can compress it.
var StallDeadline = 15 * time.Second

// beforeIO is a test seam called on the worker goroutine before each blocking
// filesystem step. A test blocks here, ignoring the context, to reproduce a
// wedged destination.
var beforeIO = func(string) {}

var (
	gate    = make(chan struct{}, 1)
	stalled atomic.Bool
)

// Stalled reports whether a managed-folder call was abandoned and has not yet
// returned. While true every call fails fast, so at most one goroutine is ever
// stuck in the filesystem however many retries are due.
func Stalled() bool { return stalled.Load() }

// watch records the last sign of life from the worker.
type watch struct{ last atomic.Int64 }

func (w *watch) touch() { w.last.Store(time.Now().UnixNano()) }

// run executes fn on its own goroutine, one at a time process-wide, and stops
// waiting when ctx ends or fn makes no progress for StallDeadline. Go cannot
// cancel a syscall blocked in the kernel, so an abandoned fn keeps running
// and keeps the gate (the single-flight latch) until it returns; abandoned is
// then called once, on that goroutine, with fn's late result so it can undo
// anything the caller no longer expects to exist.
func run[T any](ctx context.Context, fn func(context.Context, *watch) (T, error), abandoned func(T, error)) (T, error) {
	var zero T
	if stalled.Load() {
		return zero, ErrDestinationStalled
	}
	wait := time.NewTimer(StallDeadline)
	select {
	case gate <- struct{}{}:
		wait.Stop()
	case <-ctx.Done():
		wait.Stop()
		return zero, ctx.Err()
	case <-wait.C:
		return zero, ErrDestinationStalled
	}

	type result struct {
		value T
		err   error
	}
	workerCtx, cancel := context.WithCancel(ctx)
	w := &watch{}
	w.touch()
	done := make(chan result, 1)
	var mu sync.Mutex
	var finished, left bool
	go func() {
		value, err := fn(workerCtx, w)
		mu.Lock()
		if left {
			mu.Unlock()
			if abandoned != nil {
				abandoned(value, err)
			}
			// Only now may another call enter: the abandoned one has undone
			// whatever the caller no longer expects to exist.
			stalled.Store(false)
			log.Printf("papio: managed folder responding again")
			<-gate
			return
		}
		finished = true
		done <- result{value, err}
		mu.Unlock()
		<-gate
	}()
	leave := func() (result, bool) {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			cancel()
			return <-done, true
		}
		left = true
		stalled.Store(true)
		cancel()
		return result{}, false
	}

	timer := time.NewTimer(StallDeadline)
	defer timer.Stop()
	for {
		select {
		case r := <-done:
			cancel()
			return r.value, r.err
		case <-ctx.Done():
			if r, ok := leave(); ok {
				return r.value, r.err
			}
			return zero, ctx.Err()
		case <-timer.C:
			idle := time.Since(time.Unix(0, w.last.Load()))
			if idle < StallDeadline {
				timer.Reset(StallDeadline - idle)
				continue
			}
			if r, ok := leave(); ok {
				return r.value, r.err
			}
			log.Printf("papio: managed folder filing suspended: destination not responding (macOS privacy consent or a stalled mount?)")
			return zero, ErrDestinationStalled
		}
	}
}
