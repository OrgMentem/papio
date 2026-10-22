package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/notify"
)

// TestNotifyWebhookTestSeparatesHTTPStatusFromTransportFailure: the test exists
// to debug a webhook setup, so a failure says whether the endpoint answered
// (and with which status) or could not be reached, and never quotes the URL,
// whose path can be a secret.
func TestNotifyWebhookTestSeparatesHTTPStatusFromTransportFailure(t *testing.T) {
	const secretPath = "/hooks/synthetic-secret-token"
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rejecting.Close()
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	for _, test := range []struct {
		name       string
		url        string
		wantStatus int
		want       []string
	}{
		{name: "401 answer", url: rejecting.URL + secretPath, wantStatus: http.StatusUnauthorized, want: []string{"answered HTTP 401", "credential"}},
		{name: "refused connection", url: closedURL + secretPath, want: []string{"could not be reached", "connection refused"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Default()
			policy, err := notify.ResolvePolicy(cfg.Notify)
			if err != nil {
				t.Fatal(err)
			}
			system := &bootstrap.System{Config: cfg, Notify: notify.NewRouter(notify.RouterOptions{Policy: policy, Webhook: notify.NewWebhook(test.url, "")})}
			_, rpcErr := notifyWebhookTest(context.Background(), []byte(`{"category":"request_outcome"}`), system)
			if rpcErr == nil {
				t.Fatal("notifyWebhookTest succeeded against a failing endpoint")
			}
			if rpcErr.Code != "unavailable" {
				t.Fatalf("code = %q, want unavailable", rpcErr.Code)
			}
			for _, want := range test.want {
				if !strings.Contains(rpcErr.Message, want) {
					t.Fatalf("message = %q, want it to contain %q", rpcErr.Message, want)
				}
			}
			if strings.Contains(rpcErr.Message, secretPath) || strings.Contains(rpcErr.Message, "127.0.0.1") {
				t.Fatalf("message = %q leaks the endpoint URL", rpcErr.Message)
			}
			gotStatus := 0
			if rpcErr.Detail != nil {
				gotStatus = rpcErr.Detail.ErrorHTTPStatus
			}
			if gotStatus != test.wantStatus {
				t.Fatalf("detail http status = %d, want %d", gotStatus, test.wantStatus)
			}
		})
	}
}
