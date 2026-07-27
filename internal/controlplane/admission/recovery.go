package admission

import (
	"context"
	"math"

	"github.com/araihu/paje/internal/controlplane/journal"
)

type fenceTransitionSubject struct {
	Recovery RecoveryIdentity `json:"recovery"`
	Proof    FenceProof       `json:"proof"`
}

type scannerApplyTransitionSubject struct {
	Recovery               RecoveryIdentity `json:"recovery"`
	ScannerAuthorityDigest string           `json:"scanner_authority_digest"`
	Fact                   ProviderFact     `json:"fact"`
}

func (s *Service) StartObservation(
	ctx context.Context,
	request RecoveryRequest,
) (RecoveryReceipt, error) {
	if err := validateRecoveryRequest(request); err != nil {
		return RecoveryReceipt{}, err
	}
	record, created, err := s.commitTransition(
		ctx, request.Recovery.ControlRunID, request.Identity, OperationStartObservation,
		"recovery_identity", request.Recovery, "",
		func(p *projection, binding semanticBinding, requestDigest string) (outcomeEnvelope, error) {
			identity := request.Recovery
			if identity.InstallationID != "" && identity.InstallationID != p.installationID {
				return outcomeEnvelope{}, typedError(CodeConflict, OperationStartObservation, ErrConflict)
			}
			identity.InstallationID = p.installationID
			if _, exists := p.recoveries[recoveryIndex(identity)]; exists {
				return outcomeEnvelope{}, typedError(CodeConflict, OperationStartObservation, ErrConflict)
			}
			if latest, exists := latestRecovery(p, identity); exists {
				if latest.Identity.SubjectDigest != identity.SubjectDigest ||
					latest.Identity.Generation == math.MaxUint64 ||
					identity.Generation != latest.Identity.Generation+1 ||
					(latest.State != RecoveryCanceled && latest.State != RecoveryNotPerformed) {
					return outcomeEnvelope{}, typedError(CodeConflict, OperationStartObservation, ErrConflict)
				}
			}
			return outcomeEnvelope{Recovery: &RecoveryRecord{
				Identity: identity, State: RecoveryObservationStarted,
				ReceiptID: opaqueReceiptID(OperationStartObservation, binding, requestDigest),
			}}, nil
		},
	)
	if err != nil {
		return RecoveryReceipt{}, err
	}
	return recoveryReceipt(record, created), nil
}

// Observe is deliberately effect-free. It confirms that StartObservation is
// authoritative, invokes only the typed observer boundary, and returns a
// non-authoritative fact for later scanner-owned application.
func (s *Service) Observe(ctx context.Context, identity RecoveryIdentity) (ObservationReceipt, error) {
	if err := identity.validate(); err != nil {
		return ObservationReceipt{}, err
	}
	p, err := s.rebuild(ctx)
	if err != nil {
		return ObservationReceipt{}, err
	}
	identity, err = normalizeRecoveryIdentity(identity, p.installationID)
	if err != nil {
		return ObservationReceipt{}, err
	}
	record, ok := p.recoveries[recoveryIndex(identity)]
	if !ok {
		return ObservationReceipt{}, typedError(CodeNotFound, OperationObserveEffect, ErrNotFound)
	}
	if record.State == RecoveryCanceled || record.State == RecoveryNotPerformed || record.State == RecoveryFenced {
		return ObservationReceipt{}, typedError(CodeFenced, OperationObserveEffect, ErrFenced)
	}
	if latest, exists := latestRecovery(p, identity); exists && latest.Identity.Generation > identity.Generation {
		return ObservationReceipt{}, typedError(CodeFenced, OperationObserveEffect, ErrFenced)
	}
	if s.observer == nil {
		return ObservationReceipt{}, typedError(CodeInvalidRequest, OperationObserveEffect, ErrInvalidRecord)
	}
	fact, observeErr := s.observer.Observe(ctx, identity)
	if observeErr != nil {
		if err := ctx.Err(); err != nil {
			return ObservationReceipt{}, err
		}
		return ObservationReceipt{}, typedError(CodeStore, OperationObserveEffect, ErrAmbiguous)
	}
	if err := validateProviderFact(fact, identity.SubjectDigest); err != nil {
		return ObservationReceipt{}, err
	}
	return ObservationReceipt{Authoritative: false, Recovery: identity, Fact: fact}, nil
}

