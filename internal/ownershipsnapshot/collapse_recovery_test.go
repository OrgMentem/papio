// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package ownershipsnapshot

import (
	"context"
	"os"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/ownership"
)

func TestHoldingsCollapseRecoveryWindow(t *testing.T) {
	for _, kind := range []string{config.LibraryKindFile, config.LibraryKindCommand} {
		for _, reducedCount := range []int{0, 1} {
			for _, restored := range []bool{false, true} {
				name := kind + "/empty"
				if reducedCount != 0 {
					name = kind + "/reduced"
				}
				if restored {
					name += "/transient"
				}
				t.Run(name, func(t *testing.T) {
					dataDir := t.TempDir()
					path := writeFile(t, t.TempDir(), "library.bib", "")
					source := fileSource(path, "bibtex")
					setData := func(body string) {
						t.Helper()
						if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					if kind == config.LibraryKindCommand {
						fixture := newCommandFixture(t)
						source.Kind, source.Path = kind, ""
						source.Argv = []string{"/bin/sh", fixture.script}
						setData = fixture.setData
					}
					start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
					check := func(hour, count int, complete bool) {
						t.Helper()
						setData(sourceRecordsWithOneDOI(count))
						now := start.Add(time.Duration(hour) * time.Hour)
						// Reconstruct on every observation: both the old baseline and
						// the confirmation window must survive a process restart.
						provider, err := NewProviderWithDataDir(source, func() time.Time { return now }, dataDir)
						if err != nil {
							t.Fatal(err)
						}
						_, health := provider.Lookup(context.Background(), nil)
						if err := provider.(interface{ Close() error }).Close(); err != nil {
							t.Fatal(err)
						}
						if health.Complete != complete {
							t.Fatalf("hour %d health = %+v, want complete=%v", hour, health, complete)
						}
						if !complete && health.FailureCode != ownership.FailureCountCollapse {
							t.Fatalf("hour %d failure = %q, want count_collapse", hour, health.FailureCode)
						}
						if complete && (health.EntryCount != count || health.FailureCode != "") {
							t.Fatalf("hour %d health = %+v, want %d complete entries", hour, health, count)
						}
					}
					check(0, 100, true)
					check(1, reducedCount, false)
					check(25, reducedCount, false)
					if restored {
						check(49, 100, true)
						check(73, reducedCount, false)
						check(97, reducedCount, false)
						check(121, reducedCount, false)
						check(145, reducedCount, true)
					} else {
						check(49, reducedCount, false)
						check(73, reducedCount, true)
						check(97, reducedCount, true)
					}
				})
			}
		}
	}
}

func TestCollapseObservationResetsForDifferentCount(t *testing.T) {
	var observation CollapseObservation
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if observation.Accept(0, now) || observation.Accept(1, now.Add(72*time.Hour)) {
		t.Fatal("a different collapsed count must start a new confirmation window")
	}
	if !observation.Accept(1, now.Add(144*time.Hour)) {
		t.Fatal("a stable collapsed count must recover after 72 hours")
	}
}
