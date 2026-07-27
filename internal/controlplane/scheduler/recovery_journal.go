package scheduler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/araihu/paje/internal/controlplane/admission"
	"github.com/araihu/paje/internal/controlplane/journal"
)

const (
	schedulerComponentName        = "scheduler"
	schedulerRecoverySchema       = uint32(1)
	schedulerRecoveryRunID        = "scheduler-recovery"
	operationRecoveryRegister     = "scheduler.recovery_register"
	operationRecoveryCheckpoint   = "scheduler.recovery_checkpoint"
	maximumSchedulerCASAttempts   = 64
	schedulerRecoveryActionPrefix = "scheduler-recovery-action-"
)

type recoveryRegistration struct {
	Entry RecoveryEntry `json:"entry"`
}

type recoveryCheckpoint struct {
	PreviousCursor uint64           `json:"previous_cursor"`
	NextCursor     uint64           `json:"next_cursor"`
	Results        []RecoveryResult `json:"results"`
}

type recoveryJournalEnvelope struct {
	SchemaVersion uint32                `json:"schema_version"`
	Component     string                `json:"component"`
	Operation     string                `json:"operation"`
	OperationID   string                `json:"operation_id"`
	Registration  *recoveryRegistration `json:"registration,omitempty"`
	Checkpoint    *recoveryCheckpoint   `json:"checkpoint,omitempty"`
}

type registeredRecovery struct {
	Entry    RecoveryEntry
	Sequence uint64
}

type schedulerJournalRecord struct {
	RequestPayload []byte
	OutcomePayload []byte
	Receipt        journal.CommitReceipt
}

type recoveryProjection struct {
	installationID string
	global         journal.GlobalCursor
	runHeads       map[string]uint64
	entries        map[string]registeredRecovery
	cursor         uint64
	records        map[string]schedulerJournalRecord
	admission      schedulerSnapshot
}

func newRecoveryProjection(admissionSnapshot schedulerSnapshot) recoveryProjection {
	return recoveryProjection{
		installationID: admissionSnapshot.installationID,
		global:         admissionSnapshot.global,
		runHeads:       appendRunHeads(admissionSnapshot.runHeads),
		entries:        make(map[string]registeredRecovery),
		records:        make(map[string]schedulerJournalRecord),
		admission:      admissionSnapshot,
	}
}

func (s *Service) ScheduleRecovery(ctx context.Context, candidate RecoveryEntry) (RecoveryEntry, error) {
	if err := validateRecoveryCandidate(candidate); err != nil {
		return RecoveryEntry{}, err
	}
	snapshot, err := s.rebuildJournal(ctx)
	if err != nil {
		return RecoveryEntry{}, err
	}
	switch candidate.Kind {
	case RecoveryObservation:
		started, err := s.authority.StartObservation(ctx, candidate.Observation.Start)
		if err != nil {
			return RecoveryEntry{}, typedError(CodeAuthority, "schedule_observation", err)
		}
		refreshed, err := s.rebuildJournal(ctx)
		if err != nil {
			return RecoveryEntry{}, err
		}
		stored, ok := refreshed.recoveryReceipts[started.Commit.Outcome.ID]
		if !ok || !sameRecoveryReceipt(stored, started) || started.Operation != admission.OperationStartObservation ||
			started.Recovery.State != admission.RecoveryObservationStarted ||
			!matchesRequestedRecovery(started.Recovery.Identity, candidate.Observation.Start.Recovery) {
			return RecoveryEntry{}, typedError(CodeAuthority, "schedule_observation", ErrInvalidRecord)
		}
		candidate.SourceEventID = started.Commit.Outcome.ID
		candidate.Observation.Recovery = started.Recovery.Identity
		snapshot = refreshed
	case RecoveryLeaseExpiry:
		var source admission.LeaseReceipt
		for _, receipt := range snapshot.leaseReceipts {
			if (receipt.Operation == admission.OperationLeaseAcquire || receipt.Operation == admission.OperationLeaseRenew) &&
				receipt.Lease.ControlRunID == candidate.ControlRunID &&
				receipt.Lease.Subject == candidate.LeaseExpiry.Subject &&
				receipt.Lease.ExpiresAt.Equal(candidate.LeaseExpiry.ExpiresAt) &&
				receipt.Commit.Outcome.JournalPosition > source.Commit.Outcome.JournalPosition {
				source = receipt
			}
		}
		if source.Commit.Outcome.ID == "" {
			return RecoveryEntry{}, typedError(CodeAuthority, "schedule_lease_expiry", ErrInvalidRecord)
		}
		candidate.SourceEventID = source.Commit.Outcome.ID
	default:
		return RecoveryEntry{}, typedError(CodeInvalidRequest, "schedule_recovery", ErrInvalidRecord)
	}
	if snapshot.installationID == "" {
		return RecoveryEntry{}, typedError(CodeAuthority, "schedule_recovery", ErrInvalidRecord)
	}
	envelope := recoveryJournalEnvelope{
		SchemaVersion: schedulerRecoverySchema, Component: schedulerComponentName,
		Operation: operationRecoveryRegister,
		OperationID: stableSchedulerID(
			"scheduler-recovery-register", candidate.ControlRunID, candidate.ID,
		),
		Registration: &recoveryRegistration{Entry: candidate},
	}
	if _, err := s.commitRecoveryEnvelope(ctx, envelope, candidate.ControlRunID, candidateGraphRevision(candidate)); err != nil {
		return RecoveryEntry{}, err
	}
	return candidate, nil
}

