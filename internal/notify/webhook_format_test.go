// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/config"
)

type capturedRequest struct {
	method string
	header http.Header
	body   string
}

func captureWebhook(t *testing.T) (*httptest.Server, <-chan capturedRequest) {
	t.Helper()
	requests := make(chan capturedRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		requests <- capturedRequest{method: r.Method, header: r.Header.Clone(), body: string(body)}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// Each format must reach the receiver in its native shape: exact body, exact
// headers, and the Bearer credential unchanged.
func TestWebhookFormatsRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 34, 56, 0, time.UTC)
	event := Event{Kind: "watch.alert", Message: "3 new papers <b> & more", Count: 3, DeliveryKey: "papio-42"}
	tests := []struct {
		name       string
		format     string
		event      Event
		wantBody   string
		wantHeader map[string]string
		absent     []string
	}{
		{
			name: "json", format: config.WebhookFormatJSON, event: event,
			wantBody: `{"source":"papio","event":"watch.alert","message":"3 new papers \u003cb\u003e \u0026 more","count":3,"delivery_key":"papio-42","sent_at":"2026-07-20T12:34:56Z"}`,
			wantHeader: map[string]string{
				"Content-Type": "application/json", "Authorization": "Bearer shared-secret",
			},
			absent: []string{"Title", "Tags", "Priority", deliveryKeyHeader},
		},
		{
			name: "empty format is json", format: "", event: Event{Message: "ready"},
			wantBody:   `{"source":"papio","message":"ready","sent_at":"2026-07-20T12:34:56Z"}`,
			wantHeader: map[string]string{"Content-Type": "application/json"},
			absent:     []string{"Title", "Tags", "Priority"},
		},
		{
			name: "ntfy", format: config.WebhookFormatNtfy, event: event,
			wantBody: "3 new papers <b> & more",
			wantHeader: map[string]string{
				"Content-Type": "text/plain; charset=utf-8", "Authorization": "Bearer shared-secret",
				"Title": "papio: watch.alert (3)", "Tags": "papio,watch.alert", "Priority": "default",
				deliveryKeyHeader: "papio-42",
			},
		},
		{
			name: "ntfy human turn is high priority", format: config.WebhookFormatNtfy,
			event:    Event{Kind: "action.opened", Message: "papio has work waiting", Count: 1},
			wantBody: "papio has work waiting",
			wantHeader: map[string]string{
				"Title": "papio: action.opened", "Tags": "papio,action.opened", "Priority": "high",
			},
			absent: []string{deliveryKeyHeader},
		},
		{
			name: "ntfy plain notice", format: config.WebhookFormatNtfy, event: Event{Message: "ready"},
			wantBody:   "ready",
			wantHeader: map[string]string{"Title": "papio", "Tags": "papio", "Priority": "default"},
		},
		{
			name: "ntfy drops header-unsafe kind characters", format: config.WebhookFormatNtfy,
			event:      Event{Kind: "watch.alert\r\nX-Injected: 1é", Message: "m"},
			wantBody:   "m",
			wantHeader: map[string]string{"Title": "papio: watch.alertX-Injected:1", "Tags": "papio,watch.alertX-Injected:1"},
			absent:     []string{"X-Injected"},
		},
		{
			name: "slack", format: config.WebhookFormatSlack, event: event,
			wantBody: `{"text":"*papio: watch.alert (3)*\n3 new papers \u0026lt;b\u0026gt; \u0026amp; more"}`,
			wantHeader: map[string]string{
				"Content-Type": "application/json", "Authorization": "Bearer shared-secret",
				deliveryKeyHeader: "papio-42",
			},
			absent: []string{"Title", "Tags", "Priority"},
		},
		{
			name: "slack plain notice", format: config.WebhookFormatSlack, event: Event{Message: "ready"},
			wantBody:   `{"text":"*papio*\nready"}`,
			wantHeader: map[string]string{"Content-Type": "application/json"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, requests := captureWebhook(t)
			secret := ""
			if test.wantHeader["Authorization"] != "" {
				secret = "shared-secret"
			}
			sender := NewWebhook(server.URL, secret)
			sender.Format = test.format
			sender.Now = func() time.Time { return now }
			if err := sender.SendEventResult(context.Background(), test.event); err != nil {
				t.Fatalf("SendEventResult = %v", err)
			}
			got := <-requests
			if got.method != http.MethodPost {
				t.Fatalf("method = %q, want POST", got.method)
			}
			if got.body != test.wantBody {
				t.Fatalf("body = %q, want %q", got.body, test.wantBody)
			}
			for name, want := range test.wantHeader {
				if have := got.header.Get(name); have != want {
					t.Errorf("header %s = %q, want %q", name, have, want)
				}
			}
			for _, name := range test.absent {
				if values := got.header.Values(name); len(values) != 0 {
					t.Errorf("header %s = %q, want absent", name, values)
				}
			}
		})
	}
}

// An unknown format must fail the delivery before any request leaves, not
// fall back to a shape the receiver would misread.
func TestWebhookUnknownFormatFailsClosed(t *testing.T) {
	var posted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posted.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sender := NewWebhook(server.URL, "")
	sender.Format = "matrix"
	if err := sender.SendEventResult(context.Background(), Event{Message: "ready"}); err == nil {
		t.Fatal("unknown format delivered, want an error")
	}
	if posted.Load() != 0 {
		t.Fatalf("unknown format sent %d requests, want 0", posted.Load())
	}
}

func TestWebhookFormatNameDefaultsToJSON(t *testing.T) {
	if got := NewWebhook("https://example.test/h", "").FormatName(); got != config.WebhookFormatJSON {
		t.Fatalf("FormatName = %q, want json", got)
	}
	sender := NewWebhook("https://example.test/h", "")
	sender.Format = config.WebhookFormatNtfy
	if got := sender.FormatName(); got != config.WebhookFormatNtfy {
		t.Fatalf("FormatName = %q, want ntfy", got)
	}
}

// The operator test goes through the same sender as a real notification, so
// the receiver sees the configured shape; no webhook is a typed error.
func TestRouterTestWebhookUsesConfiguredFormat(t *testing.T) {
	server, requests := captureWebhook(t)
	sender := NewWebhook(server.URL, "")
	sender.Format = config.WebhookFormatNtfy
	router := NewRouter(RouterOptions{Webhook: sender})
	message, err := router.TestWebhook(context.Background(), CategoryRequestOutcome)
	if err != nil {
		t.Fatal(err)
	}
	got := <-requests
	if got.body != message || got.header.Get("Title") != "papio: request_outcome" || got.header.Get("Tags") != "papio,request_outcome" {
		t.Fatalf("request = %+v, want ntfy shape for %q", got, message)
	}
	if router.WebhookFormat() != config.WebhookFormatNtfy {
		t.Fatalf("WebhookFormat = %q", router.WebhookFormat())
	}

	none := NewRouter(RouterOptions{})
	if _, err := none.TestWebhook(context.Background(), CategoryRequestOutcome); !errors.Is(err, ErrWebhookNotConfigured) {
		t.Fatalf("TestWebhook without webhook = %v, want ErrWebhookNotConfigured", err)
	}
	if none.WebhookFormat() != "" {
		t.Fatalf("WebhookFormat without webhook = %q, want empty", none.WebhookFormat())
	}
}
