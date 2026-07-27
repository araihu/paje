package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/araihu/paje/internal/controlplane/journal"
)

const maxCASAttempts = 256

var errAdmissionDecisionChanged = errors.New("admission decision changed")

type Service struct {
	store            journal.AuthoritativeStore
	policy           Policy
	policyDigest     string
	clock            func() time.Time
	observer         Observer
	scannerAuthority string
}

func New(dependencies Dependencies) (*Service, error) {
	if dependencies.Store == nil || dependencies.Clock == nil {
		return nil, typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	if err := dependencies.Policy.validate(); err != nil {
		return nil, err
	}
	policyDigest, _, err := digestValue(dependencies.Policy)
	if err != nil {
		return nil, err
	}
	if len(dependencies.ScannerAuthority) > 128 {
		return nil, typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	return &Service{
		store: dependencies.Store, policy: dependencies.Policy, policyDigest: policyDigest,
		clock:    dependencies.Clock,
		observer: dependencies.Observer, scannerAuthority: dependencies.ScannerAuthority,
	}, nil
}

type outcomeBuilder func(*projection, semanticBinding, string) (outcomeEnvelope, error)

func (s *Service) commitTransition(
	ctx context.Context,
	controlRunID string,
	identity TransitionIdentity,
	operation SemanticOperation,
	subjectKind string,
	subject any,
	predecessorDigest string,
	build outcomeBuilder,
) (transitionRecord, bool, error) {
	if err := validateTransitionInput(controlRunID, identity, operation, subjectKind); err != nil {
		return transitionRecord{}, false, err
	}
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return transitionRecord{}, false, err
		}
		p, err := s.rebuild(ctx)
		if err != nil {
			return transitionRecord{}, false, err
		}
		subjectDigest, subjectPayload, err := digestValue(subject)
		if err != nil {
			return transitionRecord{}, false, err
		}
		binding := semanticBinding{
			ActionID: identity.ActionID, AttemptID: identity.AttemptID, ControlRunID: controlRunID,
			Generation: identity.Generation, GraphRevision: identity.GraphRevision,
			IdempotencyKey: identity.IdempotencyKey, InstallationID: p.installationID,
			OutcomeEventID: identity.OutcomeEventID, OutcomeKind: identity.OutcomeKind,
			SemanticOperation: operation, SubjectDigest: subjectDigest, TaskID: identity.TaskID,
		}
		request := requestEnvelope{
			Binding: binding, Component: component, PredecessorDigest: predecessorDigest,
			SchemaVersion: SchemaVersion, SemanticOperation: operation,
			Subject: subjectPayload, SubjectDigest: subjectDigest, SubjectKind: subjectKind,
		}
		if subjectKind == "run_admission" {
			request.PolicyVersion = s.policy.Version
			request.PolicyDigest = s.policyDigest
		}
		requestPayload, err := canonicalValue(request)
		if err != nil {
			return transitionRecord{}, false, err
		}
		if existing, ok := p.transitions[semanticIndex(controlRunID, identity.ActionID, identity.Generation)]; ok {
			if !bytes.Equal(existing.requestPayload, requestPayload) || existing.binding != binding {
				return transitionRecord{}, false, typedError(CodeConflict, operation, ErrConflict)
			}
			existing.receipt.Created = false
			return existing, false, nil
		}
		if existing, ok := p.idempotency[idempotencyIndex(controlRunID, identity.IdempotencyKey)]; ok {
			if !bytes.Equal(existing.requestPayload, requestPayload) || existing.binding != binding {
				return transitionRecord{}, false, typedError(CodeConflict, operation, ErrConflict)
			}
			existing.receipt.Created = false
			return existing, false, nil
		}
		if identity.OutcomeKind != journal.EventActionResult {
			return transitionRecord{}, false, typedError(CodeInvalidRequest, operation, ErrInvalidRecord)
		}
		outcome, err := build(p, binding, digestBytes(requestPayload))
		if err != nil {
			return transitionRecord{}, false, err
		}
		outcome.Binding = binding
		outcome.Component = component
		outcome.PredecessorDigest = predecessorDigest
		outcome.PolicyVersion = request.PolicyVersion
		outcome.PolicyDigest = request.PolicyDigest
		outcome.SchemaVersion = SchemaVersion
		outcome.SemanticOperation = operation
		outcome.Subject = append(json.RawMessage(nil), subjectPayload...)
		outcome.SubjectDigest = subjectDigest
		outcome.SubjectKind = subjectKind
		if err := validateOutcomeSemantics(request, outcome, requestPayload); err != nil {
			return transitionRecord{}, false, err
		}
		outcomePayload, err := canonicalValue(outcome)
		if err != nil {
			return transitionRecord{}, false, err
		}
		action := journal.Action{
			ID: internalActionID(binding), ControlRunID: controlRunID, TaskID: identity.TaskID,
			AttemptID: identity.AttemptID, Kind: operationKind(operation), GraphRevision: identity.GraphRevision,
			ExpectedProjection: p.runHeads[controlRunID], CanonicalRequestDigest: digestBytes(requestPayload),
			IdempotencyKey: internalIdempotencyKey(binding),
		}
		commitRequest := journal.CommitRequest{
			Action: action,
			ExpectedRun: journal.RunCursor{
				InstallationID: p.installationID, ControlRunID: controlRunID,
				SchemaVersion: journal.SchemaVersion, RunSequence: p.runHeads[controlRunID],
			},
			ExpectedGlobal: p.global, RequestPayload: requestPayload,
			Outcome: journal.Event{
				ID: internalOutcomeID(binding), ControlRunID: controlRunID, ActionID: action.ID,
				Kind: identity.OutcomeKind, PayloadDigest: digestBytes(outcomePayload),
				OccurredAt: s.now(),
			},
			OutcomePayload: outcomePayload,
		}
		receipt, commitErr := s.store.Commit(ctx, commitRequest)
		if commitErr == nil {
			record := transitionRecord{
				binding: binding, request: request, requestPayload: requestPayload,
				outcome: outcome, receipt: receipt,
			}
			return record, receipt.Created, nil
		}
		// A dependency may lose the response after the commit became visible.
		// Rebuild first and return the immutable receipt when exact membership won.
		recovered, rebuildErr := s.rebuild(ctx)
		if rebuildErr == nil {
			if existing, ok := recovered.transitions[semanticIndex(controlRunID, identity.ActionID, identity.Generation)]; ok {
				if !bytes.Equal(existing.requestPayload, requestPayload) || existing.binding != binding {
					return transitionRecord{}, false, typedError(CodeConflict, operation, ErrConflict)
				}
				existing.receipt.Created = false
				return existing, false, nil
			}
		}
		if errors.Is(commitErr, journal.ErrConflict) {
			continue
		}
		return transitionRecord{}, false, storeError(operation, commitErr)
	}
	return transitionRecord{}, false, typedError(CodeConflict, operation, ErrConflict)
}

