package scheduler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/araihu/paje/internal/controlplane/admission"
	"github.com/araihu/paje/internal/controlplane/journal"
)

const (
	admissionComponent    = "admission"
	admissionActionPrefix = "admission_action_"
)

type admissionBinding struct {
	ActionID          string                      `json:"action_id"`
	AttemptID         string                      `json:"attempt_id,omitempty"`
	ControlRunID      string                      `json:"control_run_id"`
	Generation        uint64                      `json:"generation"`
	GraphRevision     uint64                      `json:"graph_revision"`
	IdempotencyKey    string                      `json:"idempotency_key"`
	InstallationID    string                      `json:"installation_id"`
	OutcomeEventID    string                      `json:"outcome_event_id"`
	OutcomeKind       journal.EventKind           `json:"outcome_kind"`
	SemanticOperation admission.SemanticOperation `json:"semantic_operation"`
	SubjectDigest     string                      `json:"subject_digest"`
	TaskID            string                      `json:"task_id,omitempty"`
}

type admissionRequestEnvelope struct {
	Binding           admissionBinding            `json:"binding"`
	Component         string                      `json:"component"`
	PredecessorDigest string                      `json:"predecessor_digest,omitempty"`
	PolicyDigest      string                      `json:"policy_digest,omitempty"`
	PolicyVersion     uint64                      `json:"policy_version,omitempty"`
	SchemaVersion     uint32                      `json:"schema_version"`
	SemanticOperation admission.SemanticOperation `json:"semantic_operation"`
	Subject           json.RawMessage             `json:"subject"`
	SubjectDigest     string                      `json:"subject_digest"`
	SubjectKind       string                      `json:"subject_kind"`
}

type admissionOutcomeEnvelope struct {
	Admission          *admission.RunAdmission       `json:"admission,omitempty"`
	AdmissionTombstone *admission.AdmissionTombstone `json:"admission_tombstone,omitempty"`
	Binding            admissionBinding              `json:"binding"`
	Component          string                        `json:"component"`
	Handoff            *admission.EvidenceHandoff    `json:"handoff,omitempty"`
	Lease              *admission.Lease              `json:"lease,omitempty"`
	LeaseTombstone     *admission.LeaseTombstone     `json:"lease_tombstone,omitempty"`
	PredecessorDigest  string                        `json:"predecessor_digest,omitempty"`
	PolicyDigest       string                        `json:"policy_digest,omitempty"`
	PolicyVersion      uint64                        `json:"policy_version,omitempty"`
	Recovery           *admission.RecoveryRecord     `json:"recovery,omitempty"`
	SchemaVersion      uint32                        `json:"schema_version"`
	SemanticOperation  admission.SemanticOperation   `json:"semantic_operation"`
	Subject            json.RawMessage               `json:"subject"`
	SubjectDigest      string                        `json:"subject_digest"`
	SubjectKind        string                        `json:"subject_kind"`
}

type readyEntry struct {
	current         admission.RunAdmission
	queueReceipt    admission.AdmissionReceipt
	enqueueSequence uint64
	enqueuedAt      time.Time
	attempts        uint64
}

func (entry readyEntry) ready(virtualStart uint64) (ReadyItem, error) {
	weight, err := schedulerWeight(entry.current.PolicyVersion, entry.current.Subject.Primitive)
	if err != nil {
		return ReadyItem{}, err
	}
	return ReadyItem{
		ID: entry.current.Subject.ID, ControlRunID: entry.current.ControlRunID,
		Weight: weight, VirtualStart: virtualStart,
		EnqueueSequence: entry.enqueueSequence, EnqueuedAt: entry.enqueuedAt,
		Admission: admission.AdmissionRequest{
			ControlRunID: entry.current.ControlRunID,
			Subject:      entry.current.Subject,
		},
		QueueReceipt: entry.queueReceipt, Attempt: entry.attempts,
	}, nil
}

