// Copyright 2026 OrgMentem. Licensed under MIT.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"papio/internal/illiad"
)

type reconcileFakeClient struct {
	transactions []illiad.Transaction
	err          error
	gets         int
	lists        int
}

func (f *reconcileFakeClient) GetTransaction(context.Context, int) (illiad.Transaction, error) {
	f.gets++
	if f.err != nil {
		return illiad.Transaction{}, f.err
	}
	if len(f.transactions) == 0 {
		return illiad.Transaction{}, illiad.ErrNotFound
	}
	return f.transactions[0], nil
}
func (f *reconcileFakeClient) UserRequests(context.Context, string) ([]illiad.Transaction, error) {
	f.lists++
	if f.err != nil {
		return nil, f.err
	}
	return f.transactions, nil
}

func newReconcileRequest(t *testing.T, svc *Service, jobID string) *Request {
	t.Helper()
	ctx := context.Background()
	testJob(t, svc, jobID)
	if _, err := svc.store.DB().ExecContext(ctx, `UPDATE jobs SET state = 'resolving' WHERE id = ?`, jobID); err != nil {
		t.Fatal(err)
	}
	return mustCreateRequest(t, svc, CreateRequest{
		JobID: jobID, InstitutionProfile: "default", Provider: "illiad",
		RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1000/reconcile",
		GateProfileDigest: "binding",
	})
}

func mustCreateRequest(t *testing.T, svc *Service, input CreateRequest) *Request {
	t.Helper()
	req, err := svc.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func reconcileDeps(client TransactionLookup) ReconciliationDeps {
	return ReconciliationDeps{
		Client: client, PatronRef: "patron-1", ReferenceField: "ItemInfo4",
		Identity:   ReconciliationIdentity{DOI: "10.1000/reconcile", RequestClass: "digital_journal_article"},
		GateAction: ActionSubmit, CurrentBinding: "binding",
	}
}

func TestReconcileAdoptsExactTokenWithReadOnlyProviderCalls(t *testing.T) {
	svc := testService(t, time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC))
	ctx := context.Background()
	req := newReconcileRequest(t, svc, "job_reconcile_adopt")
	client := &reconcileFakeClient{transactions: []illiad.Transaction{{
		TransactionNumber: 901, TransactionStatus: "Awaiting Request Processing", RequestType: "Article",
		DOI: "10.1000/reconcile", ItemInfo4: req.IdempotencyKey,
	}}}
	result, err := svc.Reconcile(ctx, req, reconcileDeps(client))
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReconciliationAdopted || result.ProviderReference != "901" {
		t.Fatalf("result = %+v, want adopted reference 901", result)
	}
	if client.gets != 0 || client.lists != 1 {
		t.Fatalf("provider calls = GET %d, patron-list %d, want GET 0, patron-list 1", client.gets, client.lists)
	}
	got, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderReference != "901" || got.State != StateSubmitted {
		t.Fatalf("request after adoption = %+v, want submitted/901", got)
	}
}

func TestReconcileRejectsMultipleExactTokenMatches(t *testing.T) {
	svc := testService(t, time.Now())
	req := newReconcileRequest(t, svc, "job_reconcile_duplicate")
	client := &reconcileFakeClient{transactions: []illiad.Transaction{
		{TransactionNumber: 1, TransactionStatus: "Awaiting Request Processing", ItemInfo4: req.IdempotencyKey},
		{TransactionNumber: 2, TransactionStatus: "Awaiting Request Processing", ItemInfo4: req.IdempotencyKey},
	}}
	result, err := svc.Reconcile(context.Background(), req, reconcileDeps(client))
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReconciliationNeedsHuman || result.Reason != ReconciliationReasonTokenAmbiguous {
		t.Fatalf("result = %+v, want NEEDS_HUMAN/multiple_token_matches", result)
	}
}

func TestReconcileTreatsReadFailureAsHumanNotNotFound(t *testing.T) {
	svc := testService(t, time.Now())
	req := newReconcileRequest(t, svc, "job_reconcile_error")
	client := &reconcileFakeClient{err: errors.New("bounded response")}
	result, err := svc.Reconcile(context.Background(), req, reconcileDeps(client))
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReconciliationNeedsHuman || result.Reason != ReconciliationReasonReadFailed {
		t.Fatalf("result = %+v, want NEEDS_HUMAN/reconciliation_read_failed", result)
	}
}

