package controlplane

import (
	"context"
	"reflect"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/controlplane/journal"
)

// Store persists one authoritative Snapshot with compare-and-swap semantics
// as a derived checkpoint while the embedded typed journal remains the only
// write-order and replay authority.
type Store interface {
	journal.Store
	Create(context.Context, Snapshot) (Snapshot, error)
	Load(context.Context, string) (Snapshot, error)
	ReserveAction(context.Context, Snapshot, uint64, journal.Action) (Snapshot, bool, error)
	Save(context.Context, Snapshot, uint64) (Snapshot, error)
	EventsAfter(context.Context, string, uint64, int) ([]Event, uint64, error)
}

// JournalActionKind maps every supported harness action to its immutable
// journal kind. Unknown lifecycle kinds are invalid rather than cancellations.
func JournalActionKind(kind agentharness.ActionKind) (journal.Kind, error) {
	switch kind {
	case agentharness.ActionDispatch:
		return journal.KindDispatch, nil
	case agentharness.ActionObserve:
		return journal.KindObserve, nil
	case agentharness.ActionWait:
		return journal.KindWait, nil
	case agentharness.ActionSend:
		return journal.KindSend, nil
	case agentharness.ActionAcknowledge:
		return journal.KindAcknowledge, nil
	case agentharness.ActionCallback:
		return journal.KindCallback, nil
	case agentharness.ActionInterrupt:
		return journal.KindInterrupt, nil
	case agentharness.ActionClose:
		return journal.KindCloseRuntime, nil
	default:
		return "", invalidRecord("lifecycle action kind is invalid")
	}
}

// ValidateActionReservation validates the one action/snapshot transition that
// ReserveAction commits atomically. Store implementations must run it after
// their exact version check and before making either authority change durable.
func ValidateActionReservation(current, next Snapshot, reservation journal.Action) error {
	if err := ValidateSave(current, next); err != nil {
		return err
	}
	if err := journal.ValidateAction(reservation); err != nil {
		return err
	}
	if reservation.ControlRunID != current.Run.ID ||
		reservation.ExpectedProjection != current.Version ||
		reservation.GraphRevision != current.Graph.Revision {
		return journal.ErrConflict
	}
	if _, exists := current.Actions[reservation.ID]; exists {
		return journal.ErrConflict
	}
	added := 0
	for id := range next.Actions {
		if _, exists := current.Actions[id]; !exists {
			added++
			if id != reservation.ID {
				return journal.ErrConflict
			}
		}
	}
	if added != 1 {
		return journal.ErrConflict
	}
	prepared, ok := next.Actions[reservation.ID]
	if !ok {
		return journal.ErrConflict
	}
	preparedKind, kindErr := JournalActionKind(prepared.Kind)
	if kindErr != nil {
		return kindErr
	}
	if prepared.ID != reservation.ID || prepared.AttemptID != reservation.AttemptID ||
		preparedKind != reservation.Kind ||
		prepared.RequestDigest != reservation.CanonicalRequestDigest || prepared.PreparedAt.IsZero() ||
		prepared.Completed || prepared.Ambiguous {
		return journal.ErrConflict
	}
	previousAttempt, ok := current.Attempts[reservation.AttemptID]
	if !ok {
		return journal.ErrConflict
	}
	for _, actionID := range previousAttempt.ActionIDs {
		if actionID == reservation.ID {
			return journal.ErrConflict
		}
	}
	attempt, ok := next.Attempts[reservation.AttemptID]
	if !ok || attempt.TaskID != reservation.TaskID ||
		len(attempt.ActionIDs) != len(previousAttempt.ActionIDs)+1 ||
		attempt.ActionIDs[len(attempt.ActionIDs)-1] != reservation.ID {
		return journal.ErrConflict
	}
	withoutReservation := attempt
	withoutReservation.ActionIDs = append([]string(nil), attempt.ActionIDs[:len(attempt.ActionIDs)-1]...)
	if !reflect.DeepEqual(previousAttempt, withoutReservation) {
		return journal.ErrConflict
	}
	if reservation.IdempotencyKey != journalIdempotencyKey(
		reservation.ControlRunID, reservation.AttemptID, prepared.Kind, reservation.ID,
	) {
		return journal.ErrConflict
	}
	if len(next.Events) != len(current.Events)+1 {
		return journal.ErrConflict
	}
	preparedEvent := next.Events[len(next.Events)-1]
	if preparedEvent.Kind != EventActionPrepared || preparedEvent.ControlRunID != current.Run.ID ||
		preparedEvent.Cursor != current.Run.EventCursor+1 || preparedEvent.TaskID != reservation.TaskID ||
		preparedEvent.AttemptID != reservation.AttemptID || preparedEvent.ActionID != reservation.ID ||
		preparedEvent.Digest != digestStrings("prepare", reservation.ID, reservation.CanonicalRequestDigest) ||
		preparedEvent.CreatedAt.IsZero() || next.Run.UpdatedAt.IsZero() {
		return journal.ErrConflict
	}
	expected := CloneSnapshot(current)
	expected.Version = next.Version
	expected.Run.UpdatedAt = next.Run.UpdatedAt
	expected.Run.EventCursor = next.Run.EventCursor
	expected.Actions[reservation.ID] = prepared
	expected.Attempts[reservation.AttemptID] = attempt
	expected.Events = append(expected.Events, preparedEvent)
	if !reflect.DeepEqual(expected, next) {
		return journal.ErrConflict
	}
	return nil
}

// ValidateOutcomeReservation binds one terminal lifecycle transition to the
// exact immutable journal reservation returned by Store.Reservation.
func ValidateOutcomeReservation(
	next Snapshot,
	priorVersion uint64,
	actionID string,
	reservation journal.Action,
) error {
	if err := journal.ValidateAction(reservation); err != nil {
		return err
	}
	lifecycle, ok := next.Actions[actionID]
	if !ok {
		return journal.ErrConflict
	}
	attempt, ok := next.Attempts[lifecycle.AttemptID]
	if !ok {
		return journal.ErrConflict
	}
	lifecycleKind, err := JournalActionKind(lifecycle.Kind)
	if err != nil {
		return err
	}
	expected := journal.Action{
		ID: lifecycle.ID, ControlRunID: next.Run.ID, TaskID: attempt.TaskID,
		AttemptID: lifecycle.AttemptID, Kind: lifecycleKind,
		GraphRevision: reservation.GraphRevision, ExpectedProjection: reservation.ExpectedProjection,
		CanonicalRequestDigest: lifecycle.RequestDigest, IdempotencyKey: reservation.IdempotencyKey,
		AuthorityReceiptID: reservation.AuthorityReceiptID,
	}
	if actionID != lifecycle.ID || expected != reservation ||
		reservation.GraphRevision > next.Graph.Revision ||
		reservation.ExpectedProjection > priorVersion {
		return journal.ErrConflict
	}
	return nil
}
