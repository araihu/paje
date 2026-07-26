package isolation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/araihu/paje/internal/controlplane/journal"
)

const isolationSchemaVersion uint32 = 1

const isolationActionPrefix = "isolation_action_"

const (
	operationTaskPhase      = "isolation.task_phase"
	operationRunPhase       = "isolation.run_phase"
	operationInboxAppend    = "isolation.inbox_append"
	operationInboxAck       = "isolation.inbox_acknowledge"
	operationGateRegister   = "isolation.gate_register"
	operationGateWake       = "isolation.gate_wake"
	maximumCommitCASRetries = 64
)

type Service struct {
	store          journal.AuthoritativeStore
	installationID string
	now            func() time.Time
}

func New(
	store journal.AuthoritativeStore,
	installationID string,
	now func() time.Time,
) (*Service, error) {
	if store == nil || !validIdentifier(installationID, 128) || now == nil {
		return nil, ErrInvalidOperation
	}
	return &Service{store: store, installationID: installationID, now: now}, nil
}

func (s *Service) Current(ctx context.Context, scope RunScope) (Projection, error) {
	projection, _, err := s.rebuild(ctx, scope)
	return projection, err
}

func (s *Service) Observe(
	ctx context.Context,
	request ObservationRequest,
) (Projection, error) {
	if err := s.validateScope(request.Scope); err != nil ||
		!validObservationSource(request.Source) ||
		strings.TrimSpace(request.Status) != request.Status ||
		request.Status == "" || len(request.Status) > 256 {
		return Projection{}, ErrInvalidOperation
	}
	return s.Current(ctx, request.Scope)
}

func (s *Service) TransitionTask(
	ctx context.Context,
	request TaskPhaseRequest,
) (CommitResult, error) {
	if err := s.validateCommon(
		request.Scope,
		request.OperationID,
		request.GraphRevision,
		request.TaskID,
		request.AttemptID,
	); err != nil || !validTaskPhase(request.To) ||
		(request.From != "" && !validTaskPhase(request.From)) {
		return CommitResult{}, ErrInvalidOperation
	}
	delta := taskPhaseDelta{
		TaskID: request.TaskID, AttemptID: request.AttemptID,
		From: request.From, To: request.To,
	}
	return s.commit(ctx, operationSpec{
		scope: request.Scope, operationID: request.OperationID,
		graphRevision: request.GraphRevision, expectedRevision: request.ExpectedRevision,
		kind: journal.KindObserve, taskID: request.TaskID, attemptID: request.AttemptID,
		request: delta,
		outcome: outcomeEnvelope{
			SchemaVersion: isolationSchemaVersion, SemanticOperation: operationTaskPhase,
			Scope: request.Scope, TaskPhase: &delta,
		},
		validate: func(projection Projection) error {
			if projection.RunPhase == RunFrozenSecurity {
				return ErrInvalidTransition
			}
			return validateTaskTransition(projection, delta)
		},
	})
}

func (s *Service) TransitionRun(
	ctx context.Context,
	request RunPhaseRequest,
) (CommitResult, error) {
	if err := s.validateCommon(
		request.Scope,
		request.OperationID,
		request.GraphRevision,
		"",
		"",
	); err != nil || !validRunPhase(request.From) || !validRunPhase(request.To) ||
		!validIdentifier(request.Authority, 128) {
		return CommitResult{}, ErrInvalidOperation
	}
	delta := runPhaseDelta{From: request.From, To: request.To, Authority: request.Authority}
	return s.commit(ctx, operationSpec{
		scope: request.Scope, operationID: request.OperationID,
		graphRevision: request.GraphRevision, expectedRevision: request.ExpectedRevision,
		kind: journal.KindObserve, request: delta,
		outcome: outcomeEnvelope{
			SchemaVersion: isolationSchemaVersion, SemanticOperation: operationRunPhase,
			Scope: request.Scope, RunPhase: &delta,
		},
		validate: func(projection Projection) error {
			return validateRunTransition(projection, delta)
		},
	})
}

func (s *Service) AppendCallback(
	ctx context.Context,
	request CallbackRequest,
) (CommitResult, error) {
	if err := s.validateCommon(
		request.Scope,
		request.OperationID,
		request.GraphRevision,
		request.TaskID,
		request.AttemptID,
	); err != nil || !validIdentifier(request.EventID, 128) ||
		!validIdentifier(request.CorrelationID, 128) ||
		!validIdentifier(request.ExternalActionID, 128) || request.ActionGeneration == 0 ||
		!validIdentifier(request.Producer, 128) || !validIdentifier(request.Consumer, 128) ||
		!journal.ValidDigest(request.PayloadDigest) {
		return CommitResult{}, ErrInvalidOperation
	}
	delta := inboxAppendDelta{
		EventID: request.EventID, CorrelationID: request.CorrelationID,
		TaskID: request.TaskID, AttemptID: request.AttemptID,
		ExternalActionID: request.ExternalActionID, ActionGeneration: request.ActionGeneration,
		Producer: request.Producer, Consumer: request.Consumer, PayloadDigest: request.PayloadDigest,
	}
	return s.commit(ctx, operationSpec{
		scope: request.Scope, operationID: request.OperationID,
		graphRevision: request.GraphRevision, expectedRevision: request.ExpectedRevision,
		kind: journal.KindCallback, taskID: request.TaskID, attemptID: request.AttemptID,
		request: delta,
		outcome: outcomeEnvelope{
			SchemaVersion: isolationSchemaVersion, SemanticOperation: operationInboxAppend,
			Scope: request.Scope, InboxAppend: &delta,
		},
		validate: func(projection Projection) error {
			if _, exists := projection.InboxItem(delta.EventID); exists {
				return journal.ErrConflict
			}
			return nil
		},
	})
}

