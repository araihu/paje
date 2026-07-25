package codechange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/policy"
	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/run"
	"github.com/araihu/paje/internal/runner"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workspace"
)

type executeOutcome struct {
	execution    runner.ExecutionResult
	profile      repository.ProfileResult
	verification []verification.Result
	capture      gitcapture.Result
	artifact     *artifact.Reference
	failure      *run.Failure
	cause        error
	evidence     map[string]string
}

// Execute runs one fresh-workspace attempt and persists only durable evidence.
func (s *Service) Execute(ctx context.Context, runID string) (PhaseResult, error) {
	record, input, err := s.loadExecutable(ctx, runID)
	if err != nil || record.Terminal() || record.Artifact != nil {
		return phaseResult(record), err
	}
	record, started, err := s.beginStage(ctx, runID, "execute", run.StatusExecuting)
	if err != nil || record.Terminal() || record.Artifact != nil {
		return phaseResult(record), err
	}
	if !started {
		return phaseInProgress(record)
	}

	prepared, err := s.workspaces.Prepare(ctx, record.RepositoryURI, record.BaseSHA)
	if err != nil {
		failure := classifyWorkspaceFailure(ctx, err)
		return s.finishFailure(ctx, runID, failure, err)
	}

	outcome := s.executePrepared(ctx, record, input, prepared.Path())
	cleanupErr := s.cleanupAttempt(ctx, runID, prepared)
	if cleanupErr != nil {
		outcome.cause = errors.Join(outcome.cause, cleanupErr)
		if outcome.failure == nil {
			failure := run.Failure{
				Stage: "execute", Class: run.FailureCleanup, Retryable: false,
				Diagnostic: "attempt cleanup failed", CauseCode: "cleanup_failed",
			}
			outcome.failure = &failure
		} else {
			failure := *outcome.failure
			failure.Diagnostic = run.SafeDiagnostic(failure.Diagnostic + "; attempt cleanup failed")
			outcome.failure = &failure
		}
	}
	return s.finishExecute(ctx, runID, outcome)
}

func (s *Service) loadExecutable(ctx context.Context, runID string) (run.Record, templatecodechange.Input, error) {
	record, err := s.runs.Load(ctx, runID)
	if err != nil {
		return run.Record{}, templatecodechange.Input{}, err
	}
	if record.Terminal() || record.Artifact != nil {
		return record, templatecodechange.Input{}, nil
	}
	if record.Status != run.StatusExecuting || strings.TrimSpace(record.BaseSHA) == "" {
		return record, templatecodechange.Input{}, fmt.Errorf("execute code change: run is not resolved")
	}
	input, err := templatecodechange.Decode(record.Input)
	if err != nil {
		return record, templatecodechange.Input{}, fmt.Errorf("execute code change: decode persisted input: %w", err)
	}
	return record, input, nil
}

