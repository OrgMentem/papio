// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"
	"encoding/json"
	"testing"

	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
)

func TestAcknowledgeClosureV1ReopensAClosedSource(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	if err := system.Budgets.DriftClose(ctx, config.SourceOpenAlex, "cost drift"); err != nil {
		t.Fatal(err)
	}
	router := Router(system)
	params := map[string]string{"source": config.SourceOpenAlex}

	var result AcknowledgeClosureResult
	if rpcErr := callMethod(t, router, "budget.acknowledge_closure_v1", params, &result); rpcErr != nil {
		t.Fatalf("acknowledge = %+v", rpcErr)
	}
	if result.Source != config.SourceOpenAlex || !result.Cleared {
		t.Fatalf("result = %+v, want cleared openalex", result)
	}
	status, err := system.Budgets.CreditStatus(ctx, config.SourceOpenAlex)
	if err != nil {
		t.Fatal(err)
	}
	if status.DriftReason != "" {
		t.Fatalf("closure still reported after acknowledgement: %q", status.DriftReason)
	}

	if rpcErr := callMethod(t, router, "budget.acknowledge_closure_v1", params, &result); rpcErr != nil {
		t.Fatalf("second acknowledge = %+v", rpcErr)
	}
	if result.Cleared {
		t.Fatalf("second acknowledgement cleared = true, want false for an open source")
	}
}

func TestAcknowledgeClosureV1RejectsBadParamsAndAgents(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	if err := system.Budgets.DriftClose(ctx, config.SourceOpenAlex, "cost drift"); err != nil {
		t.Fatal(err)
	}
	router := Router(system)
	for name, params := range map[string]any{
		"empty source":   map[string]string{"source": ""},
		"unknown source": map[string]string{"source": "nonesuch"},
		"unknown field":  map[string]any{"source": config.SourceOpenAlex, "force": true},
	} {
		if rpcErr := callMethod(t, router, "budget.acknowledge_closure_v1", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("%s: = %+v, want invalid_argument", name, rpcErr)
		}
	}

	raw, err := json.Marshal(map[string]string{"source": config.SourceOpenAlex})
	if err != nil {
		t.Fatal(err)
	}
	_, rpcErr := router.Handle(WithPrincipal(ctx, job.PrincipalMCP), ipc.Request{Method: "budget.acknowledge_closure_v1", Params: raw})
	if rpcErr == nil || rpcErr.Code != "permission_denied" {
		t.Fatalf("MCP caller = %+v, want permission_denied", rpcErr)
	}
	status, err := system.Budgets.CreditStatus(ctx, config.SourceOpenAlex)
	if err != nil {
		t.Fatal(err)
	}
	if status.DriftReason == "" {
		t.Fatal("the refused agent call reopened the source")
	}
}