func (s *Service) AcknowledgeInbox(
	ctx context.Context,
	request AcknowledgeRequest,
) (CommitResult, error) {
	if err := s.validateCommon(
		request.Scope,
		request.OperationID,
		request.GraphRevision,
		"",
		"",
	); err != nil || !validIdentifier(request.EventID, 128) ||
		!validIdentifier(request.Consumer, 128) || !validIdentifier(request.ReceiptID, 128) {
		return CommitResult{}, ErrInvalidOperation
	}
	delta := inboxAckDelta{
		EventID: request.EventID, Consumer: request.Consumer, ReceiptID: request.ReceiptID,
	}
	return s.commit(ctx, operationSpec{
		scope: request.Scope, operationID: request.OperationID,
		graphRevision: request.GraphRevision, expectedRevision: request.ExpectedRevision,
		kind: journal.KindAcknowledge, request: delta,
		outcome: outcomeEnvelope{
			SchemaVersion: isolationSchemaVersion, SemanticOperation: operationInboxAck,
			Scope: request.Scope, InboxAck: &delta,
		},
		validate: func(projection Projection) error {
			item, exists := projection.InboxItem(delta.EventID)
			if !exists {
				return ErrNotFound
			}
			if item.Consumer != delta.Consumer {
				return ErrResolverAuthority
			}
			if item.AcknowledgementReceipt != "" {
				return journal.ErrConflict
			}
			return nil
		},
	})
}

func (s *Service) RegisterGate(
	ctx context.Context,
	request RegisterGateRequest,
) (CommitResult, error) {
	gate := request.Gate
	gate.WakeAt = gate.WakeAt.UTC()
	if err := s.validateCommon(
		request.Scope,
		request.OperationID,
		request.GraphRevision,
		gate.TaskID,
		"",
	); err != nil || gate.State != "" || gate.WakeReceipt != "" ||
		validateGateDefinition(gate) != nil {
		return CommitResult{}, ErrInvalidOperation
	}
	gate.State = GatePending
	delta := gateRegisterDelta{Gate: gate}
	return s.commit(ctx, operationSpec{
		scope: request.Scope, operationID: request.OperationID,
		graphRevision: request.GraphRevision, expectedRevision: request.ExpectedRevision,
		kind: journal.KindRunGate, taskID: gate.TaskID, request: delta,
		outcome: outcomeEnvelope{
			SchemaVersion: isolationSchemaVersion, SemanticOperation: operationGateRegister,
			Scope: request.Scope, GateRegister: &delta,
		},
		validate: func(projection Projection) error {
			task, exists := projection.Task(gate.TaskID)
			if !exists {
				return ErrNotFound
			}
			if task.Phase != TaskDeferred {
				return ErrInvalidTransition
			}
			if _, exists := projection.Gate(gate.ID); exists {
				return journal.ErrConflict
			}
			return nil
		},
	})
}

func (s *Service) WakeGate(
	ctx context.Context,
	request WakeGateRequest,
) (CommitResult, error) {
	request.WakeTime = request.WakeTime.UTC()
	if err := s.validateCommon(
		request.Scope,
		request.OperationID,
		request.GraphRevision,
		"",
		"",
	); err != nil || !validIdentifier(request.GateID, 128) ||
		!validIdentifier(request.ResolverAuthority, 128) ||
		(request.WakeEventID != "" && !validIdentifier(request.WakeEventID, 128)) {
		return CommitResult{}, ErrInvalidOperation
	}
	delta := gateWakeDelta{
		GateID: request.GateID, ResolverAuthority: request.ResolverAuthority,
		WakeEventID: request.WakeEventID, WakeTime: request.WakeTime,
		WakeReceipt: scopedID(
			"gate_wake",
			request.Scope.Key(),
			request.GateID,
			request.ResolverAuthority,
			request.WakeEventID,
			request.WakeTime.Format(time.RFC3339Nano),
		),
	}
	return s.commit(ctx, operationSpec{
		scope: request.Scope, operationID: request.OperationID,
		graphRevision: request.GraphRevision, expectedRevision: request.ExpectedRevision,
		kind: journal.KindRunGate, request: delta,
		outcome: outcomeEnvelope{
			SchemaVersion: isolationSchemaVersion, SemanticOperation: operationGateWake,
			Scope: request.Scope, GateWake: &delta,
		},
		validate: func(projection Projection) error {
			if projection.RunPhase == RunFrozenSecurity {
				return ErrInvalidTransition
			}
			return s.validateWake(projection, delta)
		},
	})
}

