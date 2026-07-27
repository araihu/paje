package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/araihu/paje/internal/controlplane/admission"
	"github.com/araihu/paje/internal/controlplane/journal"
)

type RecoveryKind string

const (
	RecoveryLeaseExpiry RecoveryKind = "lease_expiry"
	RecoveryObservation RecoveryKind = "observation"
)

type ObservationWork struct {
	Start         admission.RecoveryRequest
	Recovery      admission.RecoveryIdentity
	FenceIdentity admission.TransitionIdentity
	ApplyIdentity admission.TransitionIdentity
}

type RetryState struct {
	Step          uint64
	NextAttemptAt time.Time
}

type RecoveryEntry struct {
	ID            string
	ControlRunID  string
	SourceEventID string
	DueAt         time.Time
	Retry         RetryState
	Kind          RecoveryKind
	LeaseExpiry   *admission.LeaseRequest
	Observation   *ObservationWork
}

type RetryCode string

const (
	RetryTransient  RetryCode = "transient"
	RetryContention RetryCode = "contention"
	RetryAmbiguous  RetryCode = "ambiguous"
	RetryProvenSafe RetryCode = "proven_safe"
	RetryFenced     RetryCode = "fenced"
	RetrySemantic   RetryCode = "semantic"
)

type Backoff struct {
	Code               RetryCode
	Step               uint64
	Delay              time.Duration
	NextAttemptAt      time.Time
	RescanAllowed      bool
	ActionRetryAllowed bool
}

type RecoveryOutcome string

const (
	OutcomeApplied         RecoveryOutcome = "applied"
	OutcomeAmbiguous       RecoveryOutcome = "ambiguous"
	OutcomeRetryAllowed    RecoveryOutcome = "retry_allowed"
	OutcomeFenced          RecoveryOutcome = "fenced"
	OutcomeLeaseExpired    RecoveryOutcome = "lease_expired"
	OutcomeBudgetExhausted RecoveryOutcome = "budget_exhausted"
	OutcomeDeferred        RecoveryOutcome = "deferred"
	OutcomeFailed          RecoveryOutcome = "failed"
)

type RecoveryResult struct {
	EntryID      string
	ControlRunID string
	Outcome      RecoveryOutcome
	Backoff      *Backoff
}

type ScanResult struct {
	PreviousCursor      string
	NextCursor          string
	Examined            int
	StartedObservations int
	Results             []RecoveryResult
	Persisted           bool
}

func NextBackoff(now time.Time, state RetryState, code RetryCode, policy Policy) Backoff {
	delay := policy.InitialRetryBackoff
	switch state.Step {
	case 0:
	case 1:
		delay = policy.InitialRetryBackoff * 2
	case 2:
		delay = policy.InitialRetryBackoff * 4
	default:
		delay = policy.MaximumRetryBackoff
	}
	if delay > policy.MaximumRetryBackoff {
		delay = policy.MaximumRetryBackoff
	}
	backoff := Backoff{
		Code: code, Step: admission.SaturatingAdd(state.Step, 1), Delay: delay,
		NextAttemptAt: now.Add(delay),
	}
	if code == RetryProvenSafe {
		backoff.ActionRetryAllowed = true
	} else {
		backoff.RescanAllowed = true
	}
	return backoff
}

