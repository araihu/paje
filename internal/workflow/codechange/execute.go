package codechange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
	scrubber     durableScrubber
}

// Execute runs one fresh-workspace attempt and persists only durable evidence.
func (s *Service) Execute(ctx context.Context, runID string) (PhaseResult, error) {
	record, input, err := s.loadExecutable(ctx, runID)
	if err != nil || record.Terminal() {
		return phaseResult(record), err
	}
	record, started, err := s.beginStage(ctx, runID, "execute", run.StatusExecuting)
	if err != nil {
		return phaseResult(record), err
	}
	if record.Terminal() {
		if record.Failure != nil && record.Failure.CauseCode == "ambiguous_attempt" {
			return phaseResult(record), newPhaseError(*record.Failure, errors.New(record.Failure.Diagnostic))
		}
		return phaseResult(record), nil
	}
	if !started {
		if latest, found := latestStage(record, "execute"); found && latest.Status != run.StageRunning &&
			record.Artifact != nil {
			return phaseResult(record), nil
		}
		return phaseInProgress(record)
	}

	prepared, err := s.workspaces.Prepare(ctx, record.RepositoryURI, record.BaseSHA)
	if err != nil {
		failure := classifyWorkspaceFailure(ctx, err)
		return s.finishFailure(ctx, runID, failure, err)
	}

	outcome := s.executePrepared(ctx, record, input, prepared.Path())
	outcome = outcome.withCancellation(ctx.Err())
	if outcome.artifact != nil {
		if checkpointErr := s.checkpointArtifact(ctx, runID, *outcome.artifact); checkpointErr != nil {
			failure := run.Failure{
				Stage: "execute", Class: run.FailureInternal, Retryable: false,
				Diagnostic: "checkpoint artifact reference failed", CauseCode: "artifact_checkpoint",
			}
			outcome = outcome.withFailure(failure, checkpointErr)
		}
	}
	outcome = outcome.withCancellation(ctx.Err())
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
			failure.Retryable = false
			failure.Diagnostic = run.SafeDiagnostic(failure.Diagnostic + "; attempt cleanup failed")
			outcome.failure = &failure
		}
	}
	outcome = outcome.withCancellation(ctx.Err())
	return s.finishExecute(ctx, runID, outcome)
}

func (s *Service) loadExecutable(ctx context.Context, runID string) (run.Record, templatecodechange.Input, error) {
	record, err := s.runs.Load(ctx, runID)
	if err != nil {
		return run.Record{}, templatecodechange.Input{}, err
	}
	input, err := validateRunBinding(record)
	if err != nil {
		return record, templatecodechange.Input{}, err
	}
	if record.Terminal() {
		return record, input, nil
	}
	if record.Status != run.StatusExecuting || strings.TrimSpace(record.BaseSHA) == "" {
		return record, templatecodechange.Input{}, fmt.Errorf("execute code change: run is not resolved")
	}
	return record, input, nil
}

func (s *Service) executePrepared(
	ctx context.Context,
	record run.Record,
	input templatecodechange.Input,
	workspacePath string,
) executeOutcome {
	outcome := executeOutcome{
		evidence: make(map[string]string),
		scrubber: newDurableScrubber(workspacePath),
	}
	agentEnvironment, err := s.environments.Build(ctx, environment.Request{
		RunID: record.ID, Stage: environment.StageAgent, RequestedKeys: input.EnvironmentKeys,
	})
	if err != nil {
		return outcome.withFailure(environmentPolicyFailure(ctx, "execute"), err)
	}
	verificationEnvironment, err := s.environments.Build(ctx, environment.Request{
		RunID: record.ID, Stage: environment.StageVerification, RequestedKeys: input.EnvironmentKeys,
	})
	if err != nil {
		return outcome.withFailure(environmentPolicyFailure(ctx, "execute"), err)
	}
	outcome.scrubber = newDurableScrubber(
		workspacePath, agentEnvironment.Values, verificationEnvironment.Values,
	)
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
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return outcome.withFailure(canceledFailure("execute"), errors.Join(err, ctx.Err()))
		}
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
	outcome = outcome.withCancellation(ctx.Err())
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
		case "internal":
			failure := run.Failure{
				Stage: "execute", Class: run.FailureInternal, Retryable: false,
				Diagnostic: "verification executor failed internally",
				CauseCode:  safeCauseCode(result.CauseCode, "verification_internal"),
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
	verificationEvidence := make([]verification.Result, len(outcome.verification))
	for index, result := range outcome.verification {
		durable, err := outcome.scrubber.verificationResult(result)
		if err != nil {
			return artifact.Bundle{}, err
		}
		verificationEvidence[index] = durable
	}
	changes := append([]artifact.Change(nil), outcome.capture.Changes...)
	for index := range changes {
		changes[index].Path = outcome.scrubber.string(changes[index].Path)
		changes[index].OldPath = outcome.scrubber.string(changes[index].OldPath)
	}
	return artifact.Bundle{
		Manifest: artifact.Manifest{
			RunID: record.ID, Template: record.Template,
			Repository: record.RepositoryURI, BaseSHA: record.BaseSHA,
			TreeSHA: outcome.scrubber.string(outcome.capture.TreeSHA), Changes: changes,
			MemoryIDs: memoryIDs, MemoryCount: len(memoryIDs),
		},
		ChangesPatch:      append([]byte(nil), outcome.capture.Patch...),
		AgentOutput:       []byte(outcome.scrubber.string(outcome.execution.Output)),
		ExecutionMetadata: executionMetadata,
		Verification:      verificationEvidence,
		Preflight:         outcome.scrubber.stringMap(outcome.profile.Facts),
		Warnings:          outcome.scrubber.strings(outcome.profile.Warnings),
	}, nil
}

