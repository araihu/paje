package admission

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/araihu/paje/internal/controlplane/journal"
)

type semanticBinding struct {
	ActionID          string            `json:"action_id"`
	AttemptID         string            `json:"attempt_id,omitempty"`
	ControlRunID      string            `json:"control_run_id"`
	Generation        uint64            `json:"generation"`
	GraphRevision     uint64            `json:"graph_revision"`
	IdempotencyKey    string            `json:"idempotency_key"`
	InstallationID    string            `json:"installation_id"`
	OutcomeEventID    string            `json:"outcome_event_id"`
	OutcomeKind       journal.EventKind `json:"outcome_kind"`
	SemanticOperation SemanticOperation `json:"semantic_operation"`
	SubjectDigest     string            `json:"subject_digest"`
	TaskID            string            `json:"task_id,omitempty"`
}

type requestEnvelope struct {
	Binding           semanticBinding   `json:"binding"`
	Component         string            `json:"component"`
	PredecessorDigest string            `json:"predecessor_digest,omitempty"`
	PolicyDigest      string            `json:"policy_digest,omitempty"`
	PolicyVersion     uint64            `json:"policy_version,omitempty"`
	SchemaVersion     uint32            `json:"schema_version"`
	SemanticOperation SemanticOperation `json:"semantic_operation"`
	Subject           json.RawMessage   `json:"subject"`
	SubjectDigest     string            `json:"subject_digest"`
	SubjectKind       string            `json:"subject_kind"`
}

type outcomeEnvelope struct {
	Admission          *RunAdmission       `json:"admission,omitempty"`
	AdmissionTombstone *AdmissionTombstone `json:"admission_tombstone,omitempty"`
	Binding            semanticBinding     `json:"binding"`
	Component          string              `json:"component"`
	Handoff            *EvidenceHandoff    `json:"handoff,omitempty"`
	Lease              *Lease              `json:"lease,omitempty"`
	LeaseTombstone     *LeaseTombstone     `json:"lease_tombstone,omitempty"`
	PredecessorDigest  string              `json:"predecessor_digest,omitempty"`
	PolicyDigest       string              `json:"policy_digest,omitempty"`
	PolicyVersion      uint64              `json:"policy_version,omitempty"`
	Recovery           *RecoveryRecord     `json:"recovery,omitempty"`
	SchemaVersion      uint32              `json:"schema_version"`
	SemanticOperation  SemanticOperation   `json:"semantic_operation"`
	Subject            json.RawMessage     `json:"subject"`
	SubjectDigest      string              `json:"subject_digest"`
	SubjectKind        string              `json:"subject_kind"`
}

type transitionRecord struct {
	binding         semanticBinding
	request         requestEnvelope
	requestPayload  []byte
	outcome         outcomeEnvelope
	receipt         journal.CommitReceipt
	integrityDigest string
}

type projection struct {
	installationID  string
	global          journal.GlobalCursor
	sourceDigest    string
	runHeads        map[string]uint64
	transitions     map[string]transitionRecord
	idempotency     map[string]transitionRecord
	admissions      map[string]RunAdmission
	admissionTombs  map[string]AdmissionTombstone
	leases          map[string]Lease
	leaseTombs      map[string]LeaseTombstone
	handoffs        map[string]EvidenceHandoff
	handoffReceipts map[string]string
	recoveries      map[string]RecoveryRecord
}

// projectionCache is derived only from verified Feed/Payload membership. Its
// projection is immutable after installation; suffix rebuilds clone before
// applying new events, so cache locking never spans journal I/O.
type projectionCache struct {
	cursor      journal.GlobalCursor
	digest      string
	stateDigest string
	projection  *projection
}

type projectionCacheProjectedState struct {
	RunHeads               map[string]uint64             `json:"run_heads"`
	TransitionIndexDigest  string                        `json:"transition_index_digest"`
	IdempotencyIndexDigest string                        `json:"idempotency_index_digest"`
	Admissions             map[string]RunAdmission       `json:"admissions"`
	AdmissionTombs         map[string]AdmissionTombstone `json:"admission_tombstones"`
	Leases                 map[string]Lease              `json:"leases"`
	LeaseTombs             map[string]LeaseTombstone     `json:"lease_tombstones"`
	Handoffs               map[string]EvidenceHandoff    `json:"handoffs"`
	HandoffReceipts        map[string]string             `json:"handoff_receipts"`
	Recoveries             map[string]RecoveryRecord     `json:"recoveries"`
}

type transitionRecordIntegrity struct {
	Binding        semanticBinding       `json:"binding"`
	Request        requestEnvelope       `json:"request"`
	RequestPayload []byte                `json:"request_payload"`
	Outcome        outcomeEnvelope       `json:"outcome"`
	Receipt        journal.CommitReceipt `json:"receipt"`
}

func newProjection() *projection {
	return &projection{
		sourceDigest: digestBytes(nil),
		runHeads:     make(map[string]uint64), transitions: make(map[string]transitionRecord),
		idempotency: make(map[string]transitionRecord), admissions: make(map[string]RunAdmission),
		admissionTombs: make(map[string]AdmissionTombstone), leases: make(map[string]Lease),
		leaseTombs: make(map[string]LeaseTombstone), handoffs: make(map[string]EvidenceHandoff),
		handoffReceipts: make(map[string]string), recoveries: make(map[string]RecoveryRecord),
	}
}

func (s *Service) rebuild(ctx context.Context) (*projection, error) {
	p, cursor, cached := s.loadProjectionCache()
	rebuilt, err := s.rebuildFrom(ctx, p, cursor)
	if err != nil && cached != nil && cacheFallbackError(err) {
		s.discardProjectionCache(cached)
		cached = nil
		rebuilt, err = s.rebuildFrom(ctx, newProjection(), journal.GlobalCursor{})
	}
	if err != nil {
		return nil, err
	}
	if cached == nil || rebuilt.global != cached.cursor {
		s.installProjectionCache(rebuilt)
	}
	return rebuilt, nil
}