func (s *Service) executePrepared(
	ctx context.Context,
	record run.Record,
	input templatecodechange.Input,
	workspacePath string,
) executeOutcome {
	outcome := executeOutcome{evidence: make(map[string]string)}
	agentEnvironment, err := s.environments.Build(ctx, environment.Request{
		RunID: record.ID, Stage: environment.StageAgent, RequestedKeys: input.EnvironmentKeys,
	})
	if err != nil {
		return outcome.withFailure(environmentPolicyFailure(ctx), err)
	}
	verificationEnvironment, err := s.environments.Build(ctx, environment.Request{
		RunID: record.ID, Stage: environment.StageVerification, RequestedKeys: input.EnvironmentKeys,
	})
	if err != nil {
		return outcome.withFailure(environmentPolicyFailure(ctx), err)
	}
	outcome.evidence["agent_environment_keys"] = encodeStrings(agentEnvironment.Keys)
	outcome.evidence["verification_environment_keys"] = encodeStrings(verificationEnvironment.Keys)

	profile := s.profiles[input.Profile]
	outcome.profile, err = profile.Inspect(ctx, repository.ProfileRequest{
		Workspace: workspacePath, Environment: cloneStringMap(verificationEnvironment.Values),
		Checks:           append([]verification.CommandSpec(nil), input.Checks...),
		ModuleExclusions: append([]repository.ModuleExclusion(nil), input.ModuleExclusions...),
	})
	if err != nil {
		return outcome.withFailure(classifyProfileFailure(ctx, err), err)
	}
	outcome.evidence["preflight_fact_count"] = strconv.Itoa(len(outcome.profile.Facts))
	outcome.evidence["verification_command_count"] = strconv.Itoa(len(outcome.profile.Commands))

	prompt, err := buildPrompt(promptInput{
		Task: input.TaskDescription, BaseSHA: record.BaseSHA, Profile: input.Profile,
		Facts: outcome.profile.Facts, Memory: record.MemorySnapshot,
	}, maxAgentPromptBytes)
	if err != nil {
		failure := run.Failure{
			Stage: "execute", Class: run.FailureInput, Retryable: false,
			Diagnostic: "agent prompt exceeds configured limit", CauseCode: "prompt_too_large",
		}
		return outcome.withFailure(failure, err)
	}

	outcome.execution, err = s.agent.Run(ctx, runner.RunRequest{
		TaskDescription: prompt, WorkspacePath: workspacePath,
		Env: cloneStringMap(agentEnvironment.Values),
	})
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return outcome.withFailure(canceledFailure("execute"), errors.Join(err, ctx.Err()))
	}
	if err != nil {
		retryable := !outcome.execution.Started && !outcome.execution.Completed
		failure := run.Failure{
			Stage: "execute", Class: run.FailureAgent, Retryable: retryable,
			Diagnostic: "agent execution was unavailable", CauseCode: "agent_unavailable",
		}
		if !outcome.execution.Started {
			return outcome.withFailure(failure, err)
		}
		outcome = outcome.withFailure(failure, err)
	} else if outcome.execution.ExitCode != 0 {
		failure := run.Failure{
			Stage: "execute", Class: run.FailureAgent, Retryable: false,
			Diagnostic: fmt.Sprintf("agent exited with code %d", outcome.execution.ExitCode),
			CauseCode:  "nonzero_exit",
		}
		outcome = outcome.withFailure(failure, errors.New(failure.Diagnostic))
	} else if !outcome.execution.Completed {
		failure := run.Failure{
			Stage: "execute", Class: run.FailureAgent,
			Retryable: !outcome.execution.Started, Diagnostic: "agent response was incomplete",
			CauseCode: "incomplete_response",
		}
		if !outcome.execution.Started {
			return outcome.withFailure(failure, errors.New(failure.Diagnostic))
		}
		outcome = outcome.withFailure(failure, errors.New(failure.Diagnostic))
	}

	if outcome.failure == nil {
		outcome = s.runVerification(ctx, outcome, verificationEnvironment.Values)
		if outcome.failure != nil && outcome.failure.Class == run.FailureCanceled {
			return outcome
		}
	}

	outcome.capture, err = s.capturer.Capture(ctx, gitcapture.Request{
		Workspace: workspacePath, BaseSHA: record.BaseSHA, MaxBytes: maxCaptureBytes,
	})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return outcome.withFailure(canceledFailure("execute"), errors.Join(err, ctx.Err()))
		}
		failure := run.Failure{
			Stage: "execute", Class: run.FailureInternal,
			Retryable:  !outcome.execution.Completed,
			Diagnostic: "capture change set failed", CauseCode: "capture_failed",
		}
		return outcome.withFailure(failure, err)
	}

	decision := s.policy.Evaluate(ctx, outcome.capture)
	if ctx.Err() != nil {
		return outcome.withFailure(canceledFailure("execute"), ctx.Err())
	}
	if !decision.Allowed {
		outcome.evidence["policy_findings"] = encodeFindings(decision.Findings)
		failure := run.Failure{
			Stage: "execute", Class: run.FailurePolicy, Retryable: false,
			Diagnostic: "captured changes were denied by policy", CauseCode: "change_policy_denied",
		}
		return outcome.withFailure(failure, errors.New(failure.Diagnostic))
	}

	bundle, err := buildBundle(record, outcome)
	if err != nil {
		failure := run.Failure{
			Stage: "execute", Class: run.FailureInternal,
			Retryable:  !outcome.execution.Completed,
			Diagnostic: "build artifact evidence failed", CauseCode: "artifact_encoding",
		}
		return outcome.withFailure(failure, err)
	}
	reference, err := s.artifacts.Save(ctx, bundle)
	if err != nil {
		failure := run.Failure{
			Stage: "execute", Class: run.FailureInternal,
			Retryable:  !outcome.execution.Completed,
			Diagnostic: "persist artifact failed", CauseCode: "artifact_store",
		}
		return outcome.withFailure(failure, err)
	}
	outcome.artifact = &reference
	outcome.evidence["artifact_digest"] = reference.Digest
	outcome.evidence["artifact_size"] = strconv.FormatInt(reference.Size, 10)
	outcome.evidence["tree_sha"] = outcome.capture.TreeSHA
	return outcome
}

