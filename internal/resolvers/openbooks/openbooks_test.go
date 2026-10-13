package openbooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"papio/internal/resolver"
	"papio/internal/resolvers/resolvertest"
	"papio/internal/work"
)

const (
	testISBN   = "9789240114487"
	itemUUID   = "cef28a0f-ca49-4d96-9dc1-3a156ed228e4"
	otherUUID  = "26c254eb-82ca-429f-bad8-e71300aebe12"
	pdfUUID    = "5897b3c7-2848-47a7-ba22-0a7902342a81"
	secondUUID = "91496fd7-fe3f-47e5-a187-18cf537e50f8"
	ccURI      = "https://creativecommons.org/licenses/by-nc-sa/3.0/igo"
)

// ---- DSpace 7 fixtures ----------------------------------------------------

type meta map[string][]string

func (m meta) json() map[string][]map[string]any {
	out := make(map[string][]map[string]any, len(m))
	for key, values := range m {
		for _, v := range values {
			out[key] = append(out[key], map[string]any{"value": v})
		}
	}
	return out
}

type dspaceItemFixture struct {
	uuid, handle string
	metadata     meta
}

func searchBody(items ...dspaceItemFixture) string {
	objects := []any{}
	for _, it := range items {
		objects = append(objects, map[string]any{"_embedded": map[string]any{"indexableObject": map[string]any{
			"uuid": it.uuid, "handle": it.handle, "type": "item", "withdrawn": false, "inArchive": true,
			"metadata": it.metadata.json(),
		}}})
	}
	return mustJSON(map[string]any{"_embedded": map[string]any{"searchResult": map[string]any{
		"_embedded": map[string]any{"objects": objects},
	}}})
}

type streamFixture struct{ uuid, name, mime string }

func bundlesBody(bundles map[string][]streamFixture) string {
	list := []any{}
	for name, streams := range bundles {
		items := []any{}
		for _, s := range streams {
			items = append(items, map[string]any{
				"uuid": s.uuid, "name": s.name,
				"_embedded": map[string]any{"format": map[string]any{"mimetype": s.mime}},
			})
		}
		list = append(list, map[string]any{"name": name, "_embedded": map[string]any{"bitstreams": map[string]any{
			"_embedded": map[string]any{"bitstreams": items},
			"page":      map[string]any{"totalElements": len(items)},
		}}})
	}
	return mustJSON(map[string]any{
		"_embedded": map[string]any{"bundles": list},
		"page":      map[string]any{"totalElements": len(list)},
	})
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func whoMetadata() meta {
	return meta{
		"dc.title":              {"Mental health atlas 2024"},
		"dc.contributor.author": {"World Health Organization"},
		"dc.date.issued":        {"2025-08-29"},
		"dc.identifier.isbn":    {testISBN + " (electronic version)", "9789240114494 (print version)"},
		"dc.identifier.uri":     {"https://iris.example.org/handle/10665/382452"},
		"dc.rights":             {"CC BY-NC-SA 3.0 IGO"},
		"dc.rights.uri":         {ccURI},
		"dc.type":               {"Publications"},
	}
}

type recorder struct {
	mu       sync.Mutex
	requests []string
}

func (r *recorder) add(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, path)
}

func (r *recorder) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

