package codechange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	execution       runner.ExecutionResult
	profile         repository.ProfileResult
	verification    []verification.Result
	capture         gitcapture.Result
	artifact        *artifact.Reference
	failure         *run.Failure
	cause           error
	evidence        map[string]string
	scrubber        durableScrubber
	ownership       stageOwnership
	runtimeID       string
	ownershipLost   bool
	ownershipRecord run.Record
	ownershipErr    error
}

type stageOwnership struct {
	name      string
	attempt   int
	startedAt time.Time
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
	ownership, err := ownershipFor(record, "execute")
	if err != nil {
		return phaseResult(record), err
	}
	runtimeID := executeRuntimeID(runID, ownership)

	prepared, err := s.workspaces.Prepare(ctx, record.RepositoryURI, record.BaseSHA)
	if err != nil {
		failure := classifyWorkspaceFailure(ctx, err)
		return s.finishFailure(ctx, runID, failure, err)
	}

	outcome := s.executePrepared(ctx, record, input, prepared.Path(), runtimeID, ownership)
	outcome = outcome.withCancellation(ctx.Err())
	cleanupErr := s.cleanupAttempt(ctx, runtimeID, prepared)
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
	if outcome.ownershipLost {
		return s.finishOwnershipLost(ctx, runID, outcome, cleanupErr)
	}
	return s.finishExecute(ctx, runID, ownership, outcome)
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
	runtimeID string,
	ownership stageOwnership,
) executeOutcome {
	scrubber, scrubberErr := newDurableScrubber(workspacePath)
	outcome := executeOutcome{
		evidence:  make(map[string]string),
		scrubber:  scrubber,
		ownership: ownership,
		runtimeID: runtimeID,
	}
	if scrubberErr != nil {
		return outcome.withFailure(unsafeEphemeralPrefixFailure(), scrubberErr)
	}
	agentEnvironment, err := s.environments.Build(ctx, environment.Request{
		RunID: runtimeID, Stage: environment.StageAgent, RequestedKeys: input.EnvironmentKeys,
	})
	if err != nil {
		return outcome.withFailure(environmentPolicyFailure(ctx, "execute"), err)
	}
	verificationEnvironment, err := s.environments.Build(ctx, environment.Request{
		RunID: runtimeID, Stage: environment.StageVerification, RequestedKeys: input.EnvironmentKeys,
	})
	if err != nil {
		return outcome.withFailure(environmentPolicyFailure(ctx, "execute"), err)
	}
	outcome.scrubber, err = newDurableScrubber(
		workspacePath, agentEnvironment.Values, verificationEnvironment.Values,
	)
	if err != nil {
		return outcome.withFailure(unsafeEphemeralPrefixFailure(), err)
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
	if ownedRecord, owned, ownershipErr := s.checkExecuteOwnership(ctx, record.ID, ownership); ownershipErr != nil || !owned {
		return outcome.withOwnershipLost(ownedRecord, ownershipErr)
	}

	outcome.execution, err = s.agent.Run(ctx, runner.RunRequest{
		TaskDescription: prompt, WorkspacePath: workspacePath,
		Env: cloneStringMap(agentEnvironment.Values),
	})
	if ownedRecord, owned, ownershipErr := s.checkExecuteOwnership(ctx, record.ID, ownership); ownershipErr != nil || !owned {
		return outcome.withOwnershipLost(ownedRecord, ownershipErr)
	}
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
		outcome = s.runVerification(ctx, record.ID, outcome, verificationEnvironment.Values)
		if outcome.ownershipLost {
			return outcome
		}
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
	if err := validateCapturedEvidence(outcome.capture, outcome.scrubber); err != nil {
		failure := run.Failure{
			Stage: "execute", Class: run.FailurePolicy, Retryable: false,
			Diagnostic: "captured changes contain unsafe path evidence",
			CauseCode:  "unsafe_capture_path",
		}
		return outcome.withFailure(failure, err)
	}
	if ownedRecord, owned, ownershipErr := s.checkExecuteOwnership(ctx, record.ID, ownership); ownershipErr != nil || !owned {
		return outcome.withOwnershipLost(ownedRecord, ownershipErr)
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
			Retryable:  false,
			Diagnostic: "build artifact evidence failed", CauseCode: "artifact_encoding",
		}
		return outcome.withFailure(failure, err)
	}
	expected, err := artifact.ReferenceFor(bundle)
	if err != nil {
		failure := run.Failure{
			Stage: "execute", Class: run.FailureInternal,
			Retryable: false, Diagnostic: "build artifact reference failed",
			CauseCode: "artifact_encoding",
		}
		return outcome.withFailure(failure, err)
	}
	leased, err := s.acquireArtifactWrite(ctx, record.ID, ownership, expected)
	if err != nil {
		if errors.Is(err, ErrPhaseInProgress) {
			return outcome.withOwnershipLost(leased, err)
		}
		failure := run.Failure{
			Stage: "execute", Class: run.FailureInternal,
			Retryable: false, Diagnostic: "acquire artifact write lease failed",
			CauseCode: "artifact_lease",
		}
		return outcome.withFailure(failure, err)
	}
	saveCtx, cancelSave := context.WithTimeout(ctx, s.artifactSaveTimeout)
	reference, err := s.artifacts.Save(saveCtx, bundle)
	saveCtxErr := saveCtx.Err()
	cancelSave()
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return outcome.withFailure(canceledFailure("execute"), errors.Join(err, ctx.Err()))
		}
		failure := run.Failure{
			Stage: "execute", Class: run.FailureInternal,
			Retryable:  !outcome.execution.Completed,
			Diagnostic: "persist artifact failed", CauseCode: "artifact_store",
		}
		return outcome.withFailure(failure, errors.Join(err, saveCtxErr))
	}
	if reference != expected {
		failure := run.Failure{
			Stage: "execute", Class: run.FailureInternal, Retryable: false,
			Diagnostic: "artifact store returned an unexpected reference",
			CauseCode:  "artifact_store",
		}
		return outcome.withFailure(failure, errors.New(failure.Diagnostic))
	}
	checkpoint, err := s.checkpointArtifact(ctx, record.ID, ownership, reference)
	if err != nil {
		if errors.Is(err, ErrPhaseInProgress) {
			return outcome.withOwnershipLost(checkpoint, err)
		}
		failure := run.Failure{
			Stage: "execute", Class: run.FailureInternal, Retryable: false,
			Diagnostic: "checkpoint artifact reference failed", CauseCode: "artifact_checkpoint",
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
	runID string,
	outcome executeOutcome,
	environmentValues map[string]string,
) executeOutcome {
	for _, command := range outcome.profile.Commands {
		if ownedRecord, owned, ownershipErr := s.checkExecuteOwnership(ctx, runID, outcome.ownership); ownershipErr != nil || !owned {
			return outcome.withOwnershipLost(ownedRecord, ownershipErr)
		}
		result := s.verifier.Run(ctx, command, cloneStringMap(environmentValues))
		outcome.verification = append(outcome.verification, result)
		if ownedRecord, owned, ownershipErr := s.checkExecuteOwnership(ctx, runID, outcome.ownership); ownershipErr != nil || !owned {
			return outcome.withOwnershipLost(ownedRecord, ownershipErr)
		}
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
	preflight, err := outcome.scrubber.stringMap(outcome.profile.Facts)
	if err != nil {
		return artifact.Bundle{}, err
	}
	changes := append([]artifact.Change(nil), outcome.capture.Changes...)
	return artifact.Bundle{
		Manifest: artifact.Manifest{
			RunID: record.ID, Template: record.Template,
			Repository: record.RepositoryURI, BaseSHA: record.BaseSHA,
			TreeSHA: outcome.capture.TreeSHA, Changes: changes,
			MemoryIDs: memoryIDs, MemoryCount: len(memoryIDs),
		},
		ChangesPatch:      append([]byte(nil), outcome.capture.Patch...),
		AgentOutput:       []byte(outcome.scrubber.string(outcome.execution.Output)),
		ExecutionMetadata: executionMetadata,
		Verification:      verificationEvidence,
		Preflight:         preflight,
		Warnings:          outcome.scrubber.strings(outcome.profile.Warnings),
	}, nil
}

type durableScrubber struct {
	workspace string
	prefixes  []scrubPrefix
}

type scrubPrefix struct {
	value       string
	replacement string
}

func newDurableScrubber(
	workspacePath string,
	environments ...map[string]string,
) (durableScrubber, error) {
	workspacePath, err := validateScrubPrefix(workspacePath)
	if err != nil {
		return durableScrubber{}, fmt.Errorf("workspace scrub prefix: %w", err)
	}
	scrubber := durableScrubber{workspace: workspacePath}
	seen := make(map[string]string)
	add := func(value, replacement string) error {
		if value == "" {
			return nil
		}
		value, err = validateScrubPrefix(value)
		if err != nil {
			return err
		}
		if existing, exists := seen[value]; exists {
			if existing != replacement {
				return fmt.Errorf("scrub prefix has ambiguous replacements")
			}
			return nil
		}
		seen[value] = replacement
		scrubber.prefixes = append(scrubber.prefixes, scrubPrefix{
			value: value, replacement: replacement,
		})
		return nil
	}
	if err := add(workspacePath, "."); err != nil {
		return durableScrubber{}, err
	}
	for _, values := range environments {
		for _, key := range []string{"HOME", "TMPDIR", "TMP", "TEMP", "CODEX_HOME"} {
			if err := add(values[key], "<runtime>"); err != nil {
				return durableScrubber{}, fmt.Errorf("%s scrub prefix: %w", key, err)
			}
		}
	}
	sort.Slice(scrubber.prefixes, func(left, right int) bool {
		return len(scrubber.prefixes[left].value) > len(scrubber.prefixes[right].value)
	})
	return scrubber, nil
}

func validateScrubPrefix(value string) (string, error) {
	if value == "" || value != strings.TrimSpace(value) || !filepath.IsAbs(value) {
		return "", fmt.Errorf("prefix must be an exact absolute path")
	}
	clean := filepath.Clean(value)
	if clean != value {
		return "", fmt.Errorf("prefix must already be canonical")
	}
	root := filepath.VolumeName(clean) + string(filepath.Separator)
	if clean == root {
		return "", fmt.Errorf("filesystem root is not a safe scrub prefix")
	}
	relative := strings.TrimPrefix(clean, root)
	if !strings.Contains(relative, string(filepath.Separator)) {
		return "", fmt.Errorf("top-level directory is too broad for path scrubbing")
	}
	return clean, nil
}

func (s durableScrubber) string(value string) string {
	for _, prefix := range s.prefixes {
		value = replaceAbsolutePrefix(value, prefix.value, prefix.replacement)
	}
	return value
}

func replaceAbsolutePrefix(value, prefix, replacement string) string {
	if value == "" || prefix == "" {
		return value
	}
	var output strings.Builder
	for {
		index := strings.Index(value, prefix)
		if index < 0 {
			output.WriteString(value)
			return output.String()
		}
		end := index + len(prefix)
		beforeOK := index == 0 || scrubBoundary(value[index-1])
		afterOK := end == len(value) || value[end] == '/' || value[end] == '\\' ||
			scrubBoundary(value[end])
		if !beforeOK || !afterOK {
			output.WriteString(value[:end])
			value = value[end:]
			continue
		}
		output.WriteString(value[:index])
		output.WriteString(replacement)
		value = value[end:]
	}
}

func scrubBoundary(value byte) bool {
	if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' {
		return false
	}
	switch value {
	case '_', '-', '.', '/', '\\':
		return false
	default:
		return true
	}
}

func (s durableScrubber) strings(values []string) []string {
	scrubbed := make([]string, len(values))
	for index, value := range values {
		scrubbed[index] = s.string(value)
	}
	return scrubbed
}

func (s durableScrubber) stringMap(values map[string]string) (map[string]string, error) {
	if values == nil {
		return nil, nil
	}
	scrubbed := make(map[string]string, len(values))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		durableKey := s.string(key)
		if _, exists := scrubbed[durableKey]; exists {
			return nil, fmt.Errorf("durable evidence key collision after path scrubbing")
		}
		scrubbed[durableKey] = s.string(values[key])
	}
	return scrubbed, nil
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
	result.Command.Environment, err = s.stringMap(result.Command.Environment)
	if err != nil {
		return verification.Result{}, err
	}
	result.Output = s.string(result.Output)
	result.FailureClass = s.string(result.FailureClass)
	result.CauseCode = s.string(result.CauseCode)
	return result, nil
}

