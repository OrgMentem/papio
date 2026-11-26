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

func TestCanonicalSurnameFirstReadsCompactInitials(t *testing.T) {
	for _, name := range []string{"Smith AB", "Smith A.B.", "Smith J. A.", "Smith, Alan B"} {
		surname, initial, ok := CanonicalSurnameFirst(name)
		want := 'a'
		if name == "Smith J. A." {
			want = 'j'
		}
		if !ok || surname != "smith" || initial != want {
			t.Errorf("CanonicalSurnameFirst(%q) = %q, %q, %v", name, surname, initial, ok)
		}
	}
	if !SameListsSurnameFirst([]string{"Smith AB"}, []string{"Alan Smith"}) {
		t.Fatal("surname-first record did not match full name")
	}
}

func TestCanonicalSurnameFirstKeepsMultiWordSurnames(t *testing.T) {
	for name, want := range map[string]string{"DE WIT J": "de wit", "van der Berg JA": "van der berg", "van der Berg J. A.": "van der berg"} {
		surname, _, ok := CanonicalSurnameFirst(name)
		if !ok || surname != want {
			t.Errorf("CanonicalSurnameFirst(%q) surname = %q, want %q", name, surname, want)
		}
	}
	if !SameListsSurnameFirst([]string{"DE WIT J"}, []string{"de Wit, John"}) {
		t.Fatal("DE WIT J did not match de Wit, John")
	}
}

func TestCanonicalKeepsFullNameReading(t *testing.T) {
	surname, initial, ok := Canonical("Wei LI")
	if !ok || surname != "li" || initial != 'w' {
		t.Fatalf("Canonical(Wei LI) = %q, %q, %v", surname, initial, ok)
	}
	if !Same("Wei LI", "Wei Li") {
		t.Fatal("Wei LI should match Wei Li")
	}
}