// dspaceServer serves one DSpace 7 REST root at /server.
func dspaceServer(t *testing.T, search string, bundles map[string]string) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/server/api/discover/search/objects", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		if got := r.URL.Query().Get("query"); got != testISBN {
			t.Errorf("search query = %q, want the bare ISBN", got)
		}
		if got := r.URL.Query().Get("dsoType"); got != "ITEM" {
			t.Errorf("dsoType = %q, want ITEM", got)
		}
		_, _ = w.Write([]byte(search))
	})
	mux.HandleFunc("/server/api/core/items/", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		for uuid, body := range bundles {
			if r.URL.Path == "/server/api/core/items/"+uuid+"/bundles" {
				if r.URL.Query().Get("embed") != "bitstreams/format" {
					t.Errorf("embed = %q", r.URL.Query().Get("embed"))
				}
				_, _ = w.Write([]byte(body))
				return
			}
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

func newResolver(client HTTPClient, opts Options) *Resolver {
	opts.Client = client
	opts.RequestGap = -1
	return NewWithOptions(opts)
}

func isbnOnly() work.Work { return work.Work{ISBN: testISBN} }

func TestDSpaceHitCarriesISBNBindingAndOriginalPDF(t *testing.T) {
	srv, rec := dspaceServer(t,
		searchBody(
			// Full-text neighbour: names the ISBN in prose, lists a different one.
			dspaceItemFixture{otherUUID, "10665/383076", meta{"dc.title": {"Another book"}, "dc.identifier.isbn": {"9789240115873 (electronic version)"}, "dc.rights.uri": {ccURI}}},
			dspaceItemFixture{itemUUID, "10665/382452", whoMetadata()},
		),
		map[string]string{itemUUID: bundlesBody(map[string][]streamFixture{
			"ORIGINAL":  {{pdfUUID, "9789240114487-eng.pdf", "application/pdf"}},
			"LICENSE":   {{"c0661cbe-0f6a-41e6-b12e-23966ac76ef0", "license.txt", "text/plain"}},
			"TEXT":      {{secondUUID, "9789240114487-eng.pdf.txt", "text/plain"}},
			"THUMBNAIL": {{"bcfa2fef-5888-47bc-b64a-9edd8dd95723", "9789240114487-eng.pdf.jpg", "image/jpeg"}},
		})},
	)
	r := newResolver(srv.Client(), Options{DSpaceServers: []string{srv.URL + "/server"}})

	candidates, err := r.Resolve(context.Background(), isbnOnly())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	c := candidates[0]
	if want := srv.URL + "/server/api/core/bitstreams/" + pdfUUID + "/content"; c.URL != want {
		t.Errorf("URL = %q, want %q", c.URL, want)
	}
	if c.Source != SourceName || !c.Direct || c.ExpectedMIME != "application/pdf" || c.AccessBasis != resolver.AccessOpen {
		t.Errorf("candidate shape = %+v", c)
	}
	if c.ReuseLicense != "cc-by-nc-sa-3.0-igo" {
		t.Errorf("licence = %q", c.ReuseLicense)
	}
	if c.Landing != "https://iris.example.org/handle/10665/382452" {
		t.Errorf("landing = %q", c.Landing)
	}
	if c.ISBNBinding == nil || c.ISBNBinding.ISBN != testISBN || c.ISBNBinding.Title != "Mental health atlas 2024" {
		t.Errorf("binding = %+v", c.ISBNBinding)
	}
	if !c.Authority.MayPromoteIdentity() {
		t.Errorf("authority = %q, want exact echo", c.Authority)
	}
	resolved := c.ResolvedWork
	if resolved.ISBN != testISBN || resolved.Title != "Mental health atlas 2024" || resolved.Year != 2025 ||
		len(resolved.Authors) != 1 || resolved.DOI != "" {
		t.Errorf("resolved work = %+v (a repository record must never mint a DOI)", resolved)
	}
	if err := resolver.ValidateCandidate(c); err != nil {
		t.Errorf("candidate fails the source boundary: %v", err)
	}
	if got := rec.paths(); len(got) != 2 {
		t.Errorf("requests = %v, want one search and one bundles read", got)
	}
}

func TestDSpaceSearchIsFullTextSoRecordISBNMustMatch(t *testing.T) {
	// Each hit the search returned lists some OTHER ISBN, or none. They are
	// tried one at a time: two bad hits together would be refused as ambiguous
	// whether or not the ISBN were compared, which proves nothing.
	neighbours := map[string]meta{
		"another ISBN":  {"dc.title": {"Cites the requested book"}, "dc.identifier.isbn": {"9789240115873"}, "dc.rights.uri": {ccURI}},
		"no ISBN field": {"dc.title": {"Journal issue"}, "dc.rights.uri": {ccURI}},
		"ISBN only in prose": {"dc.title": {"Cites the requested book"}, "dc.description.abstract": {"See ISBN " + testISBN},
			"dc.rights.uri": {ccURI}},
	}
	for name, metadata := range neighbours {
		t.Run(name, func(t *testing.T) {
			srv, rec := dspaceServer(t,
				searchBody(dspaceItemFixture{itemUUID, "10665/382452", metadata}),
				map[string]string{itemUUID: bundlesBody(map[string][]streamFixture{"ORIGINAL": {{pdfUUID, "a.pdf", "application/pdf"}}})},
			)
			r := newResolver(srv.Client(), Options{DSpaceServers: []string{srv.URL + "/server"}})
			candidates, err := r.Resolve(context.Background(), isbnOnly())
			if err != nil || len(candidates) != 0 {
				t.Fatalf("candidates = %v, err = %v; want none", candidates, err)
			}
			if got := rec.paths(); len(got) != 1 {
				t.Errorf("requests = %v: a mismatched record must not cost a bundles read", got)
			}
		})
	}
}

func TestDSpaceRefusals(t *testing.T) {
	original := func(streams ...streamFixture) map[string]string {
		return map[string]string{itemUUID: bundlesBody(map[string][]streamFixture{"ORIGINAL": streams})}
	}
	withRights := func(rights ...string) meta {
		m := whoMetadata()
		delete(m, "dc.rights")
		delete(m, "dc.rights.uri")
		if len(rights) > 0 {
			m["dc.rights"] = rights
		}
		return m
	}
	pdf := streamFixture{pdfUUID, "book.pdf", "application/pdf"}
	tests := []struct {
		name        string
		item        meta
		bundles     map[string]string
		wantBundles bool
	}{
		{"no PDF in the record", whoMetadata(), original(streamFixture{pdfUUID, "book.epub", "application/epub+zip"}), true},
		{"PDF only outside ORIGINAL", whoMetadata(), map[string]string{itemUUID: bundlesBody(map[string][]streamFixture{
			"TEXT": {{pdfUUID, "book.pdf", "application/pdf"}}, "THUMBNAIL": {{secondUUID, "book.pdf.jpg", "image/jpeg"}},
		})}, true},
		{"closed licence", withRights("All rights reserved"), original(pdf), false},
		{"no rights statement", withRights(), original(pdf), false},
		{"licence with an embargo", withRights("CC BY 4.0, embargoed until 2099"), original(pdf), false},
		{"copyright line is not a licence", withRights("© World Health Organization 2025"), original(pdf), false},
		{"several PDFs are ambiguous", whoMetadata(), original(pdf, streamFixture{secondUUID, "preview.pdf", "application/pdf"}), true},
		{"chapter record", func() meta { m := whoMetadata(); m["dc.type"] = []string{"Book chapter"}; return m }(), original(pdf), false},
		{"record without a title", func() meta { m := whoMetadata(); delete(m, "dc.title"); return m }(), original(pdf), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := dspaceServer(t, searchBody(dspaceItemFixture{itemUUID, "10665/382452", tt.item}), tt.bundles)
			r := newResolver(srv.Client(), Options{DSpaceServers: []string{srv.URL + "/server"}})
			candidates, err := r.Resolve(context.Background(), isbnOnly())
			if err != nil || len(candidates) != 0 {
				t.Fatalf("candidates = %v, err = %v; want none", candidates, err)
			}
			bundlesRead := len(rec.paths()) > 1
			if bundlesRead != tt.wantBundles {
				t.Errorf("bundles read = %v, want %v (requests %v)", bundlesRead, tt.wantBundles, rec.paths())
			}
		})
	}
}

func TestDSpaceTwoRecordsListingOneISBNAreRefused(t *testing.T) {
	srv, _ := dspaceServer(t,
		searchBody(
			dspaceItemFixture{itemUUID, "10665/382452", whoMetadata()},
			dspaceItemFixture{otherUUID, "10665/999999", whoMetadata()},
		),
		map[string]string{
			itemUUID:  bundlesBody(map[string][]streamFixture{"ORIGINAL": {{pdfUUID, "a.pdf", "application/pdf"}}}),
			otherUUID: bundlesBody(map[string][]streamFixture{"ORIGINAL": {{secondUUID, "b.pdf", "application/pdf"}}}),
		},
	)
	r := newResolver(srv.Client(), Options{DSpaceServers: []string{srv.URL + "/server"}})
	if candidates, err := r.Resolve(context.Background(), isbnOnly()); err != nil || len(candidates) != 0 {
		t.Fatalf("candidates = %v, err = %v; want none", candidates, err)
	}
}

func TestOnlyISBNOnlyWorksReachARepository(t *testing.T) {
	var called bool
	client := clientFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, fmt.Errorf("must not be called")
	})
	r := newResolver(client, Options{DSpaceServers: []string{"https://repo.example/server"}, OAPEN: true})
	for _, w := range []work.Work{
		{DOI: "10.1000/chapter", ISBN: testISBN},
		{PMID: "123", ISBN: testISBN},
		{ArXiv: "2301.08745", ISBN: testISBN},
		{OpenAlex: "W2741809807", ISBN: testISBN},
		{Title: "A title without an ISBN"},
		{ISBN: "not-an-isbn"},
	} {
		if candidates, err := r.Resolve(context.Background(), w); err != nil || candidates != nil {
			t.Fatalf("Resolve(%+v) = %v, %v; want (nil, nil)", w, candidates, err)
		}
	}
	if called {
		t.Fatal("a work with another strong identifier, or no ISBN, reached a repository")
	}
}

