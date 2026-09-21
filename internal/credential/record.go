// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package credential stores typed integration credentials separately from
// serializable configuration. It never falls back to a plaintext file.
package credential

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind identifies the integration and the shape of its credential.
type Kind string

const (
	KindTypeSafe        Kind = "typesafe"
	KindOpenAlex        Kind = "openalex"
	KindSemanticScholar Kind = "semanticscholar"
	KindCORE            Kind = "core"
	KindCrossrefTDM     Kind = "crossref_tdm"
	KindOpenAIREClient  Kind = "openaire_client"
	KindOpenAIREToken   Kind = "openaire_token"
	KindILLiad          Kind = "illiad"
	KindWebhook         Kind = "webhook"

	// MaxEncodedBytes fits Windows Credential Manager's blob limit and the
	// macOS library's base64-encoded security command, including its arguments.
	MaxEncodedBytes = 2048
)

var (
	ErrNotFound         = errors.New("credential not found")
	ErrInvalidRecord    = errors.New("credential record is invalid")
	ErrInvalidReference = errors.New("credential reference is invalid")
	ErrUnavailable      = errors.New("OS credential store is unavailable")
	ErrBusy             = errors.New("OS credential store operation is still in progress")
	// ErrUncertain means a dispatched mutation may still complete. Callers
	// must not report that the stored credential was left unchanged.
	ErrUncertain = errors.New("OS credential store operation outcome is uncertain")
)

// Record holds one integration's credential. It is comparable for exact
// readback verification. Formatting and ordinary JSON encoding redact it;
// Encode is the explicit, secret-bearing persistence operation.
type Record struct {
	Kind         Kind
	APIKey       string
	ClientID     string
	ClientSecret string
	URL          string
	Bearer       string
}

func (Record) String() string   { return "credential.Record{redacted}" }
func (Record) GoString() string { return "credential.Record{redacted}" }
func (Record) MarshalJSON() ([]byte, error) {
	return []byte(`{"redacted":true}`), nil
}

// Validate checks the fields for the declared kind without normalizing bytes.
// The total encoded-size limit is checked separately by Encode and Decode.
func (r Record) Validate() error {
	switch {
	case apiKeyKind(r.Kind):
		if r.ClientID != "" || r.ClientSecret != "" || r.URL != "" || r.Bearer != "" || !printable(r.APIKey) {
			return ErrInvalidRecord
		}
		if r.Kind == KindTypeSafe {
			if len(r.APIKey) > 1024 {
				return ErrInvalidRecord
			}
			for i := range len(r.APIKey) {
				if r.APIKey[i] < 0x21 || r.APIKey[i] > 0x7e {
					return ErrInvalidRecord
				}
			}
		}
	case r.Kind == KindOpenAIREClient:
		if r.APIKey != "" || r.URL != "" || r.Bearer != "" || !printable(r.ClientID) || !printable(r.ClientSecret) {
			return ErrInvalidRecord
		}
	case r.Kind == KindWebhook:
		if r.APIKey != "" || r.ClientID != "" || r.ClientSecret != "" || !validWebhookURL(r.URL) || (r.Bearer != "" && !printable(r.Bearer)) {
			return ErrInvalidRecord
		}
	default:
		return ErrInvalidRecord
	}
	return nil
}

func apiKeyKind(kind Kind) bool {
	switch kind {
	case KindTypeSafe, KindOpenAlex, KindSemanticScholar, KindCORE, KindCrossrefTDM, KindOpenAIREToken, KindILLiad:
		return true
	default:
		return false
	}
}