type durableScrubber struct {
	workspace string
	prefixes  []string
}

func newDurableScrubber(workspacePath string, environments ...map[string]string) durableScrubber {
	scrubber := durableScrubber{workspace: filepath.Clean(workspacePath)}
	seen := make(map[string]struct{})
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || value == "." {
			return
		}
		value = filepath.Clean(value)
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		scrubber.prefixes = append(scrubber.prefixes, value)
	}
	add(workspacePath)
	runtimePaths := make([]string, 0, len(environments)*4)
	for _, values := range environments {
		for _, key := range []string{"HOME", "TMPDIR", "TMP", "TEMP"} {
			value := values[key]
			add(value)
			if strings.TrimSpace(value) != "" {
				runtimePaths = append(runtimePaths, value)
			}
		}
		add(values["CODEX_HOME"])
	}
	add(commonPathAncestor(runtimePaths))
	sort.Slice(scrubber.prefixes, func(left, right int) bool {
		return len(scrubber.prefixes[left]) > len(scrubber.prefixes[right])
	})
	return scrubber
}

func commonPathAncestor(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	common := filepath.Clean(paths[0])
	for _, candidate := range paths[1:] {
		candidate = filepath.Clean(candidate)
		for !pathWithin(common, candidate) {
			parent := filepath.Dir(common)
			if parent == common {
				return ""
			}
			common = parent
		}
	}
	return common
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (s durableScrubber) string(value string) string {
	for _, prefix := range s.prefixes {
		replacement := "<runtime>"
		if prefix == s.workspace {
			replacement = "."
		}
		value = strings.ReplaceAll(value, prefix, replacement)
	}
	return value
}

func (s durableScrubber) strings(values []string) []string {
	scrubbed := make([]string, len(values))
	for index, value := range values {
		scrubbed[index] = s.string(value)
	}
	return scrubbed
}

func (s durableScrubber) stringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	scrubbed := make(map[string]string, len(values))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		durableKey := s.string(key)
		candidate := durableKey
		for suffix := 2; ; suffix++ {
			if _, exists := scrubbed[candidate]; !exists {
				break
			}
			candidate = fmt.Sprintf("%s_%d", durableKey, suffix)
		}
		scrubbed[candidate] = s.string(values[key])
	}
	return scrubbed
}

func (s durableScrubber) verificationResult(result verification.Result) (verification.Result, error) {
	directory, err := durableDirectory(s.workspace, result.Command.Directory)
	if err != nil {
		return verification.Result{}, err
	}
	result.Command.Name = s.string(result.Command.Name)
	result.Command.Directory = directory
	result.Command.Executable = s.string(result.Command.Executable)
	result.Command.Args = s.strings(result.Command.Args)
	result.Command.Environment = s.stringMap(result.Command.Environment)
	result.Output = s.string(result.Output)
	result.FailureClass = s.string(result.FailureClass)
	result.CauseCode = s.string(result.CauseCode)
	return result, nil
}

