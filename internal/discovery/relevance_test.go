// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package discovery

import (
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
			name:      "unrelated title is weak even when it shares a word",
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
