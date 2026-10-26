// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package ownershipsnapshot

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

func TestEnumerateWorkerClosesFile(t *testing.T) {
	for _, abandoned := range []bool{false, true} {
		name := "complete"
		if abandoned {
			name = "abandoned"
		}
		t.Run(name, func(t *testing.T) {
			path := writeExport(t, "library.bib", oneEntryBibTeX("10.1000/one"))
			oldOpen, oldRead := enumerateOpen, enumerateRead
			var opened *os.File
			started, release := make(chan struct{}), make(chan struct{})
			enumerateOpen = func(path string) (*os.File, error) {
				file, err := openSource(path)
				opened = file
				return file, err
			}
			enumerateRead = func(ctx context.Context, reader io.Reader) ([]byte, error) {
				close(started)
				<-release
				return readBounded(ctx, reader)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := EnumerateBibliographicRecords(ctx, fileSource(path, "bibtex"))
				result <- err
			}()
			<-started
			enumMu.Lock()
			done := enumFlights[path]
			enumMu.Unlock()
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				<-done
				enumerateOpen, enumerateRead = oldOpen, oldRead
			})
			if abandoned {
				cancel()
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("abandoned enumeration = %v, want context.Canceled", err)
				}
			}
			close(release)
			<-done
			if !abandoned {
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			}
			if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("worker descriptor Stat = %v, want os.ErrClosed", err)
			}
		})
	}
}