func durableDirectory(workspacePath, directory string) (string, error) {
	if strings.TrimSpace(workspacePath) == "" || strings.TrimSpace(directory) == "" {
		return "", fmt.Errorf("durable verification directory is missing")
	}
	workspacePath = filepath.Clean(workspacePath)
	if !filepath.IsAbs(directory) {
		return "", fmt.Errorf("durable verification directory is not compiled")
	}
	relative, err := filepath.Rel(workspacePath, filepath.Clean(directory))
	if err != nil {
		return "", fmt.Errorf("make verification directory durable: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("make verification directory durable: directory escapes workspace")
	}
	return filepath.ToSlash(relative), nil
}

func (s *Service) cleanupAttempt(ctx context.Context, runID string, prepared workspace.Workspace) error {
	runtimeCtx, cancelRuntime := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	runtimeErr := s.environments.Cleanup(runtimeCtx, runID)
	cancelRuntime()
	workspaceCtx, cancelWorkspace := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	workspaceErr := prepared.Cleanup(workspaceCtx)
	cancelWorkspace()
	return errors.Join(runtimeErr, workspaceErr)
}

func (s *Service) checkpointArtifact(ctx context.Context, runID string, reference artifact.Reference) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	_, err := s.mutate(persistCtx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.Artifact != nil {
			if *current.Artifact != reference {
				return run.Record{}, false, fmt.Errorf("checkpoint artifact: durable reference differs")
			}
			return current, false, nil
		}
		if current.Terminal() {
			return current, false, fmt.Errorf("checkpoint artifact: run is already terminal")
		}
		next := run.CloneRecord(current)
		checkpoint := reference
		next.Artifact = &checkpoint
		next.UpdatedAt = s.clock()
		return next, true, nil
	})
	return err
}

func (s *Service) finishExecute(ctx context.Context, runID string, outcome executeOutcome) (PhaseResult, error) {
	outcome = outcome.withCancellation(ctx.Err())
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()

	record, err := s.mutate(persistCtx, runID, func(current run.Record) (run.Record, bool, error) {
		outcome = outcome.withCancellation(ctx.Err())
		if current.Terminal() {
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
			Evidence: outcome.scrubber.stringMap(outcome.evidence),
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
	outcome = outcome.withCancellation(ctx.Err())
	if err == nil && ctx.Err() != nil && record.Status != run.StatusCanceled {
		record, err = s.persistLateCancellation(ctx, runID)
	}
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

func (s *Service) persistLateCancellation(ctx context.Context, runID string) (run.Record, error) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	return s.mutate(persistCtx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.Status == run.StatusCanceled {
			return current, false, nil
		}
		if current.Terminal() {
			return current, false, fmt.Errorf("persist late cancellation: run is already terminal")
		}
		failure := canceledFailure("execute")
		now := s.clock()
		stage := run.StageResult{
			Name: "execute", Status: run.StageFailed, StartedAt: now, FinishedAt: now,
			Attempts: stageAttempt(current, "execute") + 1, Failure: &failure,
		}
		next, err := run.UpsertStage(run.CloneRecord(current), stage)
		if err != nil {
			return run.Record{}, false, err
		}
		next.Failure = &failure
		next.UpdatedAt = now
		next, err = run.Transition(next, run.StatusCanceled, now)
		if err != nil {
			return run.Record{}, false, err
		}
		return next, true, nil
	})
}

func (outcome executeOutcome) withFailure(failure run.Failure, cause error) executeOutcome {
	failure.Diagnostic = run.SafeDiagnostic(failure.Diagnostic)
	outcome.failure = &failure
	outcome.cause = errors.Join(outcome.cause, cause)
	return outcome
}

func (outcome executeOutcome) withCancellation(cause error) executeOutcome {
	if cause == nil {
		return outcome
	}
	return outcome.withFailure(canceledFailure("execute"), cause)
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

func environmentPolicyFailure(ctx context.Context, stage string) run.Failure {
	if ctx.Err() != nil {
		return canceledFailure(stage)
	}
	return run.Failure{
		Stage: stage, Class: run.FailurePolicy, Retryable: false,
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
	if err != nil {
		return phaseResult(record), err
	}
	if _, err := validateRunBinding(record); err != nil {
		return phaseResult(record), err
	}
	if record.Terminal() {
		return phaseResult(record), nil
	}
	record, err = s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.Terminal() {
			return current, false, nil
		}
		latest, found := latestStage(current, stage)
		if found && latest.Status == run.StageRunning {
			next := run.CloneRecord(current)
			failure := run.Failure{
				Stage: stage, Class: run.FailureInternal, Retryable: false,
				Diagnostic: "running attempt outcome is ambiguous", CauseCode: "ambiguous_attempt",
			}
			latest.Status = run.StageFailed
			latest.FinishedAt = s.clock()
			latest.Failure = &failure
			next.Failure = &failure
			var mutationErr error
			next, mutationErr = run.UpsertStage(next, latest)
			if mutationErr != nil {
				return run.Record{}, false, mutationErr
			}
			next, mutationErr = run.Transition(next, run.StatusFailed, s.clock())
			return next, true, mutationErr
		}
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
