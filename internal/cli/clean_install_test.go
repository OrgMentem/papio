// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/doctor"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/work"
)

type cleanInstallResolver struct{ calls int }

func (r *cleanInstallResolver) Name() string { return "clean-install-stub" }

func (r *cleanInstallResolver) Resolve(context.Context, work.Work) ([]resolver.Candidate, error) {
	r.calls++
	return nil, nil
}

// TestCleanInstallBootstrapsAndAcceptsWork exercises the production bootstrap
// dependency used by `papio init --non-interactive` from an otherwise empty
// HOME/XDG profile. Network-backed dependencies are replaced only after the
// bootstrap is open, with a resolver that deliberately returns no candidates.
func TestCleanInstallBootstrapsAndAcceptsWork(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	configPath := filepath.Join(home, ".config", "papio", "config.toml")
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("fresh profile config stat = %v, want not exist", err)
	}

	var doctorReport doctor.Report
	deps := initDependencies{
		Bootstrap: func(ctx context.Context, cfg config.Config) (io.Closer, error) {
			return bootstrap.New(ctx, cfg)
		},
		CheckZotio: func(context.Context, string) error { return nil },
		InstallNative: func(config.Config) error {
			t.Fatal("native installer ran despite --skip-browser")
			return nil
		},
		RunDoctor: func(ctx context.Context, opt *options) (doctor.Report, error) {
			cfg, err := config.Load(opt.configPath)
			if err != nil {
				return doctor.Report{}, err
			}
			system, err := bootstrap.New(ctx, cfg)
			if err != nil {
				return doctor.Report{}, err
			}
			defer system.Close()
			worker, err := os.Executable()
			if err != nil {
				return doctor.Report{}, err
			}
			// These executable paths are a hermetic capability probe: doctor only
			// verifies readiness here; acquisition does not invoke them in this test.
			doctorReport = doctor.Run(ctx, cfg, system.Store, pdf.Capability{
				PDFToText: worker,
				PDFInfo:   worker,
				PDFToPPM:  worker,
				Tesseract: worker,
			}, worker)
			return doctorReport, nil
		},
	}

	var out, errOut bytes.Buffer
	opt := &options{out: &out, errOut: &errOut}
	command := newInitCommandWithDependencies(opt, deps)
	command.SetOut(&out)
	command.SetErr(&errOut)
	command.SetArgs([]string{"--non-interactive", "--email", "reader@example.test", "--skip-browser"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("init --non-interactive: %v\n%s", err, out.String())
	}
	output := out.String()
	if !doctorReport.OK {
		t.Fatalf("fresh-profile doctor report is unhealthy: %+v", doctorReport)
	}
	if !strings.Contains(output, "SQLite integrity ok; schema version 11") {
		t.Fatalf("init doctor output does not report schema 11:\n%s", output)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load fresh config: %v", err)
	}
	if cfg.AccessMode != config.ModeConservative || cfg.Email != "reader@example.test" {
		t.Fatalf("fresh config = %+v, want conservative mode and supplied email", cfg)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "papio.db")); err != nil {
		t.Fatalf("fresh bootstrap did not create database: %v", err)
	}

	system, err := bootstrap.New(ctx, cfg)
	if err != nil {
		t.Fatalf("reopen daemon bootstrap: %v", err)
	}
	defer system.Close()
	if system.Scheduler == nil {
		t.Fatal("daemon bootstrap did not construct a scheduler")
	}
	version, err := system.Store.UserVersion(ctx)
	if err != nil || version != 11 {
		t.Fatalf("fresh schema version = %d, %v; want 11", version, err)
	}

	stub := &cleanInstallResolver{}
	system.App.Resolvers = []app.ResolverEntry{{Adapter: stub, Policy: config.Source{Enabled: true}}}
	jobID, err := system.App.Submit(ctx, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     "clean-install-request-0001",
		Identifiers:   &protocol.Identifiers{DOI: "10.1000/clean-install"},
	})
	if err != nil {
		t.Fatalf("submit on clean install: %v", err)
	}
	row, err := system.Jobs.ClaimNext(ctx, "clean-install-test", time.Minute)
	if err != nil || row == nil || row.ID != jobID {
		t.Fatalf("claim submitted work = %+v, %v; want %s", row, err, jobID)
	}
	if err := system.App.Process(ctx, row); err != nil {
		t.Fatalf("process stubbed work: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("stub resolver calls = %d, want 1", stub.calls)
	}
	events, err := system.Jobs.Events(ctx, jobID)
	if err != nil {
		t.Fatalf("read work events: %v", err)
	}
	resolved := false
	for _, event := range events {
		detail, _ := event["detail"].(map[string]any)
		if event["kind"] == "job.transition" && detail["from"] == "queued" && detail["to"] == "resolving" {
			resolved = true
			break
		}
	}
	if !resolved {
		t.Fatalf("submitted work never reached resolving: %#v", events)
	}
}
