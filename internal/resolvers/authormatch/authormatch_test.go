// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package authormatch

import "testing"

func TestSameCanonicalizesNameOrder(t *testing.T) {
	for _, author := range []string{"Smith J", "J Smith", "John Smith", "Smith, John"} {
		if !Same(author, "John Smith") {
			t.Errorf("Same(%q, %q) = false, want true", author, "John Smith")
		}
	}
}

func TestSameRejectsDistinctUTF8Initials(t *testing.T) {
	if Same("Émile Smith", "Östen Smith") {
		t.Fatal("Same accepted distinct non-ASCII initials")
	}
}

func TestSameMatchesMononyms(t *testing.T) {
	if !Same("Madonna", "Madonna") {
		t.Fatal("Same rejected identical mononyms")
	}
	if Same("Madonna", "Prince") {
		t.Fatal("Same accepted distinct mononyms")
	}
	if Same("Madonna", "John Smith") {
		t.Fatal("Same accepted mononym against full name")
	}
}

func TestSameListsPairsAuthorsInAnyOrder(t *testing.T) {
	cases := []struct {
		name      string
		record    []string
		requested []string
		want      bool
	}{
		{"empty request matches anything", []string{"Smith J"}, nil, true},
		{"different given initial", []string{"Doe A", "Smith J"}, []string{"John Smith", "Jane Doe"}, false},
		{"reordered", []string{"Doe J", "Smith J"}, []string{"John Smith", "Jane Doe"}, true},
		{"length differs", []string{"Smith J"}, []string{"John Smith", "Jane Doe"}, false},
		{"one record author cannot satisfy two requests", []string{"Smith J", "Smith J"}, []string{"John Smith", "Jane Doe"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameLists(tc.record, tc.requested); got != tc.want {
				t.Fatalf("SameLists(%q, %q) = %v, want %v", tc.record, tc.requested, got, tc.want)
			}
		})
	}
}