func (s *Service) validateWake(projection Projection, delta gateWakeDelta) error {
	gate, exists := projection.Gate(delta.GateID)
	if !exists {
		return ErrNotFound
	}
	if gate.State == GateResolved {
		return ErrGateResolved
	}
	if gate.ResolverAuthority != delta.ResolverAuthority {
		return ErrResolverAuthority
	}
	if gate.Kind == GateTimeNotBefore {
		if delta.WakeEventID != "" || delta.WakeTime.IsZero() ||
			!delta.WakeTime.Equal(gate.WakeAt) {
			return ErrWakeFactMissing
		}
		if s.now().UTC().Before(gate.WakeAt) {
			return ErrWakeNotReady
		}
		return nil
	}
	if !delta.WakeTime.IsZero() || delta.WakeEventID != gate.WakeEventID {
		return ErrWakeFactMissing
	}
	if _, exists := projection.InboxItem(delta.WakeEventID); !exists {
		return ErrWakeFactMissing
	}
	return nil
}

func (s *Service) commit(ctx context.Context, spec operationSpec) (CommitResult, error) {
	requestPayload, requestDigest, err := canonicalPayload(requestEnvelope{
		SchemaVersion:     isolationSchemaVersion,
		SemanticOperation: spec.outcome.SemanticOperation,
		Scope:             spec.scope,
		OperationID:       spec.operationID,
		GraphRevision:     spec.graphRevision,
		ExpectedRevision:  spec.expectedRevision,
		Input:             spec.request,
	})
	if err != nil {
		return CommitResult{}, err
	}
	outcomePayload, outcomeDigest, err := canonicalPayload(spec.outcome)
	if err != nil {
		return CommitResult{}, err
	}
	actionID := scopedID("isolation_action", spec.scope.Key(), spec.outcome.SemanticOperation, spec.operationID)
	action := journal.Action{
		ID: actionID, ControlRunID: spec.scope.ControlRunID,
		TaskID: spec.taskID, AttemptID: spec.attemptID, Kind: spec.kind,
		GraphRevision: spec.graphRevision, ExpectedProjection: spec.expectedRevision,
		CanonicalRequestDigest: requestDigest,
		IdempotencyKey: scopedID(
			"isolation_key", spec.scope.Key(), spec.outcome.SemanticOperation, spec.operationID,
		),
	}
	outcome := journal.Event{
		ID:           scopedID("isolation_event", spec.scope.Key(), spec.outcome.SemanticOperation, spec.operationID),
		ControlRunID: spec.scope.ControlRunID, ActionID: actionID,
		Kind: journal.EventActionResult, PayloadDigest: outcomeDigest,
		OccurredAt: time.Unix(0, 0).UTC(),
	}

	for range maximumCommitCASRetries {
		projection, runCursor, err := s.rebuild(ctx, spec.scope)
		if err != nil {
			return CommitResult{}, err
		}
		globalCursor, err := s.globalCursor(ctx)
		if err != nil {
			return CommitResult{}, err
		}
		existing, reservationErr := s.store.Reservation(ctx, spec.scope.ControlRunID, action.ID)
		exactReplay := reservationErr == nil && existing == action
		if reservationErr == nil && existing != action {
			return CommitResult{}, journal.ErrConflict
		}
		if reservationErr != nil && !errors.Is(reservationErr, journal.ErrNotFound) {
			return CommitResult{}, reservationErr
		}
		if !exactReplay {
			if projection.Revision != spec.expectedRevision {
				return CommitResult{}, ErrRevisionConflict
			}
			if err := spec.validate(projection); err != nil {
				return CommitResult{}, err
			}
		}
		receipt, err := s.store.Commit(ctx, journal.CommitRequest{
			Action: action, ExpectedRun: runCursor, ExpectedGlobal: globalCursor,
			RequestPayload: requestPayload, Outcome: outcome, OutcomePayload: outcomePayload,
		})
		if errors.Is(err, journal.ErrConflict) && !exactReplay {
			continue
		}
		if err != nil {
			return CommitResult{}, err
		}
		projection, _, err = s.rebuild(ctx, spec.scope)
		if err != nil {
			return CommitResult{}, err
		}
		return CommitResult{Projection: projection, Receipt: receipt}, nil
	}
	return CommitResult{}, journal.ErrConflict
}

