package codechange

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/araihu/paje/internal/approval"
	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/run"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
)

const approvalStage = "approval"

// Approval records the artifact-bound human decision for a pull-request run.
// Artifact-only runs durably skip the stage without calling gate.
func (s *Service) Approval(
	ctx context.Context,
	runID string,
	gate approval.Gate,
) (PhaseResult, error) {
	record, input, bundle, err := s.loadApprovalState(ctx, runID)
	if err != nil || record.Terminal() {
		return phaseResult(record), err
	}
	if input.Publication.Mode == "artifact" {
		return s.skipStage(ctx, record, approvalStage)
	}

	request := buildApprovalRequest(record, input, bundle)
	if err := request.Validate(); err != nil {
		return s.recordApprovalFailure(ctx, record, approvalBindingFailure(), err)
	}
	if record.Approval != nil {
		if err := validatePersistedApproval(record, request); err != nil {
			return s.recordApprovalFailure(ctx, record, approvalBindingFailure(), err)
		}
		return phaseResult(record), nil
	}

	record, started, err := s.beginStage(ctx, runID, approvalStage, run.StatusAwaitingApproval)
	if err != nil {
		return phaseResult(record), err
	}
	if record.Terminal() {
		return phaseResult(record), nil
	}
	if !started {
		if record.Approval != nil {
			if err := validatePersistedApproval(record, request); err != nil {
				return s.recordApprovalFailure(ctx, record, approvalBindingFailure(), err)
			}
			return phaseResult(record), nil
		}
		return phaseInProgress(record)
	}
	ownership, err := ownershipFor(record, approvalStage)
	if err != nil {
		return phaseResult(record), err
	}
	if isNil(gate) {
		failure := run.Failure{
			Stage: approvalStage, Class: run.FailureApproval, Retryable: false,
			Diagnostic: "approval provider is unavailable", CauseCode: "approval_provider_unavailable",
		}
		return s.finishFailure(ctx, runID, failure, errors.New("approval gate is required"), ownership.attempt)
	}

	decision, gateErr := gate.RequestApproval(ctx, request)
	if ctx.Err() != nil {
		return s.finishFailure(ctx, runID, canceledFailure(approvalStage), ctx.Err(), ownership.attempt)
	}
	if gateErr != nil {
		failure := run.Failure{
			Stage: approvalStage, Class: run.FailureApproval,
			Retryable:  typedRetryable(gateErr),
			Diagnostic: "approval provider request failed", CauseCode: "approval_provider_failed",
		}
		return s.finishFailure(ctx, runID, failure, gateErr, ownership.attempt)
	}
	if err := decision.Validate(request); err != nil {
		return s.finishFailure(ctx, runID, approvalBindingFailure(), err, ownership.attempt)
	}
	result, persistErr := s.persistApproval(ctx, runID, ownership, request, decision)
	if persistErr != nil && ctx.Err() != nil {
		return s.compensateOwnedCancellation(ctx, runID, ownership, persistErr)
	}
	return result, persistErr
}

func (s *Service) compensateOwnedCancellation(
	ctx context.Context,
	runID string,
	ownership stageOwnership,
	cause error,
) (PhaseResult, error) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	failure := canceledFailure(ownership.name)
	record, err := s.mutate(persistCtx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.Terminal() || !exactStageIdentity(current, ownership) {
			return current, false, nil
		}
		next := run.CloneRecord(current)
		failureCopy := failure
		next.Failure = &failureCopy
		stage := run.StageResult{
			Name: ownership.name, Status: run.StageFailed,
			StartedAt: ownership.startedAt, FinishedAt: s.clock(),
			Attempts: ownership.attempt, Failure: &failureCopy,
		}
		var mutationErr error
		next, mutationErr = run.UpsertStage(next, stage)
		if mutationErr != nil {
			return run.Record{}, false, mutationErr
		}
		next.UpdatedAt = s.clock()
		next, mutationErr = run.Transition(next, run.StatusCanceled, s.clock())
		return next, true, mutationErr
	})
	return phaseResult(record), errors.Join(newPhaseError(failure, ctx.Err()), cause, err)
}

