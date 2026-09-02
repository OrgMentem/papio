// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/triage"
	"papio/internal/watch"
)

// dismissScopeWorkKey is the single work key every fixture watch reports, so
// the snapshot groups them into one hit with several selectable watches.
const dismissScopeWorkKey = "10.1000/dismiss-scope"

// triageDismissScopeFixture is one grouped watch hit backed by three watches
// that all sighted the same work. The IDs are returned in creation order so a
// case can name "the second watch" without re-reading the snapshot.
type triageDismissScopeFixture struct {
	system   *bootstrap.System
	router   ipc.Router
	itemID   string
	watchIDs []int64
}

func newTriageDismissScopeFixture(t *testing.T) triageDismissScopeFixture {
	t.Helper()
	ctx := context.Background()
	system := testSystem(t)
	fixture := triageDismissScopeFixture{system: system, router: Router(system)}
	for i := range 3 {
		watched, err := system.Watches.Create(ctx, watch.CreateInput{
			Query: fmt.Sprintf("dismiss scope %d", i), Filters: watch.Filters{YearFrom: 2020},
			Collection: "Reading", CadenceHours: 24, PerRunCap: 5,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := system.Watches.RecordDigest(ctx, watched.ID, time.Now(), []watch.DigestEntry{{
			WorkKey: dismissScopeWorkKey, DOI: dismissScopeWorkKey, Title: "Dismiss scope",
		}}); err != nil {
			t.Fatal(err)
		}
		fixture.watchIDs = append(fixture.watchIDs, watched.ID)
	}
	var snapshot triage.Snapshot
	if rpcErr := callMethod(t, fixture.router, "triage.snapshot", map[string]any{"limit": 10}, &snapshot); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(snapshot.Items) != 1 || snapshot.Items[0].WatchHit == nil {
		t.Fatalf("fixture snapshot = %+v, want one grouped watch hit", snapshot.Items)
	}
	if got := len(snapshot.Items[0].WatchHit.Watches); got != 3 {
		t.Fatalf("grouped watches = %d, want 3", got)
	}
	fixture.itemID = snapshot.Items[0].ID
	return fixture
}

// dismiss posts one triage.decide dismissal with a verbatim watch_scope
// fragment. The params object is assembled by hand because callMethod would
// re-marshal - and so validate - a scope the test needs to send raw.
func (f triageDismissScopeFixture) dismiss(t *testing.T, scope string) *ipc.RPCError {
	t.Helper()
	params := fmt.Sprintf(`{"item_id":%q,"op":"dismiss"`, f.itemID)
	if scope != "" {
		params += `,"watch_scope":` + scope
	}
	params += "}"
	data, rpcErr := f.router.Handle(context.Background(), ipc.Request{
		Method: "triage.decide", Params: json.RawMessage(params),
	})
	if rpcErr != nil {
		return rpcErr
	}
	var outcome triageDecideResult
	if err := json.Unmarshal(data, &outcome); err != nil {
		t.Fatalf("decode decide result: %v (%s)", err, data)
	}
	if outcome.Outcome != "applied" {
		t.Fatalf("dismiss outcome = %q, want applied", outcome.Outcome)
	}
	return nil
}

// surviving reports which fixture watches still hold an unconsumed digest
// entry for the shared work. This is the discriminating end state: a dismissal
// that consumed the wrong watch, or a refusal that consumed anything at all,
// changes this set.
func (f triageDismissScopeFixture) surviving(t *testing.T) []int64 {
	t.Helper()
	var alive []int64
	for _, id := range f.watchIDs {
		entries, err := f.system.Watches.Digest(context.Background(), id, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.WorkKey == dismissScopeWorkKey {
				alive = append(alive, id)
				break
			}
		}
	}
	return alive
}

func equalIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A watch_scope list must consume exactly the named watches. Before this test
// the only exercised scope was "all", which cannot tell a correct selection
// from one that clears every sibling watch in the group.
func TestTriageDismissScopeConsumesOnlyTheListedWatches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pick    []int // indices into fixture.watchIDs
		survive []int
	}{
		{name: "one id", pick: []int{1}, survive: []int{0, 2}},
		{name: "several ids", pick: []int{0, 2}, survive: []int{1}},
		{name: "every id", pick: []int{0, 1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTriageDismissScopeFixture(t)
			ids := make([]string, 0, len(tc.pick))
			for _, index := range tc.pick {
				ids = append(ids, fmt.Sprintf("%d", fixture.watchIDs[index]))
			}
			if rpcErr := fixture.dismiss(t, "["+strings.Join(ids, ",")+"]"); rpcErr != nil {
				t.Fatalf("dismiss %v: %+v", ids, rpcErr)
			}
			want := make([]int64, 0, len(tc.survive))
			for _, index := range tc.survive {
				want = append(want, fixture.watchIDs[index])
			}
			if got := fixture.surviving(t); !equalIDs(got, want) {
				t.Fatalf("surviving digests = %v, want %v", got, want)
			}
		})
	}
}