func canonicalPayload(value any) ([]byte, string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, "", fmt.Errorf("%w: encode isolation payload", journal.ErrInvalidRecord)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, "", fmt.Errorf("%w: decode isolation payload", journal.ErrInvalidRecord)
	}
	encoded, err := journal.CanonicalJSON(generic)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(encoded)
	return encoded, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (s *Service) rebuild(
	ctx context.Context,
	scope RunScope,
) (Projection, journal.RunCursor, error) {
	if err := s.validateScope(scope); err != nil {
		return Projection{}, journal.RunCursor{}, err
	}
	projection := Projection{Scope: scope, RunPhase: RunActive}
	cursor := journal.NewRunCursor(s.installationID, scope.ControlRunID)
	for {
		events, next, err := s.store.RunEvents(ctx, cursor, 1000)
		if err != nil {
			return Projection{}, cursor, err
		}
		for _, event := range events {
			projection.LastRunSequence = event.RunSequence
			projection.LastJournalPosition = event.JournalPosition
			if event.Kind != journal.EventActionResult ||
				!strings.HasPrefix(event.ActionID, isolationActionPrefix) {
				continue
			}
			if err := s.applyOutcome(ctx, &projection, event); err != nil {
				return Projection{}, cursor, err
			}
		}
		cursor = next
		if len(events) == 0 {
			break
		}
	}
	sort.Slice(projection.Tasks, func(i, j int) bool {
		return projection.Tasks[i].TaskID < projection.Tasks[j].TaskID
	})
	sort.Slice(projection.Gates, func(i, j int) bool {
		return projection.Gates[i].ID < projection.Gates[j].ID
	})
	return projection, cursor, nil
}

func (s *Service) applyOutcome(
	ctx context.Context,
	projection *Projection,
	event journal.Event,
) error {
	outcomePayload, err := s.store.Payload(ctx, event.PayloadDigest)
	if err != nil {
		return err
	}
	if err := journal.ValidatePayload(outcomePayload, event.PayloadDigest); err != nil {
		return err
	}
	var envelope outcomeEnvelope
	if err := journal.DecodeStrict(outcomePayload, &envelope); err != nil {
		return err
	}
	if envelope.SchemaVersion != isolationSchemaVersion || envelope.Scope != projection.Scope {
		return journal.ErrInvalidRecord
	}
	action, err := s.store.Reservation(ctx, projection.Scope.ControlRunID, event.ActionID)
	if err != nil {
		return err
	}
	if action.ExpectedProjection != projection.Revision ||
		action.ControlRunID != projection.Scope.ControlRunID {
		return journal.ErrConflict
	}
	requestPayload, err := s.store.Payload(ctx, action.CanonicalRequestDigest)
	if err != nil {
		return err
	}
	if err := journal.ValidatePayload(requestPayload, action.CanonicalRequestDigest); err != nil {
		return err
	}
	var request replayRequestEnvelope
	if err := journal.DecodeStrict(requestPayload, &request); err != nil {
		return err
	}
	if err := validateReplayBinding(request, envelope, action, event, *projection); err != nil {
		return err
	}
	switch envelope.SemanticOperation {
	case operationTaskPhase:
		if err := validateTaskTransition(*projection, *envelope.TaskPhase); err != nil {
			return err
		}
		applyTaskPhase(projection, *envelope.TaskPhase)
	case operationRunPhase:
		if err := validateRunTransition(*projection, *envelope.RunPhase); err != nil {
			return err
		}
		projection.RunPhase = envelope.RunPhase.To
		if envelope.RunPhase.From == RunFrozenSecurity && envelope.RunPhase.To == RunActive {
			projection.RunPhase = deriveRunPhase(*projection)
		}
	case operationInboxAppend:
		if _, exists := projection.InboxItem(envelope.InboxAppend.EventID); exists {
			return journal.ErrConflict
		}
		projection.Inbox = append(projection.Inbox, InboxItem{
			JournalPosition: event.JournalPosition, RunSequence: event.RunSequence,
			EventID:       envelope.InboxAppend.EventID,
			CorrelationID: envelope.InboxAppend.CorrelationID,
			TaskID:        envelope.InboxAppend.TaskID, AttemptID: envelope.InboxAppend.AttemptID,
			ExternalActionID: envelope.InboxAppend.ExternalActionID,
			ActionGeneration: envelope.InboxAppend.ActionGeneration,
			Producer:         envelope.InboxAppend.Producer, Consumer: envelope.InboxAppend.Consumer,
			PayloadDigest: envelope.InboxAppend.PayloadDigest,
		})
	case operationInboxAck:
		if err := applyInboxAck(projection, *envelope.InboxAck); err != nil {
			return err
		}
	case operationGateRegister:
		if err := applyGateRegister(projection, *envelope.GateRegister); err != nil {
			return err
		}
	case operationGateWake:
		if err := applyGateWake(projection, *envelope.GateWake); err != nil {
			return err
		}
	default:
		return journal.ErrInvalidRecord
	}
	projection.Revision++
	return nil
}