func TestSecondDSpaceServerIsAskedWhenTheFirstHasNothing(t *testing.T) {
	empty, emptyRec := dspaceServer(t, searchBody(), nil)
	hit, _ := dspaceServer(t, searchBody(dspaceItemFixture{itemUUID, "10665/382452", whoMetadata()}),
		map[string]string{itemUUID: bundlesBody(map[string][]streamFixture{"ORIGINAL": {{pdfUUID, "book.pdf", "application/pdf"}}})})
	r := newResolver(http.DefaultClient, Options{DSpaceServers: []string{empty.URL + "/server", hit.URL + "/server"}})
	candidates, err := r.Resolve(context.Background(), isbnOnly())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %v, err = %v; want one", candidates, err)
	}
	if !strings.HasPrefix(candidates[0].URL, hit.URL) || len(emptyRec.paths()) != 1 {
		t.Errorf("URL %q, first-server requests %v", candidates[0].URL, emptyRec.paths())
	}
}

// ---- OAPEN (DSpace 6 REST) ------------------------------------------------

type oapenItemFixture struct {
	uuid, handle string
	metadata     [][2]string
	bitstreams   []map[string]string
}

func oapenBody(items ...oapenItemFixture) string {
	list := []any{}
	for _, it := range items {
		md := []any{}
		for _, kv := range it.metadata {
			md = append(md, map[string]any{"key": kv[0], "value": kv[1], "language": nil})
		}
		streams := []any{}
		for _, s := range it.bitstreams {
			streams = append(streams, s)
		}
		list = append(list, map[string]any{
			"uuid": it.uuid, "handle": it.handle, "type": "item", "withdrawn": "false",
			"metadata": md, "bitstreams": streams,
		})
	}
	return mustJSON(list)
}

