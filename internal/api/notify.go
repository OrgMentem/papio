// Copyright 2026 OrgMentem. Licensed under MIT.

package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"syscall"
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
		// A count this category can never produce is the caller's mistake, not
		// a daemon failure: report it as such instead of "operation failed".
		if errors.Is(err, notify.ErrPreviewCountUnrepresentable) {
			return nil, &ipc.RPCError{Code: "invalid_argument", Message: err.Error()}
		}
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

// NotifyWebhookTestParams is the request of notify.webhook_test_v1. It is a
// separate method so notify.test_v1 keeps its exact params and result for
// older CLI binaries.
type NotifyWebhookTestParams struct {
	Category string `json:"category"`
}

// NotifyWebhookTestResult reports one explicit webhook test. Unlike the
// desktop test, the daemon learns the endpoint's answer, so Acknowledged is
// true only after a 2xx status.
type NotifyWebhookTestResult struct {
	Category     string `json:"category"`
	Message      string `json:"message"`
	Format       string `json:"format"`
	Acknowledged bool   `json:"acknowledged"`
	Detail       string `json:"detail"`
}

// notifyWebhookTest posts one test event through the webhook leg. The error
// from the sender is never echoed: a transport error carries the endpoint URL,
// whose path or query can be a secret. What is reported instead is URL-free: the
// HTTP status the endpoint answered with, or the category of transport failure.
func notifyWebhookTest(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params NotifyWebhookTestParams
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
	message, err := system.Notify.TestWebhook(ctx, category)
	switch {
	case errors.Is(err, notify.ErrWebhookNotConfigured):
		return nil, &ipc.RPCError{Code: "unavailable", Message: err.Error() + "; set notify.webhook_url or a webhook credential"}
	case err != nil:
		return webhookTestFailure(err)
	}
	format := system.Notify.WebhookFormat()
	return marshal(NotifyWebhookTestResult{Category: string(category), Message: message, Format: format, Acknowledged: true, Detail: "posted to the configured webhook as " + format + "; the endpoint answered with a 2xx status"})
}

// webhookTestFailure answers a failed webhook test with a cause the operator
// can act on and the endpoint URL cannot leak through. A non-2xx answer carries
// the HTTP status (also in Detail.ErrorHTTPStatus, so --json reports it as
// http_status); a transport failure carries a URL-free category. Only the
// classified cause reaches the daemon log, never the raw error.
func webhookTestFailure(err error) ([]byte, *ipc.RPCError) {
	var status *notify.WebhookStatusError
	if errors.As(err, &status) {
		log.Printf("rpc notify webhook test: the endpoint answered HTTP %d", status.StatusCode)
		message := fmt.Sprintf("the webhook endpoint answered HTTP %d", status.StatusCode)
		switch status.StatusCode {
		case 401, 403:
			message += "; check the webhook credential"
		case 404:
			message += "; check the path of notify.webhook_url"
		case 413, 415:
			message += "; check notify.webhook_format"
		}
		return nil, &ipc.RPCError{Code: "unavailable", Message: message, Detail: &ipc.ErrorDetail{ErrorHTTPStatus: status.StatusCode}}
	}
	cause := webhookTransportCause(err)
	log.Printf("rpc notify webhook test: transport failure: %s", cause)
	return nil, &ipc.RPCError{Code: "unavailable", Message: "the webhook endpoint could not be reached: " + cause}
}

// webhookTransportCause names a transport failure without the URL that the
// underlying error quotes.
func webhookTransportCause(err error) string {
	var (
		dns        *net.DNSError
		unknownCA  x509.UnknownAuthorityError
		hostname   x509.HostnameError
		invalid    x509.CertificateInvalidError
		verify     *tls.CertificateVerificationError
		recordSize tls.RecordHeaderError
		alert      tls.AlertError
		netErr     net.Error
	)
	switch {
	case errors.Is(err, context.Canceled):
		return "the request was cancelled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "the request timed out"
	case errors.As(err, &dns):
		return "DNS lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &unknownCA), errors.As(err, &hostname), errors.As(err, &invalid), errors.As(err, &verify), errors.As(err, &recordSize), errors.As(err, &alert):
		return "TLS handshake failed"
	default:
		return "transport error"
	}
}
