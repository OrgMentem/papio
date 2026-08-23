// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package ownership

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// A doubled slash survives normalization on purpose. DataCite holds
// 10.48612//monograph-2025-2 and 10.48612/monograph-2025-2 as two separately
// registered works with different titles, so collapsing runs merges two real
// names — answering "already held" for a work the library does not have, and
// pointing a citation check at the wrong source. Some publishers do mint both
// forms for one article, so the duplicate this would prevent is real; it is
// simply the far cheaper error. Verified against the live DataCite API.
func TestNormalizeIdentifier(t *testing.T) {
	cases := []struct {
		name string
		in   Identifier
		want string
		ok   bool
	}{
		{"doi bare", Identifier{KindDOI, "10.1234/ABC"}, "doi:10.1234/abc", true},
		{"doi url", Identifier{KindDOI, "https://doi.org/10.1234/abc"}, "doi:10.1234/abc", true},
		{"doi dx url", Identifier{KindDOI, "https://dx.doi.org/10.1234/abc"}, "doi:10.1234/abc", true},
		{"doi scheme prefix", Identifier{KindDOI, "doi:10.1234/abc"}, "doi:10.1234/abc", true},
		{"doi without 10 prefix is not a doi", Identifier{KindDOI, "abc/123"}, "", false},
		{"arxiv bare", Identifier{KindArXiv, "2401.00001"}, "arxiv:2401.00001", true},
		{"arxiv prefixed", Identifier{KindArXiv, "arXiv:2401.00001"}, "arxiv:2401.00001", true},
		{"arxiv abs url", Identifier{KindArXiv, "https://arxiv.org/abs/2401.00001"}, "arxiv:2401.00001", true},
		{"arxiv version suffix names the same work", Identifier{KindArXiv, "2401.00001v3"}, "arxiv:2401.00001", true},
		{"arxiv uppercase version suffix names the same work", Identifier{KindArXiv, "2401.00001V2"}, "arxiv:2401.00001", true},
		{"arxiv old style keeps its subject class", Identifier{KindArXiv, "cs.CL/0101001"}, "arxiv:cs.cl/0101001", true},
		{"pmid", Identifier{KindPMID, "12345678"}, "pmid:12345678", true},
		{"pmid leading zeros", Identifier{KindPMID, "0012345"}, "pmid:12345", true},
		{"pmid non numeric", Identifier{KindPMID, "PMC12345"}, "", false},
		{"isbn is not a matchable kind", Identifier{"isbn", "9780262035613"}, "", false},
		{"unknown kind", Identifier{"openalex", "W123"}, "", false},
		{"empty value", Identifier{KindDOI, "   "}, "", false},
		{"doubled slash is a different registered work, never collapsed",
			Identifier{KindDOI, "10.48612//monograph-2025-2"}, "doi:10.48612//monograph-2025-2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeIdentifier(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if got.Key() != tc.want {
				t.Fatalf("key = %q, want %q", got.Key(), tc.want)
			}
		})
	}
}

