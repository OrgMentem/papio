// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bench

import (
	"reflect"
	"testing"

	"papio/internal/config"
)

// Bench must measure the resolver precedence papio actually ships, so its chain
// is production's acquisition order with the sources it does not fixture
// removed — never an order of its own. The second copy of that order is what
// this test exists to keep out: bench used to hold its own entry table, which
// stayed correct only for as long as nobody reordered production.
func TestResolverEntriesFollowCatalogAcquisitionOrder(t *testing.T) {
	rig := newSourceRig(overlaySources)
	defer rig.close()
	entries, err := resolverEntries(config.Default(), rig)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Adapter.Name())
	}
	want := make([]string, 0, len(benchAcquisitionSources))
	for _, name := range config.SourcesInRole(config.RoleAcquisitionResolver) {
		for _, wired := range benchAcquisitionSources {
			if name == wired {
				want = append(want, name)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bench resolver order = %v, want production's acquisition order filtered to bench's sources %v", got, want)
	}
	if len(got) != len(benchAcquisitionSources) {
		t.Fatalf("bench wired %d sources but built %d entries: %v", len(benchAcquisitionSources), len(got), got)
	}
}

// A bench source the catalog does not call an acquisition resolver must stop
// the run, not shorten the chain: a silently missing resolver makes the current
// overlay look worse than the code it is measuring.
func TestResolverEntriesRejectANonAcquisitionSource(t *testing.T) {
	original := benchAcquisitionSources
	benchAcquisitionSources = append(append([]string(nil), original...), config.SourceCrossrefMetadata)
	t.Cleanup(func() { benchAcquisitionSources = original })
	rig := newSourceRig(overlaySources)
	defer rig.close()
	if _, err := resolverEntries(config.Default(), rig); err == nil {
		t.Fatal("resolverEntries accepted crossref_metadata as an acquisition resolver; it has no acquisition role in the catalog")
	}
}

// Every source bench serves a fixture host for is the set it wires, plus the
// crossref_metadata enrichment seam. A resolver with no fixture host answers
// every request against an empty base URL instead of the rig.
func TestOverlaySourcesCoverEveryWiredSource(t *testing.T) {
	for _, name := range append(append([]string(nil), benchAcquisitionSources...), config.SourceCrossrefMetadata) {
		found := false
		for _, served := range overlaySources {
			if served == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("overlaySources has no fixture host for %q", name)
		}
	}
}
