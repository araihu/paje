package codechange

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/run"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

const publishStage = "publish"

// Publish publishes an approved immutable artifact. The injected publisher is
// responsible for provider-side idempotency; this service additionally avoids
// calls after a provider result has been durably checkpointed.
func (s *Service) Publish(ctx context.Context, runID string) (PhaseResult, error) {
	record, input, bundle, err := s.loadPublishable(ctx, runID)
	if err != nil || record.Terminal() {
		return phaseResult(record), err
	}
	if input.Publication.Mode == "artifact" {
		return s.skipStage(ctx, record, publishStage)
	}
	request, err := buildPublisherRequest(record, input)
	if err != nil {
		return s.recordPublishFailure(ctx, record, publishBindingFailure(), err)
	}
	approvalRequest := buildApprovalRequest(record, input, bundle)
	if err := validatePersistedApproval(record, approvalRequest); err != nil ||
		record.Approval == nil || !record.Approval.Approved {
		if err == nil {
			err = errors.New("approval decision is not approved")
		}
		return s.recordPublishFailure(ctx, record, publishBindingFailure(), err)
	}
	if record.Publication != nil {
		if err := validatePersistedPublication(record, request, input.Publication.Provider); err != nil {
			return s.recordPublishFailure(ctx, record, publishBindingFailure(), err)
		}
		return phaseResult(record), nil
	}

	record, started, err := s.beginStage(ctx, runID, publishStage, run.StatusPublishing)
	if err != nil {
		return phaseResult(record), err
	}
	if record.Terminal() {
		return phaseResult(record), nil
	}
	if !started {
		if record.Publication != nil {
			if err := validatePersistedPublication(record, request, input.Publication.Provider); err != nil {
				return s.recordPublishFailure(ctx, record, publishBindingFailure(), err)
			}
			return phaseResult(record), nil
		}
		return phaseInProgress(record)
	}
	ownership, err := ownershipFor(record, publishStage)
	if err != nil {
		return phaseResult(record), err
	}

	result, publishErr := s.publisher.Publish(ctx, request)
	if ctx.Err() != nil {
		return s.finishFailure(ctx, runID, canceledFailure(publishStage), ctx.Err(), ownership.attempt)
	}
	if publishErr != nil {
		failure := run.Failure{
			Stage: publishStage, Class: run.FailurePublication,
			Retryable:  typedRetryable(publishErr),
			Diagnostic: "publication provider request failed", CauseCode: "publication_provider_failed",
		}
		if errors.Is(publishErr, publisher.ErrConflict) {
			failure.Retryable = false
			failure.CauseCode = "publication_conflict"
			failure.Diagnostic = "publication conflicts with existing provider state"
		}
		return s.finishFailure(ctx, runID, failure, publishErr, ownership.attempt)
	}
	if err := validatePublicationResult(result, request, input.Publication.Provider); err != nil {
		failure := run.Failure{
			Stage: publishStage, Class: run.FailurePublication, Retryable: false,
			Diagnostic: "publication result binding is invalid",
			CauseCode:  "publication_binding_mismatch",
		}
		return s.finishFailure(ctx, runID, failure, err, ownership.attempt)
	}
	return s.persistPublication(ctx, runID, ownership, request, result)
}

func (s *Service) loadPublishable(
	ctx context.Context,
	runID string,
) (run.Record, templatecodechange.Input, artifact.Bundle, error) {
	record, err := s.runs.Load(ctx, runID)
	if err != nil {
		return run.Record{}, templatecodechange.Input{}, artifact.Bundle{}, err
	}
	input, err := validateRunBinding(record)
	if err != nil {
		return record, templatecodechange.Input{}, artifact.Bundle{}, err
	}
	if record.Artifact == nil {
		return record, input, artifact.Bundle{}, fmt.Errorf("%w: publish artifact is missing", ErrRunBinding)
	}
	bundle, err := s.artifacts.Load(ctx, *record.Artifact)
	if err != nil {
		return record, input, artifact.Bundle{}, err
	}
	if err := validateArtifactBinding(record, bundle); err != nil {
		return record, input, artifact.Bundle{}, err
	}
	return record, input, bundle, nil
}

func buildPublisherRequest(
	record run.Record,
	input templatecodechange.Input,
) (publisher.Request, error) {
	if record.Artifact == nil {
		return publisher.Request{}, errors.New("artifact is missing")
	}
	title := input.Publication.Title
	if title == "" {
		title = "Pajé code change " + record.ID
	}
	request := publisher.Request{
		RunID: record.ID, Repository: safePublicationRepository(record.RepositoryURI),
		BaseSHA: record.BaseSHA, TargetRef: input.Publication.TargetBranch,
		Branch: publicationBranch(record.ID), Artifact: *record.Artifact,
		Title: title,
		Body: fmt.Sprintf(
			"Automated Pajé code change.\n\nRun: %s\nArtifact: sha256:%s",
			record.ID, record.Artifact.Digest,
		),
		Draft: input.Publication.Draft,
	}
	if err := request.Validate(); err != nil {
		return publisher.Request{}, err
	}
	return request, nil
}