func (s *Service) ScanRecovery(ctx context.Context) (ScanResult, error) {
	startedAt := time.Now()
	workDeadline := startedAt.Add(s.policy.ObservationBudget)
	hardDeadline := startedAt.Add(s.policy.PassBudget)
	projection, err := callBefore(ctx, workDeadline, s.rebuildRecoveryJournal)
	if err != nil {
		if deadlineError(err) {
			return ScanResult{}, typedError(CodeBudget, "rebuild_recovery", ErrBudget)
		}
		return ScanResult{}, err
	}
	page := recoveryPage(projection, s.policy.MaxScanEntries)
	result := ScanResult{
		PreviousCursor: cursorString(projection.cursor),
		NextCursor:     cursorString(projection.cursor),
	}
	semanticNow := s.clock().UTC()
	dueRuns := dueRecoveryRuns(projection.entries, semanticNow)
	due := make([]bool, len(page))
	for index, registered := range page {
		entry := registered.Entry
		if entry.DueAt.After(semanticNow) ||
			(!entry.Retry.NextAttemptAt.IsZero() && entry.Retry.NextAttemptAt.After(semanticNow)) {
			continue
		}
		due[index] = true
	}

	processedRuns := make(map[string]struct{}, len(dueRuns))
	for index, registered := range page {
		if !time.Now().Before(workDeadline) {
			break
		}
		entry := registered.Entry
		result.Examined++
		if !due[index] {
			result.Results = append(result.Results, RecoveryResult{
				EntryID: entry.ID, ControlRunID: entry.ControlRunID, Outcome: OutcomeDeferred,
			})
			continue
		}
		if len(dueRuns) > 1 {
			if _, processed := processedRuns[entry.ControlRunID]; processed {
				result.Results = append(result.Results, RecoveryResult{
					EntryID: entry.ID, ControlRunID: entry.ControlRunID, Outcome: OutcomeDeferred,
				})
				continue
			}
		}
		processedRuns[entry.ControlRunID] = struct{}{}
		if terminal, ok := recoveryTerminalResult(projection.admission, entry, semanticNow, s.policy); ok {
			result.Results = append(result.Results, terminal)
			continue
		}
		if entry.Kind == RecoveryObservation {
			result.StartedObservations++
		}
		entryResult, processErr := s.processRecoveryEntry(ctx, entry, workDeadline)
		if processErr != nil {
			break
		}
		result.Results = append(result.Results, entryResult)
	}

	if len(result.Results) == 0 {
		if len(page) == 0 {
			return result, nil
		}
		return result, typedError(CodeBudget, "recovery_work", ErrBudget)
	}
	_, err = callBefore(ctx, hardDeadline, func(callCtx context.Context) (struct{}, error) {
		return struct{}{}, s.persistRecoveryCheckpoint(
			callCtx, projection, result.Results, page,
		)
	})
	if err != nil {
		if deadlineError(err) {
			return result, typedError(CodeBudget, "persist_recovery", ErrBudget)
		}
		return result, err
	}
	result.NextCursor = cursorString(page[len(result.Results)-1].Sequence)
	result.Persisted = true
	return result, nil
}

func recoveryTerminalResult(
	snapshot schedulerSnapshot,
	entry RecoveryEntry,
	now time.Time,
	policy Policy,
) (RecoveryResult, bool) {
	result := RecoveryResult{EntryID: entry.ID, ControlRunID: entry.ControlRunID}
	switch entry.Kind {
	case RecoveryObservation:
		recovery, ok := snapshot.recoveries[recoveryKey(entry.Observation.Recovery)]
		if !ok {
			return RecoveryResult{}, false
		}
		switch recovery.State {
		case admission.RecoveryApplied:
			result.Outcome = OutcomeApplied
			return result, true
		case admission.RecoveryFenced:
			result.Outcome = OutcomeFenced
			return result, true
		case admission.RecoveryCanceled, admission.RecoveryNotPerformed:
			result.Outcome = OutcomeRetryAllowed
			backoff := NextBackoff(now, entry.Retry, RetryProvenSafe, policy)
			result.Backoff = &backoff
			return result, true
		}
	case RecoveryLeaseExpiry:
		for _, receipt := range snapshot.leaseReceipts {
			if receipt.Operation == admission.OperationLeaseExpire && receipt.Tombstone != nil &&
				receipt.Lease.ControlRunID == entry.ControlRunID &&
				receipt.Lease.Subject == entry.LeaseExpiry.Subject &&
				receipt.Lease.ExpiresAt.Equal(entry.LeaseExpiry.ExpiresAt) &&
				receipt.Lease.State == admission.LeaseExpired {
				result.Outcome = OutcomeLeaseExpired
				return result, true
			}
		}
	}
	return RecoveryResult{}, false
}

func dueRecoveryRuns(entries map[string]registeredRecovery, now time.Time) map[string]struct{} {
	runs := make(map[string]struct{})
	for _, registered := range entries {
		entry := registered.Entry
		if !entry.DueAt.After(now) &&
			(entry.Retry.NextAttemptAt.IsZero() || !entry.Retry.NextAttemptAt.After(now)) {
			runs[entry.ControlRunID] = struct{}{}
		}
	}
	return runs
}

