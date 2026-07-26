// Package controlplane defines Pajé's provider-neutral durable Agent Control
// Plane. Records contain only stable identifiers, digests, bounded evidence,
// and lifecycle state; runtime provider objects and credentials are forbidden.
package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/araihu/paje/internal/agentharness"
)

const SchemaVersion = "paje.controlplane/v1"

const ParentAddress = "parent"

const PromotionTriggerNone = "none"

type Status string

const (
	StatusOpen     Status = "open"
	StatusClosing  Status = "closing"
	StatusClosed   Status = "closed"
	StatusBlocked  Status = "blocked"
	StatusCanceled Status = "canceled"
)

type TaskState string

const (
	TaskPending    TaskState = "pending"
	TaskReady      TaskState = "ready"
	TaskDispatched TaskState = "dispatched"
	TaskActive     TaskState = "active"
	TaskNeedsInput TaskState = "needs_input"
	TaskCompleted  TaskState = "completed"
	TaskFailed     TaskState = "failed"
	TaskCanceled   TaskState = "canceled"
)

type AttemptState string

const (
	AttemptReserved   AttemptState = "reserved"
	AttemptDispatched AttemptState = "dispatched"
	AttemptActive     AttemptState = "active"
	AttemptBlocked    AttemptState = "blocked"
	AttemptCompleted  AttemptState = "completed"
	AttemptFailed     AttemptState = "failed"
	AttemptCanceled   AttemptState = "canceled"
)

type SessionState string

const (
	SessionRegistered   SessionState = "registered"
	SessionAcknowledged SessionState = "acknowledged"
	SessionActive       SessionState = "active"
	SessionCompleted    SessionState = "completed"
	SessionFailed       SessionState = "failed"
	SessionCanceled     SessionState = "canceled"
	SessionArchived     SessionState = "archived"
)

type DispositionKind string

const (
	DispositionIntegrated DispositionKind = "integrated"
	DispositionHandedOff  DispositionKind = "handed_off"
	DispositionDiscarded  DispositionKind = "discarded"
)

type WorkCloseKind string

const (
	CloseArchive   WorkCloseKind = "archive"
	CloseRuntime   WorkCloseKind = "runtime_close"
	CloseAggregate WorkCloseKind = "aggregate"
	CloseCanceled  WorkCloseKind = "cancel"
	CloseInactive  WorkCloseKind = "inactive"
)

type EventKind string

const (
	EventPlacement       EventKind = "placement"
	EventActionPrepared  EventKind = "action_prepared"
	EventActionCompleted EventKind = "action_completed"
	EventSteering        EventKind = "steering"
	EventHandoff         EventKind = "handoff"
	EventEvidence        EventKind = "evidence"
	EventDisposition     EventKind = "disposition"
	EventClose           EventKind = "close"
)

type MessageKind string

const (
	MessageSteering          MessageKind = "steering"
	MessageDependencyHandoff MessageKind = "dependency_handoff"
	MessageNeedsInput        MessageKind = "needs_input"
)

type CallbackStatus string

const (
	CallbackDone             CallbackStatus = "done"
	CallbackDoneWithConcerns CallbackStatus = "done_with_concerns"
	CallbackBlocked          CallbackStatus = "blocked"
	CallbackNeedsInput       CallbackStatus = "needs_input"
)

type ControlRun struct {
	SchemaVersion string     `json:"schema_version"`
	ID            string     `json:"id"`
	PrincipalID   string     `json:"principal_id"`
	GoalDigest    string     `json:"goal_digest"`
	GraphRevision uint64     `json:"graph_revision"`
	Status        Status     `json:"status"`
	EventCursor   uint64     `json:"event_cursor"`
	Close         CloseState `json:"close"`
	CreatedAt     time.Time  `json:"created_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at,omitempty"`
}

type CloseState struct {
	Code                string          `json:"code,omitempty"`
	CombinedGatesPassed bool            `json:"combined_gates_passed"`
	Pending             PendingWorkGate `json:"pending"`
	ClosedAt            time.Time       `json:"closed_at,omitempty"`
}

type TaskGraph struct {
	SchemaVersion    string   `json:"schema_version"`
	ControlRunID     string   `json:"control_run_id"`
	Revision         uint64   `json:"revision"`
	Tasks            []Task   `json:"tasks"`
	IntegrationOrder []string `json:"integration_order"`
	CombinedGates    []Gate   `json:"combined_gates"`
}

type Task struct {
	ID            string              `json:"id"`
	Goal          string              `json:"goal"`
	DependsOn     []string            `json:"depends_on,omitempty"`
	Projects      []ProjectRef        `json:"projects"`
	Ownership     Ownership           `json:"ownership"`
	Placement     ExecutionPlacement  `json:"placement"`
	FrozenInputs  []FrozenInput       `json:"frozen_inputs"`
	Acceptance    []Gate              `json:"acceptance"`
	Communication []CommunicationEdge `json:"communication,omitempty"`
	State         TaskState           `json:"state"`
}

type ProjectRef struct {
	ID                string `json:"id"`
	Repository        string `json:"repository"`
	BaseRef           string `json:"base_ref"`
	BaseSHA           string `json:"base_sha"`
	WorkspaceScope    string `json:"workspace_scope"`
	CredentialScope   string `json:"credential_scope"`
	MailboxNamespace  string `json:"mailbox_namespace"`
	EvidenceNamespace string `json:"evidence_namespace"`
}

type Ownership struct {
	Mutable     []string `json:"mutable"`
	ParentOwned []string `json:"parent_owned,omitempty"`
	Forbidden   []string `json:"forbidden,omitempty"`
}

type ExecutionPlacement struct {
	ParallelismPrimitive   agentharness.Primitive    `json:"parallelism_primitive"`
	ExecutionPlacement     string                    `json:"execution_placement"`
	PlacementRationale     string                    `json:"placement_rationale"`
	CapabilityRequirements []agentharness.Capability `json:"capability_requirements"`
	LifecycleOwner         string                    `json:"lifecycle_owner"`
	Fallback               string                    `json:"fallback"`
	PromotionTrigger       string                    `json:"promotion_trigger"`
}

type FrozenInput struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

type Gate struct {
	ID       string `json:"id"`
	Digest   string `json:"digest"`
	Passed   bool   `json:"passed"`
	Evidence string `json:"evidence,omitempty"`
}

