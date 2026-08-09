// Copyright 2026 OrgMentem. Licensed under MIT.
package delivery

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"papio/internal/illiad"
)

// ReconciliationDisposition is the closed result vocabulary for an ambiguous
// submission. NOT_FOUND_YET never permits a second provider submission.
type ReconciliationDisposition string

const (
	ReconciliationAdopted     ReconciliationDisposition = "ADOPTED"
	ReconciliationNotFoundYet ReconciliationDisposition = "NOT_FOUND_YET"
	ReconciliationNeedsHuman  ReconciliationDisposition = "NEEDS_HUMAN"
)

const (
	ReconciliationReasonUnavailable          = "reconciliation_unavailable"
	ReconciliationReasonGateBinding          = "submission_binding_mismatch"
	ReconciliationReasonReferenceField       = "reference_field_unconfigured_or_changed"
	ReconciliationReasonPatronMapping        = "patron_mapping_unavailable"
	ReconciliationReasonReadFailed           = "reconciliation_read_failed"
	ReconciliationReasonTokenAmbiguous       = "multiple_token_matches"
	ReconciliationReasonTokenMissing         = "token_missing"
	ReconciliationReasonIdentityMismatch     = "strong_identity_mismatch"
	ReconciliationReasonStateUnrepresentable = "provider_state_unrepresentable"
	ReconciliationReasonCommitConflict       = "submission_commit_conflict"
)

// ReconciliationIdentity carries strong identity captured at submission time.
// Title and author are corroboration only and can never authorize adoption.
type ReconciliationIdentity struct {
	DOI          string
	PMID         string
	RequestClass string
	Title        string
	Author       string
}

// ReconciliationDeps contains facts that must still hold before adoption.
// ReferenceField is caller-supplied because it is an institution binding; this
// package never guesses an ItemInfo field.
type ReconciliationDeps struct {
	Client         TransactionLookup
	PatronRef      string
	ReferenceField string
	Identity       ReconciliationIdentity
	GateAction     Action
	CurrentBinding string
}

type ReconciliationResult struct {
	Disposition       ReconciliationDisposition
	ProviderReference string
	Reason            string
}

// ReconciliationAvailable reports whether the provider's declared adapter
// capabilities include both the patron list and reconciliation contract.
func ReconciliationAvailable(provider string) bool {
	caps, ok := providerCapabilities[provider]
	return ok && caps.canListPatron && caps.canReconcile
}

// Reconcile performs deterministic, read-only reconciliation. Provider calls
// are limited to GET transaction/list operations; local adoption uses the
// existing RecordSubmission CAS and no alternate commit path.
func (s *Service) Reconcile(ctx context.Context, req *Request, deps ReconciliationDeps) (ReconciliationResult, error) {
	if req == nil || !ReconciliationAvailable(req.Provider) || deps.Client == nil {
		return needsHuman(ReconciliationReasonUnavailable), nil
	}
	if deps.GateAction != ActionSubmit || deps.CurrentBinding == "" || deps.CurrentBinding != req.GateProfileDigest {
		return needsHuman(ReconciliationReasonGateBinding), nil
	}
	if !validReferenceField(deps.ReferenceField) {
		return needsHuman(ReconciliationReasonReferenceField), nil
	}
	if strings.TrimSpace(deps.PatronRef) == "" {
		return needsHuman(ReconciliationReasonPatronMapping), nil
	}
	identity := deps.Identity
	if identity.RequestClass == "" {
		identity.RequestClass = req.RequestClass
	}
	if identity.RequestClass == "" || identity.RequestClass != req.RequestClass {
		return needsHuman(ReconciliationReasonIdentityMismatch), nil
	}
	if identity.DOI == "" && identity.PMID == "" {
		switch {
		case strings.HasPrefix(req.WorkIdentity, "doi:"):
			identity.DOI = strings.TrimPrefix(req.WorkIdentity, "doi:")
		case strings.HasPrefix(req.WorkIdentity, "pmid:"):
			identity.PMID = strings.TrimPrefix(req.WorkIdentity, "pmid:")
		}
	}
	if identity.DOI == "" && identity.PMID == "" {
		return needsHuman(ReconciliationReasonIdentityMismatch), nil
	}
	if identity.RequestClass == "" {
		return needsHuman(ReconciliationReasonIdentityMismatch), nil
	}

	var tx *illiad.Transaction
	if req.ProviderReference != "" {
		number, err := strconv.Atoi(req.ProviderReference)
		if err != nil || number <= 0 {
			return needsHuman(ReconciliationReasonReadFailed), nil
		}
		got, err := deps.Client.GetTransaction(ctx, number)
		if err != nil {
			if errors.Is(err, illiad.ErrNotFound) {
				return ReconciliationResult{Disposition: ReconciliationNotFoundYet, Reason: "provider_reference_not_indexed"}, nil
			}
			return needsHuman(ReconciliationReasonReadFailed), nil
		}
		tx = &got
	} else {
		found, reason, err := findReconciledTransaction(ctx, deps.Client, deps.PatronRef, deps.ReferenceField, req, identity)
		if err != nil {
			return needsHuman(ReconciliationReasonReadFailed), nil
		}
		if reason != "" {
			return needsHuman(reason), nil
		}
		if found == nil {
			return ReconciliationResult{Disposition: ReconciliationNotFoundYet, Reason: "token_not_indexed"}, nil
		}
		tx = found
	}
	if reason := validateReconciledTransaction(*tx, deps.ReferenceField, req.IdempotencyKey, identity); reason != "" {
		return needsHuman(reason), nil
	}
	ref := strconv.Itoa(tx.TransactionNumber)
	won, err := s.RecordSubmission(ctx, req.ID, ref, NextCheck(s.now(), 0, 0))
	if err != nil {
		return ReconciliationResult{}, err
	}
	if !won {
		return ReconciliationResult{Disposition: ReconciliationNeedsHuman, ProviderReference: ref, Reason: ReconciliationReasonCommitConflict}, nil
	}
	return ReconciliationResult{Disposition: ReconciliationAdopted, ProviderReference: ref}, nil
}