func (s *Service) rebuildFrom(
	ctx context.Context,
	p *projection,
	cursor journal.GlobalCursor,
) (*projection, error) {
	mutable := cursor == (journal.GlobalCursor{})
	reservations := make(map[string]journal.Event)
	wantPosition := cursor.JournalPosition + 1
	for {
		events, next, err := s.store.Feed(ctx, cursor, 1000)
		if err != nil {
			return nil, storeError("", err)
		}
		if p.installationID == "" {
			p.installationID = next.InstallationID
			p.global = journal.NewGlobalCursor(next.InstallationID)
		}
		if next.InstallationID != p.installationID || next.SchemaVersion != journal.SchemaVersion {
			return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
		}
		if len(events) > 0 && !mutable {
			p = cloneProjection(p)
			mutable = true
		}
		for _, event := range events {
			if event.JournalPosition != wantPosition || event.RunSequence != p.runHeads[event.ControlRunID]+1 {
				return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
			}
			wantPosition++
			p.runHeads[event.ControlRunID] = event.RunSequence
			eventDigest, err := journal.Digest(event)
			if err != nil {
				return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
			}
			p.sourceDigest = digestBytes([]byte(p.sourceDigest + "\x00" + eventDigest))
			if event.Kind == journal.EventActionReserved {
				if _, exists := reservations[event.ActionID]; exists {
					return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
				}
				reservations[event.ActionID] = event
				continue
			}
			if !journal.IsOutcome(event.Kind) {
				continue
			}
			payload, err := s.store.Payload(ctx, event.PayloadDigest)
			if err != nil {
				return nil, storeError("", err)
			}
			ours, err := admissionComponent(payload)
			if err != nil {
				return nil, err
			}
			if !ours {
				continue
			}
			reservation, ok := reservations[event.ActionID]
			if !ok {
				return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
			}
			action, err := s.store.Reservation(ctx, event.ControlRunID, event.ActionID)
			if err != nil {
				return nil, storeError("", err)
			}
			requestPayload, err := s.store.Payload(ctx, action.CanonicalRequestDigest)
			if err != nil {
				return nil, storeError("", err)
			}
			var request requestEnvelope
			if err := journal.DecodeStrict(requestPayload, &request); err != nil {
				return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
			}
			var outcome outcomeEnvelope
			if err := journal.DecodeStrict(payload, &outcome); err != nil {
				return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
			}
			if err := validatePersistedTransition(
				p.installationID, action, reservation, event, requestPayload, payload, request, outcome,
			); err != nil {
				return nil, err
			}
			record := transitionRecord{
				binding: request.Binding, request: request, requestPayload: append([]byte(nil), requestPayload...),
				outcome: outcome, receipt: journal.CommitReceipt{
					Action: action, Reservation: reservation, Outcome: event, Created: true,
				},
			}
			if err := sealTransitionRecord(&record); err != nil {
				return nil, err
			}
			semanticKey := semanticIndex(request.Binding.ControlRunID, request.Binding.ActionID, request.Binding.Generation)
			idempotencyKey := idempotencyIndex(request.Binding.ControlRunID, request.Binding.IdempotencyKey)
			if prior, exists := p.transitions[semanticKey]; exists &&
				(!validTransitionRecord(prior) || !sameTransition(prior, record)) {
				return nil, typedError(CodeInvalidRecord, request.SemanticOperation, ErrInvalidRecord)
			}
			if prior, exists := p.idempotency[idempotencyKey]; exists &&
				(!validTransitionRecord(prior) || !sameTransition(prior, record)) {
				return nil, typedError(CodeInvalidRecord, request.SemanticOperation, ErrInvalidRecord)
			}
			p.transitions[semanticKey] = record
			p.idempotency[idempotencyKey] = record
			if err := p.apply(record); err != nil {
				return nil, err
			}
			delete(reservations, event.ActionID)
		}
		cursor = next
		if mutable {
			p.global = next
		}
		if len(events) == 0 {
			break
		}
	}
	if p.installationID == "" {
		return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
	}
	for actionID := range reservations {
		if strings.HasPrefix(actionID, "admission_action_") {
			return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
		}
	}
	return p, nil
}

func (s *Service) loadProjectionCache() (*projection, journal.GlobalCursor, *projectionCache) {
	s.cacheMu.Lock()
	cached := s.cache
	s.cacheMu.Unlock()
	if cached == nil {
		return newProjection(), journal.GlobalCursor{}, nil
	}
	if !validProjectionCache(cached) {
		s.discardProjectionCache(cached)
		return newProjection(), journal.GlobalCursor{}, nil
	}
	return cached.projection, cached.cursor, cached
}

func (s *Service) installProjectionCache(p *projection) {
	candidate, err := newProjectionCache(p)
	if err != nil {
		return
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache != nil {
		if s.cache.cursor.InstallationID != candidate.cursor.InstallationID ||
			s.cache.cursor.SchemaVersion != candidate.cursor.SchemaVersion {
			s.cache = candidate
			return
		}
		if s.cache.cursor.JournalPosition > candidate.cursor.JournalPosition {
			return
		}
		if s.cache.cursor.JournalPosition == candidate.cursor.JournalPosition {
			if s.cache.digest == candidate.digest {
				return
			}
			s.cache = nil
			return
		}
	}
	s.cache = candidate
}

func (s *Service) discardProjectionCache(cached *projectionCache) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache == cached {
		s.cache = nil
	}
}

func (s *Service) discardCachedProjection(p *projection) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache != nil && s.cache.projection == p {
		s.cache = nil
	}
}