type schedulerSnapshot struct {
	installationID   string
	global           journal.GlobalCursor
	runHeads         map[string]uint64
	ready            map[string]readyEntry
	virtualFinish    map[string]uint64
	fairness         FairnessState
	receipts         map[string]admission.AdmissionReceipt
	leaseReceipts    map[string]admission.LeaseReceipt
	recoveryReceipts map[string]admission.RecoveryReceipt
	leases           map[string]admission.Lease
	recoveries       map[string]admission.RecoveryRecord
}

func newSchedulerSnapshot() schedulerSnapshot {
	return schedulerSnapshot{
		runHeads: make(map[string]uint64), ready: make(map[string]readyEntry),
		virtualFinish: make(map[string]uint64), receipts: make(map[string]admission.AdmissionReceipt),
		leaseReceipts:    make(map[string]admission.LeaseReceipt),
		recoveryReceipts: make(map[string]admission.RecoveryReceipt),
		leases:           make(map[string]admission.Lease), recoveries: make(map[string]admission.RecoveryRecord),
	}
}

func (s *Service) rebuildJournal(ctx context.Context) (schedulerSnapshot, error) {
	snapshot := newSchedulerSnapshot()
	cursor := journal.GlobalCursor{}
	reservations := make(map[string]journal.Event)
	var wantPosition journal.JournalPosition = 1
	excluded := make(map[string]bool)
	for {
		events, next, err := s.journal.Feed(ctx, cursor, 1000)
		if err != nil {
			return schedulerSnapshot{}, typedError(CodeAuthority, "journal_feed", err)
		}
		if snapshot.installationID == "" {
			snapshot.installationID = next.InstallationID
		}
		if !bounded(snapshot.installationID, 128) || next.InstallationID != snapshot.installationID ||
			next.SchemaVersion != journal.SchemaVersion {
			return schedulerSnapshot{}, typedError(CodeAuthority, "journal_feed", ErrInvalidRecord)
		}
		for _, event := range events {
			if event.JournalPosition != wantPosition || event.RunSequence != snapshot.runHeads[event.ControlRunID]+1 {
				return schedulerSnapshot{}, typedError(CodeAuthority, "journal_feed", ErrInvalidRecord)
			}
			wantPosition++
			snapshot.runHeads[event.ControlRunID] = event.RunSequence
			key := event.ControlRunID + "\x00" + event.ActionID
			if event.Kind == journal.EventActionReserved {
				if !strings.HasPrefix(event.ActionID, admissionActionPrefix) {
					continue
				}
				if _, exists := reservations[key]; exists {
					return schedulerSnapshot{}, typedError(CodeAuthority, "journal_reservation", ErrInvalidRecord)
				}
				reservations[key] = event
				continue
			}
			if !journal.IsOutcome(event.Kind) {
				continue
			}
			if !strings.HasPrefix(event.ActionID, admissionActionPrefix) {
				continue
			}
			payload, err := s.journal.Payload(ctx, event.PayloadDigest)
			if err != nil || journal.ValidatePayload(payload, event.PayloadDigest) != nil {
				return schedulerSnapshot{}, typedError(CodeAuthority, "journal_payload", ErrInvalidRecord)
			}
			var header struct {
				Component string `json:"component"`
			}
			if err := json.Unmarshal(payload, &header); err != nil {
				return schedulerSnapshot{}, typedError(CodeAuthority, "journal_payload", ErrInvalidRecord)
			}
			if header.Component != admissionComponent {
				return schedulerSnapshot{}, typedError(CodeAuthority, "journal_payload", ErrInvalidRecord)
			}
			reservation, ok := reservations[key]
			if !ok {
				return schedulerSnapshot{}, typedError(CodeAuthority, "journal_membership", ErrInvalidRecord)
			}
			action, err := s.journal.Reservation(ctx, event.ControlRunID, event.ActionID)
			if err != nil {
				return schedulerSnapshot{}, typedError(CodeAuthority, "journal_membership", err)
			}
			requestPayload, err := s.journal.Payload(ctx, action.CanonicalRequestDigest)
			if err != nil || journal.ValidatePayload(requestPayload, action.CanonicalRequestDigest) != nil {
				return schedulerSnapshot{}, typedError(CodeAuthority, "journal_request", ErrInvalidRecord)
			}
			var request admissionRequestEnvelope
			var outcome admissionOutcomeEnvelope
			if journal.DecodeStrict(requestPayload, &request) != nil || journal.DecodeStrict(payload, &outcome) != nil ||
				validateAdmissionMembership(
					snapshot.installationID, action, reservation, event,
					requestPayload, payload, request, outcome,
				) != nil {
				return schedulerSnapshot{}, typedError(CodeAuthority, "journal_membership", ErrInvalidRecord)
			}
			if err := s.applyAdmissionOutcome(&snapshot, action, reservation, event, outcome, excluded); err != nil {
				return schedulerSnapshot{}, err
			}
			delete(reservations, key)
		}
		cursor = next
		snapshot.global = next
		if len(events) == 0 {
			break
		}
	}
	if len(reservations) != 0 {
		return schedulerSnapshot{}, typedError(CodeAuthority, "journal_reservation", ErrInvalidRecord)
	}
	return snapshot, nil
}

