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
	return sameLists(recordAuthors, requestedAuthors, Canonical)
}

// SameListsSurnameFirst is SameLists for a record whose names are surname
// first with compact initials (MEDLINE, Europe PMC authorString). Requested
// names read as Canonical does.
func SameListsSurnameFirst(recordAuthors, requestedAuthors []string) bool {
	return sameLists(recordAuthors, requestedAuthors, CanonicalSurnameFirst)
}

func sameLists(recordAuthors, requestedAuthors []string, recordCanonical func(string) (string, rune, bool)) bool {
	if len(requestedAuthors) == 0 {
		return true
	}
	if len(recordAuthors) != len(requestedAuthors) {
		return false
	}
	matched := make([]bool, len(recordAuthors))
	for _, requestedAuthor := range requestedAuthors {
		requestedSurname, requestedInitial, requestedOK := Canonical(requestedAuthor)
		found := false
		for i, recordAuthor := range recordAuthors {
			if matched[i] || !requestedOK {
				continue
			}
			surname, initial, ok := recordCanonical(recordAuthor)
			if ok && surname == requestedSurname && initial == requestedInitial {
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

func single(token string) bool {
	return utf8.RuneCountInString(strings.ReplaceAll(token, ".", "")) == 1
}

func isInitial(value string) bool {
	value = strings.Trim(value, ".")
	_, size := utf8.DecodeRuneInString(value)
	return size > 0 && size == len(value)
}

// isCompactInitials reports whether a token is a MEDLINE run of given-name
// initials: capitals only, with optional dots ("J", "AB", "A.B.").
func isCompactInitials(token string) bool {
	letters := strings.ReplaceAll(token, ".", "")
	count := utf8.RuneCountInString(letters)
	if count < 1 || count > 3 {
		return false
	}
	for _, r := range letters {
		if !unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

// CanonicalSurnameFirst reads a name known to be written surname first with
// compact initials, the MEDLINE / Europe PMC form: "Smith AB", "Smith A.B.",
// "Smith J. A.", "van der Berg JK". Trailing initials runs are stripped and
// the first one gives the initial. A name without such a run reads as
// Canonical does, so "Smith, John" and mononyms still work. Full-name sources
// must not use it: "Wei LI" there is a given name and an upper-case surname.
func CanonicalSurnameFirst(value string) (surname string, initial rune, ok bool) {
	value = strings.TrimSpace(value)
	if strings.ContainsRune(value, ',') {
		return Canonical(value)
	}
	tokens := strings.Fields(value)
	end := len(tokens)
	// One token of up to three initials ("AB"), or a run of single-letter
	// tokens ("J. A."). A longer capitalised word before the final initial
	// ("DE WIT J") is part of the surname.
	if end > 1 && isCompactInitials(tokens[end-1]) {
		end--
		if single(tokens[end]) {
			for end > 1 && single(tokens[end-1]) {
				end--
			}
		}
	}
	if end == len(tokens) || end == 0 {
		return Canonical(value)
	}
	initial, ok = firstRune(tokens[end])
	return fold(strings.Join(tokens[:end], " ")), initial, ok
}

func firstRune(value string) (rune, bool) {
	for _, r := range value {
		return unicode.ToLower(r), true
	}
	return 0, false
}