func (s *Service) processRecoveryEntry(
	ctx context.Context,
	entry RecoveryEntry,
	workDeadline time.Time,
) (RecoveryResult, error) {
	result := RecoveryResult{EntryID: entry.ID, ControlRunID: entry.ControlRunID}
	switch entry.Kind {
	case RecoveryLeaseExpiry:
		if s.clock().UTC().Before(entry.LeaseExpiry.ExpiresAt) {
			result.Outcome = OutcomeDeferred
			return result, nil
		}
		receipt, err := callBefore(ctx, workDeadline, func(callCtx context.Context) (admission.LeaseReceipt, error) {
			return s.authority.ExpireLease(callCtx, *entry.LeaseExpiry)
		})
		if err != nil {
			if deadlineError(err) {
				return RecoveryResult{}, err
			}
			return s.failedRecovery(entry, err), nil
		}
		if receipt.Lease.ControlRunID != entry.ControlRunID ||
			receipt.Lease.Subject != entry.LeaseExpiry.Subject ||
			receipt.Lease.State != admission.LeaseExpired {
			return s.failedRecovery(entry, ErrInvalidRecord), nil
		}
		result.Outcome = OutcomeLeaseExpired
		return result, nil
	case RecoveryObservation:
		boundRecovery := entry.Observation.Recovery
		observation, err := callBefore(ctx, workDeadline, func(callCtx context.Context) (admission.ObservationReceipt, error) {
			return s.authority.Observe(callCtx, boundRecovery)
		})
		if err != nil {
			if deadlineError(err) {
				return RecoveryResult{}, err
			}
			return s.failedRecovery(entry, err), nil
		}
		fact := observation.Fact
		if observation.Authoritative ||
			!sameRecoveryIdentity(observation.Recovery, boundRecovery) ||
			!validProviderFact(fact, boundRecovery.SubjectDigest) {
			return s.failedRecovery(entry, ErrInvalidRecord), nil
		}
		switch fact.Status {
		case admission.ProviderAmbiguous:
			result.Outcome = OutcomeAmbiguous
			backoff := NextBackoff(s.clock().UTC(), entry.Retry, RetryAmbiguous, s.policy)
			result.Backoff = &backoff
			return result, nil
		case admission.ProviderEffectObserved:
			receipt, err := callBefore(ctx, workDeadline, func(callCtx context.Context) (admission.RecoveryReceipt, error) {
				return s.authority.ScannerApply(callCtx, admission.ScannerApplyRequest{
					Identity: entry.Observation.ApplyIdentity, Recovery: boundRecovery,
					ScannerAuthority: s.scannerAuthority, Fact: fact,
				})
			})
			if err != nil {
				if deadlineError(err) {
					return RecoveryResult{}, err
				}
				return s.failedRecovery(entry, err), nil
			}
			if !sameRecoveryIdentity(receipt.Recovery.Identity, boundRecovery) ||
				receipt.Recovery.State != admission.RecoveryApplied {
				return s.failedRecovery(entry, ErrInvalidRecord), nil
			}
			result.Outcome = OutcomeApplied
			return result, nil
		case admission.ProviderNotPerformed, admission.ProviderCanceled, admission.ProviderFenced:
			receipt, err := callBefore(ctx, workDeadline, func(callCtx context.Context) (admission.RecoveryReceipt, error) {
				return s.authority.CancelOrFence(callCtx, admission.FenceRequest{
					Identity: entry.Observation.FenceIdentity, Recovery: boundRecovery,
					Proof: admission.FenceProof{Status: fact.Status, ReceiptID: fact.ReceiptID},
				})
			})
			if err != nil {
				if deadlineError(err) {
					return RecoveryResult{}, err
				}
				return s.failedRecovery(entry, err), nil
			}
			if !sameRecoveryIdentity(receipt.Recovery.Identity, boundRecovery) {
				return s.failedRecovery(entry, ErrInvalidRecord), nil
			}
			wantState := admission.RecoveryFenced
			wantRetry := false
			if fact.Status == admission.ProviderNotPerformed {
				wantState, wantRetry = admission.RecoveryNotPerformed, true
			}
			if fact.Status == admission.ProviderCanceled {
				wantState, wantRetry = admission.RecoveryCanceled, true
			}
			if receipt.Recovery.State != wantState || receipt.RetryAllowed != wantRetry {
				return s.failedRecovery(entry, ErrInvalidRecord), nil
			}
			if receipt.RetryAllowed {
				result.Outcome = OutcomeRetryAllowed
				backoff := NextBackoff(s.clock().UTC(), entry.Retry, RetryProvenSafe, s.policy)
				result.Backoff = &backoff
				return result, nil
			}
			result.Outcome = OutcomeFenced
			return result, nil
		default:
			return s.failedRecovery(entry, ErrInvalidRecord), nil
		}
	default:
		return s.failedRecovery(entry, ErrInvalidRecord), nil
	}
}

