// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPClient performs webhook requests. It is injectable so delivery behavior
// can be tested without opening a network connection.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// defaultWebhookClient never follows a redirect: a 3xx settles the delivery
// as failed. Go replays a POST body on 307/308, so following one would let a
// configured endpoint aim papio's POST at any destination it names —
// including a loopback or private-network service only this machine can
// reach — and a hop from https to http would carry the payload in cleartext.
var defaultWebhookClient = &http.Client{
	Timeout: notificationTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Webhook sends best-effort notifications to a remote HTTP endpoint.
type Webhook struct {
	URL    string
	Secret string
	// Format is the request shape: one of the config.WebhookFormat*
	// constants. Empty means config.WebhookFormatJSON. An unknown value
	// fails every send, so a mistyped format never posts in the wrong shape.
	Format string
	Client HTTPClient
	Now    func() time.Time
}

// NewWebhook constructs a webhook sender with the supplied endpoint and secret.
func NewWebhook(url, secret string) *Webhook {
	return &Webhook{URL: url, Secret: secret}
}

// Send posts a bounded notification and deliberately ignores every failure so
// an unavailable endpoint cannot interrupt daemon work. Callers that need the
// failure signal use SendEventResult instead.
func (w *Webhook) Send(ctx context.Context, message string) {
	_ = w.SendEventResult(ctx, Event{Message: message})
}

// SendEvent posts a bounded structured notification and deliberately ignores
// every failure so an unavailable endpoint cannot interrupt daemon work. It
// exists so Webhook satisfies the Sender and EventSender interfaces; the
// router uses SendEventResult to record a visible failed state instead.
func (w *Webhook) SendEvent(ctx context.Context, event Event) {
	_ = w.SendEventResult(ctx, event)
}

// SendEventResult posts one bounded structured notification and reports the
// delivery outcome. A nil or cancelled context, a transport error, or a
// non-2xx status is a failure. A context that is already done stops before
// any request goes out and reports errWebhookNotDispatched, so the router
// can release the claim instead of settling an ambiguous failure. Success is
// any 2xx status after the request was accepted by the endpoint.
func (w *Webhook) SendEventResult(ctx context.Context, event Event) error {
	if ctx == nil {
		return fmt.Errorf("%w: %w", errWebhookNotDispatched, context.Canceled)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", errWebhookNotDispatched, err)
	}

	bounded, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()

	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	body, header, err := w.render(event, now())
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(bounded, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header = header
	if w.Secret != "" {
		request.Header.Set("Authorization", "Bearer "+w.Secret)
	}

	client := w.Client
	if client == nil {
		client = defaultWebhookClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	if response != nil && response.Body != nil {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if response == nil {
		return fmt.Errorf("webhook: no response from endpoint")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &WebhookStatusError{StatusCode: response.StatusCode, Status: response.Status}
	}
	return nil
}

// WebhookStatusError is a webhook endpoint's non-2xx answer. The status code
// carries no secret, so operator surfaces may show it; unlike a transport error
// it never quotes the endpoint URL.
type WebhookStatusError struct {
	StatusCode int
	Status     string
}

func (e *WebhookStatusError) Error() string { return "webhook: unexpected status " + e.Status }

// WebhookDeliveryKey returns the stable per-row delivery key the router
// attaches to every ledger-backed webhook POST. The key is the durable row
// ID, so retries of the same row carry the same key and distinct rows never
// share one.
func WebhookDeliveryKey(id int64) string {
	return fmt.Sprintf("papio-%d", id)
}
