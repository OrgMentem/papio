// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type webhookPayload struct {
	Source  string `json:"source"`
	Message string `json:"message"`
	SentAt  string `json:"sent_at"`
}

type webhookRequest struct {
	method        string
	contentType   string
	authorization string
	payload       webhookPayload
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
				var payload webhookPayload
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode webhook body: %v", err)
				}
				requests <- webhookRequest{
					method:        r.Method,
					contentType:   r.Header.Get("Content-Type"),
					authorization: r.Header.Get("Authorization"),
					payload:       payload,
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
		})
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
