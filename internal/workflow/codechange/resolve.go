package codechange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/run"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

var immutableRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Resolve validates and freezes all context needed by later phase retries.
func (s *Service) Resolve(ctx context.Context, raw json.RawMessage) (PhaseResult, error) {
	return s.resolve(ctx, s.newID(), raw, false)
}

// ResolveWithRunID validates and freezes a caller-owned run using a
// preallocated durable identity. A different owner cannot attach to an
// existing idempotency binding, even when the same provider-neutral input
// would otherwise resume it.
func (s *Service) ResolveWithRunID(ctx context.Context, runID string, raw json.RawMessage) (PhaseResult, error) {
	if strings.TrimSpace(runID) == "" {
		return PhaseResult{}, fmt.Errorf("resolve code-change input: run ID is required")
	}
	return s.resolve(ctx, runID, raw, true)
}

func (s *Service) resolve(
	ctx context.Context,
	runID string,
	raw json.RawMessage,
	requireOwner bool,
) (PhaseResult, error) {
	input, canonical, inputHash, err := s.decodeInput(raw)
	if err != nil {
		return PhaseResult{}, err
	}
	record, created, err := s.runs.Reserve(ctx, run.Reservation{
		NewRunID: runID, Template: templatecodechange.ID,
		IdempotencyKey: input.IdempotencyKey, InputHash: inputHash,
		Input: canonical, RepositoryURI: input.RepositoryURI,
		BaseRef: input.BaseRef, PublicationMode: input.Publication.Mode,
		CreatedAt: s.clock(),
	})
	if err != nil {
		return phaseResult(record), err
	}
	if requireOwner && !created && record.ID != runID {
		return PhaseResult{}, fmt.Errorf(
			"resolve code-change input: durable run %q is owned by another trigger: %w",
			record.ID,
			run.ErrIdempotencyConflict,
		)
	}
	if !created && (record.BaseSHA != "" || record.Terminal()) {
		return phaseResult(record), nil
	}
	return s.resolveReserved(ctx, record.ID, input)
}