func (s *Service) rebuildRecoveryJournal(ctx context.Context) (recoveryProjection, error) {
	projection := newRecoveryProjection(newSchedulerSnapshot())
	cursor := journal.GlobalCursor{}
	admissionReservations := make(map[string]journal.Event)
	schedulerReservations := make(map[string]journal.Event)
	excluded := make(map[string]bool)
	var wantPosition journal.JournalPosition = 1
	for {
		events, next, err := s.journal.Feed(ctx, cursor, 1000)
		if err != nil {
			return recoveryProjection{}, typedError(CodeAuthority, "recovery_feed", err)
		}
		if projection.installationID == "" {
			projection.installationID = next.InstallationID
			projection.admission.installationID = next.InstallationID
		}
		if !bounded(projection.installationID, 128) || next.InstallationID != projection.installationID ||
			next.SchemaVersion != journal.SchemaVersion {
			return recoveryProjection{}, typedError(CodeAuthority, "recovery_feed", ErrInvalidRecord)
		}
		for _, event := range events {
			if event.JournalPosition != wantPosition ||
				event.RunSequence != projection.runHeads[event.ControlRunID]+1 {
				return recoveryProjection{}, typedError(CodeAuthority, "recovery_feed", ErrInvalidRecord)
			}
			wantPosition++
			projection.runHeads[event.ControlRunID] = event.RunSequence
			key := event.ControlRunID + "\x00" + event.ActionID
			isAdmission := strings.HasPrefix(event.ActionID, admissionActionPrefix)
			isScheduler := strings.HasPrefix(event.ActionID, schedulerRecoveryActionPrefix)
			if event.Kind == journal.EventActionReserved {
				switch {
				case isAdmission:
					if _, exists := admissionReservations[key]; exists {
						return recoveryProjection{}, typedError(CodeAuthority, "recovery_reservation", ErrInvalidRecord)
					}
					admissionReservations[key] = event
				case isScheduler:
					if _, exists := schedulerReservations[key]; exists {
						return recoveryProjection{}, typedError(CodeAuthority, "recovery_reservation", ErrInvalidRecord)
					}
					schedulerReservations[key] = event
				}
				continue
			}
			if !journal.IsOutcome(event.Kind) || (!isAdmission && !isScheduler) {
				continue
			}
			payload, err := s.journal.Payload(ctx, event.PayloadDigest)
			if err != nil || journal.ValidatePayload(payload, event.PayloadDigest) != nil {
				return recoveryProjection{}, typedError(CodeAuthority, "recovery_payload", ErrInvalidRecord)
			}
			if isAdmission {
				reservation, ok := admissionReservations[key]
				if !ok {
					return recoveryProjection{}, typedError(CodeAuthority, "recovery_membership", ErrInvalidRecord)
				}
				action, err := s.journal.Reservation(ctx, event.ControlRunID, event.ActionID)
				if err != nil {
					return recoveryProjection{}, typedError(CodeAuthority, "recovery_membership", err)
				}
				requestPayload, err := s.journal.Payload(ctx, action.CanonicalRequestDigest)
				if err != nil || journal.ValidatePayload(requestPayload, action.CanonicalRequestDigest) != nil {
					return recoveryProjection{}, typedError(CodeAuthority, "recovery_request", ErrInvalidRecord)
				}
				var request admissionRequestEnvelope
				var outcome admissionOutcomeEnvelope
				if journal.DecodeStrict(requestPayload, &request) != nil ||
					journal.DecodeStrict(payload, &outcome) != nil ||
					validateAdmissionMembership(
						projection.installationID, action, reservation, event,
						requestPayload, payload, request, outcome,
					) != nil {
					return recoveryProjection{}, typedError(CodeAuthority, "recovery_membership", ErrInvalidRecord)
				}
				if err := s.applyAdmissionOutcome(
					&projection.admission, action, reservation, event, outcome, excluded,
				); err != nil {
					return recoveryProjection{}, err
				}
				delete(admissionReservations, key)
				continue
			}
			reservation, ok := schedulerReservations[key]
			if !ok {
				return recoveryProjection{}, typedError(CodeAuthority, "recovery_membership", ErrInvalidRecord)
			}
			action, envelope, err := recoveryJournalAction(reservation, event, payload)
			if err != nil || validateRecoveryJournalMembership(action, reservation, event, envelope) != nil {
				return recoveryProjection{}, typedError(CodeAuthority, "recovery_membership", ErrInvalidRecord)
			}
			if err := applyRecoveryEnvelope(&projection, envelope, uint64(event.JournalPosition)); err != nil {
				return recoveryProjection{}, err
			}
			projection.records[envelope.OperationID] = schedulerJournalRecord{
				RequestPayload: append([]byte(nil), payload...),
				OutcomePayload: append([]byte(nil), payload...),
				Receipt:        journal.CommitReceipt{Action: action, Reservation: reservation, Outcome: event, Created: true},
			}
			delete(schedulerReservations, key)
		}
		cursor = next
		projection.global = next
		projection.admission.global = next
		projection.admission.runHeads = appendRunHeads(projection.runHeads)
		if len(events) == 0 {
			break
		}
	}
	if len(admissionReservations) != 0 || len(schedulerReservations) != 0 {
		return recoveryProjection{}, typedError(CodeAuthority, "recovery_reservation", ErrInvalidRecord)
	}
	return projection, nil
}

