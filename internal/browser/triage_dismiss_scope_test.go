// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"papio/internal/protocol"
	"papio/internal/triage"
	"papio/internal/watch"
)

// dismissScopeWorkKey is the single work key every fixture watch reports, so
// the snapshot groups them into one hit with several selectable watches.
const dismissScopeWorkKey = "10.1000/bridge-dismiss-scope"

// bridgeDismissScopeFixture is one grouped watch hit backed by three watches
// that all sighted the same work, reachable over the extension's own
// triage_decide frame. The IDs are in creation order so a case can name "the
// second watch" without re-reading the snapshot.
type bridgeDismissScopeFixture struct {
	bridge   *Bridge
	itemID   string
	watchIDs []int64
}

func newBridgeDismissScopeFixture(t *testing.T) bridgeDismissScopeFixture {
	t.Helper()
	ctx := context.Background()
	b, _, _, _ := newBridge(t)
	fixture := bridgeDismissScopeFixture{bridge: b}
	for i := range 3 {
		watched, err := b.watchRunner.Store.Create(ctx, watch.CreateInput{
			Query: fmt.Sprintf("dismiss scope %d", i), Filters: watch.Filters{YearFrom: 2020},
			Collection: "Reading", CadenceHours: 24, PerRunCap: 5,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.watchRunner.Store.RecordDigest(ctx, watched.ID, b.now(), []watch.DigestEntry{{
			WorkKey: dismissScopeWorkKey, DOI: dismissScopeWorkKey, Title: "Dismiss scope",
		}}); err != nil {
			t.Fatal(err)
		}
		fixture.watchIDs = append(fixture.watchIDs, watched.ID)
	}
	snapshot, err := b.triage.Snapshot(ctx, triage.SnapshotRequest{Limit: 10})
	if err != nil || len(snapshot.Items) != 1 || snapshot.Items[0].WatchHit == nil {
		t.Fatalf("fixture snapshot = %+v, %v, want one grouped watch hit", snapshot.Items, err)
	}
	if got := len(snapshot.Items[0].WatchHit.Watches); got != 3 {
		t.Fatalf("grouped watches = %d, want 3", got)
	}
	fixture.itemID = snapshot.Items[0].ID
	runSync(t, b, hello())
	return fixture
}

// decide delivers one triage_decide dismissal carrying a verbatim watch_scope
// fragment. Sync is called directly rather than through runSync so a frame the
// protocol layer refuses returns its error instead of failing the test: a
// framing violation is the one condition allowed to be transport-fatal.
func (f bridgeDismissScopeFixture) decide(t *testing.T, requestID, scope string) (*protocol.TriageDecideResultPayload, error) {
	t.Helper()
	payload := map[string]any{"request_id": requestID, "item_id": f.itemID, "op": "dismiss"}
	if scope != "" {
		payload["watch_scope"] = json.RawMessage(scope)
	}
	frames := []json.RawMessage{inFrame(t, protocol.MsgTriageDecide, "", payload)}
	out, err := f.bridge.Sync(context.Background(), testSessionID, false, frames)
	if err != nil {
		return nil, err
	}
	for _, raw := range out {
		msg, decodeErr := protocol.DecodeBrowserMessage(raw)
		if decodeErr != nil {
			t.Fatalf("outbound frame failed protocol decode: %v", decodeErr)
		}
		if msg.Type == protocol.MsgTriageDecideResult {
			return msg.Payload.(*protocol.TriageDecideResultPayload), nil
		}
	}
	t.Fatalf("no triage_decide_result among %d outbound frames", len(out))
	return nil, nil
}

// surviving reports which fixture watches still hold an unconsumed digest
// entry for the shared work. This is the discriminating end state: a dismissal
// that consumed the wrong watch, or a refusal that consumed anything at all,
// changes this set.
func (f bridgeDismissScopeFixture) surviving(t *testing.T) []int64 {
	t.Helper()
	var alive []int64
	for _, id := range f.watchIDs {
		entries, err := f.bridge.watchRunner.Store.Digest(context.Background(), id, 100)
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

func equalWatchIDs(got, want []int64) bool {
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
// the extension path only ever exercised "all", which cannot tell a correct
// selection from one that clears every sibling watch in the group.
func TestTriageDismissScopeBridgeConsumesOnlyTheListedWatches(t *testing.T) {
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
			fixture := newBridgeDismissScopeFixture(t)
			ids := make([]string, 0, len(tc.pick))
			for _, index := range tc.pick {
				ids = append(ids, fmt.Sprintf("%d", fixture.watchIDs[index]))
			}
			result, err := fixture.decide(t, "request-scope-list-1", "["+strings.Join(ids, ",")+"]")
			if err != nil {
				t.Fatalf("sync with scope %v: %v", ids, err)
			}
			if result.Outcome != "applied" {
				t.Fatalf("scope %v outcome = %+v, want applied", ids, result)
			}
			want := make([]int64, 0, len(tc.survive))
			for _, index := range tc.survive {
				want = append(want, fixture.watchIDs[index])
			}
			if got := fixture.surviving(t); !equalWatchIDs(got, want) {
				t.Fatalf("surviving digests = %v, want %v", got, want)
			}
		})
	}
}

// Ownership is the one refusal the protocol layer cannot make for itself: a
// well-formed list of positive, unique IDs that this hit does not own reaches
// the bridge. It must come back as a structured outcome - a raw error here
// would disconnect the user's browser - and it must consume nothing.
func TestTriageDismissScopeBridgeRefusesAWatchTheHitDoesNotOwn(t *testing.T) {
	fixture := newBridgeDismissScopeFixture(t)
	foreign, err := fixture.bridge.watchRunner.Store.Create(context.Background(), watch.CreateInput{
		Query: "unrelated watch", Filters: watch.Filters{YearFrom: 2020},
		Collection: "Reading", CadenceHours: 24, PerRunCap: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, syncErr := fixture.decide(t, "request-scope-foreign-1",
		fmt.Sprintf("[%d,%d]", fixture.watchIDs[0], foreign.ID))
	if syncErr != nil {
		t.Fatalf("a routine refusal must not fail the sync: %v", syncErr)
	}
	if result.Outcome != "error" || result.Detail == "" {
		t.Fatalf("foreign watch outcome = %+v, want a structured error with a detail", result)
	}
	if got := fixture.surviving(t); !equalWatchIDs(got, fixture.watchIDs) {
		t.Fatalf("refused scope consumed digests: surviving = %v, want %v", got, fixture.watchIDs)
	}
}

// The malformed scopes never reach the handler: protocol.DecodeBrowserMessage
// refuses the frame, which is a framing violation and so legitimately fatal to
// the call. What matters for the user's data is the same either way - nothing
// is consumed - so one fixture serves every case and the shared final
// assertion is that all three digests are still there.
func TestTriageDismissScopeBridgeRefusesInvalidScopeAtTheFrameBoundary(t *testing.T) {
	fixture := newBridgeDismissScopeFixture(t)
	tooMany := make([]string, 0, 101)
	for i := range 101 {
		tooMany = append(tooMany, fmt.Sprintf("%d", fixture.watchIDs[0]+int64(i)))
	}
	for _, tc := range []struct {
		name  string
		scope string
	}{
		{name: "missing scope", scope: ""},
		{name: "null scope", scope: "null"},
		{name: "bare string that is not all", scope: `"mine"`},
		{name: "empty array", scope: "[]"},
		{name: "more than 100 ids", scope: "[" + strings.Join(tooMany, ",") + "]"},
		{name: "duplicate id", scope: fmt.Sprintf("[%d,%d]", fixture.watchIDs[0], fixture.watchIDs[0])},
		{name: "zero id", scope: "[0]"},
		{name: "negative id", scope: "[-1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := fixture.decide(t, "request-scope-invalid-1", tc.scope)
			if err == nil {
				t.Fatalf("scope %s was accepted as %+v, want a refusal", tc.scope, result)
			}
			if !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("scope %s error = %v, want ErrInvalidFrame", tc.scope, err)
			}
			if got := fixture.surviving(t); !equalWatchIDs(got, fixture.watchIDs) {
				t.Fatalf("scope %s consumed digests: surviving = %v, want %v", tc.scope, got, fixture.watchIDs)
			}
		})
	}
	if got := fixture.surviving(t); !equalWatchIDs(got, fixture.watchIDs) {
		t.Fatalf("after every refusal surviving = %v, want %v", got, fixture.watchIDs)
	}
}

// triageDismissScope is mirrored in internal/api/triage.go, and this half used
// to be the weaker copy: it accepted an empty list, more than 100 IDs, a
// non-positive ID, and an absent scope. An empty list was the silent-loss
// shape - it answered "applied" while consuming nothing - so the browser's
// only defence was the protocol validator one layer up. AGENTS.md records that
// a one-sided fix to a mirrored pair is the standing failure mode here, so
// both halves are now pinned to the same decision table.
func TestTriageDismissScopeMirrorsTheApiHalf(t *testing.T) {
	watches := []triage.Watch{
		{ID: 4, WorkKey: dismissScopeWorkKey},
		{ID: 9, WorkKey: dismissScopeWorkKey},
	}
	for _, scope := range []string{
		"",           // absent
		"null",       // present but empty
		`"mine"`,     // bare string that is not all
		"[]",         // empty list: was accepted, and consumed nothing under "applied"
		"[4,4]",      // duplicate
		"[0]",        // zero
		"[-1]",       // negative
		"[4,11]",     // an ID this hit does not own
		"[4] oops",   // trailing garbage
		"[4][9]",     // a second JSON value
		`"all" oops`, // trailing garbage after the sentinel
		"[" + strings.TrimSuffix(strings.Repeat("4,", 101), ",") + "]", // over the 100 cap
	} {
		selected, err := triageDismissScope(json.RawMessage(scope), watches)
		if err == nil {
			t.Fatalf("scope %s was accepted as %v, want a refusal", scope, selected)
		}
		if selected != nil {
			t.Fatalf("scope %s refused but still selected %v", scope, selected)
		}
	}
	all, err := triageDismissScope(json.RawMessage(`"all"`), watches)
	if err != nil || len(all) != 2 || !all[4] || !all[9] {
		t.Fatalf(`scope "all" = %v, %v, want both watches selected`, all, err)
	}
	subset, err := triageDismissScope(json.RawMessage("[9]"), watches)
	if err != nil || len(subset) != 1 || !subset[9] {
		t.Fatalf("scope [9] = %v, %v, want only watch 9 selected", subset, err)
	}
}
