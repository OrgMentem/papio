// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bootstrap

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/sourcegate"
	"papio/internal/store"
)

type headerInner struct {
	headers map[string]string
	calls   int
}

func (h *headerInner) Do(*http.Request) (*http.Response, error) {
	h.calls++
	hdr := make(http.Header)
	for k, v := range h.headers {
		hdr.Set(k, v)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: hdr, Body: http.NoBody}, nil
}

func TestWrapOpenAlexRecordsProviderFuseInputs(t *testing.T) {
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC)
	budgets := budget.New(s, budget.WithNow(func() time.Time { return now }), budget.WithCreditPolicy(func(string) budget.CreditPolicy {
		return budget.CreditPolicy{DailyCreditFraction: 0.5, DailyCreditLimit: 10_000}
	}))
	keyed := config.Source{Enabled: true, APIKey: "private-key"}
	inner := &headerInner{headers: map[string]string{
		"X-RateLimit-Limit":                 "10000",
		"X-RateLimit-Remaining":             "9000",
		"X-RateLimit-Reset":                 "3600",
		"X-RateLimit-Credits-Used":          "240",
		"X-RateLimit-Prepaid-Remaining-USD": "1.0",
	}}
	stack, err := sourcegate.WrapOpenAlex(budgets, budgets, config.SourceOpenAlex, keyed, inner)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://api.openalex.org/works", nil)
	if err != nil {
		t.Fatal(err)
	}
	sourcegate.SetOpenAlexAuthorization(req, "private-key")
	resp, err := stack.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	day := now.UTC().Format("2006-01-02")
	var denom sql.NullInt64
	var committed int
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT denominator, credits_committed FROM source_credit_fuse WHERE source = ? AND utc_day = ?`,
		config.SourceOpenAlex, day).Scan(&denom, &committed); err != nil {
		t.Fatal(err)
	}
	if !denom.Valid || denom.Int64 != 10000 {
		t.Fatalf("denominator = %v, want 10000 from primary identity response", denom)
	}
	if committed < 240 {
		t.Fatalf("credits_committed = %d, want seeded credits-used 240", committed)
	}
}
