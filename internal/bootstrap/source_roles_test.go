// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bootstrap

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"papio/internal/config"
	"papio/internal/store/storetest"
)

// allSourcesEnabled returns a config with every catalog source enabled, so
// every role's production wiring is actually constructed. A role whose
// constructor is skipped on a default config would otherwise read as "no
// wiring" and make these comparisons vacuous.
func allSourcesEnabled(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = storetest.DataDir(t)
	cfg.Email = "roles@example.org"
	cfg.PDF.OCREnabled = false
	cfg.Zotio.AutoEnrich = false
	sources := make(map[string]config.Source, len(cfg.Sources))
	for name, source := range cfg.Sources {
		source.Enabled = true
		sources[name] = source
	}
	cfg.Sources = sources
	return cfg
}

func newSystemForRoles(t *testing.T, cfg config.Config) *System {
	t.Helper()
	system, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := system.Close(); err != nil {
			t.Errorf("close system: %v", err)
		}
	})
	return system
}

// The catalog's acquisition role and the resolver chain must agree exactly, in
// order: the catalog row is what a source declares, resolverEntries is what
// papio builds, and nothing in the compiler ties the two together. Both
// directions fail here — a declared acquisition source with no constructor,
// and a constructor for a source that does not declare the role — which is what
// makes "one catalog row plus one constructor" the whole edit surface.
//
// The order is part of the contract, not incidental: it is resolver precedence,
// and internal/bench reproduces it from the same catalog.
func TestResolverChainMatchesCatalogAcquisitionRole(t *testing.T) {
	cfg := allSourcesEnabled(t)
	system := newSystemForRoles(t, cfg)
	got := make([]string, 0, len(system.App.Resolvers))
	for _, entry := range system.App.Resolvers {
		if entry.Adapter == nil {
			t.Fatal("nil resolver adapter")
		}
		got = append(got, entry.Adapter.Name())
	}
	want := config.SourcesInRole(config.RoleAcquisitionResolver)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolver chain = %v, want the catalog's acquisition role in catalog order %v", got, want)
	}
}

// A source that only answers bibliographic or integrity questions must never be
// admitted as an acquisition candidate producer: crossref_metadata returns
// metadata and retraction_watch returns an integrity feed, so either one in the
// chain would spend a resolver pass on a source that can never return a PDF.
// TestResolverChainMatchesCatalogAcquisitionRole pins the chain to this role
// set, so proving they are outside it proves they are outside the chain.
//
// A provider may legitimately hold several roles — OpenAlex resolves, discovers
// and enriches against one keyed allowance — so the rule is about these two
// single-role feeds, not about non-acquisition roles in general.
func TestMetadataAndRetractionFeedsStayOutOfTheResolverChain(t *testing.T) {
	acquisition := config.SourcesInRole(config.RoleAcquisitionResolver)
	for _, name := range []string{config.SourceCrossrefMetadata, config.SourceRetractionWatch} {
		for _, resolver := range acquisition {
			if name == resolver {
				t.Errorf("%s declares the acquisition role; it would enter the resolver chain and can never return a PDF", name)
			}
		}
	}
	if len(config.SourcesInRole(config.RoleMetadataEnricher)) == 0 {
		t.Error("no source declares the enricher role; crossref_metadata's roles were lost, not merely reordered")
	}
	if len(config.SourcesInRole(config.RoleRetractionSource)) == 0 {
		t.Error("no source declares the retraction role; retraction_watch's roles were lost, not merely reordered")
	}
}

// The enrichment wiring builds one enricher per provider it knows how to build,
// and the catalog declares which providers those are. crossref_metadata is the
// Crossref seam (also the typed-version-relations gate) and OpenAlex enriches
// against the same keyed daily allowance its resolver and discovery paths use.
func TestMetadataEnrichersMatchCatalogEnricherRole(t *testing.T) {
	cfg := allSourcesEnabled(t)
	system := newSystemForRoles(t, cfg)
	got := make([]string, 0, len(system.App.MetadataEnrichers))
	for _, entry := range system.App.MetadataEnrichers {
		if entry.Enricher == nil {
			t.Fatalf("enricher %q is wired with a nil implementation", entry.Name)
		}
		got = append(got, entry.Name)
	}
	want := config.SourcesInRole(config.RoleMetadataEnricher)
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if !reflect.DeepEqual(got, sortedWant) {
		t.Fatalf("metadata enrichers = %v, want the catalog's enricher role %v", got, sortedWant)
	}
	if system.App.Enricher == nil {
		t.Fatal("Service.Enricher is unwired: typed version-relation traversal reads it, and crossref_metadata is enabled")
	}
}

// The retraction sentinel is wired from exactly the catalog's retraction role:
// enabling that source builds it, and disabling it leaves nothing behind. If
// the wiring read some other source name, the second half would still find a
// sentinel and fail.
func TestRetractionSentinelMatchesCatalogRetractionRole(t *testing.T) {
	names := config.SourcesInRole(config.RoleRetractionSource)
	if len(names) == 0 {
		t.Fatal("no source declares the retraction role, but bootstrap still builds a sentinel")
	}
	enabled := newSystemForRoles(t, allSourcesEnabled(t))
	if enabled.Retractions == nil {
		t.Fatalf("retraction sentinel unwired with %v enabled", names)
	}
	cfg := allSourcesEnabled(t)
	for _, name := range names {
		source := cfg.Sources[name]
		source.Enabled = false
		cfg.Sources[name] = source
	}
	disabled := newSystemForRoles(t, cfg)
	if disabled.Retractions != nil {
		t.Fatalf("retraction sentinel survived disabling every retraction-role source %v", names)
	}
}

// discoverySources' switch and the catalog's discovery role must agree. Feeding
// every catalog name in catalog order makes the switch itself the answer: a
// name it has no case for produces no backend, and a case for a source that
// does not declare the role produces one nobody declared. config.validate()
// refuses such a name before it ever reaches here, so this is the check that
// keeps that refusal honest.
func TestDiscoveryBackendsMatchCatalogDiscoveryRole(t *testing.T) {
	cfg := config.Default()
	cfg.Discovery.Sources = config.SourceNames()
	backends, err := discoverySources(cfg, floorTestBudgets(t), &countingInner{})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(backends))
	for _, backend := range backends {
		got = append(got, backend.Name())
	}
	want := make([]string, 0, len(got))
	for _, name := range config.SourceNames() {
		for _, discoveryName := range config.SourcesInRole(config.RoleDiscoveryBackend) {
			if name == discoveryName {
				want = append(want, name)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discovery backends = %v, want the catalog's discovery role %v", got, want)
	}
}
