// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package agentjson

import (
	"encoding/json"
	"testing"
)

// The contract is two keys and no more: a consumer that reads payload["jobs"]
// and payload["truncated"] must never meet a third key or a bare array.
func TestEnvelopeEmitsExactlyTheContractKeys(t *testing.T) {
	data, err := json.Marshal(Envelope("jobs", []string{"a", "b"}, true))
	if err != nil {
		t.Fatal(err)
	}
	var page map[string]json.RawMessage
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatalf("envelope is not a JSON object: %v (%s)", err, data)
	}
	if len(page) != 2 {
		t.Fatalf("envelope has %d keys, want exactly jobs and truncated: %s", len(page), data)
	}
	if _, ok := page["jobs"]; !ok {
		t.Fatalf("envelope missing the row key: %s", data)
	}
	var truncated bool
	if err := json.Unmarshal(page["truncated"], &truncated); err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
}

// A nil slice reaching an agent as `null` breaks the obvious iteration. This is
// the single most consequential invariant in the package.
func TestEmptyResultsMarshalAsArrayNotNull(t *testing.T) {
	var missing []string
	data, err := json.Marshal(Envelope("works", missing, false))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"works":[],"truncated":false}`; got != want {
		t.Fatalf("nil rows marshalled as %s, want %s", got, want)
	}

	rows, truncated := Truncate(missing, 10)
	if rows == nil {
		t.Fatal("Truncate returned a nil slice; it must normalize to empty")
	}
	if len(rows) != 0 || truncated {
		t.Fatalf("Truncate(nil) = %v, %t; want empty, false", rows, truncated)
	}
}

func TestTruncateReportsDroppedRows(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rows          []int
		cap           int
		wantLen       int
		wantTruncated bool
	}{
		{name: "under the cap", rows: []int{1, 2}, cap: 3, wantLen: 2},
		{name: "exactly the cap", rows: []int{1, 2, 3}, cap: 3, wantLen: 3},
		{name: "over the cap", rows: []int{1, 2, 3, 4}, cap: 3, wantLen: 3, wantTruncated: true},
		{name: "cap disabled", rows: []int{1, 2, 3, 4}, cap: 0, wantLen: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, truncated := Truncate(tc.rows, tc.cap)
			if len(rows) != tc.wantLen || truncated != tc.wantTruncated {
				t.Fatalf("Truncate(%v, %d) = %v, %t; want len %d, %t",
					tc.rows, tc.cap, rows, truncated, tc.wantLen, tc.wantTruncated)
			}
		})
	}
}

// Capped answers the question a --limit flag actually poses: the daemon was
// asked for at most limit rows, so a full page means more may exist upstream.
func TestCappedFlagsAFullPageAsPossiblyIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rows          []int
		limit         int
		wantTruncated bool
	}{
		{name: "short page is complete", rows: []int{1, 2}, limit: 5},
		{name: "full page may hide more", rows: []int{1, 2, 3}, limit: 3, wantTruncated: true},
		{name: "no limit never truncates", rows: []int{1, 2, 3}, limit: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, truncated := Capped(tc.rows, tc.limit)
			if truncated != tc.wantTruncated {
				t.Fatalf("Capped(%v, %d) truncated = %t, want %t", tc.rows, tc.limit, truncated, tc.wantTruncated)
			}
			if len(rows) != len(tc.rows) {
				t.Fatalf("Capped must not drop rows: got %d, want %d", len(rows), len(tc.rows))
			}
		})
	}
}
