// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package adaptercontrol

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
)

// embeddedPublicKey is the ONE place the papio control public key lives: the
// standard-base64 encoding of the 32-byte Ed25519 public key that
// `papio-control keygen` prints. With a key embedded, the daemon always
// verifies and applies a persisted signed document. Fetching new documents
// is separate: it also needs updates.check (default off).
// The matching private key never enters this repository.
//
// Empty means this build trusts no control key. Such a build neither fetches
// the document nor accepts one, and `papio doctor` says so. It is a var, not a
// const, so a release build may stamp it with
// -ldflags "-X papio/internal/adaptercontrol.embeddedPublicKey=<base64>".
var embeddedPublicKey = "8KD9MFWfxPmoYctO2fgNu8Mchc77RM1vBAU5uxDcF+w="

// TrustedKey returns the embedded control public key, or nil when this build
// embeds none. A malformed embedded value is reported, never silently ignored.
func TrustedKey() (ed25519.PublicKey, error) {
	if embeddedPublicKey == "" {
		return nil, nil
	}
	key, err := ParsePublicKey(embeddedPublicKey)
	if err != nil {
		return nil, fmt.Errorf("embedded adapter control public key: %w", err)
	}
	return key, nil
}

// ParsePublicKey decodes a standard-base64 Ed25519 public key.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("not base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("want %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}
