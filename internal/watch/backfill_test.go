package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/ownership"
	"papio/internal/ownershipsnapshot"
	"papio/internal/protocol"
	"papio/internal/store"
)

func backfillRegistry(t *testing.T, sources []config.LibrarySource) *ownership.Registry {
	t.Helper()
	providers := make([]ownership.Provider, 0, len(sources))
	for _, source := range sources {
		p, err := ownershipsnapshot.NewProvider(source, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		providers = append(providers, p)
	}
	return ownership.NewRegistry(providers...)
}

func TestGenericBackfillCompleteFeedsCapRestartAndChangedExport(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	dir := t.TempDir()
	records, pdfs := filepath.Join(dir, "records.bib"), filepath.Join(dir, "pdfs.bib")
	initial := "@article{one,title={First Work},doi={10.1000/one},file={not-a-proof.pdf}}\n@article{held,title={Held Work},doi={10.1000/held}}\n@misc{noid,title={No Identifier}}\n"
	if err := os.WriteFile(records, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pdfs, []byte("@article{held,doi={10.1000/held}}"), 0600); err != nil {
		t.Fatal(err)
	}
	sources := []config.LibrarySource{{Name: "records", Kind: config.LibraryKindFile, Path: records, Format: "bibtex", Claim: config.LibraryClaimRecordPresent}, {Name: "pdfs", Kind: config.LibraryKindFile, Path: pdfs, Format: "bibtex", Claim: config.LibraryClaimPDFPresent}}
	submitter := &fakeSubmitter{}
	r := &Runner{Store: s, Holdings: backfillRegistry(t, sources), LibrarySources: sources, Submitter: submitter}
	created, err := r.AddGenericBackfill(ctx, GenericBackfillInput{SourceName: "records", CadenceHours: 24, PerRunCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Run(ctx, created.Watch.ID)
	if err != nil || result.Queued != 1 {
		t.Fatalf("first run = %+v, %v", result, err)
	}
	if len(submitter.calls) != 1 || submitter.calls[0].Identifiers.DOI != "10.1000/one" || submitter.auto[0] == nil || *submitter.auto[0] {
		t.Fatalf("unexpected submission: %+v, %+v", submitter.calls, submitter.auto)
	}
	path := filepath.Dir(s.S.Path())
	if err := s.S.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	r = &Runner{Store: NewStore(reopened), Holdings: backfillRegistry(t, sources), LibrarySources: sources, Submitter: submitter}
	result, err = r.Run(ctx, created.Watch.ID)
	if err != nil || result.Queued != 0 {
		t.Fatalf("restart run = %+v, %v", result, err)
	}
	if err := os.WriteFile(records, []byte(initial+"@article{two,title={Second Work},doi={10.1000/two}}"), 0600); err != nil {
		t.Fatal(err)
	}
	r.Holdings = backfillRegistry(t, sources)
	result, err = r.Run(ctx, created.Watch.ID)
	if err != nil || result.Queued != 1 || len(submitter.calls) != 2 || submitter.calls[1].Identifiers.DOI != "10.1000/two" {
		t.Fatalf("changed feed = %+v, %v, submitted %+v", result, err, submitter.calls)
	}
	if err := os.WriteFile(pdfs, []byte("@article{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	r.Holdings = backfillRegistry(t, sources)
	if _, err := r.Run(ctx, created.Watch.ID); err == nil || len(submitter.calls) != 2 {
		t.Fatalf("malformed ownership feed did not fail closed: %v", err)
	}
	if err := os.WriteFile(records, []byte("@article{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(ctx, created.Watch.ID); err == nil || len(submitter.calls) != 2 {
		t.Fatalf("malformed record feed did not fail closed: %v", err)
	}
}

type lostReplySubmitter struct {
	service *app.Service
	calls   int
}

func (s *lostReplySubmitter) SubmitWithAutoImport(ctx context.Context, r protocol.WorkRequest, auto *bool) (string, error) {
	return s.service.SubmitWithAutoImport(ctx, r, auto)
}

func (s *lostReplySubmitter) SubmitOnceWithAutoImport(ctx context.Context, r protocol.WorkRequest, auto *bool) (string, error) {
	s.calls++
	id, err := s.service.SubmitOnceWithAutoImport(ctx, r, auto)
	if err != nil {
		return "", err
	}
	if s.calls == 1 {
		return "", errors.New("lost reply after durable job creation")
	}
	return id, nil
}

func (s *fakeSubmitter) SubmitOnceWithAutoImport(ctx context.Context, r protocol.WorkRequest, auto *bool) (string, error) {
	return s.SubmitWithAutoImport(ctx, r, auto)
}

func TestGenericBackfillRecoversLostReplyWithoutTerminalResubmission(t *testing.T) {
	ctx := context.Background()
	watches := testStore(t)
	path := filepath.Join(t.TempDir(), "records.bib")
	if err := os.WriteFile(path, []byte("@article{one,title={First Work},doi={10.1000/one}}"), 0600); err != nil {
		t.Fatal(err)
	}
	sources := []config.LibrarySource{{Name: "records", Kind: config.LibraryKindFile, Path: path, Format: "bibtex", Claim: config.LibraryClaimRecordPresent}}
	cfg := config.Default()
	cfg.AccessMode = "conservative"
	submitter := &lostReplySubmitter{service: &app.Service{Config: cfg, Jobs: &job.Store{S: watches.S}}}
	r := &Runner{Store: watches, Holdings: backfillRegistry(t, sources), LibrarySources: sources, Submitter: submitter}
	w, err := r.AddGenericBackfill(ctx, GenericBackfillInput{SourceName: "records", CadenceHours: 24, PerRunCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(ctx, w.Watch.ID); err == nil {
		t.Fatal("lost reply did not fail")
	}
	if _, err := watches.S.DB().ExecContext(ctx, `UPDATE jobs SET state='failed'`); err != nil {
		t.Fatal(err)
	}
	result, err := r.Run(ctx, w.Watch.ID)
	if err != nil || result.Queued != 1 || submitter.calls != 2 {
		t.Fatalf("recovery = %+v, %v, calls %d", result, err, submitter.calls)
	}
	result, err = r.Run(ctx, w.Watch.ID)
	if err != nil || result.Queued != 0 || submitter.calls != 2 {
		t.Fatalf("dedup = %+v, %v, calls %d", result, err, submitter.calls)
	}
	var count int
	if err := watches.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("once-only job count = %d, %v", count, err)
	}
}