func (s *Service) runVerification(
	ctx context.Context,
	outcome executeOutcome,
	environmentValues map[string]string,
) executeOutcome {
	for _, command := range outcome.profile.Commands {
		result := s.verifier.Run(ctx, command, cloneStringMap(environmentValues))
		outcome.verification = append(outcome.verification, result)
		if !command.Required || result.Passed || outcome.failure != nil {
			continue
		}
		switch result.FailureClass {
		case "canceled":
			failure := canceledFailure("execute")
			outcome = outcome.withFailure(failure, context.Canceled)
		case "environment":
			failure := run.Failure{
				Stage: "execute", Class: run.FailureEnvironment,
				Retryable:  !outcome.execution.Completed && transientEnvironmentCause(result.CauseCode),
				Diagnostic: "required verification environment is unavailable",
				CauseCode:  safeCauseCode(result.CauseCode, "verification_environment"),
			}
			outcome = outcome.withFailure(failure, errors.New(failure.Diagnostic))
		default:
			failure := run.Failure{
				Stage: "execute", Class: run.FailureVerification, Retryable: false,
				Diagnostic: "required verification command failed",
				CauseCode:  safeCauseCode(result.CauseCode, "verification_failed"),
			}
			outcome = outcome.withFailure(failure, errors.New(failure.Diagnostic))
		}
	}
	return outcome
}

func buildBundle(record run.Record, outcome executeOutcome) (artifact.Bundle, error) {
	executionMetadata, err := json.Marshal(artifact.ExecutionEvidenceFrom(outcome.execution))
	if err != nil {
		return artifact.Bundle{}, err
	}
	memoryIDs := make([]string, 0, len(record.MemorySnapshot))
	for _, item := range record.MemorySnapshot {
		memoryIDs = append(memoryIDs, item.ID)
	}
	sort.Strings(memoryIDs)
	return artifact.Bundle{
		Manifest: artifact.Manifest{
			RunID: record.ID, Template: record.Template,
			Repository: record.RepositoryURI, BaseSHA: record.BaseSHA,
			TreeSHA: outcome.capture.TreeSHA, Changes: append([]artifact.Change(nil), outcome.capture.Changes...),
			MemoryIDs: memoryIDs, MemoryCount: len(memoryIDs),
		},
		ChangesPatch:      append([]byte(nil), outcome.capture.Patch...),
		AgentOutput:       []byte(outcome.execution.Output),
		ExecutionMetadata: executionMetadata,
		Verification:      append([]verification.Result(nil), outcome.verification...),
		Preflight:         cloneStringMap(outcome.profile.Facts),
		Warnings:          append([]string(nil), outcome.profile.Warnings...),
	}, nil
}

func (s *Service) cleanupAttempt(ctx context.Context, runID string, prepared workspace.Workspace) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	runtimeErr := s.environments.Cleanup(cleanupCtx, runID)
	workspaceErr := prepared.Cleanup(cleanupCtx)
	return errors.Join(runtimeErr, workspaceErr)
}

func (s *Service) finishExecute(ctx context.Context, runID string, outcome executeOutcome) (PhaseResult, error) {
	persistCtx := ctx
	cancel := func() {}
	if ctx.Err() != nil {
		persistCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	}
	defer cancel()

	record, err := s.mutate(persistCtx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.Terminal() || current.Artifact != nil {
			return current, false, nil
		}
		next := run.CloneRecord(current)
		if outcome.artifact != nil {
			reference := *outcome.artifact
			next.Artifact = &reference
		}
		stage := run.StageResult{
			Name: "execute", StartedAt: latestStageStart(current, "execute"),
			FinishedAt: s.clock(), Attempts: stageAttempt(current, "execute"),
			Evidence: cloneStringMap(outcome.evidence),
		}
		if outcome.failure == nil {
			stage.Status = run.StageSucceeded
			next.Failure = nil
		} else {
			stage.Status = run.StageFailed
			failure := *outcome.failure
			stage.Failure = &failure
			next.Failure = &failure
		}
		var mutationErr error
		next, mutationErr = run.UpsertStage(next, stage)
		if mutationErr != nil {
			return run.Record{}, false, mutationErr
		}
		next.UpdatedAt = s.clock()
		if outcome.failure != nil && !outcome.failure.Retryable {
			status := run.StatusFailed
			if outcome.failure.Class == run.FailureCanceled {
				status = run.StatusCanceled
			}
			next, mutationErr = run.Transition(next, status, s.clock())
			if mutationErr != nil {
				return run.Record{}, false, mutationErr
			}
		}
		return next, true, nil
	})
	if err != nil {
		if outcome.failure == nil {
			return phaseResult(record), err
		}
		return phaseResult(record), errors.Join(newPhaseError(*outcome.failure, outcome.cause), err)
	}
	if outcome.failure != nil {
		return phaseResult(record), newPhaseError(*outcome.failure, outcome.cause)
	}
	return phaseResult(record), nil
}