func recoveryJournalAction(
	reservation journal.Event,
	event journal.Event,
	payload []byte,
) (journal.Action, recoveryJournalEnvelope, error) {
	var envelope recoveryJournalEnvelope
	if journal.DecodeStrict(payload, &envelope) != nil {
		return journal.Action{}, recoveryJournalEnvelope{}, ErrInvalidRecord
	}
	var runID string
	var graphRevision uint64
	switch envelope.Operation {
	case operationRecoveryRegister:
		if envelope.Registration == nil || validateRecoveryEntry(envelope.Registration.Entry) != nil {
			return journal.Action{}, recoveryJournalEnvelope{}, ErrInvalidRecord
		}
		runID = envelope.Registration.Entry.ControlRunID
		graphRevision = candidateGraphRevision(envelope.Registration.Entry)
	case operationRecoveryCheckpoint:
		if envelope.Checkpoint == nil {
			return journal.Action{}, recoveryJournalEnvelope{}, ErrInvalidRecord
		}
		runID = schedulerRecoveryRunID
		graphRevision = 1
	default:
		return journal.Action{}, recoveryJournalEnvelope{}, ErrInvalidRecord
	}
	expectedProjection := uint64(0)
	if reservation.RunSequence > 0 {
		expectedProjection = reservation.RunSequence - 1
	}
	return journal.Action{
		ID: recoveryJournalActionID(envelope.OperationID), ControlRunID: runID, Kind: journal.KindObserve,
		GraphRevision: graphRevision, ExpectedProjection: expectedProjection,
		CanonicalRequestDigest: event.PayloadDigest,
		IdempotencyKey:         recoveryJournalIdempotencyKey(envelope.OperationID),
	}, envelope, nil
}

func (s *Service) advanceRecoveryProjection(
	ctx context.Context,
	projection recoveryProjection,
) (recoveryProjection, error) {
	projection = cloneRecoveryProjection(projection)
	cursor := projection.global
	admissionReservations := make(map[string]journal.Event)
	schedulerReservations := make(map[string]journal.Event)
	wantPosition := cursor.JournalPosition + 1
	for {
		events, next, err := s.journal.Feed(ctx, cursor, 1000)
		if err != nil {
			return recoveryProjection{}, typedError(CodeAuthority, "recovery_advance", err)
		}
		if next.InstallationID != projection.installationID || next.SchemaVersion != journal.SchemaVersion {
			return recoveryProjection{}, typedError(CodeAuthority, "recovery_advance", ErrInvalidRecord)
		}
		for _, event := range events {
			if event.JournalPosition != wantPosition || event.RunSequence != projection.runHeads[event.ControlRunID]+1 {
				return recoveryProjection{}, typedError(CodeAuthority, "recovery_advance", ErrInvalidRecord)
			}
			wantPosition++
			projection.runHeads[event.ControlRunID] = event.RunSequence
			key := event.ControlRunID + "\x00" + event.ActionID
			isAdmission := strings.HasPrefix(event.ActionID, admissionActionPrefix)
			isScheduler := strings.HasPrefix(event.ActionID, schedulerRecoveryActionPrefix)
			if event.Kind == journal.EventActionReserved {
				switch {
				case isAdmission:
					admissionReservations[key] = event
				case isScheduler:
					schedulerReservations[key] = event
				}
				continue
			}
			if !journal.IsOutcome(event.Kind) || (!isAdmission && !isScheduler) {
				continue
			}
			payload, err := s.journal.Payload(ctx, event.PayloadDigest)
			if err != nil || journal.ValidatePayload(payload, event.PayloadDigest) != nil {
				return recoveryProjection{}, typedError(CodeAuthority, "recovery_advance", ErrInvalidRecord)
			}
			action, err := s.journal.Reservation(ctx, event.ControlRunID, event.ActionID)
			if err != nil {
				return recoveryProjection{}, typedError(CodeAuthority, "recovery_advance", err)
			}
			requestPayload, err := s.journal.Payload(ctx, action.CanonicalRequestDigest)
			if err != nil || journal.ValidatePayload(requestPayload, action.CanonicalRequestDigest) != nil {
				return recoveryProjection{}, typedError(CodeAuthority, "recovery_advance", ErrInvalidRecord)
			}
			if isAdmission {
				reservation, ok := admissionReservations[key]
				if !ok {
					return recoveryProjection{}, typedError(CodeAuthority, "recovery_advance", ErrInvalidRecord)
				}
				var request admissionRequestEnvelope
				var outcome admissionOutcomeEnvelope
				if journal.DecodeStrict(requestPayload, &request) != nil ||
					journal.DecodeStrict(payload, &outcome) != nil ||
					validateAdmissionMembership(
						projection.installationID, action, reservation, event,
						requestPayload, payload, request, outcome,
					) != nil {
					return recoveryProjection{}, typedError(CodeAuthority, "recovery_advance", ErrInvalidRecord)
				}
				if outcome.Lease != nil || outcome.LeaseTombstone != nil || outcome.Recovery != nil {
					if err := s.applyAdmissionOutcome(
						&projection.admission, action, reservation, event, outcome, make(map[string]bool),
					); err != nil {
						return recoveryProjection{}, err
					}
				}
				delete(admissionReservations, key)
				continue
			}
			reservation, ok := schedulerReservations[key]
			if !ok {
				return recoveryProjection{}, typedError(CodeAuthority, "recovery_advance", ErrInvalidRecord)
			}
			var request, outcome recoveryJournalEnvelope
			if journal.DecodeStrict(requestPayload, &request) != nil ||
				journal.DecodeStrict(payload, &outcome) != nil || !reflect.DeepEqual(request, outcome) ||
				validateRecoveryJournalMembership(action, reservation, event, request) != nil {
				return recoveryProjection{}, typedError(CodeAuthority, "recovery_advance", ErrInvalidRecord)
			}
			if err := applyRecoveryEnvelope(&projection, request, uint64(event.JournalPosition)); err != nil {
				return recoveryProjection{}, err
			}
			projection.records[request.OperationID] = schedulerJournalRecord{
				RequestPayload: append([]byte(nil), requestPayload...),
				OutcomePayload: append([]byte(nil), payload...),
				Receipt: journal.CommitReceipt{
					Action: action, Reservation: reservation, Outcome: event, Created: true,
				},
			}
			delete(schedulerReservations, key)
		}
		cursor = next
		projection.global = next
		projection.admission.global = next
		projection.admission.runHeads = appendRunHeads(projection.runHeads)
		if len(events) == 0 {
			break
		}
	}
	if len(admissionReservations) != 0 || len(schedulerReservations) != 0 {
		return recoveryProjection{}, typedError(CodeAuthority, "recovery_advance", ErrInvalidRecord)
	}
	return projection, nil
}