func validateCapturedEvidence(result gitcapture.Result, scrubber durableScrubber) error {
	for _, change := range result.Changes {
		if err := validateRepositoryPath(change.Path, false); err != nil {
			return fmt.Errorf("captured path %q: %w", change.Path, err)
		}
		if err := validateRepositoryPath(change.OldPath, true); err != nil {
			return fmt.Errorf("captured old path %q: %w", change.OldPath, err)
		}
	}
	patch := string(result.Patch)
	for _, prefix := range scrubber.prefixes {
		if replaceAbsolutePrefix(patch, prefix.value, "\x00") != patch {
			return fmt.Errorf("captured patch contains an ephemeral absolute path")
		}
	}
	return nil
}

func validateRepositoryPath(value string, optional bool) error {
	if value == "" && optional {
		return nil
	}
	if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') ||
		strings.Contains(value, "\\") || path.IsAbs(value) {
		return fmt.Errorf("path must be a non-empty UTF-8 repository-relative path")
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return fmt.Errorf("path is not canonical repository-relative form")
	}
	return nil
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

func (s *Service) acquireArtifactWrite(
	ctx context.Context,
	runID string,
	ownership stageOwnership,
	reference artifact.Reference,
) (run.Record, error) {
	persistCtx, cancel := context.WithTimeout(ctx, s.cleanupTimeout)
	defer cancel()
	return s.mutate(persistCtx, runID, func(current run.Record) (run.Record, bool, error) {
		if !s.ownsStage(current, ownership) {
			return current, false, ErrPhaseInProgress
		}
		if current.Artifact != nil {
			return current, false, ErrPhaseInProgress
		}
		now := s.clock()
		deadline := now.Add(s.artifactWriteLeaseDuration())
		if current.ArtifactWriteLease != nil {
			lease := current.ArtifactWriteLease
			if !artifactWriteLeaseMatches(current, ownership, reference, now) {
				return current, false, ErrPhaseInProgress
			}
			if !deadline.After(lease.Deadline) {
				return current, false, nil
			}
		}
		next := run.CloneRecord(current)
		next.ArtifactWriteLease = &run.ArtifactWriteLease{
			Attempt: ownership.attempt, StartedAt: ownership.startedAt,
			Deadline: deadline, Reference: reference,
		}
		next.UpdatedAt = now
		return next, true, nil
	})
}

func (s *Service) checkpointArtifact(
	ctx context.Context,
	runID string,
	ownership stageOwnership,
	reference artifact.Reference,
) (run.Record, error) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	return s.mutate(persistCtx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.Terminal() ||
			!artifactWriteLeaseMatches(current, ownership, reference, s.clock()) {
			return current, false, ErrPhaseInProgress
		}
		next := run.CloneRecord(current)
		checkpoint := reference
		next.Artifact = &checkpoint
		next.ArtifactWriteLease = nil
		next.UpdatedAt = s.clock()
		return next, true, nil
	})
}

