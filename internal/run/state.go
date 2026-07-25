package run

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"time"
)

func Validate(record Record) error {
	canonicalInput, err := CanonicalInput(record.Input)
	if err != nil {
		return err
	}
	switch {
	case !runIDPattern.MatchString(record.ID):
		return invalidRecord("run ID is invalid")
	case record.Version == 0:
		return invalidRecord("version must be positive")
	case strings.TrimSpace(record.Template.Name) == "" || record.Template.Version <= 0:
		return invalidRecord("template is invalid")
	case strings.TrimSpace(record.InputHash) == "":
		return invalidRecord("input hash is required")
	case len(record.Input) == 0:
		return invalidRecord("input is required")
	case !bytes.Equal(record.Input, canonicalInput):
		return invalidRecord("input is not canonical JSON")
	case strings.TrimSpace(record.RepositoryURI) == "":
		return invalidRecord("repository URI is required")
	case strings.TrimSpace(record.BaseRef) == "":
		return invalidRecord("base ref is required")
	case record.PublicationMode != "artifact" && record.PublicationMode != "pull_request":
		return invalidRecord("publication mode is invalid")
	case !validStatus(record.Status):
		return invalidRecord("status is invalid")
	case record.CreatedAt.IsZero() || record.UpdatedAt.IsZero():
		return invalidRecord("timestamps are required")
	case record.UpdatedAt.Before(record.CreatedAt):
		return invalidRecord("updated time precedes created time")
	}

	seenStages := make(map[string]int, len(record.Stages))
	for _, stage := range record.Stages {
		if err := validateStage(stage); err != nil {
			return err
		}
		if previous, exists := seenStages[stage.Name]; exists && stage.Attempts <= previous {
			return invalidRecord("stage attempts are not strictly increasing")
		}
		seenStages[stage.Name] = stage.Attempts
	}
	if record.Artifact != nil && record.Artifact.RunID != record.ID {
		return invalidRecord("artifact run ID differs from record")
	}
	if record.Approval != nil {
		if record.Artifact == nil {
			return invalidRecord("approval requires artifact")
		}
		if record.Approval.RunID != record.ID || record.Approval.ArtifactDigest != record.Artifact.Digest {
			return invalidRecord("approval binding differs from artifact")
		}
	}
	if record.Publication != nil && record.Artifact == nil {
		return invalidRecord("publication requires artifact")
	}

	switch record.Status {
	case StatusAwaitingApproval:
		if record.Artifact == nil {
			return invalidRecord("awaiting approval requires artifact")
		}
	case StatusPublishing:
		if record.Artifact == nil {
			return invalidRecord("publishing requires artifact")
		}
		if record.PublicationMode == "pull_request" && (record.Approval == nil || !record.Approval.Approved) {
			return invalidRecord("publishing pull request requires approval")
		}
	case StatusSucceeded:
		if record.Artifact == nil {
			return invalidRecord("successful run requires artifact")
		}
		if !record.OutcomeMemorySaved {
			return invalidRecord("successful run requires saved outcome memory")
		}
		if record.PublicationMode == "pull_request" {
			if record.Approval == nil || !record.Approval.Approved {
				return invalidRecord("successful pull request run requires approved decision")
			}
			if record.Publication == nil {
				return invalidRecord("successful pull request run requires publication")
			}
		}
	case StatusFailed, StatusCanceled:
		if record.Failure == nil {
			return invalidRecord("failed or canceled run requires failure")
		}
	case StatusDeclined:
		if record.Approval == nil || record.Approval.Approved {
			return invalidRecord("declined run requires declined approval")
		}
	}
	if record.Failure != nil {
		if err := validateFailure(*record.Failure); err != nil {
			return err
		}
		if record.Terminal() && record.Failure.Retryable {
			return invalidRecord("terminal run cannot be retryable")
		}
	}
	if record.Status == StatusCanceled &&
		(record.Failure == nil || record.Failure.Class != FailureCanceled || record.Failure.Retryable) {
		return invalidRecord("canceled run requires non-retryable canceled failure")
	}
	return nil
}

