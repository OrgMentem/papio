// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"papio/internal/work"
)

func TestScoreClassifiesTitleMatches(t *testing.T) {
	const title = "My Advisor, Her AI, and Me: Doctoral Student Experiences"
	for _, tc := range []struct {
		name     string
		query    string
		title    string
		wantKind string
		// confident records the contract callers actually branch on.
		confident bool
	}{
		{
			name:      "exact title ignoring punctuation and case",
			query:     "my advisor her ai and me doctoral student experiences",
			title:     title,
			wantKind:  MatchExactTitle,
			confident: true,
		},
		{
			name:      "query is a contiguous fragment of the title",
			query:     "My Advisor, Her AI, and Me",
			title:     title,
			wantKind:  MatchTitlePhrase,
			confident: true,
		},
		{
			name:      "query carries the whole title plus pasted context",
			query:     "My Advisor, Her AI, and Me: Doctoral Student Experiences (2026 preprint)",
			title:     title,
			wantKind:  MatchTitlePhrase,
			confident: true,
		},
		{
			name:      "same words out of order still matches",
			query:     "doctoral student experiences with my advisor and her ai",
			title:     title,
			wantKind:  MatchTitleTokens,
			confident: true,
		},
		{
			name:      "unrelated title is weak; it shares no words with the query",
			query:     "trust asymmetries in human ai collaboration",
			title:     "Artificial intelligence: Multidisciplinary perspectives on emerging challenges",
			wantKind:  MatchWeak,
			confident: false,
		},
		{
			name:      "short query is a keyword search, not a title",
			query:     "advisor ai",
			title:     title,
			wantKind:  MatchUnscored,
			confident: false,
		},
		{
			name:      "citation snowball has no query to judge",
			query:     "",
			title:     title,
			wantKind:  MatchUnscored,
			confident: false,
		},
		{
			name:      "empty title has no query to judge either direction",
			query:     "trust asymmetries in human ai collaboration",
			title:     "",
			wantKind:  MatchUnscored,
			confident: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, kind := score(tc.query, tc.title)
			if kind != tc.wantKind {
				t.Fatalf("score(%q, %q) kind = %q, want %q (score %.3f)", tc.query, tc.title, kind, tc.wantKind, got)
			}
			if Confident(kind) != tc.confident {
				t.Fatalf("Confident(%q) = %t, want %t", kind, Confident(kind), tc.confident)
			}
			if got < 0 || got > 1 {
				t.Fatalf("score = %.3f, want within 0..1", got)
			}
			if tc.wantKind == MatchUnscored && got != 0 {
				t.Fatalf("unscored match carried score %.3f, want 0", got)
			}
		})
	}
}

// An exact title must outrank a mere fragment, and both must outrank reworded
// matches, so the closest answer to the query leads.
func TestScoreOrdersMatchKindsByStrength(t *testing.T) {
	const title = "My Advisor, Her AI, and Me: Doctoral Student Experiences"
	exact, _ := score("my advisor her ai and me doctoral student experiences", title)
	phrase, _ := score("My Advisor, Her AI, and Me", title)
	tokens, _ := score("doctoral student experiences with my advisor and her ai", title)
	if !(exact > phrase && phrase > tokens) {
		t.Fatalf("expected exact > phrase > tokens, got %.3f, %.3f, %.3f", exact, phrase, tokens)
	}
}

// A longer query covering more of the title is the better match of the two.
func TestPhraseScoreRewardsCoverage(t *testing.T) {
	const title = "My Advisor, Her AI, and Me: Doctoral Student Experiences"
	broad, _ := score("My Advisor, Her AI, and Me: Doctoral Student", title)
	narrow, _ := score("My Advisor, Her AI", title)
	if broad <= narrow {
		t.Fatalf("broader phrase scored %.3f, want more than the narrower %.3f", broad, narrow)
	}
}

