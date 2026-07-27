package admission

import (
	"context"
	"time"
)

type leaseTransitionSubject struct {
	Lease     LeaseSubject `json:"lease"`
	ExpiresAt time.Time    `json:"expires_at"`
}

func (s *Service) AcquireLease(ctx context.Context, request LeaseRequest) (LeaseReceipt, error) {
	if err := validateLeaseRequest(request); err != nil {
		return LeaseReceipt{}, err
	}
	key := scopedID(request.ControlRunID, request.Subject.ID)
	record, created, err := s.commitTransition(
		ctx, request.ControlRunID, request.Identity, OperationLeaseAcquire,
		"lease_request", leaseSubject(request), "",
		func(p *projection, binding semanticBinding, requestDigest string) (outcomeEnvelope, error) {
			now := s.now()
			if !now.Before(request.ExpiresAt) {
				return outcomeEnvelope{}, typedError(CodeTerminal, OperationLeaseAcquire, ErrTerminal)
			}
			if _, exists := p.leases[key]; exists {
				return outcomeEnvelope{}, typedError(CodeConflict, OperationLeaseAcquire, ErrConflict)
			}
			if conflictingLease(p, request.ControlRunID, request.Subject) {
				return outcomeEnvelope{}, typedError(CodeLeaseBusy, OperationLeaseAcquire, ErrLeaseBusy)
			}
			return outcomeEnvelope{Lease: &Lease{
				ControlRunID: request.ControlRunID, Subject: request.Subject, State: LeaseActive,
				IssuedAt: now, ExpiresAt: request.ExpiresAt.UTC(), Generation: binding.Generation,
				GraphRevision: binding.GraphRevision, OriginalRequestDigest: requestDigest,
				ReceiptID: opaqueReceiptID(OperationLeaseAcquire, binding, requestDigest),
			}}, nil
		},
	)
	if err != nil {
		return LeaseReceipt{}, err
	}
	return leaseReceipt(record, created), nil
}

func (s *Service) RenewLease(ctx context.Context, request LeaseRequest) (LeaseReceipt, error) {
	if err := validateLeaseRequest(request); err != nil {
		return LeaseReceipt{}, err
	}
	key := scopedID(request.ControlRunID, request.Subject.ID)
	record, created, err := s.commitTransition(
		ctx, request.ControlRunID, request.Identity, OperationLeaseRenew,
		"lease_request", leaseSubject(request), "",
		func(p *projection, binding semanticBinding, _ string) (outcomeEnvelope, error) {
			now := s.now()
			if !now.Before(request.ExpiresAt) {
				return outcomeEnvelope{}, typedError(CodeTerminal, OperationLeaseRenew, ErrTerminal)
			}
			prior, ok := p.leases[key]
			if !ok {
				return outcomeEnvelope{}, typedError(CodeNotFound, OperationLeaseRenew, ErrNotFound)
			}
			if prior.Subject != request.Subject {
				return outcomeEnvelope{}, typedError(CodeConflict, OperationLeaseRenew, ErrConflict)
			}
			if prior.State != LeaseActive || !now.Before(prior.ExpiresAt) {
				return outcomeEnvelope{}, typedError(CodeTerminal, OperationLeaseRenew, ErrTerminal)
			}
			if binding.Generation <= prior.Generation || !request.ExpiresAt.After(prior.ExpiresAt) {
				return outcomeEnvelope{}, typedError(CodeConflict, OperationLeaseRenew, ErrConflict)
			}
			prior.ExpiresAt = request.ExpiresAt.UTC()
			prior.Generation = binding.Generation
			prior.GraphRevision = binding.GraphRevision
			prior.ReceiptID = opaqueReceiptID(OperationLeaseRenew, binding, prior.OriginalRequestDigest)
			return outcomeEnvelope{Lease: &prior}, nil
		},
	)
	if err != nil {
		return LeaseReceipt{}, err
	}
	return leaseReceipt(record, created), nil
}

func (s *Service) ReleaseLease(ctx context.Context, request LeaseRequest) (LeaseReceipt, error) {
	return s.terminalLease(ctx, request, OperationLeaseRelease, LeaseReleased)
}

func (s *Service) ExpireLease(ctx context.Context, request LeaseRequest) (LeaseReceipt, error) {
	return s.terminalLease(ctx, request, OperationLeaseExpire, LeaseExpired)
}