func validateTransitionInput(
	controlRunID string,
	identity TransitionIdentity,
	operation SemanticOperation,
	subjectKind string,
) error {
	if !boundedRequired(controlRunID, 128) || !boundedRequired(subjectKind, 64) || operationKind(operation) == "" {
		return typedError(CodeInvalidRequest, operation, ErrInvalidRecord)
	}
	if err := identity.validate(); err != nil {
		return typedError(CodeInvalidRequest, operation, ErrInvalidRecord)
	}
	return nil
}

func (s *Service) now() time.Time { return s.clock().UTC() }

func (s *Service) Reserve(ctx context.Context, request AdmissionRequest) (AdmissionReceipt, error) {
	if err := validateAdmissionRequest(request); err != nil {
		return AdmissionReceipt{}, err
	}
	record, created, err := s.commitTransition(
		ctx, request.ControlRunID, request.Identity, OperationAdmissionReserve,
		"run_admission", request.Subject, "",
		func(p *projection, binding semanticBinding, requestDigest string) (outcomeEnvelope, error) {
			key := scopedID(request.ControlRunID, request.Subject.ID)
			if _, exists := p.admissions[key]; exists {
				return outcomeEnvelope{}, typedError(CodeConflict, OperationAdmissionReserve, ErrConflict)
			}
			return outcomeEnvelope{Admission: &RunAdmission{
				ControlRunID: request.ControlRunID, Subject: request.Subject, State: AdmissionReserved,
				GraphRevision: binding.GraphRevision, Generation: binding.Generation,
				PolicyVersion: s.policy.Version, PolicyDigest: s.policyDigest,
				OriginalRequestDigest: requestDigest,
				ReceiptID:             opaqueReceiptID(OperationAdmissionReserve, binding, requestDigest),
			}}, nil
		},
	)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	return admissionReceipt(record, created), nil
}

func (s *Service) Enqueue(ctx context.Context, request AdmissionRequest) (AdmissionReceipt, error) {
	return s.admissionStateTransition(ctx, request, OperationQueueEnqueue, AdmissionQueued, QuotaNone)
}

