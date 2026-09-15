// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/config"
)

func TestDoctorRetractionCheckStatuses(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		disabled   bool
		status     *retractionCacheStatus
		raw        string
		wantStatus string
	}{
		{name: "disabled", disabled: true, wantStatus: Skip},
		{
			name: "recent success",
			status: &retractionCacheStatus{
				Version: retractionCacheVersion, CheckedAt: now.Add(-time.Hour),
				Notices: map[string]json.RawMessage{"notice": json.RawMessage(`{}`)},
			},
			wantStatus: Pass,
		},
		{
			name: "latest fetch failed",
			status: &retractionCacheStatus{
				Version: retractionCacheVersion, CheckedAt: now.Add(-time.Hour),
				LastFetchError: "Retraction Watch returned HTTP 503", LastFetchErrorAt: now.Add(-time.Minute),
			},
			wantStatus: Warn,
		},
		{
			name: "stale",
			status: &retractionCacheStatus{
				Version: retractionCacheVersion, CheckedAt: now.Add(-49 * time.Hour),
			},
			wantStatus: Warn,
		},
		{name: "corrupt cache", raw: "{", wantStatus: Warn},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.DataDir = t.TempDir()
			policy := cfg.Sources[config.SourceRetractionWatch]
			policy.Enabled = !test.disabled
			cfg.Sources[config.SourceRetractionWatch] = policy
			if test.status != nil || test.raw != "" {
				data := []byte(test.raw)
				if test.status != nil {
					var err error
					data, err = json.Marshal(test.status)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(cfg.DataDir, retractionCacheFileName), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var checks []Check
			checkRetraction(cfg, func(name, status, detail, remediation string) {
				checks = append(checks, Check{Name: name, Status: status, Detail: detail, Remediation: remediation})
			})
			if len(checks) != 1 || checks[0].Name != "retraction" || checks[0].Status != test.wantStatus {
				t.Fatalf("check = %#v, want retraction %s", checks, test.wantStatus)
			}
		})
	}
}
