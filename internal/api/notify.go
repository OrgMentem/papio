// Copyright 2026 OrgMentem. Licensed under MIT.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/notify"
)

// NotifyRouteRow is the stable, purpose-built row returned by notify.show_v1.
type NotifyRouteRow struct {
	Category      string `json:"category"`
	Desktop       string `json:"desktop"`
	Webhook       string `json:"webhook"`
	WindowSeconds int64  `json:"window_seconds"`
	Source        string `json:"source"`
}

// NotifyShowResult is the effective notification routing table. It is a new
// result rather than an addition to an existing status response so older CLI
// clients remain strict-decode compatible.
type NotifyShowResult struct {
	Preset string           `json:"preset"`
	Rows   []NotifyRouteRow `json:"rows"`
}

type NotifyPreviewParams struct {
	Category string `json:"category"`
	Count    int    `json:"count,omitempty"`
}

type NotifyPreviewResult struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
	Message  string `json:"message"`
}

type NotifyTestParams struct {
	Category string `json:"category"`
}

// NotifyTestResult is deliberately explicit that the platform was attempted
// but cannot acknowledge display. The daemon never claims delivery.
type NotifyTestResult struct {
	Category             string `json:"category"`
	Message              string `json:"message"`
	Attempted            bool   `json:"attempted"`
	DeliveryAcknowledged bool   `json:"delivery_acknowledged"`
	Detail               string `json:"detail"`
}

func notifyUnavailable() ([]byte, *ipc.RPCError) {
	return nil, &ipc.RPCError{Code: "unavailable", Message: "notification router is unavailable; restart the papio daemon"}
}

func decodeNotifyCategory(raw string) (notify.Category, *ipc.RPCError) {
	category := notify.Category(strings.TrimSpace(raw))
	for _, known := range notify.Categories() {
		if category == known {
			return category, nil
		}
	}
	valid := make([]string, 0, len(notify.Categories()))
	for _, known := range notify.Categories() {
		valid = append(valid, string(known))
	}
	return "", &ipc.RPCError{Code: "invalid_argument", Message: fmt.Sprintf("unknown notification category %q; valid categories: %s", raw, strings.Join(valid, ", "))}
}

func notifyShow(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system == nil || system.Notify == nil {
		return notifyUnavailable()
	}
	policy, err := notify.ResolvePolicy(system.Config.Notify)
	if err != nil {
		return failure(err)
	}
	rows := system.Notify.Table()
	out := NotifyShowResult{Preset: policy.Preset, Rows: make([]NotifyRouteRow, 0, len(rows))}
	for _, row := range rows {
		out.Rows = append(out.Rows, NotifyRouteRow{
			Category: string(row.Category), Desktop: row.Desktop, Webhook: row.Webhook,
			WindowSeconds: int64(row.Window / time.Second), Source: row.Source,
		})
	}
	return marshal(out)
}

func notifyPreview(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params NotifyPreviewParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system == nil || system.Notify == nil {
		return notifyUnavailable()
	}
	category, rpcErr := decodeNotifyCategory(params.Category)
	if rpcErr != nil {
		return nil, rpcErr
	}
	message, err := system.Notify.Preview(category, params.Count)
	if err != nil {
		return failure(err)
	}
	count := params.Count
	if count < 1 {
		count = 1
	}
	return marshal(NotifyPreviewResult{Category: string(category), Count: count, Message: message})
}

func notifyTest(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params NotifyTestParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system == nil || system.Notify == nil {
		return notifyUnavailable()
	}
	category, rpcErr := decodeNotifyCategory(params.Category)
	if rpcErr != nil {
		return nil, rpcErr
	}
	available, detail := notify.PlatformCapability()
	if !available {
		return nil, &ipc.RPCError{Code: "unavailable", Message: detail + "; use papio inbox or Activity"}
	}
	message, err := system.Notify.Test(ctx, category)
	if err != nil {
		return nil, &ipc.RPCError{Code: "unavailable", Message: safeMessage(err, "desktop notification sender is unavailable; use papio inbox or Activity")}
	}
	return marshal(NotifyTestResult{Category: string(category), Message: message, Attempted: true, DeliveryAcknowledged: false, Detail: "sent to the configured local platform channel; the platform provides no delivery acknowledgement"})
}