type boundedCallResult[T any] struct {
	value T
	err   error
}

func callBefore[T any](
	ctx context.Context,
	deadline time.Time,
	call func(context.Context) (T, error),
) (T, error) {
	var zero T
	if !time.Now().Before(deadline) {
		return zero, context.DeadlineExceeded
	}
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	completed := make(chan boundedCallResult[T], 1)
	go func() {
		value, err := call(callCtx)
		completed <- boundedCallResult[T]{value: value, err: err}
	}()
	select {
	case result := <-completed:
		return result.value, result.err
	case <-callCtx.Done():
		return zero, callCtx.Err()
	}
}

func deadlineError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (s *Service) failedRecovery(entry RecoveryEntry, err error) RecoveryResult {
	code, retryable, outcome := classifyRecoveryError(err)
	result := RecoveryResult{EntryID: entry.ID, ControlRunID: entry.ControlRunID, Outcome: outcome}
	if retryable {
		backoff := NextBackoff(s.clock().UTC(), entry.Retry, code, s.policy)
		result.Backoff = &backoff
	} else if outcome != OutcomeFenced {
		result.Backoff = nonRetryable(code, entry.Retry)
	}
	return result
}

func classifyRecoveryError(err error) (RetryCode, bool, RecoveryOutcome) {
	var safe *admission.Error
	if errors.As(err, &safe) {
		switch safe.Code {
		case admission.CodeFenced, admission.CodeTerminal:
			return RetryFenced, false, OutcomeFenced
		case admission.CodeLeaseBusy, admission.CodeQuota, admission.CodeNotExpired:
			return RetryContention, true, OutcomeFailed
		case admission.CodeAmbiguous, admission.CodeStore:
			return RetryAmbiguous, true, OutcomeAmbiguous
		default:
			return RetrySemantic, false, OutcomeFailed
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return RetryTransient, true, OutcomeBudgetExhausted
	}
	if errors.Is(err, admission.ErrLeaseBusy) || errors.Is(err, admission.ErrQuota) ||
		errors.Is(err, admission.ErrNotExpired) {
		return RetryContention, true, OutcomeFailed
	}
	if errors.Is(err, ErrInvalidRecord) || errors.Is(err, ErrInvalidPolicy) {
		return RetrySemantic, false, OutcomeFailed
	}
	return RetryTransient, true, OutcomeFailed
}

func nonRetryable(code RetryCode, state RetryState) *Backoff {
	return &Backoff{Code: code, Step: state.Step}
}

func validateRecoveryEntry(entry RecoveryEntry) error {
	if !bounded(entry.ID, 128) || !bounded(entry.ControlRunID, 128) || entry.DueAt.IsZero() ||
		len(entry.SourceEventID) > 128 ||
		(entry.Retry.Step == 0) != entry.Retry.NextAttemptAt.IsZero() {
		return typedError(CodeInvalidRequest, "recovery_entry", ErrInvalidRecord)
	}
	switch entry.Kind {
	case RecoveryLeaseExpiry:
		if entry.LeaseExpiry == nil || entry.Observation != nil ||
			entry.LeaseExpiry.ControlRunID != entry.ControlRunID || entry.LeaseExpiry.ExpiresAt.IsZero() ||
			!validTransitionIdentity(entry.LeaseExpiry.Identity) {
			return typedError(CodeInvalidRequest, "recovery_entry", ErrInvalidRecord)
		}
	case RecoveryObservation:
		if entry.Observation == nil || entry.LeaseExpiry != nil ||
			entry.Observation.Start.Recovery.ControlRunID != entry.ControlRunID ||
			!validTransitionIdentity(entry.Observation.Start.Identity) ||
			!validTransitionIdentity(entry.Observation.FenceIdentity) ||
			!validTransitionIdentity(entry.Observation.ApplyIdentity) ||
			!validRequestedRecovery(entry.Observation.Start.Recovery) ||
			(entry.Observation.Recovery.InstallationID != "" &&
				!matchesRequestedRecovery(entry.Observation.Recovery, entry.Observation.Start.Recovery)) {
			return typedError(CodeInvalidRequest, "recovery_entry", ErrInvalidRecord)
		}
	default:
		return typedError(CodeInvalidRequest, "recovery_entry", ErrInvalidRecord)
	}
	return nil
}

func validateRecoveryCandidate(entry RecoveryEntry) error {
	if err := validateRecoveryEntry(entry); err != nil || entry.SourceEventID != "" ||
		entry.Retry.Step != 0 || !entry.Retry.NextAttemptAt.IsZero() ||
		(entry.Observation != nil && entry.Observation.Recovery.InstallationID != "") {
		return typedError(CodeInvalidRequest, "schedule_recovery", ErrInvalidRecord)
	}
	return nil
}

func validateRegisteredRecovery(entry RecoveryEntry) error {
	if err := validateRecoveryEntry(entry); err != nil || !bounded(entry.SourceEventID, 128) ||
		entry.Retry.Step != 0 || !entry.Retry.NextAttemptAt.IsZero() ||
		(entry.Observation != nil && !sameRecoveryIdentity(
			entry.Observation.Recovery,
			withRecoveryInstallation(entry.Observation.Start.Recovery, entry.Observation.Recovery.InstallationID),
		)) {
		return typedError(CodeAuthority, "recovery_register", ErrInvalidRecord)
	}
	return nil
}

func withRecoveryInstallation(identity admission.RecoveryIdentity, installationID string) admission.RecoveryIdentity {
	identity.InstallationID = installationID
	return identity
}

func validTransitionIdentity(identity admission.TransitionIdentity) bool {
	return bounded(identity.ActionID, 128) && bounded(identity.IdempotencyKey, 256) &&
		bounded(identity.OutcomeEventID, 128) && identity.OutcomeKind == journal.EventActionResult &&
		identity.GraphRevision > 0 && identity.Generation > 0 && len(identity.TaskID) <= 128 &&
		len(identity.AttemptID) <= 128 && (identity.AttemptID == "" || identity.TaskID != "")
}

func validRequestedRecovery(identity admission.RecoveryIdentity) bool {
	return identity.InstallationID == "" && bounded(identity.ControlRunID, 128) &&
		bounded(identity.ActionID, 128) && identity.Generation > 0 && journal.ValidDigest(identity.SubjectDigest)
}

func sameRecoveryIdentity(got, want admission.RecoveryIdentity) bool {
	return got == want && got.InstallationID != ""
}

func matchesRequestedRecovery(got, want admission.RecoveryIdentity) bool {
	return got.InstallationID != "" &&
		(want.InstallationID == "" || got.InstallationID == want.InstallationID) &&
		got.ControlRunID == want.ControlRunID && got.ActionID == want.ActionID &&
		got.Generation == want.Generation && got.SubjectDigest == want.SubjectDigest
}

func validProviderFact(fact admission.ProviderFact, subjectDigest string) bool {
	if fact.SubjectDigest != subjectDigest {
		return false
	}
	switch fact.Status {
	case admission.ProviderAmbiguous:
		return fact.ReceiptID == ""
	case admission.ProviderEffectObserved, admission.ProviderNotPerformed,
		admission.ProviderCanceled, admission.ProviderFenced:
		return bounded(fact.ReceiptID, 128)
	default:
		return false
	}
}