func (s *Service) artifactWriteLeaseDuration() time.Duration {
	grace := s.cleanupTimeout
	if grace <= 0 {
		grace = time.Second
	}
	timeout := s.artifactSaveTimeout
	if timeout <= 0 {
		timeout = time.Second
	}
	return timeout + 2*grace
}

func (s *Service) finishExecute(
	ctx context.Context,
	runID string,
	ownership stageOwnership,
	outcome executeOutcome,
) (PhaseResult, error) {
	outcome = outcome.withCancellation(ctx.Err())
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()

	var persistedFailure *run.Failure
	record, err := s.mutate(persistCtx, runID, func(current run.Record) (run.Record, bool, error) {
		outcome = outcome.withCancellation(ctx.Err())
		owned := s.ownsStage(current, ownership)
		if outcome.artifact != nil {
			owned = ownsCheckpointedArtifact(current, ownership, *outcome.artifact)
		}
		if !owned {
			return current, false, ErrPhaseInProgress
		}
		next := run.CloneRecord(current)
		if outcome.artifact != nil {
			reference := *outcome.artifact
			next.Artifact = &reference
		}
		stage := run.StageResult{
			Name: "execute", StartedAt: latestStageStart(current, "execute"),
			FinishedAt: s.clock(), Attempts: stageAttempt(current, "execute"),
		}
		var mutationErr error
		stage.Evidence, mutationErr = outcome.scrubber.stringMap(outcome.evidence)
		if mutationErr != nil {
			return run.Record{}, false, mutationErr
		}
		if outcome.failure == nil {
			stage.Status = run.StageSucceeded
			next.Failure = nil
		} else {
			stage.Status = run.StageFailed
			failure := *outcome.failure
			stage.Failure = &failure
			next.Failure = &failure
			persistedFailure = &failure
		}
		next.ArtifactWriteLease = nil
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
	if ctx.Err() != nil {
		originalErr := err
		compensated, compensationErr := s.persistLateCancellation(
			ctx, runID, ownership, persistedFailure,
		)
		if compensationErr == nil {
			record = compensated
		} else if record.ID == "" {
			loadCtx, loadCancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
			loaded, loadErr := s.runs.Load(loadCtx, runID)
			loadCancel()
			if loadErr == nil {
				record = loaded
			}
			compensationErr = errors.Join(compensationErr, loadErr)
		}
		err = errors.Join(originalErr, compensationErr)
	}
	if errors.Is(err, ErrPhaseInProgress) {
		return s.finishOwnershipLost(
			ctx, runID, outcome.withOwnershipLost(record, err), nil,
		)
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

func ownershipFor(record run.Record, name string) (stageOwnership, error) {
	latest, found := latestStage(record, name)
	if !found || latest.Status != run.StageRunning {
		return stageOwnership{}, fmt.Errorf("capture %s ownership: no running attempt", name)
	}
	return stageOwnership{name: name, attempt: latest.Attempts, startedAt: latest.StartedAt}, nil
}

func (s *Service) ownsStage(record run.Record, ownership stageOwnership) bool {
	if record.Terminal() {
		return false
	}
	latest, found := latestStage(record, ownership.name)
	if !found || latest.Status != run.StageRunning ||
		latest.Attempts != ownership.attempt ||
		!latest.StartedAt.Equal(ownership.startedAt) {
		return false
	}
	return !stageExpired(latest, s.clock(), s.leaseFor(ownership.name)) ||
		activeArtifactWriteLease(record, ownership, s.clock())
}

func activeArtifactWriteLease(
	record run.Record,
	ownership stageOwnership,
	now time.Time,
) bool {
	lease := record.ArtifactWriteLease
	return lease != nil &&
		lease.Attempt == ownership.attempt &&
		lease.StartedAt.Equal(ownership.startedAt) &&
		now.Before(lease.Deadline)
}

func artifactWriteLeaseMatches(
	record run.Record,
	ownership stageOwnership,
	reference artifact.Reference,
	now time.Time,
) bool {
	return activeArtifactWriteLease(record, ownership, now) &&
		record.ArtifactWriteLease.Reference == reference
}

func exactStageIdentity(record run.Record, ownership stageOwnership) bool {
	if record.Terminal() {
		return false
	}
	latest, found := latestStage(record, ownership.name)
	return found && latest.Status == run.StageRunning &&
		latest.Attempts == ownership.attempt &&
		latest.StartedAt.Equal(ownership.startedAt)
}

func ownsCheckpointedArtifact(
	record run.Record,
	ownership stageOwnership,
	reference artifact.Reference,
) bool {
	return exactStageIdentity(record, ownership) &&
		record.Artifact != nil && *record.Artifact == reference &&
		record.ArtifactWriteLease == nil
}

func (s *Service) leaseFor(stage string) time.Duration {
	if stage == "execute" {
		return s.executeLease
	}
	return s.resolveLease
}

func (s *Service) checkExecuteOwnership(
	ctx context.Context,
	runID string,
	ownership stageOwnership,
) (run.Record, bool, error) {
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	record, err := s.runs.Load(checkCtx, runID)
	if err != nil {
		return run.Record{}, false, err
	}
	if _, err := validateRunBinding(record); err != nil {
		return record, false, err
	}
	return record, s.ownsStage(record, ownership), nil
}

func executeRuntimeID(runID string, ownership stageOwnership) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%s\x00%s\x00%d\x00%s", runID, ownership.name, ownership.attempt,
		ownership.startedAt.UTC().Format(time.RFC3339Nano),
	)))
	return "execute-" + hex.EncodeToString(sum[:16])
}

