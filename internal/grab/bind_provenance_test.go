// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package grab

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"papio/internal/store"
	"papio/internal/store/storetest"
)

func TestMarkBoundToJobLegalStates(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(context.Context, *Service, *store.Store) string
	}{
		{
			name: "awaiting_file",
			setup: func(ctx context.Context, svc *Service, s *store.Store) string {
				g, err := svc.Allocate(ctx, "bind.example.org", "awaiting_file case")
				if err != nil {
					t.Fatalf("Allocate: %v", err)
				}
				return g.ID
			},
		},
		{
			name: "quarantined",
			setup: func(ctx context.Context, svc *Service, s *store.Store) string {
				g, err := svc.Allocate(ctx, "bind.example.org", "quarantined case")
				if err != nil {
					t.Fatalf("Allocate: %v", err)
				}
				if err := svc.MarkQuarantined(ctx, g.ID, "/tmp/q/"+g.ID); err != nil {
					t.Fatalf("MarkQuarantined: %v", err)
				}
				return g.ID
			},
		},
		{
			name: "identified",
			setup: func(ctx context.Context, svc *Service, s *store.Store) string {
				g, err := svc.Allocate(ctx, "bind.example.org", "identified case")
				if err != nil {
					t.Fatalf("Allocate: %v", err)
				}
				if err := svc.MarkQuarantined(ctx, g.ID, "/tmp/q/"+g.ID); err != nil {
					t.Fatalf("MarkQuarantined: %v", err)
				}
				if err := svc.MarkIdentified(ctx, g.ID); err != nil {
					t.Fatalf("MarkIdentified: %v", err)
				}
				return g.ID
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.Open(ctx, storetest.DataDir(t))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			svc := New(s, nil)
			now := store.Now()
			const jobID = "job_00000000000000000000000011"
			if _, err := s.DB().ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, "req-bind-"+tc.name, now); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
				jobID, "req-bind-"+tc.name, "awaiting_human", `{}`, now, now); err != nil {
				t.Fatal(err)
			}
			id := tc.setup(ctx, svc, s)
			prov := BindProvenance{
				Method:               "candidate_auto_bind",
				Rule:                 "candidate_auto_bind/1",
				Winner:               jobID,
				CandidatesConsidered: 3,
				Evidence:             []string{"title_printed", "year_compatible"},
			}
			if err := svc.MarkBoundToJob(ctx, id, jobID, "job_created", prov); err != nil {
				t.Fatalf("MarkBoundToJob: %v", err)
			}
			got, err := svc.Get(ctx, id)
			if err != nil || got == nil {
				t.Fatalf("Get after bind: %v %v", got, err)
			}
			if got.State != StateJobCreated {
				t.Fatalf("state = %q, want job_created", got.State)
			}
			if got.JobID != jobID {
				t.Fatalf("job_id = %q, want %q", got.JobID, jobID)
			}
			if got.Outcome != "job_created" {
				t.Fatalf("outcome = %q, want job_created", got.Outcome)
			}
			if got.BindProvenance == "" {
				t.Fatal("bind_provenance empty, want JSON")
			}
			var decoded BindProvenance
			if err := json.Unmarshal([]byte(got.BindProvenance), &decoded); err != nil {
				t.Fatalf("unmarshal provenance: %v", err)
			}
			if decoded.Method != prov.Method || decoded.Rule != prov.Rule || decoded.Winner != prov.Winner || decoded.CandidatesConsidered != prov.CandidatesConsidered {
				t.Fatalf("provenance mismatch got %+v want %+v", decoded, prov)
			}
			if len(decoded.Evidence) != len(prov.Evidence) {
				t.Fatalf("evidence len = %d, want %d", len(decoded.Evidence), len(prov.Evidence))
			}
			// Verify column is not NULL.
			var raw sql.NullString
			if err := s.DB().QueryRowContext(ctx, `SELECT bind_provenance FROM pdf_grabs WHERE id=?`, id).Scan(&raw); err != nil {
				t.Fatalf("raw provenance query: %v", err)
			}
			if !raw.Valid {
				t.Fatal("bind_provenance is NULL, want JSON")
			}
		})
	}
}

