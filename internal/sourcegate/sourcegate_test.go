package sourcegate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"papio/internal/config"
	"papio/internal/fetch"
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

// hopRefuser admits the first `allow` reservations and refuses the rest.
type hopRefuser struct {
	calls int
	allow int
	err   error
}

func (r *hopRefuser) Acquire(context.Context, string, config.Source, float64) error {
	r.calls++
	if r.calls > r.allow {
		return r.err
	}
	return nil
}

// A redirect is another physical request the provider counts, so a 302 -> 302
// -> 200 chain through the production secure client needs three admissions,
// and a refusal at a redirected hop stops that hop reaching the provider.
func TestEveryRedirectHopIsAdmitted(t *testing.T) {
	served := map[string]int{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/a":
			http.Redirect(w, r, "/b", http.StatusFound)
		case "/b":
			http.Redirect(w, r, "/c", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	policy := fetch.DefaultPolicy()
	policy.AllowHTTPLoopback = true
	inner, err := fetch.NewSecureHTTPClient(policy, nil, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}

	admitAll := &hopRefuser{allow: 100}
	client, err := New(admitAll, "arxiv", config.Source{Enabled: true}, 0, inner)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(request(t, server.URL+"/a"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if admitAll.calls != 3 {
		t.Fatalf("admissions for a two-redirect chain = %d, want 3 (one per physical request)", admitAll.calls)
	}

	refusal := errors.New("source arxiv is deferred")
	refuseHop := &hopRefuser{allow: 1, err: refusal}
	client, err = New(refuseHop, "arxiv", config.Source{Enabled: true}, 0, inner)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clear(served)
	mu.Unlock()
	resp, err = client.Do(request(t, server.URL+"/a"))
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("a refused redirect hop returned a response")
	}
	if !errors.Is(err, refusal) {
		t.Fatalf("err = %v, want the hop's reservation error unwrapped", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if served["/a"] != 1 || served["/b"] != 0 || served["/c"] != 0 {
		t.Fatalf("served = %v, want only /a once behind a refused redirect hop", served)
	}
}