func (outcome executeOutcome) withOwnershipLost(record run.Record, cause error) executeOutcome {
	outcome.ownershipLost = true
	outcome.ownershipRecord = record
	if cause == nil {
		cause = ErrPhaseInProgress
	}
	outcome.ownershipErr = errors.Join(outcome.ownershipErr, cause)
	return outcome
}

func (s *Service) finishOwnershipLost(
	ctx context.Context,
	runID string,
	outcome executeOutcome,
	cleanupErr error,
) (PhaseResult, error) {
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	record, loadErr := s.runs.Load(checkCtx, runID)
	if loadErr != nil {
		record = outcome.ownershipRecord
	}
	cause := errors.Join(outcome.ownershipErr, cleanupErr, loadErr)
	if record.Terminal() && record.Failure != nil {
		return phaseResult(record), errors.Join(
			newPhaseError(*record.Failure, errors.New(record.Failure.Diagnostic)),
			cause,
		)
	}
	result, inProgressErr := phaseInProgress(record)
	return result, errors.Join(inProgressErr, cause)
}

func (s *Service) persistLateCancellation(
	ctx context.Context,
	runID string,
	ownership stageOwnership,
	persistedFailure *run.Failure,
) (run.Record, error) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	return s.mutate(persistCtx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.Status == run.StatusCanceled {
			return current, false, nil
		}
		if current.Status == run.StatusFailed {
			if persistedFailure == nil || current.Failure == nil ||
				*current.Failure != *persistedFailure {
				return current, false, ErrPhaseInProgress
			}
			now := s.clock()
			if !now.After(current.UpdatedAt) {
				now = current.UpdatedAt.Add(time.Nanosecond)
			}
			next, err := run.CompensateExecuteCancellation(
				current, ownership.attempt, ownership.startedAt, now,
			)
			if err != nil {
				return run.Record{}, false, err
			}
			return next, true, nil
		}
		if current.Terminal() {
			return current, false, fmt.Errorf("persist late cancellation: run is already terminal")
		}
		latest, found := latestStage(current, "execute")
		if !found || latest.Attempts != ownership.attempt ||
			!latest.StartedAt.Equal(ownership.startedAt) {
			return current, false, ErrPhaseInProgress
		}
		failure := canceledFailure("execute")
		now := s.clock()
		stage := latest
		if stage.Status == run.StageRunning {
			stage.Status = run.StageFailed
			stage.FinishedAt = now
			stage.Failure = &failure
		} else {
			stage = run.StageResult{
				Name: "execute", Status: run.StageFailed, StartedAt: now, FinishedAt: now,
				Attempts: stageAttempt(current, "execute") + 1, Failure: &failure,
			}
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

func unsafeEphemeralPrefixFailure() run.Failure {
	return run.Failure{
		Stage: "execute", Class: run.FailureInternal, Retryable: false,
		Diagnostic: "execution environment contains an unsafe ephemeral path",
		CauseCode:  "unsafe_ephemeral_prefix",
	}
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
	if stage == finalizeStage {
		return s.exhaustFinalize(ctx, runID)
	}
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
	inProgress := false
	record, err = s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		inProgress = false
		if current.Terminal() {
			return current, false, nil
		}
		latest, found := latestStage(current, stage)
		if found && latest.Status == run.StageRunning {
			ownership := stageOwnership{
				name: stage, attempt: latest.Attempts, startedAt: latest.StartedAt,
			}
			if !stageExpired(latest, s.clock(), s.leaseFor(stage)) ||
				(stage == "execute" && activeArtifactWriteLease(current, ownership, s.clock())) {
				inProgress = true
				return current, false, nil
			}
			next := run.CloneRecord(current)
			failure := run.Failure{
				Stage: stage, Class: run.FailureInternal, Retryable: false,
				Diagnostic: "running attempt outcome is ambiguous", CauseCode: "ambiguous_attempt",
			}
			latest.Status = run.StageFailed
			latest.FinishedAt = s.clock()
			latest.Failure = &failure
			next.Failure = &failure
			next.ArtifactWriteLease = nil
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
	if err == nil && inProgress {
		return phaseInProgress(record)
	}
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