func validateRecoveryJournalMembership(
	action journal.Action,
	reservation, event journal.Event,
	envelope recoveryJournalEnvelope,
) error {
	wantOperationID, err := expectedRecoveryOperationID(envelope)
	if err != nil || envelope.OperationID != wantOperationID ||
		envelope.SchemaVersion != schedulerRecoverySchema || envelope.Component != schedulerComponentName ||
		!bounded(envelope.OperationID, 128) || action.ID != recoveryJournalActionID(envelope.OperationID) ||
		action.IdempotencyKey != recoveryJournalIdempotencyKey(envelope.OperationID) ||
		action.Kind != journal.KindObserve || event.Kind != journal.EventActionResult ||
		action.TaskID != "" || action.AttemptID != "" || action.AuthorityReceiptID != "" ||
		event.ID != recoveryJournalEventID(envelope.OperationID) || event.ActionID != action.ID ||
		event.ControlRunID != action.ControlRunID || reservation.ActionID != action.ID ||
		reservation.ControlRunID != action.ControlRunID || reservation.Kind != journal.EventActionReserved ||
		reservation.RunSequence == 0 || action.ExpectedProjection != reservation.RunSequence-1 ||
		event.RunSequence != reservation.RunSequence+1 || event.JournalPosition != reservation.JournalPosition+1 ||
		!event.OccurredAt.Equal(time.Unix(0, 0).UTC()) || event.ProviderReceipt != "" {
		return ErrInvalidRecord
	}
	actionDigest, err := journal.Digest(action)
	if err != nil || reservation.PayloadDigest != actionDigest {
		return ErrInvalidRecord
	}
	switch envelope.Operation {
	case operationRecoveryRegister:
		if envelope.Registration == nil || envelope.Checkpoint != nil ||
			action.ControlRunID != envelope.Registration.Entry.ControlRunID ||
			action.GraphRevision != candidateGraphRevision(envelope.Registration.Entry) {
			return ErrInvalidRecord
		}
	case operationRecoveryCheckpoint:
		if envelope.Checkpoint == nil || envelope.Registration != nil ||
			action.ControlRunID != schedulerRecoveryRunID || action.GraphRevision != 1 {
			return ErrInvalidRecord
		}
	default:
		return ErrInvalidRecord
	}
	return nil
}