func (s *Service) persistPublication(
	ctx context.Context,
	runID string,
	ownership stageOwnership,
	request publisher.Request,
	result publisher.Result,
) (PhaseResult, error) {
	record, err := s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.Terminal() {
			return current, false, nil
		}
		if !s.ownsStage(current, ownership) {
			return current, false, ErrPhaseInProgress
		}
		input, err := validateRunBinding(current)
		if err != nil {
			return run.Record{}, false, err
		}
		currentRequest, err := buildPublisherRequest(current, input)
		if err != nil || currentRequest != request {
			return run.Record{}, false, fmt.Errorf("%w: publication request changed", ErrRunBinding)
		}
		if current.Publication != nil {
			if *current.Publication != result {
				return run.Record{}, false, fmt.Errorf("%w: publication result conflict", ErrRunBinding)
			}
			return current, false, nil
		}

		next := run.CloneRecord(current)
		resultCopy := result
		next.Publication = &resultCopy
		stage := run.StageResult{
			Name: publishStage, Status: run.StageSucceeded,
			StartedAt: ownership.startedAt, FinishedAt: s.clock(),
			Attempts: ownership.attempt, Evidence: publicationEvidence(request, result),
		}
		next, err = run.UpsertStage(next, stage)
		if err != nil {
			return run.Record{}, false, err
		}
		next.UpdatedAt = s.clock()
		return next, true, nil
	})
	if errors.Is(err, ErrPhaseInProgress) {
		return phaseInProgress(record)
	}
	return phaseResult(record), err
}

func validatePersistedPublication(
	record run.Record,
	request publisher.Request,
	expectedProvider string,
) error {
	if record.Publication == nil {
		return errors.New("publication result is missing")
	}
	if err := validatePublicationResult(*record.Publication, request, expectedProvider); err != nil {
		return err
	}
	stage, found := latestStage(record, publishStage)
	if !found || stage.Status != run.StageSucceeded ||
		!sameStringMap(stage.Evidence, publicationEvidence(request, *record.Publication)) {
		return errors.New("publication binding evidence is missing or changed")
	}
	return nil
}

func validatePublicationResult(
	result publisher.Result,
	request publisher.Request,
	expectedProvider string,
) error {
	if err := result.Validate(request); err != nil {
		return err
	}
	if result.Provider != expectedProvider {
		return errors.New("publication provider does not match requested provider")
	}
	parsed, err := url.Parse(result.PullRequestURL)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("publication URL contains unsafe components")
	}
	return nil
}

func publicationEvidence(
	request publisher.Request,
	result publisher.Result,
) map[string]string {
	return map[string]string{
		"run_id": request.RunID, "base_sha": request.BaseSHA,
		"target_branch": request.TargetRef, "publication_branch": request.Branch,
		"artifact_digest": request.Artifact.Digest, "provider": result.Provider,
		"commit_sha": result.CommitSHA, "pull_request_id": result.PullRequestID,
		"pull_request_url": result.PullRequestURL,
	}
}

func (s *Service) recordPublishFailure(
	ctx context.Context,
	record run.Record,
	failure run.Failure,
	cause error,
) (PhaseResult, error) {
	if record.Terminal() {
		return phaseResult(record), cause
	}
	if latest, found := latestStage(record, publishStage); found && latest.Status == run.StageRunning {
		return s.finishFailure(ctx, record.ID, failure, cause, latest.Attempts)
	}
	// A missing or changed approval cannot legally transition to publishing.
	// Record it against the approval state and fail closed.
	if record.Status != run.StatusAwaitingApproval {
		failed, err := s.failPublishBeforeTransition(ctx, record.ID, failure)
		return phaseResult(failed), errors.Join(newPhaseError(failure, cause), err)
	}
	started, didStart, err := s.beginStage(ctx, record.ID, publishStage, run.StatusPublishing)
	if err != nil {
		return phaseResult(record), errors.Join(cause, err)
	}
	if !didStart {
		return phaseInProgress(started)
	}
	return s.finishFailure(ctx, record.ID, failure, cause, stageAttempt(started, publishStage))
}

func (s *Service) failPublishBeforeTransition(
	ctx context.Context,
	runID string,
	failure run.Failure,
) (run.Record, error) {
	return s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.Terminal() {
			return current, false, nil
		}
		now := s.clock()
		next := run.CloneRecord(current)
		failureCopy := failure
		next.Failure = &failureCopy
		stage := run.StageResult{
			Name: publishStage, Status: run.StageFailed,
			StartedAt: now, FinishedAt: now,
			Attempts: stageAttempt(current, publishStage) + 1,
			Failure:  &failureCopy,
		}
		var err error
		next, err = run.UpsertStage(next, stage)
		if err != nil {
			return run.Record{}, false, err
		}
		next.UpdatedAt = now
		next, err = run.Transition(next, run.StatusFailed, now)
		return next, true, err
	})
}

func publishBindingFailure() run.Failure {
	return run.Failure{
		Stage: publishStage, Class: run.FailurePublication, Retryable: false,
		Diagnostic: "publication approval binding is invalid",
		CauseCode:  "publication_binding_mismatch",
	}
}

func safePublicationRepository(repository string) string {
	parsed, err := url.Parse(repository)
	if err != nil || parsed.Scheme == "" {
		return repository
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