func newProjectionCache(p *projection) (*projectionCache, error) {
	stateDigest, digest, err := projectionCacheDigests(p)
	if err != nil {
		return nil, err
	}
	return &projectionCache{
		cursor: p.global, digest: digest, stateDigest: stateDigest, projection: p,
	}, nil
}

func validProjectionCache(cached *projectionCache) bool {
	if cached == nil || cached.projection == nil || !journal.ValidDigest(cached.digest) {
		return false
	}
	p := cached.projection
	if !boundedRequired(p.installationID, 128) || cached.cursor != p.global ||
		cached.cursor.InstallationID != p.installationID ||
		cached.cursor.SchemaVersion != journal.SchemaVersion || !journal.ValidDigest(p.sourceDigest) ||
		p.runHeads == nil || p.transitions == nil || p.idempotency == nil ||
		p.admissions == nil || p.admissionTombs == nil || p.leases == nil ||
		p.leaseTombs == nil || p.handoffs == nil || p.handoffReceipts == nil || p.recoveries == nil {
		return false
	}
	var eventCount uint64
	for _, runHead := range p.runHeads {
		if runHead > uint64(cached.cursor.JournalPosition)-eventCount {
			return false
		}
		eventCount += runHead
	}
	if eventCount != uint64(cached.cursor.JournalPosition) {
		return false
	}
	stateDigest, digest, err := projectionCacheDigests(p)
	return err == nil && stateDigest == cached.stateDigest && digest == cached.digest
}

func projectionCacheDigest(p *projection) (string, error) {
	_, digest, err := projectionCacheDigests(p)
	return digest, err
}

func projectionCacheDigests(p *projection) (string, string, error) {
	transitionsDigest, err := transitionIndexDigest(p.transitions)
	if err != nil {
		return "", "", err
	}
	idempotencyIndexDigest, err := transitionIndexDigest(p.idempotency)
	if err != nil {
		return "", "", err
	}
	statePayload, err := json.Marshal(projectionCacheProjectedState{
		RunHeads: p.runHeads, TransitionIndexDigest: transitionsDigest,
		IdempotencyIndexDigest: idempotencyIndexDigest,
		Admissions:             p.admissions, AdmissionTombs: p.admissionTombs,
		Leases: p.leases, LeaseTombs: p.leaseTombs, Handoffs: p.handoffs,
		HandoffReceipts: p.handoffReceipts, Recoveries: p.recoveries,
	})
	if err != nil {
		return "", "", typedError(CodeInvalidRecord, "", ErrInvalidRecord)
	}
	stateDigest := digestBytes(statePayload)
	digest, err := digestValueOnly(struct {
		InstallationID  string               `json:"installation_id"`
		Cursor          journal.GlobalCursor `json:"cursor"`
		SourceDigest    string               `json:"source_digest"`
		StateDigest     string               `json:"state_digest"`
		RunCount        int                  `json:"run_count"`
		TransitionCount int                  `json:"transition_count"`
		Idempotency     int                  `json:"idempotency_count"`
		Admissions      int                  `json:"admission_count"`
		AdmissionTombs  int                  `json:"admission_tombstone_count"`
		Leases          int                  `json:"lease_count"`
		LeaseTombs      int                  `json:"lease_tombstone_count"`
		Handoffs        int                  `json:"handoff_count"`
		HandoffReceipts int                  `json:"handoff_receipt_count"`
		Recoveries      int                  `json:"recovery_count"`
	}{
		InstallationID: p.installationID, Cursor: p.global, SourceDigest: p.sourceDigest,
		StateDigest: stateDigest,
		RunCount:    len(p.runHeads), TransitionCount: len(p.transitions), Idempotency: len(p.idempotency),
		Admissions: len(p.admissions), AdmissionTombs: len(p.admissionTombs), Leases: len(p.leases),
		LeaseTombs: len(p.leaseTombs), Handoffs: len(p.handoffs), HandoffReceipts: len(p.handoffReceipts),
		Recoveries: len(p.recoveries),
	})
	return stateDigest, digest, err
}

func sealTransitionRecord(record *transitionRecord) error {
	digest, err := transitionIntegrityDigest(*record)
	if err != nil {
		return err
	}
	record.integrityDigest = digest
	return nil
}

func validTransitionRecord(record transitionRecord) bool {
	if !journal.ValidDigest(record.integrityDigest) {
		return false
	}
	digest, err := transitionIntegrityDigest(record)
	return err == nil && digest == record.integrityDigest
}

func transitionIntegrityDigest(record transitionRecord) (string, error) {
	payload, err := json.Marshal(transitionRecordIntegrity{
		Binding: record.binding, Request: record.request, RequestPayload: record.requestPayload,
		Outcome: record.outcome, Receipt: record.receipt,
	})
	if err != nil {
		return "", typedError(CodeInvalidRecord, "", ErrInvalidRecord)
	}
	return digestBytes(payload), nil
}

func transitionIndexDigest(values map[string]transitionRecord) (string, error) {
	// XOR makes the index seal independent of Go map iteration order. Entry
	// digests bind both the key and immutable record seal; counts are bound by
	// the enclosing cache digest.
	var combined [sha256.Size]byte
	for key, record := range values {
		if !journal.ValidDigest(record.integrityDigest) {
			return "", typedError(CodeInvalidRecord, "", ErrInvalidRecord)
		}
		entry := sha256.Sum256([]byte(key + "\x00" + record.integrityDigest))
		for index := range combined {
			combined[index] ^= entry[index]
		}
	}
	return "sha256:" + hex.EncodeToString(combined[:]), nil
}

func digestValueOnly(value any) (string, error) {
	digest, _, err := digestValue(value)
	return digest, err
}

func cacheFallbackError(err error) bool {
	return errors.Is(err, ErrInvalidRecord) || errors.Is(err, ErrNotFound)
}