func validateAdmissionMembership(
	installationID string,
	action journal.Action,
	reservation, event journal.Event,
	requestPayload, outcomePayload []byte,
	request admissionRequestEnvelope,
	outcome admissionOutcomeEnvelope,
) error {
	binding := request.Binding
	identity := admission.TransitionIdentity{
		ActionID: binding.ActionID, IdempotencyKey: binding.IdempotencyKey,
		OutcomeEventID: binding.OutcomeEventID, OutcomeKind: binding.OutcomeKind,
		GraphRevision: binding.GraphRevision, Generation: binding.Generation,
		TaskID: binding.TaskID, AttemptID: binding.AttemptID,
	}
	subjectPayload := append(append([]byte(nil), request.Subject...), '\n')
	if request.Component != admissionComponent || outcome.Component != admissionComponent ||
		request.SchemaVersion != admission.SchemaVersion || outcome.SchemaVersion != admission.SchemaVersion ||
		request.Binding != outcome.Binding || request.SemanticOperation != outcome.SemanticOperation ||
		request.SemanticOperation != request.Binding.SemanticOperation ||
		request.SubjectKind != outcome.SubjectKind || request.SubjectDigest != outcome.SubjectDigest ||
		request.SubjectDigest != request.Binding.SubjectDigest ||
		request.PolicyVersion != outcome.PolicyVersion || request.PolicyDigest != outcome.PolicyDigest ||
		request.PredecessorDigest != outcome.PredecessorDigest || !bytes.Equal(request.Subject, outcome.Subject) ||
		request.Binding.InstallationID != installationID || request.Binding.ControlRunID != action.ControlRunID ||
		request.Binding.GraphRevision != action.GraphRevision || request.Binding.TaskID != action.TaskID ||
		request.Binding.AttemptID != action.AttemptID || request.Binding.OutcomeKind != event.Kind ||
		!validTransitionIdentity(identity) || binding.OutcomeKind != journal.EventActionResult ||
		action.ID != admissionInternalActionID(binding) ||
		action.IdempotencyKey != admissionInternalIdempotencyKey(binding) ||
		action.AuthorityReceiptID != "" || event.ID != admissionInternalOutcomeID(binding) ||
		event.ActionID != action.ID || event.ControlRunID != action.ControlRunID ||
		reservation.ActionID != action.ID || reservation.ControlRunID != action.ControlRunID ||
		reservation.Kind != journal.EventActionReserved || reservation.RunSequence == 0 ||
		action.ExpectedProjection != reservation.RunSequence-1 || event.RunSequence != reservation.RunSequence+1 ||
		event.JournalPosition != reservation.JournalPosition+1 ||
		action.CanonicalRequestDigest != schedulerDigestBytes(requestPayload) ||
		event.PayloadDigest != schedulerDigestBytes(outcomePayload) ||
		action.Kind != admissionOperationKind(outcome.SemanticOperation) ||
		journal.ValidatePayload(subjectPayload, request.SubjectDigest) != nil ||
		(request.PredecessorDigest != "" && !journal.ValidDigest(request.PredecessorDigest)) ||
		!validAdmissionOutcomeShape(outcome) {
		return ErrInvalidRecord
	}
	if request.SubjectKind == "run_admission" {
		if request.PolicyVersion == 0 || !journal.ValidDigest(request.PolicyDigest) {
			return ErrInvalidRecord
		}
	} else if request.PolicyVersion != 0 || request.PolicyDigest != "" {
		return ErrInvalidRecord
	}
	if outcome.Admission != nil {
		value := outcome.Admission
		if value.ReceiptID != admissionOpaqueReceiptID(
			outcome.SemanticOperation, binding, value.OriginalRequestDigest,
		) || (outcome.SemanticOperation == admission.OperationAdmissionReserve &&
			value.OriginalRequestDigest != schedulerDigestBytes(requestPayload)) {
			return ErrInvalidRecord
		}
	}
	actionDigest, err := journal.Digest(action)
	if err != nil || reservation.PayloadDigest != actionDigest || !journal.ValidDigest(request.SubjectDigest) {
		return ErrInvalidRecord
	}
	return nil
}

