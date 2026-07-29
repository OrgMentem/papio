// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"fmt"
	"testing"

	"papio/internal/job"
	"papio/internal/protocol"
)

func TestSubmitWithOptionsAsPersistsPrincipal(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	for i, principal := range []job.Principal{job.PrincipalCLI, job.PrincipalMCP} {
		requestID := fmt.Sprintf("wr_submit_principal_%d", i)
		result, err := svc.SubmitWithOptionsAs(ctx, principal, protocol.WorkRequest{
			SchemaVersion: protocol.WorkRequestSchemaVersion,
			RequestID:     requestID,
			Identifiers:   &protocol.Identifiers{DOI: fmt.Sprintf("10.1000/principal-%d", i)},
		}, SubmitOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var requester string
		if err := jobs.S.DB().QueryRowContext(ctx, `SELECT requester FROM work_requests WHERE id = ?`, requestID).Scan(&requester); err != nil {
			t.Fatal(err)
		}
		if requester != string(principal) {
			t.Errorf("requester for %s = %q, want %q", result.JobID, requester, principal)
		}
	}
}

func TestSubmitWithoutPrincipalDoesNotAssumeCLI(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	const requestID = "wr_submit_principal_default"
	if _, err := svc.Submit(ctx, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     requestID,
		Identifiers:   &protocol.Identifiers{DOI: "10.1000/principal-default"},
	}); err != nil {
		t.Fatal(err)
	}
	var requester string
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT requester FROM work_requests WHERE id = ?`, requestID).Scan(&requester); err != nil {
		t.Fatal(err)
	}
	if requester != string(job.PrincipalUnknown) {
		t.Fatalf("default requester = %q, want %q", requester, job.PrincipalUnknown)
	}
}
