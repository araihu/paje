package codechange

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/run"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

const (
	finalizeStage        = "finalize"
	outcomeSearchLimit   = 100
	outcomeFailureReason = "outcome_memory_failed"
)

// Finalize saves one scoped outcome memory and returns a durable reconstruction
// of the code-change result.
func (s *Service) Finalize(
	ctx context.Context,
	runID string,
) (templatecodechange.Result, error) {
	unlock := s.finalizeLocks.Lock(runID)
	defer unlock()

	record, err := s.runs.Load(ctx, runID)
	if err != nil {
		return templatecodechange.Result{}, err
	}
	input, err := validateRunBinding(record)
	if err != nil {
		result, resultErr := s.resultFromRecord(ctx, record)
		return result, errors.Join(err, resultErr)
	}
	if finalizeExhausted(record) {
		return s.resultFromRecord(ctx, record)
	}
	if record.OutcomeMemorySaved {
		record, err = s.finishSavedOutcome(ctx, record.ID)
		record = s.reloadForResult(ctx, runID, record)
		if err != nil {
			result, resultErr := s.resultFromRecord(ctx, record)
			return result, errors.Join(err, resultErr)
		}
		return s.resultFromRecord(ctx, record)
	}

	record, ownership, err := s.beginFinalize(ctx, record)
	if err != nil {
		record = s.reloadForResult(ctx, runID, record)
		result, resultErr := s.resultFromRecord(ctx, record)
		return result, errors.Join(err, resultErr)
	}
	content := outcomeContent(record)
	tags := outcomeTags(record, input)
	query := fmt.Sprintf("Pajé run %s completed", record.ID)

	existing, err := s.memory.Search(ctx, query, outcomeSearchLimit, map[string]string{
		"user_id": tags["user_id"], "app_id": tags["app_id"], "run_id": record.ID,
	})
	if err == nil && !containsExactOutcome(existing, content) {
		err = s.memory.Save(ctx, content, tags)
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		failed, persistErr := s.failFinalize(ctx, record, ownership, err)
		failed = s.reloadForResult(ctx, runID, failed)
		result, resultErr := s.resultFromRecord(context.WithoutCancel(ctx), failed)
		return result, errors.Join(newPhaseError(finalizeMemoryFailure(), err), persistErr, resultErr)
	}

	record, err = s.finishFinalize(ctx, record.ID, ownership)
	record = s.reloadForResult(ctx, runID, record)
	result, resultErr := s.resultFromRecord(ctx, record)
	return result, errors.Join(err, resultErr)
}

func (s *Service) finishSavedOutcome(ctx context.Context, runID string) (run.Record, error) {
	return s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		if !current.OutcomeMemorySaved {
			return run.Record{}, false, errors.New("finish saved outcome: marker is missing")
		}
		if current.Terminal() {
			return current, false, nil
		}
		if !finalizable(current) {
			return run.Record{}, false, errors.New("finish saved outcome: run is not ready")
		}
		next := run.CloneRecord(current)
		now := s.clock()
		latest, found := latestStage(current, finalizeStage)
		switch {
		case found && latest.Status == run.StageSucceeded:
		case found && latest.Status == run.StageRunning:
			latest.Status = run.StageSucceeded
			latest.FinishedAt = now
			latest.Evidence = map[string]string{"outcome_memory_saved": "true"}
			var err error
			next, err = run.UpsertStage(next, latest)
			if err != nil {
				return run.Record{}, false, err
			}
		default:
			stage := run.StageResult{
				Name: finalizeStage, Status: run.StageSucceeded,
				StartedAt: now, FinishedAt: now,
				Attempts: stageAttempt(current, finalizeStage) + 1,
				Evidence: map[string]string{"outcome_memory_saved": "true"},
			}
			var err error
			next, err = run.UpsertStage(next, stage)
			if err != nil {
				return run.Record{}, false, err
			}
		}
		next.Failure = nilIfFinalizeFailure(next.Failure)
		next.UpdatedAt = now
		var err error
		switch next.PublicationMode {
		case "artifact":
			next, err = run.Transition(next, run.StatusSucceeded, now)
		case "pull_request":
			next, err = run.Transition(next, run.StatusSucceeded, now)
		default:
			err = errors.New("finish saved outcome: unsupported publication mode")
		}
		return next, true, err
	})
}

func (s *Service) reloadForResult(
	ctx context.Context,
	runID string,
	record run.Record,
) run.Record {
	if record.ID != "" {
		return record
	}
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	loaded, err := s.runs.Load(loadCtx, runID)
	if err == nil {
		return loaded
	}
	return record
}