// The band boundary is a documented guarantee: sorting confident matches by
// score must agree with the kind hierarchy. The worst case is a reworded title
// sharing every word (overlap 1.0) against the weakest possible phrase match.
func TestTokenScoresStayBelowEveryPhraseMatch(t *testing.T) {
	// Same token set, different order, so containment cannot fire.
	maxTokens, kind := score("collaboration ai human in asymmetries trust", "Trust asymmetries in human AI collaboration")
	if kind != MatchTitleTokens {
		t.Fatalf("kind = %q, want %q for a full-overlap rewording", kind, MatchTitleTokens)
	}
	// A phrase match covering almost none of a very long title.
	weakestPhrase, phraseKind := score("trust asymmetries in", "Trust asymmetries in "+strings.Repeat("very long tail ", 40))
	if phraseKind != MatchTitlePhrase {
		t.Fatalf("kind = %q, want %q", phraseKind, MatchTitlePhrase)
	}
	if maxTokens > weakestPhrase {
		t.Fatalf("best token overlap scored %.4f, above the weakest phrase match %.4f — the bands overlap", maxTokens, weakestPhrase)
	}
}

// P1 regression: phrase containment must fire only on whole-word boundaries.
// Without padding, strings.Contains treats a short title as a match merely
// because its letters appear inside a longer query word — "trust" is a
// substring of "mistrust" — which classified an unrelated row as a confident
// title match and promoted it to the top of the results. search is
// MCP-exposed, so an agent consuming match_kind was fed a false positive
// directly, not just a human reading a CLI banner.
func TestScorePhraseContainmentRequiresWholeWords(t *testing.T) {
	const extendedTitle = "Trust Asymmetries in Human-AI Collaboration: An Extended Study"
	for _, tc := range []struct {
		name       string
		query      string
		title      string
		wantPhrase bool
	}{
		{
			name:  "title hides inside one query word (mistrust contains trust)",
			query: "the mistrust of automated systems",
			title: "Trust",
		},
		{
			name:  "title crosses a query word boundary without matching the whole word",
			query: "walking in situational awareness training programs",
			title: "In Situ",
		},
		{
			name:  "single-word title hides inside a longer query word (confusion)",
			query: "confusion about quantum mechanics and entanglement",
			title: "On",
		},
		{
			name:  "single-word title hides inside a longer query word (chemistry)",
			query: "understanding chemistry through experimentation",
			title: "He",
		},
		{
			name:       "genuine whole-word phrase fragment still matches",
			query:      "Trust Asymmetries in Human-AI Collaboration",
			title:      extendedTitle,
			wantPhrase: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, kind := score(tc.query, tc.title)
			if tc.wantPhrase {
				if kind != MatchTitlePhrase {
					t.Fatalf("score(%q, %q) kind = %q, want %q (score %.3f)", tc.query, tc.title, kind, MatchTitlePhrase, got)
				}
				if !Confident(kind) {
					t.Fatalf("score(%q, %q) kind %q, want Confident", tc.query, tc.title, kind)
				}
				return
			}
			if kind == MatchTitlePhrase {
				t.Fatalf("score(%q, %q) = %.3f, %q — matched as a phrase with no whole-word containment", tc.query, tc.title, got, kind)
			}
			if Confident(kind) {
				t.Fatalf("score(%q, %q) kind %q reported Confident, want false", tc.query, tc.title, kind)
			}
		})
	}
}