func (s *Service) CancelOrFence(ctx context.Context, request FenceRequest) (RecoveryReceipt, error) {
	if err := request.Identity.validate(); err != nil {
		return RecoveryReceipt{}, err
	}
	if err := request.Recovery.validate(); err != nil {
		return RecoveryReceipt{}, err
	}
	if !boundedRequired(request.Proof.ReceiptID, 128) {
		return RecoveryReceipt{}, typedError(CodeInvalidRequest, OperationCancelOrFence, ErrInvalidRecord)
	}
	state := RecoveryState("")
	switch request.Proof.Status {
	case ProviderCanceled:
		state = RecoveryCanceled
	case ProviderNotPerformed:
		state = RecoveryNotPerformed
	case ProviderFenced:
		state = RecoveryFenced
	default:
		return RecoveryReceipt{}, typedError(CodeInvalidRequest, OperationCancelOrFence, ErrInvalidRecord)
	}
	record, created, err := s.commitTransition(
		ctx, request.Recovery.ControlRunID, request.Identity, OperationCancelOrFence,
		"recovery_fence", fenceTransitionSubject{Recovery: request.Recovery, Proof: request.Proof}, "",
		func(p *projection, binding semanticBinding, requestDigest string) (outcomeEnvelope, error) {
			identity, err := normalizeRecoveryIdentity(request.Recovery, p.installationID)
			if err != nil {
				return outcomeEnvelope{}, err
			}
			prior, ok := p.recoveries[recoveryIndex(identity)]
			if !ok {
				return outcomeEnvelope{}, typedError(CodeNotFound, OperationCancelOrFence, ErrNotFound)
			}
			if latest, exists := latestRecovery(p, identity); exists && latest.Identity.Generation > identity.Generation {
				return outcomeEnvelope{}, typedError(CodeFenced, OperationCancelOrFence, ErrFenced)
			}
			if prior.State != RecoveryObservationStarted && prior.State != RecoveryEffectObserved {
				return outcomeEnvelope{}, typedError(CodeConflict, OperationCancelOrFence, ErrConflict)
			}
			fact := ProviderFact{
				Status: request.Proof.Status, ReceiptID: request.Proof.ReceiptID,
				SubjectDigest: identity.SubjectDigest,
			}
			return outcomeEnvelope{Recovery: &RecoveryRecord{
				Identity: identity, State: state,
				ReceiptID: opaqueReceiptID(OperationCancelOrFence, binding, requestDigest), Fact: fact,
			}}, nil
		},
	)
	if err != nil {
		return RecoveryReceipt{}, err
	}
	return recoveryReceipt(record, created), nil
}

