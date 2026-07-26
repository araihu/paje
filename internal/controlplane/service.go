package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/araihu/paje/internal/agentharness"
)

type Option func(*Service)

type Service struct {
	store    Store
	registry *agentharness.Registry
	now      func() time.Time
}

func WithClock(clock func() time.Time) Option {
	return func(service *Service) {
		if clock != nil {
			service.now = clock
		}
	}
}

func WithHarnessRegistry(registry *agentharness.Registry) Option {
	return func(service *Service) { service.registry = registry }
}

func NewService(store Store, options ...Option) *Service {
	service := &Service{store: store, now: func() time.Time { return time.Now().UTC() }}
	for _, option := range options {
		if option != nil {
			option(service)
		}
	}
	return service
}

func (s *Service) Create(ctx context.Context, run ControlRun, graph TaskGraph) (Snapshot, error) {
	if s == nil || s.store == nil {
		return Snapshot{}, invalidRecord("store is required")
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	now := s.now()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = now
	}
	snapshot, err := NewSnapshot(run, graph)
	if err != nil {
		return Snapshot{}, err
	}
	return s.store.Create(ctx, snapshot)
}

func (s *Service) Load(ctx context.Context, controlRunID string) (Snapshot, error) {
	if s == nil || s.store == nil {
		return Snapshot{}, invalidRecord("store is required")
	}
	return s.store.Load(ctx, controlRunID)
}

func (s *Service) UpdateGraph(
	ctx context.Context,
	controlRunID string,
	expectedRevision uint64,
	graph TaskGraph,
) (Snapshot, error) {
	return s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		if snapshot.Graph.Revision != expectedRevision {
			return ErrVersionConflict
		}
		if err := ValidateGraph(graph, &snapshot.Graph); err != nil {
			return err
		}
		snapshot.Graph = CloneGraph(graph)
		snapshot.Run.GraphRevision = graph.Revision
		return nil
	})
}

func (s *Service) CreateAttempt(
	ctx context.Context,
	controlRunID, taskID string,
	capabilities agentharness.CapabilitySnapshot,
) (PlacementAttempt, error) {
	var result PlacementAttempt
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		if err := capabilities.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrCapabilityUnavailable, err)
		}
		task := taskByID(snapshot.Graph, taskID)
		if task == nil {
			return ErrNotFound
		}
		for _, attempt := range snapshot.Attempts {
			if attempt.TaskID == taskID && !attemptTerminal(attempt.State) {
				if attempt.Primitive != task.Placement.ParallelismPrimitive ||
					attempt.CapabilitySnapshot.HarnessID != capabilities.HarnessID {
					return ErrActionConflict
				}
				result = attempt
				return nil
			}
		}
		ready, err := ReadyTasks(snapshot.Graph)
		if err != nil {
			return err
		}
		readyByGraph := false
		for _, candidate := range ready {
			if candidate.ID == taskID {
				readyByGraph = true
				break
			}
		}
		if !readyByGraph {
			return fmt.Errorf("%w: task predecessors are not terminal", ErrInvalidPlacement)
		}
		if err := validatePredecessorReadiness(*snapshot, *task); err != nil {
			return err
		}
		capacity := Capacity{
			Active:        make(map[agentharness.Primitive]int),
			ProjectActive: make(map[string]int),
			ProjectLimits: make(map[string]int),
		}
		active := make([]ActiveOwnership, 0, len(snapshot.Attempts))
		for _, existing := range snapshot.Attempts {
			if attemptTerminal(existing.State) {
				continue
			}
			capacity.Active[existing.Primitive]++
			existingTask := taskByID(snapshot.Graph, existing.TaskID)
			if existingTask == nil {
				return ErrInvalidRecord
			}
			projectIDs := make([]string, 0, len(existingTask.Projects))
			for _, project := range existingTask.Projects {
				capacity.ProjectActive[project.ID]++
				projectIDs = append(projectIDs, project.ID)
			}
			active = append(active, ActiveOwnership{
				TaskID: existing.TaskID, ProjectIDs: projectIDs,
				Mutable: append([]string(nil), existingTask.Ownership.Mutable...),
			})
		}
		if err := validateServiceAdmission(*task, capabilities, capacity, active); err != nil {
			return err
		}
		id := stableID("attempt", controlRunID, taskID, fmt.Sprintf("%d", snapshot.Graph.Revision), fmt.Sprintf("%d", len(snapshot.Attempts)+1))
		result = PlacementAttempt{
			ID: id, TaskID: taskID, Primitive: task.Placement.ParallelismPrimitive,
			CapabilitySnapshot: cloneCapabilities(capabilities),
			LifecycleOwner:     task.Placement.LifecycleOwner, State: AttemptReserved,
			RuntimeWorkIDs: []string{}, ActionIDs: []string{}, ObservedEvents: map[string]string{},
		}
		snapshot.Attempts[id] = result
		setTaskState(&snapshot.Graph, taskID, TaskDispatched)
		if result.Primitive == agentharness.LocalSequential {
			result.State = AttemptActive
			snapshot.Attempts[id] = result
			setTaskState(&snapshot.Graph, taskID, TaskActive)
		}
		appendEvent(snapshot, Event{
			Kind: EventPlacement, TaskID: taskID, AttemptID: id,
			Digest: digestStrings("placement", string(result.Primitive), task.Placement.LifecycleOwner),
		}, s.now())
		return nil
	})
	return result, err
}

