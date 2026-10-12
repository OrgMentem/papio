// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package authormatch compares author names the way resolvers accept a title
// search hit: by surname plus first initial, whatever the name order. It is the
// one place that rule lives, so a change to author canonicalization cannot make
// two resolvers disagree about the same record.
package authormatch

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SameLists reports whether the record's authors are the requested authors, in
// any order. An empty request matches anything; otherwise the lists must have
// the same length and pair off one-to-one under Same.
func SameLists(recordAuthors, requestedAuthors []string) bool {
	if len(requestedAuthors) == 0 {
		return true
	}
	if len(recordAuthors) != len(requestedAuthors) {
		return false
	}
	matched := make([]bool, len(recordAuthors))
	for _, requestedAuthor := range requestedAuthors {
		found := false
		for i, recordAuthor := range recordAuthors {
			if !matched[i] && Same(recordAuthor, requestedAuthor) {
				matched[i], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Same reports whether two names canonicalize to the same surname and first
// initial. A name that cannot be read never matches, not even itself.
func Same(left, right string) bool {
	leftSurname, leftInitial, leftOK := Canonical(left)
	rightSurname, rightInitial, rightOK := Canonical(right)
	return leftOK && rightOK && leftSurname == rightSurname && leftInitial == rightInitial
}

// Canonical returns the lower-cased surname and first-given-name initial of a
// name written as "Smith, John", "Smith J", "J Smith" or "John Smith". A
// mononym returns itself as the surname with a zero initial. ok is false when
// no surname can be read.
func Canonical(value string) (surname string, initial rune, ok bool) {
	value = strings.TrimSpace(value)
	if comma := strings.IndexRune(value, ','); comma >= 0 {
		surname := fold(value[:comma])
		givenNames := strings.Fields(fold(value[comma+1:]))
		if surname == "" || len(givenNames) == 0 {
			return "", 0, false
		}
		initial, ok := firstRune(givenNames[0])
		return surname, initial, ok
	}

	parts := strings.Fields(fold(value))
	switch len(parts) {
	case 0:
		return "", 0, false
	case 1:
		// Mononymous authors ("Madonna") carry no separate initial; match on
		// the bare name so a valid single-name author cannot sink the list.
		return parts[0], 0, true
	}
	if isInitial(parts[len(parts)-1]) {
		initial, ok := firstRune(parts[len(parts)-1])
		return strings.Join(parts[:len(parts)-1], " "), initial, ok
	}
	initial, ok = firstRune(parts[0])
	return parts[len(parts)-1], initial, ok
}

func fold(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func isInitial(value string) bool {
	value = strings.Trim(value, ".")
	_, size := utf8.DecodeRuneInString(value)
	return size > 0 && size == len(value)
}

func firstRune(value string) (rune, bool) {
	for _, r := range value {
		return unicode.ToLower(r), true
	}
	return 0, false
}
