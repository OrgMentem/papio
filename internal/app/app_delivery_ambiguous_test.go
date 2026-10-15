// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// A 2xx ILLiad create response with no transaction number must never be
// recorded as a request. The stub provider and wiring here serve the
// document-delivery ambiguous-POST tests in the sibling files.

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"papio/internal/delivery"
	"papio/internal/job"
	"strings"
	"sync/atomic"
	"testing"
)

// ambiguousProvider is an ILLiad stub whose create POST never yields a usable
// transaction: postStatus/postBody decide the response, and every token lookup
// finds nothing yet.
type ambiguousProvider struct {
	*httptest.Server
	posts, gets atomic.Int32
}

func newAmbiguousProvider(t *testing.T, postStatus int, postBody string) *ambiguousProvider {
	t.Helper()
	p := &ambiguousProvider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			p.posts.Add(1)
			w.WriteHeader(postStatus)
			_, _ = w.Write([]byte(postBody))
		case strings.Contains(r.URL.Path, "Users/ExternalUserId/"):
			p.gets.Add(1)
			_, _ = w.Write([]byte(`{"UserName":"campus-student"}`))
		case strings.Contains(r.URL.Path, "Transaction/UserRequests/"):
			p.gets.Add(1)
			_, _ = w.Write([]byte(`[]`))
		default:
			p.gets.Add(1)
			http.Error(w, "unexpected provider read", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

// newAmbiguousDeliveryService wires an auto-capable ILLiad profile at p.
func newAmbiguousDeliveryService(t *testing.T, p *ambiguousProvider) (*Service, *job.Store, *delivery.Service) {
	t.Helper()
	svc, jobs, deliverySvc := newDeliveryTestService(t)
	svc.Delivery = deliverySvc
	svc.IlliadHTTPClient = p.Client()
	svc.Config.Browser.DocumentDelivery = autoCapableDocumentDelivery(p.URL)
	svc.Resolvers = deliveryTestResolvers()
	if err := deliverySvc.RecordLiveAcceptance(context.Background(), "default", "illiad"); err != nil {
		t.Fatal(err)
	}
	return svc, jobs, deliverySvc
}

// TestZeroTransactionNumberIsAmbiguousNotSubmitted proves a 2xx create
// response with no TransactionNumber is looked up by token and never recorded
// as a submitted request with provider reference "0".
func TestZeroTransactionNumberIsAmbiguousNotSubmitted(t *testing.T) {
	p := newAmbiguousProvider(t, http.StatusOK, `{}`)
	svc, jobs, deliverySvc := newAmbiguousDeliveryService(t, p)
	ctx := context.Background()
	id := processNewDeliveryJob(t, svc, jobs, "wr_zero_txn", "10.1000/zero-txn")

	row, err := deliverySvc.GetByJobID(ctx, id)
	if err != nil || row == nil {
		t.Fatalf("delivery row = %+v, err = %v", row, err)
	}
	if row.State == delivery.StateSubmitted || row.ProviderReference == "0" {
		t.Fatalf("row = state %q reference %q, want it never recorded as submitted with reference 0", row.State, row.ProviderReference)
	}
	if row.State != delivery.StateOffered || row.ProviderReference != "" {
		t.Fatalf("row = %+v, want offered with no reference while the outcome is unknown", row)
	}
	if p.posts.Load() != 1 {
		t.Fatalf("provider POSTs = %d, want 1", p.posts.Load())
	}
	if p.gets.Load() == 0 {
		t.Fatal("no token reconciliation read was made for a 2xx with no transaction number")
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateRetryWait {
		t.Fatalf("job state = %q, want retry_wait for the bounded reconciliation recheck", got.State)
	}
}