func (s *Service) PromoteAttempt(
	ctx context.Context,
	controlRunID, attemptID string,
	promotion Promotion,
	capabilities agentharness.CapabilitySnapshot,
) (PlacementAttempt, Handoff, error) {
	var replacement PlacementAttempt
	var handoff Handoff
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		source, ok := snapshot.Attempts[attemptID]
		if !ok {
			return ErrNotFound
		}
		for _, existing := range snapshot.Attempts {
			if existing.PromotedFromAttemptID == attemptID {
				replacement = existing
				handoff = snapshot.Handoffs[existing.HandoffID]
				return nil
			}
		}
		if source.Primitive != agentharness.EphemeralSubagent {
			return fmt.Errorf("%w: promotion source is not ephemeral", ErrActionIncomplete)
		}
		if !attemptTerminal(source.State) || source.TerminalEvidence != promotion.Checkpoint {
			return fmt.Errorf("%w: promotion checkpoint is not terminal", ErrActionIncomplete)
		}
		if source.CloseEvidence.Kind != CloseRuntime || source.CloseEvidence.Receipt == "" {
			return fmt.Errorf("%w: promotion source runtime is not closed", ErrActionIncomplete)
		}
		if source.Disposition.Kind != DispositionHandedOff ||
			source.Disposition.EvidenceID != promotion.Checkpoint.ID {
			return fmt.Errorf("%w: promotion source is not handed off", ErrActionIncomplete)
		}
		task := taskByID(snapshot.Graph, source.TaskID)
		if task == nil {
			return ErrNotFound
		}
		promoted, proposedHandoff, err := Promote(*task, promotion, capabilities)
		if err != nil {
			return err
		}
		capacity := Capacity{
			Active: make(map[agentharness.Primitive]int), ProjectActive: make(map[string]int),
			ProjectLimits: make(map[string]int),
		}
		active := make([]ActiveOwnership, 0, len(snapshot.Attempts))
		for _, existing := range snapshot.Attempts {
			if attemptTerminal(existing.State) {
				continue
			}
			capacity.Active[existing.Primitive]++
			existingTask := taskByID(snapshot.Graph, existing.TaskID)
			if existingTask == nil {
				return ErrInvalidRecord
			}
			projectIDs := make([]string, 0, len(existingTask.Projects))
			for _, project := range existingTask.Projects {
				capacity.ProjectActive[project.ID]++
				projectIDs = append(projectIDs, project.ID)
			}
			active = append(active, ActiveOwnership{TaskID: existing.TaskID, ProjectIDs: projectIDs, Mutable: append([]string(nil), existingTask.Ownership.Mutable...)})
		}
		if err := validateServiceAdmission(promoted, capabilities, capacity, active); err != nil {
			return err
		}
		promoted.State = TaskDispatched
		for index := range snapshot.Graph.Tasks {
			if snapshot.Graph.Tasks[index].ID == promoted.ID {
				snapshot.Graph.Tasks[index] = promoted
				break
			}
		}
		handoff = proposedHandoff
		if snapshot.Evidence[handoff.Evidence.ID].ID == "" {
			return ErrNotFound
		}
		snapshot.Handoffs[handoff.ID] = handoff
		appendEvent(snapshot, Event{
			Kind: EventHandoff, TaskID: promoted.ID,
			Digest: digestStrings("promotion_handoff", handoff.ID, handoff.Evidence.Digest),
		}, s.now())
		replacement = PlacementAttempt{
			ID:     stableID("attempt", controlRunID, promoted.ID, "promotion", source.ID, handoff.ID),
			TaskID: promoted.ID, Primitive: agentharness.PersistentSession,
			CapabilitySnapshot: cloneCapabilities(capabilities), LifecycleOwner: promoted.Placement.LifecycleOwner,
			State: AttemptReserved, RuntimeWorkIDs: []string{}, ActionIDs: []string{},
			ObservedEvents: map[string]string{}, PromotedFromAttemptID: source.ID, HandoffID: handoff.ID,
		}
		snapshot.Attempts[replacement.ID] = replacement
		appendEvent(snapshot, Event{
			Kind: EventPlacement, TaskID: promoted.ID, AttemptID: replacement.ID,
			Digest: digestStrings("promotion_placement", replacement.ID, source.ID, handoff.ID),
		}, s.now())
		return nil
	})
	return replacement, handoff, err
}

func (s *Service) PrepareAction(
	ctx context.Context,
	controlRunID, attemptID string,
	kind agentharness.ActionKind,
	requestDigest string,
) (LifecycleAction, error) {
	var result LifecycleAction
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		var prepareErr error
		result, _, prepareErr = s.prepareAction(snapshot, controlRunID, attemptID, kind, requestDigest)
		return prepareErr
	})
	return result, err
}

// ObserveRequestDigest binds an Observe action to one exact persisted attempt
// cursor tuple under a versioned provider-neutral domain.
func ObserveRequestDigest(
	controlRunID, attemptID, afterCursor string,
	afterSequence uint64,
) string {
	return digestStrings(
		"paje-control-observe-v1", controlRunID, attemptID, afterCursor,
		strconv.FormatUint(afterSequence, 10),
	)
}

// PrepareObserve atomically reconciles an exact prior prepare before checking
// that a new Observe action starts from the attempt's current cursor tuple.
func (s *Service) PrepareObserve(
	ctx context.Context,
	controlRunID, attemptID, requestDigest, afterCursor string,
	afterSequence uint64,
) (LifecycleAction, bool, error) {
	var result LifecycleAction
	var reused bool
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		attempt, ok := snapshot.Attempts[attemptID]
		if !ok || requestDigest != ObserveRequestDigest(controlRunID, attemptID, afterCursor, afterSequence) {
			return ErrActionConflict
		}
		capability, err := agentharness.OperationCapability(agentharness.ActionObserve, attempt.Primitive)
		if err != nil {
			return err
		}
		primitiveCapabilities := attempt.CapabilitySnapshot.Primitives[attempt.Primitive]
		if !primitiveCapabilities.Supports(capability) {
			return ErrCapabilityUnavailable
		}
		for _, actionID := range attempt.ActionIDs {
			action := snapshot.Actions[actionID]
			if action.Kind == agentharness.ActionObserve && action.RequestDigest == requestDigest {
				result = action
				reused = true
				return nil
			}
		}
		if primitiveCapabilities.Supports(agentharness.CapCursor) {
			if afterCursor != attempt.LastCursor || afterSequence != attempt.CursorSequence {
				return ErrActionConflict
			}
		} else if afterCursor != "" || afterSequence != 0 {
			return ErrActionConflict
		}
		result, reused, err = s.prepareAction(
			snapshot, controlRunID, attemptID, agentharness.ActionObserve, requestDigest,
		)
		return err
	})
	return result, reused, err
}

func (s *Service) prepareAction(
	snapshot *Snapshot,
	controlRunID, attemptID string,
	kind agentharness.ActionKind,
	requestDigest string,
) (LifecycleAction, bool, error) {
	attempt, ok := snapshot.Attempts[attemptID]
	if !ok {
		return LifecycleAction{}, false, ErrNotFound
	}
	capability, err := agentharness.OperationCapability(kind, attempt.Primitive)
	if err != nil {
		return LifecycleAction{}, false, err
	}
	primitiveCapabilities := attempt.CapabilitySnapshot.Primitives[attempt.Primitive]
	if !primitiveCapabilities.Supports(capability) {
		return LifecycleAction{}, false, ErrCapabilityUnavailable
	}
	for _, actionID := range attempt.ActionIDs {
		action := snapshot.Actions[actionID]
		if action.Kind != kind {
			continue
		}
		if action.RequestDigest != requestDigest {
			if singleShotAction(kind) {
				return LifecycleAction{}, false, ErrActionConflict
			}
			continue
		}
		return action, true, nil
	}
	actionID, err := agentharness.StableActionID(
		controlRunID, attempt.TaskID, attemptID, snapshot.Graph.Revision,
		attempt.Primitive, kind, requestDigest,
	)
	if err != nil {
		return LifecycleAction{}, false, err
	}
	result := LifecycleAction{
		ID: actionID, AttemptID: attemptID, Kind: kind,
		RequestDigest: requestDigest, PreparedAt: s.now(),
	}
	snapshot.Actions[actionID] = result
	attempt.ActionIDs = append(attempt.ActionIDs, actionID)
	snapshot.Attempts[attemptID] = attempt
	appendEvent(snapshot, Event{
		Kind: EventActionPrepared, TaskID: attempt.TaskID, AttemptID: attemptID,
		ActionID: actionID, Digest: digestStrings("prepare", actionID, requestDigest),
	}, s.now())
	return result, false, nil
}