// The confidentOverlap boundary is a documented guarantee: the kind flips to
// MatchTitleTokens at exactly 0.6 (the check is >=, not >), and the reported
// score keeps the identical overlap*phraseFloor banding on both sides of that
// flip — only the kind changes, never how the score is computed.
func TestScoreConfidentOverlapBoundary(t *testing.T) {
	const query = "alpha bravo charlie delta echo"
	for _, tc := range []struct {
		name        string
		title       string
		wantOverlap float64
		wantKind    string
	}{
		{
			name:        "just below the boundary",
			title:       "Charlie Alpha Echo Foxtrot Golf Hotel",
			wantOverlap: 6.0 / 11.0,
			wantKind:    MatchWeak,
		},
		{
			name:        "exactly at the boundary",
			title:       "Charlie Alpha Echo Foxtrot Golf",
			wantOverlap: 0.6,
			wantKind:    MatchTitleTokens,
		},
		{
			name:        "just above the boundary",
			title:       "Charlie Alpha Echo Foxtrot",
			wantOverlap: 6.0 / 9.0,
			wantKind:    MatchTitleTokens,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, kind := score(query, tc.title)
			if kind != tc.wantKind {
				t.Fatalf("score(%q, %q) kind = %q, want %q", query, tc.title, kind, tc.wantKind)
			}
			wantConfident := tc.wantKind == MatchTitleTokens
			if Confident(kind) != wantConfident {
				t.Fatalf("Confident(%q) = %t, want %t", kind, Confident(kind), wantConfident)
			}
			want := tc.wantOverlap * phraseFloor
			if diff := got - want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("score = %.6f, want overlap*phraseFloor = %.6f (overlap %.6f)", got, want, tc.wantOverlap)
			}
		})
	}
}

func discovered(title string, citedBy int) DiscoveredWork {
	return DiscoveredWork{Work: work.Work{Title: title}, CitedBy: citedBy}
}

func titlesOf(works []DiscoveredWork) []string {
	titles := make([]string, 0, len(works))
	for _, discovered := range works {
		titles = append(titles, discovered.Work.Title)
	}
	return titles
}

// The regression this whole change exists for: a user searched an exact title and
// the wanted paper came back behind unrelated high-citation reviews, because the
// backend ranks on the citation graph. Ranking must lift it to the top.
func TestRankLiftsAnExactTitleAboveBetterCitedNoise(t *testing.T) {
	const wanted = "My Advisor, Her AI, and Me: Doctoral Student Experiences"
	works := []DiscoveredWork{
		discovered("Artificial intelligence: Multidisciplinary perspectives on emerging challenges", 4200),
		discovered("Machine learning in practice: a broad survey", 3100),
		discovered("Trust and automation: a review", 900),
		discovered(wanted, 3),
	}
	rank(works, wanted)
	if got := works[0].Work.Title; got != wanted {
		t.Fatalf("top result = %q, want the exact title match %q (order: %q)", got, wanted, titlesOf(works))
	}
	if works[0].MatchKind != MatchExactTitle {
		t.Fatalf("top match kind = %q, want %q", works[0].MatchKind, MatchExactTitle)
	}
}

// The conservative half of the policy: with nothing confident, papio must not
// pretend to know better than the backend that ordered these.
func TestRankPreservesBackendOrderWithoutAConfidentMatch(t *testing.T) {
	works := []DiscoveredWork{
		discovered("Artificial intelligence: Multidisciplinary perspectives", 4200),
		discovered("Machine learning in practice", 3100),
		discovered("Trust and automation: a review", 900),
	}
	before := titlesOf(works)
	rank(works, "quantum error correction thresholds")
	if got := titlesOf(works); !equalStrings(got, before) {
		t.Fatalf("order changed to %q, want the backend order %q preserved", got, before)
	}
	for _, discovered := range works {
		if Confident(discovered.MatchKind) {
			t.Fatalf("%q reported a confident kind %q", discovered.Work.Title, discovered.MatchKind)
		}
	}
}

// A keyword search must be left alone: below the title threshold papio has no
// opinion worth acting on, and the backend's relevance is the better signal.
func TestRankLeavesKeywordSearchesUntouched(t *testing.T) {
	works := []DiscoveredWork{
		discovered("Trust and automation: a review", 900),
		discovered("Trust asymmetries in collaboration", 5),
	}
	before := titlesOf(works)
	rank(works, "trust")
	if got := titlesOf(works); !equalStrings(got, before) {
		t.Fatalf("short query reordered results to %q, want %q", got, before)
	}
	for _, discovered := range works {
		if discovered.MatchKind != MatchUnscored {
			t.Fatalf("%q kind = %q, want %q for a keyword search", discovered.Work.Title, discovered.MatchKind, MatchUnscored)
		}
	}
}