func admissionOperationKind(operation admission.SemanticOperation) journal.Kind {
	switch operation {
	case admission.OperationAdmissionReserve, admission.OperationQueueEnqueue,
		admission.OperationQueueAdmit, admission.OperationBackpressureDefer,
		admission.OperationLeaseAcquire, admission.OperationLeaseRenew:
		return journal.KindAllocateResource
	case admission.OperationAdmissionRelease, admission.OperationQueueRelease,
		admission.OperationLeaseRelease, admission.OperationLeaseExpire:
		return journal.KindDisposeResource
	case admission.OperationStartObservation,
		admission.OperationCancelOrFence, admission.OperationScannerApply:
		return journal.KindObserve
	case admission.OperationHandoffIssue, admission.OperationHandoffGrant,
		admission.OperationHandoffAcknowledge:
		return journal.KindSend
	default:
		return ""
	}
}

func validAdmissionOutcomeShape(outcome admissionOutcomeEnvelope) bool {
	count := 0
	for _, present := range []bool{
		outcome.Admission != nil, outcome.AdmissionTombstone != nil, outcome.Handoff != nil,
		outcome.Lease != nil, outcome.LeaseTombstone != nil, outcome.Recovery != nil,
	} {
		if present {
			count++
		}
	}
	if count != 1 {
		return false
	}
	switch outcome.SemanticOperation {
	case admission.OperationAdmissionReserve, admission.OperationQueueEnqueue,
		admission.OperationQueueAdmit, admission.OperationBackpressureDefer:
		return outcome.Admission != nil
	case admission.OperationAdmissionRelease, admission.OperationQueueRelease:
		return outcome.AdmissionTombstone != nil
	case admission.OperationLeaseAcquire, admission.OperationLeaseRenew:
		return outcome.Lease != nil
	case admission.OperationLeaseRelease, admission.OperationLeaseExpire:
		return outcome.LeaseTombstone != nil
	case admission.OperationStartObservation, admission.OperationCancelOrFence,
		admission.OperationScannerApply:
		return outcome.Recovery != nil
	case admission.OperationHandoffIssue, admission.OperationHandoffGrant,
		admission.OperationHandoffAcknowledge:
		return outcome.Handoff != nil
	default:
		return false
	}
}