func TestQueryFor(t *testing.T) {
	tests := []struct {
		name       string
		doi        string
		arxiv      string
		pmid       string
		version    string
		entityKind string
		want       Query
	}{
		{
			name: "doi",
			doi:  "https://doi.org/10.1234/ABC",
			want: Query{Identifiers: []Identifier{{Kind: KindDOI, Value: "10.1234/abc"}}},
		},
		{
			name:  "arxiv",
			arxiv: "arXiv:2401.00001",
			want:  Query{Identifiers: []Identifier{{Kind: KindArXiv, Value: "2401.00001"}}},
		},
		{
			name: "pmid",
			pmid: "0012345",
			want: Query{Identifiers: []Identifier{{Kind: KindPMID, Value: "12345"}}},
		},
		{
			name:       "all identifiers in canonical order",
			doi:        "10.1234/DOI",
			arxiv:      "2401.00001v2",
			pmid:       "0012345",
			version:    VersionPublished,
			entityKind: EntityArticle,
			want: Query{
				Identifiers: []Identifier{
					{Kind: KindDOI, Value: "10.1234/doi"},
					{Kind: KindArXiv, Value: "2401.00001"},
					{Kind: KindPMID, Value: "12345"},
				},
				DesiredVersion: VersionPublished,
				EntityKind:     EntityArticle,
			},
		},
		{
			name: "all inputs empty",
			want: Query{},
		},
		{
			name:       "invalid and empty identifiers are dropped",
			doi:        "not-a-doi",
			arxiv:      " ",
			pmid:       "PMC12345",
			version:    VersionAny,
			entityKind: EntityUnknown,
			want: Query{
				DesiredVersion: VersionAny,
				EntityKind:     EntityUnknown,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := QueryFor(tt.doi, tt.arxiv, tt.pmid, tt.version, tt.entityKind); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("QueryFor() = %+v, want %+v", got, tt.want)
			}
		})
	}

	// ADR-0008 makes ownership normalization version-collapsing: v3 names the
	// same work as the unversioned arXiv identifier.
	versioned := QueryFor("", "2401.00001v3", "", "", "")
	unversioned := QueryFor("", "2401.00001", "", "", "")
	if !reflect.DeepEqual(versioned, unversioned) {
		t.Fatalf("versioned QueryFor() = %+v, unversioned = %+v", versioned, unversioned)
	}

	// ADR-0008 preserves repeated slash runs because these are separate
	// registered works and must not collide.
	doubledSlash := QueryFor("10.48612//monograph-2025-2", "", "", "", "")
	singleSlash := QueryFor("10.48612/monograph-2025-2", "", "", "", "")
	if doubledSlash.Identifiers[0].Value != "10.48612//monograph-2025-2" {
		t.Fatalf("doubled slash = %+v, want the repeated slash preserved", doubledSlash)
	}
	if reflect.DeepEqual(doubledSlash, singleSlash) {
		t.Fatalf("doubled and single slash queries collided: %+v", doubledSlash)
	}
}

