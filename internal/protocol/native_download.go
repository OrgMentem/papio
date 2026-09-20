// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package protocol

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const NativeClickAdoptionFeature = "native_click_adoption_v1"

// Native download paths are transient browser observations, never authority.
// The daemon confines and opens them only under its own pinned source root.
type NativeDownloadArmRequestV1Payload struct {
	RequestID    string                  `json:"request_id"`
	Producer     ArtifactProducerPayload `json:"producer"`
	BrowserEpoch string                  `json:"browser_epoch"`
	DocumentID   string                  `json:"document_id"`
}
type NativeDownloadArmResultV1Payload struct {
	RequestID     string `json:"request_id"`
	Outcome       string `json:"outcome"`
	ReservationID string `json:"reservation_id,omitempty"`
	ExpiresAtMS   int64  `json:"expires_at_ms,omitempty"`
	Reason        string `json:"reason,omitempty"`
}
type NativeDownloadImportRequestV1Payload struct {
	RequestID     string                  `json:"request_id"`
	ReservationID string                  `json:"reservation_id"`
	Producer      ArtifactProducerPayload `json:"producer"`
	BrowserEpoch  string                  `json:"browser_epoch"`
	DocumentID    string                  `json:"document_id"`
	DownloadID    int64                   `json:"download_id"`
	StartedAtMS   int64                   `json:"started_at_ms"`
	SourcePath    string                  `json:"source_path"`
	SizeBytes     int64                   `json:"size_bytes"`
}
type NativeDownloadImportResultV1Payload struct {
	RequestID     string `json:"request_id"`
	ReservationID string `json:"reservation_id"`
	DownloadID    int64  `json:"download_id"`
	Outcome       string `json:"outcome"`
	Reason        string `json:"reason,omitempty"`
}

var nativeBindingRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var nativeAbsolutePathRE = regexp.MustCompile(`^(?:/|[A-Za-z]:[\\/])`)

func nativeDownloadReason(reason string) error {
	if reason == "" {
		return nil
	}
	return enumRequired("native download reason", reason, "unavailable", "authority_lost", "already_armed", "expired", "source_rejected", "source_busy", "invalid_pdf", "validation_pending")
}
func nativePositive(n int64) bool { return n > 0 && n <= MaxBrowserInteger }
func validateNativeProducer(p ArtifactProducerPayload) error {
	if p.EffectKind != "generic_drive" || p.Strategy != "generic" || p.Ordinal == nil || p.ClaimID != "" || p.BindingID != "" || p.EffectOrdinal != nil || p.InstitutionalRequestID != "" {
		return fmt.Errorf("native download requires a complete generic producer")
	}
	return validateDriveEpochTuple(p.DriveAttemptID, *p.Ordinal, p.Strategy, p.Revision, "native download producer")
}
func validateNativeBinding(requestID, epoch, document string, p ArtifactProducerPayload) error {
	if err := validateCorrelationID("native download request_id", requestID); err != nil {
		return err
	}
	if !nativeBindingRE.MatchString(epoch) || !nativeBindingRE.MatchString(document) {
		return fmt.Errorf("native download binding is invalid")
	}
	return validateNativeProducer(p)
}
func (p *NativeDownloadArmRequestV1Payload) Validate() error {
	return validateNativeBinding(p.RequestID, p.BrowserEpoch, p.DocumentID, p.Producer)
}
func (p *NativeDownloadImportRequestV1Payload) Validate() error {
	if err := validateNativeBinding(p.RequestID, p.BrowserEpoch, p.DocumentID, p.Producer); err != nil {
		return err
	}
	if err := validateCorrelationID("native download reservation_id", p.ReservationID); err != nil {
		return err
	}
	if !nativePositive(p.DownloadID) || !nativePositive(p.StartedAtMS) || !nativePositive(p.SizeBytes) {
		return fmt.Errorf("native download integer is out of bounds")
	}
	if len(p.SourcePath) > 4096 || !nativeAbsolutePathRE.MatchString(p.SourcePath) || strings.ContainsAny(p.SourcePath, "\x00\r\n") {
		return fmt.Errorf("native download source_path is invalid")
	}
	for _, r := range p.SourcePath {
		if r < 32 || r == 127 {
			return fmt.Errorf("native download source_path contains controls")
		}
	}
	return nil
}
func (p *NativeDownloadArmResultV1Payload) Validate() error {
	if err := validateCorrelationID("native download request_id", p.RequestID); err != nil {
		return err
	}
	if err := enumRequired("native download outcome", p.Outcome, "armed", "refused", "stale", "unavailable"); err != nil {
		return err
	}
	if p.Outcome == "armed" {
		if err := validateCorrelationID("native download reservation_id", p.ReservationID); err != nil {
			return err
		}
		if !nativePositive(p.ExpiresAtMS) {
			return fmt.Errorf("native download expiry is invalid")
		}
	} else if p.ReservationID != "" || p.ExpiresAtMS != 0 {
		return fmt.Errorf("native download refusal must not grant a reservation")
	}
	return nativeDownloadReason(p.Reason)
}
func (p *NativeDownloadImportResultV1Payload) Validate() error {
	if err := validateCorrelationID("native download request_id", p.RequestID); err != nil {
		return err
	}
	if err := validateCorrelationID("native download reservation_id", p.ReservationID); err != nil {
		return err
	}
	if !nativePositive(p.DownloadID) {
		return fmt.Errorf("native download id is invalid")
	}
	if err := enumRequired("native download outcome", p.Outcome, "ready", "review", "rejected", "deferred", "stale", "refused"); err != nil {
		return err
	}
	return nativeDownloadReason(p.Reason)
}

