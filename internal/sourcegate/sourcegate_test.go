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
	client, err := New(reserve, "openalex", config.Source{Enabled: true}, 0, server.Client())
	if err != nil {
		t.Fatal(err)
	}
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
	client, cerr := New(&recorder{err: refusal}, "openalex", config.Source{Enabled: true}, 0, server.Client())
	if cerr != nil {
		t.Fatal(cerr)
	}
	resp, err := client.Do(request(t, server.URL))
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("a refused reservation returned a response; nothing should have been sent")
	}
	if !errors.Is(err, refusal) {
		t.Fatalf("err = %v, want the reservation error unwrapped so callers can classify a rate limit", err)
	}
	if served != 0 {
		t.Fatalf("provider served %d requests behind a closed gate, want 0", served)
	}
}

// A missing reserver must be a construction error, not a silent bypass. This
// test previously asserted the opposite -- that New returned the inner client
// unwrapped -- which pinned the exact defect the package exists to prevent: a
// provider client that works perfectly and is invisible to accounting.
func TestAMissingReserverIsAConstructionError(t *testing.T) {
	if _, err := New(nil, "openalex", config.Source{}, 0, http.DefaultClient); err == nil {
		t.Fatal("New with no reserver succeeded; an unaccounted provider client must not be constructible")
	}
	if _, err := New(&recorder{}, "openalex", config.Source{}, 0, nil); err == nil {
		t.Fatal("New with no inner client succeeded")
	}
	if _, err := New(&recorder{}, "openalex", config.Source{Enabled: true}, 0, http.DefaultClient); err != nil {
		t.Fatalf("New with both dependencies = %v, want success", err)
	}
}