func admissionInternalActionID(binding admissionBinding) string {
	return admissionStableID(
		"admission_action", binding.InstallationID, binding.ControlRunID,
		binding.ActionID, fmt.Sprint(binding.Generation),
	)
}

func admissionInternalOutcomeID(binding admissionBinding) string {
	return admissionStableID(
		"admission_outcome", binding.InstallationID, binding.ControlRunID, binding.OutcomeEventID,
	)
}

func admissionInternalIdempotencyKey(binding admissionBinding) string {
	return admissionStableID(
		"admission_key", binding.InstallationID, binding.ControlRunID, binding.IdempotencyKey,
	)
}

func admissionStableID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(append([]string{prefix}, values...), "\x00")))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func admissionOpaqueReceiptID(
	operation admission.SemanticOperation,
	binding admissionBinding,
	requestDigest string,
) string {
	return admissionStableID(
		"receipt", string(operation), binding.InstallationID, binding.ControlRunID,
		binding.ActionID, fmt.Sprint(binding.Generation), requestDigest,
	)
}

func schedulerDigestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Service) applyAdmissionOutcome(
	snapshot *schedulerSnapshot,
	action journal.Action,
	reservation, event journal.Event,
	outcome admissionOutcomeEnvelope,
	excluded map[string]bool,
) error {
	operation := outcome.SemanticOperation
	if outcome.Admission != nil {
		value := *outcome.Admission
		var subject admission.AdmissionSubject
		if journal.DecodeStrict(outcome.Subject, &subject) != nil || !validAdmissionSubject(subject) ||
			subject != value.Subject ||
			outcome.SubjectKind != "run_admission" || value.ControlRunID != action.ControlRunID ||
			value.Sequence != 0 || value.GraphRevision != outcome.Binding.GraphRevision ||
			value.Generation != outcome.Binding.Generation || value.PolicyVersion != outcome.PolicyVersion ||
			value.PolicyDigest != outcome.PolicyDigest || !journal.ValidDigest(value.PolicyDigest) ||
			!journal.ValidDigest(value.OriginalRequestDigest) || !bounded(value.ReceiptID, 128) {
			return typedError(CodeAuthority, "admission_outcome", ErrInvalidRecord)
		}
		value.Sequence = uint64(event.JournalPosition)
		receipt := admission.AdmissionReceipt{
			Commit:    journal.CommitReceipt{Action: action, Reservation: reservation, Outcome: event, Created: true},
			Operation: operation, Admission: value, Created: true,
		}
		key := admissionKey(value.ControlRunID, value.Subject.ID)
		switch operation {
		case admission.OperationAdmissionReserve:
			if value.State != admission.AdmissionReserved || value.LimitingQuota != admission.QuotaNone {
				return typedError(CodeAuthority, "admission_reserve", ErrInvalidRecord)
			}
		case admission.OperationQueueEnqueue:
			if value.State != admission.AdmissionQueued || value.LimitingQuota != admission.QuotaNone {
				return typedError(CodeAuthority, "queue_enqueue", ErrInvalidRecord)
			}
			if _, exists := snapshot.ready[key]; exists {
				return typedError(CodeAuthority, "queue_enqueue", ErrInvalidRecord)
			}
			snapshot.ready[key] = readyEntry{
				current: value, queueReceipt: receipt,
				enqueueSequence: uint64(event.JournalPosition), enqueuedAt: event.OccurredAt.UTC(),
			}
		case admission.OperationBackpressureDefer:
			entry, exists := snapshot.ready[key]
			if !exists || entry.current.Subject != value.Subject || value.State != admission.AdmissionDeferred ||
				value.LimitingQuota == admission.QuotaNone {
				return typedError(CodeAuthority, "backpressure", ErrInvalidRecord)
			}
			if err := s.verifyHistoricalChoice(*snapshot, key, event.OccurredAt.UTC(), excluded); err != nil {
				return err
			}
			receipt.Backpressure = &admission.BackpressureReceipt{
				LimitingQuota: value.LimitingQuota, NextEligibility: admission.EligibilityQuotaAvailable,
				PolicyVersion: value.PolicyVersion, PolicyDigest: value.PolicyDigest,
			}
			entry.current, entry.queueReceipt = value, receipt
			entry.attempts = admission.SaturatingAdd(entry.attempts, 1)
			snapshot.ready[key] = entry
			excluded[key] = true
		case admission.OperationQueueAdmit:
			entry, exists := snapshot.ready[key]
			if !exists || entry.current.Subject != value.Subject || value.State != admission.AdmissionAdmitted ||
				value.LimitingQuota != admission.QuotaNone {
				return typedError(CodeAuthority, "queue_admit", ErrInvalidRecord)
			}
			selected, nextState, err := s.historicalSelection(*snapshot, event.OccurredAt.UTC(), excluded)
			if err != nil || admissionKey(selected.Item.ControlRunID, selected.Item.ID) != key {
				return typedError(CodeAuthority, "queue_admit", ErrInvalidRecord)
			}
			snapshot.virtualFinish[value.ControlRunID] = selected.VirtualFinish
			snapshot.fairness = nextState
			delete(snapshot.ready, key)
			clear(excluded)
		default:
			return typedError(CodeAuthority, "admission_outcome", ErrInvalidRecord)
		}
		snapshot.receipts[event.ID] = receipt
		return nil
	}
	if outcome.AdmissionTombstone != nil {
		value := *outcome.AdmissionTombstone
		if operation != admission.OperationAdmissionRelease && operation != admission.OperationQueueRelease ||
			value.ControlRunID != action.ControlRunID || value.Subject.ID == "" {
			return typedError(CodeAuthority, "admission_tombstone", ErrInvalidRecord)
		}
		delete(snapshot.ready, admissionKey(value.ControlRunID, value.Subject.ID))
		return nil
	}
	if outcome.Lease != nil {
		value := *outcome.Lease
		if value.ControlRunID != action.ControlRunID || value.Subject.ID == "" || value.State != admission.LeaseActive {
			return typedError(CodeAuthority, "lease_outcome", ErrInvalidRecord)
		}
		snapshot.leases[admissionKey(value.ControlRunID, value.Subject.ID)] = value
		snapshot.leaseReceipts[event.ID] = admission.LeaseReceipt{
			Commit:    journal.CommitReceipt{Action: action, Reservation: reservation, Outcome: event, Created: true},
			Operation: operation, Lease: value, Created: true,
		}
		return nil
	}
	if outcome.LeaseTombstone != nil {
		value := *outcome.LeaseTombstone
		delete(snapshot.leases, admissionKey(value.ControlRunID, value.Subject.ID))
		snapshot.leaseReceipts[event.ID] = admission.LeaseReceipt{
			Commit:    journal.CommitReceipt{Action: action, Reservation: reservation, Outcome: event, Created: true},
			Operation: operation,
			Lease: admission.Lease{
				ControlRunID: value.ControlRunID, Subject: value.Subject, State: value.State,
				IssuedAt: value.IssuedAt, ExpiresAt: value.ExpiresAt, Generation: value.Generation,
				GraphRevision: value.GraphRevision, OriginalRequestDigest: value.OriginalRequestDigest,
				ReceiptID: value.TerminalReceiptID,
			},
			Tombstone: &value, Created: true,
		}
		return nil
	}
	if outcome.Recovery != nil {
		value := *outcome.Recovery
		if value.Identity.InstallationID != snapshot.installationID || value.Identity.ControlRunID != action.ControlRunID {
			return typedError(CodeAuthority, "recovery_outcome", ErrInvalidRecord)
		}
		snapshot.recoveries[recoveryKey(value.Identity)] = value
		snapshot.recoveryReceipts[event.ID] = admission.RecoveryReceipt{
			Commit:    journal.CommitReceipt{Action: action, Reservation: reservation, Outcome: event, Created: true},
			Operation: operation, Recovery: value,
			RetryAllowed: value.State == admission.RecoveryNotPerformed || value.State == admission.RecoveryCanceled,
			Created:      true,
		}
		return nil
	}
	// Evidence handoffs are authoritative admission records but irrelevant to scheduling.
	if outcome.Handoff != nil {
		return nil
	}
	return typedError(CodeAuthority, "admission_outcome", ErrInvalidRecord)
}