func (s *Service) ScannerApply(
	ctx context.Context,
	request ScannerApplyRequest,
) (RecoveryReceipt, error) {
	if s.scannerAuthority == "" || request.ScannerAuthority != s.scannerAuthority {
		return RecoveryReceipt{}, typedError(CodeUnauthorized, OperationScannerApply, ErrUnauthorized)
	}
	if err := request.Identity.validate(); err != nil {
		return RecoveryReceipt{}, err
	}
	if err := request.Recovery.validate(); err != nil {
		return RecoveryReceipt{}, err
	}
	if err := validateProviderFact(request.Fact, request.Recovery.SubjectDigest); err != nil {
		return RecoveryReceipt{}, err
	}
	if request.Fact.Status == ProviderAmbiguous {
		return RecoveryReceipt{}, typedError(CodeAmbiguous, OperationScannerApply, ErrAmbiguous)
	}
	record, created, err := s.commitTransition(
		ctx, request.Recovery.ControlRunID, request.Identity, OperationScannerApply,
		"recovery_scanner_apply", scannerApplyTransitionSubject{
			Recovery:               request.Recovery,
			ScannerAuthorityDigest: stableID("scanner_authority", request.ScannerAuthority),
			Fact:                   request.Fact,
		}, "",
		func(p *projection, binding semanticBinding, requestDigest string) (outcomeEnvelope, error) {
			identity, err := normalizeRecoveryIdentity(request.Recovery, p.installationID)
			if err != nil {
				return outcomeEnvelope{}, err
			}
			prior, ok := p.recoveries[recoveryIndex(identity)]
			if !ok {
				return outcomeEnvelope{}, typedError(CodeNotFound, OperationScannerApply, ErrNotFound)
			}
			if latest, exists := latestRecovery(p, identity); exists && latest.Identity.Generation > identity.Generation {
				return outcomeEnvelope{}, typedError(CodeFenced, OperationScannerApply, ErrFenced)
			}
			if prior.State == RecoveryCanceled || prior.State == RecoveryNotPerformed || prior.State == RecoveryFenced {
				return outcomeEnvelope{}, typedError(CodeFenced, OperationScannerApply, ErrFenced)
			}
			if prior.State != RecoveryObservationStarted && prior.State != RecoveryEffectObserved {
				return outcomeEnvelope{}, typedError(CodeConflict, OperationScannerApply, ErrConflict)
			}
			state := RecoveryApplied
			switch request.Fact.Status {
			case ProviderNotPerformed:
				state = RecoveryNotPerformed
			case ProviderCanceled:
				state = RecoveryCanceled
			case ProviderFenced:
				state = RecoveryFenced
			case ProviderEffectObserved:
				state = RecoveryApplied
			default:
				return outcomeEnvelope{}, typedError(CodeAmbiguous, OperationScannerApply, ErrAmbiguous)
			}
			return outcomeEnvelope{Recovery: &RecoveryRecord{
				Identity: identity, State: state,
				ReceiptID: opaqueReceiptID(OperationScannerApply, binding, requestDigest), Fact: request.Fact,
			}}, nil
		},
	)
	if err != nil {
		return RecoveryReceipt{}, err
	}
	return recoveryReceipt(record, created), nil
}

func validateRecoveryRequest(request RecoveryRequest) error {
	if err := request.Identity.validate(); err != nil {
		return err
	}
	return request.Recovery.validate()
}

func normalizeRecoveryIdentity(identity RecoveryIdentity, installationID string) (RecoveryIdentity, error) {
	if identity.InstallationID != "" && identity.InstallationID != installationID {
		return RecoveryIdentity{}, typedError(CodeConflict, OperationScannerApply, ErrConflict)
	}
	identity.InstallationID = installationID
	return identity, nil
}

func latestRecovery(p *projection, identity RecoveryIdentity) (RecoveryRecord, bool) {
	var latest RecoveryRecord
	found := false
	for _, candidate := range p.recoveries {
		if candidate.Identity.InstallationID != identity.InstallationID ||
			candidate.Identity.ControlRunID != identity.ControlRunID ||
			candidate.Identity.ActionID != identity.ActionID {
			continue
		}
		if !found || candidate.Identity.Generation > latest.Identity.Generation {
			latest, found = candidate, true
		}
	}
	return latest, found
}

func validateProviderFact(fact ProviderFact, wantDigest string) error {
	if fact.SubjectDigest != wantDigest || !journal.ValidDigest(fact.SubjectDigest) || len(fact.ReceiptID) > 128 {
		return typedError(CodeInvalidRequest, OperationObserveEffect, ErrInvalidRecord)
	}
	switch fact.Status {
	case ProviderEffectObserved, ProviderNotPerformed, ProviderCanceled, ProviderFenced:
		if !boundedRequired(fact.ReceiptID, 128) {
			return typedError(CodeInvalidRequest, OperationObserveEffect, ErrInvalidRecord)
		}
	case ProviderAmbiguous:
		if fact.ReceiptID != "" {
			return typedError(CodeInvalidRequest, OperationObserveEffect, ErrInvalidRecord)
		}
	default:
		return typedError(CodeInvalidRequest, OperationObserveEffect, ErrInvalidRecord)
	}
	return nil
}

func recoveryReceipt(record transitionRecord, created bool) RecoveryReceipt {
	receipt := RecoveryReceipt{
		Commit: record.receipt, Operation: record.outcome.SemanticOperation, Created: created,
	}
	receipt.Commit.Created = created
	if record.outcome.Recovery != nil {
		receipt.Recovery = *record.outcome.Recovery
		receipt.RetryAllowed = receipt.Recovery.State == RecoveryNotPerformed ||
			receipt.Recovery.State == RecoveryCanceled
	}
	return receipt
}