func (s *Service) CompleteAction(
	ctx context.Context,
	controlRunID, actionID string,
	result agentharness.ActionResult,
) (LifecycleAction, error) {
	var completed LifecycleAction
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		return s.completeAction(snapshot, controlRunID, actionID, result, &completed, nil)
	})
	return completed, err
}

func (s *Service) completeAction(
	snapshot *Snapshot,
	controlRunID, actionID string,
	result agentharness.ActionResult,
	completed *LifecycleAction,
	completedAttempt *PlacementAttempt,
) error {
	action, ok := snapshot.Actions[actionID]
	if !ok {
		return ErrNotFound
	}
	if action.Ambiguous {
		return ErrAmbiguousDispatch
	}
	if result.ActionID != actionID || strings.TrimSpace(result.ResultDigest) == "" {
		return ErrActionConflict
	}
	if action.Completed {
		if !sameActionResult(action.Result, result) {
			return ErrActionConflict
		}
		*completed = action
		if completedAttempt != nil {
			*completedAttempt = snapshot.Attempts[action.AttemptID]
		}
		return nil
	}
	attempt := snapshot.Attempts[action.AttemptID]
	primitiveCapabilities := attempt.CapabilitySnapshot.Primitives[attempt.Primitive]
	expectedRuntimeIDs := attempt.RuntimeWorkIDs
	if action.Kind == agentharness.ActionDispatch {
		expectedRuntimeIDs = nil
	}
	trustedResult := result
	trustedResult.Events = make([]agentharness.WorkEvent, 0, len(result.Events))
	duplicateEvents := 0
	if attempt.ObservedEvents == nil {
		attempt.ObservedEvents = make(map[string]string)
	}
	for _, event := range result.Events {
		if digest, exists := attempt.ObservedEvents[event.ID]; exists {
			if digest != event.ResultDigest || !completedEventMatches(*snapshot, attempt, event) {
				return ErrActionConflict
			}
			duplicateEvents++
			continue
		}
		trustedResult.Events = append(trustedResult.Events, event)
	}
	validationCursorSequence := attempt.CursorSequence
	if duplicateEvents > 0 && len(trustedResult.Events) == 0 {
		if result.Cursor != attempt.LastCursor || result.CursorSequence != attempt.CursorSequence {
			return ErrActionConflict
		}
		validationCursorSequence = 0
	}
	if err := agentharness.ValidateActionResult(
		action.Kind, attempt.Primitive, expectedRuntimeIDs,
		validationCursorSequence, primitiveCapabilities.Supports(agentharness.CapCursor), trustedResult,
	); err != nil {
		if errors.Is(err, agentharness.ErrCursorRegression) {
			return ErrCursorRegression
		}
		if errors.Is(err, agentharness.ErrUnexpectedRuntimeIdentity) {
			return err
		}
		return ErrActionConflict
	}
	if (action.Kind == agentharness.ActionSend || action.Kind == agentharness.ActionAcknowledge) &&
		strings.TrimSpace(result.MessageReceipt) == "" {
		return ErrActionConflict
	}
	if len(result.RuntimeWorkIDs) > 0 && !primitiveCapabilities.Supports(agentharness.CapRuntimeIdentity) {
		return agentharness.ErrUnexpectedRuntimeIdentity
	}
	if action.Kind == agentharness.ActionDispatch &&
		attempt.Primitive == agentharness.PersistentSession && len(result.RuntimeWorkIDs) != 1 {
		return agentharness.ErrUnexpectedRuntimeIdentity
	}
	if action.Kind != agentharness.ActionDispatch && len(result.RuntimeWorkIDs) > 0 &&
		!sameStrings(attempt.RuntimeWorkIDs, result.RuntimeWorkIDs) {
		return ErrActionConflict
	}
	action.Completed = true
	action.Result = cloneActionResult(result)
	action.CompletedAt = s.now()
	snapshot.Actions[actionID] = action
	if result.Cursor != "" && result.CursorSequence > attempt.CursorSequence {
		attempt.LastCursor = result.Cursor
		attempt.CursorSequence = result.CursorSequence
	}
	if action.Kind == agentharness.ActionDispatch {
		attempt.RuntimeWorkIDs = append([]string(nil), result.RuntimeWorkIDs...)
		attempt.State = AttemptActive
		setTaskState(&snapshot.Graph, attempt.TaskID, TaskActive)
		if attempt.Primitive == agentharness.PersistentSession {
			runtimeID := result.RuntimeWorkIDs[0]
			sessionID := stableID("session", controlRunID, attempt.ID, runtimeID)
			if existing, exists := snapshot.Sessions[sessionID]; exists && existing.RuntimeChildID != runtimeID {
				return ErrActionConflict
			}
			snapshot.Sessions[sessionID] = AgentSession{
				ID: sessionID, AttemptID: attempt.ID, TaskID: attempt.TaskID,
				HarnessID: attempt.CapabilitySnapshot.HarnessID, RuntimeChildID: runtimeID,
				LastCursor: result.Cursor, State: SessionRegistered,
			}
		}
	}
	if action.Kind == agentharness.ActionObserve || action.Kind == agentharness.ActionWait {
		for _, event := range trustedResult.Events {
			attempt.ObservedEvents[event.ID] = event.ResultDigest
			if event.Terminal {
				attempt.TerminalObserved = true
				break
			}
		}
	}
	if action.Kind == agentharness.ActionClose {
		if err := validateClosePrerequisites(*snapshot, attempt); err != nil {
			return err
		}
		closeEvidence, err := controlCloseEvidence(result.CloseEvidence)
		if err != nil {
			return err
		}
		if err := validateCloseEvidence(attempt.Primitive, closeEvidence); err != nil {
			return err
		}
		if attempt.CloseEvidence.Kind != "" && attempt.CloseEvidence != closeEvidence {
			return ErrActionConflict
		}
		attempt.CloseEvidence = closeEvidence
		if attempt.TerminalEvidence.ID != "" {
			attempt.State = AttemptCompleted
		}
		if attempt.Primitive == agentharness.PersistentSession {
			for id, session := range snapshot.Sessions {
				if session.AttemptID == attempt.ID {
					session.ArchiveReceipt = closeEvidence.Receipt
					session.State = SessionArchived
					snapshot.Sessions[id] = session
				}
			}
		}
	}
	snapshot.Attempts[attempt.ID] = attempt
	appendEvent(snapshot, Event{
		Kind: EventActionCompleted, TaskID: attempt.TaskID, AttemptID: attempt.ID,
		ActionID: actionID, Digest: digestStrings("complete", actionID, result.ResultDigest),
	}, s.now())
	*completed = action
	if completedAttempt != nil {
		*completedAttempt = attempt
	}
	return nil
}