// Decide carries ADR-0008 invariant 2: only a fresh, explicit artifact-present
// claim may suppress acquisition. Every case here is a way that could go wrong
// and silently withhold a paper the user asked for.
func TestDecideSuppression(t *testing.T) {
	doi := Identifier{KindDOI, "10.1234/abc"}
	arxiv := Identifier{KindArXiv, "2401.00001"}
	cases := []struct {
		name         string
		query        Query
		claims       []Claim
		wantSuppress bool
		wantRecord   bool
	}{
		{
			name:         "no claims suppresses nothing",
			query:        Query{Identifiers: []Identifier{doi}},
			wantSuppress: false,
		},
		{
			name:         "record present without an artifact never suppresses",
			query:        Query{Identifiers: []Identifier{doi}},
			claims:       []Claim{{Source: "refs", Matched: doi, RecordPresent: true, Artifact: ArtifactUnknown}},
			wantSuppress: false,
			wantRecord:   true,
		},
		{
			name:         "artifact explicitly missing never suppresses",
			query:        Query{Identifiers: []Identifier{doi}},
			claims:       []Claim{{Source: "refs", Matched: doi, RecordPresent: true, Artifact: ArtifactMissing}},
			wantSuppress: false,
			wantRecord:   true,
		},
		{
			name:         "artifact present with unspecified desired version suppresses",
			query:        Query{Identifiers: []Identifier{doi}},
			claims:       []Claim{{Source: "refs", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent}},
			wantSuppress: true,
			wantRecord:   true,
		},
		{
			name:         "unknown held version cannot satisfy an explicit published request",
			query:        Query{Identifiers: []Identifier{doi}, DesiredVersion: VersionPublished},
			claims:       []Claim{{Source: "refs", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent, ArtifactVersion: VersionUnknown}},
			wantSuppress: false,
			wantRecord:   true,
		},
		{
			name:         "known published version satisfies a published request",
			query:        Query{Identifiers: []Identifier{doi}, DesiredVersion: VersionPublished},
			claims:       []Claim{{Source: "holdings", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent, ArtifactVersion: VersionPublished}},
			wantSuppress: true,
			wantRecord:   true,
		},
		{
			name:         "held preprint cannot satisfy a published request",
			query:        Query{Identifiers: []Identifier{doi}, DesiredVersion: VersionPublished},
			claims:       []Claim{{Source: "holdings", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent, ArtifactVersion: VersionPreprint}},
			wantSuppress: false,
			wantRecord:   true,
		},
		{
			name:         "arxiv match cannot satisfy a published request even when a version is claimed",
			query:        Query{Identifiers: []Identifier{arxiv}, DesiredVersion: VersionPublished},
			claims:       []Claim{{Source: "holdings", Matched: arxiv, RecordPresent: true, Artifact: ArtifactPresent, ArtifactVersion: VersionPublished}},
			wantSuppress: false,
			wantRecord:   true,
		},
		{
			name:         "arxiv match cannot satisfy an accepted request",
			query:        Query{Identifiers: []Identifier{arxiv}, DesiredVersion: VersionAccepted},
			claims:       []Claim{{Source: "holdings", Matched: arxiv, RecordPresent: true, Artifact: ArtifactPresent}},
			wantSuppress: false,
			wantRecord:   true,
		},
		{
			name:         "arxiv match satisfies an explicit any request",
			query:        Query{Identifiers: []Identifier{arxiv}, DesiredVersion: VersionAny},
			claims:       []Claim{{Source: "refs", Matched: arxiv, RecordPresent: true, Artifact: ArtifactPresent}},
			wantSuppress: true,
			wantRecord:   true,
		},
		{
			name:         "a held book never satisfies a chapter request",
			query:        Query{Identifiers: []Identifier{doi}, EntityKind: EntityChapter},
			claims:       []Claim{{Source: "refs", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent, EntityKind: EntityBook}},
			wantSuppress: false,
			wantRecord:   true,
		},
		{
			name:         "a held book satisfies a book request",
			query:        Query{Identifiers: []Identifier{doi}, EntityKind: EntityBook},
			claims:       []Claim{{Source: "refs", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent, EntityKind: EntityBook}},
			wantSuppress: true,
			wantRecord:   true,
		},
		{
			name:  "one usable claim among unusable ones still suppresses",
			query: Query{Identifiers: []Identifier{doi}},
			claims: []Claim{
				{Source: "citations", Matched: doi, RecordPresent: true, Artifact: ArtifactMissing},
				{Source: "pdfs", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent},
			},
			wantSuppress: true,
			wantRecord:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.query, WorkResult{Claims: tc.claims})
			if got.Suppress != tc.wantSuppress {
				t.Fatalf("Suppress = %v, want %v", got.Suppress, tc.wantSuppress)
			}
			if got.RecordPresent != tc.wantRecord {
				t.Fatalf("RecordPresent = %v, want %v", got.RecordPresent, tc.wantRecord)
			}
			if got.Suppress && got.Source == "" {
				t.Fatal("a suppressing decision must name the source that drove it")
			}
		})
	}
}

// stubProvider reports exactly the claims and health it is given.
type stubProvider struct {
	name   string
	claims [][]Claim
	health SourceHealth
}

func (s stubProvider) Name() string { return s.name }

func (s stubProvider) Lookup(context.Context, []Query) ([][]Claim, SourceHealth) {
	return s.claims, s.health
}

func TestRegistryNames(t *testing.T) {
	registry := NewRegistry(
		stubProvider{name: "zotero"},
		stubProvider{name: "papis"},
		stubProvider{name: "calibre"},
	)
	if got, want := registry.Names(), []string{"zotero", "papis", "calibre"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}

	empty := NewRegistry()
	if got := empty.Names(); got != nil {
		t.Fatalf("Names() for an empty registry = %v, want nil", got)
	}
}