type CommunicationEdge struct {
	ProjectID string `json:"project_id"`
	TaskID    string `json:"task_id"`
}

type EvidenceRef struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

type WorkCloseEvidence struct {
	Kind    WorkCloseKind `json:"kind,omitempty"`
	Receipt string        `json:"receipt,omitempty"`
	Digest  string        `json:"digest,omitempty"`
}

type Disposition struct {
	Kind       DispositionKind `json:"kind,omitempty"`
	EvidenceID string          `json:"evidence_id,omitempty"`
	Reason     string          `json:"reason,omitempty"`
}

type PlacementAttempt struct {
	ID                    string                          `json:"id"`
	TaskID                string                          `json:"task_id"`
	Primitive             agentharness.Primitive          `json:"primitive"`
	CapabilitySnapshot    agentharness.CapabilitySnapshot `json:"capability_snapshot"`
	LifecycleOwner        string                          `json:"lifecycle_owner"`
	RuntimeWorkIDs        []string                        `json:"runtime_work_ids,omitempty"`
	LastCursor            string                          `json:"last_cursor,omitempty"`
	CursorSequence        uint64                          `json:"cursor_sequence"`
	TerminalObserved      bool                            `json:"terminal_observed"`
	State                 AttemptState                    `json:"state"`
	TerminalEvidence      EvidenceRef                     `json:"terminal_evidence"`
	Disposition           Disposition                     `json:"disposition"`
	CloseEvidence         WorkCloseEvidence               `json:"close_evidence"`
	ActionIDs             []string                        `json:"action_ids,omitempty"`
	ObservedEvents        map[string]string               `json:"observed_events,omitempty"`
	BlockCode             string                          `json:"block_code,omitempty"`
	PromotedFromAttemptID string                          `json:"promoted_from_attempt_id,omitempty"`
	HandoffID             string                          `json:"handoff_id,omitempty"`
	PromotionTrigger      string                          `json:"promotion_trigger"`
}

type AgentSession struct {
	ID                        string       `json:"id"`
	AttemptID                 string       `json:"attempt_id"`
	TaskID                    string       `json:"task_id"`
	HarnessID                 string       `json:"harness_id"`
	RuntimeChildID            string       `json:"runtime_child_id"`
	RuntimeIDAcknowledged     bool         `json:"runtime_id_acknowledged"`
	RegistrationMessageDigest string       `json:"registration_message_digest,omitempty"`
	LastCursor                string       `json:"last_cursor,omitempty"`
	State                     SessionState `json:"state"`
	Disposition               Disposition  `json:"disposition"`
	ArchiveReceipt            string       `json:"archive_receipt,omitempty"`
}

type PendingWorkGate struct {
	PersistentSessionsUnarchived int `json:"persistent_sessions_unarchived"`
	EphemeralAttemptsOpen        int `json:"ephemeral_attempts_open"`
	NativeFanoutsUnaggregated    int `json:"native_fanouts_unaggregated"`
	LocalAttemptsActive          int `json:"local_attempts_active"`
	TotalPendingWork             int `json:"total_pending_work"`
}

type TestEvidence struct {
	CommandDigest string `json:"command_digest"`
	ResultDigest  string `json:"result_digest"`
	Passed        bool   `json:"passed"`
}

type Evidence struct {
	ID               string         `json:"id"`
	TaskID           string         `json:"task_id"`
	AttemptID        string         `json:"attempt_id"`
	Branch           string         `json:"branch,omitempty"`
	BaseSHA          string         `json:"base_sha"`
	HeadSHA          string         `json:"head_sha"`
	PushedSHA        string         `json:"pushed_sha,omitempty"`
	OwnedPathsDigest string         `json:"owned_paths_digest"`
	ArtifactRef      string         `json:"artifact_ref,omitempty"`
	ReportRef        string         `json:"report_ref,omitempty"`
	Tests            []TestEvidence `json:"tests"`
	ReviewDigest     string         `json:"review_digest,omitempty"`
}

type Handoff struct {
	ID                      string      `json:"id"`
	ProducerTaskID          string      `json:"producer_task_id"`
	ConsumerTaskID          string      `json:"consumer_task_id"`
	FromOwner               string      `json:"from_owner"`
	ToOwner                 string      `json:"to_owner"`
	Evidence                EvidenceRef `json:"evidence"`
	AcknowledgementRequired bool        `json:"acknowledgement_required"`
	Acknowledged            bool        `json:"acknowledged"`
}

type Message struct {
	ID           string      `json:"id"`
	FromTaskID   string      `json:"from_task_id"`
	ToTaskID     string      `json:"to_task_id"`
	Kind         MessageKind `json:"kind"`
	Digest       string      `json:"digest"`
	Acknowledged bool        `json:"acknowledged"`
}

type CompletionCallback struct {
	AttemptID               string         `json:"attempt_id"`
	RuntimeChildID          string         `json:"runtime_child_id"`
	Status                  CallbackStatus `json:"status"`
	Branch                  string         `json:"branch,omitempty"`
	BaseSHA                 string         `json:"base_sha"`
	HeadSHA                 string         `json:"head_sha"`
	PushedSHA               string         `json:"pushed_sha,omitempty"`
	OwnedPathsDigest        string         `json:"owned_paths_digest"`
	TestsDigest             string         `json:"tests_digest"`
	HandoffsDigest          string         `json:"handoffs_digest,omitempty"`
	ConcernsDigest          string         `json:"concerns_digest,omitempty"`
	ReportRef               string         `json:"report_ref,omitempty"`
	RecommendedParentAction string         `json:"recommended_parent_action"`
}