func applyRecoveryEnvelope(
	projection *recoveryProjection,
	envelope recoveryJournalEnvelope,
	sequence uint64,
) error {
	switch envelope.Operation {
	case operationRecoveryRegister:
		entry := envelope.Registration.Entry
		if err := validateRegisteredRecovery(entry); err != nil {
			return typedError(CodeAuthority, "recovery_register", ErrInvalidRecord)
		}
		if err := validateRecoverySource(projection.admission, entry); err != nil {
			return err
		}
		key := recoveryEntryKey(entry)
		if prior, exists := projection.entries[key]; exists {
			if !reflect.DeepEqual(prior.Entry, entry) {
				return typedError(CodeAuthority, "recovery_register", ErrInvalidRecord)
			}
			return nil
		}
		projection.entries[key] = registeredRecovery{Entry: entry, Sequence: sequence}
	case operationRecoveryCheckpoint:
		checkpoint := envelope.Checkpoint
		if checkpoint.PreviousCursor != projection.cursor || len(checkpoint.Results) == 0 ||
			len(checkpoint.Results) > MaximumScanEntries {
			return typedError(CodeAuthority, "recovery_checkpoint", ErrInvalidRecord)
		}
		page := recoveryPage(*projection, MaximumScanEntries)
		if len(checkpoint.Results) > len(page) {
			return typedError(CodeAuthority, "recovery_checkpoint", ErrInvalidRecord)
		}
		for index, result := range checkpoint.Results {
			registered := page[index]
			if result.EntryID != registered.Entry.ID || result.ControlRunID != registered.Entry.ControlRunID ||
				!validRecoveryResult(result) ||
				!validRecoveryBackoffTransition(registered.Entry, result) ||
				validateRecoveryOutcomeMembership(projection.admission, registered.Entry, result) != nil {
				return typedError(CodeAuthority, "recovery_checkpoint", ErrInvalidRecord)
			}
			entry := registered.Entry
			if result.Backoff != nil {
				entry.Retry = RetryState{Step: result.Backoff.Step, NextAttemptAt: result.Backoff.NextAttemptAt}
			}
			if recoveryResultTerminal(result) {
				delete(projection.entries, recoveryEntryKey(entry))
			} else {
				registered.Entry = entry
				projection.entries[recoveryEntryKey(entry)] = registered
			}
		}
		last := page[len(checkpoint.Results)-1]
		if checkpoint.NextCursor != last.Sequence {
			return typedError(CodeAuthority, "recovery_checkpoint", ErrInvalidRecord)
		}
		projection.cursor = checkpoint.NextCursor
	default:
		return typedError(CodeAuthority, "recovery_journal", ErrInvalidRecord)
	}
	return nil
}

func validateRecoverySource(snapshot schedulerSnapshot, entry RecoveryEntry) error {
	switch entry.Kind {
	case RecoveryObservation:
		receipt, ok := snapshot.recoveryReceipts[entry.SourceEventID]
		if !ok || receipt.Operation != admission.OperationStartObservation ||
			receipt.Recovery.State != admission.RecoveryObservationStarted ||
			!sameRecoveryIdentity(receipt.Recovery.Identity, entry.Observation.Recovery) {
			return typedError(CodeAuthority, "recovery_source", ErrInvalidRecord)
		}
	case RecoveryLeaseExpiry:
		receipt, ok := snapshot.leaseReceipts[entry.SourceEventID]
		if !ok || (receipt.Operation != admission.OperationLeaseAcquire && receipt.Operation != admission.OperationLeaseRenew) ||
			receipt.Lease.ControlRunID != entry.ControlRunID || receipt.Lease.Subject != entry.LeaseExpiry.Subject ||
			!receipt.Lease.ExpiresAt.Equal(entry.LeaseExpiry.ExpiresAt) {
			return typedError(CodeAuthority, "recovery_source", ErrInvalidRecord)
		}
	default:
		return typedError(CodeAuthority, "recovery_source", ErrInvalidRecord)
	}
	return nil
}

func (s *Service) commitRecoveryEnvelope(
	ctx context.Context,
	envelope recoveryJournalEnvelope,
	runID string,
	graphRevision uint64,
) (journal.CommitReceipt, error) {
	for range maximumSchedulerCASAttempts {
		projection, err := s.rebuildRecoveryJournal(ctx)
		if err != nil {
			return journal.CommitReceipt{}, err
		}
		receipt, err := s.commitRecoveryEnvelopeAt(ctx, envelope, runID, graphRevision, projection)
		if err == nil || !errors.Is(err, ErrCursor) {
			return receipt, err
		}
	}
	return journal.CommitReceipt{}, typedError(CodeCursor, "recovery_commit", ErrCursor)
}