// Two confident matches sort by strength, not by citation count.
func TestRankOrdersConfidentMatchesByStrength(t *testing.T) {
	const wanted = "Trust Asymmetries in Human-AI Collaboration"
	works := []DiscoveredWork{
		discovered("Trust Asymmetries in Human-AI Collaboration: An Extended Study", 700),
		discovered(wanted, 2),
	}
	rank(works, wanted)
	if works[0].Work.Title != wanted {
		t.Fatalf("top = %q, want the exact match %q", works[0].Work.Title, wanted)
	}
}

// Confident rows appearing partway through a longer non-confident run — not
// just "near the end" or "all non-confident", the two shapes already
// covered — is the shape where an unstable sort would visibly reshuffle:
// several confident rows tie on score (identical title), so only sort
// stability keeps them, and the backend rows around them, in their original
// order.
func TestRankIsStableWithConfidentRowsInterleaved(t *testing.T) {
	const wanted = "Trust Asymmetries in Human-AI Collaboration"
	works := []DiscoveredWork{
		discovered("Backend row 1", 1600),
		discovered("Backend row 2", 1500),
		discovered(wanted, 111),
		discovered("Backend row 3", 1400),
		discovered("Backend row 4", 1300),
		discovered("Backend row 5", 1200),
		discovered(wanted, 222),
		discovered("Backend row 6", 1100),
		discovered("Backend row 7", 1000),
		discovered("Backend row 8", 900),
		discovered(wanted, 333),
		discovered("Backend row 9", 800),
		discovered("Backend row 10", 700),
		discovered("Backend row 11", 600),
	}
	before := titlesOf(works)
	wantBackendOrder := []string{
		"Backend row 1", "Backend row 2", "Backend row 3", "Backend row 4", "Backend row 5",
		"Backend row 6", "Backend row 7", "Backend row 8", "Backend row 9", "Backend row 10", "Backend row 11",
	}
	rank(works, wanted)
	wantConfidentCitedBy := []int{111, 222, 333}
	for i, wantCitedBy := range wantConfidentCitedBy {
		if works[i].Work.Title != wanted || works[i].CitedBy != wantCitedBy {
			t.Fatalf("confident row %d = %q citedBy %d, want %q citedBy %d — tied scores must keep their original relative order",
				i, works[i].Work.Title, works[i].CitedBy, wanted, wantCitedBy)
		}
	}
	gotBackendOrder := titlesOf(works[len(wantConfidentCitedBy):])
	if !equalStrings(gotBackendOrder, wantBackendOrder) {
		t.Fatalf("non-confident rows = %q, want backend order %q preserved (input order was %q)", gotBackendOrder, wantBackendOrder, before)
	}
}

// Every row reports both fields on every path, so the payload shape never varies
// by invocation — the same rule the JSON page contract enforces elsewhere.
func TestRankAlwaysPopulatesMatchFields(t *testing.T) {
	for _, query := range []string{"", "ai", "trust asymmetries in human ai collaboration"} {
		works := []DiscoveredWork{discovered("Trust Asymmetries in Human-AI Collaboration", 2)}
		rank(works, query)
		if works[0].MatchKind == "" {
			t.Fatalf("query %q left match_kind empty", query)
		}
		if works[0].MatchScore < 0 {
			t.Fatalf("query %q produced a negative score %.3f", query, works[0].MatchScore)
		}
	}
}

// Ranking before truncation is the structural half of the fix: a confident match
// the backend ranked below the limit must still reach the caller. Truncating
// first — the old behaviour — dropped it before anything could promote it.
func TestFinalizeRanksBeforeApplyingTheLimit(t *testing.T) {
	const wanted = "Trust Asymmetries in Human-AI Collaboration"
	backend := []DiscoveredWork{
		discovered("Artificial intelligence: Multidisciplinary perspectives", 4200),
		discovered("Machine learning in practice", 3100),
		discovered(wanted, 2),
	}
	got := finalize([][]DiscoveredWork{backend}, SearchParams{Query: wanted, Limit: 1})
	if len(got) != 1 {
		t.Fatalf("finalize returned %d rows, want 1", len(got))
	}
	if got[0].Work.Title != wanted {
		t.Fatalf("single returned row = %q, want the promoted match %q", got[0].Work.Title, wanted)
	}
}