func oapenPDF(name, uuid, retrieve string) map[string]string {
	return map[string]string{"uuid": uuid, "name": name, "bundleName": "ORIGINAL", "mimeType": "application/pdf", "retrieveLink": retrieve}
}

func oapenMetadata() [][2]string {
	return [][2]string{
		{"dc.title", "An Open Monograph"},
		{"dc.contributor.author", "Doe, Jane"},
		{"dc.date.issued", "2021"},
		{"dc.identifier.isbn", "9783110419191"},
		{"dc.rights", "https://creativecommons.org/licenses/by/4.0/"},
		{"dc.type", "book"},
	}
}

func oapenServer(t *testing.T, status int, body string) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		if r.URL.Path != "/rest/search" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("query") != `dc.identifier.isbn:"9783110419191"` || q.Get("expand") != "metadata,bitstreams" {
			t.Errorf("query = %v", q)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestOAPENHit(t *testing.T) {
	srv, _ := oapenServer(t, 200, oapenBody(oapenItemFixture{
		uuid: itemUUID, handle: "20.500.12657/47285", metadata: oapenMetadata(),
		bitstreams: []map[string]string{
			oapenPDF("book.pdf", pdfUUID, "/bitstreams/"+pdfUUID+"/retrieve"),
			{"uuid": secondUUID, "name": "cover.jpg", "bundleName": "ORIGINAL", "mimeType": "image/jpeg", "retrieveLink": "/bitstreams/" + secondUUID + "/retrieve"},
			{"uuid": otherUUID, "name": "book.pdf.txt", "bundleName": "TEXT", "mimeType": "text/plain", "retrieveLink": "/bitstreams/" + otherUUID + "/retrieve"},
		},
	}))
	r := newResolver(http.DefaultClient, Options{OAPEN: true, OAPENBaseURL: srv.URL + "/rest"})
	candidates, err := r.Resolve(context.Background(), work.Work{ISBN: "9783110419191"})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %v, err = %v; want one", candidates, err)
	}
	c := candidates[0]
	if want := srv.URL + "/rest/bitstreams/" + pdfUUID + "/retrieve"; c.URL != want {
		t.Errorf("URL = %q, want %q", c.URL, want)
	}
	if c.ReuseLicense != "cc-by-4.0" || c.ISBNBinding == nil || c.ISBNBinding.Title != "An Open Monograph" {
		t.Errorf("licence %q binding %+v", c.ReuseLicense, c.ISBNBinding)
	}
	if c.Landing != srv.URL+"/handle/20.500.12657/47285" || c.ResolvedWork.Year != 2021 {
		t.Errorf("landing %q year %d", c.Landing, c.ResolvedWork.Year)
	}

	// The same file addressed with the REST prefix already in the link.
	prefixed, _ := oapenServer(t, 200, oapenBody(oapenItemFixture{
		uuid: itemUUID, handle: "20.500.12657/47285", metadata: oapenMetadata(),
		bitstreams: []map[string]string{oapenPDF("book.pdf", pdfUUID, "/rest/bitstreams/"+pdfUUID+"/retrieve")},
	}))
	got, err := newResolver(http.DefaultClient, Options{OAPEN: true, OAPENBaseURL: prefixed.URL + "/rest"}).
		Resolve(context.Background(), work.Work{ISBN: "9783110419191"})
	if err != nil || len(got) != 1 || got[0].URL != prefixed.URL+"/rest/bitstreams/"+pdfUUID+"/retrieve" {
		t.Fatalf("prefixed link: %v, %v", got, err)
	}
}