func (s *Service) MarkAmbiguous(
	ctx context.Context,
	controlRunID, actionID string,
) (Snapshot, error) {
	snapshot, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		action, ok := snapshot.Actions[actionID]
		if !ok {
			return ErrNotFound
		}
		if action.Completed {
			return ErrActionConflict
		}
		code := ambiguityCode(action, snapshot.Attempts[action.AttemptID])
		action.Ambiguous = true
		action.AmbiguityCode = code
		snapshot.Actions[actionID] = action
		attempt := snapshot.Attempts[action.AttemptID]
		attempt.State = AttemptBlocked
		attempt.BlockCode = code
		snapshot.Attempts[attempt.ID] = attempt
		snapshot.Run.Status = StatusBlocked
		setTaskState(&snapshot.Graph, attempt.TaskID, TaskNeedsInput)
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	action := snapshot.Actions[actionID]
	return snapshot, ambiguityError(action.AmbiguityCode)
}

func (s *Service) AdvanceCursor(
	ctx context.Context,
	controlRunID, attemptID, cursor string,
	sequence uint64,
) (PlacementAttempt, error) {
	var result PlacementAttempt
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		attempt, ok := snapshot.Attempts[attemptID]
		if !ok {
			return ErrNotFound
		}
		if cursor == "" || sequence <= attempt.CursorSequence {
			return ErrCursorRegression
		}
		attempt.LastCursor = cursor
		attempt.CursorSequence = sequence
		snapshot.Attempts[attemptID] = attempt
		for id, session := range snapshot.Sessions {
			if session.AttemptID == attemptID {
				session.LastCursor = cursor
				snapshot.Sessions[id] = session
			}
		}
		result = attempt
		return nil
	})
	return result, err
}

func (s *Service) AcknowledgeRuntimeID(
	ctx context.Context,
	controlRunID, attemptID, runtimeChildID string,
) (AgentSession, error) {
	var result AgentSession
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		attempt, ok := snapshot.Attempts[attemptID]
		if !ok {
			return ErrNotFound
		}
		for id, session := range snapshot.Sessions {
			if session.AttemptID != attemptID {
				continue
			}
			if session.RuntimeChildID != runtimeChildID {
				return ErrActionConflict
			}
			expectedDigest := RegistrationMessageDigest(controlRunID, attemptID, runtimeChildID)
			if !completedAcknowledgement(snapshot.Actions, attempt.ActionIDs, runtimeChildID, expectedDigest) {
				return ErrActionIncomplete
			}
			session.RuntimeIDAcknowledged = true
			session.RegistrationMessageDigest = expectedDigest
			session.State = SessionAcknowledged
			snapshot.Sessions[id] = session
			result = session
			return nil
		}
		return ErrNotFound
	})
	return result, err
}

func (s *Service) RecordCallback(
	ctx context.Context,
	controlRunID string,
	callback CompletionCallback,
) (CompletionCallback, error) {
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		attempt, ok := snapshot.Attempts[callback.AttemptID]
		if !ok || attempt.Primitive != agentharness.PersistentSession || !validCallback(callback) {
			return ErrInvalidRecord
		}
		if existing, ok := snapshot.Callbacks[callback.AttemptID]; ok {
			if existing != callback {
				return ErrActionConflict
			}
			return nil
		}
		sessionFound := false
		for id, session := range snapshot.Sessions {
			if session.AttemptID != callback.AttemptID {
				continue
			}
			if session.RuntimeChildID != callback.RuntimeChildID {
				return ErrActionConflict
			}
			if !session.RuntimeIDAcknowledged {
				return ErrActionIncomplete
			}
			sessionFound = true
			switch callback.Status {
			case CallbackDone, CallbackDoneWithConcerns:
				session.State = SessionCompleted
			case CallbackBlocked, CallbackNeedsInput:
				session.State = SessionActive
				setTaskState(&snapshot.Graph, attempt.TaskID, TaskNeedsInput)
			}
			snapshot.Sessions[id] = session
			break
		}
		if !sessionFound {
			return ErrNotFound
		}
		if !completedCallback(
			snapshot.Actions, attempt.ActionIDs, callback.RuntimeChildID,
			CompletionCallbackDigest(callback),
		) {
			return ErrActionIncomplete
		}
		snapshot.Callbacks[callback.AttemptID] = callback
		appendEvent(snapshot, Event{
			Kind: EventActionCompleted, TaskID: attempt.TaskID, AttemptID: attempt.ID,
			Digest: digestStrings("callback", callback.RuntimeChildID, callback.HeadSHA, string(callback.Status)),
		}, s.now())
		return nil
	})
	return callback, err
}

func (s *Service) SendMessage(
	ctx context.Context,
	controlRunID string,
	message Message,
) (Message, error) {
	var result Message
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		if err := validateSendMessage(*snapshot, message); err != nil {
			return err
		}
		if existing, ok := snapshot.Messages[message.ID]; ok {
			if !sameSendMessage(existing, message) {
				return ErrActionConflict
			}
			result = existing
			return nil
		}
		snapshot.Messages[message.ID] = message
		appendMessageEvent(snapshot, message, s.now())
		result = message
		return nil
	})
	return result, err
}

