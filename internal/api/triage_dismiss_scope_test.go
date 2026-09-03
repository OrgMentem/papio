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
	// wantMessage separates the two refusal reasons the production guard can
	// give. Asserting only the code cannot discriminate the 100-ID cap: with
	// `len(ids) > 100` removed, a 101-ID list built from a 3-watch fixture
	// still fails on the FIRST unowned ID and still reports invalid_argument.
	// Four distinct refusal reasons, each pinned to the arm that produces it.
	const requiredMsg = "watch_scope is required for dismiss"
	const bareMsg = "watch_scope must be all or watch IDs"
	const shapeMsg = "watch_scope must be all or 1 to 100 watch IDs"
	const idMsg = "watch_scope contains an invalid watch ID"
	for _, tc := range []struct {
		name        string
		scope       string
		wantMessage string
	}{
		{name: "missing scope", scope: "", wantMessage: requiredMsg},
		// JSON null unmarshals into a string without error, leaving it empty,
		// so this takes the bare-string arm rather than the array arm.
		{name: "null scope", scope: "null", wantMessage: bareMsg},
		{name: "bare string that is not all", scope: `"mine"`, wantMessage: bareMsg},
		{name: "empty array", scope: "[]", wantMessage: shapeMsg},
		{name: "more than 100 ids", scope: "[" + strings.Join(tooMany, ",") + "]", wantMessage: shapeMsg},
		{name: "unowned watch id", scope: fmt.Sprintf("[%d]", unowned), wantMessage: idMsg},
		{name: "duplicate id", scope: fmt.Sprintf("[%d,%d]", fixture.watchIDs[0], fixture.watchIDs[0]), wantMessage: idMsg},
		{name: "zero id", scope: "[0]", wantMessage: idMsg},
		{name: "negative id", scope: "[-1]", wantMessage: idMsg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpcErr := fixture.dismiss(t, tc.scope)
			if rpcErr == nil {
				t.Fatalf("scope %s was accepted, want invalid_argument", tc.scope)
			}
			if rpcErr.Code != "invalid_argument" {
				t.Fatalf("scope %s error = %+v, want invalid_argument", tc.scope, rpcErr)
			}
			if !strings.Contains(rpcErr.Message, tc.wantMessage) {
				t.Fatalf("scope %s message = %q, want %q: the two refusal reasons must stay distinguishable", tc.scope, rpcErr.Message, tc.wantMessage)
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
// The IPC decoder still rejects it before the normalized scope reaches the
// domain mutation.
func TestTriageDismissScopeRejectsTrailingGarbage(t *testing.T) {
	for _, scope := range []string{`[7] oops`, `[7][7]`, `[7],`, `"all" oops`} {
		decoded, err := decodeTriageDismissScope(json.RawMessage(scope))
		if err == nil {
			t.Fatalf("scope %s was accepted as %+v, want a refusal", scope, decoded)
		}
	}
	// Trailing whitespace is not garbage: the list is still exactly one value.
	decoded, err := decodeTriageDismissScope(json.RawMessage("[7] \n"))
	if err != nil || len(decoded.WatchIDs) != 1 || decoded.WatchIDs[0] != 7 {
		t.Fatalf("whitespace-padded scope = %+v, %v, want watch 7 selected", decoded, err)
	}
}