func (s *Service) commitRecoveryEnvelopeAt(
	ctx context.Context,
	envelope recoveryJournalEnvelope,
	runID string,
	graphRevision uint64,
	projection recoveryProjection,
) (journal.CommitReceipt, error) {
	projection = cloneRecoveryProjection(projection)
	requestPayload, requestDigest, err := recoveryJournalPayload(envelope)
	if err != nil {
		return journal.CommitReceipt{}, err
	}
	outcomePayload := append([]byte(nil), requestPayload...)
	if existing, ok := projection.records[envelope.OperationID]; ok {
		if !bytes.Equal(existing.RequestPayload, requestPayload) ||
			!bytes.Equal(existing.OutcomePayload, outcomePayload) {
			return journal.CommitReceipt{}, typedError(CodeCursor, "recovery_exact_replay", ErrCursor)
		}
		existing.Receipt.Created = false
		return existing.Receipt, nil
	}
	sequence := uint64(projection.global.JournalPosition)
	if sequence > ^uint64(0)-2 {
		return journal.CommitReceipt{}, typedError(CodeCursor, "recovery_commit", ErrCursor)
	}
	if err := applyRecoveryEnvelope(&projection, envelope, sequence+2); err != nil {
		if envelope.Operation == operationRecoveryCheckpoint {
			return journal.CommitReceipt{}, typedError(CodeCursor, "recovery_checkpoint", ErrCursor)
		}
		return journal.CommitReceipt{}, err
	}
	actionID := recoveryJournalActionID(envelope.OperationID)
	action := journal.Action{
		ID: actionID, ControlRunID: runID, Kind: journal.KindObserve,
		GraphRevision: graphRevision, ExpectedProjection: projection.runHeads[runID],
		CanonicalRequestDigest: requestDigest,
		IdempotencyKey:         recoveryJournalIdempotencyKey(envelope.OperationID),
	}
	outcome := journal.Event{
		ID: recoveryJournalEventID(envelope.OperationID), ControlRunID: runID,
		ActionID: actionID, Kind: journal.EventActionResult,
		PayloadDigest: requestDigest, OccurredAt: time.Unix(0, 0).UTC(),
	}
	expectedRun := journal.RunCursor{
		InstallationID: projection.installationID, ControlRunID: runID,
		SchemaVersion: journal.SchemaVersion, RunSequence: projection.runHeads[runID],
	}
	receipt, commitErr := s.journal.Commit(ctx, journal.CommitRequest{
		Action: action, ExpectedRun: expectedRun, ExpectedGlobal: projection.global,
		RequestPayload: requestPayload, Outcome: outcome, OutcomePayload: outcomePayload,
	})
	if commitErr == nil {
		return receipt, nil
	}
	recovered, recoveryErr := s.recoverExactRecoveryCommit(
		ctx, expectedRun, action, requestPayload, outcomePayload, envelope,
	)
	if recoveryErr == nil {
		return recovered, nil
	}
	if errors.Is(commitErr, journal.ErrConflict) {
		return journal.CommitReceipt{}, typedError(CodeCursor, "recovery_commit", ErrCursor)
	}
	return journal.CommitReceipt{}, typedError(CodeAuthority, "recovery_commit", commitErr)
}

func (s *Service) recoverExactRecoveryCommit(
	ctx context.Context,
	prior journal.RunCursor,
	action journal.Action,
	requestPayload, outcomePayload []byte,
	envelope recoveryJournalEnvelope,
) (journal.CommitReceipt, error) {
	events, _, err := s.journal.RunEvents(ctx, prior, 2)
	if err != nil || len(events) != 2 {
		return journal.CommitReceipt{}, ErrCursor
	}
	storedAction, err := s.journal.Reservation(ctx, action.ControlRunID, action.ID)
	if err != nil || storedAction != action {
		return journal.CommitReceipt{}, ErrCursor
	}
	storedRequest, err := s.journal.Payload(ctx, action.CanonicalRequestDigest)
	if err != nil || !bytes.Equal(storedRequest, requestPayload) {
		return journal.CommitReceipt{}, ErrCursor
	}
	storedOutcome, err := s.journal.Payload(ctx, events[1].PayloadDigest)
	if err != nil || !bytes.Equal(storedOutcome, outcomePayload) ||
		validateRecoveryJournalMembership(action, events[0], events[1], envelope) != nil {
		return journal.CommitReceipt{}, ErrCursor
	}
	return journal.CommitReceipt{
		Action: action, Reservation: events[0], Outcome: events[1], Created: false,
	}, nil
}

func (s *Service) persistRecoveryCheckpoint(
	ctx context.Context,
	projection recoveryProjection,
	results []RecoveryResult,
	page []registeredRecovery,
) error {
	if len(results) == 0 || len(results) > len(page) {
		return typedError(CodeInvalidRequest, "persist_recovery", ErrInvalidRecord)
	}
	next := page[len(results)-1].Sequence
	checkpoint := recoveryCheckpoint{
		PreviousCursor: projection.cursor, NextCursor: next,
		Results: append([]RecoveryResult(nil), results...),
	}
	payload, _, err := recoveryJournalPayload(checkpoint)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	operationID := "checkpoint-" + hex.EncodeToString(sum[:])
	envelope := recoveryJournalEnvelope{
		SchemaVersion: schedulerRecoverySchema, Component: schedulerComponentName,
		Operation: operationRecoveryCheckpoint, OperationID: operationID,
		Checkpoint: &checkpoint,
	}
	projection, err = s.advanceRecoveryProjection(ctx, projection)
	if err != nil {
		return err
	}
	_, err = s.commitRecoveryEnvelopeAt(ctx, envelope, schedulerRecoveryRunID, 1, projection)
	return err
}