func Transition(record Record, next Status, now time.Time) (Record, error) {
	if err := Validate(record); err != nil {
		return Record{}, fmt.Errorf("transition source: %w", err)
	}
	if record.Terminal() {
		return Record{}, fmt.Errorf("%w: terminal status %q cannot change", ErrInvalidTransition, record.Status)
	}
	if now.IsZero() || now.Before(record.UpdatedAt) {
		return Record{}, fmt.Errorf("%w: transition time is invalid", ErrInvalidTransition)
	}
	cloned := CloneRecord(record)
	cloned.Status = next
	cloned.UpdatedAt = now
	if err := validateEdge(record.Status, next, cloned); err != nil {
		return Record{}, err
	}
	if err := Validate(cloned); err != nil {
		return Record{}, err
	}
	return cloned, nil
}

func PrepareSave(current, next Record) (Record, error) {
	if err := Validate(current); err != nil {
		return Record{}, fmt.Errorf("prepare save current: %w", err)
	}
	if next.Version != current.Version {
		return Record{}, fmt.Errorf("%w: caller changed version", ErrVersionConflict)
	}
	if !SameImmutableIdentity(current, next) {
		return Record{}, invalidRecord("immutable identity or input changed")
	}
	if current.Terminal() {
		if err := validateTerminalUpdate(current, next); err != nil {
			return Record{}, err
		}
	} else {
		if err := validateMonotonicEvidence(current, next); err != nil {
			return Record{}, err
		}
	}
	if next.UpdatedAt.Before(current.UpdatedAt) {
		return Record{}, invalidRecord("updated time moved backward")
	}
	if current.Status != next.Status {
		if current.Terminal() {
			return Record{}, fmt.Errorf("%w: terminal status %q cannot change", ErrInvalidTransition, current.Status)
		}
		if err := validateEdge(current.Status, next.Status, next); err != nil {
			return Record{}, err
		}
	}
	if err := Validate(next); err != nil {
		return Record{}, err
	}
	saved := CloneRecord(next)
	saved.Version = current.Version + 1
	return saved, nil
}

func UpsertStage(record Record, result StageResult) (Record, error) {
	if err := validateStage(result); err != nil {
		return Record{}, err
	}
	cloned := CloneRecord(record)
	highestAttempt := 0
	for index := range cloned.Stages {
		existing := cloned.Stages[index]
		if existing.Name != result.Name {
			continue
		}
		if existing.Attempts > highestAttempt {
			highestAttempt = existing.Attempts
		}
	}
	if result.Attempts < highestAttempt {
		return Record{}, invalidRecord("stage attempt moved backward")
	}
	for index := range cloned.Stages {
		existing := cloned.Stages[index]
		if existing.Name != result.Name || existing.Attempts != result.Attempts {
			continue
		}
		if stageFinished(existing) && !stageFinished(result) {
			return Record{}, invalidRecord("finished stage cannot become unfinished")
		}
		cloned.Stages[index] = cloneStage(result)
		return cloned, nil
	}
	cloned.Stages = append(cloned.Stages, cloneStage(result))
	return cloned, nil
}

func validateMonotonicEvidence(current, next Record) error {
	switch {
	case current.BaseSHA != "" && next.BaseSHA != current.BaseSHA:
		return invalidRecord("base SHA is write-once")
	case (current.BaseSHA != "" || current.MemorySnapshot != nil) &&
		!reflect.DeepEqual(current.MemorySnapshot, next.MemorySnapshot):
		return invalidRecord("memory snapshot is write-once")
	case current.Artifact != nil && !reflect.DeepEqual(current.Artifact, next.Artifact):
		return invalidRecord("artifact is write-once")
	case current.Approval != nil && !reflect.DeepEqual(current.Approval, next.Approval):
		return invalidRecord("approval is write-once")
	case current.Publication != nil && !reflect.DeepEqual(current.Publication, next.Publication):
		return invalidRecord("publication is write-once")
	case current.OutcomeMemorySaved && !next.OutcomeMemorySaved:
		return invalidRecord("outcome memory marker cannot regress")
	}
	if err := validateMonotonicFailure(current, next); err != nil {
		return err
	}
	return validateMonotonicStages(current.Stages, next.Stages)
}

func validateMonotonicStages(current, next []StageResult) error {
	nextByKey := make(map[string]StageResult, len(next))
	latestAttempt := make(map[string]int, len(current))
	for _, stage := range current {
		if stage.Attempts > latestAttempt[stage.Name] {
			latestAttempt[stage.Name] = stage.Attempts
		}
	}
	for _, stage := range next {
		nextByKey[stageKey(stage)] = stage
	}
	for _, existing := range current {
		candidate, ok := nextByKey[stageKey(existing)]
		if !ok {
			return invalidRecord("stage history cannot be removed")
		}
		if stageFinished(existing) && !reflect.DeepEqual(existing, candidate) &&
			!(existing.Attempts == latestAttempt[existing.Name] && isRetryExhaustion(existing, candidate)) {
			return invalidRecord("finished stage evidence is immutable")
		}
		if !stageFinished(existing) &&
			(candidate.Name != existing.Name || candidate.Attempts != existing.Attempts ||
				!candidate.StartedAt.Equal(existing.StartedAt)) {
			return invalidRecord("running stage identity is immutable")
		}
	}
	return nil
}