func printable(value string) bool {
	if len(value) == 0 || len(value) > MaxEncodedBytes || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func validWebhookURL(value string) bool {
	if !printable(value) || strings.ContainsFunc(value, unicode.IsSpace) {
		return false
	}
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil
}

// wireRecord is deliberately separate from Record's redacting serializers.
type wireRecord struct {
	Version      int    `json:"version"`
	Kind         Kind   `json:"kind"`
	APIKey       string `json:"api_key,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	URL          string `json:"url,omitempty"`
	Bearer       string `json:"bearer,omitempty"`
}

// Encode returns a secret-bearing version-1 JSON record. Do not log its result.
func Encode(r Record) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	w := wireRecord{1, r.Kind, r.APIKey, r.ClientID, r.ClientSecret, r.URL, r.Bearer}
	encoded, err := json.Marshal(w)
	if err != nil || len(encoded) > MaxEncodedBytes {
		return "", ErrInvalidRecord
	}
	return string(encoded), nil
}

// Decode rejects unknown, duplicate, wrongly cased and kind-inappropriate
// fields, nulls, unsupported versions, trailing content and oversized records.
// Errors are fixed vocabulary and never include record contents.
func Decode(encoded string) (Record, error) {
	if len(encoded) == 0 || len(encoded) > MaxEncodedBytes || !utf8.ValidString(encoded) || !validUnicodeEscapes(encoded) {
		return Record{}, ErrInvalidRecord
	}
	d := json.NewDecoder(strings.NewReader(encoded))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return Record{}, ErrInvalidRecord
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return Record{}, ErrInvalidRecord
		}
		if _, exists := fields[name]; exists {
			return Record{}, ErrInvalidRecord
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil || string(value) == "null" {
			return Record{}, ErrInvalidRecord
		}
		fields[name] = value
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return Record{}, ErrInvalidRecord
	}
	if _, err := d.Token(); err != io.EOF {
		return Record{}, ErrInvalidRecord
	}
	var version int
	var r Record
	if json.Unmarshal(fields["version"], &version) != nil || version != 1 || json.Unmarshal(fields["kind"], &r.Kind) != nil {
		return Record{}, ErrInvalidRecord
	}
	for name, value := range fields {
		var dst *string
		switch name {
		case "version", "kind":
			continue
		case "api_key":
			if apiKeyKind(r.Kind) {
				dst = &r.APIKey
			}
		case "client_id":
			if r.Kind == KindOpenAIREClient {
				dst = &r.ClientID
			}
		case "client_secret":
			if r.Kind == KindOpenAIREClient {
				dst = &r.ClientSecret
			}
		case "url":
			if r.Kind == KindWebhook {
				dst = &r.URL
			}
		case "bearer":
			if r.Kind == KindWebhook {
				dst = &r.Bearer
			}
		}
		if dst == nil || json.Unmarshal(value, dst) != nil {
			return Record{}, ErrInvalidRecord
		}
	}
	if err := r.Validate(); err != nil {
		return Record{}, err
	}
	return r, nil
}

// encoding/json replaces unpaired UTF-16 escapes with U+FFFD instead of
// rejecting them. A credential decoder must not silently change secret bytes.
// Ordinary JSON syntax is checked by the decoder; here we pair only escapes.
func validUnicodeEscapes(encoded string) bool {
	for i := 0; i < len(encoded); i++ {
		if encoded[i] != '\\' {
			continue
		}
		i++
		if i >= len(encoded) || encoded[i] != 'u' {
			continue
		}
		if i+4 >= len(encoded) {
			return false
		}
		code, err := strconv.ParseUint(encoded[i+1:i+5], 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(encoded) || encoded[i+1:i+3] != `\u` {
				return false
			}
			low, err := strconv.ParseUint(encoded[i+3:i+7], 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

// DecodeEnvironment accepts an unchanged raw API key only when exactly one
// API-key kind is allowed. Structured or ambiguous bindings require the same
// version-1 JSON accepted by Decode, including an explicit allowed kind.
func DecodeEnvironment(value string, allowed []Kind) (Record, error) {
	var r Record
	var err error
	if len(allowed) == 1 && apiKeyKind(allowed[0]) {
		r = Record{Kind: allowed[0], APIKey: value}
		_, err = Encode(r)
	} else {
		r, err = Decode(value)
	}
	if err != nil || !allows(allowed, r.Kind) {
		return Record{}, ErrInvalidRecord
	}
	return r, nil
}

func allows(kinds []Kind, kind Kind) bool {
	for _, candidate := range kinds {
		if candidate == kind {
			return true
		}
	}
	return false
}