func decodeNativeDownload(data []byte, kind string) (any, error) {
	var target interface{ Validate() error }
	var required, optional []string
	switch kind {
	case MsgNativeDownloadArmRequestV1:
		target = &NativeDownloadArmRequestV1Payload{}
		required = []string{"request_id", "producer", "browser_epoch", "document_id"}
	case MsgNativeDownloadImportRequestV1:
		target = &NativeDownloadImportRequestV1Payload{}
		required = []string{"request_id", "reservation_id", "producer", "browser_epoch", "document_id", "download_id", "started_at_ms", "source_path", "size_bytes"}
	case MsgNativeDownloadArmResultV1:
		target = &NativeDownloadArmResultV1Payload{}
		required = []string{"request_id", "outcome"}
		optional = []string{"reservation_id", "expires_at_ms", "reason"}
	case MsgNativeDownloadImportResultV1:
		target = &NativeDownloadImportResultV1Payload{}
		required = []string{"request_id", "reservation_id", "download_id", "outcome"}
		optional = []string{"reason"}
	default:
		return nil, fmt.Errorf("unknown native download message")
	}
	fields, err := agentDecideObjectFields(data, kind, required, optional...)
	if err != nil {
		return nil, err
	}
	if raw, ok := fields["producer"]; ok {
		if _, err := agentDecideObjectFields(raw, kind+".producer", []string{"effect_kind", "drive_attempt_id", "ordinal", "strategy", "revision"}); err != nil {
			return nil, err
		}
	}
	if err := strictDecode(data, target); err != nil {
		return nil, err
	}
	if p, ok := target.(*NativeDownloadArmResultV1Payload); ok && p.Outcome != "armed" {
		if _, ok := fields["reservation_id"]; ok {
			return nil, fmt.Errorf("refused native arm carries reservation")
		}
		if _, ok := fields["expires_at_ms"]; ok {
			return nil, fmt.Errorf("refused native arm carries expiry")
		}
	}
	if raw, ok := fields["reason"]; ok {
		var reason string
		if err := json.Unmarshal(raw, &reason); err != nil || reason == "" {
			return nil, fmt.Errorf("native download reason is empty or invalid")
		}
	}
	return target, target.Validate()
}