func (outcome executeOutcome) withFailure(failure run.Failure, cause error) executeOutcome {
	failure.Diagnostic = run.SafeDiagnostic(failure.Diagnostic)
	outcome.failure = &failure
	outcome.cause = errors.Join(outcome.cause, cause)
	return outcome
}

func classifyWorkspaceFailure(ctx context.Context, err error) run.Failure {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return canceledFailure("execute")
	}
	return run.Failure{
		Stage: "execute", Class: run.FailureEnvironment, Retryable: true,
		Diagnostic: "prepare isolated workspace failed", CauseCode: "workspace_unavailable",
	}
}

func environmentPolicyFailure(ctx context.Context) run.Failure {
	if ctx.Err() != nil {
		return canceledFailure("execute")
	}
	return run.Failure{
		Stage: "execute", Class: run.FailurePolicy, Retryable: false,
		Diagnostic: "requested execution environment was denied", CauseCode: "environment_policy_denied",
	}
}

func classifyProfileFailure(ctx context.Context, err error) run.Failure {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return canceledFailure("execute")
	}
	var environmentError *repository.EnvironmentError
	if errors.As(err, &environmentError) {
		return run.Failure{
			Stage: "execute", Class: run.FailureEnvironment, Retryable: false,
			Diagnostic: "required repository tool is unavailable", CauseCode: "tool_unavailable",
		}
	}
	return run.Failure{
		Stage: "execute", Class: run.FailureEnvironment, Retryable: false,
		Diagnostic: "repository preflight failed", CauseCode: "preflight_failed",
	}
}

func transientEnvironmentCause(code string) bool {
	return code == "timeout" || code == "docker_unavailable" || code == "temporary_unavailable"
}

func safeCauseCode(code, fallback string) string {
	code = strings.TrimSpace(code)
	if code == "" || strings.ContainsAny(code, " \t\r\n\x00") {
		return fallback
	}
	return code
}

func encodeStrings(values []string) string {
	encoded, _ := json.Marshal(append([]string(nil), values...))
	return string(encoded)
}

func encodeFindings(findings []policy.Finding) string {
	encoded, _ := json.Marshal(findings)
	return string(encoded)
}

// Exhaust turns the named stage's latest retryable failure into durable,
// terminal retry exhaustion evidence.
func (s *Service) Exhaust(ctx context.Context, runID, stage string) (PhaseResult, error) {
	record, err := s.runs.Load(ctx, runID)
	if err != nil || record.Terminal() {
		return phaseResult(record), err
	}
	record, err = s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.Terminal() {
			return current, false, nil
		}
		latest, found := latestStage(current, stage)
		if !found || latest.Failure == nil || !latest.Failure.Retryable ||
			current.Failure == nil || current.Failure.Stage != stage {
			return run.Record{}, false, fmt.Errorf("exhaust stage %q: no retryable failure", stage)
		}
		next := run.CloneRecord(current)
		failure := *latest.Failure
		failure.Retryable = false
		failure.CauseCode = "retries_exhausted"
		latest.Failure = &failure
		next.Failure = &failure
		var mutationErr error
		next, mutationErr = run.UpsertStage(next, latest)
		if mutationErr != nil {
			return run.Record{}, false, mutationErr
		}
		next, mutationErr = run.Transition(next, run.StatusFailed, s.clock())
		return next, true, mutationErr
	})
	return phaseResult(record), err
}

func latestStage(record run.Record, name string) (run.StageResult, bool) {
	var latest run.StageResult
	found := false
	for _, stage := range record.Stages {
		if stage.Name == name && (!found || stage.Attempts > latest.Attempts) {
			latest = stage
			found = true
		}
	}
	return latest, found
}