func TestReconcileReturnsNotFoundYetForCompleteEmptyList(t *testing.T) {
	svc := testService(t, time.Now())
	req := newReconcileRequest(t, svc, "job_reconcile_absent")
	result, err := svc.Reconcile(context.Background(), req, reconcileDeps(&reconcileFakeClient{}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReconciliationNotFoundYet {
		t.Fatalf("result = %+v, want NOT_FOUND_YET", result)
	}
}

func TestReconcileRejectsContradictoryMetadata(t *testing.T) {
	tests := []struct {
		name          string
		contradiction func(*illiad.Transaction)
	}{
		{
			name: "DOI",
			contradiction: func(tx *illiad.Transaction) {
				tx.DOI = "10.1000/other"
			},
		},
		{
			name: "title and author",
			contradiction: func(tx *illiad.Transaction) {
				tx.PhotoArticleTitle, tx.PhotoArticleAuthor = "Different title", "Different author"
			},
		},
		{
			name: "request type",
			contradiction: func(tx *illiad.Transaction) {
				tx.RequestType = "Loan"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := testService(t, time.Now())
			req := newReconcileRequest(t, svc, "job_reconcile_contradictory_metadata")
			transaction := illiad.Transaction{
				TransactionNumber: 7, TransactionStatus: "Awaiting Request Processing",
				ItemInfo4: req.IdempotencyKey,
			}
			tc.contradiction(&transaction)
			client := &reconcileFakeClient{transactions: []illiad.Transaction{transaction}}
			deps := reconcileDeps(client)
			deps.Identity.Title, deps.Identity.Author = "Expected title", "Expected author"
			result, err := svc.Reconcile(context.Background(), req, deps)
			if err != nil {
				t.Fatal(err)
			}
			if result.Disposition != ReconciliationNeedsHuman || result.Reason != ReconciliationReasonIdentityMismatch {
				t.Fatalf("result = %+v, want NEEDS_HUMAN/strong_identity_mismatch", result)
			}
		})
	}
}

func TestReconcileDoesNotAdoptTitleAuthorWithoutToken(t *testing.T) {
	svc := testService(t, time.Now())
	req := newReconcileRequest(t, svc, "job_reconcile_title_only")
	client := &reconcileFakeClient{transactions: []illiad.Transaction{{
		TransactionNumber: 8, TransactionStatus: "Awaiting Request Processing",
		PhotoArticleTitle: "A title", PhotoArticleAuthor: "An author",
	}}}
	deps := reconcileDeps(client)
	deps.Identity.Title, deps.Identity.Author = "A title", "An author"
	result, err := svc.Reconcile(context.Background(), req, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReconciliationNeedsHuman || result.Reason != ReconciliationReasonTokenMissing {
		t.Fatalf("result = %+v, want NEEDS_HUMAN/token_missing", result)
	}
}

func TestReconcileRejectsUnconfiguredReferenceField(t *testing.T) {
	svc := testService(t, time.Now())
	req := newReconcileRequest(t, svc, "job_reconcile_field")
	deps := reconcileDeps(&reconcileFakeClient{})
	deps.ReferenceField = ""
	result, err := svc.Reconcile(context.Background(), req, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReconciliationNeedsHuman || result.Reason != ReconciliationReasonReferenceField {
		t.Fatalf("result = %+v, want NEEDS_HUMAN/reference_field_unconfigured_or_changed", result)
	}
}

// A request that already names a provider reference is reconciled by one GET
// of that transaction, never by the patron list, and each GET outcome maps to
// its own disposition: a match is adopted, an unindexed reference is retried,
// and anything that cannot be read or does not match goes to a human.
func TestReconcileLooksUpAKnownProviderReferenceDirectly(t *testing.T) {
	matching := illiad.Transaction{
		TransactionNumber: 901, TransactionStatus: "Awaiting Request Processing", RequestType: "Article",
		DOI: "10.1000/reconcile",
	}
	tests := []struct {
		name        string
		reference   string
		client      *reconcileFakeClient
		wantGets    int
		disposition ReconciliationDisposition
		reason      string
		adopted     string
	}{
		{name: "match adopted", reference: "901", client: &reconcileFakeClient{transactions: []illiad.Transaction{matching}},
			wantGets: 1, disposition: ReconciliationAdopted, adopted: "901"},
		{name: "not indexed yet", reference: "901", client: &reconcileFakeClient{},
			wantGets: 1, disposition: ReconciliationNotFoundYet, reason: "provider_reference_not_indexed"},
		{name: "read failure", reference: "901", client: &reconcileFakeClient{err: errors.New("bounded response")},
			wantGets: 1, disposition: ReconciliationNeedsHuman, reason: ReconciliationReasonReadFailed},
		{name: "non-numeric reference", reference: "TN-901", client: &reconcileFakeClient{transactions: []illiad.Transaction{matching}},
			wantGets: 0, disposition: ReconciliationNeedsHuman, reason: ReconciliationReasonReadFailed},
		{name: "token mismatch", reference: "901", client: &reconcileFakeClient{transactions: []illiad.Transaction{{
			TransactionNumber: 901, TransactionStatus: "Awaiting Request Processing", ItemInfo4: "someone-else",
		}}}, wantGets: 1, disposition: ReconciliationNeedsHuman, reason: ReconciliationReasonIdentityMismatch},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := testService(t, time.Now())
			ctx := context.Background()
			req := newReconcileRequest(t, svc, fmt.Sprintf("job_reconcile_direct_%d", i))
			if tc.client.transactions != nil && tc.client.transactions[0].ItemInfo4 == "" {
				tc.client.transactions[0].ItemInfo4 = req.IdempotencyKey
			}
			known := *req
			known.ProviderReference = tc.reference
			result, err := svc.Reconcile(ctx, &known, reconcileDeps(tc.client))
			if err != nil {
				t.Fatal(err)
			}
			if result.Disposition != tc.disposition || result.Reason != tc.reason || result.ProviderReference != tc.adopted {
				t.Fatalf("result = %+v, want %s/%q reference %q", result, tc.disposition, tc.reason, tc.adopted)
			}
			if tc.client.gets != tc.wantGets || tc.client.lists != 0 {
				t.Fatalf("provider calls = GET %d, patron-list %d, want GET %d, patron-list 0", tc.client.gets, tc.client.lists, tc.wantGets)
			}
			got, err := svc.Get(ctx, req.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantState, wantRef := StateOffered, ""
			if tc.adopted != "" {
				wantState, wantRef = StateSubmitted, tc.adopted
			}
			if got.State != wantState || got.ProviderReference != wantRef {
				t.Fatalf("request = %s/%q, want %s/%q", got.State, got.ProviderReference, wantState, wantRef)
			}
		})
	}
}

// A reconciliation that finds the transaction but loses the RecordSubmission
// CAS to another writer must hand the request to a human and name the
// reference it found, never overwrite the reference the winner recorded.
func TestReconcileReportsCommitConflictWithoutOverwriting(t *testing.T) {
	svc := testService(t, time.Now())
	ctx := context.Background()
	req := newReconcileRequest(t, svc, "job_reconcile_conflict")
	won, err := svc.RecordSubmission(ctx, req.ID, "555", time.Now().Add(time.Hour))
	if err != nil || !won {
		t.Fatalf("seed submission = %v, %v; want won", won, err)
	}
	client := &reconcileFakeClient{transactions: []illiad.Transaction{{
		TransactionNumber: 901, TransactionStatus: "Awaiting Request Processing", RequestType: "Article",
		DOI: "10.1000/reconcile", ItemInfo4: req.IdempotencyKey,
	}}}
	// req is the stale offered snapshot read before the other writer won.
	result, err := svc.Reconcile(ctx, req, reconcileDeps(client))
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReconciliationNeedsHuman || result.Reason != ReconciliationReasonCommitConflict || result.ProviderReference != "901" {
		t.Fatalf("result = %+v, want NEEDS_HUMAN/%s naming 901", result, ReconciliationReasonCommitConflict)
	}
	got, err := svc.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSubmitted || got.ProviderReference != "555" {
		t.Fatalf("request = %s/%q, want the winner's submitted/555 preserved", got.State, got.ProviderReference)
	}
}

func TestReconcileHTTPAdoptionSendsOnlyGETs(t *testing.T) {
	svc := testService(t, time.Now())
	req := newReconcileRequest(t, svc, "job_reconcile_http")
	var methods, paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/Users/ExternalUserId/patron-1":
			_, _ = w.Write([]byte(`{"UserName":"patron1"}`))
		case "/Transaction/UserRequests/patron1":
			_, _ = w.Write([]byte(`[{"TransactionNumber":901,"TransactionStatus":"Awaiting Request Processing","RequestType":"Article","DOI":"10.1000/reconcile","ItemInfo4":"` + req.IdempotencyKey + `"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	deps := reconcileDeps(illiad.New(server.Client(), server.URL, "key"))
	result, err := svc.Reconcile(context.Background(), req, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != ReconciliationAdopted {
		t.Fatalf("result = %+v, want ADOPTED", result)
	}
	if len(methods) != 2 || methods[0] != http.MethodGet || methods[1] != http.MethodGet {
		t.Fatalf("methods = %v, want exactly two GETs", methods)
	}
	postCount := 0
	for _, method := range methods {
		if method == http.MethodPost {
			postCount++
		}
	}
	if postCount != 0 {
		t.Fatalf("POST count = %d, want zero", postCount)
	}
	if len(paths) != 2 || paths[0] != "/Users/ExternalUserId/patron-1" || paths[1] != "/Transaction/UserRequests/patron1" {
		t.Fatalf("paths = %v, want external-user resolution then patron list", paths)
	}
}