func (s *Service) decodeInput(raw json.RawMessage) (templatecodechange.Input, json.RawMessage, string, error) {
	definition, err := s.templates.Resolve(templatecodechange.ID)
	if err != nil {
		return templatecodechange.Input{}, nil, "", err
	}
	if err := definition.Validate(raw); err != nil {
		return templatecodechange.Input{}, nil, "", err
	}
	input, err := templatecodechange.Decode(raw)
	if err != nil {
		return templatecodechange.Input{}, nil, "", err
	}
	if input.Publication.Mode == "pull_request" {
		input.RepositoryURI, err = canonicalGitHubRepository(input.RepositoryURI)
		if err != nil {
			return templatecodechange.Input{}, nil, "", fmt.Errorf("resolve code-change input: %w", err)
		}
	}
	if _, ok := s.profiles[input.Profile]; !ok {
		return templatecodechange.Input{}, nil, "", fmt.Errorf("resolve code-change input: profile %q is unavailable", input.Profile)
	}
	canonical, err := canonicalInput(input)
	if err != nil {
		return templatecodechange.Input{}, nil, "", fmt.Errorf("canonicalize code-change input: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return input, canonical, hex.EncodeToString(sum[:]), nil
}

func canonicalInput(input templatecodechange.Input) (json.RawMessage, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	memoryLimit, err := json.Marshal(input.MemoryLimit)
	if err != nil {
		return nil, err
	}
	fields["memory_limit"] = memoryLimit
	encoded, err = json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return run.CanonicalInput(encoded)
}

func (s *Service) resolveReserved(ctx context.Context, runID string, input templatecodechange.Input) (PhaseResult, error) {
	record, started, err := s.beginStage(ctx, runID, "resolve", run.StatusResolving)
	if err != nil {
		return phaseResult(record), err
	}
	if !started {
		if record.BaseSHA != "" || record.Terminal() {
			return phaseResult(record), nil
		}
		return phaseInProgress(record)
	}
	if record.BaseSHA != "" || record.Terminal() {
		return phaseResult(record), nil
	}
	attempt := stageAttempt(record, "resolve")

	revision, err := s.resolver.Resolve(ctx, input.RepositoryURI, input.BaseRef)
	if err != nil {
		failure := run.Failure{
			Stage: "resolve", Class: run.FailureEnvironment, Retryable: true,
			Diagnostic: "repository revision is temporarily unavailable", CauseCode: "repository_unavailable",
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			failure = canceledFailure("resolve")
		}
		return s.finishFailure(ctx, runID, failure, err, attempt)
	}
	if revision.SourceDirty {
		failure := run.Failure{
			Stage: "resolve", Class: run.FailureInput, Retryable: false,
			Diagnostic: "local repository source is dirty", CauseCode: "source_dirty",
		}
		return s.finishFailure(ctx, runID, failure, errors.New("local repository source is dirty"), attempt)
	}
	if revision.RepositoryURI != input.RepositoryURI ||
		revision.Ref != input.BaseRef ||
		!immutableRevisionPattern.MatchString(revision.SHA) {
		failure := run.Failure{
			Stage: "resolve", Class: run.FailureInternal, Retryable: false,
			Diagnostic: "repository resolver returned an invalid immutable revision", CauseCode: "invalid_revision",
		}
		return s.finishFailure(ctx, runID, failure, errors.New("repository resolver returned an invalid revision"), attempt)
	}

	capabilityEvidence, capabilityFailure, capabilityErr := s.validateResolveCapabilities(ctx, runID, attempt, input)
	if capabilityFailure != nil {
		return s.finishFailure(ctx, runID, *capabilityFailure, capabilityErr, attempt)
	}

	query := input.MemoryQuery
	if query == "" {
		query = input.TaskDescription
	}
	memories := []memory.Memory{}
	if input.MemoryLimit > 0 {
		memories, err = s.memory.Search(ctx, query, input.MemoryLimit, cloneStringMap(input.Tags))
		if err != nil {
			failure := run.Failure{
				Stage: "resolve", Class: run.FailureInternal, Retryable: true,
				Diagnostic: "scoped memory is temporarily unavailable", CauseCode: "memory_unavailable",
			}
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				failure = canceledFailure("resolve")
			}
			return s.finishFailure(ctx, runID, failure, err, attempt)
		}
	}

	record, err = s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.BaseSHA != "" || current.Terminal() {
			return current, false, nil
		}
		if !runningStageAttempt(current, "resolve", attempt) {
			return current, false, nil
		}
		next := run.CloneRecord(current)
		next.BaseSHA = revision.SHA
		next.MemorySnapshot = cloneMemories(memories)
		next.Failure = nil
		finished := run.StageResult{
			Name: "resolve", Status: run.StageSucceeded,
			StartedAt:  latestStageStart(current, "resolve"),
			FinishedAt: s.clock(), Attempts: stageAttempt(current, "resolve"),
			Evidence: map[string]string{
				"base_sha": revision.SHA, "memory_count": fmt.Sprintf("%d", len(memories)),
			},
		}
		for key, value := range capabilityEvidence {
			finished.Evidence[key] = value
		}
		next, err := run.UpsertStage(next, finished)
		if err != nil {
			return run.Record{}, false, err
		}
		next, err = run.Transition(next, run.StatusExecuting, s.clock())
		return next, true, err
	})
	if err != nil {
		return phaseResult(record), err
	}
	if record.BaseSHA == "" && !record.Terminal() {
		return phaseInProgress(record)
	}
	return phaseResult(record), nil
}

