// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func gateObservation(id string, typ HumanGateType, scopeClass HumanGateScopeClass, scopeKey string, rev int64) HumanGateObservation {
	return HumanGateObservation{
		ID: id, GateType: typ, ScopeClass: string(scopeClass), ScopeKey: scopeKey,
		ObservationRevision: rev, Status: HumanGateOpen, DetailJSON: `{}`,
	}
}

func TestHumanGateOneClaimManySiblingsAggregatesOneSurface(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	first := gateObservation("login-1", HumanGateLogin, HumanGateScopeAuthenticationClaim, "claim-a", 1)
	first.AuthenticationClaimID = "claim-a"
	first.DependentJobIDs = []string{"job-b", "job-a", "job-a"}
	first.ClaimMemberJobIDs = []string{"job-b"}
	if err := js.UpsertHumanGateObservation(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := gateObservation("login-2", HumanGateLogin, HumanGateScopeAuthenticationClaim, "claim-a", 2)
	second.AuthenticationClaimID = "claim-a"
	second.DependentJobIDs = []string{"job-c", "job-b"}
	second.ClaimMemberJobIDs = []string{"job-c", "job-a"}
	if err := js.UpsertHumanGateObservation(ctx, second); err != nil {
		t.Fatal(err)
	}
	rows, err := js.CurrentHumanGateObservations(ctx, string(HumanGateScopeAuthenticationClaim), "claim-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("gate rows = %+v, want one surface", rows)
	}
	if got, want := rows[0].DependentJobIDs, []string{"job-a", "job-b", "job-c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dependent jobs = %v, want %v", got, want)
	}
	if got, want := rows[0].ClaimMemberJobIDs, []string{"job-a", "job-b", "job-c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claim members = %v, want %v", got, want)
	}
	attention, err := js.CurrentHumanAttention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attention.Count != 1 || len(attention.Gates) != 1 {
		t.Fatalf("attention = %+v, want one live surface", attention)
	}
}

func TestHumanGateSameProfileDifferentTypesRemainIndependent(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	login := gateObservation("login-profile", HumanGateLogin, HumanGateScopeInstitutionProfile, "profile-a", 1)
	terms := gateObservation("terms-profile", HumanGateTermsRequired, HumanGateScopeInstitutionProfile, "profile-a", 1)
	if err := js.UpsertHumanGateObservation(ctx, login); err != nil {
		t.Fatal(err)
	}
	if err := js.UpsertHumanGateObservation(ctx, terms); err != nil {
		t.Fatal(err)
	}
	attention, err := js.CurrentHumanAttention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attention.Count != 2 {
		t.Fatalf("attention count = %d, want two typed gates", attention.Count)
	}
	if err := js.ResolveHumanGate(ctx, HumanGateLogin, string(HumanGateScopeInstitutionProfile), "profile-a", 1); err != nil {
		t.Fatal(err)
	}
	rows, err := js.CurrentHumanGateObservations(ctx, string(HumanGateScopeInstitutionProfile), "profile-a")
	if err != nil {
		t.Fatal(err)
	}
	var resolved, open int
	for _, row := range rows {
		switch row.Status {
		case HumanGateResolved:
			resolved++
		case HumanGateOpen:
			open++
		}
	}
	if resolved != 1 || open != 1 {
		t.Fatalf("same-profile gate statuses = %+v, want one resolved and one open", rows)
	}
	count, err := js.HumanGateAttentionCount(ctx)
	if err != nil || count != 1 {
		t.Fatalf("attention count after matching close = %d, %v; want one", count, err)
	}
}

func TestHumanGatePlatformPermissionScopesAndMatchingClose(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	host := gateObservation("host-permission", HumanGateBrowserHostPermission, HumanGateScopeBrowserHost, "host-a", 4)
	downloads := gateObservation("downloads-permission", HumanGateDownloadsFolderPermission, HumanGateScopePlatform, "downloads-folder", 2)
	if err := js.UpsertHumanGateObservation(ctx, host); err != nil {
		t.Fatal(err)
	}
	if err := js.UpsertHumanGateObservation(ctx, downloads); err != nil {
		t.Fatal(err)
	}
	if err := js.ResolveHumanGate(ctx, HumanGateBrowserHostPermission, string(HumanGateScopeBrowserHost), "host-a", 4); err != nil {
		t.Fatal(err)
	}
	if err := js.ResolveHumanGate(ctx, HumanGateBrowserHostPermission, string(HumanGateScopeBrowserHost), "other-host", 4); !errors.Is(err, ErrConflict) {
		t.Fatalf("unrelated host close = %v, want conflict", err)
	}
	rows, err := js.CurrentHumanGateObservations(ctx, string(HumanGateScopePlatform), "downloads-folder")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != HumanGateOpen {
		t.Fatalf("downloads permission = %+v, want open and untouched", rows)
	}
	count, err := js.CountHumanGateAttention(ctx)
	if err != nil || count != 1 {
		t.Fatalf("platform attention count = %d, %v; want one", count, err)
	}
}

