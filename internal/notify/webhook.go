// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// HTTPClient performs webhook requests. It is injectable so delivery behavior
// can be tested without opening a network connection.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

var defaultWebhookClient = &http.Client{Timeout: notificationTimeout}

// Webhook sends best-effort notifications to a remote HTTP endpoint.
type Webhook struct {
	URL    string
	Secret string
	Client HTTPClient
	Now    func() time.Time
}

// NewWebhook constructs a webhook sender with the supplied endpoint and secret.
func NewWebhook(url, secret string) *Webhook {
	return &Webhook{URL: url, Secret: secret}
}

// Send posts a bounded notification and deliberately ignores every failure so
// an unavailable endpoint cannot interrupt daemon work.
func (w *Webhook) Send(ctx context.Context, message string) {
	w.SendEvent(ctx, Event{Message: message})
}

// SendEvent posts a bounded structured notification and deliberately ignores
// every failure so an unavailable endpoint cannot interrupt daemon work.
func (w *Webhook) SendEvent(ctx context.Context, event Event) {
	if ctx == nil || ctx.Err() != nil {
		return
	}

	bounded, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()

	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	payload, err := json.Marshal(struct {
		Source string `json:"source"`
		Event
		SentAt string `json:"sent_at"`
	}{
		Source: "papio",
		Event:  event,
		SentAt: now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}

	request, err := http.NewRequestWithContext(bounded, http.MethodPost, w.URL, bytes.NewReader(payload))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	if w.Secret != "" {
		request.Header.Set("Authorization", "Bearer "+w.Secret)
	}

	client := w.Client
	if client == nil {
		client = defaultWebhookClient
	}
	response, _ := client.Do(request)
	if response != nil && response.Body != nil {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
}
