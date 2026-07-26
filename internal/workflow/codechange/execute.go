package codechange

import (
	"context"
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
	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/executor/commandrunner"
	"github.com/araihu/paje/internal/policy"
	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/run"
	"github.com/araihu/paje/internal/secret"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workerprofile"
	"github.com/araihu/paje/internal/workspace"
)

const (
	defaultSandboxTimeout = time.Minute
	defaultOutputLimit    = int64(1 << 20)
)

type executeOutcome struct {
	execution        executor.Result
	agentOutput      string
	profile          repository.ProfileResult
	verification     []verification.Result
	capture          gitcapture.Result
	profileSnapshot  workerprofile.Snapshot
	runtimeIsolated  bool
	runtimeCertified bool
	harnessEvidence  artifact.HarnessEvidence
	toolEvidence     []artifact.ToolEvidence
	attemptEvidence  []artifact.AttemptEvidence
	artifact         *artifact.Reference
	failure          *run.Failure
	cause            error
	cleanupFailed    bool
	evidence         map[string]string
	scrubber         durableScrubber
	ownership        stageOwnership
	ownershipLost    bool
	ownershipRecord  run.Record
	ownershipErr     error
}

type activeLease struct {
	id              string
	materialization secret.Materialization
}

type fencedExecutor struct {
	service   *Service
	target    executor.Executor
	runID     string
	ownership stageOwnership
	outcome   *executeOutcome
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
	record, started, err := s.beginExecuteStage(ctx, runID)
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
	prepared, err := s.workspaces.Prepare(ctx, record.RepositoryURI, record.BaseSHA)
	if err != nil {
		failure := classifyWorkspaceFailure(ctx, err)
		return s.finishFailure(ctx, runID, failure, err)
	}

	outcome := s.executePrepared(ctx, record, input, prepared.Path(), ownership)
	outcome = outcome.withCancellation(ctx.Err())
	cleanupErr := s.cleanupAttempt(ctx, prepared)
	outcome = outcome.withCleanupFailure(cleanupErr)
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

// beginExecuteStage is deliberately separate from the generic stage helper:
// taking over Execute requires executor observation, while every other phase
// can classify an expired worker from durable state alone.
func (s *Service) beginExecuteStage(ctx context.Context, runID string) (run.Record, bool, error) {
	for recovery := 0; recovery < maxCASAttempts; recovery++ {
		record, err := s.runs.Load(ctx, runID)
		if err != nil {
			return run.Record{}, false, err
		}
		if _, err := validateRunBinding(record); err != nil {
			return record, false, err
		}
		if record.Terminal() {
			return record, false, nil
		}
		latest, found := latestStage(record, "execute")
		if !found || latest.Status != run.StageRunning {
			return s.beginStage(ctx, runID, "execute", run.StatusExecuting)
		}
		ownership := stageOwnership{name: "execute", attempt: latest.Attempts, startedAt: latest.StartedAt}
		if !stageExpired(latest, s.clock(), s.executeLease) || activeArtifactWriteLease(record, ownership, s.clock()) {
			return record, false, nil
		}
		if record.Artifact != nil {
			recovered, err := s.completeCheckpointedExecute(ctx, runID, ownership)
			return recovered, false, err
		}
		if record.WorkerProfile == nil {
			return record, false, fmt.Errorf("recover execute: resolved worker profile is missing")
		}
		target, err := s.executors.Resolve(record.WorkerProfile.Clone())
		if err != nil {
			return s.failAmbiguousExecute(ctx, runID, ownership, err)
		}
		agentAttempt := executorAttempt(runID, ownership, executor.PurposeAgent, 0)
		state, inspectErr := target.Inspect(ctx, agentAttempt)
		if inspectErr != nil {
			return s.failAmbiguousExecute(ctx, runID, ownership, inspectErr)
		}
		owned, identityErr := s.checkExecuteIdentity(ctx, runID, ownership)
		if identityErr != nil {
			return owned, false, identityErr
		}
		if !exactStageIdentity(owned, ownership) {
			continue
		}
		durableStart := latest.Evidence["agent_started"] == "true"
		if (state != executor.StateAbsent && state != executor.StateCreated) || durableStart {
			return s.failAmbiguousExecute(ctx, runID, ownership, errors.New("prior agent attempt may have started"))
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
		destroyErr := target.Destroy(cleanupCtx, agentAttempt)
		cancel()
		if destroyErr != nil {
			return s.failAmbiguousExecute(ctx, runID, ownership, destroyErr)
		}
		return s.retryConclusiveNonStart(ctx, runID, ownership)
	}
	return run.Record{}, false, fmt.Errorf("recover execute after %d ownership races: %w", maxCASAttempts, run.ErrVersionConflict)
}

func (s *Service) checkExecuteIdentity(ctx context.Context, runID string, ownership stageOwnership) (run.Record, error) {
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	record, err := s.runs.Load(checkCtx, runID)
	if err != nil {
		return run.Record{}, err
	}
	if _, err := validateRunBinding(record); err != nil {
		return record, err
	}
	return record, nil
}

func (s *Service) completeCheckpointedExecute(ctx context.Context, runID string, ownership stageOwnership) (run.Record, error) {
	return s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		if !exactStageIdentity(current, ownership) || current.Artifact == nil {
			return current, false, ErrPhaseInProgress
		}
		next := run.CloneRecord(current)
		latest, _ := latestStage(next, "execute")
		latest.Status = run.StageSucceeded
		latest.FinishedAt = s.clock()
		latest.Failure = nil
		next.Failure = nil
		next.ArtifactWriteLease = nil
		var err error
		next, err = run.UpsertStage(next, latest)
		if err != nil {
			return run.Record{}, false, err
		}
		next.UpdatedAt = s.clock()
		return next, true, nil
	})
}

