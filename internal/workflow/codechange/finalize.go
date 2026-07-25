package codechange

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/araihu/paje/internal/approval"
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
	unlock, err := s.finalizeLocks.Lock(ctx, runID)
	if err != nil {
		return templatecodechange.Result{}, err
	}
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
	if finalizeClosed(record) {
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
		result, resultErr := s.resultFromRecordBounded(ctx, record)
		return result, errors.Join(err, resultErr)
	}
	if record.OutcomeMemorySaved {
		record, err = s.finishSavedOutcome(ctx, record.ID)
		record = s.reloadForResult(ctx, runID, record)
		result, resultErr := s.resultFromRecordBounded(ctx, record)
		return result, errors.Join(err, resultErr)
	}
	if finalizeClosed(record) {
		return s.resultFromRecordBounded(ctx, record)
	}
	if err := s.validateDurableEvidence(ctx, record, input); err != nil {
		result, resultErr := s.resultFromRecordBounded(ctx, record)
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
		failure := finalizeMemoryFailure()
		if ctx.Err() != nil {
			failure = canceledFailure(finalizeStage)
		}
		failed, persistErr := s.failFinalize(ctx, record, ownership, failure)
		failed = s.reloadForResult(ctx, runID, failed)
		result, resultErr := s.resultFromRecordBounded(ctx, failed)
		return result, errors.Join(newPhaseError(failure, err), persistErr, resultErr)
	}

	beforeFinish := record
	record, err = s.finishFinalize(ctx, record.ID, ownership)
	if err != nil && ctx.Err() != nil {
		failure := canceledFailure(finalizeStage)
		failed, persistErr := s.failFinalize(ctx, beforeFinish, ownership, failure)
		failed = s.reloadForResult(ctx, runID, failed)
		result, resultErr := s.resultFromRecordBounded(ctx, failed)
		return result, errors.Join(newPhaseError(failure, ctx.Err()), err, persistErr, resultErr)
	}
	record = s.reloadForResult(ctx, runID, record)
	var result templatecodechange.Result
	var resultErr error
	if err != nil {
		result, resultErr = s.resultFromRecordBounded(ctx, record)
	} else {
		result, resultErr = s.resultFromRecord(ctx, record)
	}
	return result, errors.Join(err, resultErr)
}