func TestAggregateUnionsPositiveClaims(t *testing.T) {
	doi := Identifier{KindDOI, "10.1234/abc"}
	queries := []Query{{Identifiers: []Identifier{doi}}}
	// One library not holding a paper never negates another that does: absence
	// is source-scoped (ADR-0008 invariant 10).
	empty := stubProvider{name: "zotero-export", claims: [][]Claim{nil}, health: SourceHealth{Complete: true}}
	holds := stubProvider{
		name:   "papis",
		claims: [][]Claim{{{Source: "papis", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent}}},
		health: SourceHealth{Complete: true, EntryCount: 1},
	}
	result := Aggregate(context.Background(), []Provider{empty, holds}, queries)
	if !result.Complete() {
		t.Fatalf("two healthy sources must aggregate as complete: %+v", result.Sources)
	}
	if got := Decide(queries[0], result.Works[0]); !got.Suppress || got.Source != "papis" {
		t.Fatalf("decision = %+v, want suppression from papis", got)
	}
	if len(result.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(result.Sources))
	}
	if result.Sources[0].Name != "zotero-export" {
		t.Fatalf("health name not defaulted from the provider: %+v", result.Sources[0])
	}
}

// A failed provider must never contribute a negative fact, and must never let a
// caller read "no claims" as "not held" (ADR-0008 invariant 1).
func TestAggregateFailedProviderIsIncompleteNotNegative(t *testing.T) {
	doi := Identifier{KindDOI, "10.1234/abc"}
	queries := []Query{{Identifiers: []Identifier{doi}}}
	broken := stubProvider{name: "papis", health: SourceHealth{Complete: false, FailureCode: FailureUnreadable}}
	result := Aggregate(context.Background(), []Provider{broken}, queries)
	if result.Complete() {
		t.Fatal("an unreadable source must make the aggregate incomplete")
	}
	if got := result.Incomplete(); len(got) != 1 || got[0] != "papis" {
		t.Fatalf("Incomplete() = %v, want [papis]", got)
	}
	if got := Decide(queries[0], result.Works[0]); got.Suppress {
		t.Fatal("a failed lookup must not suppress acquisition")
	}
}

// A provider returning a claim slice misaligned with the queries is a bug in
// that provider; its claims must be discarded rather than applied to the wrong
// work, which would suppress an unrelated paper.
func TestAggregateRejectsMisalignedClaims(t *testing.T) {
	doi := Identifier{KindDOI, "10.1234/abc"}
	queries := []Query{{Identifiers: []Identifier{doi}}, {Identifiers: []Identifier{{KindDOI, "10.1234/def"}}}}
	misaligned := stubProvider{
		name:   "papis",
		claims: [][]Claim{{{Source: "papis", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent}}},
		health: SourceHealth{Complete: true, EntryCount: 1},
	}
	result := Aggregate(context.Background(), []Provider{misaligned}, queries)
	for i, work := range result.Works {
		if len(work.Claims) != 0 {
			t.Fatalf("work %d kept claims from a misaligned provider: %+v", i, work.Claims)
		}
	}
	if result.Complete() {
		t.Fatal("a misaligned provider response must make the aggregate incomplete")
	}
	if got := result.Sources[0].FailureCode; got != FailureMisaligned {
		t.Fatalf("FailureCode = %q, want %q", got, FailureMisaligned)
	}
}

func TestAggregateRejectsSwappedSameLengthClaims(t *testing.T) {
	first := Identifier{KindDOI, "10.1234/first"}
	second := Identifier{KindDOI, "10.1234/second"}
	queries := []Query{{Identifiers: []Identifier{first}}, {Identifiers: []Identifier{second}}}
	swapped := stubProvider{
		name: "papis",
		claims: [][]Claim{
			{{Source: "papis", Matched: second, RecordPresent: true, Artifact: ArtifactPresent}},
			{{Source: "papis", Matched: first, RecordPresent: true, Artifact: ArtifactPresent}},
		},
		health: SourceHealth{Complete: true, EntryCount: 2},
	}
	result := Aggregate(context.Background(), []Provider{swapped}, queries)
	if result.Complete() {
		t.Fatal("swapped same-length claims must make the aggregate incomplete")
	}
	if got := result.Sources[0].FailureCode; got != FailureMisaligned {
		t.Fatalf("FailureCode = %q, want %q", got, FailureMisaligned)
	}
	for i, work := range result.Works {
		if len(work.Claims) != 0 {
			t.Fatalf("work %d kept swapped claims: %+v", i, work.Claims)
		}
	}
}

