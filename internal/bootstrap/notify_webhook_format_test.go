// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bootstrap

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/notify"
	"papio/internal/store/storetest"
)

// notify.webhook_format must reach the real sender: a routed notification
// leaves through the ledger-backed webhook leg in the configured shape.
func TestNotifyWebhookFormatReachesRoutedDelivery(t *testing.T) {
	type seen struct{ body, contentType, key string }
	requests := make(chan seen, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- seen{body: string(body), contentType: r.Header.Get("Content-Type"), key: r.Header.Get("X-Papio-Delivery-Key")}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = storetest.DataDir(t)
	cfg.PDF.OCREnabled = false
	cfg.Zotio.AutoEnrich = false
	cfg.Notify.Enabled = false
	cfg.Notify.WebhookURL = server.URL
	cfg.Notify.WebhookFormat = config.WebhookFormatSlack
	system, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })
	if got := system.Notify.WebhookFormat(); got != config.WebhookFormatSlack {
		t.Fatalf("router webhook format = %q, want slack", got)
	}

	now := time.Now().UTC()
	message := "Request finished"
	if err := system.Notify.Route(context.Background(), notify.Intent{
		EventKind: "request.outcome", Category: notify.CategoryRequestOutcome,
		AggregateKey: "job:format", Phase: notify.PhaseTerminal,
		WindowStart: now, HappenedAt: now, Message: message,
		Detail: notify.Event{Kind: "request.outcome", Message: message, Count: 1},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-requests:
		if got.contentType != "application/json" || got.key == "" {
			t.Fatalf("request = %+v, want JSON content type and a delivery key header", got)
		}
		if want := `{"text":"*papio: request.outcome*\n` + message + `"}`; got.body != want {
			t.Fatalf("body = %q, want %q", got.body, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook delivery")
	}
}
