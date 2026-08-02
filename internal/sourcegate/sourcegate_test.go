package sourcegate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"papio/internal/config"
)

type recorder struct {
	calls []string
	err   error
}

func (r *recorder) Acquire(_ context.Context, source string, _ config.Source, _ float64) error {
	r.calls = append(r.calls, source)
	return r.err
}

func request(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// Every forwarded request must reserve, because the provider counts requests
// and not logical calls. A discovery search that resolves a seed DOI first
// issues two, and accounting for one is the under-reporting this package ends.
func TestEveryRequestReservesOnce(t *testing.T) {
	var served int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { served++ }))
	defer server.Close()

	reserve := &recorder{}
	client := New(reserve, "openalex", config.Source{Enabled: true}, 0, server.Client())
	for range 3 {
		resp, err := client.Do(request(t, server.URL))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if len(reserve.calls) != 3 || served != 3 {
		t.Fatalf("reservations = %d, requests served = %d, want 3 and 3", len(reserve.calls), served)
	}
	for _, source := range reserve.calls {
		if source != "openalex" {
			t.Fatalf("reserved against %q, want openalex", source)
		}
	}
}

// A refused reservation must stop the request reaching the provider. Left
// ungated, discovery kept calling an API whose durable gate had already paused
// acquisition -- the failure this exists to prevent, so it is asserted on the
// server rather than only on the returned error.
func TestARefusedReservationNeverReachesTheProvider(t *testing.T) {
	var served int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { served++ }))
	defer server.Close()

	refusal := errors.New("source openalex is deferred until tomorrow")
	client := New(&recorder{err: refusal}, "openalex", config.Source{Enabled: true}, 0, server.Client())
	_, err := client.Do(request(t, server.URL))
	if !errors.Is(err, refusal) {
		t.Fatalf("err = %v, want the reservation error unwrapped so callers can classify a rate limit", err)
	}
	if served != 0 {
		t.Fatalf("provider served %d requests behind a closed gate, want 0", served)
	}
}

// A caller with no budget manager keeps its transport rather than silently
// losing it, which would turn a missing dependency into a nil-pointer panic on
// the first request.
func TestNoReserverReturnsTheInnerClientUnchanged(t *testing.T) {
	inner := http.DefaultClient
	if got := New(nil, "openalex", config.Source{}, 0, inner); got != HTTPClient(inner) {
		t.Fatalf("New with a nil reserver = %#v, want the inner client unchanged", got)
	}
}