func TestAggregateStaleHealthStampsClaims(t *testing.T) {
	doi := Identifier{KindDOI, "10.1234/abc"}
	queries := []Query{{Identifiers: []Identifier{doi}}}
	stale := stubProvider{
		name:   "papis",
		claims: [][]Claim{{{Source: "papis", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent}}},
		health: SourceHealth{Complete: true, Stale: true, EntryCount: 1},
	}
	result := Aggregate(context.Background(), []Provider{stale}, queries)
	if len(result.Works[0].Claims) != 1 || !result.Works[0].Claims[0].Stale {
		t.Fatalf("stale source must produce stale claims: %+v", result.Works[0].Claims)
	}
	if got := Decide(queries[0], result.Works[0]); got.Suppress {
		t.Fatal("a stale source must not suppress acquisition")
	}

}
func TestAggregateSkipsNilProvider(t *testing.T) {
	result := Aggregate(context.Background(), []Provider{nil}, []Query{{}})
	if len(result.Sources) != 0 {
		t.Fatalf("sources = %+v, want none", result.Sources)
	}
	if !result.Complete() {
		t.Fatal("no configured sources is complete, not incomplete")
	}
}

func TestBuildIndexAndClaims(t *testing.T) {
	entries := []Entry{
		{
			Identifiers: []Identifier{{KindDOI, "10.1234/ABC"}, {KindArXiv, "arXiv:2401.00001v2"}},
			Artifact:    ArtifactPresent,
		},
		{Identifiers: []Identifier{{"isbn", "9780262035613"}}}, // no matchable identifier
		{Identifiers: []Identifier{{KindPMID, "0012345"}}, Artifact: "nonsense"},
	}
	index := BuildIndex(entries)
	if index.Len() != 2 {
		t.Fatalf("Len = %d, want 2", index.Len())
	}
	if index.SkippedNoIdentifier != 1 {
		t.Fatalf("SkippedNoIdentifier = %d, want 1", index.SkippedNoIdentifier)
	}

	// Both identifiers reach the same entry, and it is claimed once, not twice.
	query := Query{Identifiers: []Identifier{{KindDOI, "10.1234/abc"}, {KindArXiv, "2401.00001"}}}
	claims := index.Claims("refs", query)
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want 1 deduplicated claim: %+v", len(claims), claims)
	}
	if claims[0].Artifact != ArtifactPresent || claims[0].Source != "refs" {
		t.Fatalf("claim = %+v", claims[0])
	}

	// An unrecognized artifact state degrades to unknown rather than present:
	// nothing may suppress by accident.
	pmid := index.Claims("refs", Query{Identifiers: []Identifier{{KindPMID, "12345"}}})
	if len(pmid) != 1 {
		t.Fatalf("pmid claims = %+v", pmid)
	}
	if pmid[0].Artifact != ArtifactUnknown {
		t.Fatalf("artifact = %q, want %q", pmid[0].Artifact, ArtifactUnknown)
	}

	if got := index.Claims("refs", Query{Identifiers: []Identifier{{KindDOI, "10.9999/absent"}}}); got != nil {
		t.Fatalf("a miss must yield no claim, got %+v", got)
	}
}

func TestNilIndexIsUsable(t *testing.T) {
	var index *Index
	if index.Len() != 0 {
		t.Fatal("nil index length")
	}
	if got := index.Claims("refs", Query{Identifiers: []Identifier{{KindDOI, "10.1/a"}}}); got != nil {
		t.Fatalf("nil index claims = %+v", got)
	}
}