func isRetryExhaustion(current, next StageResult) bool {
	if current.Failure == nil || next.Failure == nil || !isRetryExhaustionFailure(*current.Failure, *next.Failure) {
		return false
	}
	currentWithoutFailure := current
	currentWithoutFailure.Failure = nil
	nextWithoutFailure := next
	nextWithoutFailure.Failure = nil
	return reflect.DeepEqual(currentWithoutFailure, nextWithoutFailure)
}

func isRetryExhaustionFailure(current, next Failure) bool {
	if !current.Retryable || next.Retryable || next.CauseCode != "retries_exhausted" {
		return false
	}
	current.Retryable = false
	current.CauseCode = "retries_exhausted"
	return reflect.DeepEqual(current, next)
}

func validateMonotonicFailure(current, next Record) error {
	if reflect.DeepEqual(current.Failure, next.Failure) {
		return nil
	}
	if current.Failure == nil {
		if next.Failure == nil {
			return nil
		}
		latest, found := latestStage(next.Stages, next.Failure.Stage)
		if !found || latest.Failure == nil || !reflect.DeepEqual(latest.Failure, next.Failure) {
			return invalidRecord("new top-level failure is not bound to latest stage attempt")
		}
		return nil
	}
	currentLatest, currentFound := latestStage(current.Stages, current.Failure.Stage)
	if !currentFound || currentLatest.Failure == nil || !reflect.DeepEqual(currentLatest.Failure, current.Failure) {
		return invalidRecord("top-level failure is not bound to latest stage attempt")
	}
	if next.Failure != nil && isRetryExhaustionFailure(*current.Failure, *next.Failure) {
		nextLatest, found := latestStage(next.Stages, next.Failure.Stage)
		if !found || nextLatest.Attempts != currentLatest.Attempts ||
			nextLatest.Failure == nil || !reflect.DeepEqual(nextLatest.Failure, next.Failure) ||
			!isRetryExhaustion(currentLatest, nextLatest) {
			return invalidRecord("retry exhaustion is not bound to latest stage attempt")
		}
		return nil
	}

	stageName := current.Failure.Stage
	if next.Failure != nil {
		stageName = next.Failure.Stage
	}
	nextLatest, found := latestStage(next.Stages, stageName)
	if !found {
		return invalidRecord("failure change lacks a new stage attempt")
	}
	if stageName == current.Failure.Stage && nextLatest.Attempts <= currentLatest.Attempts {
		return invalidRecord("failure changed without a newer stage attempt")
	}
	if next.Failure == nil {
		if nextLatest.Failure != nil {
			return invalidRecord("cleared top-level failure differs from latest stage")
		}
		return nil
	}
	if nextLatest.Failure == nil || !reflect.DeepEqual(nextLatest.Failure, next.Failure) {
		return invalidRecord("top-level failure differs from latest stage")
	}
	return nil
}

func latestStage(stages []StageResult, name string) (StageResult, bool) {
	var latest StageResult
	found := false
	for _, stage := range stages {
		if stage.Name == name && (!found || stage.Attempts > latest.Attempts) {
			latest = stage
			found = true
		}
	}
	return latest, found
}

func validateTerminalUpdate(current, next Record) error {
	switch {
	case current.Status != next.Status:
		return invalidRecord("terminal status is immutable")
	case current.BaseSHA != next.BaseSHA:
		return invalidRecord("terminal base SHA is immutable")
	case !reflect.DeepEqual(current.MemorySnapshot, next.MemorySnapshot):
		return invalidRecord("terminal memory snapshot is immutable")
	case !reflect.DeepEqual(current.Artifact, next.Artifact):
		return invalidRecord("terminal artifact is immutable")
	case !reflect.DeepEqual(current.Approval, next.Approval):
		return invalidRecord("terminal approval is immutable")
	case !reflect.DeepEqual(current.Publication, next.Publication):
		return invalidRecord("terminal publication is immutable")
	case !reflect.DeepEqual(current.Failure, next.Failure):
		return invalidRecord("terminal failure is immutable")
	case current.OutcomeMemorySaved && !next.OutcomeMemorySaved:
		return invalidRecord("terminal outcome memory marker cannot regress")
	case !reflect.DeepEqual(nonFinalizeStages(current.Stages), nonFinalizeStages(next.Stages)):
		return invalidRecord("terminal non-finalize stages are immutable")
	}
	return validateMonotonicStages(current.Stages, next.Stages)
}