func (s *Service) loadApprovalState(
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
		return record, input, artifact.Bundle{}, fmt.Errorf("%w: approval artifact is missing", ErrRunBinding)
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

func validateArtifactBinding(record run.Record, bundle artifact.Bundle) error {
	reference, err := artifact.ReferenceFor(bundle)
	if err != nil {
		return fmt.Errorf("%w: canonical artifact", ErrRunBinding)
	}
	if record.Artifact == nil || reference != *record.Artifact ||
		bundle.Manifest.RunID != record.ID ||
		bundle.Manifest.Template != record.Template ||
		bundle.Manifest.Repository != record.RepositoryURI ||
		bundle.Manifest.BaseSHA != record.BaseSHA {
		return fmt.Errorf("%w: artifact manifest", ErrRunBinding)
	}
	return nil
}

func buildApprovalRequest(
	record run.Record,
	input templatecodechange.Input,
	bundle artifact.Bundle,
) approval.Request {
	changed := make([]string, 0, len(bundle.Manifest.Changes)*2)
	for _, change := range bundle.Manifest.Changes {
		if change.OldPath != "" {
			changed = append(changed, change.OldPath)
		}
		if change.Path != "" {
			changed = append(changed, change.Path)
		}
	}
	sort.Strings(changed)
	changed = compactStrings(changed)

	checks := cloneVerification(bundle.Verification)
	for index := range checks {
		// Approval needs pass/fail evidence, never arbitrary command output.
		checks[index].Output = ""
		checks[index].Command.Environment = nil
	}
	return approval.Request{
		RunID: record.ID, TemplateID: record.Template.String(),
		Repository: record.RepositoryURI,
		BaseSHA:    record.BaseSHA, TargetBranch: input.Publication.TargetBranch,
		PublicationMode:   input.Publication.Mode,
		PublicationBranch: publicationBranch(record.ID),
		ArtifactDigest:    record.Artifact.Digest,
		Description:       "Review Pajé code change " + record.ID,
		AgentSummary: fmt.Sprintf(
			"Agent produced %d changed path(s) with %d verification check(s).",
			len(changed), len(checks),
		),
		ChangedPaths: changed, Verification: checks,
		Warnings: append([]string(nil), bundle.Warnings...),
	}
}

func (s *Service) persistApproval(
	ctx context.Context,
	runID string,
	ownership stageOwnership,
	request approval.Request,
	decision approval.Result,
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
		if current.Artifact == nil {
			return run.Record{}, false, fmt.Errorf("%w: approval artifact disappeared", ErrRunBinding)
		}
		bound := approvalBindingEvidence(current, input)
		if !sameApprovalRequestBinding(request, bound) {
			return run.Record{}, false, fmt.Errorf("%w: approval request changed", ErrRunBinding)
		}
		if current.Approval != nil {
			if *current.Approval != decision {
				return run.Record{}, false, fmt.Errorf("%w: approval decision conflict", ErrRunBinding)
			}
			return current, false, nil
		}

		next := run.CloneRecord(current)
		decisionCopy := decision
		next.Approval = &decisionCopy
		stage := run.StageResult{
			Name: approvalStage, Status: run.StageSucceeded,
			StartedAt: ownership.startedAt, FinishedAt: s.clock(),
			Attempts: ownership.attempt, Evidence: approvalEvidence(request),
		}
		next, err = run.UpsertStage(next, stage)
		if err != nil {
			return run.Record{}, false, err
		}
		next.UpdatedAt = s.clock()
		if !decision.Approved {
			next, err = run.Transition(next, run.StatusDeclined, s.clock())
			if err != nil {
				return run.Record{}, false, err
			}
		}
		return next, true, nil
	})
	if errors.Is(err, ErrPhaseInProgress) {
		return phaseInProgress(record)
	}
	return phaseResult(record), err
}

func (s *Service) recordApprovalFailure(
	ctx context.Context,
	record run.Record,
	failure run.Failure,
	cause error,
) (PhaseResult, error) {
	if record.Terminal() {
		return phaseResult(record), cause
	}
	if latest, found := latestStage(record, approvalStage); found && latest.Status == run.StageRunning {
		return s.finishFailure(ctx, record.ID, failure, cause, latest.Attempts)
	}
	started, didStart, err := s.beginStage(ctx, record.ID, approvalStage, run.StatusAwaitingApproval)
	if err != nil {
		return phaseResult(record), errors.Join(cause, err)
	}
	if !didStart {
		return phaseInProgress(started)
	}
	return s.finishFailure(ctx, record.ID, failure, cause, stageAttempt(started, approvalStage))
}

func validatePersistedApproval(record run.Record, request approval.Request) error {
	if record.Approval == nil {
		return fmt.Errorf("approval decision is missing")
	}
	if err := record.Approval.Validate(request); err != nil {
		return err
	}
	stage, found := latestStage(record, approvalStage)
	if !found || stage.Status != run.StageSucceeded {
		return fmt.Errorf("approval stage evidence is missing")
	}
	if !sameStringMap(stage.Evidence, approvalEvidence(request)) {
		return fmt.Errorf("approval request binding changed")
	}
	return nil
}

