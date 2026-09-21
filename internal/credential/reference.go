// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package credential

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// NewReference creates a portable, non-secret record identifier independently
// of any configuration path, data directory or integration name.
func NewReference() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", ErrUnavailable
	}
	return "keyring:" + hex.EncodeToString(id[:]), nil
}

// ValidateReference accepts generated keyring identifiers or explicit
// environment names. The environment variable name is at most 128 ASCII bytes.
func ValidateReference(ref string) error {
	if keyringReference(ref) {
		return nil
	}
	if _, ok := EnvironmentName(ref); ok {
		return nil
	}
	return ErrInvalidReference
}

func keyringReference(ref string) bool {
	if len(ref) != len("keyring:")+32 || !strings.HasPrefix(ref, "keyring:") {
		return false
	}
	for _, c := range ref[len("keyring:"):] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// EnvironmentName returns the variable name only for a valid env reference.
func EnvironmentName(ref string) (string, bool) {
	if !strings.HasPrefix(ref, "env:") {
		return "", false
	}
	name := ref[len("env:"):]
	if len(name) == 0 || len(name) > 128 {
		return "", false
	}
	for i, c := range name {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' || (i > 0 && c >= '0' && c <= '9')) {
			return "", false
		}
	}
	return name, true
}