func TestFinalizeDedupesAcrossBackends(t *testing.T) {
	const shared = "10.1000/shared"
	first := []DiscoveredWork{{Work: work.Work{Title: "Shared work", DOI: shared}, Source: "openalex"}}
	second := []DiscoveredWork{{Work: work.Work{Title: "Shared work", DOI: shared}, Source: "semanticscholar"}}
	got := finalize([][]DiscoveredWork{first, second}, SearchParams{Query: "shared work title", Limit: 10})
	if len(got) != 1 {
		t.Fatalf("finalize kept %d copies of the same DOI, want 1", len(got))
	}
	if got[0].Source != "openalex" {
		t.Fatalf("kept the copy from %q, want the preferred backend openalex", got[0].Source)
	}
}

// Scoring and dedupe are separate passes inside finalize, but no test — here
// or in multi_test.go's TestMultiSearch* dedupe fixtures, which all use a
// single-token query that never leaves MatchUnscored — exercises them
// together with a query long enough to actually score. A regression that
// only shows up once MatchKind/MatchScore are populated on a deduped row
// would pass every existing test.
func TestFinalizeScoresAndDedupesTogether(t *testing.T) {
	const wanted = "Trust Asymmetries in Human-AI Collaboration"
	const shared = "10.1000/shared-trust-paper"
	first := []DiscoveredWork{{Work: work.Work{Title: wanted, DOI: shared}, CitedBy: 5, Source: "openalex"}}
	second := []DiscoveredWork{{Work: work.Work{Title: wanted, DOI: shared}, CitedBy: 900, Source: "semanticscholar"}}
	got := finalize([][]DiscoveredWork{first, second}, SearchParams{Query: wanted, Limit: 10})
	if len(got) != 1 {
		t.Fatalf("finalize kept %d copies of the same DOI, want 1 (results: %+v)", len(got), got)
	}
	if got[0].Source != "openalex" {
		t.Fatalf("kept the copy from %q, want the preferred backend openalex", got[0].Source)
	}
	if got[0].MatchKind != MatchExactTitle || !Confident(got[0].MatchKind) {
		t.Fatalf("deduped row kind = %q, want %q — scoring must still apply to the merged result", got[0].MatchKind, MatchExactTitle)
	}
}

// P2 regression: DiscoveredWork's doc comment promises MatchScore/MatchKind
// are always present, including MatchUnscored as the zero value, but
// discoveredWork used to leave the Go zero value "" on every row Client
// builds — only rank ever set MatchKind, and Client.Search/LookupWork return
// straight to the caller without going through rank or Multi. LookupWork is
// reachable directly from bootstrap, so this was a live gap, not a
// theoretical one.
func TestClientResultsCarryMatchUnscored(t *testing.T) {
	const searchBody = `{"results":[{"id":"https://openalex.org/W1","title":"A resilient discovery paper","publication_year":2024}]}`
	const lookupBody = `{"id":"https://openalex.org/W1","title":"A resilient discovery paper","publication_year":2024}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "doi:") {
			_, _ = w.Write([]byte(lookupBody))
			return
		}
		_, _ = w.Write([]byte(searchBody))
	}))
	defer server.Close()

	client := NewWithOptions(Options{Client: http.DefaultClient, ContactEmail: "researcher@example.org", BaseURL: server.URL})

	searched, err := client.Search(context.Background(), SearchParams{Query: "a resilient paper", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(searched) != 1 || searched[0].MatchKind != MatchUnscored {
		t.Fatalf("Search result = %+v, want a single row with MatchKind %q", searched, MatchUnscored)
	}

	looked, err := client.LookupWork(context.Background(), "10.1000/example")
	if err != nil {
		t.Fatal(err)
	}
	if looked.MatchKind != MatchUnscored {
		t.Fatalf("LookupWork result MatchKind = %q, want %q", looked.MatchKind, MatchUnscored)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
