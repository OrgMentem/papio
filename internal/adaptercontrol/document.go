// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package adaptercontrol verifies and persists the signed, restrictive-only
// adapter control document (ADR-0021 Decision 4).
//
// One online papio control key signs a monotonic-sequence document. The
// document may only suspend exact packaged adapter revision IDs and safety
// domains; a higher sequence replaces the whole set, which is how a suspension
// is lifted. It carries no host, selector, path, method, text, threshold, or
// URL, so a stolen key can only reduce automation.
package adaptercontrol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"time"

	"papio/internal/protocol"
)

const (
	// DocumentSchema names the signed payload.
	DocumentSchema = "papio-adapter-control/1"
	// EnvelopeSchema names the published file that carries payload and signature.
	EnvelopeSchema = "papio-adapter-control-envelope/1"
	// MaxEnvelopeBytes bounds the published file and the persisted copy.
	MaxEnvelopeBytes = 32 << 10
	// signingDomain separates control signatures from every other use of the key.
	signingDomain = "papio-adapter-control/1\x00"
)

var keyIDRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

var (
	// ErrEnvelope marks a published file that is not a well-formed envelope.
	ErrEnvelope = errors.New("adapter control envelope is malformed")
	// ErrSignature marks a payload whose signature does not verify under the
	// embedded key, including every tampered payload.
	ErrSignature = errors.New("adapter control signature does not verify")
	// ErrDocument marks a correctly signed payload that breaks the document rules.
	ErrDocument = errors.New("adapter control document is invalid")
	// ErrNoTrustAnchor marks a build that embeds no control public key.
	ErrNoTrustAnchor = errors.New("this build embeds no adapter control public key")
)

// Envelope is the published file. Payload is the exact signed byte string,
// base64 encoded, so verification covers the bytes before any parsing.
type Envelope struct {
	Schema    string `json:"schema"`
	KeyID     string `json:"key_id"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Document is the signed payload.
type Document struct {
	Schema                 string   `json:"schema"`
	Sequence               int64    `json:"sequence"`
	IssuedAt               string   `json:"issued_at"`
	KeyID                  string   `json:"key_id"`
	SuspendedRevisions     []string `json:"suspended_revisions"`
	SuspendedSafetyDomains []string `json:"suspended_safety_domains"`
}

// Verified is a document whose envelope verified under the trusted key.
type Verified struct {
	Document Document
	IssuedAt time.Time
	// Envelope is the exact published file, persisted verbatim.
	Envelope []byte
	// Payload is the exact signed byte string.
	Payload []byte
	Digest  string
}

// Sign builds a published envelope for doc. It sorts both lists so the digest
// the daemon and the extension compute is canonical, and refuses a document
// the verifier would refuse. It is used by cmd/papio-control and by tests.
func Sign(doc Document, key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("private key has the wrong length")
	}
	doc.Schema = DocumentSchema
	doc.SuspendedRevisions = sortedCopy(doc.SuspendedRevisions)
	doc.SuspendedSafetyDomains = sortedCopy(doc.SuspendedSafetyDomains)
	payload, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if _, err := parsePayload(payload); err != nil {
		return nil, err
	}
	signature := ed25519.Sign(key, append([]byte(signingDomain), payload...))
	envelope, err := json.MarshalIndent(Envelope{
		Schema:    EnvelopeSchema,
		KeyID:     doc.KeyID,
		Payload:   base64.StdEncoding.EncodeToString(payload),
		Signature: base64.StdEncoding.EncodeToString(signature),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(envelope, '\n'), nil
}

// Verify checks the envelope, then the signature over the exact payload bytes,
// and only then parses the payload.
func Verify(envelopeBytes []byte, key ed25519.PublicKey) (*Verified, error) {
	if len(key) != ed25519.PublicKeySize {
		return nil, ErrNoTrustAnchor
	}
	if len(envelopeBytes) > MaxEnvelopeBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrEnvelope, len(envelopeBytes), MaxEnvelopeBytes)
	}
	var envelope Envelope
	if err := decodeStrict(envelopeBytes, &envelope); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEnvelope, err)
	}
	if envelope.Schema != EnvelopeSchema {
		return nil, fmt.Errorf("%w: schema %q, want %q", ErrEnvelope, envelope.Schema, EnvelopeSchema)
	}
	if !keyIDRE.MatchString(envelope.KeyID) {
		return nil, fmt.Errorf("%w: key_id is invalid", ErrEnvelope)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(envelope.Payload)
	if err != nil || len(payload) == 0 {
		return nil, fmt.Errorf("%w: payload is not base64", ErrEnvelope)
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature is not a base64 Ed25519 signature", ErrEnvelope)
	}
	if !ed25519.Verify(key, append([]byte(signingDomain), payload...), signature) {
		return nil, ErrSignature
	}
	verified, err := parsePayload(payload)
	if err != nil {
		return nil, err
	}
	if verified.Document.KeyID != envelope.KeyID {
		return nil, fmt.Errorf("%w: payload key_id differs from the envelope", ErrDocument)
	}
	verified.Envelope = bytes.Clone(envelopeBytes)
	return verified, nil
}

func parsePayload(payload []byte) (*Verified, error) {
	var doc Document
	if err := decodeStrict(payload, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDocument, err)
	}
	if doc.Schema != DocumentSchema {
		return nil, fmt.Errorf("%w: schema %q, want %q", ErrDocument, doc.Schema, DocumentSchema)
	}
	if doc.Sequence < 1 || doc.Sequence > protocol.MaxBrowserInteger {
		return nil, fmt.Errorf("%w: sequence must be between 1 and %d", ErrDocument, protocol.MaxBrowserInteger)
	}
	issuedAt, err := time.Parse(time.RFC3339, doc.IssuedAt)
	if err != nil {
		return nil, fmt.Errorf("%w: issued_at is not RFC 3339", ErrDocument)
	}
	if !keyIDRE.MatchString(doc.KeyID) {
		return nil, fmt.Errorf("%w: key_id is invalid", ErrDocument)
	}
	if doc.SuspendedRevisions == nil || doc.SuspendedSafetyDomains == nil {
		return nil, fmt.Errorf("%w: both suspension lists are required", ErrDocument)
	}
	if err := protocol.ValidateSuspensionSets("document", doc.SuspendedRevisions, doc.SuspendedSafetyDomains); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDocument, err)
	}
	return &Verified{
		Document: doc,
		IssuedAt: issuedAt.UTC(),
		Payload:  bytes.Clone(payload),
		Digest:   protocol.SuspensionsDigest(doc.SuspendedRevisions, doc.SuspendedSafetyDomains),
	}, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

func sortedCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}