func (s *Service) terminalLease(
	ctx context.Context,
	request LeaseRequest,
	operation SemanticOperation,
	state LeaseState,
) (LeaseReceipt, error) {
	if err := validateLeaseRequest(request); err != nil {
		return LeaseReceipt{}, err
	}
	key := scopedID(request.ControlRunID, request.Subject.ID)
	record, created, err := s.commitTransition(
		ctx, request.ControlRunID, request.Identity, operation, "lease_request", leaseSubject(request), "",
		func(p *projection, binding semanticBinding, _ string) (outcomeEnvelope, error) {
			now := s.now()
			prior, ok := p.leases[key]
			if !ok {
				return outcomeEnvelope{}, typedError(CodeNotFound, operation, ErrNotFound)
			}
			if prior.Subject != request.Subject {
				return outcomeEnvelope{}, typedError(CodeConflict, operation, ErrConflict)
			}
			if !prior.ExpiresAt.Equal(request.ExpiresAt.UTC()) {
				return outcomeEnvelope{}, typedError(CodeConflict, operation, ErrConflict)
			}
			if prior.State != LeaseActive {
				return outcomeEnvelope{}, typedError(CodeTerminal, operation, ErrTerminal)
			}
			if state == LeaseExpired && now.Before(prior.ExpiresAt) {
				return outcomeEnvelope{}, typedError(CodeNotExpired, operation, ErrNotExpired)
			}
			return outcomeEnvelope{LeaseTombstone: &LeaseTombstone{
				ControlRunID: request.ControlRunID, Subject: request.Subject, State: state,
				IssuedAt: prior.IssuedAt, ExpiresAt: prior.ExpiresAt,
				Generation: binding.Generation, GraphRevision: binding.GraphRevision,
				OriginalRequestDigest: prior.OriginalRequestDigest,
				TerminalReceiptID:     opaqueReceiptID(operation, binding, prior.OriginalRequestDigest),
				TerminalAt:            now,
			}}, nil
		},
	)
	if err != nil {
		return LeaseReceipt{}, err
	}
	return leaseReceipt(record, created), nil
}

func (s *Service) Lease(ctx context.Context, controlRunID, leaseID string) (Lease, *LeaseTombstone, error) {
	if !boundedRequired(controlRunID, 128) || !boundedRequired(leaseID, 128) {
		return Lease{}, nil, typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	p, err := s.rebuild(ctx)
	if err != nil {
		return Lease{}, nil, err
	}
	key := scopedID(controlRunID, leaseID)
	lease, ok := p.leases[key]
	if !ok {
		return Lease{}, nil, typedError(CodeNotFound, "", ErrNotFound)
	}
	if tombstone, ok := p.leaseTombs[key]; ok {
		copy := tombstone
		return lease, &copy, nil
	}
	return lease, nil, nil
}

func validateLeaseRequest(request LeaseRequest) error {
	if !boundedRequired(request.ControlRunID, 128) || request.ExpiresAt.IsZero() {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	if err := request.Identity.validate(); err != nil {
		return err
	}
	return request.Subject.validate()
}

func conflictingLease(p *projection, controlRunID string, subject LeaseSubject) bool {
	for key, lease := range p.leases {
		if lease.State != LeaseActive || key == scopedID(controlRunID, subject.ID) ||
			lease.Subject.Resource != subject.Resource {
			continue
		}
		if lease.Subject.Mode == LeaseExclusive || subject.Mode == LeaseExclusive {
			return true
		}
	}
	return false
}

func leaseSubject(request LeaseRequest) leaseTransitionSubject {
	return leaseTransitionSubject{Lease: request.Subject, ExpiresAt: request.ExpiresAt.UTC()}
}

func leaseReceipt(record transitionRecord, created bool) LeaseReceipt {
	receipt := LeaseReceipt{
		Commit: record.receipt, Operation: record.outcome.SemanticOperation, Created: created,
	}
	receipt.Commit.Created = created
	if record.outcome.Lease != nil {
		receipt.Lease = *record.outcome.Lease
	}
	if record.outcome.LeaseTombstone != nil {
		value := *record.outcome.LeaseTombstone
		receipt.Tombstone = &value
		receipt.Lease = Lease{
			ControlRunID: value.ControlRunID, Subject: value.Subject, State: value.State,
			IssuedAt: value.IssuedAt, ExpiresAt: value.ExpiresAt,
			Generation: value.Generation, GraphRevision: value.GraphRevision,
			OriginalRequestDigest: value.OriginalRequestDigest, ReceiptID: value.TerminalReceiptID,
		}
	}
	return receipt
}