func TestMarkBoundToJobIllegalStates(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	now := store.Now()
	const jobID = "job_00000000000000000000000022"
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, "req-illegal", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		jobID, "req-illegal", "awaiting_human", `{}`, now, now); err != nil {
		t.Fatal(err)
	}
	prov := BindProvenance{Method: "candidate_auto_bind", Rule: "candidate_auto_bind/1", Winner: jobID, CandidatesConsidered: 1}

	// Already job_created.
	g1, err := svc.Allocate(ctx, "illegal.example.org", "already job_created")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkJobCreated(ctx, g1.ID, jobID, "job_created"); err != nil {
		t.Fatal(err)
	}
	err = svc.MarkBoundToJob(ctx, g1.ID, jobID, "job_created", prov)
	if err == nil || !strings.Contains(err.Error(), "changed underneath its own transition") {
		t.Fatalf("MarkBoundToJob on job_created err = %v, want changed underneath", err)
	}
	got, _ := svc.Get(ctx, g1.ID)
	if got.State != StateJobCreated || got.JobID != jobID {
		t.Fatalf("row changed after failed transition: %+v", got)
	}
	var provRaw sql.NullString
	if err := s.DB().QueryRowContext(ctx, `SELECT bind_provenance FROM pdf_grabs WHERE id=?`, g1.ID).Scan(&provRaw); err != nil {
		t.Fatal(err)
	}
	if provRaw.Valid {
		t.Fatalf("bind_provenance = %q, want NULL after illegal transition", provRaw.String)
	}

	// Abandoned.
	g2, err := svc.Allocate(ctx, "illegal.example.org", "abandoned case")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkAbandoned(ctx, g2.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	err = svc.MarkBoundToJob(ctx, g2.ID, jobID, "job_created", prov)
	if err == nil || !strings.Contains(err.Error(), "changed underneath its own transition") {
		t.Fatalf("MarkBoundToJob on abandoned err = %v, want changed underneath", err)
	}
	got2, _ := svc.Get(ctx, g2.ID)
	if got2.State != StateAbandoned {
		t.Fatalf("abandoned row changed: %+v", got2)
	}
}

func TestMarkBoundToJobSettlesPermit(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	now := store.Now()
	const jobID = "job_00000000000000000000000033"
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, "req-permit", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		jobID, "req-permit", "awaiting_human", `{}`, now, now); err != nil {
		t.Fatal(err)
	}
	g, err := svc.AllocateEffect(ctx, "permit.example.org", "Paper", 9, "pdf_grab:permit.example.org", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("AllocateEffect: %v", err)
	}
	prov := BindProvenance{Method: "candidate_auto_bind", Rule: "candidate_auto_bind/1", Winner: jobID, CandidatesConsidered: 1, Evidence: []string{"title_printed"}}
	if err := svc.MarkBoundToJob(ctx, g.ID, jobID, "job_created", prov); err != nil {
		t.Fatalf("MarkBoundToJob: %v", err)
	}
	var status string
	if err := s.DB().QueryRowContext(ctx, `SELECT status FROM effect_permits WHERE grab_id=?`, g.ID).Scan(&status); err != nil {
		t.Fatalf("permit lookup: %v", err)
	}
	if status != "settled" {
		t.Fatalf("permit status = %q, want settled", status)
	}
	got, _ := svc.Get(ctx, g.ID)
	if got.State != StateJobCreated {
		t.Fatalf("state = %q, want job_created", got.State)
	}
}

