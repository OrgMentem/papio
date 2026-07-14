// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/config"
)

func TestNewWiresResolverOrderAndCoreServices(t *testing.T) {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.PDF.OCREnabled = false
	system, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := system.Close(); err != nil {
			t.Errorf("close system: %v", err)
		}
	})
	var names []string
	for _, entry := range system.App.Resolvers {
		if entry.Adapter == nil {
			t.Fatal("nil resolver adapter")
		}
		names = append(names, entry.Adapter.Name())
	}
	want := []string{
		config.SourceArXiv,
		config.SourceEuropePMC,
		config.SourceUnpaywall,
		config.SourceOpenAlex,
		config.SourceCORE,
		config.SourceCrossrefTDM,
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("resolver order = %v, want %v", names, want)
	}
	if system.App.Fetch == nil || system.App.Validate == nil || system.Scheduler == nil || system.Bundle == nil {
		t.Fatal("bootstrap left a core service unwired")
	}
	if system.PDFCapability.PDFToPPM != "" || system.PDFCapability.Tesseract != "" {
		t.Fatal("OCR helpers remained enabled when pdf.ocr_enabled=false")
	}
}

type autoImporterFunc func(context.Context, string) (string, string, string, error)

func (f autoImporterFunc) PlanAndApply(ctx context.Context, jobID string) (string, string, string, error) {
	return f(ctx, jobID)
}

func TestSerialAutoImporterSerializesConcurrentCalls(t *testing.T) {
	var active, maxActive, calls atomic.Int32
	importer := autoImporterFunc(func(context.Context, string) (string, string, string, error) {
		calls.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for {
			maximum := maxActive.Load()
			if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		if current > 1 {
			return "failed", "", "", errors.New("concurrent call")
		}
		return "attached", "parent", "attachment", nil
	})
	serial := newSerialAutoImporter(importer)

	const workers = 20
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, _, _, err := serial.PlanAndApply(context.Background(), "job")
			if err != nil {
				errs <- err
				return
			}
			if status != "attached" {
				errs <- errors.New("unexpected status")
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maximum concurrent calls = %d, want 1", got)
	}
	if got := calls.Load(); got != workers {
		t.Fatalf("calls = %d, want %d", got, workers)
	}
}

func TestSerialAutoImporterRetriesOnce(t *testing.T) {
	var calls atomic.Int32
	importer := autoImporterFunc(func(context.Context, string) (string, string, string, error) {
		if calls.Add(1) == 1 {
			return "failed", "", "", errors.New("temporary failure")
		}
		return "attached", "parent", "attachment", nil
	})
	serial := newSerialAutoImporter(importer)
	serial.backoff = time.Millisecond

	status, parentKey, attachmentKey, err := serial.PlanAndApply(context.Background(), "job")
	if err != nil {
		t.Fatal(err)
	}
	if status != "attached" || parentKey != "parent" || attachmentKey != "attachment" {
		t.Fatalf("result = (%q, %q, %q), want attached result", status, parentKey, attachmentKey)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestSerialAutoImporterStopsRetryWhenContextCancelled(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	var calls atomic.Int32
	importer := autoImporterFunc(func(context.Context, string) (string, string, string, error) {
		calls.Add(1)
		startedOnce.Do(func() { close(started) })
		return "failed", "", "", errors.New("temporary failure")
	})
	serial := newSerialAutoImporter(importer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, _, err := serial.PlanAndApply(ctx, "job")
		result <- err
	}()

	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry backoff did not stop after context cancellation")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}
