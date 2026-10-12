// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"

	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
)

// AcknowledgeClosureParams names the source whose sticky egress closure the
// operator acknowledges.
type AcknowledgeClosureParams struct {
	Source string `json:"source"`
}

// AcknowledgeClosureResult reports whether the source was closed. Cleared is
// false for a source that was already open, which is not an error at this
// layer: the CLI turns it into a nonzero exit.
type AcknowledgeClosureResult struct {
	Source  string `json:"source"`
	Cleared bool   `json:"cleared"`
}

// acknowledgeClosure reopens paid egress for one source (ADR-0024 §8). It is
// the operator's decision alone: an MCP caller is refused here as well as by
// the hidden CLI command, so a prompt-injected agent that reaches the router
// in-process still cannot spend the operator's money.
func acknowledgeClosure(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params AcknowledgeClosureParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if params.Source == "" {
		return badParams(errors.New("source is required"))
	}
	if !slices.Contains(config.SourceNames(), params.Source) {
		return badParams(errors.New("unknown source"))
	}
	principal := PrincipalFrom(ctx)
	if principal == job.PrincipalMCP {
		return nil, &ipc.RPCError{Code: "permission_denied", Message: "reopening a closed source is an operator decision and is not available to agents"}
	}
	if system == nil || system.Budgets == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "budgets are not configured"}
	}
	cleared, err := system.Budgets.AcknowledgeClosure(ctx, params.Source)
	if err != nil {
		return failure(err)
	}
	// Nothing durable records who acknowledged; the daemon log is the audit.
	log.Printf("budget: operator acknowledged egress closure for source %s (principal %s, cleared=%t)", params.Source, principal, cleared)
	return marshal(AcknowledgeClosureResult{Source: params.Source, Cleared: cleared})
}
