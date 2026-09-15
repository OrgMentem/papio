// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// shippedSourceDefaults is the [sources.*] baseline papio has shipped, written
// out independently of the catalog. A config value is a promise to every
// existing install: a pacing figure, an enablement decision, or an armed credit
// fuse changed by accident is a behaviour change nobody chose. So this table is
// deliberately a second, hand-maintained copy — changing a default must mean
// changing this test too, with the reason in the commit.
var shippedSourceDefaults = map[string]Source{
	SourceArXiv:            {Enabled: true, RatePerSec: 1, Burst: 1},
	SourceEuropePMC:        {Enabled: true, RatePerSec: 2, Burst: 2},
	SourceUnpaywall:        {Enabled: true, RatePerSec: 1, Burst: 1},
	SourceOpenAlex:         {Enabled: false, RatePerSec: 2, Burst: 2, DailyCreditFraction: DefaultDailyCreditFraction},
	SourceSemanticScholar:  {Enabled: true, RatePerSec: 1, Burst: 1},
	SourceCORE:             {Enabled: false, RatePerSec: 0.4, Burst: 1},
	SourceCrossrefTDM:      {Enabled: false, RatePerSec: 1, Burst: 1},
	SourceCrossrefMetadata: {Enabled: true, RatePerSec: 1, Burst: 1},
	SourceRetractionWatch:  {Enabled: true, RatePerSec: 1, Burst: 1},
	SourceOpenAIRE:         {Enabled: true, RatePerSec: OpenAIREKeylessRatePerSec, Burst: 1},
}

// shippedSourceRoles is the role declaration papio has shipped, written out
// independently of the catalog for the same reason as the defaults above:
// losing a role silently unwires a whole capability (OpenAlex enriches,
// discovers AND resolves against one keyed allowance), and gaining one claims
// wiring that does not exist.
var shippedSourceRoles = map[string]SourceRole{
	SourceArXiv:            RoleAcquisitionResolver | RoleDiscoveryBackend,
	SourceEuropePMC:        RoleAcquisitionResolver,
	SourceUnpaywall:        RoleAcquisitionResolver,
	SourceOpenAlex:         RoleAcquisitionResolver | RoleDiscoveryBackend | RoleMetadataEnricher,
	SourceSemanticScholar:  RoleAcquisitionResolver | RoleDiscoveryBackend,
	SourceCORE:             RoleAcquisitionResolver,
	SourceCrossrefTDM:      RoleAcquisitionResolver,
	SourceCrossrefMetadata: RoleMetadataEnricher,
	SourceRetractionWatch:  RoleRetractionSource,
	SourceOpenAIRE:         RoleAcquisitionResolver,
}

func TestDefaultSourcesAreTheShippedBaseline(t *testing.T) {
	got := Default().Sources
	if !reflect.DeepEqual(got, shippedSourceDefaults) {
		t.Fatalf("default sources = %+v, want the shipped baseline %+v", got, shippedSourceDefaults)
	}
	if got[SourceOpenAlex].DailyCreditFraction == 0 {
		t.Error("OpenAlex ships with an unarmed credit fuse; papio init would install an inert ceiling")
	}
	if got[SourceOpenAIRE].RatePerSec != OpenAIREKeylessRatePerSec {
		t.Errorf("OpenAIRE ships at %v/s, want the keyless tier %v/s: pacing to the authenticated ceiling keyless is 120x what OpenAIRE allows",
			got[SourceOpenAIRE].RatePerSec, OpenAIREKeylessRatePerSec)
	}
}

// Default() must hand out a private map: the catalog's rows are process-wide,
// and a caller that flips Enabled on the returned map (internal/bench does)
// would otherwise reconfigure every later Default().
func TestDefaultSourcesAreNotSharedWithTheCatalog(t *testing.T) {
	first := Default().Sources
	source := first[SourceArXiv]
	source.Enabled = !source.Enabled
	source.RatePerSec = 99
	first[SourceArXiv] = source
	second := Default().Sources
	if !reflect.DeepEqual(second, shippedSourceDefaults) {
		t.Fatalf("mutating one Default() changed the next: %+v", second[SourceArXiv])
	}
}

