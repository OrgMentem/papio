// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package budget

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
)

func TestClientCredentialIdentityDoesNotInheritAnonymousDeferral(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	anon := config.Source{Enabled: true}
	pair := config.Source{Enabled: true, ClientID: "private-client", ClientSecret: "private-secret"}
	if err := m.Defer(ctx, config.SourceOpenAIRE, anon, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(ctx, config.SourceOpenAIRE, pair, 0); err != nil {
		t.Fatalf("authenticated pair inherited anonymous deferral: %v", err)
	}
	if err := m.Defer(ctx, config.SourceOpenAIRE, pair, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	shared := pair
	shared.CredentialRef = "keyring:0123456789abcdef0123456789abcdef"
	shared.ClientID, shared.ClientSecret = " private-client ", " private-secret "
	var deferred *ErrDeferred
	if err := m.Acquire(ctx, config.SourceOpenAIRE, shared, 0); !errors.As(err, &deferred) {
		t.Fatal("same resolved pair escaped its durable gate after rebinding")
	}
	other := pair
	other.ClientSecret = "other-secret"
	if err := m.Acquire(ctx, config.SourceOpenAIRE, other, 0); err != nil {
		t.Fatal("different pair inherited another account's gate")
	}
	for _, policy := range []config.Source{pair, shared, other} {
		identity := IdentityFor(policy)
		if identity == "anonymous" || strings.Contains(identity, "private") || strings.Contains(identity, "secret") || strings.Contains(identity, "keyring") {
			t.Fatal("credential identity is not a secret-free account fingerprint")
		}
	}
	// OpenAIRE's client pair takes precedence over its legacy token fallback.
	withToken := pair
	withToken.APIKey = "ignored-token"
	if IdentityFor(withToken) != IdentityFor(pair) {
		t.Fatal("unused token changed the actual client-pair identity")
	}
	// Field boundaries must remain unambiguous.
	if IdentityFor(config.Source{ClientID: "ab", ClientSecret: "c"}) == IdentityFor(config.Source{ClientID: "a", ClientSecret: "bc"}) {
		t.Fatal("ambiguous pair encoding")
	}
}