type reconciliationSemanticError struct{ reason string }

func (e *reconciliationSemanticError) Error() string { return e.reason }

func needsHuman(reason string) ReconciliationResult {
	return ReconciliationResult{Disposition: ReconciliationNeedsHuman, Reason: reason}
}

func classifyReconciliationReadError(err error) error {
	if err == nil {
		return nil
	}
	if _, temporary := illiad.Temporary(err); temporary {
		return err
	}
	return &reconciliationSemanticError{reason: ReconciliationReasonReadFailed}
}

// findReconciledTransaction is the single patron-list matcher shared by the
// ambiguous-submission path and Poll's 404 reconciliation path. It requires
// exactly one exact-token match; a second match or an identity disagreement is
// never silently ignored.
func findReconciledTransaction(ctx context.Context, client TransactionLookup, patronRef, field string, req *Request, identity ReconciliationIdentity) (*illiad.Transaction, string, error) {
	if strings.TrimSpace(patronRef) == "" || !validReferenceField(field) {
		return nil, ReconciliationReasonReferenceField, nil
	}
	txs, err := client.UserRequests(ctx, patronRef)
	if err != nil {
		return nil, "", err
	}
	matches := make([]illiad.Transaction, 0, 1)
	corroborating := false
	for i := range txs {
		ref, ok := txs[i].ReferenceValue(field)
		if ok && ref == req.IdempotencyKey {
			matches = append(matches, txs[i])
			continue
		}
		if titleAuthorCorroborates(txs[i], identity) {
			corroborating = true
		}
	}
	if len(matches) == 0 {
		if corroborating {
			return nil, ReconciliationReasonTokenMissing, nil
		}
		return nil, "", nil
	}
	if len(matches) != 1 {
		return nil, ReconciliationReasonTokenAmbiguous, nil
	}
	if reason := validateReconciledTransaction(matches[0], field, req.IdempotencyKey, identity); reason != "" {
		return nil, reason, nil
	}
	return &matches[0], "", nil
}

func validateReconciledTransaction(tx illiad.Transaction, field, token string, identity ReconciliationIdentity) string {
	if tx.TransactionNumber <= 0 {
		return ReconciliationReasonReadFailed
	}
	ref, ok := tx.ReferenceValue(field)
	if !ok || ref != token {
		return ReconciliationReasonIdentityMismatch
	}
	if identity.DOI != "" && tx.DOI != "" && tx.DOI != identity.DOI {
		return ReconciliationReasonIdentityMismatch
	}
	if identity.PMID != "" && tx.PMID != "" && tx.PMID != identity.PMID {
		return ReconciliationReasonIdentityMismatch
	}
	if identity.RequestClass == "digital_journal_article" && tx.RequestType != "" && tx.RequestType != "Article" {
		return ReconciliationReasonIdentityMismatch
	}
	if identity.Title != "" && tx.PhotoArticleTitle != "" &&
		!strings.EqualFold(strings.TrimSpace(tx.PhotoArticleTitle), strings.TrimSpace(identity.Title)) {
		return ReconciliationReasonIdentityMismatch
	}
	if identity.Author != "" && tx.PhotoArticleAuthor != "" &&
		!strings.EqualFold(strings.TrimSpace(tx.PhotoArticleAuthor), strings.TrimSpace(identity.Author)) {
		return ReconciliationReasonIdentityMismatch
	}
	if strings.TrimSpace(tx.TransactionStatus) == "" {
		return ReconciliationReasonStateUnrepresentable
	}
	return ""
}

func titleAuthorCorroborates(tx illiad.Transaction, identity ReconciliationIdentity) bool {
	if identity.Title == "" || identity.Author == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(tx.PhotoArticleTitle), strings.TrimSpace(identity.Title)) &&
		strings.EqualFold(strings.TrimSpace(tx.PhotoArticleAuthor), strings.TrimSpace(identity.Author))
}

func validReferenceField(field string) bool {
	if strings.TrimSpace(field) == "" {
		return false
	}
	var tx illiad.Transaction
	_, ok := tx.ReferenceValue(field)
	return ok
}