func (s *Service) failAmbiguousExecute(ctx context.Context, runID string, ownership stageOwnership, cause error) (run.Record, bool, error) {
	record, err := s.mutate(context.WithoutCancel(ctx), runID, func(current run.Record) (run.Record, bool, error) {
		if !exactStageIdentity(current, ownership) {
			return current, false, ErrPhaseInProgress
		}
		next := run.CloneRecord(current)
		latest, _ := latestStage(next, "execute")
		failure := ambiguousAttemptFailure()
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
	})
	if err != nil {
		return record, false, err
	}
	failure := ambiguousAttemptFailure()
	return record, false, newPhaseError(failure, cause)
}

func (s *Service) retryConclusiveNonStart(ctx context.Context, runID string, ownership stageOwnership) (run.Record, bool, error) {
	started := false
	record, err := s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		if !exactStageIdentity(current, ownership) || !stageExpired(mustLatestStage(current, "execute"), s.clock(), s.executeLease) {
			return current, false, ErrPhaseInProgress
		}
		next := run.CloneRecord(current)
		latest, _ := latestStage(next, "execute")
		lost := run.Failure{
			Stage: "execute", Class: run.FailureInternal, Retryable: true,
			Diagnostic: "worker was lost before the agent started", CauseCode: "worker_lost_before_start",
		}
		latest.Status = run.StageFailed
		latest.FinishedAt = s.clock()
		latest.Failure = &lost
		var mutationErr error
		next, mutationErr = run.UpsertStage(next, latest)
		if mutationErr != nil {
			return run.Record{}, false, mutationErr
		}
		next.Failure = nil
		stage := run.StageResult{
			Name: "execute", Status: run.StageRunning, StartedAt: s.clock(), Attempts: latest.Attempts + 1,
		}
		next, mutationErr = run.UpsertStage(next, stage)
		if mutationErr != nil {
			return run.Record{}, false, mutationErr
		}
		next.UpdatedAt = s.clock()
		started = true
		return next, true, nil
	})
	return record, started, err
}

func mustLatestStage(record run.Record, name string) run.StageResult {
	stage, _ := latestStage(record, name)
	return stage
}

func ambiguousAttemptFailure() run.Failure {
	return run.Failure{
		Stage: "execute", Class: run.FailureInternal, Retryable: false,
		Diagnostic: "running attempt outcome is ambiguous", CauseCode: "ambiguous_attempt",
	}
}

func providerReportedAmbiguousAttempt(err error) bool {
	var providerError *executor.ProviderError
	return errors.As(err, &providerError) && providerError.CauseCode == "ambiguous_attempt"
}