func TestOAPENRefusals(t *testing.T) {
	book := func(mutate func(*oapenItemFixture)) string {
		it := oapenItemFixture{
			uuid: itemUUID, handle: "20.500.12657/47285", metadata: oapenMetadata(),
			bitstreams: []map[string]string{oapenPDF("book.pdf", pdfUUID, "/bitstreams/"+pdfUUID+"/retrieve")},
		}
		mutate(&it)
		return oapenBody(it)
	}
	tests := map[string]string{
		"ISBN mismatch": book(func(it *oapenItemFixture) {
			it.metadata = [][2]string{{"dc.title", "Other"}, {"dc.identifier.isbn", "9783110419207"}}
		}),
		"no PDF": book(func(it *oapenItemFixture) {
			it.bitstreams = []map[string]string{{"uuid": secondUUID, "name": "book.epub", "bundleName": "ORIGINAL", "mimeType": "application/epub+zip", "retrieveLink": "/bitstreams/" + secondUUID + "/retrieve"}}
		}),
		"multiple PDFs": book(func(it *oapenItemFixture) {
			it.bitstreams = append(it.bitstreams, oapenPDF("part2.pdf", secondUUID, "/bitstreams/"+secondUUID+"/retrieve"))
		}),
		"chapter record": book(func(it *oapenItemFixture) {
			it.metadata = append(it.metadata, [2]string{"oapen.relation.isPartOfBook", "20.500.12657/1"})
		}),
		"unrecognised retrieve link": book(func(it *oapenItemFixture) {
			it.bitstreams = []map[string]string{oapenPDF("book.pdf", pdfUUID, "https://elsewhere.example/steal.pdf")}
		}),
		"empty result": `[]`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			srv, _ := oapenServer(t, 200, body)
			r := newResolver(http.DefaultClient, Options{OAPEN: true, OAPENBaseURL: srv.URL + "/rest"})
			if candidates, err := r.Resolve(context.Background(), work.Work{ISBN: "9783110419191"}); err != nil || len(candidates) != 0 {
				t.Fatalf("candidates = %v, err = %v; want none", candidates, err)
			}
		})
	}
}

func TestOAPENIsOffUnlessEnabled(t *testing.T) {
	srv, rec := oapenServer(t, 200, `[]`)
	r := newResolver(http.DefaultClient, Options{OAPEN: false, OAPENBaseURL: srv.URL + "/rest"})
	if _, err := r.Resolve(context.Background(), work.Work{ISBN: "9783110419191"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.paths()) != 0 {
		t.Fatalf("requests = %v with OAPEN disabled", rec.paths())
	}
}

// ---- failures and pacing --------------------------------------------------

type clientFunc func(*http.Request) (*http.Response, error)

func (f clientFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestFailureClassification(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		header    string
		wantErr   bool
		temporary bool
		retry     time.Duration
	}{
		{"refused network is permanent", 403, "", true, false, 0},
		{"unauthorised is permanent", 401, "", true, false, 0},
		{"rate limited is retryable", 429, "7", true, true, 7 * time.Second},
		{"upstream failure is retryable", 503, "", true, true, 0},
		{"not found is absence", 404, "", false, false, 0},
		{"other status is permanent", 418, "", true, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			r := newResolver(http.DefaultClient, Options{DSpaceServers: []string{srv.URL + "/server"}})
			candidates, err := r.Resolve(context.Background(), isbnOnly())
			if len(candidates) != 0 || (err != nil) != tt.wantErr {
				t.Fatalf("candidates = %v, err = %v", candidates, err)
			}
			if !tt.wantErr {
				return
			}
			retry, temporary := resolver.Temporary(err)
			if temporary != tt.temporary || (tt.retry != 0 && retry != tt.retry) {
				t.Fatalf("temporary = %v/%v, want %v/%v", temporary, retry, tt.temporary, tt.retry)
			}
		})
	}
}

func TestOneRepositoryFailingDoesNotHideAnotherRepositorysHit(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer down.Close()
	srv, _ := oapenServer(t, 200, oapenBody(oapenItemFixture{
		uuid: itemUUID, handle: "20.500.12657/47285", metadata: oapenMetadata(),
		bitstreams: []map[string]string{oapenPDF("book.pdf", pdfUUID, "/bitstreams/"+pdfUUID+"/retrieve")},
	}))
	r := newResolver(http.DefaultClient, Options{DSpaceServers: []string{down.URL + "/server"}, OAPEN: true, OAPENBaseURL: srv.URL + "/rest"})
	candidates, err := r.Resolve(context.Background(), work.Work{ISBN: "9783110419191"})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %v, err = %v; want the OAPEN hit", candidates, err)
	}
}

func TestRetryableFailureOutranksPermanentWhenNothingWasFound(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer down.Close()
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer flaky.Close()
	r := newResolver(http.DefaultClient, Options{DSpaceServers: []string{down.URL + "/server", flaky.URL + "/server"}})
	_, err := r.Resolve(context.Background(), isbnOnly())
	if _, temporary := resolver.Temporary(err); !temporary {
		t.Fatalf("err = %v, want the retryable failure so the job is retried", err)
	}
}

func TestRequestGapPacesRequestsInsideOneLookup(t *testing.T) {
	srv, rec := dspaceServer(t, searchBody(dspaceItemFixture{itemUUID, "10665/382452", whoMetadata()}),
		map[string]string{itemUUID: bundlesBody(map[string][]streamFixture{"ORIGINAL": {{pdfUUID, "book.pdf", "application/pdf"}}})})
	const gap = 80 * time.Millisecond
	r := NewWithOptions(Options{Client: srv.Client(), DSpaceServers: []string{srv.URL + "/server"}, RequestGap: gap})
	start := time.Now()
	if candidates, err := r.Resolve(context.Background(), isbnOnly()); err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %v, err = %v", candidates, err)
	}
	if elapsed := time.Since(start); len(rec.paths()) != 2 || elapsed < gap {
		t.Fatalf("2 requests expected spaced by %v, got %d in %v", gap, len(rec.paths()), elapsed)
	}
}

