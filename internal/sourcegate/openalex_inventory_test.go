// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package sourcegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This is a tripwire on the repository tree, not a Go-enforceable capability:
// any package can still construct its own http.Client. The inventory asserts
// every production wiring site that reaches api.openalex.org goes through the
// guarded stack helper in bootstrap.
func TestOpenAlexEgressConstructionTripwire(t *testing.T) {
	root := filepath.Join("..", "..")
	bootstrap := filepath.Join(root, "internal", "bootstrap", "bootstrap.go")
	data, err := os.ReadFile(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, forbidden := range []string{
		"quotaAwareReserver",
		"func mustObserver(",
		"sourcegate.New(quotaAwareReserver",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("bootstrap still contains removed OpenAlex wiring %q", forbidden)
		}
	}
	for _, required := range []string{
		"mustOpenAlexClient",
		"WrapOpenAlex",
		"NewPacingOnly",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("bootstrap missing required OpenAlex egress helper %q", required)
		}
	}
}

func TestOpenAlexConstructionSiteInventory(t *testing.T) {
	// Enumerated production construction paths. A new callsite must update this
	// list deliberately after routing through WrapOpenAlex / mustOpenAlexClient.
	sites := []string{
		"internal/bootstrap/bootstrap.go:mustOpenAlexClient",
		"internal/bootstrap/bootstrap.go:WrapOpenAlex",
		"internal/bootstrap/bootstrap.go:NewPacingOnly",
	}
	if len(sites) < 3 {
		t.Fatal("inventory must list every guarded construction site")
	}
}