func recoveryPage(projection recoveryProjection, limit int) []registeredRecovery {
	values := make([]registeredRecovery, 0, len(projection.entries))
	for _, entry := range projection.entries {
		values = append(values, entry)
	}
	sort.Slice(values, func(left, right int) bool { return values[left].Sequence < values[right].Sequence })
	ordered := make([]registeredRecovery, 0, len(values))
	for _, entry := range values {
		if entry.Sequence > projection.cursor {
			ordered = append(ordered, entry)
		}
	}
	for _, entry := range values {
		if entry.Sequence <= projection.cursor {
			ordered = append(ordered, entry)
		}
	}
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered
}

func validRecoveryResult(result RecoveryResult) bool {
	if !bounded(result.EntryID, 128) || !bounded(result.ControlRunID, 128) {
		return false
	}
	backoff := result.Backoff
	switch result.Outcome {
	case OutcomeApplied, OutcomeFenced, OutcomeLeaseExpired, OutcomeDeferred:
		return backoff == nil
	case OutcomeFailed:
		if backoff != nil && backoff.Code == RetrySemantic && backoff.Delay == 0 &&
			backoff.NextAttemptAt.IsZero() && !backoff.RescanAllowed && !backoff.ActionRetryAllowed {
			return true
		}
		return validRetryBackoff(backoff, RetryContention, true, false) ||
			validRetryBackoff(backoff, RetryTransient, true, false)
	case OutcomeAmbiguous:
		return validRetryBackoff(backoff, RetryAmbiguous, true, false)
	case OutcomeRetryAllowed:
		return validRetryBackoff(backoff, RetryProvenSafe, false, true)
	case OutcomeBudgetExhausted:
		return validRetryBackoff(backoff, RetryTransient, true, false)
	default:
		return false
	}
}

func validateRecoveryOutcomeMembership(
	snapshot schedulerSnapshot,
	entry RecoveryEntry,
	result RecoveryResult,
) error {
	switch entry.Kind {
	case RecoveryObservation:
		recovery, ok := snapshot.recoveries[recoveryKey(entry.Observation.Recovery)]
		if !ok {
			return ErrInvalidRecord
		}
		switch result.Outcome {
		case OutcomeApplied:
			if recovery.State != admission.RecoveryApplied {
				return ErrInvalidRecord
			}
		case OutcomeRetryAllowed:
			if recovery.State != admission.RecoveryNotPerformed && recovery.State != admission.RecoveryCanceled {
				return ErrInvalidRecord
			}
		case OutcomeFenced:
			if recovery.State != admission.RecoveryFenced && recovery.State != admission.RecoveryNotPerformed &&
				recovery.State != admission.RecoveryCanceled {
				return ErrInvalidRecord
			}
		case OutcomeAmbiguous, OutcomeBudgetExhausted, OutcomeDeferred, OutcomeFailed:
		default:
			return ErrInvalidRecord
		}
	case RecoveryLeaseExpiry:
		switch result.Outcome {
		case OutcomeLeaseExpired:
			found := false
			for _, receipt := range snapshot.leaseReceipts {
				if receipt.Operation == admission.OperationLeaseExpire && receipt.Tombstone != nil &&
					receipt.Lease.ControlRunID == entry.ControlRunID &&
					receipt.Lease.Subject == entry.LeaseExpiry.Subject &&
					receipt.Lease.ExpiresAt.Equal(entry.LeaseExpiry.ExpiresAt) &&
					receipt.Lease.State == admission.LeaseExpired {
					found = true
					break
				}
			}
			if !found {
				return ErrInvalidRecord
			}
		case OutcomeAmbiguous, OutcomeBudgetExhausted, OutcomeDeferred, OutcomeFailed:
		default:
			return ErrInvalidRecord
		}
	default:
		return ErrInvalidRecord
	}
	return nil
}

func validRetryBackoff(backoff *Backoff, code RetryCode, rescan, actionRetry bool) bool {
	return backoff != nil && backoff.Code == code && backoff.Step > 0 && backoff.Delay > 0 &&
		backoff.Delay <= MaximumRetryBackoff && !backoff.NextAttemptAt.IsZero() &&
		backoff.RescanAllowed == rescan && backoff.ActionRetryAllowed == actionRetry
}

func validRecoveryBackoffTransition(entry RecoveryEntry, result RecoveryResult) bool {
	if result.Backoff == nil {
		return true
	}
	backoff := result.Backoff
	if backoff.Delay == 0 {
		return result.Outcome == OutcomeFailed && backoff.Step == entry.Retry.Step
	}
	expected := NextBackoff(time.Unix(0, 0).UTC(), entry.Retry, backoff.Code, DefaultPolicy())
	return backoff.Step == expected.Step && backoff.Delay == expected.Delay
}

func recoveryResultTerminal(result RecoveryResult) bool {
	switch result.Outcome {
	case OutcomeApplied, OutcomeRetryAllowed, OutcomeFenced, OutcomeLeaseExpired:
		return true
	case OutcomeFailed:
		return result.Backoff != nil && !result.Backoff.RescanAllowed && !result.Backoff.ActionRetryAllowed
	default:
		return false
	}
}