func TestCancelledContextStopsTheLookup(t *testing.T) {
	srv, _ := dspaceServer(t, searchBody(dspaceItemFixture{itemUUID, "10665/382452", whoMetadata()}), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := newResolver(srv.Client(), Options{DSpaceServers: []string{srv.URL + "/server"}})
	if _, err := r.Resolve(ctx, isbnOnly()); err == nil {
		t.Fatal("a cancelled context must surface")
	}
}

func TestOversizedBodyFailsClosed(t *testing.T) {
	resolvertest.CheckOversizedBodyFailsClosed(t, searchBody(dspaceItemFixture{itemUUID, "10665/382452", whoMetadata()}),
		func(t *testing.T, maxBytes int64, body string) error {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer srv.Close()
			r := NewWithOptions(Options{Client: srv.Client(), DSpaceServers: []string{srv.URL + "/server"}, MaxResponseBytes: maxBytes, RequestGap: -1})
			_, err := r.Resolve(context.Background(), isbnOnly())
			return err
		})
}

func TestParseRetryAfterClampsHugeValues(t *testing.T) {
	resolvertest.CheckParseRetryAfterClampsHugeValues(t, parseRetryAfter)
}

func TestOpenLicence(t *testing.T) {
	tests := []struct {
		values []string
		label  string
		open   bool
	}{
		{[]string{ccURI}, "cc-by-nc-sa-3.0-igo", true},
		{[]string{"CC BY-NC-SA 3.0 IGO", ccURI}, "cc-by-nc-sa-3.0-igo", true},
		{[]string{"CC BY 4.0"}, "cc-by-4.0", true},
		{[]string{"https://creativecommons.org/publicdomain/zero/1.0/"}, "cc-zero-1.0", true},
		{[]string{"Creative Commons Attribution"}, "creative-commons-attribution", true},
		{[]string{"All rights reserved"}, "", false},
		{[]string{"CC BY 4.0", "embargoed until 2099"}, "", false},
		{[]string{"© 2025 Publisher"}, "", false},
		{nil, "", false},
	}
	for _, tt := range tests {
		label, open := openLicence(tt.values)
		if label != tt.label || open != tt.open {
			t.Errorf("openLicence(%q) = %q, %v; want %q, %v", tt.values, label, open, tt.label, tt.open)
		}
	}
}

// ---- paging ----------------------------------------------------------------

// neighbourPage is one full page of search hits that merely mention the ISBN.
func neighbourPage(n int) []dspaceItemFixture {
	items := make([]dspaceItemFixture, n)
	for i := range items {
		items[i] = dspaceItemFixture{
			fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), fmt.Sprintf("10665/%d", 100+i),
			meta{"dc.title": {"Cites the requested book"}, "dc.identifier.isbn": {"9789240115873"}, "dc.rights.uri": {ccURI}},
		}
	}
	return items
}

// pagedDSpaceServer serves search page N from pages[N] and reports totalPages
// as given (so a test can lie about it). Pages beyond the slice are empty.
func pagedDSpaceServer(t *testing.T, pages [][]dspaceItemFixture, totalPages int, bundles map[string]string) (*httptest.Server, *recorder, *[]string) {
	t.Helper()
	rec := &recorder{}
	var asked []string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/server/api/discover/search/objects", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		page := 0
		_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
		mu.Lock()
		asked = append(asked, r.URL.Query().Get("page"))
		mu.Unlock()
		var items []dspaceItemFixture
		if page < len(pages) {
			items = pages[page]
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(searchBody(items...)), &body)
		if totalPages >= 0 {
			body["_embedded"].(map[string]any)["searchResult"].(map[string]any)["page"] = map[string]any{"totalPages": totalPages, "number": page}
		}
		_, _ = w.Write([]byte(mustJSON(body)))
	})
	mux.HandleFunc("/server/api/core/items/", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		for uuid, body := range bundles {
			if r.URL.Path == "/server/api/core/items/"+uuid+"/bundles" {
				_, _ = w.Write([]byte(body))
				return
			}
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec, &asked
}

func oneBook() map[string]string {
	return map[string]string{itemUUID: bundlesBody(map[string][]streamFixture{"ORIGINAL": {{pdfUUID, "book.pdf", "application/pdf"}}})}
}

func TestDSpaceReadsEveryResultPage(t *testing.T) {
	// The record that lists the ISBN is on the SECOND page; reading only the
	// first would report "no open copy".
	srv, _, asked := pagedDSpaceServer(t, [][]dspaceItemFixture{
		neighbourPage(searchSize),
		{{itemUUID, "10665/382452", whoMetadata()}},
	}, 2, oneBook())
	r := newResolver(srv.Client(), Options{DSpaceServers: []string{srv.URL + "/server"}})
	candidates, err := r.Resolve(context.Background(), isbnOnly())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %v, err = %v; want the hit on page 2", candidates, err)
	}
	if got := strings.Join(*asked, ","); got != "0,1" {
		t.Fatalf("pages read = %q, want 0,1", got)
	}
}