func TestResultIncompleteIsSorted(t *testing.T) {
	result := Result{Sources: []SourceHealth{
		{Name: "zebra", Complete: false},
		{Name: "alpha", Complete: false},
		{Name: "healthy", Complete: true, LastSuccess: time.Now()},
	}}
	got := result.Incomplete()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zebra" {
		t.Fatalf("Incomplete() = %v, want [alpha zebra]", got)
	}
}

// A stale claim is evidence but not licence to skip: the file it describes may
// have been moved or deleted since the index was built.
func TestDecideIgnoresStaleClaimsForSuppression(t *testing.T) {
	doi := Identifier{KindDOI, "10.1234/abc"}
	work := WorkResult{Claims: []Claim{
		{Source: "papis", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent, Stale: true},
	}}
	got := Decide(Query{Identifiers: []Identifier{doi}}, work)
	if got.Suppress {
		t.Fatal("a stale artifact-present claim must not suppress acquisition")
	}
	if !got.RecordPresent {
		t.Fatal("a stale claim still reports that the record is known")
	}
}

// Health governs the right to conclude absence, not the value of positive
// evidence: a provider serving a last-known-good index still knows things.
func TestAggregateKeepsClaimsFromAnUnhealthySource(t *testing.T) {
	doi := Identifier{KindDOI, "10.1234/abc"}
	queries := []Query{{Identifiers: []Identifier{doi}}}
	degraded := stubProvider{
		name:   "papis",
		claims: [][]Claim{{{Source: "papis", Matched: doi, RecordPresent: true, Artifact: ArtifactPresent}}},
		health: SourceHealth{Complete: false, FailureCode: FailureParse, EntryCount: 4000},
	}
	result := Aggregate(context.Background(), []Provider{degraded}, queries)
	if result.Complete() {
		t.Fatal("a failed refresh must leave the aggregate incomplete")
	}
	if got := Decide(queries[0], result.Works[0]); !got.Suppress {
		t.Fatal("a fresh last-known-good positive claim should still suppress a redundant download")
	}
}

func TestSatisfiesEntityRejectsKnownMismatches(t *testing.T) {
	known := []string{EntityArticle, EntityChapter, EntityBook}
	for _, requested := range known {
		for _, held := range known {
			if requested == held {
				continue
			}
			if satisfiesEntity(requested, Claim{EntityKind: held}) {
				t.Errorf("requested %q incorrectly satisfied by held %q", requested, held)
			}
		}
	}
	for _, tc := range []struct {
		requested string
		held      string
	}{
		{EntityUnknown, EntityUnknown},
		{EntityUnknown, EntityArticle},
		{EntityBook, EntityUnknown},
	} {
		if !satisfiesEntity(tc.requested, Claim{EntityKind: tc.held}) {
			t.Errorf("requested %q should be satisfied by held %q", tc.requested, tc.held)
		}
	}
}

func TestIndexClaimsPrefersDOIRegardlessOfQueryOrder(t *testing.T) {
	doi := Identifier{KindDOI, "10.1234/abc"}
	arxiv := Identifier{KindArXiv, "2401.00001"}
	index := BuildIndex([]Entry{{
		Identifiers:     []Identifier{doi, arxiv},
		Artifact:        ArtifactPresent,
		ArtifactVersion: VersionPublished,
	}})
	for _, identifiers := range [][]Identifier{{doi, arxiv}, {arxiv, doi}} {
		query := Query{Identifiers: identifiers, DesiredVersion: VersionPublished}
		claims := index.Claims("refs", query)
		if len(claims) != 1 {
			t.Fatalf("claims = %d, want one claim: %+v", len(claims), claims)
		}
		if got := claims[0].Matched; got != doi {
			t.Fatalf("Matched = %+v, want DOI %+v", got, doi)
		}
		if decision := Decide(query, WorkResult{Claims: claims}); !decision.Suppress {
			t.Fatalf("DOI match must suppress a published request: %+v", decision)
		}
	}
}