func (s *Service) globalCursor(ctx context.Context) (journal.GlobalCursor, error) {
	cursor := journal.NewGlobalCursor(s.installationID)
	for {
		events, next, err := s.store.Feed(ctx, cursor, 1000)
		if err != nil {
			return cursor, err
		}
		cursor = next
		if len(events) == 0 {
			return cursor, nil
		}
	}
}

func (s *Service) validateScope(scope RunScope) error {
	if err := validateRunScope(scope); err != nil || scope.InstallationID != s.installationID {
		return ErrInvalidScope
	}
	return nil
}

func (s *Service) validateCommon(
	scope RunScope,
	operationID string,
	graphRevision uint64,
	taskID, attemptID string,
) error {
	if err := s.validateScope(scope); err != nil ||
		!validIdentifier(operationID, 128) || graphRevision == 0 ||
		(taskID != "" && !validIdentifier(taskID, 128)) ||
		(attemptID != "" && !validIdentifier(attemptID, 128)) ||
		(attemptID != "" && taskID == "") {
		return ErrInvalidOperation
	}
	return nil
}

type operationSpec struct {
	scope            RunScope
	operationID      string
	graphRevision    uint64
	expectedRevision uint64
	kind             journal.Kind
	taskID           string
	attemptID        string
	request          any
	outcome          outcomeEnvelope
	validate         func(Projection) error
}

type requestEnvelope struct {
	SchemaVersion     uint32   `json:"schema_version"`
	SemanticOperation string   `json:"semantic_operation"`
	Scope             RunScope `json:"scope"`
	OperationID       string   `json:"operation_id"`
	GraphRevision     uint64   `json:"graph_revision"`
	ExpectedRevision  uint64   `json:"expected_revision"`
	Input             any      `json:"input"`
}

type replayRequestEnvelope struct {
	SchemaVersion     uint32          `json:"schema_version"`
	SemanticOperation string          `json:"semantic_operation"`
	Scope             RunScope        `json:"scope"`
	OperationID       string          `json:"operation_id"`
	GraphRevision     uint64          `json:"graph_revision"`
	ExpectedRevision  uint64          `json:"expected_revision"`
	Input             json.RawMessage `json:"input"`
}

type outcomeEnvelope struct {
	SchemaVersion     uint32             `json:"schema_version"`
	SemanticOperation string             `json:"semantic_operation"`
	Scope             RunScope           `json:"scope"`
	TaskPhase         *taskPhaseDelta    `json:"task_phase,omitempty"`
	RunPhase          *runPhaseDelta     `json:"run_phase,omitempty"`
	InboxAppend       *inboxAppendDelta  `json:"inbox_append,omitempty"`
	InboxAck          *inboxAckDelta     `json:"inbox_ack,omitempty"`
	GateRegister      *gateRegisterDelta `json:"gate_register,omitempty"`
	GateWake          *gateWakeDelta     `json:"gate_wake,omitempty"`
}

type taskPhaseDelta struct {
	TaskID    string    `json:"task_id"`
	AttemptID string    `json:"attempt_id,omitempty"`
	From      TaskPhase `json:"from,omitempty"`
	To        TaskPhase `json:"to"`
}

type runPhaseDelta struct {
	From      RunPhase `json:"from"`
	To        RunPhase `json:"to"`
	Authority string   `json:"authority"`
}

type inboxAppendDelta struct {
	EventID          string `json:"event_id"`
	CorrelationID    string `json:"correlation_id"`
	TaskID           string `json:"task_id"`
	AttemptID        string `json:"attempt_id"`
	ExternalActionID string `json:"external_action_id"`
	ActionGeneration uint64 `json:"action_generation"`
	Producer         string `json:"producer"`
	Consumer         string `json:"consumer"`
	PayloadDigest    string `json:"payload_digest"`
}

type inboxAckDelta struct {
	EventID   string `json:"event_id"`
	Consumer  string `json:"consumer"`
	ReceiptID string `json:"receipt_id"`
}

type gateRegisterDelta struct {
	Gate PendingWorkGate `json:"gate"`
}

type gateWakeDelta struct {
	GateID            string    `json:"gate_id"`
	ResolverAuthority string    `json:"resolver_authority"`
	WakeEventID       string    `json:"wake_event_id,omitempty"`
	WakeTime          time.Time `json:"wake_time,omitempty"`
	WakeReceipt       string    `json:"wake_receipt"`
}