func TestDSpaceAmbiguityIsJudgedAcrossAllPages(t *testing.T) {
	srv, _, _ := pagedDSpaceServer(t, [][]dspaceItemFixture{
		append(neighbourPage(searchSize-1), dspaceItemFixture{itemUUID, "10665/382452", whoMetadata()}),
		{{otherUUID, "10665/999999", whoMetadata()}},
	}, 2, oneBook())
	r := newResolver(srv.Client(), Options{DSpaceServers: []string{srv.URL + "/server"}})
	if candidates, err := r.Resolve(context.Background(), isbnOnly()); err != nil || len(candidates) != 0 {
		t.Fatalf("candidates = %v, err = %v; a second record on page 2 must make the lookup ambiguous", candidates, err)
	}
}

func TestDSpaceRefusesAResultTooLargeToRead(t *testing.T) {
	pages := make([][]dspaceItemFixture, maxSearchPages+2)
	for i := range pages {
		pages[i] = neighbourPage(searchSize)
	}
	pages[maxSearchPages+1] = []dspaceItemFixture{{itemUUID, "10665/382452", whoMetadata()}}
	srv, rec, asked := pagedDSpaceServer(t, pages, maxSearchPages+2, oneBook())
	r := newResolver(srv.Client(), Options{DSpaceServers: []string{srv.URL + "/server"}})
	if candidates, err := r.Resolve(context.Background(), isbnOnly()); err != nil || len(candidates) != 0 {
		t.Fatalf("candidates = %v, err = %v; want none", candidates, err)
	}
	if len(*asked) != maxSearchPages || len(rec.paths()) != maxSearchPages {
		t.Fatalf("pages read = %v, want exactly %d and no bundles read", *asked, maxSearchPages)
	}
}

func TestDSpaceFullPageWithoutAPageCountIsIncomplete(t *testing.T) {
	srv, _, _ := pagedDSpaceServer(t, [][]dspaceItemFixture{
		append(neighbourPage(searchSize-1), dspaceItemFixture{itemUUID, "10665/382452", whoMetadata()}),
	}, -1, oneBook())
	r := newResolver(srv.Client(), Options{DSpaceServers: []string{srv.URL + "/server"}})
	if candidates, err := r.Resolve(context.Background(), isbnOnly()); err != nil || len(candidates) != 0 {
		t.Fatalf("candidates = %v, err = %v; a full page with no page count is not known to be complete", candidates, err)
	}
	// A short page with no page count is complete.
	short, _, _ := pagedDSpaceServer(t, [][]dspaceItemFixture{{{itemUUID, "10665/382452", whoMetadata()}}}, -1, oneBook())
	r = newResolver(short.Client(), Options{DSpaceServers: []string{short.URL + "/server"}})
	if candidates, err := r.Resolve(context.Background(), isbnOnly()); err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %v, err = %v; a short page is complete", candidates, err)
	}
}

// oapenPagedServer serves OAPEN search pages by offset.
func oapenPagedServer(t *testing.T, pages []string) (*httptest.Server, *[]string) {
	t.Helper()
	var asked []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		_, _ = fmt.Sscan(r.URL.Query().Get("offset"), &offset)
		mu.Lock()
		asked = append(asked, r.URL.Query().Get("offset"))
		mu.Unlock()
		if page := offset / searchSize; page < len(pages) {
			_, _ = w.Write([]byte(pages[page]))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func oapenNeighbours(n int) []oapenItemFixture {
	items := make([]oapenItemFixture, n)
	for i := range items {
		items[i] = oapenItemFixture{
			uuid: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), handle: fmt.Sprintf("20.500.12657/%d", 100+i),
			metadata: [][2]string{{"dc.title", "Other"}, {"dc.identifier.isbn", "9783110419207"}},
		}
	}
	return items
}

func oapenBook(uuid string) oapenItemFixture {
	return oapenItemFixture{
		uuid: uuid, handle: "20.500.12657/47285", metadata: oapenMetadata(),
		bitstreams: []map[string]string{oapenPDF("book.pdf", pdfUUID, "/bitstreams/"+pdfUUID+"/retrieve")},
	}
}

func TestOAPENReadsEveryResultPage(t *testing.T) {
	srv, asked := oapenPagedServer(t, []string{oapenBody(oapenNeighbours(searchSize)...), oapenBody(oapenBook(itemUUID))})
	r := newResolver(http.DefaultClient, Options{OAPEN: true, OAPENBaseURL: srv.URL + "/rest"})
	candidates, err := r.Resolve(context.Background(), work.Work{ISBN: "9783110419191"})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %v, err = %v; want the hit on page 2", candidates, err)
	}
	if got := strings.Join(*asked, ","); got != "0,20" {
		t.Fatalf("offsets read = %q, want 0,20", got)
	}
}