func TestCatalogRolesAreTheShippedRoles(t *testing.T) {
	got := make(map[string]SourceRole, len(sourceCatalog))
	for _, entry := range sourceCatalog {
		if _, duplicate := got[entry.Name]; duplicate {
			t.Fatalf("catalog lists %q twice", entry.Name)
		}
		got[entry.Name] = entry.Roles
	}
	if !reflect.DeepEqual(got, shippedSourceRoles) {
		t.Fatalf("catalog roles = %v, want the shipped roles %v", got, shippedSourceRoles)
	}
	if len(SourcesInRole(RoleAcquisitionResolver)) == 0 {
		t.Error("no source declares the acquisition role; the resolver chain would be empty")
	}
}

// The acquisition role's order is resolver precedence — internal/bootstrap
// builds the chain in it and internal/bench reproduces it — so it is a pinned
// contract, not an artifact of how the rows happen to be typed.
func TestAcquisitionRoleOrderIsResolverPrecedence(t *testing.T) {
	want := []string{
		SourceArXiv,
		SourceEuropePMC,
		SourceUnpaywall,
		SourceOpenAlex,
		SourceSemanticScholar,
		SourceCORE,
		SourceCrossrefTDM,
		SourceOpenAIRE,
	}
	if got := SourcesInRole(RoleAcquisitionResolver); !reflect.DeepEqual(got, want) {
		t.Fatalf("acquisition order = %v, want %v", got, want)
	}
	if got := SourcesInRole(RoleDiscoveryBackend); !reflect.DeepEqual(got, []string{SourceArXiv, SourceOpenAlex, SourceSemanticScholar}) {
		t.Fatalf("discovery order = %v, want arxiv, openalex, then semanticscholar (merge preference)", got)
	}
}

// Every name papio validates against must be a name it advertises: the two used
// to be a map and a hand-written const string, so a source could be accepted
// and never named, or named and rejected.
func TestValidSourceNamesListNamesEveryCatalogSource(t *testing.T) {
	for _, name := range SourceNames() {
		if !strings.Contains(validSourceNamesList, name) {
			t.Errorf("valid name %q is missing from the error text %q", name, validSourceNamesList)
		}
	}
	for _, name := range strings.Split(validSourceNamesList, ", ") {
		if _, known := validSourceNames[name]; !known {
			t.Errorf("error text advertises %q, which validate() rejects", name)
		}
	}
	if got, want := len(SourceNames()), len(sourceCatalog); got != want {
		t.Fatalf("SourceNames() returned %d names for %d catalog rows", got, want)
	}
}

// discovery.sources selection is discovery's own enablement, so a name papio
// has no discovery backend for must fail closed. It used to be checked against
// two hardcoded names beside the catalog; now the catalog's role decides, and
// an acquisition-only source is the case that proves it still refuses.
func TestDiscoverySourcesAcceptExactlyTheDiscoveryRole(t *testing.T) {
	for _, name := range SourcesInRole(RoleDiscoveryBackend) {
		cfg := Default()
		cfg.AccessMode = ModeConservative
		cfg.Discovery.Sources = []string{name}
		if err := cfg.validate(); err != nil {
			t.Errorf("discovery backend %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{SourceUnpaywall, SourceCrossrefMetadata, "not_a_source"} {
		cfg := Default()
		cfg.AccessMode = ModeConservative
		cfg.Discovery.Sources = []string{name}
		err := cfg.validate()
		if err == nil {
			t.Errorf("discovery.sources accepted %q, which declares no discovery role", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("rejection of %q = %q, want the offending name in the message", name, err)
		}
	}
}

// The fail-closed rule survives the catalog restructuring in both halves: an
// unknown [sources.*] key is still a load error naming every accepted source,
// and a name papio shipped and then removed is still tolerated and dropped so
// an existing config stays parseable on upgrade.
func TestLoadFailsClosedOnUnknownSourceAndToleratesRemovedOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[sources.foo]\nenabled=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("[sources.foo] loaded; an unknown source name must fail closed, not silently do nothing")
	}
	if !strings.Contains(err.Error(), "sources.foo") || !strings.Contains(err.Error(), validSourceNamesList) {
		t.Fatalf("rejection = %q, want it to name sources.foo and list %q", err, validSourceNamesList)
	}

	for removed := range removedSourceNames {
		path := filepath.Join(t.TempDir(), "config.toml")
		body := "access_mode='conservative'\n[sources." + removed + "]\nenabled=true\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("config naming removed source %q must still load: %v", removed, err)
		}
		if _, ok := cfg.Sources[removed]; ok {
			t.Errorf("removed source %q survived Load; the next config save must rewrite the file without it", removed)
		}
	}
}