func (s *Service) Admit(ctx context.Context, request AdmissionRequest) (AdmissionReceipt, error) {
	if err := validateAdmissionRequest(request); err != nil {
		return AdmissionReceipt{}, err
	}
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		p, err := s.rebuild(ctx)
		if err != nil {
			return AdmissionReceipt{}, err
		}
		limiter := s.limitingQuota(p, request)
		operation := OperationQueueAdmit
		state := AdmissionAdmitted
		if limiter != QuotaNone {
			operation = OperationBackpressureDefer
			state = AdmissionDeferred
		}
		receipt, err := s.admissionStateTransition(ctx, request, operation, state, limiter)
		if err == nil {
			return receipt, nil
		}
		if errors.Is(err, errAdmissionDecisionChanged) {
			continue
		}
		return AdmissionReceipt{}, err
	}
	return AdmissionReceipt{}, typedError(CodeConflict, OperationQueueAdmit, ErrConflict)
}

func (s *Service) admissionStateTransition(
	ctx context.Context,
	request AdmissionRequest,
	operation SemanticOperation,
	state AdmissionState,
	limiter QuotaKind,
) (AdmissionReceipt, error) {
	if err := validateAdmissionRequest(request); err != nil {
		return AdmissionReceipt{}, err
	}
	key := scopedID(request.ControlRunID, request.Subject.ID)
	record, created, err := s.commitTransition(
		ctx, request.ControlRunID, request.Identity, operation, "run_admission", request.Subject, "",
		func(p *projection, binding semanticBinding, _ string) (outcomeEnvelope, error) {
			prior, ok := p.admissions[key]
			if !ok {
				return outcomeEnvelope{}, typedError(CodeNotFound, operation, ErrNotFound)
			}
			if prior.Subject != request.Subject || prior.State == AdmissionReleased || prior.State == AdmissionAdmitted {
				return outcomeEnvelope{}, typedError(CodeConflict, operation, ErrConflict)
			}
			if operation == OperationQueueAdmit && s.limitingQuota(p, request) != QuotaNone ||
				operation == OperationBackpressureDefer && s.limitingQuota(p, request) != limiter {
				return outcomeEnvelope{}, errAdmissionDecisionChanged
			}
			prior.State = state
			prior.LimitingQuota = limiter
			prior.Generation = binding.Generation
			prior.GraphRevision = binding.GraphRevision
			prior.PolicyVersion = s.policy.Version
			prior.PolicyDigest = s.policyDigest
			prior.ReceiptID = opaqueReceiptID(operation, binding, prior.OriginalRequestDigest)
			prior.Sequence = 0
			return outcomeEnvelope{Admission: &prior}, nil
		},
	)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	return admissionReceipt(record, created), nil
}

func (s *Service) Release(ctx context.Context, request AdmissionRequest) (AdmissionReceipt, error) {
	if err := validateAdmissionRequest(request); err != nil {
		return AdmissionReceipt{}, err
	}
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		p, err := s.rebuild(ctx)
		if err != nil {
			return AdmissionReceipt{}, err
		}
		prior, ok := p.admissions[scopedID(request.ControlRunID, request.Subject.ID)]
		if !ok {
			return AdmissionReceipt{}, typedError(CodeNotFound, OperationAdmissionRelease, ErrNotFound)
		}
		operation := releaseOperation(prior.State)
		receipt, err := s.release(ctx, request, operation)
		if err == nil {
			return receipt, nil
		}
		if errors.Is(err, errAdmissionDecisionChanged) {
			continue
		}
		return AdmissionReceipt{}, err
	}
	return AdmissionReceipt{}, typedError(CodeConflict, OperationAdmissionRelease, ErrConflict)
}

func (s *Service) release(
	ctx context.Context,
	request AdmissionRequest,
	operation SemanticOperation,
) (AdmissionReceipt, error) {
	key := scopedID(request.ControlRunID, request.Subject.ID)
	record, created, err := s.commitTransition(
		ctx, request.ControlRunID, request.Identity, operation,
		"run_admission", request.Subject, "",
		func(p *projection, binding semanticBinding, _ string) (outcomeEnvelope, error) {
			prior, ok := p.admissions[key]
			if !ok {
				return outcomeEnvelope{}, typedError(CodeNotFound, operation, ErrNotFound)
			}
			if prior.Subject != request.Subject || prior.State == AdmissionReleased {
				return outcomeEnvelope{}, typedError(CodeConflict, operation, ErrConflict)
			}
			if releaseOperation(prior.State) != operation {
				return outcomeEnvelope{}, errAdmissionDecisionChanged
			}
			return outcomeEnvelope{AdmissionTombstone: &AdmissionTombstone{
				ControlRunID: request.ControlRunID, Subject: request.Subject,
				GraphRevision: binding.GraphRevision, Generation: binding.Generation,
				PolicyVersion: s.policy.Version, PolicyDigest: s.policyDigest,
				OriginalRequestDigest: prior.OriginalRequestDigest,
				TerminalReceiptID:     opaqueReceiptID(operation, binding, prior.OriginalRequestDigest),
				ReleasedAt:            s.now(),
			}}, nil
		},
	)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	return admissionReceipt(record, created), nil
}

