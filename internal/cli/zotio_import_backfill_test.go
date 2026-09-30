// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/zotio"
)

// An --apply run that recorded failed imports used to exit 0, so automation
// read a partly filed batch as complete. The receipt (and its resume cursor)
// must still print; a dry run's expected_fail is only a prediction.
func TestZotioImportBackfillExitStatusFollowsAppliedFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  zotio.ImportBackfillResult
		args    []string
		wantErr bool
	}{
		{
			name:    "apply with failures",
			result:  zotio.ImportBackfillResult{Summary: zotio.ImportBackfillSummary{Selected: 3, NewlyFiled: 2, Failed: 1}, Cursor: "c1"},
			args:    []string{"--apply"},
			wantErr: true,
		},
		{
			name:   "apply without failures",
			result: zotio.ImportBackfillResult{Summary: zotio.ImportBackfillSummary{Selected: 2, NewlyFiled: 2}, Cursor: "c1"},
			args:   []string{"--apply"},
		},
		{
			name:   "dry run with expected failures",
			result: zotio.ImportBackfillResult{DryRun: true, Summary: zotio.ImportBackfillSummary{Selected: 2, WouldImport: 1, ExpectedFail: 1}, Cursor: "c1"},
		},
	} {
		for _, jsonOutput := range []bool{false, true} {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
				if method != "zotio.import_backfill" {
					t.Fatalf("method = %q", method)
				}
				*result.(*zotio.ImportBackfillResult) = tc.result
				return nil
			})
			args := append([]string{"zotio", "import-backfill"}, tc.args...)
			if jsonOutput {
				args = append([]string{"--json"}, args...)
			}
			root.SetArgs(args)
			err := root.ExecuteContext(context.Background())
			if tc.wantErr != errors.Is(err, errImportBackfillFailed) || (!tc.wantErr && err != nil) {
				t.Fatalf("%s json=%t: err = %v, want failure %t", tc.name, jsonOutput, err, tc.wantErr)
			}
			if jsonOutput {
				var got zotio.ImportBackfillResult
				if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Cursor != "c1" {
					t.Fatalf("%s: JSON receipt = %s (%v), want it printed with the cursor", tc.name, out.String(), err)
				}
			} else if !strings.Contains(out.String(), "cursor: c1") {
				t.Fatalf("%s: text receipt = %q, want it printed with the cursor", tc.name, out.String())
			}
		}
	}
}