func validateReplayBinding(
	request replayRequestEnvelope,
	outcome outcomeEnvelope,
	action journal.Action,
	event journal.Event,
	projection Projection,
) error {
	if request.SchemaVersion != isolationSchemaVersion ||
		request.SemanticOperation != outcome.SemanticOperation ||
		request.Scope != outcome.Scope || request.Scope != projection.Scope ||
		!validIdentifier(request.OperationID, 128) || request.GraphRevision == 0 {
		return journal.ErrInvalidRecord
	}
	expectedActionID := scopedID(
		"isolation_action",
		request.Scope.Key(),
		request.SemanticOperation,
		request.OperationID,
	)
	expectedIdempotencyKey := scopedID(
		"isolation_key",
		request.Scope.Key(),
		request.SemanticOperation,
		request.OperationID,
	)
	expectedEventID := scopedID(
		"isolation_event",
		request.Scope.Key(),
		request.SemanticOperation,
		request.OperationID,
	)
	if action.ID != expectedActionID || action.IdempotencyKey != expectedIdempotencyKey ||
		action.ControlRunID != request.Scope.ControlRunID ||
		action.GraphRevision != request.GraphRevision ||
		action.ExpectedProjection != request.ExpectedRevision ||
		event.ID != expectedEventID || event.ControlRunID != request.Scope.ControlRunID ||
		event.ActionID != expectedActionID || event.Kind != journal.EventActionResult {
		return journal.ErrInvalidRecord
	}
	if err := validateEnvelopeShape(outcome, action); err != nil {
		return err
	}
	return validateRequestOutcome(request, outcome)
}

func validateRequestOutcome(request replayRequestEnvelope, outcome outcomeEnvelope) error {
	switch request.SemanticOperation {
	case operationTaskPhase:
		var input taskPhaseDelta
		if err := journal.DecodeStrict(request.Input, &input); err != nil ||
			outcome.TaskPhase == nil || input != *outcome.TaskPhase {
			return journal.ErrInvalidRecord
		}
	case operationRunPhase:
		var input runPhaseDelta
		if err := journal.DecodeStrict(request.Input, &input); err != nil ||
			outcome.RunPhase == nil || input != *outcome.RunPhase {
			return journal.ErrInvalidRecord
		}
	case operationInboxAppend:
		var input inboxAppendDelta
		if err := journal.DecodeStrict(request.Input, &input); err != nil ||
			outcome.InboxAppend == nil || input != *outcome.InboxAppend {
			return journal.ErrInvalidRecord
		}
	case operationInboxAck:
		var input inboxAckDelta
		if err := journal.DecodeStrict(request.Input, &input); err != nil ||
			outcome.InboxAck == nil || input != *outcome.InboxAck {
			return journal.ErrInvalidRecord
		}
	case operationGateRegister:
		var input gateRegisterDelta
		if err := journal.DecodeStrict(request.Input, &input); err != nil ||
			outcome.GateRegister == nil || input != *outcome.GateRegister {
			return journal.ErrInvalidRecord
		}
	case operationGateWake:
		var input gateWakeDelta
		if err := journal.DecodeStrict(request.Input, &input); err != nil ||
			outcome.GateWake == nil || input != *outcome.GateWake {
			return journal.ErrInvalidRecord
		}
	default:
		return journal.ErrInvalidRecord
	}
	return nil
}

func validateEnvelopeShape(envelope outcomeEnvelope, action journal.Action) error {
	count := 0
	for _, present := range []bool{
		envelope.TaskPhase != nil,
		envelope.RunPhase != nil,
		envelope.InboxAppend != nil,
		envelope.InboxAck != nil,
		envelope.GateRegister != nil,
		envelope.GateWake != nil,
	} {
		if present {
			count++
		}
	}
	if count != 1 {
		return journal.ErrInvalidRecord
	}
	switch envelope.SemanticOperation {
	case operationTaskPhase:
		delta := envelope.TaskPhase
		if delta == nil || action.Kind != journal.KindObserve ||
			action.TaskID != delta.TaskID || action.AttemptID != delta.AttemptID ||
			!validIdentifier(delta.TaskID, 128) ||
			(delta.AttemptID != "" && !validIdentifier(delta.AttemptID, 128)) ||
			(delta.From != "" && !validTaskPhase(delta.From)) || !validTaskPhase(delta.To) {
			return journal.ErrInvalidRecord
		}
	case operationRunPhase:
		delta := envelope.RunPhase
		if delta == nil || action.Kind != journal.KindObserve ||
			action.TaskID != "" || action.AttemptID != "" ||
			!validRunPhase(delta.From) || !validRunPhase(delta.To) ||
			!validIdentifier(delta.Authority, 128) {
			return journal.ErrInvalidRecord
		}
	case operationInboxAppend:
		delta := envelope.InboxAppend
		if delta == nil || action.Kind != journal.KindCallback ||
			action.TaskID != delta.TaskID || action.AttemptID != delta.AttemptID ||
			!validInboxAppendDelta(*delta) {
			return journal.ErrInvalidRecord
		}
	case operationInboxAck:
		delta := envelope.InboxAck
		if delta == nil || action.Kind != journal.KindAcknowledge ||
			action.TaskID != "" || action.AttemptID != "" || !validInboxAckDelta(*delta) {
			return journal.ErrInvalidRecord
		}
	case operationGateRegister:
		delta := envelope.GateRegister
		if delta == nil || action.Kind != journal.KindRunGate ||
			action.TaskID != delta.Gate.TaskID || action.AttemptID != "" ||
			validateGateDefinition(delta.Gate) != nil || delta.Gate.State != GatePending ||
			delta.Gate.WakeReceipt != "" {
			return journal.ErrInvalidRecord
		}
	case operationGateWake:
		delta := envelope.GateWake
		if delta == nil || action.Kind != journal.KindRunGate ||
			action.TaskID != "" || action.AttemptID != "" || !validGateWakeDelta(*delta) {
			return journal.ErrInvalidRecord
		}
	default:
		return journal.ErrInvalidRecord
	}
	return nil
}