func (s *Service) executePrepared(
	ctx context.Context,
	record run.Record,
	input templatecodechange.Input,
	workspacePath string,
	ownership stageOwnership,
) executeOutcome {
	scrubber, scrubberErr := newDurableScrubber(workspacePath)
	outcome := executeOutcome{
		evidence:  make(map[string]string),
		scrubber:  scrubber,
		ownership: ownership,
	}
	if scrubberErr != nil {
		return outcome.withFailure(unsafeEphemeralPrefixFailure(), scrubberErr)
	}
	if record.WorkerProfile == nil {
		return outcome.withFailure(ambiguousAttemptFailure(), errors.New("resolved worker profile is missing"))
	}
	outcome.profileSnapshot = record.WorkerProfile.Clone()
	target, err := s.executors.Resolve(outcome.profileSnapshot)
	if err != nil {
		return outcome.withFailure(executorUnavailableFailure(), err)
	}
	adapter, err := s.harnesses.Resolve(outcome.profileSnapshot)
	if err != nil {
		return outcome.withFailure(executorUnavailableFailure(), err)
	}
	fenced := &fencedExecutor{
		service: s, target: target, runID: record.ID, ownership: ownership, outcome: &outcome,
	}
	baseline := sandboxEnvironment()
	outcome.evidence["agent_environment_keys"] = encodeStrings(sortedMapKeys(baseline))
	outcome.evidence["verification_environment_keys"] = encodeStrings(sortedMapKeys(baseline))

	harnessProbe := adapter.Probe()
	harnessResult, probeErr, probeCleanupErr := fenced.runProbe(
		ctx, workspacePath, outcome.profileSnapshot, harnessProbe, 0,
	)
	if probeCleanupErr != nil {
		harnessResult.Destroy()
		return outcome.withCleanupFailure(errors.Join(probeErr, probeCleanupErr))
	}
	if probeErr != nil || !probeSucceeded(harnessResult, outcome.profileSnapshot.Harness.Version) ||
		!runtimeProbeMatches(outcome.profileSnapshot, harnessResult.SafeFacts) {
		harnessResult.Destroy()
		return outcome.withFailure(executorUnavailableFailure(), errors.Join(probeErr, errors.New("harness probe failed")))
	}
	outcome.runtimeIsolated = harnessResult.SafeFacts["isolated"] == "true"
	outcome.runtimeCertified = harnessResult.SafeFacts["certified"] == "true"
	harnessResult.Destroy()
	outcome.harnessEvidence = artifact.HarnessEvidence{
		ID: adapter.ID(), DeclaredVersion: adapter.Version(), ProbedVersion: adapter.Version(),
		ProbePassed: true, Sequence: 0,
	}
	for index, tool := range outcome.profileSnapshot.Tools {
		command := executor.Command{
			Executable: tool.Probe.Executable, Args: append([]string(nil), tool.Probe.Args...),
			Directory: executor.SandboxWorkspaceRoot,
		}
		result, probeErr, probeCleanupErr := fenced.runProbe(
			ctx, workspacePath, outcome.profileSnapshot, command, index+1,
		)
		if probeCleanupErr != nil {
			result.Destroy()
			return outcome.withCleanupFailure(errors.Join(probeErr, probeCleanupErr))
		}
		if probeErr != nil || !probeSucceeded(result, tool.Probe.OutputContains) {
			result.Destroy()
			return outcome.withFailure(executorUnavailableFailure(), errors.Join(probeErr, errors.New("required tool probe failed")))
		}
		result.Destroy()
		outcome.toolEvidence = append(outcome.toolEvidence, artifact.ToolEvidence{
			Name: tool.Name, DeclaredVersion: tool.Version, ProbedVersion: tool.Version,
			ProbePassed: true, Sequence: index + 1,
		})
	}
	if outcome.ownershipLost {
		return outcome
	}

	preflightRunner, err := commandrunner.New(commandrunner.Config{
		Executor: fenced, Profile: outcome.profileSnapshot,
		Attempt:   executorAttempt(record.ID, ownership, executor.PurposeProbe, len(outcome.profileSnapshot.Tools)),
		Workspace: workspacePath, Environment: baseline, OutputLimit: defaultOutputLimit,
	})
	if err != nil {
		return outcome.withFailure(executorUnavailableFailure(), err)
	}
	outcome.profile, err = s.profiles[input.Profile].Inspect(ctx, repository.ProfileRequest{
		Workspace: workspacePath, Commands: preflightRunner,
		Checks:           append([]verification.CommandSpec(nil), input.Checks...),
		ModuleExclusions: append([]repository.ModuleExclusion(nil), input.ModuleExclusions...),
	})
	if outcome.ownershipLost {
		return outcome
	}
	if err != nil {
		return outcome.withFailure(classifyProfileFailure(ctx, err), err)
	}
	outcome.evidence["preflight_fact_count"] = strconv.Itoa(len(outcome.profile.Facts))
	outcome.evidence["verification_command_count"] = strconv.Itoa(len(outcome.profile.Commands))

	prompt, err := buildPrompt(promptInput{
		Task: input.TaskDescription, BaseSHA: record.BaseSHA, Profile: input.Profile,
		WorkerProfile: outcome.profileSnapshot.Metadata.String(), WorkerProfileDigest: outcome.profileSnapshot.Digest,
		Facts: outcome.profile.Facts, Memory: record.MemorySnapshot,
	}, maxAgentPromptBytes)
	if err != nil {
		failure := run.Failure{
			Stage: "execute", Class: run.FailureInput, Retryable: false,
			Diagnostic: "agent prompt exceeds configured limit", CauseCode: "prompt_too_large",
		}
		return outcome.withFailure(failure, err)
	}

	leases, acquireErr := s.acquireAgentLeases(ctx, record, ownership, &outcome)
	if acquireErr != nil {
		return outcome.withFailure(secretAcquisitionFailure(ctx), acquireErr)
	}
	detector := detectorForLeases(leases)
	defer detector.Destroy()
	agentCommand, err := adapter.AgentCommand(prompt)
	if err != nil {
		cleanupErr := s.cleanupAgent(ctx, fenced, leases, false)
		outcome = outcome.withFailure(agentProtocolFailure(), err)
		return outcome.withCleanupFailure(cleanupErr)
	}
	agentAttempt := executorAttempt(record.ID, ownership, executor.PurposeAgent, 0)
	agentRequest := executor.Request{
		Attempt: agentAttempt, Profile: outcome.profileSnapshot.Clone(), Command: agentCommand,
		Workspace:   executor.Workspace{HostPath: workspacePath, SandboxPath: executor.SandboxWorkspaceRoot, Writable: true},
		Environment: cloneStringMap(baseline), Secrets: leaseMaterializations(leases),
		Timeout: s.executeLease, OutputLimit: defaultOutputLimit,
	}
	var executorErr error
	outcome.execution, executorErr = fenced.Execute(ctx, agentRequest)
	agentRequest.Destroy()

	if ctx.Err() != nil || errors.Is(executorErr, context.Canceled) || errors.Is(executorErr, context.DeadlineExceeded) {
		confirmed, cleanupErr := s.cancelAndCleanupAgent(ctx, fenced, leases, agentAttempt)
		if !confirmed {
			return outcome.withFailure(ambiguousAttemptFailure(), errors.Join(executorErr, ctx.Err(), cleanupErr))
		}
		return outcome.withFailure(canceledFailure("execute"), errors.Join(executorErr, ctx.Err(), cleanupErr))
	}
	if outcome.ownershipLost {
		if cleanupErr := s.cleanupAgent(ctx, fenced, leases, false); cleanupErr != nil {
			outcome.cleanupFailed = true
			outcome.ownershipErr = errors.Join(outcome.ownershipErr, cleanupErr)
		}
		return outcome
	}
	if outcome.execution.SecretDetected || detector.Scan(outcome.execution.Stdout) || detector.Scan(outcome.execution.Stderr) {
		cleanupErr := s.cleanupAgent(ctx, fenced, leases, false)
		outcome.execution.Destroy()
		outcome = outcome.withFailure(secretDetectedFailure(), nil)
		return outcome.withCleanupFailure(cleanupErr)
	}
	if outcome.failure == nil {
		switch {
		case providerReportedAmbiguousAttempt(executorErr):
			outcome = outcome.withFailure(ambiguousAttemptFailure(), executorErr)
		case executorErr != nil && outcome.execution.Started:
			outcome = outcome.withFailure(ambiguousAttemptFailure(), executorErr)
		case executorErr != nil:
			failure := run.Failure{Stage: "execute", Class: run.FailureAgent, Retryable: !outcome.execution.Started,
				Diagnostic: "agent execution was unavailable", CauseCode: "agent_unavailable"}
			outcome = outcome.withFailure(failure, executorErr)
		case !outcome.execution.Started || !outcome.execution.Completed:
			if outcome.execution.Started {
				outcome = outcome.withFailure(ambiguousAttemptFailure(), errors.New("agent response was incomplete after start"))
			} else {
				failure := run.Failure{Stage: "execute", Class: run.FailureAgent, Retryable: true,
					Diagnostic: "agent did not start", CauseCode: "agent_unavailable"}
				outcome = outcome.withFailure(failure, errors.New(failure.Diagnostic))
			}
		case outcome.execution.ExitCode != 0:
			failure := run.Failure{Stage: "execute", Class: run.FailureAgent, Retryable: false,
				Diagnostic: fmt.Sprintf("agent exited with code %d", outcome.execution.ExitCode), CauseCode: "nonzero_exit"}
			outcome = outcome.withFailure(failure, errors.New(failure.Diagnostic))
		}
	}
	if outcome.failure == nil {
		var parseErr error
		outcome.agentOutput, parseErr = adapter.Parse(outcome.execution.Clone())
		if parseErr == nil && detector.Scan([]byte(outcome.agentOutput)) {
			parseErr = errors.New("agent parser returned leased secret material")
			outcome = outcome.withFailure(secretDetectedFailure(), parseErr)
		} else if parseErr != nil {
			outcome = outcome.withFailure(agentProtocolFailure(), parseErr)
		}
	}
	outcome.execution.Destroy()

	if outcome.execution.Started {
		transient, captureErr := s.captureOwned(ctx, record, workspacePath, ownership, &outcome)
		if captureErr != nil && !outcome.ownershipLost {
			outcome = outcome.withFailure(captureFailure(outcome.execution.Completed), captureErr)
		} else if !policy.DetectSecretMaterial(ctx, transient, detector).Allowed {
			outcome = outcome.withFailure(secretDetectedFailure(), errors.New("transient capture contains leased secret material"))
		}
	}
	cleanupErr := s.cleanupAgent(ctx, fenced, leases, false)
	if cleanupErr != nil {
		outcome = outcome.withCleanupFailure(cleanupErr)
	}
	if outcome.ownershipLost {
		return outcome
	}
	if outcome.failure != nil && outcome.failure.Class == run.FailurePolicy {
		return outcome
	}
	if outcome.failure != nil && (!outcome.execution.Started ||
		outcome.cleanupFailed ||
		outcome.failure.CauseCode == "ambiguous_attempt" ||
		outcome.failure.CauseCode == "capture_failed" ||
		outcome.failure.CauseCode == "cleanup_failed") {
		return outcome
	}

	if outcome.failure == nil {
		outcome = s.runVerification(ctx, record, workspacePath, baseline, detector, fenced, outcome)
		if outcome.ownershipLost || outcome.failure != nil && outcome.failure.Class == run.FailureCanceled {
			return outcome
		}
	}
	outcome.capture, err = s.captureOwned(ctx, record, workspacePath, ownership, &outcome)
	if outcome.ownershipLost {
		return outcome
	}
	if err != nil {
		return outcome.withFailure(captureFailure(outcome.execution.Completed), err)
	}
	if !policy.DetectSecretMaterial(ctx, outcome.capture, detector).Allowed {
		return outcome.withFailure(secretDetectedFailure(), errors.New("captured changes contain leased secret material"))
	}
	if err := validateCapturedEvidence(outcome.capture, outcome.scrubber); err != nil {
		failure := run.Failure{Stage: "execute", Class: run.FailurePolicy, Retryable: false,
			Diagnostic: "captured changes contain unsafe path evidence", CauseCode: "unsafe_capture_path"}
		return outcome.withFailure(failure, err)
	}
	decision := s.policy.Evaluate(ctx, outcome.capture)
	if ownedRecord, owned, ownershipErr := s.checkExecuteOwnership(ctx, record.ID, ownership); ownershipErr != nil || !owned {
		return outcome.withOwnershipLost(ownedRecord, ownershipErr)
	}
	if ctx.Err() != nil {
		return outcome.withFailure(canceledFailure("execute"), ctx.Err())
	}
	if !decision.Allowed {
		outcome.evidence["policy_findings"] = encodeFindings(decision.Findings)
		failure := run.Failure{Stage: "execute", Class: run.FailurePolicy, Retryable: false,
			Diagnostic: "captured changes were denied by policy", CauseCode: "change_policy_denied"}
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
	record run.Record,
	workspacePath string,
	environmentValues map[string]string,
	detector secret.Detector,
	fenced *fencedExecutor,
	outcome executeOutcome,
) executeOutcome {
	runner, err := commandrunner.New(commandrunner.Config{
		Executor: fenced, Profile: outcome.profileSnapshot,
		Attempt:   executorAttempt(record.ID, outcome.ownership, executor.PurposeVerification, 0),
		Workspace: workspacePath, Environment: environmentValues, OutputLimit: defaultOutputLimit,
	})
	if err != nil {
		return outcome.withFailure(executorUnavailableFailure(), err)
	}
	for _, original := range outcome.profile.Commands {
		command := original
		command.Directory, err = durableDirectory(workspacePath, command.Directory)
		if err != nil {
			return outcome.withFailure(run.Failure{Stage: "execute", Class: run.FailureInternal, Retryable: false,
				Diagnostic: "verification command directory is unsafe", CauseCode: "verification_internal"}, err)
		}
		result := runner.Run(ctx, command)
		outcome.verification = append(outcome.verification, result)
		if outcome.ownershipLost {
			return outcome
		}
		if detector.Scan([]byte(result.Output)) {
			return outcome.withFailure(secretDetectedFailure(), errors.New("verification output contains leased secret material"))
		}
		if !command.Required || result.Passed || outcome.failure != nil {
			continue
		}
		switch result.FailureClass {
		case "canceled":
			failure := canceledFailure("execute")
			outcome = outcome.withFailure(failure, context.Canceled)
		case "cleanup":
			outcome = outcome.withCleanupFailure(errors.New("verification sandbox cleanup failed"))
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
	outcome.attemptEvidence = append([]artifact.AttemptEvidence(nil), fenced.outcome.attemptEvidence...)
	if fenced.outcome.ownershipLost {
		return outcome.withOwnershipLost(fenced.outcome.ownershipRecord, fenced.outcome.ownershipErr)
	}
	return outcome
}

func executorAttempt(runID string, ownership stageOwnership, purpose executor.Purpose, sequence int) executor.AttemptID {
	return executor.AttemptID{
		RunID: runID, Stage: ownership.name, Attempt: ownership.attempt,
		StartedAt: ownership.startedAt, Purpose: purpose, Sequence: sequence,
	}
}

func sandboxEnvironment() map[string]string {
	return map[string]string{
		"HOME": "/home/paje", "PATH": "/usr/local/bin:/usr/bin:/bin", "TMPDIR": "/tmp",
	}
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (s *Service) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := s.cleanupTimeout
	if timeout <= 0 {
		timeout = time.Second
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

func (target *fencedExecutor) check(ctx context.Context) bool {
	record, owned, err := target.service.checkExecuteOwnership(ctx, target.runID, target.ownership)
	if err == nil && owned {
		return true
	}
	*target.outcome = target.outcome.withOwnershipLost(record, err)
	return false
}

func (target *fencedExecutor) Execute(ctx context.Context, request executor.Request) (executor.Result, error) {
	if !target.check(ctx) {
		return executor.Result{}, ErrPhaseInProgress
	}
	result, executeErr := target.target.Execute(ctx, request)
	target.recordExecution(request.Attempt, result)
	if request.Attempt.Purpose == executor.PurposeAgent && result.Started {
		if err := target.service.checkpointAgentLifecycle(ctx, target.runID, target.ownership, result); err != nil {
			executeErr = errors.Join(executeErr, err)
			if errors.Is(err, ErrPhaseInProgress) {
				target.check(ctx)
			}
		}
	}
	if !target.check(ctx) {
		return result, errors.Join(executeErr, ErrPhaseInProgress)
	}
	return result, executeErr
}

func (target *fencedExecutor) Inspect(ctx context.Context, attempt executor.AttemptID) (executor.State, error) {
	if !target.check(ctx) {
		return executor.StateUnknown, ErrPhaseInProgress
	}
	state, err := target.target.Inspect(ctx, attempt)
	if !target.check(ctx) {
		return state, errors.Join(err, ErrPhaseInProgress)
	}
	return state, err
}

func (target *fencedExecutor) Cancel(ctx context.Context, attempt executor.AttemptID) error {
	preOwned := target.check(ctx)
	cancelCtx, cancel := target.service.cleanupContext(ctx)
	err := target.target.Cancel(cancelCtx, attempt)
	cancel()
	postOwned := target.check(ctx)
	target.markAttempt(attempt, func(evidence *artifact.AttemptEvidence) { evidence.Canceled = err == nil })
	if !preOwned || !postOwned {
		return errors.Join(err, ErrPhaseInProgress)
	}
	return err
}

func (target *fencedExecutor) Destroy(ctx context.Context, attempt executor.AttemptID) error {
	preOwned := target.check(ctx)
	destroyCtx, cancelDestroy := target.service.cleanupContext(ctx)
	destroyErr := target.target.Destroy(destroyCtx, attempt)
	cancelDestroy()
	postDestroyOwned := target.check(ctx)

	inspectCtx, cancelInspect := target.service.cleanupContext(ctx)
	state, inspectErr := target.Inspect(inspectCtx, attempt)
	cancelInspect()
	confirmed := destroyErr == nil && inspectErr == nil &&
		(state == executor.StateAbsent || state == executor.StateDestroyed)
	target.markAttempt(attempt, func(evidence *artifact.AttemptEvidence) { evidence.Destroyed = confirmed })
	var confirmationErr error
	if !confirmed {
		confirmationErr = errors.New("executor termination was not confirmed")
	}
	if !preOwned || !postDestroyOwned {
		return errors.Join(destroyErr, inspectErr, confirmationErr, ErrPhaseInProgress)
	}
	return errors.Join(destroyErr, inspectErr, confirmationErr)
}

func (target *fencedExecutor) runProbe(
	ctx context.Context,
	workspacePath string,
	profile workerprofile.Snapshot,
	command executor.Command,
	sequence int,
) (executor.Result, error, error) {
	attempt := executorAttempt(target.runID, target.ownership, executor.PurposeProbe, sequence)
	request := executor.Request{
		Attempt: attempt, Profile: profile.Clone(), Command: command.Clone(),
		Workspace:   executor.Workspace{HostPath: workspacePath, SandboxPath: executor.SandboxWorkspaceRoot},
		Environment: sandboxEnvironment(), Timeout: defaultSandboxTimeout, OutputLimit: defaultOutputLimit,
	}
	result, err := target.Execute(ctx, request)
	request.Destroy()
	destroyErr := target.Destroy(ctx, attempt)
	return result, err, destroyErr
}

func (target *fencedExecutor) recordExecution(attempt executor.AttemptID, result executor.Result) {
	target.markAttempt(attempt, func(evidence *artifact.AttemptEvidence) {
		evidence.Created = result.Created
		evidence.Started = result.Started
		evidence.Completed = result.Completed
		evidence.ExitCode = result.ExitCode
		evidence.Duration = result.Duration.Seconds()
		evidence.Truncated = result.StdoutTruncated || result.StderrTruncated
	})
}

func (target *fencedExecutor) markAttempt(attempt executor.AttemptID, update func(*artifact.AttemptEvidence)) {
	for index := range target.outcome.attemptEvidence {
		if target.outcome.attemptEvidence[index].ID.Key() == attempt.Key() {
			update(&target.outcome.attemptEvidence[index])
			return
		}
	}
	evidence := artifact.AttemptEvidence{ID: attempt}
	update(&evidence)
	target.outcome.attemptEvidence = append(target.outcome.attemptEvidence, evidence)
}

func (s *Service) checkpointAgentLifecycle(
	ctx context.Context,
	runID string,
	ownership stageOwnership,
	result executor.Result,
) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	_, err := s.mutate(persistCtx, runID, func(current run.Record) (run.Record, bool, error) {
		if !exactStageIdentity(current, ownership) {
			return current, false, ErrPhaseInProgress
		}
		next := run.CloneRecord(current)
		latest, _ := latestStage(next, "execute")
		if latest.Evidence == nil {
			latest.Evidence = make(map[string]string)
		}
		changed := false
		for key, present := range map[string]bool{
			"agent_created": result.Created, "agent_started": result.Started, "agent_completed": result.Completed,
		} {
			if present && latest.Evidence[key] != "true" {
				latest.Evidence[key] = "true"
				changed = true
			}
		}
		if !changed {
			return current, false, nil
		}
		var mutationErr error
		next, mutationErr = run.UpsertStage(next, latest)
		if mutationErr != nil {
			return run.Record{}, false, mutationErr
		}
		next.UpdatedAt = s.clock()
		return next, true, nil
	})
	return err
}

func probeSucceeded(result executor.Result, required string) bool {
	if !result.Created || !result.Started || !result.Completed || result.ExitCode != 0 || result.SecretDetected {
		return false
	}
	return required == "" || strings.Contains(string(result.Stdout)+string(result.Stderr), required)
}

func runtimeProbeMatches(profile workerprofile.Snapshot, facts map[string]string) bool {
	switch profile.Runtime.Kind {
	case workerprofile.RuntimeOCI:
		return facts["runtime_kind"] == workerprofile.RuntimeOCI &&
			facts["image"] == profile.Runtime.Image && facts["platform"] == profile.Runtime.Platform &&
			facts["isolated"] == "true"
	case workerprofile.RuntimeHost:
		return facts["runtime_kind"] == workerprofile.RuntimeHost &&
			facts["isolated"] == "false" && facts["certified"] == "false"
	default:
		return false
	}
}

func (s *Service) acquireAgentLeases(
	ctx context.Context,
	record run.Record,
	ownership stageOwnership,
	outcome *executeOutcome,
) ([]activeLease, error) {
	profile := record.WorkerProfile.Clone()
	leases := make([]activeLease, 0, len(profile.Secrets))
	deadline := ownership.startedAt.Add(s.executeLease)
	for index, requirement := range profile.Secrets {
		ownedRecord, owned, err := s.checkExecuteOwnership(ctx, record.ID, ownership)
		if err != nil || !owned {
			*outcome = outcome.withOwnershipLost(ownedRecord, err)
			return leases, errors.Join(ErrPhaseInProgress, err, s.revokeAgentLeases(ctx, record.ID, ownership, leases, outcome))
		}
		if index >= len(record.SecretBindings) {
			return leases, errors.Join(errors.New("resolved secret binding is missing"), s.revokeAgentLeases(ctx, record.ID, ownership, leases, outcome))
		}
		binding := record.SecretBindings[index]
		lease, acquireErr := s.secrets.Acquire(ctx, secret.AcquireRequest{
			RunID: record.ID, Attempt: ownership.attempt, StartedAt: ownership.startedAt,
			ProfileID: profile.Metadata, Capability: requirement.Capability,
			Binding: binding.Revision, Delivery: requirement, Deadline: deadline,
		})
		if acquireErr != nil {
			return leases, errors.Join(acquireErr, s.revokeAgentLeases(ctx, record.ID, ownership, leases, outcome))
		}
		materialization := lease.Materialization()
		leases = append(leases, activeLease{id: lease.ID(), materialization: materialization})
		lease.Destroy()
		ownedRecord, owned, err = s.checkExecuteOwnership(ctx, record.ID, ownership)
		if err != nil || !owned {
			*outcome = outcome.withOwnershipLost(ownedRecord, err)
			return leases, errors.Join(ErrPhaseInProgress, err, s.revokeAgentLeases(ctx, record.ID, ownership, leases, outcome))
		}
	}
	return leases, nil
}

func detectorForLeases(leases []activeLease) secret.Detector {
	materializations := make([]secret.Materialization, len(leases))
	for index := range leases {
		materializations[index] = leases[index].materialization.Clone()
	}
	detector := secret.NewDetector(materializations...)
	for index := range materializations {
		materializations[index].Destroy()
	}
	return detector
}

func leaseMaterializations(leases []activeLease) []secret.Materialization {
	materializations := make([]secret.Materialization, len(leases))
	for index := range leases {
		materializations[index] = leases[index].materialization.Clone()
	}
	return materializations
}

func (s *Service) cleanupAgent(
	ctx context.Context,
	target *fencedExecutor,
	leases []activeLease,
	_ bool,
) error {
	agentAttempt := executorAttempt(target.runID, target.ownership, executor.PurposeAgent, 0)
	destroyErr := target.Destroy(ctx, agentAttempt)
	revokeErr := s.revokeAgentLeases(ctx, target.runID, target.ownership, leases, target.outcome)
	return errors.Join(destroyErr, revokeErr)
}

func (s *Service) cancelAndCleanupAgent(
	ctx context.Context,
	target *fencedExecutor,
	leases []activeLease,
	attempt executor.AttemptID,
) (bool, error) {
	cancelErr := target.Cancel(ctx, attempt)
	cleanupErr := s.cleanupAgent(ctx, target, leases, false)
	return cleanupErr == nil, errors.Join(cancelErr, cleanupErr)
}

func (s *Service) revokeAgentLeases(
	ctx context.Context,
	runID string,
	ownership stageOwnership,
	leases []activeLease,
	outcome *executeOutcome,
) error {
	var revokeErrors []error
	for index := len(leases) - 1; index >= 0; index-- {
		if ownedRecord, owned, err := s.checkExecuteOwnership(ctx, runID, ownership); err != nil || !owned {
			*outcome = outcome.withOwnershipLost(ownedRecord, err)
		}
		revokeCtx, cancelRevoke := s.cleanupContext(ctx)
		err := s.secrets.Revoke(revokeCtx, leases[index].id)
		cancelRevoke()
		if err != nil {
			outcome.cleanupFailed = true
			revokeErrors = append(revokeErrors, err)
		}
		if ownedRecord, owned, err := s.checkExecuteOwnership(ctx, runID, ownership); err != nil || !owned {
			*outcome = outcome.withOwnershipLost(ownedRecord, err)
		}
		leases[index].materialization.Destroy()
	}
	return errors.Join(revokeErrors...)
}

func (s *Service) captureOwned(
	ctx context.Context,
	record run.Record,
	workspacePath string,
	ownership stageOwnership,
	outcome *executeOutcome,
) (gitcapture.Result, error) {
	if ownedRecord, owned, err := s.checkExecuteOwnership(ctx, record.ID, ownership); err != nil || !owned {
		*outcome = outcome.withOwnershipLost(ownedRecord, err)
		return gitcapture.Result{}, errors.Join(ErrPhaseInProgress, err)
	}
	result, err := s.capturer.Capture(ctx, gitcapture.Request{
		Workspace: workspacePath, BaseSHA: record.BaseSHA, MaxBytes: maxCaptureBytes,
	})
	if ownedRecord, owned, ownershipErr := s.checkExecuteOwnership(ctx, record.ID, ownership); ownershipErr != nil || !owned {
		*outcome = outcome.withOwnershipLost(ownedRecord, ownershipErr)
		return result, errors.Join(err, ErrPhaseInProgress, ownershipErr)
	}
	return result, err
}

func executorUnavailableFailure() run.Failure {
	return run.Failure{Stage: "execute", Class: run.FailureEnvironment, Retryable: false,
		Diagnostic: "worker runtime is unavailable", CauseCode: "executor_unavailable"}
}

func agentProtocolFailure() run.Failure {
	return run.Failure{Stage: "execute", Class: run.FailureAgent, Retryable: false,
		Diagnostic: "agent harness protocol failed", CauseCode: "agent_protocol"}
}

func secretAcquisitionFailure(ctx context.Context) run.Failure {
	if ctx.Err() != nil {
		return canceledFailure("execute")
	}
	return run.Failure{Stage: "execute", Class: run.FailureEnvironment, Retryable: true,
		Diagnostic: "required agent secret is unavailable", CauseCode: "secret_unavailable"}
}

func secretDetectedFailure() run.Failure {
	return run.Failure{Stage: "execute", Class: run.FailurePolicy, Retryable: false,
		Diagnostic: "execution output contains secret material", CauseCode: "secret_detected"}
}

func captureFailure(agentCompleted bool) run.Failure {
	return run.Failure{Stage: "execute", Class: run.FailureInternal, Retryable: !agentCompleted,
		Diagnostic: "capture change set failed", CauseCode: "capture_failed"}
}

func (outcome executeOutcome) withCleanupFailure(cause error) executeOutcome {
	if cause == nil {
		return outcome
	}
	outcome.cleanupFailed = true
	outcome.cause = errors.Join(outcome.cause, cause)
	return outcome.normalizeCleanupFailure()
}

func (outcome executeOutcome) normalizeCleanupFailure() executeOutcome {
	if !outcome.cleanupFailed {
		return outcome
	}
	diagnostic := "attempt cleanup failed"
	if outcome.failure != nil && outcome.failure.Diagnostic != "" &&
		outcome.failure.Class != run.FailureCleanup {
		diagnostic = outcome.failure.Diagnostic + "; " + diagnostic
	}
	failure := run.Failure{Stage: "execute", Class: run.FailureCleanup, Retryable: false,
		Diagnostic: run.SafeDiagnostic(diagnostic), CauseCode: "cleanup_failed"}
	outcome.failure = &failure
	return outcome
}

func imageDigest(image string) string {
	const marker = "@sha256:"
	index := strings.LastIndex(image, marker)
	if index < 0 {
		return ""
	}
	return image[index+len(marker):]
}

func buildBundle(record run.Record, outcome executeOutcome) (artifact.Bundle, error) {
	runtimeEvidence := artifact.RuntimeEvidence{
		Kind: outcome.profileSnapshot.Runtime.Kind, Isolated: outcome.runtimeIsolated, Certified: outcome.runtimeCertified,
	}
	if outcome.profileSnapshot.Runtime.Kind == workerprofile.RuntimeOCI {
		runtimeEvidence.ImageDigest = imageDigest(outcome.profileSnapshot.Runtime.Image)
		runtimeEvidence.Platform = outcome.profileSnapshot.Runtime.Platform
	}
	tools := make(artifact.ToolEvidenceList, len(outcome.toolEvidence))
	copy(tools, outcome.toolEvidence)
	attempts := make(artifact.AttemptEvidenceList, len(outcome.attemptEvidence))
	copy(attempts, outcome.attemptEvidence)
	agentKeys := artifact.EnvironmentKeyList(sortedMapKeys(sandboxEnvironment()))
	verificationKeys := artifact.EnvironmentKeyList(sortedMapKeys(sandboxEnvironment()))
	executionMetadata, err := json.Marshal(artifact.ExecutionEvidence{
		ExitCode: outcome.execution.ExitCode, Duration: outcome.execution.Duration.Seconds(),
		Started: outcome.execution.Started, Completed: outcome.execution.Completed,
		Truncated: outcome.execution.StdoutTruncated || outcome.execution.StderrTruncated,
		Profile: &artifact.WorkerProfileEvidence{Name: outcome.profileSnapshot.Metadata.Name,
			Revision: outcome.profileSnapshot.Metadata.Revision, Digest: outcome.profileSnapshot.Digest},
		Runtime: &runtimeEvidence, Harness: &outcome.harnessEvidence,
		Tools:                       &tools,
		Attempts:                    &attempts,
		AgentEnvironmentKeys:        &agentKeys,
		VerificationEnvironmentKeys: &verificationKeys,
	})
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
		AgentOutput:       []byte(outcome.scrubber.string(outcome.agentOutput)),
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
		relative := filepath.ToSlash(filepath.Clean(directory))
		if relative == "." {
			return relative, nil
		}
		if relative == ".." || strings.HasPrefix(relative, "../") || path.Clean(relative) != relative {
			return "", fmt.Errorf("durable verification directory escapes workspace")
		}
		return relative, nil
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

func (s *Service) cleanupAttempt(ctx context.Context, prepared workspace.Workspace) error {
	workspaceCtx, cancelWorkspace := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	workspaceErr := prepared.Cleanup(workspaceCtx)
	cancelWorkspace()
	return workspaceErr
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
	outcome = outcome.normalizeCleanupFailure()
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
	if ctx.Err() != nil && !outcome.cleanupFailed && (outcome.failure == nil ||
		outcome.failure.CauseCode != "ambiguous_attempt" && outcome.failure.Class != run.FailureCleanup) {
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
	return outcome.normalizeCleanupFailure()
}

func (outcome executeOutcome) withCancellation(cause error) executeOutcome {
	if cause == nil {
		return outcome
	}
	if outcome.cleanupFailed || outcome.failure != nil &&
		(outcome.failure.CauseCode == "ambiguous_attempt" || outcome.failure.Class == run.FailureCleanup) {
		outcome.cause = errors.Join(outcome.cause, cause)
		return outcome.normalizeCleanupFailure()
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