func (s *Service) validateResolveCapabilities(
	ctx context.Context,
	runID string,
	attempt int,
	input templatecodechange.Input,
) (map[string]string, *run.Failure, error) {
	evidence := make(map[string]string)
	var primary error
	runtimeID := resolveCapabilityRuntimeID(runID, attempt)
	for _, stage := range []environment.Stage{environment.StageAgent, environment.StageVerification} {
		result, err := s.environments.Build(ctx, environment.Request{
			RunID: runtimeID, Stage: stage, RequestedKeys: append([]string(nil), input.EnvironmentKeys...),
		})
		if err != nil {
			primary = err
			break
		}
		evidence["capability_"+string(stage)+"_keys"] = encodeStrings(result.Keys)
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	cleanupErr := s.environments.Cleanup(cleanupCtx, runtimeID)
	cancel()
	if ctx.Err() != nil || errors.Is(primary, context.Canceled) {
		failure := canceledFailure("resolve")
		return nil, &failure, errors.Join(primary, cleanupErr, ctx.Err())
	}
	if primary != nil {
		failure := environmentPolicyFailure(ctx, "resolve")
		if cleanupErr != nil {
			failure.Retryable = false
			failure.Diagnostic = run.SafeDiagnostic(failure.Diagnostic + "; capability cleanup failed")
		}
		return nil, &failure, errors.Join(primary, cleanupErr)
	}
	if cleanupErr != nil {
		failure := run.Failure{
			Stage: "resolve", Class: run.FailureCleanup, Retryable: false,
			Diagnostic: "capability validation cleanup failed", CauseCode: "cleanup_failed",
		}
		return nil, &failure, cleanupErr
	}
	return evidence, nil, nil
}

func resolveCapabilityRuntimeID(runID string, attempt int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", runID, attempt)))
	return "resolve-" + hex.EncodeToString(sum[:16])
}

func (s *Service) beginStage(ctx context.Context, runID, name string, status run.Status) (run.Record, bool, error) {
	started := false
	record, err := s.mutate(ctx, runID, func(current run.Record) (run.Record, bool, error) {
		started = false
		if current.Terminal() {
			return current, false, nil
		}
		if name == "resolve" && current.BaseSHA != "" {
			return current, false, nil
		}
		next := run.CloneRecord(current)
		if latest, found := latestStage(current, name); found && latest.Status == run.StageRunning {
			lease := s.resolveLease
			if name == "execute" {
				lease = s.executeLease
			}
			if !stageExpired(latest, s.clock(), lease) ||
				(name == "execute" && activeArtifactWriteLease(
					current,
					stageOwnership{name: name, attempt: latest.Attempts, startedAt: latest.StartedAt},
					s.clock(),
				)) {
				return current, false, nil
			}
			if name == "execute" {
				failure := run.Failure{
					Stage: name, Class: run.FailureInternal, Retryable: false,
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
			lost := run.Failure{
				Stage: name, Class: run.FailureInternal, Retryable: true,
				Diagnostic: "worker lease expired before resolve completed", CauseCode: "worker_lost",
			}
			latest.Status = run.StageFailed
			latest.FinishedAt = s.clock()
			latest.Failure = &lost
			var mutationErr error
			next, mutationErr = run.UpsertStage(next, latest)
			if mutationErr != nil {
				return run.Record{}, false, mutationErr
			}
		}
		if name == "execute" && current.Artifact != nil {
			return current, false, nil
		}
		var mutationErr error
		if next.Status != status {
			next, mutationErr = run.Transition(next, status, s.clock())
			if mutationErr != nil {
				return run.Record{}, false, mutationErr
			}
		}
		next.Failure = nil
		stage := run.StageResult{
			Name: name, Status: run.StageRunning, StartedAt: s.clock(),
			Attempts: stageAttempt(next, name) + 1,
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

func stageExpired(stage run.StageResult, now time.Time, lease time.Duration) bool {
	return lease > 0 && !now.Before(stage.StartedAt.Add(lease))
}

func (s *Service) finishFailure(
	ctx context.Context,
	runID string,
	failure run.Failure,
	cause error,
	expectedAttempt ...int,
) (PhaseResult, error) {
	persistCtx := ctx
	cancel := func() {}
	if ctx.Err() != nil {
		persistCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	}
	defer cancel()
	record, err := s.mutate(persistCtx, runID, func(current run.Record) (run.Record, bool, error) {
		if current.Terminal() {
			return current, false, nil
		}
		if len(expectedAttempt) != 0 &&
			!runningStageAttempt(current, failure.Stage, expectedAttempt[0]) {
			return current, false, nil
		}
		next := run.CloneRecord(current)
		failureCopy := failure
		next.Failure = &failureCopy
		stage := run.StageResult{
			Name: failure.Stage, Status: run.StageFailed,
			StartedAt:  latestStageStart(current, failure.Stage),
			FinishedAt: s.clock(), Attempts: stageAttempt(current, failure.Stage),
			Failure: &failureCopy,
		}
		var upsertErr error
		next, upsertErr = run.UpsertStage(next, stage)
		if upsertErr != nil {
			return run.Record{}, false, upsertErr
		}
		next.UpdatedAt = s.clock()
		if !failure.Retryable {
			status := run.StatusFailed
			if failure.Class == run.FailureCanceled {
				status = run.StatusCanceled
			}
			next, upsertErr = run.Transition(next, status, s.clock())
			if upsertErr != nil {
				return run.Record{}, false, upsertErr
			}
		}
		return next, true, nil
	})
	if err != nil {
		return phaseResult(record), errors.Join(newPhaseError(failure, cause), err)
	}
	if len(expectedAttempt) != 0 &&
		!finishedStageAttempt(record, failure.Stage, expectedAttempt[0]) {
		if record.Terminal() || record.BaseSHA != "" {
			return phaseResult(record), nil
		}
		return phaseInProgress(record)
	}
	return phaseResult(record), newPhaseError(failure, cause)
}

func runningStageAttempt(record run.Record, name string, attempt int) bool {
	latest, found := latestStage(record, name)
	return found && latest.Attempts == attempt && latest.Status == run.StageRunning
}

func finishedStageAttempt(record run.Record, name string, attempt int) bool {
	latest, found := latestStage(record, name)
	return found && latest.Attempts == attempt && latest.Status != run.StageRunning
}

func latestStageStart(record run.Record, name string) time.Time {
	var started time.Time
	attempt := 0
	for _, stage := range record.Stages {
		if stage.Name == name && stage.Attempts >= attempt {
			attempt = stage.Attempts
			started = stage.StartedAt
		}
	}
	if started.IsZero() {
		started = record.UpdatedAt
	}
	return started
}