func recoveryJournalPayload(value any) ([]byte, string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, "", typedError(CodeInvalidRequest, "recovery_payload", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, "", typedError(CodeInvalidRequest, "recovery_payload", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, "", typedError(CodeInvalidRequest, "recovery_payload", ErrInvalidRecord)
	}
	payload, err := journal.CanonicalJSON(normalized)
	if err != nil {
		return nil, "", typedError(CodeInvalidRequest, "recovery_payload", err)
	}
	sum := sha256.Sum256(payload)
	return payload, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func expectedRecoveryOperationID(envelope recoveryJournalEnvelope) (string, error) {
	switch envelope.Operation {
	case operationRecoveryRegister:
		if envelope.Registration == nil {
			return "", ErrInvalidRecord
		}
		entry := envelope.Registration.Entry
		return stableSchedulerID("scheduler-recovery-register", entry.ControlRunID, entry.ID), nil
	case operationRecoveryCheckpoint:
		if envelope.Checkpoint == nil {
			return "", ErrInvalidRecord
		}
		payload, _, err := recoveryJournalPayload(*envelope.Checkpoint)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(payload)
		return "checkpoint-" + hex.EncodeToString(sum[:]), nil
	default:
		return "", ErrInvalidRecord
	}
}

func recoveryJournalActionID(operationID string) string {
	return stableSchedulerID("scheduler-recovery-action", operationID)
}

func recoveryJournalIdempotencyKey(operationID string) string {
	return stableSchedulerID("scheduler-recovery-key", operationID)
}

func recoveryJournalEventID(operationID string) string {
	return stableSchedulerID("scheduler-recovery-event", operationID)
}

func stableSchedulerID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(append([]string{prefix}, values...), "\x00")))
	return prefix + "-" + hex.EncodeToString(sum[:])
}

func recoveryEntryKey(entry RecoveryEntry) string {
	return entry.ControlRunID + "\x00" + entry.ID
}

func candidateGraphRevision(entry RecoveryEntry) uint64 {
	if entry.Kind == RecoveryObservation {
		return entry.Observation.Start.Identity.GraphRevision
	}
	return entry.LeaseExpiry.Identity.GraphRevision
}

func sameRecoveryReceipt(stored, got admission.RecoveryReceipt) bool {
	stored.Created, got.Created = false, false
	stored.Commit.Created, got.Commit.Created = false, false
	return reflect.DeepEqual(stored, got)
}

func appendRunHeads(values map[string]uint64) map[string]uint64 {
	result := make(map[string]uint64, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneRecoveryProjection(value recoveryProjection) recoveryProjection {
	cloned := value
	cloned.runHeads = appendRunHeads(value.runHeads)
	cloned.entries = make(map[string]registeredRecovery, len(value.entries))
	for key, entry := range value.entries {
		cloned.entries[key] = entry
	}
	cloned.records = make(map[string]schedulerJournalRecord, len(value.records))
	for key, record := range value.records {
		record.RequestPayload = append([]byte(nil), record.RequestPayload...)
		record.OutcomePayload = append([]byte(nil), record.OutcomePayload...)
		cloned.records[key] = record
	}
	cloned.admission = cloneSchedulerSnapshot(value.admission)
	return cloned
}

func cloneSchedulerSnapshot(value schedulerSnapshot) schedulerSnapshot {
	cloned := value
	cloned.runHeads = appendRunHeads(value.runHeads)
	cloned.ready = make(map[string]readyEntry, len(value.ready))
	for key, entry := range value.ready {
		cloned.ready[key] = entry
	}
	cloned.virtualFinish = make(map[string]uint64, len(value.virtualFinish))
	for key, finish := range value.virtualFinish {
		cloned.virtualFinish[key] = finish
	}
	cloned.receipts = make(map[string]admission.AdmissionReceipt, len(value.receipts))
	for key, receipt := range value.receipts {
		cloned.receipts[key] = receipt
	}
	cloned.leaseReceipts = make(map[string]admission.LeaseReceipt, len(value.leaseReceipts))
	for key, receipt := range value.leaseReceipts {
		cloned.leaseReceipts[key] = receipt
	}
	cloned.recoveryReceipts = make(map[string]admission.RecoveryReceipt, len(value.recoveryReceipts))
	for key, receipt := range value.recoveryReceipts {
		cloned.recoveryReceipts[key] = receipt
	}
	cloned.leases = make(map[string]admission.Lease, len(value.leases))
	for key, lease := range value.leases {
		cloned.leases[key] = lease
	}
	cloned.recoveries = make(map[string]admission.RecoveryRecord, len(value.recoveries))
	for key, recovery := range value.recoveries {
		cloned.recoveries[key] = recovery
	}
	return cloned
}

func cursorString(cursor uint64) string {
	if cursor == 0 {
		return ""
	}
	return strconv.FormatUint(cursor, 10)
}