func TestMarkBoundToJobValidation(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)
	now := store.Now()
	const jobID = "job_00000000000000000000000044"
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, "req-validation", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		jobID, "req-validation", "awaiting_human", `{}`, now, now); err != nil {
		t.Fatal(err)
	}
	validProv := BindProvenance{Method: "candidate_auto_bind", Rule: "candidate_auto_bind/1", Winner: jobID, CandidatesConsidered: 1}

	cases := []struct {
		name   string
		jobID  string
		prov   BindProvenance
		errSub string
	}{
		{"blank job id", "", validProv, "job id is required"},
		{"blank method", jobID, BindProvenance{Method: "", Rule: "candidate_auto_bind/1", Winner: jobID}, "binding method is required"},
		{"blank rule", jobID, BindProvenance{Method: "candidate_auto_bind", Rule: "", Winner: jobID}, "binding rule is required"},
		{"blank method whitespace", jobID, BindProvenance{Method: "   ", Rule: "candidate_auto_bind/1", Winner: jobID}, "binding method is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := svc.Allocate(ctx, "validation.example.org", tc.name)
			if err != nil {
				t.Fatal(err)
			}
			// Ensure a held permit exists so we can verify it is not settled on validation failure.
			// Use AllocateEffect for one case, but Allocate is sufficient to check row untouched.
			err = svc.MarkBoundToJob(ctx, g.ID, tc.jobID, "job_created", tc.prov)
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("MarkBoundToJob err = %v, want containing %q", err, tc.errSub)
			}
			got, _ := svc.Get(ctx, g.ID)
			if got.State != StateAwaitingFile {
				t.Fatalf("state after validation failure = %q, want awaiting_file", got.State)
			}
			if got.JobID != "" {
				t.Fatalf("job_id after validation failure = %q, want empty", got.JobID)
			}
			var raw sql.NullString
			if err := s.DB().QueryRowContext(ctx, `SELECT bind_provenance FROM pdf_grabs WHERE id=?`, g.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if raw.Valid {
				t.Fatalf("bind_provenance = %q, want NULL after validation failure", raw.String)
			}
		})
	}
}

func TestMarkBoundToJobNullProvenance(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := New(s, nil)

	// A freshly allocated grab has NULL provenance and reads back as empty.
	g, err := svc.Allocate(ctx, "null.example.org", "null provenance")
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v %v", got, err)
	}
	if got.BindProvenance != "" {
		t.Fatalf("BindProvenance = %q, want empty for NULL column", got.BindProvenance)
	}
	var raw sql.NullString
	if err := s.DB().QueryRowContext(ctx, `SELECT bind_provenance FROM pdf_grabs WHERE id=?`, g.ID).Scan(&raw); err != nil {
		t.Fatalf("raw query: %v", err)
	}
	if raw.Valid {
		t.Fatalf("bind_provenance valid = %v, want NULL", raw.Valid)
	}

	// MarkJobCreated does not set provenance — still NULL.
	now := store.Now()
	const jobID = "job_00000000000000000000000055"
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO work_requests(id,created_at) VALUES(?,?)`, "req-null-2", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO jobs(id,work_request_id,state,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		jobID, "req-null-2", "awaiting_human", `{}`, now, now); err != nil {
		t.Fatal(err)
	}
	g2, err := svc.Allocate(ctx, "null.example.org", "null after job_created")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkJobCreated(ctx, g2.ID, jobID, "job_created"); err != nil {
		t.Fatalf("MarkJobCreated: %v", err)
	}
	got2, _ := svc.Get(ctx, g2.ID)
	if got2.BindProvenance != "" {
		t.Fatalf("BindProvenance after MarkJobCreated = %q, want empty", got2.BindProvenance)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT bind_provenance FROM pdf_grabs WHERE id=?`, g2.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw.Valid {
		t.Fatalf("bind_provenance after MarkJobCreated valid, want NULL")
	}

	// A zero-value BindProvenance passed to MarkBoundToJob is rejected by
	// validation (blank method/rule) and leaves the row untouched with NULL.
	// This verifies the nullable convention: empty provenance never writes "{}".
	g3, err := svc.Allocate(ctx, "null.example.org", "zero provenance attempt")
	if err != nil {
		t.Fatal(err)
	}
	err = svc.MarkBoundToJob(ctx, g3.ID, jobID, "job_created", BindProvenance{})
	if err == nil || !strings.Contains(err.Error(), "binding method is required") {
		t.Fatalf("zero provenance err = %v, want binding method is required", err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT bind_provenance FROM pdf_grabs WHERE id=?`, g3.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw.Valid {
		t.Fatalf("bind_provenance after zero-value attempt = %q, want NULL", raw.String)
	}
	got3, _ := svc.Get(ctx, g3.ID)
	if got3.BindProvenance != "" {
		t.Fatalf("BindProvenance after zero-value attempt = %q, want empty", got3.BindProvenance)
	}
}