func validAdmissionSubject(subject admission.AdmissionSubject) bool {
	return bounded(subject.ID, 128) && bounded(subject.PrincipalID, 128) &&
		bounded(subject.ProjectID, 256) && bounded(subject.Primitive, 64) && bounded(subject.WorkID, 128)
}

func (s *Service) verifyHistoricalChoice(
	snapshot schedulerSnapshot,
	wantKey string,
	now time.Time,
	excluded map[string]bool,
) error {
	selected, _, err := s.historicalSelection(snapshot, now, excluded)
	if err == nil && admissionKey(selected.Item.ControlRunID, selected.Item.ID) == wantKey {
		return nil
	}
	if len(excluded) > 0 {
		selected, _, err = s.historicalSelection(snapshot, now, nil)
		if err == nil && admissionKey(selected.Item.ControlRunID, selected.Item.ID) == wantKey {
			clear(excluded)
			return nil
		}
	}
	return typedError(CodeAuthority, "historical_fairness", ErrInvalidRecord)
}

func (s *Service) historicalSelection(
	snapshot schedulerSnapshot,
	now time.Time,
	excluded map[string]bool,
) (RankedItem, FairnessState, error) {
	items := make([]ReadyItem, 0, len(snapshot.ready))
	for key, entry := range snapshot.ready {
		if excluded[key] {
			continue
		}
		item, err := entry.ready(snapshot.virtualFinish[entry.current.ControlRunID])
		if err != nil {
			return RankedItem{}, snapshot.fairness, err
		}
		items = append(items, item)
	}
	return selectReady(items, snapshot.fairness, now, s.policy)
}