func (s *Service) beginFinalize(
	ctx context.Context,
	record run.Record,
) (run.Record, stageOwnership, error) {
	var ownership stageOwnership
	updated, err := s.mutate(ctx, record.ID, func(current run.Record) (run.Record, bool, error) {
		if _, err := validateRunBinding(current); err != nil {
			return run.Record{}, false, err
		}
		if current.OutcomeMemorySaved || finalizeExhausted(current) {
			return current, false, nil
		}
		if !finalizable(current) {
			return run.Record{}, false, fmt.Errorf("finalize run %q: status %q is not ready", current.ID, current.Status)
		}
		if latest, found := latestStage(current, finalizeStage); found &&
			latest.Status == run.StageRunning {
			ownership = stageOwnership{
				name: finalizeStage, attempt: latest.Attempts, startedAt: latest.StartedAt,
			}
			return current, false, nil
		}

		next := run.CloneRecord(current)
		if next.Failure != nil && next.Failure.Stage == finalizeStage && !next.Terminal() {
			next.Failure = nil
		}
		now := s.clock()
		stage := run.StageResult{
			Name: finalizeStage, Status: run.StageRunning, StartedAt: now,
			Attempts: stageAttempt(current, finalizeStage) + 1,
		}
		var err error
		next, err = run.UpsertStage(next, stage)
		if err != nil {
			return run.Record{}, false, err
		}
		next.UpdatedAt = now
		ownership = stageOwnership{
			name: finalizeStage, attempt: stage.Attempts, startedAt: stage.StartedAt,
		}
		return next, true, nil
	})
	if err != nil {
		return updated, stageOwnership{}, err
	}
	if ownership.name == "" && !updated.OutcomeMemorySaved && !finalizeExhausted(updated) {
		return updated, stageOwnership{}, errors.New("finalize stage ownership is missing")
	}
	return updated, ownership, nil
}

func (s *Service) finishFinalize(
	ctx context.Context,
	runID string,
	ownership stageOwnership,
) (run.Record, error) {
	return s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.OutcomeMemorySaved {
			return current, false, nil
		}
		if !finalizeOwnership(current, ownership) {
			return current, false, ErrPhaseInProgress
		}
		next := run.CloneRecord(current)
		next.OutcomeMemorySaved = true
		next.Failure = nilIfFinalizeFailure(next.Failure)
		stage := run.StageResult{
			Name: finalizeStage, Status: run.StageSucceeded,
			StartedAt: ownership.startedAt, FinishedAt: s.clock(),
			Attempts: ownership.attempt,
			Evidence: map[string]string{"outcome_memory_saved": "true"},
		}
		var err error
		next, err = run.UpsertStage(next, stage)
		if err != nil {
			return run.Record{}, false, err
		}
		next.UpdatedAt = s.clock()
		switch {
		case next.Terminal():
			return next, true, nil
		case next.PublicationMode == "artifact" && next.Status == run.StatusExecuting:
			next, err = run.Transition(next, run.StatusSucceeded, s.clock())
		case next.PublicationMode == "pull_request" && next.Status == run.StatusPublishing &&
			next.Approval != nil && next.Approval.Approved && next.Publication != nil:
			next, err = run.Transition(next, run.StatusSucceeded, s.clock())
		default:
			err = fmt.Errorf("finalize run %q: durable publication state is incomplete", next.ID)
		}
		return next, true, err
	})
}

func (s *Service) failFinalize(
	ctx context.Context,
	record run.Record,
	ownership stageOwnership,
	cause error,
) (run.Record, error) {
	persistCtx := ctx
	cancel := func() {}
	if ctx.Err() != nil {
		persistCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	}
	defer cancel()

	return s.mutate(persistCtx, record.ID, func(current run.Record) (run.Record, bool, error) {
		if current.OutcomeMemorySaved || finalizeExhausted(current) {
			return current, false, nil
		}
		if !finalizeOwnership(current, ownership) {
			return current, false, ErrPhaseInProgress
		}
		next := run.CloneRecord(current)
		failure := finalizeMemoryFailure()
		stage := run.StageResult{
			Name: finalizeStage, Status: run.StageFailed,
			StartedAt: ownership.startedAt, FinishedAt: s.clock(),
			Attempts: ownership.attempt, Failure: &failure,
		}
		var err error
		next, err = run.UpsertStage(next, stage)
		if err != nil {
			return run.Record{}, false, err
		}
		if !next.Terminal() {
			next.Failure = &failure
		}
		next.UpdatedAt = s.clock()
		return next, true, nil
	})
}

func finalizable(record run.Record) bool {
	if record.Terminal() {
		return true
	}
	if record.Artifact == nil {
		return false
	}
	switch record.PublicationMode {
	case "artifact":
		return record.Status == run.StatusExecuting
	case "pull_request":
		return record.Status == run.StatusPublishing &&
			record.Approval != nil && record.Approval.Approved &&
			record.Publication != nil
	default:
		return false
	}
}

func finalizeExhausted(record run.Record) bool {
	latest, found := latestStage(record, finalizeStage)
	return found && latest.Status == run.StageFailed && latest.Failure != nil &&
		!latest.Failure.Retryable && latest.Failure.CauseCode == "retries_exhausted"
}

func finalizeOwnership(record run.Record, ownership stageOwnership) bool {
	latest, found := latestStage(record, ownership.name)
	return found && latest.Status == run.StageRunning &&
		latest.Attempts == ownership.attempt &&
		latest.StartedAt.Equal(ownership.startedAt)
}

func finalizeMemoryFailure() run.Failure {
	return run.Failure{
		Stage: finalizeStage, Class: run.FailureInternal, Retryable: true,
		Diagnostic: outcomeFailureReason, CauseCode: outcomeFailureReason,
	}
}

