package admission

import "context"

func (s *Service) IssueHandoff(ctx context.Context, request HandoffRequest) (HandoffReceipt, error) {
	return s.handoffTransition(ctx, request, OperationHandoffIssue, HandoffIssued)
}

func (s *Service) GrantHandoff(ctx context.Context, request HandoffRequest) (HandoffReceipt, error) {
	return s.handoffTransition(ctx, request, OperationHandoffGrant, HandoffGranted)
}

func (s *Service) AcknowledgeHandoff(ctx context.Context, request HandoffRequest) (HandoffReceipt, error) {
	return s.handoffTransition(ctx, request, OperationHandoffAcknowledge, HandoffAcknowledged)
}

func (s *Service) handoffTransition(
	ctx context.Context,
	request HandoffRequest,
	operation SemanticOperation,
	state HandoffState,
) (HandoffReceipt, error) {
	if err := validateHandoffRequest(request, state); err != nil {
		return HandoffReceipt{}, err
	}
	subjectDigest, _, err := digestValue(request.Subject)
	if err != nil {
		return HandoffReceipt{}, err
	}
	handoffID := stableID("handoff", request.ControlRunID, subjectDigest)
	predecessorDigest := ""
	if request.PredecessorReceiptID != "" {
		predecessorDigest, _, err = digestValue(map[string]string{"receipt_id": request.PredecessorReceiptID})
		if err != nil {
			return HandoffReceipt{}, err
		}
	}
	record, created, err := s.commitTransition(
		ctx, request.ControlRunID, request.Identity, operation,
		"evidence_handoff", request.Subject, predecessorDigest,
		func(p *projection, binding semanticBinding, requestDigest string) (outcomeEnvelope, error) {
			key := scopedID(request.ControlRunID, handoffID)
			prior, exists := p.handoffs[key]
			if state == HandoffIssued {
				if exists {
					return outcomeEnvelope{}, typedError(CodeConflict, operation, ErrConflict)
				}
			} else {
				if !exists {
					if owner, receiptExists := p.handoffReceipts[request.PredecessorReceiptID]; receiptExists && owner != key {
						return outcomeEnvelope{}, typedError(CodeConflict, operation, ErrConflict)
					}
					return outcomeEnvelope{}, typedError(CodeNotFound, operation, ErrNotFound)
				}
				if prior.Subject != request.Subject || prior.ReceiptID != request.PredecessorReceiptID ||
					!validHandoffTransition(prior.State, state) {
					return outcomeEnvelope{}, typedError(CodeConflict, operation, ErrConflict)
				}
			}
			receiptID := opaqueReceiptID(operation, binding, requestDigest)
			return outcomeEnvelope{Handoff: &EvidenceHandoff{
				ID: handoffID, ControlRunID: request.ControlRunID, Subject: request.Subject,
				State: state, ReceiptID: receiptID, PredecessorReceiptID: request.PredecessorReceiptID,
			}}, nil
		},
	)
	if err != nil {
		return HandoffReceipt{}, err
	}
	return handoffReceipt(record, created), nil
}

func (s *Service) EvidenceDisclosure(
	ctx context.Context,
	controlRunID, handoffID string,
) (EvidenceDisclosure, error) {
	if !boundedRequired(controlRunID, 128) || !boundedRequired(handoffID, 128) {
		return EvidenceDisclosure{}, typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	p, err := s.rebuild(ctx)
	if err != nil {
		return EvidenceDisclosure{}, err
	}
	handoff, ok := p.handoffs[scopedID(controlRunID, handoffID)]
	if !ok {
		return EvidenceDisclosure{}, typedError(CodeNotFound, "", ErrNotFound)
	}
	return EvidenceDisclosure{
		Authoritative: false, HandoffID: handoff.ID, ControlRunID: handoff.ControlRunID,
		EdgeID: handoff.Subject.EdgeID, ProducerProjectID: handoff.Subject.Producer.ProjectID,
		ConsumerProjectID: handoff.Subject.Consumer.ProjectID,
		EvidenceDigest:    handoff.Subject.EvidenceDigest, State: handoff.State,
	}, nil
}

func validateHandoffRequest(request HandoffRequest, state HandoffState) error {
	if !boundedRequired(request.ControlRunID, 128) || len(request.PredecessorReceiptID) > 128 {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	if state == HandoffIssued && request.PredecessorReceiptID != "" ||
		state != HandoffIssued && !boundedRequired(request.PredecessorReceiptID, 128) {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	if err := request.Identity.validate(); err != nil {
		return err
	}
	if request.Identity.GraphRevision != request.Subject.GraphRevision {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	return request.Subject.validate()
}

func handoffReceipt(record transitionRecord, created bool) HandoffReceipt {
	receipt := HandoffReceipt{
		Commit: record.receipt, Operation: record.outcome.SemanticOperation, Created: created,
	}
	receipt.Commit.Created = created
	if record.outcome.Handoff != nil {
		receipt.Handoff = *record.outcome.Handoff
		receipt.Handoff.Sequence = uint64(record.receipt.Outcome.JournalPosition)
	}
	return receipt
}