func schedulerWeight(policyVersion uint64, primitive string) (uint64, error) {
	if policyVersion != 1 || strings.TrimSpace(primitive) == "" {
		return 0, typedError(CodeInvalidPolicy, "weight", ErrInvalidPolicy)
	}
	switch primitive {
	case "persistent_session":
		return 1, nil
	case "ephemeral_subagent":
		return 2, nil
	case "harness_native_parallel":
		return 4, nil
	case "local_sequential":
		return 8, nil
	default:
		return 1, nil
	}
}

func (s *Service) Ready(ctx context.Context) ([]RankedItem, error) {
	snapshot, err := s.rebuildJournal(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]ReadyItem, 0, len(snapshot.ready))
	for _, entry := range snapshot.ready {
		item, err := entry.ready(snapshot.virtualFinish[entry.current.ControlRunID])
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return rankReady(items, s.clock().UTC(), s.policy)
}

func (s *Service) Enqueue(
	ctx context.Context,
	request admission.AdmissionRequest,
) (admission.AdmissionReceipt, error) {
	receipt, err := s.authority.Enqueue(ctx, request)
	if err != nil {
		return admission.AdmissionReceipt{}, typedError(CodeAuthority, "enqueue", err)
	}
	snapshot, err := s.rebuildJournal(ctx)
	if err != nil {
		return admission.AdmissionReceipt{}, err
	}
	stored, ok := snapshot.receipts[receipt.Commit.Outcome.ID]
	if !ok || !sameAdmissionReceipt(stored, receipt) || stored.Operation != admission.OperationQueueEnqueue {
		return admission.AdmissionReceipt{}, typedError(CodeAuthority, "enqueue", ErrInvalidRecord)
	}
	return receipt, nil
}

