// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const NativeViewerSaveFeature = "native_viewer_save_v1"

var nativeViewerBadEscapeRE = regexp.MustCompile(`%($|[^0-9A-Fa-f]|[0-9A-Fa-f]($|[^0-9A-Fa-f]))`)

// SourceURL is sensitive, transient document identity. Preserve its exact text,
// including its fragment; never persist it or include it in diagnostics.
type NativeViewerSaveRequestV1Payload struct {
	RequestID      string `json:"request_id"`
	ActionID       int64  `json:"action_id"`
	ActionRevision int64  `json:"action_revision"`
	BrowserEpoch   string `json:"browser_epoch"`
	DocumentID     string `json:"document_id"`
	SourceURL      string `json:"source_url"`
	OperationID    string `json:"operation_id,omitempty"`
	Step           string `json:"step"`
	// Prepare-only selection provenance. Omission has automatic authority.
	Selection string `json:"selection,omitempty"`
}

type NativeViewerSaveResultV1Payload struct {
	RequestID   string `json:"request_id"`
	OperationID string `json:"operation_id,omitempty"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason,omitempty"`
}

func (p *NativeViewerSaveRequestV1Payload) Validate() error {
	if err := validateCorrelationID("native viewer request_id", p.RequestID); err != nil {
		return err
	}
	if !nativePositive(p.ActionID) || !nativePositive(p.ActionRevision) {
		return fmt.Errorf("native viewer action identity must be positive safe integers")
	}
	if !nativeBindingRE.MatchString(p.BrowserEpoch) || !nativeBindingRE.MatchString(p.DocumentID) {
		return fmt.Errorf("native viewer document binding is invalid")
	}
	if err := validateNativeViewerSourceURL(p.SourceURL); err != nil {
		return err
	}
	switch p.Step {
	case "prepare":
		if p.Selection != "" && p.Selection != "automatic" && p.Selection != "explicit" {
			return fmt.Errorf("native viewer selection is invalid")
		}
		if p.OperationID != "" {
			return fmt.Errorf("native viewer prepare must not carry operation_id")
		}
	case "advance", "cancel":
		if p.Selection != "" {
			return fmt.Errorf("native viewer selection is prepare-only")
		}
		if err := validateCorrelationID("native viewer operation_id", p.OperationID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("native viewer step is invalid")
	}
	return nil
}

func validateNativeViewerSourceURL(value string) error {
	invalid := func() error { return fmt.Errorf("native viewer source_url is invalid") }
	if len(value) > 8192 || !utf8.ValidString(value) || strings.ContainsAny(value, " \\") || nativeViewerBadEscapeRE.MatchString(value) {
		return invalid()
	}
	for _, r := range value {
		if r < 32 || (r >= 127 && r <= 159) {
			return invalid()
		}
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return invalid() // net/url errors can contain the sensitive input.
	}
	if strings.HasPrefix(u.Host, "[") && net.ParseIP(u.Hostname()) == nil {
		return invalid()
	}
	if port := u.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return invalid()
		}
	}
	return nil
}

func (p *NativeViewerSaveResultV1Payload) Validate() error {
	if err := validateCorrelationID("native viewer request_id", p.RequestID); err != nil {
		return err
	}
	if err := enumRequired("native viewer outcome", p.Outcome, "prepared", "pending", "ready", "review", "rejected", "refused", "stale", "unavailable"); err != nil {
		return err
	}
	if p.OperationID != "" || p.Outcome == "prepared" || p.Outcome == "pending" {
		if err := validateCorrelationID("native viewer operation_id", p.OperationID); err != nil {
			return err
		}
	}
	if p.Reason != "" {
		return enumRequired("native viewer reason", p.Reason, "unavailable", "authority_lost", "already_started", "expired", "source_rejected", "source_busy", "invalid_pdf", "validation_pending", "native_failed", "document_changed", "unsupported")
	}
	return nil
}

func decodeNativeViewerSave(data []byte, kind string) (any, error) {
	var target interface{ Validate() error }
	var required, optional []string
	switch kind {
	case MsgNativeViewerSaveRequestV1:
		target = &NativeViewerSaveRequestV1Payload{}
		required = []string{"request_id", "action_id", "action_revision", "browser_epoch", "document_id", "source_url", "step"}
		optional = []string{"operation_id", "selection"}
	case MsgNativeViewerSaveResultV1:
		target = &NativeViewerSaveResultV1Payload{}
		required = []string{"request_id", "outcome"}
		optional = []string{"operation_id", "reason"}
	default:
		return nil, fmt.Errorf("unknown native viewer message")
	}
	fields, err := agentDecideObjectFields(data, kind, required, optional...)
	if err != nil {
		return nil, err
	}
	if err := strictDecode(data, target); err != nil {
		return nil, err
	}
	for _, key := range []string{"operation_id", "reason", "selection"} {
		if raw, present := fields[key]; present {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil || value == "" {
				return nil, fmt.Errorf("native viewer %s must be a nonempty string when present", key)
			}
		}
	}
	return target, target.Validate()
}

// JSON object decoding otherwise keeps only the last duplicate. Check every
// nesting level before interpreting this family, including escaped key aliases.
func rejectNativeViewerDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var objects []map[string]bool
	var expectingKey []bool
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("native viewer JSON is invalid")
		}
		last := len(objects) - 1
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				if last >= 0 && objects[last] != nil {
					expectingKey[last] = true
				}
				var keys map[string]bool
				if delimiter == '{' {
					keys = map[string]bool{}
				}
				objects = append(objects, keys)
				expectingKey = append(expectingKey, true)
			case '}', ']':
				objects = objects[:last]
				expectingKey = expectingKey[:last]
			}
		} else if last >= 0 && objects[last] != nil {
			if expectingKey[last] {
				key := token.(string) // The JSON decoder enforces object key types.
				if objects[last][key] {
					return fmt.Errorf("native viewer JSON contains a duplicate object member")
				}
				objects[last][key] = true
			}
			expectingKey[last] = !expectingKey[last]
		}
	}
}