func (s *Service) exhaustFinalize(ctx context.Context, runID string) (PhaseResult, error) {
	record, err := s.runs.Load(ctx, runID)
	if err != nil {
		return phaseResult(record), err
	}
	if _, err := validateRunBinding(record); err != nil {
		return phaseResult(record), err
	}
	if record.OutcomeMemorySaved || finalizeExhausted(record) {
		return phaseResult(record), nil
	}
	record, err = s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.OutcomeMemorySaved || finalizeExhausted(current) {
			return current, false, nil
		}
		latest, found := latestStage(current, finalizeStage)
		if !found || latest.Status != run.StageFailed || latest.Failure == nil ||
			!latest.Failure.Retryable {
			return run.Record{}, false, errors.New("exhaust finalize: no retryable failure")
		}
		next := run.CloneRecord(current)
		failure := *latest.Failure
		failure.Retryable = false
		failure.CauseCode = "retries_exhausted"
		latest.Failure = &failure
		next, err := run.UpsertStage(next, latest)
		if err != nil {
			return run.Record{}, false, err
		}
		next.UpdatedAt = s.clock()
		if current.Terminal() {
			return next, true, nil
		}
		next.Failure = &failure
		next, err = run.Transition(next, run.StatusFailed, s.clock())
		return next, true, err
	})
	return phaseResult(record), err
}

func nilIfFinalizeFailure(failure *run.Failure) *run.Failure {
	if failure != nil && failure.Stage == finalizeStage {
		return nil
	}
	return failure
}

func outcomeContent(record run.Record) string {
	statuses := make([]string, 0, 5)
	for _, name := range []string{"resolve", "execute", "approval", "publish"} {
		statuses = append(statuses, name+"="+outcomeStageStatus(record, name))
	}
	statuses = append(statuses, "finalize=succeeded")

	failure := "none"
	if record.Failure != nil && record.Failure.Stage != finalizeStage {
		failure = string(record.Failure.Class) + "/" + record.Failure.CauseCode
	}
	publication := "none"
	if record.Publication != nil {
		publication = record.Publication.PullRequestURL
	}
	artifactDigest := "none"
	if record.Artifact != nil {
		artifactDigest = "sha256:" + record.Artifact.Digest
	}
	return fmt.Sprintf(
		"Pajé run %s completed\nTemplate: %s\nBase SHA: %s\nArtifact: %s\nStages: %s\nFailure: %s\nPublication: %s",
		record.ID, record.Template.String(), record.BaseSHA, artifactDigest,
		strings.Join(statuses, ", "), failure, publication,
	)
}

func outcomeStageStatus(record run.Record, name string) string {
	stage, found := latestStage(record, name)
	if !found {
		return "not_run"
	}
	return string(stage.Status)
}

func outcomeTags(
	record run.Record,
	input templatecodechange.Input,
) map[string]string {
	status := record.Status
	if !record.Terminal() {
		status = run.StatusSucceeded
	}
	return map[string]string{
		"user_id": input.Tags["user_id"], "app_id": input.Tags["app_id"],
		"run_id": record.ID, "paje_run_id": record.ID,
		"paje_template": record.Template.String(), "paje_base_sha": record.BaseSHA,
		"paje_artifact_digest": outcomeArtifactDigest(record),
		"paje_status":          string(status),
	}
}

func outcomeArtifactDigest(record run.Record) string {
	if record.Artifact == nil {
		return ""
	}
	return record.Artifact.Digest
}

func containsExactOutcome(memories []memory.Memory, content string) bool {
	for _, item := range memories {
		if item.Content == content {
			return true
		}
	}
	return false
}

func (s *Service) resultFromRecord(
	ctx context.Context,
	record run.Record,
) (templatecodechange.Result, error) {
	result := templatecodechange.Result{
		RunID: record.ID, Status: record.Status, BaseSHA: record.BaseSHA,
	}
	if record.Artifact != nil {
		result.Artifact = *record.Artifact
		bundle, err := s.artifacts.Load(ctx, *record.Artifact)
		if err != nil {
			return result, err
		}
		if err := validateArtifactBinding(record, bundle); err != nil {
			return result, err
		}
		result.Verification = cloneVerification(bundle.Verification)
	}
	if record.Approval != nil {
		value := *record.Approval
		result.Approval = &value
	}
	if record.Publication != nil {
		value := *record.Publication
		result.Publication = &value
	}
	if record.Failure != nil {
		value := *record.Failure
		result.Failure = &value
	}
	return result, nil
}

type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedMutexEntry
}

type keyedMutexEntry struct {
	mu   sync.Mutex
	refs int
}

func (m *keyedMutex) Lock(key string) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = make(map[string]*keyedMutexEntry)
	}
	entry := m.locks[key]
	if entry == nil {
		entry = &keyedMutexEntry{}
		m.locks[key] = entry
	}
	entry.refs++
	m.mu.Unlock()
	entry.mu.Lock()

	return func() {
		entry.mu.Unlock()
		m.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(m.locks, key)
		}
		m.mu.Unlock()
	}
}