// SendRequestDigest binds an ActionSend prepare to the immutable sender input.
// Acknowledged is intentionally excluded because it is owned by the server.
func SendRequestDigest(message Message) string {
	input := struct {
		ID         string      `json:"id"`
		FromTaskID string      `json:"from_task_id"`
		ToTaskID   string      `json:"to_task_id"`
		Kind       MessageKind `json:"kind"`
		Digest     string      `json:"digest"`
	}{
		ID: message.ID, FromTaskID: message.FromTaskID, ToTaskID: message.ToTaskID,
		Kind: message.Kind, Digest: message.Digest,
	}
	raw, _ := json.Marshal(input)
	sum := sha256.Sum256(append([]byte("paje-control-send-v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CompleteSend atomically completes a bound ActionSend and persists its exact
// immutable message plus mailbox event in the same durable store transaction.
func (s *Service) CompleteSend(
	ctx context.Context,
	controlRunID, attemptID, actionID string,
	result agentharness.ActionResult,
	message Message,
) (LifecycleAction, PlacementAttempt, Message, error) {
	var completed LifecycleAction
	var attempt PlacementAttempt
	var persisted Message
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		if err := validateSendMessage(*snapshot, message); err != nil {
			return err
		}
		action, ok := snapshot.Actions[actionID]
		if !ok {
			return ErrNotFound
		}
		if action.Kind != agentharness.ActionSend || action.AttemptID != attemptID ||
			action.RequestDigest != SendRequestDigest(message) {
			return ErrActionConflict
		}
		existing, exists := snapshot.Messages[message.ID]
		if exists && !sameSendMessage(existing, message) {
			return ErrActionConflict
		}
		if exists && !sendMessageBoundToAction(*snapshot, existing, actionID) {
			return ErrActionConflict
		}
		if err := s.completeAction(
			snapshot, controlRunID, actionID, result, &completed, &attempt,
		); err != nil {
			return err
		}
		if exists {
			persisted = existing
			return nil
		}
		snapshot.Messages[message.ID] = message
		appendMessageEvent(snapshot, message, s.now())
		persisted = message
		return nil
	})
	return completed, attempt, persisted, err
}

func validateSendMessage(snapshot Snapshot, message Message) error {
	if message.Acknowledged || message.ID == "" || !validMessageKind(message.Kind) ||
		!validDigest(message.Digest) ||
		!messageScopeAllowed(snapshot.Graph, message.FromTaskID, message.ToTaskID) {
		return ErrInvalidGraph
	}
	return nil
}

func sameSendMessage(first, second Message) bool {
	return first.ID == second.ID && first.FromTaskID == second.FromTaskID &&
		first.ToTaskID == second.ToTaskID && first.Kind == second.Kind && first.Digest == second.Digest
}

func sendMessageBoundToAction(snapshot Snapshot, message Message, actionID string) bool {
	bound := false
	requestDigest := SendRequestDigest(message)
	for id, action := range snapshot.Actions {
		if action.Kind != agentharness.ActionSend || !action.Completed || action.RequestDigest != requestDigest {
			continue
		}
		if id != actionID {
			return false
		}
		bound = true
	}
	return bound
}

func appendMessageEvent(snapshot *Snapshot, message Message, now time.Time) {
	kind := EventSteering
	if message.Kind == MessageDependencyHandoff {
		kind = EventHandoff
	}
	appendEvent(snapshot, Event{
		Kind: kind, TaskID: message.ToTaskID,
		Digest: digestStrings("message", message.ID, message.Digest),
	}, now)
}

func (s *Service) AcknowledgeMessage(
	ctx context.Context,
	controlRunID, messageID, taskID string,
) (Message, error) {
	var result Message
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		message, ok := snapshot.Messages[messageID]
		if !ok {
			return ErrNotFound
		}
		if message.ToTaskID != taskID {
			return ErrActionConflict
		}
		message.Acknowledged = true
		snapshot.Messages[messageID] = message
		result = message
		return nil
	})
	return result, err
}

func (s *Service) RecordEvidence(
	ctx context.Context,
	controlRunID string,
	evidence Evidence,
) (Evidence, error) {
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		if err := validateEvidence(evidence, snapshot); err != nil {
			return err
		}
		if existing, ok := snapshot.Evidence[evidence.ID]; ok {
			if !sameEvidence(existing, evidence) {
				return ErrEvidenceImmutable
			}
			return nil
		}
		snapshot.Evidence[evidence.ID] = evidence
		appendEvent(snapshot, Event{
			Kind: EventEvidence, TaskID: evidence.TaskID, AttemptID: evidence.AttemptID,
			Digest: digestStrings("evidence", evidence.ID, evidence.HeadSHA, evidence.OwnedPathsDigest),
		}, s.now())
		return nil
	})
	return evidence, err
}

func (s *Service) SetDisposition(
	ctx context.Context,
	controlRunID, attemptID string,
	disposition Disposition,
) (PlacementAttempt, error) {
	var result PlacementAttempt
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		if !validDisposition(disposition) {
			return ErrClosePrecondition
		}
		if snapshot.Evidence[disposition.EvidenceID].ID == "" {
			return ErrNotFound
		}
		attempt, ok := snapshot.Attempts[attemptID]
		if !ok {
			return ErrNotFound
		}
		if attempt.Disposition.Kind != "" && attempt.Disposition != disposition {
			return ErrEvidenceImmutable
		}
		if attempt.Disposition == disposition {
			result = attempt
			return nil
		}
		attempt.Disposition = disposition
		snapshot.Attempts[attemptID] = attempt
		for id, session := range snapshot.Sessions {
			if session.AttemptID == attemptID {
				session.Disposition = disposition
				snapshot.Sessions[id] = session
			}
		}
		appendEvent(snapshot, Event{
			Kind: EventDisposition, TaskID: attempt.TaskID, AttemptID: attempt.ID,
			Digest: digestStrings("disposition", string(disposition.Kind), disposition.EvidenceID),
		}, s.now())
		result = attempt
		return nil
	})
	return result, err
}

func (s *Service) CloseAttempt(
	ctx context.Context,
	controlRunID, attemptID string,
	evidence WorkCloseEvidence,
) (PlacementAttempt, error) {
	var result PlacementAttempt
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		attempt, ok := snapshot.Attempts[attemptID]
		if !ok {
			return ErrNotFound
		}
		if err := validateCloseEvidence(attempt.Primitive, evidence); err != nil {
			return err
		}
		if attempt.Primitive != agentharness.LocalSequential {
			return ErrActionIncomplete
		}
		if err := validateClosePrerequisites(*snapshot, attempt); err != nil {
			return err
		}
		if attempt.CloseEvidence.Kind != "" && attempt.CloseEvidence != evidence {
			return ErrActionConflict
		}
		attempt.CloseEvidence = evidence
		if attempt.TerminalEvidence.ID != "" {
			attempt.State = AttemptCompleted
		}
		snapshot.Attempts[attemptID] = attempt
		if attempt.Primitive == agentharness.PersistentSession {
			for id, session := range snapshot.Sessions {
				if session.AttemptID == attemptID {
					session.ArchiveReceipt = evidence.Receipt
					session.State = SessionArchived
					snapshot.Sessions[id] = session
				}
			}
		}
		result = attempt
		return nil
	})
	return result, err
}