func TestOAPENAmbiguityIsJudgedAcrossAllPages(t *testing.T) {
	page0 := append(oapenNeighbours(searchSize-1), oapenBook(itemUUID))
	srv, _ := oapenPagedServer(t, []string{oapenBody(page0...), oapenBody(oapenBook(otherUUID))})
	r := newResolver(http.DefaultClient, Options{OAPEN: true, OAPENBaseURL: srv.URL + "/rest"})
	if candidates, err := r.Resolve(context.Background(), work.Work{ISBN: "9783110419191"}); err != nil || len(candidates) != 0 {
		t.Fatalf("candidates = %v, err = %v; a second record on page 2 must make the lookup ambiguous", candidates, err)
	}
}

func TestOAPENRefusesAResultTooLargeToRead(t *testing.T) {
	pages := make([]string, maxSearchPages+1)
	for i := range pages {
		pages[i] = oapenBody(oapenNeighbours(searchSize)...)
	}
	pages[maxSearchPages] = oapenBody(oapenBook(itemUUID))
	srv, asked := oapenPagedServer(t, pages)
	r := newResolver(http.DefaultClient, Options{OAPEN: true, OAPENBaseURL: srv.URL + "/rest"})
	if candidates, err := r.Resolve(context.Background(), work.Work{ISBN: "9783110419191"}); err != nil || len(candidates) != 0 {
		t.Fatalf("candidates = %v, err = %v; want none", candidates, err)
	}
	if len(*asked) != maxSearchPages {
		t.Fatalf("pages read = %v, want exactly %d", *asked, maxSearchPages)
	}
}

// ---- counting PDFs ---------------------------------------------------------

// Every ORIGINAL PDF counts BEFORE its link is judged: a record with two PDFs
// is ambiguous even when one of them has a broken link, and a record's only PDF
// with a broken link is refused rather than skipped.
func TestPDFsAreCountedBeforeTheirLinksAreValidated(t *testing.T) {
	dspace := map[string][]streamFixture{
		"two PDFs, one with a malformed id":   {{pdfUUID, "a.pdf", "application/pdf"}, {"not-a-uuid", "b.pdf", "application/pdf"}},
		"only PDF has a malformed id":         {{"../../../etc/passwd", "a.pdf", "application/pdf"}},
		"second PDF declared as octet-stream": {{pdfUUID, "a.pdf", "application/pdf"}, {secondUUID, "b.pdf", "application/octet-stream"}},
		"second PDF with no declared format":  {{pdfUUID, "a.pdf", "application/pdf"}, {secondUUID, "b.PDF", ""}},
		"PDF named only by its format, twice": {{pdfUUID, "a", "application/pdf"}, {secondUUID, "b", "APPLICATION/PDF"}},
	}
	for name, streams := range dspace {
		t.Run("dspace/"+name, func(t *testing.T) {
			srv, _ := dspaceServer(t, searchBody(dspaceItemFixture{itemUUID, "10665/382452", whoMetadata()}),
				map[string]string{itemUUID: bundlesBody(map[string][]streamFixture{"ORIGINAL": streams})})
			r := newResolver(srv.Client(), Options{DSpaceServers: []string{srv.URL + "/server"}})
			if candidates, err := r.Resolve(context.Background(), isbnOnly()); err != nil || len(candidates) != 0 {
				t.Fatalf("candidates = %v, err = %v; want none", candidates, err)
			}
		})
	}
	oapen := map[string][]map[string]string{
		"two PDFs, one with a bad link": {oapenPDF("a.pdf", pdfUUID, "/bitstreams/"+pdfUUID+"/retrieve"), oapenPDF("b.pdf", secondUUID, "https://elsewhere.example/b.pdf")},
		"only PDF has a bad link":       {oapenPDF("a.pdf", pdfUUID, "/bitstreams/../../x/retrieve")},
		"second PDF declared by name":   {oapenPDF("a.pdf", pdfUUID, "/bitstreams/"+pdfUUID+"/retrieve"), {"uuid": secondUUID, "name": "b.pdf", "bundleName": "ORIGINAL", "mimeType": "application/octet-stream", "retrieveLink": "/bitstreams/" + secondUUID + "/retrieve"}},
	}
	for name, streams := range oapen {
		t.Run("oapen/"+name, func(t *testing.T) {
			item := oapenBook(itemUUID)
			item.bitstreams = streams
			srv, _ := oapenServer(t, 200, oapenBody(item))
			r := newResolver(http.DefaultClient, Options{OAPEN: true, OAPENBaseURL: srv.URL + "/rest"})
			if candidates, err := r.Resolve(context.Background(), work.Work{ISBN: "9783110419191"}); err != nil || len(candidates) != 0 {
				t.Fatalf("candidates = %v, err = %v; want none", candidates, err)
			}
		})
	}
}