func approvalBindingEvidence(
	record run.Record,
	input templatecodechange.Input,
) approval.Request {
	return approval.Request{
		RunID: record.ID, TemplateID: record.Template.String(),
		Repository: record.RepositoryURI,
		BaseSHA:    record.BaseSHA, TargetBranch: input.Publication.TargetBranch,
		PublicationMode:   input.Publication.Mode,
		PublicationBranch: publicationBranch(record.ID),
		ArtifactDigest:    record.Artifact.Digest,
	}
}

func sameApprovalRequestBinding(left, right approval.Request) bool {
	return left.RunID == right.RunID &&
		left.TemplateID == right.TemplateID &&
		left.Repository == right.Repository &&
		left.BaseSHA == right.BaseSHA &&
		left.TargetBranch == right.TargetBranch &&
		left.PublicationMode == right.PublicationMode &&
		left.PublicationBranch == right.PublicationBranch &&
		left.ArtifactDigest == right.ArtifactDigest
}

func approvalEvidence(request approval.Request) map[string]string {
	return map[string]string{
		"run_id": request.RunID, "template_id": request.TemplateID,
		"repository": request.Repository, "base_sha": request.BaseSHA,
		"target_branch":      request.TargetBranch,
		"publication_mode":   request.PublicationMode,
		"publication_branch": request.PublicationBranch,
		"artifact_digest":    request.ArtifactDigest,
	}
}

func approvalBindingFailure() run.Failure {
	return run.Failure{
		Stage: approvalStage, Class: run.FailureApproval, Retryable: false,
		Diagnostic: "approval binding is invalid", CauseCode: "approval_binding_mismatch",
	}
}

func typedRetryable(err error) bool {
	var marker interface{ Retryable() bool }
	return errors.As(err, &marker) && marker.Retryable()
}

func publicationBranch(runID string) string {
	return "paje/code-change/" + runID
}

func canonicalGitHubRepository(repository string) (string, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" || strings.ContainsAny(repository, "\x00\r\n") {
		return "", errors.New("GitHub repository is invalid")
	}
	parsed, err := url.Parse(repository)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		!strings.EqualFold(parsed.Hostname(), "github.com") || parsed.Port() != "" ||
		parsed.RawPath != "" {
		return "", errors.New("GitHub repository must be a credential-free HTTPS URL")
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 2 {
		return "", errors.New("GitHub repository path must contain owner and repository")
	}
	owner := parts[0]
	name := strings.TrimSuffix(parts[1], ".git")
	if !safeRepositoryComponent(owner) || !safeRepositoryComponent(name) {
		return "", errors.New("GitHub repository owner or name is invalid")
	}
	return "https://github.com/" + owner + "/" + name + ".git", nil
}

func safeRepositoryComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if !('a' <= character && character <= 'z') &&
			!('A' <= character && character <= 'Z') &&
			!('0' <= character && character <= '9') &&
			character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	write := 1
	for read := 1; read < len(values); read++ {
		if values[read] == values[write-1] {
			continue
		}
		values[write] = values[read]
		write++
	}
	return values[:write]
}

func cloneVerification(source []verification.Result) []verification.Result {
	if source == nil {
		return nil
	}
	result := make([]verification.Result, len(source))
	for index, item := range source {
		result[index] = item
		result[index].Command.Args = append([]string(nil), item.Command.Args...)
		result[index].Command.Environment = cloneStringMap(item.Command.Environment)
	}
	return result
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func (s *Service) skipStage(
	ctx context.Context,
	record run.Record,
	name string,
) (PhaseResult, error) {
	if record.ID == "" {
		return phaseResult(record), run.ErrNotFound
	}
	updated, err := s.mutate(ctx, record.ID, func(current run.Record) (run.Record, bool, error) {
		if _, err := validateRunBinding(current); err != nil {
			return run.Record{}, false, err
		}
		if current.Terminal() {
			return current, false, nil
		}
		if latest, found := latestStage(current, name); found {
			if latest.Status == run.StageSkipped {
				return current, false, nil
			}
			if latest.Status == run.StageRunning {
				return current, false, ErrPhaseInProgress
			}
		}
		now := s.clock()
		next, err := run.UpsertStage(current, run.StageResult{
			Name: name, Status: run.StageSkipped, StartedAt: now, FinishedAt: now,
			Attempts: stageAttempt(current, name) + 1,
			Evidence: map[string]string{"publication_mode": "artifact"},
		})
		if err != nil {
			return run.Record{}, false, err
		}
		next.UpdatedAt = now
		return next, true, nil
	})
	if errors.Is(err, ErrPhaseInProgress) {
		return phaseInProgress(updated)
	}
	return phaseResult(updated), err
}
