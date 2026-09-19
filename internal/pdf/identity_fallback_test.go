// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package pdf

import (
	"testing"

	"papio/internal/work"
)

func TestIdentityFallbackParksSurnameYearCitationBeforeWrappedTitlePhrase(t *testing.T) {
	target := work.Work{
		Title:   "The calibration incident method",
		Authors: []string{"John C. Example"},
		Year:    1954,
	}
	text := "Fifty years of the calibration incident\n" +
		"method: 1954–2004 and beyond\n" +
		"Mira Reviewer and Owen Analyst\n\n" +
		"It has now been fifty years since Example (1954) wrote a classic article on\n" +
		"the calibration incident method (CIM). During the intervening years, the method changed.\n"

	got := MatchIdentity(text, target)
	if got.Result != IdentityReview {
		t.Fatalf("result = %+v, want review: a surname-year citation must not establish authorship", got)
	}
}

func TestIdentityFallbackAcceptsShortTitleFollowedByItsAuthorAndYear(t *testing.T) {
	target := work.Work{
		Title:   "The calibration incident method",
		Authors: []string{"John C. Example"},
		Year:    1954,
	}
	text := "The calibration incident\nmethod. John C. Example (1954).\n"

	if got := MatchIdentity(text, target); got.Result != IdentityPass {
		t.Fatalf("result = %+v, want pass: the author-year byline follows the punctuated title", got)
	}
}
