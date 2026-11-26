package bibparse

import "testing"

func TestBibTeXYearShortValues(t *testing.T) {
	for _, v := range []string{"", "1", "12", "123", "x", "2024", "c. 1999"} {
		_ = bibTeXYear(v)
	}
	if bibTeXYear("c. 1999") != 1999 {
		t.Fatal("year not found")
	}
}
