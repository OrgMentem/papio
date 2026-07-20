// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type webhookPayload struct {
	Source     string `json:"source"`
	Kind       string `json:"event,omitempty"`
	Message    string `json:"message"`
	WatchID    int64  `json:"watch_id,omitempty"`
	WatchLabel string `json:"watch_label,omitempty"`
	Count      int    `json:"count,omitempty"`
	SentAt     string `json:"sent_at"`
}

type webhookRequest struct {
	method        string
	contentType   string
	authorization string
	payload       webhookPayload
	fields        map[string]json.RawMessage
}

func TestWebhookSendPostsNotification(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 34, 56, 0, time.UTC)
	tests := []struct {
		name              string
		secret            string
		wantAuthorization string
	}{
		{name: "with secret", secret: "shared-secret", wantAuthorization: "Bearer shared-secret"},
		{name: "without secret"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan webhookRequest, 1)
			var requestCount atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read webhook body: %v", err)
				}
				var payload webhookPayload
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Errorf("decode webhook body: %v", err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Errorf("decode webhook fields: %v", err)
				}
				requests <- webhookRequest{
					method:        r.Method,
					contentType:   r.Header.Get("Content-Type"),
					authorization: r.Header.Get("Authorization"),
					payload:       payload,
					fields:        fields,
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			sender := NewWebhook(server.URL, test.secret)
			sender.Now = func() time.Time { return now }
			sender.Send(context.Background(), "paper imported")

			if count := requestCount.Load(); count != 1 {
				t.Fatalf("request count = %d, want 1", count)
			}

			request := <-requests
			if request.method != http.MethodPost {
				t.Fatalf("method = %q, want %q", request.method, http.MethodPost)
			}
			if request.contentType != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", request.contentType)
			}
			if request.authorization != test.wantAuthorization {
				t.Fatalf("Authorization = %q, want %q", request.authorization, test.wantAuthorization)
			}
			want := webhookPayload{Source: "papio", Message: "paper imported", SentAt: now.Format(time.RFC3339)}
			if !reflect.DeepEqual(request.payload, want) {
				t.Fatalf("payload = %#v, want %#v", request.payload, want)
			}
			wantFields := map[string]bool{"source": true, "message": true, "sent_at": true}
			if len(request.fields) != len(wantFields) {
				t.Fatalf("payload field count = %d, want %d: %v", len(request.fields), len(wantFields), request.fields)
			}
			for field := range request.fields {
				if !wantFields[field] {
					t.Fatalf("unexpected payload field %q in %v", field, request.fields)
				}
			}
		})
	}
}

func TestWebhookSendEventMessageOnlyUsesLegacyPayload(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook body: %v", err)
		}
		requests <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	now := time.Date(2026, 7, 20, 12, 34, 56, 0, time.UTC)
	sender := NewWebhook(server.URL, "")
	sender.Now = func() time.Time { return now }
	sender.SendEvent(context.Background(), Event{Message: "paper imported"})

	want := map[string]any{
		"source":  "papio",
		"message": "paper imported",
		"sent_at": now.Format(time.RFC3339),
	}
	if got := <-requests; !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %#v, want %#v", got, want)
	}
}

func TestWebhookSendEventPostsStructuredNotification(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook body: %v", err)
		}
		requests <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	now := time.Date(2026, 7, 20, 5, 34, 56, 0, time.FixedZone("PDT", -7*60*60))
	sender := NewWebhook(server.URL, "")
	sender.Now = func() time.Time { return now }
	sender.SendEvent(context.Background(), Event{
		Kind:       "watch.alert",
		Message:    "3 papers imported",
		WatchID:    42,
		WatchLabel: "quantum",
		Count:      3,
	})

	want := map[string]any{
		"source":      "papio",
		"event":       "watch.alert",
		"message":     "3 papers imported",
		"watch_id":    float64(42),
		"watch_label": "quantum",
		"count":       float64(3),
		"sent_at":     "2026-07-20T12:34:56Z",
	}
	if got := <-requests; !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %#v, want %#v", got, want)
	}
}

func TestWebhookSendSwallowsFailedDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	closed := httptest.NewServer(nil)
	closedURL := closed.URL
	closed.Close()

	tests := []struct {
		name string
		url  string
	}{
		{name: "non-success response", url: server.URL},
		{name: "connection refused", url: closedURL},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			NewWebhook(test.url, "").Send(context.Background(), "paper imported")
		})
	}
}

func TestWebhookSendSkipsCancelledContext(t *testing.T) {
	requests := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	NewWebhook(server.URL, "").Send(ctx, "paper imported")

	select {
	case <-requests:
		t.Fatal("webhook request was delivered for a cancelled context")
	default:
	}
}

type senderFunc func(context.Context, string)

func (f senderFunc) Send(ctx context.Context, message string) {
	f(ctx, message)
}

type eventSenderFunc func(context.Context, Event)

func (f eventSenderFunc) Send(ctx context.Context, message string) {
	f(ctx, Event{Message: message})
}

func (f eventSenderFunc) SendEvent(ctx context.Context, event Event) {
	f(ctx, event)
}

func TestFanoutSendEventDeliversStructuredAndPlainNotifications(t *testing.T) {
	event := Event{
		Kind:       "watch.alert",
		Message:    "3 papers imported",
		WatchID:    42,
		WatchLabel: "quantum",
		Count:      3,
	}
	var structured []Event
	var plain []string
	fanout := Fanout(
		eventSenderFunc(func(_ context.Context, got Event) {
			structured = append(structured, got)
		}),
		nil,
		senderFunc(func(_ context.Context, message string) {
			plain = append(plain, message)
		}),
	)

	eventFanout, ok := fanout.(EventSender)
	if !ok {
		t.Fatal("Fanout result does not implement EventSender")
	}
	eventFanout.SendEvent(context.Background(), event)

	if !reflect.DeepEqual(structured, []Event{event}) {
		t.Fatalf("structured events = %#v, want %#v", structured, []Event{event})
	}
	if !reflect.DeepEqual(plain, []string{event.Message}) {
		t.Fatalf("plain messages = %#v, want %#v", plain, []string{event.Message})
	}

	Fanout().(EventSender).SendEvent(context.Background(), event)
}

func TestFanoutDeliversSequentiallyAndSkipsNil(t *testing.T) {
	tests := []struct {
		name  string
		input []int
		want  []int
	}{
		{name: "skips nil sender", input: []int{1, 0, 2}, want: []int{1, 2}},
		{name: "no usable senders", input: []int{0, 0}, want: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls []int
			senders := make([]Sender, 0, len(test.input))
			for _, value := range test.input {
				if value == 0 {
					senders = append(senders, nil)
					continue
				}
				value := value
				senders = append(senders, senderFunc(func(_ context.Context, message string) {
					if message != "paper imported" {
						t.Errorf("message = %q, want paper imported", message)
					}
					calls = append(calls, value)
				}))
			}

			Fanout(senders...).Send(context.Background(), "paper imported")
			if !reflect.DeepEqual(calls, test.want) {
				t.Fatalf("calls = %#v, want %#v", calls, test.want)
			}
		})
	}
}
