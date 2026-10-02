// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"
	"encoding/json"
	"errors"

	"papio/internal/bootstrap"
	"papio/internal/discovery"
	"papio/internal/ipc"
	"papio/internal/ownership"
	"papio/internal/zotio"
)

func searchUnowned(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var request discovery.UnownedRequest
	if err := ipc.DecodeParams(raw, &request); err != nil {
		return badParams(err)
	}
	if system == nil || system.Discovery == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "discovery is not configured"}
	}
	classify := func(ctx context.Context, works []discovery.DiscoveredWork) error {
		if len(works) == 0 {
			return nil
		}
		incomplete := errors.New("ownership classification is incomplete; cannot certify new-only results")
		if system.Holdings != nil && system.Holdings.Enabled() {
			queries := make([]ownership.Query, len(works))
			for i, item := range works {
				queries[i] = ownership.QueryFor(item.Work.DOI, item.Work.ArXiv, item.Work.PMID, "", "")
				if len(queries[i].Identifiers) == 0 {
					return incomplete
				}
			}
			result := system.Holdings.Lookup(ctx, queries)
			if !result.Complete() || len(result.Works) != len(works) {
				return incomplete
			}
			for _, source := range result.Sources {
				if source.Stale {
					return incomplete
				}
			}
			for i := range works {
				decision := ownership.Decide(queries[i], result.Works[i])
				works[i].Owned = decision.Suppress || decision.RecordPresent
			}
			return nil
		}
		if system.Zotio == nil {
			return incomplete
		}
		request := zotio.LookupWorksRequest{Works: make([]zotio.LookupWork, len(works))}
		for i, item := range works {
			request.Works[i] = zotio.LookupWorkFrom(item.Work)
		}
		result, err := system.Zotio.LookupWorks(ctx, request)
		if err != nil || result == nil || len(result.Works) != len(works) || result.StalenessWarning != "" {
			return incomplete
		}
		for i, item := range result.Works {
			switch item.Status {
			case zotio.OwnershipNotOwned:
				works[i].Owned = false
			case zotio.OwnershipOwnedWithPDF, zotio.OwnershipOwnedMissingPDF:
				works[i].Owned = true
				works[i].OwnedItemKey = item.ItemKey
			default:
				return incomplete
			}
		}
		return nil
	}
	result, err := discovery.SearchUnowned(ctx, system.Discovery, request, classify)
	if err != nil {
		if errors.Is(err, discovery.ErrInvalidPageToken) {
			return badParams(err)
		}
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: discovery.SanitizeError(err)}
	}
	return marshal(result)
}