func cloneProjection(source *projection) *projection {
	cloned := newProjection()
	cloned.installationID = source.installationID
	cloned.global = source.global
	cloned.sourceDigest = source.sourceDigest
	for key, value := range source.runHeads {
		cloned.runHeads[key] = value
	}
	for key, value := range source.transitions {
		cloned.transitions[key] = value
	}
	for key, value := range source.idempotency {
		cloned.idempotency[key] = value
	}
	for key, value := range source.admissions {
		cloned.admissions[key] = value
	}
	for key, value := range source.admissionTombs {
		cloned.admissionTombs[key] = value
	}
	for key, value := range source.leases {
		cloned.leases[key] = value
	}
	for key, value := range source.leaseTombs {
		cloned.leaseTombs[key] = value
	}
	for key, value := range source.handoffs {
		cloned.handoffs[key] = value
	}
	for key, value := range source.handoffReceipts {
		cloned.handoffReceipts[key] = value
	}
	for key, value := range source.recoveries {
		cloned.recoveries[key] = value
	}
	return cloned
}

func admissionComponent(payload []byte) (bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return false, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
	}
	raw, ok := fields["component"]
	if !ok {
		return false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
	}
	return value == component, nil
}

func validatePersistedTransition(
	installationID string,
	action journal.Action,
	reservation, event journal.Event,
	requestPayload, outcomePayload []byte,
	request requestEnvelope,
	outcome outcomeEnvelope,
) error {
	if request.Component != component || outcome.Component != component ||
		request.SchemaVersion != SchemaVersion || outcome.SchemaVersion != SchemaVersion ||
		request.SemanticOperation != request.Binding.SemanticOperation ||
		outcome.SemanticOperation != outcome.Binding.SemanticOperation ||
		request.Binding != outcome.Binding || request.SubjectKind != outcome.SubjectKind ||
		request.SubjectDigest != outcome.SubjectDigest || request.SubjectDigest != request.Binding.SubjectDigest ||
		request.PredecessorDigest != outcome.PredecessorDigest ||
		request.PolicyDigest != outcome.PolicyDigest || request.PolicyVersion != outcome.PolicyVersion ||
		!bytes.Equal(request.Subject, outcome.Subject) || !journal.ValidDigest(request.SubjectDigest) ||
		request.Binding.InstallationID != installationID ||
		request.Binding.ControlRunID != action.ControlRunID || request.Binding.ControlRunID != event.ControlRunID ||
		action.ID != internalActionID(request.Binding) ||
		action.IdempotencyKey != internalIdempotencyKey(request.Binding) ||
		action.Kind != operationKind(request.SemanticOperation) ||
		action.GraphRevision != request.Binding.GraphRevision ||
		action.TaskID != request.Binding.TaskID || action.AttemptID != request.Binding.AttemptID ||
		action.CanonicalRequestDigest != digestBytes(requestPayload) ||
		reservation.ActionID != action.ID || reservation.ControlRunID != action.ControlRunID ||
		reservation.Kind != journal.EventActionReserved ||
		reservation.RunSequence != action.ExpectedProjection+1 ||
		event.RunSequence != reservation.RunSequence+1 ||
		event.JournalPosition != reservation.JournalPosition+1 ||
		event.ID != internalOutcomeID(request.Binding) || event.ActionID != action.ID ||
		event.Kind != request.Binding.OutcomeKind || event.PayloadDigest != digestBytes(outcomePayload) {
		return typedError(CodeInvalidRecord, request.SemanticOperation, ErrInvalidRecord)
	}
	actionDigest, err := journal.Digest(action)
	if err != nil || reservation.PayloadDigest != actionDigest {
		return typedError(CodeInvalidRecord, request.SemanticOperation, ErrInvalidRecord)
	}
	if err := validateOutcomeSemantics(request, outcome, requestPayload); err != nil {
		return err
	}
	return nil
}