func (s *Service) AttachTerminalEvidence(
	ctx context.Context,
	controlRunID, attemptID string,
	reference EvidenceRef,
) (PlacementAttempt, error) {
	var result PlacementAttempt
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		attempt, ok := snapshot.Attempts[attemptID]
		evidence := snapshot.Evidence[reference.ID]
		if !ok || evidence.ID == "" || !validDigest(reference.Digest) {
			return ErrNotFound
		}
		if reference.Digest != EvidenceDigest(evidence) {
			return ErrActionConflict
		}
		if attempt.TerminalEvidence.ID != "" && attempt.TerminalEvidence != reference {
			return ErrEvidenceImmutable
		}
		attempt.TerminalEvidence = reference
		if attempt.CloseEvidence.Kind != "" {
			attempt.State = AttemptCompleted
		}
		snapshot.Attempts[attemptID] = attempt
		result = attempt
		return nil
	})
	return result, err
}

func (s *Service) AddHandoff(
	ctx context.Context,
	controlRunID string,
	handoff Handoff,
) (Handoff, error) {
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		producer := taskByID(snapshot.Graph, handoff.ProducerTaskID)
		consumer := taskByID(snapshot.Graph, handoff.ConsumerTaskID)
		if handoff.ID == "" || producer == nil || consumer == nil ||
			snapshot.Evidence[handoff.Evidence.ID].ID == "" ||
			handoff.Evidence.Digest != EvidenceDigest(snapshot.Evidence[handoff.Evidence.ID]) ||
			!contains(consumer.DependsOn, producer.ID) && producer.ID != consumer.ID {
			return ErrInvalidGraph
		}
		if existing, ok := snapshot.Handoffs[handoff.ID]; ok {
			if existing != handoff {
				return ErrActionConflict
			}
			return nil
		}
		snapshot.Handoffs[handoff.ID] = handoff
		appendEvent(snapshot, Event{
			Kind: EventHandoff, TaskID: handoff.ConsumerTaskID,
			Digest: digestStrings("handoff", handoff.ID, handoff.Evidence.Digest),
		}, s.now())
		return nil
	})
	return handoff, err
}

func (s *Service) AcknowledgeHandoff(
	ctx context.Context,
	controlRunID, handoffID string,
) (Handoff, error) {
	var result Handoff
	_, err := s.mutate(ctx, controlRunID, func(snapshot *Snapshot) error {
		handoff, ok := snapshot.Handoffs[handoffID]
		if !ok {
			return ErrNotFound
		}
		handoff.Acknowledged = true
		snapshot.Handoffs[handoffID] = handoff
		result = handoff
		return nil
	})
	return result, err
}

func (s *Service) CloseControlRun(ctx context.Context, controlRunID string) (Snapshot, error) {
	if s == nil || s.store == nil {
		return Snapshot{}, invalidRecord("store is required")
	}
	current, err := s.store.Load(ctx, controlRunID)
	if err != nil {
		return Snapshot{}, err
	}
	if current.Run.Status == StatusClosed {
		return current, nil
	}
	next := CloneSnapshot(current)
	closed, closeErr := CloseSnapshot(next, s.now())
	if closeErr == nil {
		next = closed
		appendEvent(&next, Event{
			Kind: EventClose, Digest: digestStrings("closed", controlRunID),
		}, s.now())
	} else {
		next.Run.Status = StatusClosing
		next.Run.Close.Pending = PendingWork(next)
		next.Run.Close.Code = "cleanup_incomplete"
		next.Run.UpdatedAt = s.now()
	}
	saved, saveErr := s.store.Save(ctx, next, current.Version)
	if saveErr != nil {
		if closeErr != nil {
			return Snapshot{}, errors.Join(closeErr, saveErr)
		}
		return Snapshot{}, saveErr
	}
	return saved, closeErr
}

func (s *Service) EventsAfter(
	ctx context.Context,
	controlRunID string,
	after uint64,
	limit int,
) ([]Event, uint64, error) {
	if s == nil || s.store == nil {
		return nil, after, invalidRecord("store is required")
	}
	return s.store.EventsAfter(ctx, controlRunID, after, limit)
}

func (s *Service) Recover(ctx context.Context, controlRunID string) (Snapshot, error) {
	for {
		snapshot, err := s.Load(ctx, controlRunID)
		if err != nil {
			return Snapshot{}, err
		}
		if err := ValidateSnapshot(snapshot); err != nil {
			return Snapshot{}, err
		}
		var pending *LifecycleAction
		for _, action := range snapshot.Actions {
			if action.Ambiguous {
				return snapshot, ambiguityError(action.AmbiguityCode)
			}
			if !action.Completed {
				copy := action
				pending = &copy
				break
			}
		}
		if pending == nil {
			return snapshot, nil
		}
		result, reconciled := s.reconcileAction(ctx, snapshot, *pending)
		if reconciled {
			if _, err := s.CompleteAction(ctx, controlRunID, pending.ID, result); err != nil {
				return Snapshot{}, err
			}
			continue
		}
		return s.MarkAmbiguous(ctx, controlRunID, pending.ID)
	}
}

func (s *Service) reconcileAction(
	ctx context.Context,
	snapshot Snapshot,
	action LifecycleAction,
) (agentharness.ActionResult, bool) {
	if s.registry == nil || action.Kind != agentharness.ActionDispatch {
		return agentharness.ActionResult{}, false
	}
	attempt, ok := snapshot.Attempts[action.AttemptID]
	if !ok {
		return agentharness.ActionResult{}, false
	}
	capabilities := attempt.CapabilitySnapshot.Primitives[attempt.Primitive]
	if !capabilities.Supports(agentharness.CapObserve) ||
		!capabilities.Supports(agentharness.CapIdempotency) ||
		!capabilities.Supports(agentharness.CapRestart) {
		return agentharness.ActionResult{}, false
	}
	harness, err := s.registry.Resolve(attempt.CapabilitySnapshot.HarnessID)
	if err != nil {
		return agentharness.ActionResult{}, false
	}
	reconcileDigest := digestStrings("reconcile", action.ID, action.RequestDigest)
	reconcileID, err := agentharness.StableActionID(
		snapshot.Run.ID, attempt.TaskID, attempt.ID, snapshot.Graph.Revision,
		attempt.Primitive, agentharness.ActionObserve, reconcileDigest,
	)
	if err != nil {
		return agentharness.ActionResult{}, false
	}
	events, err := harness.Observe(ctx, agentharness.ObserveWorkRequest{
		ActionID: reconcileID, ControlRunID: snapshot.Run.ID, TaskID: attempt.TaskID,
		AttemptID: attempt.ID, AfterCursor: attempt.LastCursor,
		AfterCursorSequence: attempt.CursorSequence,
		ReconcileActionID:   action.ID, ReconcileRequestDigest: action.RequestDigest,
	})
	if err != nil || events.ReconciledResult == nil || events.ReconciledResult.ActionID != action.ID {
		return agentharness.ActionResult{}, false
	}
	return cloneActionResult(*events.ReconciledResult), true
}