func nonFinalizeStages(stages []StageResult) []StageResult {
	result := make([]StageResult, 0, len(stages))
	for _, stage := range stages {
		if stage.Name != "finalize" {
			result = append(result, stage)
		}
	}
	return result
}

func stageKey(stage StageResult) string {
	return fmt.Sprintf("%s\x00%d", stage.Name, stage.Attempts)
}

func validateEdge(current, next Status, record Record) error {
	legal := false
	switch {
	case next == StatusFailed || next == StatusCanceled:
		legal = !recordStatusTerminal(current)
	case current == StatusPending && next == StatusResolving:
		legal = true
	case current == StatusResolving && next == StatusExecuting:
		legal = true
	case current == StatusExecuting && next == StatusAwaitingApproval:
		legal = true
	case current == StatusAwaitingApproval && next == StatusPublishing:
		legal = true
	case current == StatusPublishing && next == StatusSucceeded:
		legal = true
	case current == StatusAwaitingApproval && next == StatusDeclined:
		legal = true
	case current == StatusExecuting && next == StatusSucceeded:
		legal = record.PublicationMode == "artifact" && finalizeSucceeded(record)
	}
	if !legal {
		return fmt.Errorf("%w: %q to %q", ErrInvalidTransition, current, next)
	}
	return nil
}

func validStatus(status Status) bool {
	switch status {
	case StatusPending, StatusResolving, StatusExecuting, StatusAwaitingApproval,
		StatusPublishing, StatusSucceeded, StatusFailed, StatusCanceled, StatusDeclined:
		return true
	default:
		return false
	}
}

func recordStatusTerminal(status Status) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusDeclined:
		return true
	default:
		return false
	}
}

func finalizeSucceeded(record Record) bool {
	for _, stage := range record.Stages {
		if stage.Name == "finalize" && stage.Status == StageSucceeded {
			return true
		}
	}
	return false
}

func validateStage(stage StageResult) error {
	switch {
	case strings.TrimSpace(stage.Name) == "":
		return invalidRecord("stage name is required")
	case stage.Attempts <= 0:
		return invalidRecord("stage attempt must be positive")
	case stage.StartedAt.IsZero():
		return invalidRecord("stage start time is required")
	case !validStageStatus(stage.Status):
		return invalidRecord("stage status is invalid")
	case stage.Status == StageRunning && !stage.FinishedAt.IsZero():
		return invalidRecord("running stage has finish time")
	case stage.Status != StageRunning && stage.FinishedAt.IsZero():
		return invalidRecord("finished stage lacks finish time")
	case !stage.FinishedAt.IsZero() && stage.FinishedAt.Before(stage.StartedAt):
		return invalidRecord("stage finish precedes start")
	}
	if stage.Failure != nil {
		return validateFailure(*stage.Failure)
	}
	return nil
}

func validateFailure(failure Failure) error {
	switch {
	case strings.TrimSpace(failure.Stage) == "":
		return invalidRecord("failure stage is required")
	case !validFailureClass(failure.Class):
		return invalidRecord("failure class is invalid")
	case strings.TrimSpace(failure.CauseCode) == "":
		return invalidRecord("failure cause code is required")
	case failure.Diagnostic != SafeDiagnostic(failure.Diagnostic):
		return invalidRecord("failure diagnostic is unsafe")
	default:
		return nil
	}
}

func validFailureClass(class FailureClass) bool {
	switch class {
	case FailureInput, FailureEnvironment, FailureAgent, FailureVerification,
		FailurePolicy, FailureApproval, FailurePublication, FailureCleanup,
		FailureCanceled, FailureInternal:
		return true
	default:
		return false
	}
}

func validStageStatus(status StageStatus) bool {
	switch status {
	case StageRunning, StageSucceeded, StageSkipped, StageWarning, StageFailed:
		return true
	default:
		return false
	}
}

func stageFinished(stage StageResult) bool {
	return stage.Status != StageRunning && !stage.FinishedAt.IsZero()
}