func validInboxAppendDelta(delta inboxAppendDelta) bool {
	return validIdentifier(delta.EventID, 128) && validIdentifier(delta.CorrelationID, 128) &&
		validIdentifier(delta.TaskID, 128) && validIdentifier(delta.AttemptID, 128) &&
		validIdentifier(delta.ExternalActionID, 128) && delta.ActionGeneration > 0 &&
		validIdentifier(delta.Producer, 128) && validIdentifier(delta.Consumer, 128) &&
		journal.ValidDigest(delta.PayloadDigest)
}

func validInboxAckDelta(delta inboxAckDelta) bool {
	return validIdentifier(delta.EventID, 128) && validIdentifier(delta.Consumer, 128) &&
		validIdentifier(delta.ReceiptID, 128)
}

func validGateWakeDelta(delta gateWakeDelta) bool {
	return validIdentifier(delta.GateID, 128) &&
		validIdentifier(delta.ResolverAuthority, 128) &&
		validIdentifier(delta.WakeReceipt, 128) &&
		(delta.WakeEventID == "" || validIdentifier(delta.WakeEventID, 128))
}

func validateTaskTransition(projection Projection, delta taskPhaseDelta) error {
	current, exists := projection.Task(delta.TaskID)
	if !exists {
		if delta.From != "" || delta.To != TaskDiscovered {
			return ErrInvalidTransition
		}
		return nil
	}
	if current.AttemptID != delta.AttemptID || current.Phase != delta.From ||
		!allowedTaskTransition(delta.From, delta.To) {
		return ErrInvalidTransition
	}
	return nil
}

func applyTaskPhase(projection *Projection, delta taskPhaseDelta) {
	for index := range projection.Tasks {
		if projection.Tasks[index].TaskID == delta.TaskID {
			projection.Tasks[index].Phase = delta.To
			projection.RunPhase = deriveRunPhase(*projection)
			return
		}
	}
	projection.Tasks = append(projection.Tasks, TaskState{
		TaskID: delta.TaskID, AttemptID: delta.AttemptID, Phase: delta.To,
	})
	projection.RunPhase = deriveRunPhase(*projection)
}

func validateRunTransition(projection Projection, delta runPhaseDelta) error {
	if projection.RunPhase != delta.From {
		return ErrInvalidTransition
	}
	if delta.To == RunFrozenSecurity &&
		(delta.From == RunActive || delta.From == RunQuiescent) {
		return nil
	}
	if delta.From == RunFrozenSecurity && delta.To == RunActive {
		return nil
	}
	return ErrInvalidTransition
}

func applyInboxAck(projection *Projection, delta inboxAckDelta) error {
	for index := range projection.Inbox {
		item := &projection.Inbox[index]
		if item.EventID != delta.EventID {
			continue
		}
		if item.Consumer != delta.Consumer || item.AcknowledgementReceipt != "" {
			return journal.ErrConflict
		}
		item.AcknowledgementReceipt = delta.ReceiptID
		return nil
	}
	return journal.ErrInvalidRecord
}

func applyGateRegister(projection *Projection, delta gateRegisterDelta) error {
	if err := validateGateDefinition(delta.Gate); err != nil || delta.Gate.State != GatePending {
		return journal.ErrInvalidRecord
	}
	task, exists := projection.Task(delta.Gate.TaskID)
	if !exists || task.Phase != TaskDeferred {
		return journal.ErrInvalidRecord
	}
	if _, exists := projection.Gate(delta.Gate.ID); exists {
		return journal.ErrConflict
	}
	projection.Gates = append(projection.Gates, delta.Gate)
	if projection.RunPhase != RunFrozenSecurity {
		projection.RunPhase = deriveRunPhase(*projection)
	}
	return nil
}