func TestHumanGateStaleSuccessCannotCloseCurrentGate(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	gate := gateObservation("captcha-1", HumanGateCaptchaOrSecurity, HumanGateScopeInstitutionProfile, "profile-captcha", 2)
	if err := js.UpsertHumanGateObservation(ctx, gate); err != nil {
		t.Fatal(err)
	}
	if err := js.ResolveHumanGate(ctx, HumanGateCaptchaOrSecurity, string(HumanGateScopeInstitutionProfile), "profile-captcha", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale close = %v, want conflict", err)
	}
	rows, err := js.CurrentHumanGateObservations(ctx, string(HumanGateScopeInstitutionProfile), "profile-captcha")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != HumanGateOpen || rows[0].ObservationRevision != 2 {
		t.Fatalf("stale close changed gate = %+v", rows)
	}
}

func TestHumanGatePlatformPermissionTypeRequiresPlatformScope(t *testing.T) {
	js := testStore(t)
	err := js.UpsertHumanGateObservation(context.Background(), gateObservation(
		"downloads-wrong-scope", HumanGateDownloadsFolderPermission,
		HumanGateScopeInstitutionProfile, "profile-a", 1))
	if err == nil {
		t.Fatal("downloads-folder gate accepted non-platform scope")
	}
	err = js.UpsertHumanGateObservation(context.Background(), gateObservation(
		"host-wrong-scope", HumanGateBrowserHostPermission,
		HumanGateScopePlatform, "platform-a", 1))
	if err == nil {
		t.Fatal("browser-host gate accepted non-browser scope")
	}
}
func TestHumanGateClosedVocabularyProjectsAllTypes(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	cases := []struct {
		typ   HumanGateType
		class HumanGateScopeClass
		key   string
	}{
		{HumanGateLogin, HumanGateScopeAuthenticationClaim, "claim-all"},
		{HumanGateMFA, HumanGateScopeAuthenticationClaim, "claim-all"},
		{HumanGateCaptchaOrSecurity, HumanGateScopeInstitutionProfile, "profile-all"},
		{HumanGateBrowserHostPermission, HumanGateScopeBrowserHost, "host-all"},
		{HumanGateDownloadsFolderPermission, HumanGateScopePlatform, "downloads-all"},
		{HumanGateTermsRequired, HumanGateScopeInstitutionProfile, "profile-all"},
		{HumanGateContractualDeclaration, HumanGateScopeInstitutionProfile, "profile-all"},
		{HumanGateIdentityAmbiguous, HumanGateScopeBinding, "binding-all"},
	}
	for i, tc := range cases {
		if err := js.UpsertHumanGateObservation(ctx, gateObservation(
			"typed-gate-"+string(rune('a'+i)), tc.typ, tc.class, tc.key, 1)); err != nil {
			t.Fatalf("%s: %v", tc.typ, err)
		}
	}
	attention, err := js.CurrentHumanAttention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attention.Count != len(cases) {
		t.Fatalf("typed attention count = %d, want %d", attention.Count, len(cases))
	}
}
func TestHumanGateIDFencedClosePreservesReplacement(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	gate := gateObservation("login-current", HumanGateLogin, HumanGateScopeAuthenticationClaim, "claim-id", 3)
	if err := js.UpsertHumanGateObservation(ctx, gate); err != nil {
		t.Fatal(err)
	}
	stale := gate
	stale.ID = "login-stale"
	stale.Status = HumanGateResolved
	if err := js.ResolveHumanGateObservation(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong observation close = %v, want conflict", err)
	}
	rows, err := js.CurrentHumanGateObservations(ctx, string(HumanGateScopeAuthenticationClaim), "claim-id")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != HumanGateOpen || rows[0].ID != "login-current" {
		t.Fatalf("wrong observation changed current gate = %+v", rows)
	}
}

func TestHumanGateExactReplayIsIdempotentAndNewObservationReopens(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	open := gateObservation("login-frame-1", HumanGateLogin, HumanGateScopeAuthenticationClaim, "claim-replay", 1)
	if err := js.UpsertHumanGateObservation(ctx, open); err != nil {
		t.Fatal(err)
	}
	replay := open
	replay.ObservationRevision = 99
	if err := js.UpsertHumanGateObservation(ctx, replay); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay = %v, want conflict", err)
	}
	if err := js.ResolveHumanGate(ctx, HumanGateLogin, string(HumanGateScopeAuthenticationClaim), "claim-replay", 1); err != nil {
		t.Fatal(err)
	}
	lateOpen := gateObservation("login-frame-2", HumanGateLogin, HumanGateScopeAuthenticationClaim, "claim-replay", 3)
	if err := js.UpsertHumanGateObservation(ctx, lateOpen); err != nil {
		t.Fatal(err)
	}
	rows, err := js.CurrentHumanGateObservations(ctx, string(HumanGateScopeAuthenticationClaim), "claim-replay")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != HumanGateOpen ||
		rows[0].ObservationRevision != 3 || rows[0].ID != "login-frame-2" {
		t.Fatalf("replay advanced or new observation failed to reopen gate: %+v", rows)
	}
}

func TestHumanGateRejectsEscapedURLsAndCredentialDetails(t *testing.T) {
	js := testStore(t)
	for _, detail := range []string{`{"next":"https:\/\/example.invalid"}`, `{"password":"secret"}`} {
		observation := gateObservation("private-"+string(rune(len(detail))), HumanGateLogin,
			HumanGateScopeAuthenticationClaim, "claim-private", 1)
		observation.DetailJSON = detail
		if err := js.UpsertHumanGateObservation(context.Background(), observation); err == nil {
			t.Fatalf("accepted private gate detail %s", detail)
		}
	}
}