func releaseOperation(state AdmissionState) SemanticOperation {
	if state == AdmissionQueued || state == AdmissionDeferred {
		return OperationQueueRelease
	}
	return OperationAdmissionRelease
}

func (s *Service) Admission(ctx context.Context, controlRunID, admissionID string) (RunAdmission, error) {
	if !boundedRequired(controlRunID, 128) || !boundedRequired(admissionID, 128) {
		return RunAdmission{}, typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	p, err := s.rebuild(ctx)
	if err != nil {
		return RunAdmission{}, err
	}
	value, ok := p.admissions[scopedID(controlRunID, admissionID)]
	if !ok {
		return RunAdmission{}, typedError(CodeNotFound, "", ErrNotFound)
	}
	return value, nil
}

func validateAdmissionRequest(request AdmissionRequest) error {
	if !boundedRequired(request.ControlRunID, 128) {
		return typedError(CodeInvalidRequest, "", ErrInvalidRecord)
	}
	if err := request.Identity.validate(); err != nil {
		return err
	}
	return request.Subject.validate()
}

func (s *Service) limitingQuota(p *projection, request AdmissionRequest) QuotaKind {
	var installation, principal, run, project, primitive uint64
	for _, value := range p.admissions {
		if value.State != AdmissionAdmitted {
			continue
		}
		installation = SaturatingAdd(installation, 1)
		if value.Subject.PrincipalID == request.Subject.PrincipalID {
			principal = SaturatingAdd(principal, 1)
		}
		if value.ControlRunID == request.ControlRunID {
			run = SaturatingAdd(run, 1)
		}
		if value.Subject.ProjectID == request.Subject.ProjectID {
			project = SaturatingAdd(project, 1)
		}
		if value.Subject.Primitive == request.Subject.Primitive {
			primitive = SaturatingAdd(primitive, 1)
		}
	}
	for _, fixture := range []struct {
		kind  QuotaKind
		count uint64
		limit uint64
	}{
		{QuotaInstallation, installation, s.policy.InstallationLimit},
		{QuotaPrincipal, principal, s.policy.PrincipalLimit},
		{QuotaRun, run, s.policy.RunLimit},
		{QuotaProject, project, s.policy.ProjectLimit},
		{QuotaPrimitive, primitive, s.policy.PrimitiveLimit},
	} {
		if fixture.count >= fixture.limit {
			return fixture.kind
		}
	}
	return QuotaNone
}

func admissionReceipt(record transitionRecord, created bool) AdmissionReceipt {
	receipt := AdmissionReceipt{
		Commit: record.receipt, Operation: record.outcome.SemanticOperation, Created: created,
	}
	receipt.Commit.Created = created
	if record.outcome.Admission != nil {
		receipt.Admission = *record.outcome.Admission
		receipt.Admission.Sequence = uint64(record.receipt.Outcome.JournalPosition)
		if receipt.Admission.State == AdmissionDeferred {
			receipt.Backpressure = &BackpressureReceipt{
				LimitingQuota:   receipt.Admission.LimitingQuota,
				NextEligibility: EligibilityQuotaAvailable,
				PolicyVersion:   receipt.Admission.PolicyVersion,
				PolicyDigest:    receipt.Admission.PolicyDigest,
			}
		}
	}
	if record.outcome.AdmissionTombstone != nil {
		value := *record.outcome.AdmissionTombstone
		receipt.Tombstone = &value
		receipt.Admission = RunAdmission{
			ControlRunID: value.ControlRunID, Subject: value.Subject, State: AdmissionReleased,
			GraphRevision: value.GraphRevision, Generation: value.Generation,
			PolicyVersion: value.PolicyVersion, PolicyDigest: value.PolicyDigest,
			OriginalRequestDigest: value.OriginalRequestDigest, ReceiptID: value.TerminalReceiptID,
			Sequence: uint64(record.receipt.Outcome.JournalPosition),
		}
	}
	return receipt
}

func opaqueReceiptID(operation SemanticOperation, binding semanticBinding, requestDigest string) string {
	return stableID("receipt", string(operation), binding.InstallationID, binding.ControlRunID,
		binding.ActionID, fmt.Sprint(binding.Generation), requestDigest)
}