func (s *Service) mutate(
	ctx context.Context,
	controlRunID string,
	change func(*Snapshot) error,
) (Snapshot, error) {
	if s == nil || s.store == nil {
		return Snapshot{}, invalidRecord("store is required")
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	current, err := s.store.Load(ctx, controlRunID)
	if err != nil {
		return Snapshot{}, err
	}
	next := CloneSnapshot(current)
	if err := change(&next); err != nil {
		return Snapshot{}, err
	}
	next.Run.UpdatedAt = s.now()
	return s.store.Save(ctx, next, current.Version)
}

func appendEvent(snapshot *Snapshot, event Event, now time.Time) {
	snapshot.Run.EventCursor++
	event.Cursor = snapshot.Run.EventCursor
	event.ControlRunID = snapshot.Run.ID
	event.CreatedAt = now
	snapshot.Events = append(snapshot.Events, event)
}

func setTaskState(graph *TaskGraph, taskID string, state TaskState) {
	for index := range graph.Tasks {
		if graph.Tasks[index].ID == taskID {
			graph.Tasks[index].State = state
			return
		}
	}
}

func validateServiceAdmission(
	task Task,
	capabilities agentharness.CapabilitySnapshot,
	capacity Capacity,
	active []ActiveOwnership,
) error {
	projectIDs := make([]string, 0, len(task.Projects))
	for _, project := range task.Projects {
		projectIDs = append(projectIDs, project.ID)
	}
	filtered := make([]ActiveOwnership, 0, len(active))
	for _, writer := range active {
		if len(writer.ProjectIDs) == 0 || sharesString(projectIDs, writer.ProjectIDs) {
			filtered = append(filtered, writer)
		}
	}
	return ValidatePlacement(task, capabilities, capacity, filtered)
}

func validatePredecessorReadiness(snapshot Snapshot, task Task) error {
	for _, predecessorID := range task.DependsOn {
		attemptCount := 0
		for _, attempt := range snapshot.Attempts {
			if attempt.TaskID != predecessorID {
				continue
			}
			attemptCount++
			evidence := snapshot.Evidence[attempt.TerminalEvidence.ID]
			if !attemptTerminal(attempt.State) || evidence.ID == "" ||
				attempt.TerminalEvidence.Digest != EvidenceDigest(evidence) ||
				!validDisposition(attempt.Disposition) ||
				attempt.Disposition.EvidenceID != evidence.ID ||
				validateCloseEvidence(attempt.Primitive, attempt.CloseEvidence) != nil {
				return fmt.Errorf("%w: predecessor %q placement is not terminal", ErrClosePrecondition, predecessorID)
			}
		}
		if attemptCount == 0 {
			return fmt.Errorf("%w: predecessor %q has no placement attempt", ErrClosePrecondition, predecessorID)
		}
		for _, handoff := range snapshot.Handoffs {
			if handoff.ProducerTaskID == predecessorID && handoff.ConsumerTaskID == task.ID &&
				handoff.AcknowledgementRequired && !handoff.Acknowledged {
				return fmt.Errorf("%w: predecessor %q handoff is unacknowledged", ErrClosePrecondition, predecessorID)
			}
		}
	}
	return nil
}

func completedEventMatches(snapshot Snapshot, attempt PlacementAttempt, candidate agentharness.WorkEvent) bool {
	for _, actionID := range attempt.ActionIDs {
		action := snapshot.Actions[actionID]
		if !action.Completed || action.Ambiguous ||
			(action.Kind != agentharness.ActionObserve && action.Kind != agentharness.ActionWait) {
			continue
		}
		for _, event := range action.Result.Events {
			if event == candidate {
				return true
			}
		}
	}
	return false
}

func validateEvidence(evidence Evidence, snapshot *Snapshot) error {
	attempt, ok := snapshot.Attempts[evidence.AttemptID]
	if evidence.ID == "" || !ok || attempt.TaskID != evidence.TaskID ||
		!validGitSHA(evidence.BaseSHA) || !validGitSHA(evidence.HeadSHA) ||
		(evidence.PushedSHA != "" && !validGitSHA(evidence.PushedSHA)) ||
		!validDigest(evidence.OwnedPathsDigest) || len(evidence.Tests) == 0 {
		return ErrInvalidRecord
	}
	task := taskByID(snapshot.Graph, evidence.TaskID)
	baseMatches := false
	if task != nil {
		for _, project := range task.Projects {
			if project.BaseSHA == evidence.BaseSHA {
				baseMatches = true
				break
			}
		}
	}
	if !baseMatches {
		return ErrInvalidRecord
	}
	for _, test := range evidence.Tests {
		if !validDigest(test.CommandDigest) || !validDigest(test.ResultDigest) {
			return ErrInvalidRecord
		}
	}
	return nil
}

func validateCloseEvidence(primitive agentharness.Primitive, evidence WorkCloseEvidence) error {
	if evidence.Receipt == "" || !validDigest(evidence.Digest) {
		return ErrCleanupIncomplete
	}
	switch primitive {
	case agentharness.PersistentSession:
		if evidence.Kind != CloseArchive {
			return ErrCleanupIncomplete
		}
	case agentharness.EphemeralSubagent:
		if evidence.Kind != CloseRuntime {
			return ErrCleanupIncomplete
		}
	case agentharness.HarnessNativeParallel:
		if evidence.Kind != CloseAggregate && evidence.Kind != CloseCanceled {
			return ErrCleanupIncomplete
		}
	case agentharness.LocalSequential:
		if evidence.Kind != CloseInactive {
			return ErrCleanupIncomplete
		}
	default:
		return ErrInvalidPlacement
	}
	return nil
}

func validateClosePrerequisites(snapshot Snapshot, attempt PlacementAttempt) error {
	switch attempt.Primitive {
	case agentharness.PersistentSession:
		callback, callbackFound := snapshot.Callbacks[attempt.ID]
		if !callbackFound || callback.RuntimeChildID == "" || !attempt.TerminalObserved {
			return ErrActionIncomplete
		}
		sessionFound := false
		for _, session := range snapshot.Sessions {
			if session.AttemptID != attempt.ID {
				continue
			}
			sessionFound = true
			if !session.RuntimeIDAcknowledged || session.RuntimeChildID != callback.RuntimeChildID {
				return ErrActionIncomplete
			}
		}
		if !sessionFound {
			return ErrActionIncomplete
		}
	case agentharness.EphemeralSubagent:
		if !attempt.TerminalObserved {
			return ErrActionIncomplete
		}
	}
	return nil
}

func controlCloseEvidence(evidence agentharness.CloseEvidence) (WorkCloseEvidence, error) {
	result := WorkCloseEvidence{Receipt: evidence.Receipt, Digest: evidence.Digest}
	switch evidence.Kind {
	case agentharness.CloseArchive:
		result.Kind = CloseArchive
	case agentharness.CloseRuntime:
		result.Kind = CloseRuntime
	case agentharness.CloseAggregate:
		result.Kind = CloseAggregate
	case agentharness.CloseCanceled:
		result.Kind = CloseCanceled
	case agentharness.CloseInactive:
		result.Kind = CloseInactive
	default:
		return WorkCloseEvidence{}, ErrCleanupIncomplete
	}
	return result, nil
}

func messageScopeAllowed(graph TaskGraph, fromTaskID, toTaskID string) bool {
	from := taskByID(graph, fromTaskID)
	to := taskByID(graph, toTaskID)
	if fromTaskID == ParentAddress {
		return to != nil
	}
	if toTaskID == ParentAddress {
		return from != nil
	}
	if from == nil || to == nil || fromTaskID == toTaskID {
		return false
	}
	for _, edge := range from.Communication {
		if edge.TaskID == toTaskID && projectByID(*to, edge.ProjectID) != nil {
			return true
		}
	}
	for _, edge := range to.Communication {
		if edge.TaskID == fromTaskID && projectByID(*from, edge.ProjectID) != nil {
			return true
		}
	}
	return false
}

func stableID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(append([]string{prefix}, values...), "\x00")))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func digestStrings(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneCapabilities(source agentharness.CapabilitySnapshot) agentharness.CapabilitySnapshot {
	result := source
	result.Primitives = make(map[agentharness.Primitive]agentharness.PrimitiveCapabilities, len(source.Primitives))
	for key, value := range source.Primitives {
		cloned := value
		cloned.Capabilities = make(map[agentharness.Capability]bool, len(value.Capabilities))
		for capability, supported := range value.Capabilities {
			cloned.Capabilities[capability] = supported
		}
		result.Primitives[key] = cloned
	}
	return result
}

func cloneActionResult(result agentharness.ActionResult) agentharness.ActionResult {
	cloned := result
	cloned.RuntimeWorkIDs = append([]string(nil), result.RuntimeWorkIDs...)
	cloned.Events = append([]agentharness.WorkEvent(nil), result.Events...)
	cloned.Diagnostic = agentharness.SafeDiagnostic(result.Diagnostic)
	return cloned
}

func sameActionResult(first, second agentharness.ActionResult) bool {
	firstDigest, firstErr := agentharness.CanonicalResultDigest(first)
	secondDigest, secondErr := agentharness.CanonicalResultDigest(second)
	return firstErr == nil && secondErr == nil && firstDigest == secondDigest
}

func sameEvidence(first, second Evidence) bool {
	return first.ID == second.ID && first.TaskID == second.TaskID &&
		first.AttemptID == second.AttemptID && first.Branch == second.Branch &&
		first.BaseSHA == second.BaseSHA && first.HeadSHA == second.HeadSHA &&
		first.PushedSHA == second.PushedSHA &&
		first.OwnedPathsDigest == second.OwnedPathsDigest &&
		first.ArtifactRef == second.ArtifactRef && first.ReportRef == second.ReportRef &&
		first.ReviewDigest == second.ReviewDigest && testsEqual(first.Tests, second.Tests)
}

func testsEqual(first, second []TestEvidence) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func sameStrings(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func singleShotAction(kind agentharness.ActionKind) bool {
	switch kind {
	case agentharness.ActionDispatch, agentharness.ActionInterrupt,
		agentharness.ActionClose, agentharness.ActionAcknowledge,
		agentharness.ActionCallback:
		return true
	default:
		return false
	}
}

func RegistrationMessageDigest(controlRunID, attemptID, runtimeChildID string) string {
	return digestStrings("registration_message", controlRunID, attemptID, ParentAddress, runtimeChildID)
}

func CompletionCallbackDigest(callback CompletionCallback) string {
	return digestStrings(
		"completion_callback", callback.AttemptID, callback.RuntimeChildID, string(callback.Status),
		callback.Branch, callback.BaseSHA, callback.HeadSHA, callback.PushedSHA,
		callback.OwnedPathsDigest, callback.TestsDigest, callback.HandoffsDigest,
		callback.ConcernsDigest, callback.ReportRef, callback.RecommendedParentAction,
	)
}

func EvidenceDigest(evidence Evidence) string {
	encoded, _ := json.Marshal(evidence)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func completedAcknowledgement(
	actions map[string]LifecycleAction,
	actionIDs []string,
	runtimeChildID, expectedDigest string,
) bool {
	for _, actionID := range actionIDs {
		action := actions[actionID]
		if action.Kind == agentharness.ActionAcknowledge && action.Completed && !action.Ambiguous &&
			sameStrings(action.Result.RuntimeWorkIDs, []string{runtimeChildID}) &&
			action.RequestDigest == expectedDigest && action.Result.ResultDigest == expectedDigest &&
			action.Result.MessageReceipt != "" {
			return true
		}
	}
	return false
}

func completedCallback(
	actions map[string]LifecycleAction,
	actionIDs []string,
	runtimeChildID, expectedDigest string,
) bool {
	for _, actionID := range actionIDs {
		action := actions[actionID]
		if action.Kind == agentharness.ActionCallback && action.Completed && !action.Ambiguous &&
			action.RequestDigest == expectedDigest && action.Result.ResultDigest == expectedDigest &&
			sameStrings(action.Result.RuntimeWorkIDs, []string{runtimeChildID}) {
			return true
		}
	}
	return false
}

func completedClose(
	actions map[string]LifecycleAction,
	actionIDs []string,
	expected WorkCloseEvidence,
) bool {
	for _, actionID := range actionIDs {
		action := actions[actionID]
		if action.Kind != agentharness.ActionClose || !action.Completed || action.Ambiguous {
			continue
		}
		actual, err := controlCloseEvidence(action.Result.CloseEvidence)
		if err == nil && actual == expected {
			return true
		}
	}
	return false
}

func ambiguityCode(action LifecycleAction, attempt PlacementAttempt) string {
	if action.Kind == agentharness.ActionDispatch && attempt.Primitive == agentharness.PersistentSession {
		return "ambiguous_create"
	}
	switch action.Kind {
	case agentharness.ActionDispatch:
		return "ambiguous_dispatch"
	case agentharness.ActionInterrupt:
		return "ambiguous_interrupt"
	case agentharness.ActionClose:
		return "ambiguous_close"
	case agentharness.ActionAcknowledge:
		return "ambiguous_acknowledge"
	case agentharness.ActionCallback:
		return "ambiguous_callback"
	default:
		return "ambiguous_action"
	}
}

func ambiguityError(code string) error {
	if code == "ambiguous_create" {
		return errors.Join(ErrAmbiguousDispatch, ErrAmbiguousCreate)
	}
	return ErrAmbiguousDispatch
}