func (s *Service) AdmitNext(ctx context.Context) (AdmissionDecision, error) {
	decision := AdmissionDecision{}
	excluded := make(map[string]bool)
	for {
		snapshot, err := s.rebuildJournal(ctx)
		if err != nil {
			return AdmissionDecision{}, err
		}
		selected, _, err := s.historicalSelection(snapshot, s.clock().UTC(), excluded)
		if err != nil {
			if errors.Is(err, ErrNoEligible) && len(decision.Backpressure) > 0 {
				decision.State = snapshot.fairness
				return decision, nil
			}
			return AdmissionDecision{}, err
		}
		key := admissionKey(selected.Item.ControlRunID, selected.Item.ID)
		entry := snapshot.ready[key]
		request := selected.Item.Admission
		request.Identity = schedulerDecisionIdentity(entry)
		receipt, err := s.authority.Admit(ctx, request)
		if err != nil {
			return AdmissionDecision{}, typedError(CodeAuthority, "admit", err)
		}
		after, err := s.rebuildJournal(ctx)
		if err != nil {
			return AdmissionDecision{}, err
		}
		stored, ok := after.receipts[receipt.Commit.Outcome.ID]
		if !ok || !sameAdmissionReceipt(stored, receipt) || receipt.Admission.ControlRunID != selected.Item.ControlRunID ||
			receipt.Admission.Subject != selected.Item.Admission.Subject {
			return AdmissionDecision{}, typedError(CodeAuthority, "admit", ErrInvalidRecord)
		}
		switch receipt.Operation {
		case admission.OperationQueueAdmit:
			decision.Admitted, decision.Item, decision.Receipt = true, selected, receipt
			decision.State = after.fairness
			return decision, nil
		case admission.OperationBackpressureDefer:
			if receipt.Backpressure == nil {
				return AdmissionDecision{}, typedError(CodeAuthority, "admit", ErrInvalidRecord)
			}
			decision.Backpressure = append(decision.Backpressure, BackpressureDecision{
				ItemID: selected.Item.ID, ControlRunID: selected.Item.ControlRunID,
				Receipt: *receipt.Backpressure,
			})
			excluded[key] = true
		default:
			return AdmissionDecision{}, typedError(CodeAuthority, "admit", ErrInvalidRecord)
		}
	}
}

func sameAdmissionReceipt(stored, got admission.AdmissionReceipt) bool {
	stored.Created, got.Created = false, false
	stored.Commit.Created, got.Commit.Created = false, false
	return reflect.DeepEqual(stored, got)
}

func schedulerDecisionIdentity(entry readyEntry) admission.TransitionIdentity {
	attempt := admission.SaturatingAdd(entry.attempts, 1)
	seed := strings.Join([]string{
		entry.current.ControlRunID, entry.current.Subject.ID, entry.current.ReceiptID,
		fmt.Sprintf("%d", attempt),
	}, "\x00")
	sum := sha256.Sum256([]byte(seed))
	token := hex.EncodeToString(sum[:])
	return admission.TransitionIdentity{
		ActionID:       "scheduler-admit-" + token,
		IdempotencyKey: "scheduler-admit-" + token,
		OutcomeEventID: "scheduler-admit-result-" + token,
		OutcomeKind:    journal.EventActionResult,
		GraphRevision:  entry.current.GraphRevision,
		Generation:     admission.SaturatingAdd(entry.current.Generation, 1),
	}
}

func admissionKey(runID, itemID string) string { return runID + "\x00" + itemID }

func recoveryKey(identity admission.RecoveryIdentity) string {
	return strings.Join([]string{
		identity.InstallationID, identity.ControlRunID, identity.ActionID,
		fmt.Sprintf("%d", identity.Generation),
	}, "\x00")
}

func sortedReadyKeys(values map[string]readyEntry) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