func applyGateWake(projection *Projection, delta gateWakeDelta) error {
	gateIndex := -1
	for index := range projection.Gates {
		if projection.Gates[index].ID == delta.GateID {
			gateIndex = index
			break
		}
	}
	if gateIndex < 0 {
		return journal.ErrInvalidRecord
	}
	gate := &projection.Gates[gateIndex]
	if gate.State != GatePending || gate.ResolverAuthority != delta.ResolverAuthority {
		return journal.ErrConflict
	}
	if gate.Kind == GateTimeNotBefore {
		if delta.WakeEventID != "" || !delta.WakeTime.Equal(gate.WakeAt) {
			return journal.ErrInvalidRecord
		}
	} else {
		if !delta.WakeTime.IsZero() || delta.WakeEventID != gate.WakeEventID {
			return journal.ErrInvalidRecord
		}
		if _, exists := projection.InboxItem(delta.WakeEventID); !exists {
			return journal.ErrInvalidRecord
		}
	}
	gate.State = GateResolved
	gate.WakeReceipt = delta.WakeReceipt
	if !hasPendingGate(*projection, gate.TaskID) {
		for index := range projection.Tasks {
			if projection.Tasks[index].TaskID == gate.TaskID {
				if projection.Tasks[index].Phase != TaskDeferred {
					return journal.ErrInvalidRecord
				}
				projection.Tasks[index].Phase = TaskReadyForOwnership
				break
			}
		}
	}
	projection.RunPhase = deriveRunPhase(*projection)
	return nil
}

func validateGateDefinition(gate PendingWorkGate) error {
	if !validIdentifier(gate.ID, 128) || !validIdentifier(gate.TaskID, 128) ||
		!validGateKind(gate.Kind) || !validIdentifier(gate.ResolverAuthority, 128) {
		return ErrInvalidOperation
	}
	if gate.Kind == GateTimeNotBefore {
		if gate.WakeAt.IsZero() || gate.WakeEventID != "" {
			return ErrInvalidOperation
		}
		return nil
	}
	if !gate.WakeAt.IsZero() || !validIdentifier(gate.WakeEventID, 128) {
		return ErrInvalidOperation
	}
	return nil
}

func deriveRunPhase(projection Projection) RunPhase {
	if projection.RunPhase == RunFrozenSecurity {
		return RunFrozenSecurity
	}
	nonterminal := 0
	for _, task := range projection.Tasks {
		if task.Phase == TaskAccepted || task.Phase == TaskFailed {
			continue
		}
		nonterminal++
		if task.Phase != TaskDeferred || !hasPendingGate(projection, task.TaskID) {
			return RunActive
		}
	}
	if nonterminal > 0 {
		return RunQuiescent
	}
	return RunActive
}

func hasPendingGate(projection Projection, taskID string) bool {
	for _, gate := range projection.Gates {
		if gate.TaskID == taskID && gate.State == GatePending {
			return true
		}
	}
	return false
}

func validTaskPhase(phase TaskPhase) bool {
	switch phase {
	case TaskDiscovered, TaskAuditingReadOnly, TaskReadyForOwnership, TaskOwned,
		TaskExecuting, TaskVerifying, TaskAccepted, TaskDeferred, TaskNeedsInput,
		TaskRollbackRequired, TaskFailed:
		return true
	default:
		return false
	}
}

func allowedTaskTransition(from, to TaskPhase) bool {
	switch from {
	case TaskDiscovered:
		return to == TaskAuditingReadOnly || to == TaskDeferred ||
			to == TaskNeedsInput || to == TaskFailed
	case TaskAuditingReadOnly:
		return to == TaskReadyForOwnership || to == TaskDeferred ||
			to == TaskNeedsInput || to == TaskFailed
	case TaskReadyForOwnership:
		return to == TaskOwned || to == TaskDeferred || to == TaskNeedsInput || to == TaskFailed
	case TaskOwned:
		return to == TaskExecuting || to == TaskDeferred || to == TaskNeedsInput ||
			to == TaskRollbackRequired || to == TaskFailed
	case TaskExecuting:
		return to == TaskVerifying || to == TaskDeferred || to == TaskNeedsInput ||
			to == TaskRollbackRequired || to == TaskFailed
	case TaskVerifying:
		return to == TaskAccepted || to == TaskDeferred || to == TaskNeedsInput ||
			to == TaskRollbackRequired || to == TaskFailed
	case TaskDeferred:
		return to == TaskReadyForOwnership || to == TaskNeedsInput || to == TaskFailed
	case TaskNeedsInput:
		return to == TaskReadyForOwnership || to == TaskDeferred || to == TaskFailed
	case TaskRollbackRequired:
		return to == TaskExecuting || to == TaskVerifying || to == TaskFailed
	default:
		return false
	}
}

func validRunPhase(phase RunPhase) bool {
	return phase == RunActive || phase == RunFrozenSecurity || phase == RunQuiescent
}

func validObservationSource(source ObservationSource) bool {
	return source == ObservationProvider || source == ObservationTerminal ||
		source == ObservationYAML || source == ObservationUI
}

func validGateKind(kind GateKind) bool {
	switch kind {
	case GateTimeNotBefore, GateExternalStatus, GateWorkflowTerminal,
		GateEvidenceRequired, GateNoOverlapWindow, GateHumanApproval,
		GateSecurityContainment:
		return true
	default:
		return false
	}
}