type Event struct {
	Cursor       uint64    `json:"cursor"`
	ControlRunID string    `json:"control_run_id"`
	Kind         EventKind `json:"kind"`
	TaskID       string    `json:"task_id,omitempty"`
	AttemptID    string    `json:"attempt_id,omitempty"`
	ActionID     string    `json:"action_id,omitempty"`
	Digest       string    `json:"digest"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
}

type LifecycleAction struct {
	ID            string                    `json:"id"`
	AttemptID     string                    `json:"attempt_id"`
	Kind          agentharness.ActionKind   `json:"kind"`
	RequestDigest string                    `json:"request_digest"`
	Result        agentharness.ActionResult `json:"result"`
	PreparedAt    time.Time                 `json:"prepared_at,omitempty"`
	CompletedAt   time.Time                 `json:"completed_at,omitempty"`
	Completed     bool                      `json:"completed"`
	Ambiguous     bool                      `json:"ambiguous"`
	AmbiguityCode string                    `json:"ambiguity_code,omitempty"`
}

type Snapshot struct {
	SchemaVersion string                        `json:"schema_version"`
	Version       uint64                        `json:"version"`
	Run           ControlRun                    `json:"run"`
	Graph         TaskGraph                     `json:"graph"`
	Attempts      map[string]PlacementAttempt   `json:"attempts"`
	Sessions      map[string]AgentSession       `json:"sessions"`
	Evidence      map[string]Evidence           `json:"evidence"`
	Handoffs      map[string]Handoff            `json:"handoffs"`
	Messages      map[string]Message            `json:"messages"`
	Callbacks     map[string]CompletionCallback `json:"callbacks"`
	Actions       map[string]LifecycleAction    `json:"actions"`
	Events        []Event                       `json:"events"`
}

func NewSnapshot(run ControlRun, graph TaskGraph) (Snapshot, error) {
	if run.SchemaVersion == "" {
		run.SchemaVersion = SchemaVersion
	}
	if graph.SchemaVersion == "" {
		graph.SchemaVersion = SchemaVersion
	}
	normalizePromotionTriggers(&graph, nil)
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion, Version: 1, Run: run, Graph: CloneGraph(graph),
		Attempts: map[string]PlacementAttempt{}, Sessions: map[string]AgentSession{},
		Evidence: map[string]Evidence{}, Handoffs: map[string]Handoff{},
		Messages: map[string]Message{}, Callbacks: map[string]CompletionCallback{},
		Actions: map[string]LifecycleAction{}, Events: []Event{},
	}
	if err := ValidateSnapshot(snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func DecodeTaskGraph(encoded []byte) (TaskGraph, error) {
	var graph TaskGraph
	if err := strictDecode(encoded, &graph); err != nil {
		return TaskGraph{}, fmt.Errorf("%w: decode task graph: %v", ErrInvalidRecord, err)
	}
	normalizePromotionTriggers(&graph, nil)
	if err := ValidateGraph(graph, nil); err != nil {
		return TaskGraph{}, err
	}
	return graph, nil
}

func DecodeSnapshot(encoded []byte) (Snapshot, error) {
	var snapshot Snapshot
	if err := strictDecode(encoded, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("%w: decode snapshot: %v", ErrInvalidRecord, err)
	}
	normalizePromotionTriggers(&snapshot.Graph, snapshot.Attempts)
	if err := ValidateSnapshot(snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func ValidateSnapshot(snapshot Snapshot) error {
	if snapshot.SchemaVersion != SchemaVersion || snapshot.Version == 0 {
		return invalidRecord("schema version and durable version are required")
	}
	if snapshot.Attempts == nil || snapshot.Sessions == nil || snapshot.Evidence == nil ||
		snapshot.Handoffs == nil || snapshot.Messages == nil || snapshot.Callbacks == nil ||
		snapshot.Actions == nil || snapshot.Events == nil {
		return invalidRecord("durable collections must be canonical and non-null")
	}
	if snapshot.Run.SchemaVersion != SchemaVersion || snapshot.Graph.SchemaVersion != SchemaVersion ||
		snapshot.Run.ID == "" || snapshot.Run.ID != snapshot.Graph.ControlRunID ||
		snapshot.Run.PrincipalID == "" || !validDigest(snapshot.Run.GoalDigest) ||
		snapshot.Run.GraphRevision != snapshot.Graph.Revision || !validStatus(snapshot.Run.Status) {
		return invalidRecord("run identity or state is invalid")
	}
	if err := ValidateGraph(snapshot.Graph, nil); err != nil {
		return err
	}
	activeAttemptByTask := make(map[string]string)
	for id, attempt := range snapshot.Attempts {
		if id == "" || attempt.ID != id || taskByID(snapshot.Graph, attempt.TaskID) == nil ||
			!agentharness.ValidPrimitive(attempt.Primitive) || attempt.LifecycleOwner == "" ||
			!validAttemptState(attempt.State) {
			return invalidRecord("placement attempt is invalid")
		}
		if err := attempt.CapabilitySnapshot.Validate(); err != nil {
			return invalidRecord("attempt capability snapshot is invalid")
		}
		if !attemptTerminal(attempt.State) {
			if previous := activeAttemptByTask[attempt.TaskID]; previous != "" {
				return invalidRecord("task has overlapping active placement attempts")
			}
			activeAttemptByTask[attempt.TaskID] = attempt.ID
		}
		if (attempt.State == AttemptBlocked) != (attempt.BlockCode != "") {
			return invalidRecord("blocked attempt classification is invalid")
		}
		caps, ok := attempt.CapabilitySnapshot.Primitives[attempt.Primitive]
		if !ok {
			return invalidRecord("attempt capability snapshot does not support primitive")
		}
		for _, requirement := range agentharness.RequiredCapabilities(attempt.Primitive) {
			if !caps.Supports(requirement) {
				return invalidRecord("attempt capability requirement is unsatisfied")
			}
		}
	}
	for id, session := range snapshot.Sessions {
		attempt, ok := snapshot.Attempts[session.AttemptID]
		if id == "" || session.ID != id || !ok || attempt.Primitive != agentharness.PersistentSession ||
			session.RuntimeChildID == "" || session.HarnessID == "" || !validSessionState(session.State) {
			return invalidRecord("persistent session is invalid")
		}
		if session.RuntimeIDAcknowledged {
			expected := RegistrationMessageDigest(snapshot.Run.ID, session.AttemptID, session.RuntimeChildID)
			if session.RegistrationMessageDigest != expected ||
				!completedAcknowledgement(snapshot.Actions, attempt.ActionIDs, session.RuntimeChildID, expected) {
				return invalidRecord("persistent session acknowledgement is not ledger-backed")
			}
		}
	}
	for id, evidence := range snapshot.Evidence {
		if evidence.ID != id {
			return invalidRecord("evidence identity is invalid")
		}
		if err := validateEvidence(evidence, &snapshot); err != nil {
			return err
		}
	}
	for _, attempt := range snapshot.Attempts {
		if attempt.TerminalEvidence.ID != "" {
			evidence := snapshot.Evidence[attempt.TerminalEvidence.ID]
			if evidence.ID == "" || evidence.AttemptID != attempt.ID ||
				evidence.TaskID != attempt.TaskID || attempt.TerminalEvidence.Digest != EvidenceDigest(evidence) {
				return invalidRecord("attempt terminal evidence is missing or foreign")
			}
		}
		if attempt.Disposition.Kind != "" {
			evidence := snapshot.Evidence[attempt.Disposition.EvidenceID]
			if !validDisposition(attempt.Disposition) || evidence.ID == "" ||
				evidence.AttemptID != attempt.ID {
				return invalidRecord("attempt disposition is invalid")
			}
		}
		if attempt.CloseEvidence.Kind != "" {
			if err := validateCloseEvidence(attempt.Primitive, attempt.CloseEvidence); err != nil {
				return err
			}
			if attempt.Primitive != agentharness.LocalSequential &&
				!completedClose(snapshot.Actions, attempt.ActionIDs, attempt.CloseEvidence) {
				return invalidRecord("non-local close evidence is not ledger-backed")
			}
		}
		if attempt.PromotedFromAttemptID != "" || attempt.HandoffID != "" {
			if !validPromotedAttempt(snapshot, attempt) {
				return invalidRecord("promoted placement attempt is invalid")
			}
		}
	}
	for _, session := range snapshot.Sessions {
		if session.Disposition.Kind != "" {
			evidence := snapshot.Evidence[session.Disposition.EvidenceID]
			if !validDisposition(session.Disposition) || evidence.ID == "" ||
				evidence.AttemptID != session.AttemptID {
				return invalidRecord("session disposition is invalid")
			}
		}
	}
	for id, handoff := range snapshot.Handoffs {
		if handoff.ID != id || taskByID(snapshot.Graph, handoff.ProducerTaskID) == nil ||
			taskByID(snapshot.Graph, handoff.ConsumerTaskID) == nil ||
			snapshot.Evidence[handoff.Evidence.ID].ID == "" ||
			handoff.Evidence.Digest != EvidenceDigest(snapshot.Evidence[handoff.Evidence.ID]) {
			return invalidRecord("dependency handoff is invalid")
		}
	}
	var cursor uint64
	for _, event := range snapshot.Events {
		if event.ControlRunID != snapshot.Run.ID || event.Cursor != cursor+1 ||
			event.Kind == "" || !validDigest(event.Digest) {
			return invalidRecord("event stream is invalid")
		}
		cursor = event.Cursor
	}
	if snapshot.Run.EventCursor != cursor {
		return invalidRecord("event cursor does not match append-only stream")
	}
	for id, action := range snapshot.Actions {
		_, kindErr := JournalActionKind(action.Kind)
		if action.ID != id || snapshot.Attempts[action.AttemptID].ID == "" ||
			action.RequestDigest == "" || kindErr != nil {
			return invalidRecord("lifecycle action is invalid")
		}
		if action.Completed && action.Result.ActionID != action.ID {
			return invalidRecord("completed action result is not bound")
		}
		if action.Completed && action.Ambiguous || action.Ambiguous != (action.AmbiguityCode != "") {
			return invalidRecord("lifecycle action ambiguity classification is invalid")
		}
		if action.Completed {
			attempt := snapshot.Attempts[action.AttemptID]
			expectedRuntimeIDs := attempt.RuntimeWorkIDs
			if action.Kind == agentharness.ActionDispatch {
				expectedRuntimeIDs = nil
			}
			if err := agentharness.ValidateActionResult(
				action.Kind, attempt.Primitive, expectedRuntimeIDs, 0,
				attempt.CapabilitySnapshot.Primitives[attempt.Primitive].Supports(agentharness.CapCursor), action.Result,
			); err != nil {
				return invalidRecord("completed action result is invalid")
			}
		}
	}
	for id, message := range snapshot.Messages {
		if message.ID != id || !validMessageKind(message.Kind) ||
			!validDigest(message.Digest) ||
			!messageScopeAllowed(snapshot.Graph, message.FromTaskID, message.ToTaskID) {
			return invalidRecord("mailbox message is invalid")
		}
	}
	for attemptID, callback := range snapshot.Callbacks {
		attempt, ok := snapshot.Attempts[attemptID]
		if !ok || attempt.Primitive != agentharness.PersistentSession ||
			callback.AttemptID != attemptID || !validCallback(callback) ||
			!completedCallback(snapshot.Actions, attempt.ActionIDs, callback.RuntimeChildID, CompletionCallbackDigest(callback)) {
			return invalidRecord("persistent completion callback is invalid")
		}
	}
	return nil
}

func ValidateSave(current, next Snapshot) error {
	if err := ValidateSnapshot(current); err != nil {
		return err
	}
	if err := ValidateSnapshot(next); err != nil {
		return err
	}
	if next.Version != current.Version+1 ||
		current.SchemaVersion != next.SchemaVersion ||
		current.Run.SchemaVersion != next.Run.SchemaVersion ||
		current.Run.ID != next.Run.ID ||
		current.Run.PrincipalID != next.Run.PrincipalID ||
		current.Run.GoalDigest != next.Run.GoalDigest ||
		!current.Run.CreatedAt.Equal(next.Run.CreatedAt) {
		return fmt.Errorf("%w: durable run identity changed", ErrImmutableBoundary)
	}
	terminalOutcomes := 0
	for id, nextAction := range next.Actions {
		previous := current.Actions[id]
		if !previous.Completed && nextAction.Completed ||
			!previous.Ambiguous && nextAction.Ambiguous {
			terminalOutcomes++
		}
	}
	if terminalOutcomes > 1 {
		return invalidRecord("save introduces multiple terminal action outcomes")
	}
	if next.Graph.Revision < current.Graph.Revision ||
		next.Graph.Revision > current.Graph.Revision+1 {
		return ErrVersionConflict
	}
	if len(next.Events) < len(current.Events) ||
		!reflect.DeepEqual(next.Events[:len(current.Events)], current.Events) {
		return fmt.Errorf("%w: append-only event history changed", ErrImmutableBoundary)
	}
	if current.Run.Status == StatusClosed && !reflect.DeepEqual(current, withVersion(next, current.Version)) {
		return fmt.Errorf("%w: closed control run changed", ErrImmutableBoundary)
	}
	for id, evidence := range current.Evidence {
		if !reflect.DeepEqual(evidence, next.Evidence[id]) {
			return ErrEvidenceImmutable
		}
	}
	for id, action := range current.Actions {
		nextAction, ok := next.Actions[id]
		if !ok || action.ID != nextAction.ID || action.AttemptID != nextAction.AttemptID ||
			action.Kind != nextAction.Kind || action.RequestDigest != nextAction.RequestDigest ||
			!action.PreparedAt.Equal(nextAction.PreparedAt) {
			return fmt.Errorf("%w: lifecycle action identity changed", ErrImmutableBoundary)
		}
		if action.Completed && !reflect.DeepEqual(action, nextAction) {
			return fmt.Errorf("%w: completed action changed", ErrImmutableBoundary)
		}
	}
	for id, attempt := range current.Attempts {
		nextAttempt, ok := next.Attempts[id]
		if !ok {
			return fmt.Errorf("%w: placement attempt removed", ErrImmutableBoundary)
		}
		if attempt.Disposition.Kind != "" && attempt.Disposition != nextAttempt.Disposition {
			return ErrEvidenceImmutable
		}
		if attempt.TerminalEvidence.ID != "" && attempt.TerminalEvidence != nextAttempt.TerminalEvidence {
			return ErrEvidenceImmutable
		}
		if attempt.CloseEvidence.Kind != "" && attempt.CloseEvidence != nextAttempt.CloseEvidence {
			return fmt.Errorf("%w: close evidence changed", ErrImmutableBoundary)
		}
	}
	for _, task := range current.Graph.Tasks {
		if !taskBoundaryActive(task.State) {
			continue
		}
		nextTask := taskByID(next.Graph, task.ID)
		if nextTask == nil || task.Goal != nextTask.Goal ||
			!reflect.DeepEqual(task.Projects, nextTask.Projects) ||
			!reflect.DeepEqual(task.FrozenInputs, nextTask.FrozenInputs) {
			return fmt.Errorf("%w: active task %q changed", ErrImmutableBoundary, task.ID)
		}
		if (!reflect.DeepEqual(task.Ownership, nextTask.Ownership) ||
			!reflect.DeepEqual(task.Placement, nextTask.Placement)) &&
			!validPromotionSave(current, next, task, *nextTask) {
			return fmt.Errorf("%w: active task %q changed outside promotion", ErrImmutableBoundary, task.ID)
		}
	}
	return nil
}

func withVersion(snapshot Snapshot, version uint64) Snapshot {
	cloned := CloneSnapshot(snapshot)
	cloned.Version = version
	return cloned
}

func ValidateGraph(graph TaskGraph, previous *TaskGraph) error {
	if graph.SchemaVersion != SchemaVersion || graph.ControlRunID == "" || graph.Revision == 0 ||
		len(graph.Tasks) == 0 || len(graph.IntegrationOrder) != len(graph.Tasks) ||
		len(graph.CombinedGates) == 0 {
		return invalidGraph("version, run, tasks, complete integration order, and combined gates are required")
	}
	tasks := make(map[string]Task, len(graph.Tasks))
	projects := make(map[string]ProjectRef)
	for _, task := range graph.Tasks {
		if task.ID == "" || strings.TrimSpace(task.Goal) == "" {
			return invalidGraph("task identity and goal are required")
		}
		if _, duplicate := tasks[task.ID]; duplicate {
			return invalidGraph("duplicate task ID")
		}
		if err := validateTask(task); err != nil {
			return err
		}
		for _, project := range task.Projects {
			if existing, ok := projects[project.ID]; ok && !reflect.DeepEqual(existing, project) {
				return invalidGraph("project ID is rebound to a different immutable identity")
			}
			projects[project.ID] = project
		}
		tasks[task.ID] = task
	}
	for _, task := range graph.Tasks {
		for _, predecessor := range task.DependsOn {
			if predecessor == task.ID || tasks[predecessor].ID == "" {
				return invalidGraph("missing or self predecessor")
			}
		}
		for _, edge := range task.Communication {
			target, ok := tasks[edge.TaskID]
			if !ok || !contains(task.DependsOn, edge.TaskID) || projectByID(target, edge.ProjectID) == nil {
				return invalidGraph("communication edge is not declared by dependency")
			}
		}
	}
	if graphHasCycle(tasks) {
		return invalidGraph("task graph contains a cycle")
	}
	if err := validateIntegrationOrder(graph, tasks); err != nil {
		return err
	}
	if err := validateActiveOwnership(graph.Tasks); err != nil {
		return err
	}
	if err := validateProjectIsolation(graph.Tasks); err != nil {
		return err
	}
	if previous != nil {
		if graph.ControlRunID != previous.ControlRunID || graph.Revision != previous.Revision+1 {
			return invalidGraph("graph compare-and-swap revision is invalid")
		}
		previousProjects := make(map[string]ProjectRef)
		for _, oldTask := range previous.Tasks {
			for _, project := range oldTask.Projects {
				previousProjects[project.ID] = project
			}
		}
		for id, project := range projects {
			if previousProject, exists := previousProjects[id]; exists && !reflect.DeepEqual(previousProject, project) {
				return fmt.Errorf("%w: project %q identity changed", ErrImmutableBoundary, id)
			}
		}
		for id := range previousProjects {
			if _, retained := projects[id]; !retained {
				return fmt.Errorf("%w: project %q identity was removed", ErrImmutableBoundary, id)
			}
		}
		for _, oldTask := range previous.Tasks {
			if !taskBoundaryActive(oldTask.State) {
				continue
			}
			next := tasks[oldTask.ID]
			if next.ID == "" || oldTask.Goal != next.Goal ||
				!reflect.DeepEqual(oldTask.Projects, next.Projects) ||
				!reflect.DeepEqual(oldTask.Ownership, next.Ownership) ||
				!reflect.DeepEqual(oldTask.FrozenInputs, next.FrozenInputs) {
				return fmt.Errorf("%w: task %q", ErrImmutableBoundary, oldTask.ID)
			}
		}
	}
	return nil
}

func ReadyTasks(graph TaskGraph) ([]Task, error) {
	if err := ValidateGraph(graph, nil); err != nil {
		return nil, err
	}
	tasks := make(map[string]Task, len(graph.Tasks))
	for _, task := range graph.Tasks {
		tasks[task.ID] = task
	}
	ready := make([]Task, 0)
	for _, task := range graph.Tasks {
		if task.State != TaskPending && task.State != TaskReady {
			continue
		}
		satisfied := true
		for _, predecessor := range task.DependsOn {
			if tasks[predecessor].State != TaskCompleted {
				satisfied = false
				break
			}
		}
		if satisfied {
			ready = append(ready, task)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
	return ready, nil
}

func CloneGraph(graph TaskGraph) TaskGraph {
	var cloned TaskGraph
	cloneJSON(graph, &cloned)
	return cloned
}

func CloneSnapshot(snapshot Snapshot) Snapshot {
	var cloned Snapshot
	cloneJSON(snapshot, &cloned)
	return cloned
}

func PendingWork(snapshot Snapshot) PendingWorkGate {
	var gate PendingWorkGate
	for _, attempt := range snapshot.Attempts {
		switch attempt.Primitive {
		case agentharness.PersistentSession:
			archived := false
			for _, session := range snapshot.Sessions {
				if session.AttemptID == attempt.ID && session.ArchiveReceipt != "" {
					archived = true
					break
				}
			}
			if !archived {
				gate.PersistentSessionsUnarchived++
			}
		case agentharness.EphemeralSubagent:
			if attempt.CloseEvidence.Kind != CloseRuntime || attempt.CloseEvidence.Receipt == "" {
				gate.EphemeralAttemptsOpen++
			}
		case agentharness.HarnessNativeParallel:
			if (attempt.CloseEvidence.Kind != CloseAggregate && attempt.CloseEvidence.Kind != CloseCanceled) ||
				attempt.CloseEvidence.Receipt == "" {
				gate.NativeFanoutsUnaggregated++
			}
		case agentharness.LocalSequential:
			if attempt.CloseEvidence.Kind != CloseInactive || attempt.CloseEvidence.Receipt == "" {
				gate.LocalAttemptsActive++
			}
		}
	}
	gate.TotalPendingWork = gate.PersistentSessionsUnarchived + gate.EphemeralAttemptsOpen +
		gate.NativeFanoutsUnaggregated + gate.LocalAttemptsActive
	return gate
}

func CloseSnapshot(snapshot Snapshot, now time.Time) (Snapshot, error) {
	if err := ValidateSnapshot(snapshot); err != nil {
		return Snapshot{}, err
	}
	candidate := CloneSnapshot(snapshot)
	gate := PendingWork(candidate)
	candidate.Run.Close.Pending = gate
	if gate != (PendingWorkGate{}) {
		candidate.Run.Status = StatusClosing
		candidate.Run.Close.Code = "cleanup_incomplete"
		return Snapshot{}, fmt.Errorf("%w: pending work remains", ErrCleanupIncomplete)
	}
	attemptsByTask := make(map[string]int, len(candidate.Graph.Tasks))
	for _, attempt := range candidate.Attempts {
		attemptsByTask[attempt.TaskID]++
	}
	for _, task := range candidate.Graph.Tasks {
		if !taskTerminal(task.State) {
			return Snapshot{}, fmt.Errorf("%w: task %q is not terminal", ErrClosePrecondition, task.ID)
		}
		if attemptsByTask[task.ID] == 0 {
			return Snapshot{}, fmt.Errorf("%w: task %q has no placement attempt", ErrClosePrecondition, task.ID)
		}
	}
	for _, attempt := range candidate.Attempts {
		if !attemptTerminal(attempt.State) || !validDisposition(attempt.Disposition) ||
			attempt.TerminalEvidence.ID == "" {
			return Snapshot{}, fmt.Errorf("%w: attempt %q is incomplete", ErrClosePrecondition, attempt.ID)
		}
	}
	for _, session := range candidate.Sessions {
		if !validDisposition(session.Disposition) || session.ArchiveReceipt == "" {
			return Snapshot{}, fmt.Errorf("%w: session %q is incomplete", ErrCleanupIncomplete, session.ID)
		}
	}
	for _, handoff := range candidate.Handoffs {
		if handoff.AcknowledgementRequired && !handoff.Acknowledged {
			return Snapshot{}, fmt.Errorf("%w: handoff %q is unacknowledged", ErrClosePrecondition, handoff.ID)
		}
	}
	for _, gate := range candidate.Graph.CombinedGates {
		if !gate.Passed {
			return Snapshot{}, fmt.Errorf("%w: combined gate %q did not pass", ErrClosePrecondition, gate.ID)
		}
	}
	candidate.Run.Status = StatusClosed
	candidate.Run.Close = CloseState{
		Code: "closed", CombinedGatesPassed: true, Pending: PendingWorkGate{}, ClosedAt: now,
	}
	candidate.Run.UpdatedAt = now
	return candidate, nil
}

func strictDecode(encoded []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func validateTask(task Task) error {
	if len(task.Projects) == 0 || len(task.FrozenInputs) == 0 || len(task.Acceptance) == 0 ||
		!validTaskState(task.State) {
		return invalidGraph("task projects, frozen inputs, acceptance, and state are required")
	}
	if err := validatePlacementFields(task.Placement); err != nil {
		return err
	}
	if err := validateOwnership(task.Ownership); err != nil {
		return err
	}
	projectIDs := make(map[string]bool, len(task.Projects))
	for _, project := range task.Projects {
		if project.ID == "" || projectIDs[project.ID] || project.Repository == "" ||
			project.BaseRef == "" || !validGitSHA(project.BaseSHA) ||
			project.WorkspaceScope == "" || project.CredentialScope == "" ||
			project.MailboxNamespace == "" || project.EvidenceNamespace == "" {
			return invalidGraph("project reference must bind immutable base and isolated namespaces")
		}
		projectIDs[project.ID] = true
	}
	for _, frozen := range task.FrozenInputs {
		if frozen.ID == "" || !validDigest(frozen.Digest) {
			return invalidGraph("frozen input is invalid")
		}
	}
	for _, gate := range task.Acceptance {
		if gate.ID == "" || !validDigest(gate.Digest) ||
			(gate.Passed && !validDigest(gate.Evidence)) {
			return invalidGraph("acceptance gate is invalid")
		}
	}
	return nil
}

func validateOwnership(ownership Ownership) error {
	sets := [][]string{ownership.Mutable, ownership.ParentOwned, ownership.Forbidden}
	for _, values := range sets {
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			canonical := cleanOwnership(value)
			if canonical == "" || canonical != value || seen[canonical] {
				return invalidGraph("ownership path is invalid or duplicated")
			}
			seen[canonical] = true
		}
	}
	if ownershipOverlaps(ownership.Mutable, ownership.ParentOwned) ||
		ownershipOverlaps(ownership.Mutable, ownership.Forbidden) {
		return invalidGraph("mutable ownership overlaps parent-owned or forbidden paths")
	}
	return nil
}

func validatePlacementFields(placement ExecutionPlacement) error {
	if !agentharness.ValidPrimitive(placement.ParallelismPrimitive) ||
		strings.TrimSpace(placement.ExecutionPlacement) == "" ||
		strings.TrimSpace(placement.PlacementRationale) == "" ||
		len(placement.CapabilityRequirements) == 0 ||
		strings.TrimSpace(placement.LifecycleOwner) == "" ||
		strings.TrimSpace(placement.Fallback) == "" {
		return fmt.Errorf("%w: all placement fields are required", ErrInvalidPlacement)
	}
	if !safeFallback(placement.ParallelismPrimitive, placement.Fallback) {
		return fmt.Errorf("%w: unsafe fallback %q for %s", ErrInvalidPlacement, placement.Fallback, placement.ParallelismPrimitive)
	}
	required := agentharness.RequiredCapabilities(placement.ParallelismPrimitive)
	for _, capability := range required {
		if !contains(placement.CapabilityRequirements, capability) {
			return fmt.Errorf("%w: primitive %s requires %s", ErrInvalidPlacement, placement.ParallelismPrimitive, capability)
		}
	}
	return nil
}

func safeFallback(primitive agentharness.Primitive, fallback string) bool {
	switch primitive {
	case agentharness.PersistentSession:
		return fallback == "block"
	case agentharness.EphemeralSubagent:
		return fallback == "local_sequential" || fallback == "block"
	case agentharness.HarnessNativeParallel:
		return fallback == "persistent_session_or_local_sequential" ||
			fallback == "persistent_session" || fallback == "local_sequential" ||
			fallback == "block"
	case agentharness.LocalSequential:
		return fallback == "block"
	default:
		return false
	}
}

func validateIntegrationOrder(graph TaskGraph, tasks map[string]Task) error {
	seen := make(map[string]int, len(graph.IntegrationOrder))
	for index, id := range graph.IntegrationOrder {
		if tasks[id].ID == "" {
			return invalidGraph("integration order references missing task")
		}
		if _, duplicate := seen[id]; duplicate {
			return invalidGraph("integration order is ambiguous")
		}
		seen[id] = index
	}
	for _, task := range graph.Tasks {
		for _, predecessor := range task.DependsOn {
			if seen[predecessor] >= seen[task.ID] {
				return invalidGraph("integration order violates dependencies")
			}
		}
	}
	for _, gate := range graph.CombinedGates {
		if gate.ID == "" || !validDigest(gate.Digest) ||
			(gate.Passed && !validDigest(gate.Evidence)) {
			return invalidGraph("combined gate is invalid")
		}
	}
	return nil
}

func validateActiveOwnership(tasks []Task) error {
	for i := range tasks {
		if !ownershipActive(tasks[i].State) {
			continue
		}
		for j := i + 1; j < len(tasks); j++ {
			if !ownershipActive(tasks[j].State) || !sameProject(tasks[i], tasks[j]) {
				continue
			}
			if ownershipOverlaps(tasks[i].Ownership.Mutable, tasks[j].Ownership.Mutable) {
				return fmt.Errorf("%w: tasks %q and %q", ErrOwnershipConflict, tasks[i].ID, tasks[j].ID)
			}
		}
	}
	return nil
}

func validateProjectIsolation(tasks []Task) error {
	type namespace struct {
		repository, workspace, credential, mailbox, evidence string
	}
	var all []namespace
	for _, task := range tasks {
		for _, project := range task.Projects {
			current := namespace{project.Repository, project.WorkspaceScope, project.CredentialScope, project.MailboxNamespace, project.EvidenceNamespace}
			for _, previous := range all {
				if previous.repository == current.repository {
					continue
				}
				if previous.workspace == current.workspace || previous.credential == current.credential ||
					previous.mailbox == current.mailbox || previous.evidence == current.evidence {
					return invalidGraph("unrelated projects share isolation namespace")
				}
			}
			all = append(all, current)
		}
	}
	return nil
}

func graphHasCycle(tasks map[string]Task) bool {
	visiting := make(map[string]bool, len(tasks))
	visited := make(map[string]bool, len(tasks))
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, predecessor := range tasks[id].DependsOn {
			if visit(predecessor) {
				return true
			}
		}
		delete(visiting, id)
		visited[id] = true
		return false
	}
	for id := range tasks {
		if visit(id) {
			return true
		}
	}
	return false
}

func ownershipOverlaps(first, second []string) bool {
	for _, left := range first {
		for _, right := range second {
			if pathPatternsOverlap(left, right) {
				return true
			}
		}
	}
	return false
}

func pathPatternsOverlap(first, second string) bool {
	first = cleanOwnership(first)
	second = cleanOwnership(second)
	if first == "" || second == "" {
		return false
	}
	if first == second {
		return true
	}
	firstPrefix := strings.TrimSuffix(first, "/**")
	secondPrefix := strings.TrimSuffix(second, "/**")
	if strings.HasSuffix(first, "/**") &&
		(second == firstPrefix || strings.HasPrefix(second, firstPrefix+"/")) {
		return true
	}
	if strings.HasSuffix(second, "/**") &&
		(first == secondPrefix || strings.HasPrefix(first, secondPrefix+"/")) {
		return true
	}
	return false
}

func cleanOwnership(value string) string {
	if strings.ContainsRune(value, '\\') || strings.HasPrefix(value, "/") ||
		strings.Contains(value, "..") {
		return ""
	}
	return strings.TrimPrefix(path.Clean(value), "./")
}

func sameProject(first, second Task) bool {
	for _, left := range first.Projects {
		for _, right := range second.Projects {
			if left.Repository == right.Repository && left.BaseSHA == right.BaseSHA {
				return true
			}
		}
	}
	return false
}

func taskByID(graph TaskGraph, id string) *Task {
	for index := range graph.Tasks {
		if graph.Tasks[index].ID == id {
			return &graph.Tasks[index]
		}
	}
	return nil
}

func projectByID(task Task, id string) *ProjectRef {
	for index := range task.Projects {
		if task.Projects[index].ID == id {
			return &task.Projects[index]
		}
	}
	return nil
}

func taskBoundaryActive(state TaskState) bool {
	return state == TaskDispatched || state == TaskActive || state == TaskNeedsInput
}

func ownershipActive(state TaskState) bool {
	return state == TaskReady || taskBoundaryActive(state)
}

func taskTerminal(state TaskState) bool {
	return state == TaskCompleted || state == TaskFailed || state == TaskCanceled
}

func attemptTerminal(state AttemptState) bool {
	return state == AttemptCompleted || state == AttemptFailed || state == AttemptCanceled
}

func validStatus(status Status) bool {
	switch status {
	case StatusOpen, StatusClosing, StatusClosed, StatusBlocked, StatusCanceled:
		return true
	default:
		return false
	}
}

func validTaskState(state TaskState) bool {
	switch state {
	case TaskPending, TaskReady, TaskDispatched, TaskActive, TaskNeedsInput,
		TaskCompleted, TaskFailed, TaskCanceled:
		return true
	default:
		return false
	}
}

func validAttemptState(state AttemptState) bool {
	switch state {
	case AttemptReserved, AttemptDispatched, AttemptActive, AttemptBlocked,
		AttemptCompleted, AttemptFailed, AttemptCanceled:
		return true
	default:
		return false
	}
}

func validSessionState(state SessionState) bool {
	switch state {
	case SessionRegistered, SessionAcknowledged, SessionActive, SessionCompleted,
		SessionFailed, SessionCanceled, SessionArchived:
		return true
	default:
		return false
	}
}

func validDisposition(disposition Disposition) bool {
	switch disposition.Kind {
	case DispositionIntegrated, DispositionHandedOff, DispositionDiscarded:
		return disposition.EvidenceID != "" && (disposition.Kind != DispositionDiscarded || disposition.Reason != "")
	default:
		return false
	}
}

func validMessageKind(kind MessageKind) bool {
	switch kind {
	case MessageSteering, MessageDependencyHandoff, MessageNeedsInput:
		return true
	default:
		return false
	}
}

func validCallback(callback CompletionCallback) bool {
	switch callback.Status {
	case CallbackDone, CallbackDoneWithConcerns, CallbackBlocked, CallbackNeedsInput:
	default:
		return false
	}
	return callback.AttemptID != "" && callback.RuntimeChildID != "" &&
		validGitSHA(callback.BaseSHA) && validGitSHA(callback.HeadSHA) &&
		(callback.PushedSHA == "" || validGitSHA(callback.PushedSHA)) &&
		validDigest(callback.OwnedPathsDigest) && validDigest(callback.TestsDigest) &&
		strings.TrimSpace(callback.RecommendedParentAction) != ""
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func validGitSHA(value string) bool {
	if validDigest(value) {
		return true
	}
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func contains[T comparable](values []T, want T) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func cloneJSON(source, destination any) {
	encoded, err := json.Marshal(source)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(encoded, destination); err != nil {
		panic(err)
	}
}

func invalidRecord(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidRecord, message)
}

func invalidGraph(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidGraph, message)
}

func validPromotedAttempt(snapshot Snapshot, attempt PlacementAttempt) bool {
	if attempt.PromotedFromAttemptID == "" || attempt.HandoffID == "" ||
		attempt.Primitive != agentharness.PersistentSession {
		return false
	}
	source, ok := snapshot.Attempts[attempt.PromotedFromAttemptID]
	if !ok || source.TaskID != attempt.TaskID || source.Primitive != agentharness.EphemeralSubagent ||
		!attemptTerminal(source.State) || source.TerminalEvidence.ID == "" ||
		source.CloseEvidence.Kind != CloseRuntime || source.CloseEvidence.Receipt == "" ||
		source.Disposition.Kind != DispositionHandedOff || source.LifecycleOwner == attempt.LifecycleOwner {
		return false
	}
	handoff, ok := snapshot.Handoffs[attempt.HandoffID]
	return ok && handoff.ProducerTaskID == attempt.TaskID && handoff.ConsumerTaskID == attempt.TaskID &&
		handoff.FromOwner == source.LifecycleOwner && handoff.ToOwner == attempt.LifecycleOwner &&
		handoff.Evidence == source.TerminalEvidence && handoff.AcknowledgementRequired
}

func validPromotionSave(current, next Snapshot, oldTask, nextTask Task) bool {
	if nextTask.State != TaskDispatched ||
		nextTask.Placement.ParallelismPrimitive != agentharness.PersistentSession {
		return false
	}
	matched := 0
	for id, replacement := range next.Attempts {
		if _, existed := current.Attempts[id]; existed || replacement.TaskID != oldTask.ID ||
			replacement.PromotedFromAttemptID == "" {
			continue
		}
		source, ok := current.Attempts[replacement.PromotedFromAttemptID]
		if !ok || source.TaskID != oldTask.ID || !validPromotedAttempt(next, replacement) ||
			replacement.LifecycleOwner != nextTask.Placement.LifecycleOwner {
			return false
		}
		matched++
	}
	return matched == 1
}

func normalizePromotionTriggers(graph *TaskGraph, attempts map[string]PlacementAttempt) {
	for index := range graph.Tasks {
		if strings.TrimSpace(graph.Tasks[index].Placement.PromotionTrigger) == "" {
			graph.Tasks[index].Placement.PromotionTrigger = PromotionTriggerNone
		}
	}
	for id, attempt := range attempts {
		if strings.TrimSpace(attempt.PromotionTrigger) == "" {
			attempt.PromotionTrigger = PromotionTriggerNone
			attempts[id] = attempt
		}
	}
}
