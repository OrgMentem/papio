// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package batch

import (
	"context"
	"fmt"
	"papio/internal/protocol"
	"papio/internal/zotio"
)

// Preview classifies the exact selected set without creating jobs or manifests.
func Preview(ctx context.Context, caller Caller, requests []protocol.WorkRequest, options SubmitOptions) (*zotio.LookupWorksResult, error) {
	if len(requests) == 0 || len(requests) > 50 {
		return nil, fmt.Errorf("selection must contain 1-50 works")
	}
	for _, request := range requests {
		if err := request.Validate(); err != nil {
			return nil, err
		}
	}
	result, err := classifyBatchOwnership(ctx, caller, requests, options)
	if err != nil {
		return nil, err
	}
	if _, _, err := ApplyOwnership(requests, *result, options.Collection, options.IncludeOwned); err != nil {
		return nil, err
	}
	return result, nil
}