func (s *Service) resultFromRecordBounded(
	ctx context.Context,
	record run.Record,
) (templatecodechange.Result, error) {
	resultCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	return s.resultFromRecord(resultCtx, record)
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
		if current.OutcomeMemorySaved || finalizeClosed(current) {
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
	if ownership.name == "" && !updated.OutcomeMemorySaved && !finalizeClosed(updated) {
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
	failure run.Failure,
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
		if !next.Terminal() && !failure.Retryable {
			status := run.StatusFailed
			if failure.Class == run.FailureCanceled {
				status = run.StatusCanceled
			}
			next, err = run.Transition(next, status, s.clock())
			if err != nil {
				return run.Record{}, false, err
			}
		}
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

func finalizeClosed(record run.Record) bool {
	latest, found := latestStage(record, finalizeStage)
	return found && latest.Status == run.StageFailed &&
		latest.Failure != nil && !latest.Failure.Retryable
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
	if record.Terminal() {
		latest, found := latestStage(record, finalizeStage)
		if !found || latest.Failure == nil || !latest.Failure.Retryable {
			return phaseResult(record), nil
		}
	}
	record, err = s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.OutcomeMemorySaved || finalizeExhausted(current) {
			return current, false, nil
		}
		latest, found := latestStage(current, finalizeStage)
		if current.Terminal() && (!found || latest.Failure == nil || !latest.Failure.Retryable) {
			return current, false, nil
		}
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
	input, err := validateRunBinding(record)
	if err != nil {
		return result, err
	}
	if err := s.validateDurableEvidence(ctx, record, input); err != nil {
		return result, err
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

func (s *Service) validateDurableEvidence(
	ctx context.Context,
	record run.Record,
	input templatecodechange.Input,
) error {
	approvalStageResult, approvalFound, err := uniqueLatestProviderStage(record, approvalStage)
	if err != nil {
		return err
	}
	publishStageResult, publishFound, err := uniqueLatestProviderStage(record, publishStage)
	if err != nil {
		return err
	}
	if record.Artifact == nil {
		if record.Approval != nil || record.Publication != nil {
			return fmt.Errorf("%w: durable decision lacks artifact", ErrRunBinding)
		}
		if (approvalFound && approvalStageResult.Status == run.StageSucceeded) ||
			(publishFound && publishStageResult.Status == run.StageSucceeded) {
			return fmt.Errorf("%w: provider success lacks artifact", ErrRunBinding)
		}
		return nil
	}
	bundle, err := s.artifacts.Load(ctx, *record.Artifact)
	if err != nil {
		return err
	}
	if err := validateArtifactBinding(record, bundle); err != nil {
		return err
	}
	if input.Publication.Mode == "artifact" {
		if record.Approval != nil || record.Publication != nil {
			return fmt.Errorf("%w: artifact-only run contains publication evidence", ErrRunBinding)
		}
		if err := validateArtifactSkipStage(approvalStageResult, approvalFound); err != nil {
			return fmt.Errorf("%w: approval evidence: %v", ErrRunBinding, err)
		}
		if err := validateArtifactSkipStage(publishStageResult, publishFound); err != nil {
			return fmt.Errorf("%w: publication evidence: %v", ErrRunBinding, err)
		}
		return nil
	}
	if input.Publication.Mode != "pull_request" {
		return fmt.Errorf("%w: unsupported publication mode", ErrRunBinding)
	}
	approvalRequest := buildApprovalRequest(record, input, bundle)
	if err := validatePullRequestApprovalEvidence(
		record, approvalRequest, approvalStageResult, approvalFound,
	); err != nil {
		return fmt.Errorf("%w: approval evidence: %v", ErrRunBinding, err)
	}
	if err := validatePullRequestPublicationEvidence(
		record, input, publishStageResult, publishFound,
	); err != nil {
		return fmt.Errorf("%w: publication evidence: %v", ErrRunBinding, err)
	}
	return nil
}

func uniqueLatestProviderStage(
	record run.Record,
	name string,
) (run.StageResult, bool, error) {
	var latest run.StageResult
	found := false
	count := 0
	for _, stage := range record.Stages {
		if stage.Name != name {
			continue
		}
		switch {
		case !found || stage.Attempts > latest.Attempts:
			latest = stage
			found = true
			count = 1
		case stage.Attempts == latest.Attempts:
			count++
		}
	}
	if count > 1 {
		return run.StageResult{}, false, fmt.Errorf(
			"%w: %s stage has ambiguous latest evidence", ErrRunBinding, name,
		)
	}
	return latest, found, nil
}

func validateArtifactSkipStage(stage run.StageResult, found bool) error {
	if !found {
		return errors.New("skipped stage is missing")
	}
	if stage.Status != run.StageSkipped {
		return fmt.Errorf("stage status is %q, want %q", stage.Status, run.StageSkipped)
	}
	if !sameStringMap(stage.Evidence, map[string]string{"publication_mode": "artifact"}) {
		return errors.New("skip binding is missing or changed")
	}
	return nil
}

func validatePullRequestApprovalEvidence(
	record run.Record,
	request approval.Request,
	stage run.StageResult,
	found bool,
) error {
	if found && stage.Status == run.StageSkipped {
		return errors.New("pull-request approval cannot be skipped")
	}
	switch {
	case found && stage.Status == run.StageSucceeded && record.Approval == nil:
		return errors.New("succeeded approval stage lacks decision")
	case record.Approval != nil && (!found || stage.Status != run.StageSucceeded):
		return errors.New("approval decision lacks succeeded stage")
	case record.Status == run.StatusDeclined &&
		(record.Approval == nil || record.Approval.Approved):
		return errors.New("declined status lacks declined decision")
	case record.Approval != nil && !record.Approval.Approved &&
		record.Status != run.StatusDeclined:
		return errors.New("declined decision requires declined status")
	}
	if record.Approval == nil {
		if record.Status == run.StatusPublishing || record.Status == run.StatusSucceeded {
			return errors.New("publication status lacks approved decision")
		}
		return nil
	}
	if err := record.Approval.Validate(request); err != nil {
		return err
	}
	if !sameStringMap(stage.Evidence, approvalEvidence(request)) {
		return errors.New("approval request binding changed")
	}
	if record.Status != run.StatusDeclined && !record.Approval.Approved {
		return errors.New("approval decision is not approved")
	}
	return nil
}

func validatePullRequestPublicationEvidence(
	record run.Record,
	input templatecodechange.Input,
	stage run.StageResult,
	found bool,
) error {
	if record.Status == run.StatusDeclined {
		if record.Publication != nil || found {
			return errors.New("declined run contains publication evidence")
		}
		return nil
	}
	if found && stage.Status == run.StageSkipped {
		return errors.New("pull-request publication cannot be skipped")
	}
	switch {
	case found && stage.Status == run.StageSucceeded && record.Publication == nil:
		return errors.New("succeeded publish stage lacks result")
	case record.Publication != nil && (!found || stage.Status != run.StageSucceeded):
		return errors.New("publication result lacks succeeded stage")
	}
	if record.Publication == nil {
		if record.Status == run.StatusSucceeded {
			return errors.New("successful run lacks publication result")
		}
		return nil
	}
	if record.Approval == nil || !record.Approval.Approved {
		return errors.New("publication lacks approved decision")
	}
	request, err := buildPublisherRequest(record, input)
	if err != nil {
		return fmt.Errorf("publisher request: %v", err)
	}
	if err := validatePublicationResult(
		*record.Publication, request, input.Publication.Provider,
	); err != nil {
		return err
	}
	if !sameStringMap(stage.Evidence, publicationEvidence(request, *record.Publication)) {
		return errors.New("publication binding evidence is missing or changed")
	}
	return nil
}

type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedMutexEntry
}

type keyedMutexEntry struct {
	token chan struct{}
	refs  int
}

func (m *keyedMutex) Lock(ctx context.Context, key string) (func(), error) {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = make(map[string]*keyedMutexEntry)
	}
	entry := m.locks[key]
	if entry == nil {
		entry = &keyedMutexEntry{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		m.locks[key] = entry
	}
	entry.refs++
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		m.releaseEntry(key, entry)
		return nil, ctx.Err()
	case <-entry.token:
	}

	return func() {
		entry.token <- struct{}{}
		m.releaseEntry(key, entry)
	}, nil
}

func (m *keyedMutex) releaseEntry(key string, entry *keyedMutexEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry.refs--
	if entry.refs == 0 {
		delete(m.locks, key)
	}
}