// Every rejection arm must refuse with invalid_argument and consume nothing.
// One fixture serves all of them precisely because a refusal may not change
// state: the shared final assertion is that all three digests are still there.
func TestTriageDismissScopeRejectsInvalidScope(t *testing.T) {
	fixture := newTriageDismissScopeFixture(t)
	tooMany := make([]string, 0, 101)
	for i := range 101 {
		tooMany = append(tooMany, fmt.Sprintf("%d", fixture.watchIDs[0]+int64(i)))
	}
	unowned := fixture.watchIDs[2] + 1000
	for _, tc := range []struct {
		name  string
		scope string
	}{
		{name: "missing scope", scope: ""},
		{name: "null scope", scope: "null"},
		{name: "bare string that is not all", scope: `"mine"`},
		{name: "empty array", scope: "[]"},
		{name: "more than 100 ids", scope: "[" + strings.Join(tooMany, ",") + "]"},
		{name: "unowned watch id", scope: fmt.Sprintf("[%d]", unowned)},
		{name: "duplicate id", scope: fmt.Sprintf("[%d,%d]", fixture.watchIDs[0], fixture.watchIDs[0])},
		{name: "zero id", scope: "[0]"},
		{name: "negative id", scope: "[-1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpcErr := fixture.dismiss(t, tc.scope)
			if rpcErr == nil {
				t.Fatalf("scope %s was accepted, want invalid_argument", tc.scope)
			}
			if rpcErr.Code != "invalid_argument" {
				t.Fatalf("scope %s error = %+v, want invalid_argument", tc.scope, rpcErr)
			}
			if got := fixture.surviving(t); !equalIDs(got, fixture.watchIDs) {
				t.Fatalf("scope %s consumed digests: surviving = %v, want %v", tc.scope, got, fixture.watchIDs)
			}
		})
	}
	if got := fixture.surviving(t); !equalIDs(got, fixture.watchIDs) {
		t.Fatalf("after every refusal surviving = %v, want %v", got, fixture.watchIDs)
	}
}

// Trailing JSON after the ID list cannot arrive over triage.decide, because
// ipc.DecodeParams rejects a params object that is not exactly one JSON value.
// The guard is still the scope parser's own contract, and it is the arm the
// browser mirror lacked, so it is asserted directly on both halves.
func TestTriageDismissScopeRejectsTrailingGarbage(t *testing.T) {
	watches := []triage.Watch{{ID: 7, WorkKey: dismissScopeWorkKey}}
	for _, scope := range []string{`[7] oops`, `[7][7]`, `[7],`, `"all" oops`} {
		selected, err := triageDismissScope(json.RawMessage(scope), watches)
		if err == nil {
			t.Fatalf("scope %s was accepted as %v, want a refusal", scope, selected)
		}
		if selected != nil {
			t.Fatalf("scope %s refused but still selected %v", scope, selected)
		}
	}
	// Trailing whitespace is not garbage: the list is still exactly one value.
	selected, err := triageDismissScope(json.RawMessage("[7] \n"), watches)
	if err != nil || !selected[7] || len(selected) != 1 {
		t.Fatalf("whitespace-padded scope = %v, %v, want watch 7 selected", selected, err)
	}
}