func validateOutcomeShape(outcome outcomeEnvelope) error {
	count := 0
	for _, present := range []bool{
		outcome.Admission != nil, outcome.AdmissionTombstone != nil, outcome.Handoff != nil,
		outcome.Lease != nil, outcome.LeaseTombstone != nil, outcome.Recovery != nil,
	} {
		if present {
			count++
		}
	}
	if count != 1 || operationKind(outcome.SemanticOperation) == "" {
		return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
	}
	switch outcome.SemanticOperation {
	case OperationAdmissionReserve, OperationQueueEnqueue, OperationQueueAdmit, OperationBackpressureDefer:
		if outcome.Admission == nil {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
	case OperationAdmissionRelease, OperationQueueRelease:
		if outcome.AdmissionTombstone == nil {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
	case OperationLeaseAcquire, OperationLeaseRenew:
		if outcome.Lease == nil {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
	case OperationLeaseRelease, OperationLeaseExpire:
		if outcome.LeaseTombstone == nil {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
	case OperationHandoffIssue, OperationHandoffGrant, OperationHandoffAcknowledge:
		if outcome.Handoff == nil {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
	case OperationStartObservation, OperationCancelOrFence, OperationScannerApply:
		if outcome.Recovery == nil {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
	default:
		return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
	}
	return nil
}

func validateOutcomeSemantics(
	request requestEnvelope,
	outcome outcomeEnvelope,
	requestPayload []byte,
) error {
	if err := validateOutcomeShape(outcome); err != nil {
		return err
	}
	binding := outcome.Binding
	identity := TransitionIdentity{
		ActionID: binding.ActionID, AttemptID: binding.AttemptID,
		GraphRevision: binding.GraphRevision, Generation: binding.Generation,
		IdempotencyKey: binding.IdempotencyKey, OutcomeEventID: binding.OutcomeEventID,
		OutcomeKind: binding.OutcomeKind, TaskID: binding.TaskID,
	}
	if err := identity.validate(); err != nil || binding.OutcomeKind != journal.EventActionResult ||
		!journal.ValidDigest(binding.SubjectDigest) ||
		(request.PredecessorDigest != "" && !journal.ValidDigest(request.PredecessorDigest)) {
		return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
	}
	requestDigest := digestBytes(requestPayload)
	if err := validatePolicyBinding(request, outcome); err != nil {
		return err
	}
	switch outcome.SemanticOperation {
	case OperationAdmissionReserve, OperationQueueEnqueue, OperationQueueAdmit, OperationBackpressureDefer:
		return validateAdmissionOutcome(request, outcome, requestDigest)
	case OperationAdmissionRelease, OperationQueueRelease:
		return validateAdmissionTombstoneOutcome(request, outcome)
	case OperationLeaseAcquire, OperationLeaseRenew:
		return validateLeaseOutcome(request, outcome, requestDigest)
	case OperationLeaseRelease, OperationLeaseExpire:
		return validateLeaseTombstoneOutcome(request, outcome)
	case OperationHandoffIssue, OperationHandoffGrant, OperationHandoffAcknowledge:
		return validateHandoffOutcome(request, outcome, requestDigest)
	case OperationStartObservation, OperationCancelOrFence, OperationScannerApply:
		return validateRecoveryOutcome(request, outcome, requestDigest)
	default:
		return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
	}
}

func validateAdmissionOutcome(request requestEnvelope, outcome outcomeEnvelope, requestDigest string) error {
	if request.SubjectKind != "run_admission" || request.PredecessorDigest != "" {
		return invalidOutcome(outcome.SemanticOperation)
	}
	var subject AdmissionSubject
	if err := journal.DecodeStrict(request.Subject, &subject); err != nil || subject.validate() != nil ||
		!matchesSubjectDigest(subject, request.SubjectDigest) {
		return invalidOutcome(outcome.SemanticOperation)
	}
	value := outcome.Admission
	if value.ControlRunID != outcome.Binding.ControlRunID || value.Subject != subject ||
		value.Sequence != 0 || value.GraphRevision != outcome.Binding.GraphRevision ||
		value.Generation != outcome.Binding.Generation || value.PolicyVersion != request.PolicyVersion ||
		value.PolicyDigest != request.PolicyDigest || !journal.ValidDigest(value.OriginalRequestDigest) ||
		value.ReceiptID != opaqueReceiptID(outcome.SemanticOperation, outcome.Binding, value.OriginalRequestDigest) {
		return invalidOutcome(outcome.SemanticOperation)
	}
	wantState, wantLimiter := AdmissionState(""), QuotaNone
	switch outcome.SemanticOperation {
	case OperationAdmissionReserve:
		wantState = AdmissionReserved
		if value.OriginalRequestDigest != requestDigest {
			return invalidOutcome(outcome.SemanticOperation)
		}
	case OperationQueueEnqueue:
		wantState = AdmissionQueued
	case OperationQueueAdmit:
		wantState = AdmissionAdmitted
	case OperationBackpressureDefer:
		wantState = AdmissionDeferred
		wantLimiter = value.LimitingQuota
		if !validQuota(wantLimiter) || wantLimiter == QuotaNone {
			return invalidOutcome(outcome.SemanticOperation)
		}
	}
	if value.State != wantState || value.LimitingQuota != wantLimiter {
		return invalidOutcome(outcome.SemanticOperation)
	}
	return nil
}

func validateAdmissionTombstoneOutcome(request requestEnvelope, outcome outcomeEnvelope) error {
	if request.SubjectKind != "run_admission" || request.PredecessorDigest != "" {
		return invalidOutcome(outcome.SemanticOperation)
	}
	var subject AdmissionSubject
	if err := journal.DecodeStrict(request.Subject, &subject); err != nil || subject.validate() != nil ||
		!matchesSubjectDigest(subject, request.SubjectDigest) {
		return invalidOutcome(outcome.SemanticOperation)
	}
	value := outcome.AdmissionTombstone
	if value.ControlRunID != outcome.Binding.ControlRunID || value.Subject != subject ||
		value.GraphRevision != outcome.Binding.GraphRevision || value.Generation != outcome.Binding.Generation ||
		value.PolicyVersion != request.PolicyVersion || value.PolicyDigest != request.PolicyDigest ||
		!journal.ValidDigest(value.OriginalRequestDigest) || value.ReleasedAt.IsZero() ||
		value.TerminalReceiptID != opaqueReceiptID(outcome.SemanticOperation, outcome.Binding, value.OriginalRequestDigest) {
		return invalidOutcome(outcome.SemanticOperation)
	}
	return nil
}

func validateLeaseOutcome(request requestEnvelope, outcome outcomeEnvelope, requestDigest string) error {
	subject, err := decodeLeaseTransitionSubject(request, outcome.SemanticOperation)
	if err != nil {
		return err
	}
	value := outcome.Lease
	if value.ControlRunID != outcome.Binding.ControlRunID || value.Subject != subject.Lease ||
		value.State != LeaseActive || value.IssuedAt.IsZero() || !value.ExpiresAt.Equal(subject.ExpiresAt) ||
		!value.ExpiresAt.After(value.IssuedAt) || value.Generation != outcome.Binding.Generation ||
		value.GraphRevision != outcome.Binding.GraphRevision || !journal.ValidDigest(value.OriginalRequestDigest) ||
		value.ReceiptID != opaqueReceiptID(outcome.SemanticOperation, outcome.Binding, value.OriginalRequestDigest) {
		return invalidOutcome(outcome.SemanticOperation)
	}
	if outcome.SemanticOperation == OperationLeaseAcquire && value.OriginalRequestDigest != requestDigest {
		return invalidOutcome(outcome.SemanticOperation)
	}
	return nil
}

func validateLeaseTombstoneOutcome(request requestEnvelope, outcome outcomeEnvelope) error {
	subject, err := decodeLeaseTransitionSubject(request, outcome.SemanticOperation)
	if err != nil {
		return err
	}
	value := outcome.LeaseTombstone
	wantState := LeaseReleased
	if outcome.SemanticOperation == OperationLeaseExpire {
		wantState = LeaseExpired
	}
	if value.ControlRunID != outcome.Binding.ControlRunID || value.Subject != subject.Lease ||
		value.State != wantState || value.IssuedAt.IsZero() || !value.ExpiresAt.Equal(subject.ExpiresAt) ||
		value.Generation != outcome.Binding.Generation || value.GraphRevision != outcome.Binding.GraphRevision ||
		!journal.ValidDigest(value.OriginalRequestDigest) || value.TerminalAt.IsZero() ||
		value.TerminalReceiptID != opaqueReceiptID(outcome.SemanticOperation, outcome.Binding, value.OriginalRequestDigest) {
		return invalidOutcome(outcome.SemanticOperation)
	}
	return nil
}

func decodeLeaseTransitionSubject(request requestEnvelope, operation SemanticOperation) (leaseTransitionSubject, error) {
	if request.SubjectKind != "lease_request" || request.PredecessorDigest != "" {
		return leaseTransitionSubject{}, invalidOutcome(operation)
	}
	var subject leaseTransitionSubject
	if err := journal.DecodeStrict(request.Subject, &subject); err != nil ||
		subject.Lease.validate() != nil || subject.ExpiresAt.IsZero() ||
		!matchesSubjectDigest(subject, request.SubjectDigest) {
		return leaseTransitionSubject{}, invalidOutcome(operation)
	}
	return subject, nil
}

func validateHandoffOutcome(request requestEnvelope, outcome outcomeEnvelope, requestDigest string) error {
	if request.SubjectKind != "evidence_handoff" {
		return invalidOutcome(outcome.SemanticOperation)
	}
	var subject EvidenceHandoffSubject
	if err := journal.DecodeStrict(request.Subject, &subject); err != nil || subject.validate() != nil ||
		!matchesSubjectDigest(subject, request.SubjectDigest) {
		return invalidOutcome(outcome.SemanticOperation)
	}
	value := outcome.Handoff
	wantState, wantPredecessor := HandoffIssued, false
	switch outcome.SemanticOperation {
	case OperationHandoffGrant:
		wantState, wantPredecessor = HandoffGranted, true
	case OperationHandoffAcknowledge:
		wantState, wantPredecessor = HandoffAcknowledged, true
	}
	if value.ControlRunID != outcome.Binding.ControlRunID || value.Subject != subject ||
		subject.GraphRevision != outcome.Binding.GraphRevision ||
		value.ID != stableID("handoff", outcome.Binding.ControlRunID, outcome.SubjectDigest) ||
		value.State != wantState || value.Sequence != 0 ||
		value.ReceiptID != opaqueReceiptID(outcome.SemanticOperation, outcome.Binding, requestDigest) {
		return invalidOutcome(outcome.SemanticOperation)
	}
	if !wantPredecessor {
		if value.PredecessorReceiptID != "" || request.PredecessorDigest != "" {
			return invalidOutcome(outcome.SemanticOperation)
		}
		return nil
	}
	if !boundedRequired(value.PredecessorReceiptID, 128) {
		return invalidOutcome(outcome.SemanticOperation)
	}
	digest, _, err := digestValue(map[string]string{"receipt_id": value.PredecessorReceiptID})
	if err != nil || request.PredecessorDigest != digest {
		return invalidOutcome(outcome.SemanticOperation)
	}
	return nil
}

func validateRecoveryOutcome(request requestEnvelope, outcome outcomeEnvelope, requestDigest string) error {
	var identity RecoveryIdentity
	var wantState RecoveryState
	var wantFact ProviderFact
	switch outcome.SemanticOperation {
	case OperationStartObservation:
		if request.SubjectKind != "recovery_identity" || request.PredecessorDigest != "" {
			return invalidOutcome(outcome.SemanticOperation)
		}
		if err := journal.DecodeStrict(request.Subject, &identity); err != nil ||
			!matchesSubjectDigest(identity, request.SubjectDigest) {
			return invalidOutcome(outcome.SemanticOperation)
		}
		wantState = RecoveryObservationStarted
	case OperationCancelOrFence:
		if request.SubjectKind != "recovery_fence" || request.PredecessorDigest != "" {
			return invalidOutcome(outcome.SemanticOperation)
		}
		var subject fenceTransitionSubject
		if err := journal.DecodeStrict(request.Subject, &subject); err != nil ||
			!matchesSubjectDigest(subject, request.SubjectDigest) {
			return invalidOutcome(outcome.SemanticOperation)
		}
		identity = subject.Recovery
		wantFact = ProviderFact{Status: subject.Proof.Status, ReceiptID: subject.Proof.ReceiptID, SubjectDigest: identity.SubjectDigest}
		switch subject.Proof.Status {
		case ProviderCanceled:
			wantState = RecoveryCanceled
		case ProviderNotPerformed:
			wantState = RecoveryNotPerformed
		case ProviderFenced:
			wantState = RecoveryFenced
		default:
			return invalidOutcome(outcome.SemanticOperation)
		}
	case OperationScannerApply:
		if request.SubjectKind != "recovery_scanner_apply" || request.PredecessorDigest != "" {
			return invalidOutcome(outcome.SemanticOperation)
		}
		var subject scannerApplyTransitionSubject
		if err := journal.DecodeStrict(request.Subject, &subject); err != nil ||
			!boundedRequired(subject.ScannerAuthorityDigest, 128) ||
			!matchesSubjectDigest(subject, request.SubjectDigest) {
			return invalidOutcome(outcome.SemanticOperation)
		}
		identity, wantFact = subject.Recovery, subject.Fact
		switch subject.Fact.Status {
		case ProviderEffectObserved:
			wantState = RecoveryApplied
		case ProviderNotPerformed:
			wantState = RecoveryNotPerformed
		case ProviderCanceled:
			wantState = RecoveryCanceled
		case ProviderFenced:
			wantState = RecoveryFenced
		default:
			return invalidOutcome(outcome.SemanticOperation)
		}
	}
	if err := identity.validate(); err != nil || identity.ControlRunID != outcome.Binding.ControlRunID {
		return invalidOutcome(outcome.SemanticOperation)
	}
	if identity.InstallationID != "" && identity.InstallationID != outcome.Binding.InstallationID {
		return invalidOutcome(outcome.SemanticOperation)
	}
	identity.InstallationID = outcome.Binding.InstallationID
	value := outcome.Recovery
	if value.Identity != identity || value.State != wantState || value.Fact != wantFact ||
		value.ReceiptID != opaqueReceiptID(outcome.SemanticOperation, outcome.Binding, requestDigest) {
		return invalidOutcome(outcome.SemanticOperation)
	}
	if wantState != RecoveryObservationStarted {
		if err := validateProviderFact(value.Fact, identity.SubjectDigest); err != nil {
			return invalidOutcome(outcome.SemanticOperation)
		}
	}
	return nil
}

func validQuota(value QuotaKind) bool {
	switch value {
	case QuotaNone, QuotaInstallation, QuotaPrincipal, QuotaRun, QuotaProject, QuotaPrimitive:
		return true
	default:
		return false
	}
}

func validatePolicyBinding(request requestEnvelope, outcome outcomeEnvelope) error {
	isAdmission := request.SubjectKind == "run_admission"
	if isAdmission {
		if request.PolicyVersion == 0 || !journal.ValidDigest(request.PolicyDigest) {
			return invalidOutcome(outcome.SemanticOperation)
		}
		return nil
	}
	if request.PolicyVersion != 0 || request.PolicyDigest != "" {
		return invalidOutcome(outcome.SemanticOperation)
	}
	return nil
}

func matchesSubjectDigest(subject any, want string) bool {
	digest, _, err := digestValue(subject)
	return err == nil && digest == want
}

func invalidOutcome(operation SemanticOperation) error {
	return typedError(CodeInvalidRecord, operation, ErrInvalidRecord)
}

func (p *projection) apply(record transitionRecord) error {
	outcome := record.outcome
	runID := record.binding.ControlRunID
	sequence := uint64(record.receipt.Outcome.JournalPosition)
	switch {
	case outcome.Admission != nil:
		value := *outcome.Admission
		value.Sequence = sequence
		key := scopedID(runID, value.Subject.ID)
		if prior, ok := p.admissions[key]; ok {
			if prior.Subject != value.Subject || !validAdmissionTransition(prior.State, value.State) {
				return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
			}
		} else if value.State != AdmissionReserved {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
		p.admissions[key] = value
	case outcome.AdmissionTombstone != nil:
		value := *outcome.AdmissionTombstone
		key := scopedID(runID, value.Subject.ID)
		prior, ok := p.admissions[key]
		if !ok || prior.State == AdmissionReleased || prior.Subject != value.Subject ||
			prior.OriginalRequestDigest != value.OriginalRequestDigest ||
			releaseOperation(prior.State) != outcome.SemanticOperation {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
		prior.State = AdmissionReleased
		prior.ReceiptID = value.TerminalReceiptID
		prior.Sequence = sequence
		prior.GraphRevision = value.GraphRevision
		prior.Generation = value.Generation
		prior.PolicyVersion = value.PolicyVersion
		prior.PolicyDigest = value.PolicyDigest
		p.admissions[key] = prior
		p.admissionTombs[key] = value
	case outcome.Lease != nil:
		value := *outcome.Lease
		key := scopedID(runID, value.Subject.ID)
		if prior, ok := p.leases[key]; ok {
			if prior.Subject != value.Subject || prior.State != LeaseActive || value.State != LeaseActive ||
				value.Generation <= prior.Generation || value.OriginalRequestDigest != prior.OriginalRequestDigest ||
				!value.IssuedAt.Equal(prior.IssuedAt) || !value.ExpiresAt.After(prior.ExpiresAt) {
				return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
			}
		} else if value.State != LeaseActive {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
		p.leases[key] = value
	case outcome.LeaseTombstone != nil:
		value := *outcome.LeaseTombstone
		key := scopedID(runID, value.Subject.ID)
		prior, ok := p.leases[key]
		var requestSubject leaseTransitionSubject
		requestErr := journal.DecodeStrict(record.request.Subject, &requestSubject)
		if !ok || prior.State != LeaseActive || prior.Subject != value.Subject ||
			prior.OriginalRequestDigest != value.OriginalRequestDigest ||
			(value.State != LeaseReleased && value.State != LeaseExpired) || requestErr != nil ||
			requestSubject.Lease != prior.Subject || !requestSubject.ExpiresAt.Equal(prior.ExpiresAt) ||
			!value.IssuedAt.Equal(prior.IssuedAt) || !value.ExpiresAt.Equal(prior.ExpiresAt) ||
			value.TerminalAt.Before(prior.IssuedAt) ||
			(value.State == LeaseExpired && value.TerminalAt.Before(prior.ExpiresAt)) {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
		prior.State = value.State
		prior.ReceiptID = value.TerminalReceiptID
		prior.Generation = value.Generation
		prior.GraphRevision = value.GraphRevision
		p.leases[key] = prior
		p.leaseTombs[key] = value
	case outcome.Handoff != nil:
		value := *outcome.Handoff
		value.Sequence = sequence
		key := scopedID(runID, value.ID)
		if prior, ok := p.handoffs[key]; ok {
			if prior.Subject != value.Subject || !validHandoffTransition(prior.State, value.State) ||
				value.PredecessorReceiptID != prior.ReceiptID {
				return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
			}
		} else if value.State != HandoffIssued || value.PredecessorReceiptID != "" {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
		if existing, ok := p.handoffReceipts[value.ReceiptID]; ok && existing != key {
			return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
		}
		p.handoffs[key] = value
		p.handoffReceipts[value.ReceiptID] = key
	case outcome.Recovery != nil:
		value := *outcome.Recovery
		key := recoveryIndex(value.Identity)
		prior, exists := p.recoveries[key]
		latest, hasLatest := latestRecovery(p, value.Identity)
		if exists {
			if hasLatest && latest.Identity.Generation > value.Identity.Generation ||
				!validRecoveryTransition(prior.State, value.State) {
				return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
			}
		} else {
			if value.State != RecoveryObservationStarted {
				return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
			}
			if hasLatest && (latest.Identity.SubjectDigest != value.Identity.SubjectDigest ||
				latest.Identity.Generation == ^uint64(0) ||
				value.Identity.Generation != latest.Identity.Generation+1 ||
				(latest.State != RecoveryCanceled && latest.State != RecoveryNotPerformed)) {
				return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
			}
		}
		p.recoveries[key] = value
	default:
		return typedError(CodeInvalidRecord, outcome.SemanticOperation, ErrInvalidRecord)
	}
	return nil
}

func validAdmissionTransition(prior, next AdmissionState) bool {
	switch prior {
	case AdmissionReserved:
		return next == AdmissionQueued || next == AdmissionAdmitted || next == AdmissionDeferred
	case AdmissionQueued:
		return next == AdmissionAdmitted || next == AdmissionDeferred
	case AdmissionDeferred:
		return next == AdmissionAdmitted || next == AdmissionDeferred
	case AdmissionAdmitted:
		return false
	default:
		return false
	}
}

func validHandoffTransition(prior, next HandoffState) bool {
	return prior == HandoffIssued && next == HandoffGranted ||
		prior == HandoffGranted && next == HandoffAcknowledged
}

func validRecoveryTransition(prior, next RecoveryState) bool {
	if prior == RecoveryObservationStarted {
		return next == RecoveryCanceled || next == RecoveryFenced || next == RecoveryNotPerformed ||
			next == RecoveryEffectObserved || next == RecoveryAmbiguous || next == RecoveryApplied
	}
	if prior == RecoveryEffectObserved {
		return next == RecoveryApplied || next == RecoveryFenced
	}
	return false
}

func sameTransition(left, right transitionRecord) bool {
	return left.binding == right.binding && bytes.Equal(left.requestPayload, right.requestPayload) &&
		reflect.DeepEqual(left.outcome, right.outcome) && left.receipt.Action == right.receipt.Action &&
		left.receipt.Reservation == right.receipt.Reservation && left.receipt.Outcome == right.receipt.Outcome
}

func canonicalValue(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
	}
	canonical, err := journal.CanonicalJSON(normalized)
	if err != nil {
		return nil, typedError(CodeInvalidRecord, "", ErrInvalidRecord)
	}
	return canonical, nil
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestValue(value any) (string, []byte, error) {
	encoded, err := canonicalValue(value)
	if err != nil {
		return "", nil, err
	}
	return digestBytes(encoded), encoded, nil
}

func internalActionID(binding semanticBinding) string {
	return stableID("admission_action", binding.InstallationID, binding.ControlRunID, binding.ActionID,
		fmt.Sprint(binding.Generation))
}

func internalOutcomeID(binding semanticBinding) string {
	return stableID("admission_outcome", binding.InstallationID, binding.ControlRunID, binding.OutcomeEventID)
}

func internalIdempotencyKey(binding semanticBinding) string {
	return stableID("admission_key", binding.InstallationID, binding.ControlRunID, binding.IdempotencyKey)
}

func stableID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(append([]string{prefix}, values...), "\x00")))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func scopedID(runID, value string) string { return runID + "\x00" + value }

func semanticIndex(runID, actionID string, generation uint64) string {
	return fmt.Sprintf("%s\x00%s\x00%d", runID, actionID, generation)
}

func idempotencyIndex(runID, key string) string { return scopedID(runID, key) }

func recoveryIndex(identity RecoveryIdentity) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d", identity.InstallationID, identity.ControlRunID, identity.ActionID, identity.Generation)
}

func operationKind(operation SemanticOperation) journal.Kind {
	switch operation {
	case OperationAdmissionReserve, OperationQueueEnqueue, OperationQueueAdmit,
		OperationBackpressureDefer, OperationLeaseAcquire, OperationLeaseRenew:
		return journal.KindAllocateResource
	case OperationAdmissionRelease, OperationQueueRelease, OperationLeaseRelease, OperationLeaseExpire:
		return journal.KindDisposeResource
	case OperationStartObservation, OperationObserveEffect, OperationCancelOrFence, OperationScannerApply:
		return journal.KindObserve
	case OperationHandoffIssue, OperationHandoffGrant, OperationHandoffAcknowledge:
		return journal.KindSend
	default:
		return ""
	}
}

func storeError(operation SemanticOperation, err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, journal.ErrConflict):
		return typedError(CodeConflict, operation, ErrConflict)
	case errors.Is(err, journal.ErrNotFound):
		return typedError(CodeNotFound, operation, ErrNotFound)
	case errors.Is(err, journal.ErrInvalidRecord), errors.Is(err, journal.ErrCursor):
		return typedError(CodeInvalidRecord, operation, ErrInvalidRecord)
	default:
		return typedError(CodeStore, operation, ErrInvalidRecord)
	}
}
