// Package codechange coordinates the provider-neutral code-change@v1 phases.
package codechange

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/harness"
	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/policy"
	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/run"
	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workerprofile"
	"github.com/araihu/paje/internal/workspace"
)

const (
	maxCASAttempts        = 3
	maxAgentPromptBytes   = 1 << 20
	maxCaptureBytes       = 10 << 20
	defaultCleanupTimeout = 30 * time.Second
	defaultResolveLease   = 5 * time.Minute
	defaultExecuteLease   = 35 * time.Minute
	defaultArtifactSave   = 2 * time.Minute
)

// ErrPhaseInProgress asks an outer adapter to retry rather than launch a
// duplicate side-effecting attempt.
var ErrPhaseInProgress = errors.New("workflow phase already in progress")

// ErrRunBinding indicates durable duplicated identity fields no longer agree.
var ErrRunBinding = errors.New("durable run binding mismatch")

// Dependencies is the complete provider-neutral port bundle used by the
// code-change service.
type Dependencies struct {
	Templates      *template.Registry
	Runs           run.Store
	Memory         memory.Store
	Resolver       repository.Resolver
	Workspaces     workspace.Manager
	Profiles       map[string]repository.Profile
	WorkerProfiles workerprofile.Registry
	SecretBindings secret.Registry
	Secrets        secret.Broker
	Executors      *executor.Registry
	Harnesses      *harness.Registry
	Environments   environment.Builder
	Agent          runner.Runner
	Verifier       verification.Runner
	Capturer       gitcapture.Capturer
	Policy         policy.Evaluator
	Artifacts      artifact.Store
	Publisher      publisher.Publisher
	Clock          func() time.Time
	NewID          func() string
}

// PhaseResult contains only durable values safe to pass between adapters.
type PhaseResult struct {
	RunID        string              `json:"run_id"`
	Status       run.Status          `json:"status"`
	Artifact     *artifact.Reference `json:"artifact,omitempty"`
	FailureClass run.FailureClass    `json:"failure_class,omitempty"`
	Retryable    bool                `json:"retryable"`
}

// Service implements the provider-neutral workflow phases.
type Service struct {
	templates           *template.Registry
	runs                run.Store
	memory              memory.Store
	resolver            repository.Resolver
	workspaces          workspace.Manager
	profiles            map[string]repository.Profile
	workerProfiles      workerprofile.Registry
	secretBindings      secret.Registry
	secrets             secret.Broker
	executors           *executor.Registry
	harnesses           *harness.Registry
	environments        environment.Builder
	agent               runner.Runner
	verifier            verification.Runner
	capturer            gitcapture.Capturer
	policy              policy.Evaluator
	artifacts           artifact.Store
	publisher           publisher.Publisher
	clock               func() time.Time
	newID               func() string
	cleanupTimeout      time.Duration
	resolveLease        time.Duration
	executeLease        time.Duration
	artifactSaveTimeout time.Duration
	finalizeLocks       *keyedMutex
}

// New validates and snapshots the workflow dependency bundle.
func New(dependencies Dependencies) (*Service, error) {
	required := []struct {
		name  string
		value any
	}{
		{"template registry", dependencies.Templates},
		{"run store", dependencies.Runs},
		{"memory store", dependencies.Memory},
		{"repository resolver", dependencies.Resolver},
		{"workspace manager", dependencies.Workspaces},
		{"worker profile registry", dependencies.WorkerProfiles},
		{"secret binding registry", dependencies.SecretBindings},
		{"secret broker", dependencies.Secrets},
		{"executor registry", dependencies.Executors},
		{"harness registry", dependencies.Harnesses},
		{"environment builder", dependencies.Environments},
		{"agent runner", dependencies.Agent},
		{"verification runner", dependencies.Verifier},
		{"Git capturer", dependencies.Capturer},
		{"change policy", dependencies.Policy},
		{"artifact store", dependencies.Artifacts},
		{"publisher", dependencies.Publisher},
	}
	for _, dependency := range required {
		if isNil(dependency.value) {
			return nil, fmt.Errorf("create code-change service: %s is required", dependency.name)
		}
	}
	if dependencies.Clock == nil {
		return nil, fmt.Errorf("create code-change service: clock is required")
	}
	if dependencies.NewID == nil {
		return nil, fmt.Errorf("create code-change service: ID generator is required")
	}
	if _, err := dependencies.Templates.Resolve(templatecodechange.ID); err != nil {
		return nil, fmt.Errorf("create code-change service: built-in template: %w", err)
	}

	profiles := make(map[string]repository.Profile, len(dependencies.Profiles))
	for _, name := range []string{"generic", "go"} {
		profile, ok := dependencies.Profiles[name]
		if !ok || isNil(profile) || profile.Name() != name {
			return nil, fmt.Errorf("create code-change service: profile %q is required", name)
		}
		profiles[name] = profile
	}
	if len(dependencies.Profiles) != len(profiles) {
		for name, profile := range dependencies.Profiles {
			if _, supported := profiles[name]; !supported || isNil(profile) || profile.Name() != name {
				return nil, fmt.Errorf("create code-change service: invalid profile %q", name)
			}
		}
	}

	return &Service{
		templates: dependencies.Templates, runs: dependencies.Runs,
		memory: dependencies.Memory, resolver: dependencies.Resolver,
		workspaces: dependencies.Workspaces, profiles: profiles,
		workerProfiles: dependencies.WorkerProfiles,
		secretBindings: dependencies.SecretBindings,
		secrets:        dependencies.Secrets,
		executors:      dependencies.Executors, harnesses: dependencies.Harnesses,
		environments: dependencies.Environments, agent: dependencies.Agent,
		verifier: dependencies.Verifier, capturer: dependencies.Capturer,
		policy: dependencies.Policy, artifacts: dependencies.Artifacts,
		publisher: dependencies.Publisher, clock: dependencies.Clock,
		newID: dependencies.NewID, cleanupTimeout: defaultCleanupTimeout,
		resolveLease: defaultResolveLease, executeLease: defaultExecuteLease,
		artifactSaveTimeout: defaultArtifactSave,
		finalizeLocks:       &keyedMutex{},
	}, nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflection := reflect.ValueOf(value)
	switch reflection.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflection.IsNil()
	default:
		return false
	}
}

func phaseResult(record run.Record) PhaseResult {
	result := PhaseResult{RunID: record.ID, Status: record.Status}
	if record.Artifact != nil {
		reference := *record.Artifact
		result.Artifact = &reference
	}
	if record.Failure != nil {
		result.FailureClass = record.Failure.Class
		result.Retryable = record.Failure.Retryable
	}
	return result
}

func phaseInProgress(record run.Record) (PhaseResult, error) {
	result := phaseResult(record)
	result.FailureClass = run.FailureInternal
	result.Retryable = true
	return result, ErrPhaseInProgress
}

type phaseError struct {
	failure run.Failure
	cause   error
}

func (e *phaseError) Error() string {
	return fmt.Sprintf("%s phase failed (%s): %s", e.failure.Stage, e.failure.CauseCode, e.failure.Diagnostic)
}

func (e *phaseError) Unwrap() error { return e.cause }

func newPhaseError(failure run.Failure, cause error) error {
	return &phaseError{failure: failure, cause: cause}
}

type recordMutation func(run.Record) (run.Record, bool, error)

func (s *Service) mutate(ctx context.Context, runID string, mutation recordMutation) (run.Record, error) {
	var last error
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		current, err := s.runs.Load(ctx, runID)
		if err != nil {
			return run.Record{}, err
		}
		next, changed, err := mutation(current)
		if err != nil {
			return run.Record{}, err
		}
		if !changed {
			return current, nil
		}
		saved, err := s.runs.Save(ctx, next, current.Version)
		if err == nil {
			return saved, nil
		}
		if !errors.Is(err, run.ErrVersionConflict) {
			return run.Record{}, err
		}
		last = err
	}
	return run.Record{}, fmt.Errorf("persist run after %d CAS attempts: %w", maxCASAttempts, last)
}

func stageAttempt(record run.Record, name string) int {
	attempt := 0
	for _, stage := range record.Stages {
		if stage.Name == name && stage.Attempts > attempt {
			attempt = stage.Attempts
		}
	}
	return attempt
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func cloneMemories(source []memory.Memory) []memory.Memory {
	if source == nil {
		return []memory.Memory{}
	}
	cloned := make([]memory.Memory, len(source))
	for index, item := range source {
		cloned[index] = item
		cloned[index].Metadata = cloneStringMap(item.Metadata)
	}
	return cloned
}

func canceledFailure(stage string) run.Failure {
	return run.Failure{
		Stage: stage, Class: run.FailureCanceled, Retryable: false,
		Diagnostic: "caller canceled", CauseCode: "caller_canceled",
	}
}

func validateRunBinding(record run.Record) (templatecodechange.Input, error) {
	if record.Template != templatecodechange.ID {
		return templatecodechange.Input{}, fmt.Errorf("%w: template", ErrRunBinding)
	}
	input, err := templatecodechange.Decode(record.Input)
	if err != nil {
		return templatecodechange.Input{}, fmt.Errorf("%w: decode input", ErrRunBinding)
	}
	canonical, err := canonicalInput(input)
	if err != nil || !bytes.Equal(canonical, record.Input) {
		return templatecodechange.Input{}, fmt.Errorf("%w: canonical input", ErrRunBinding)
	}
	sum := sha256.Sum256(canonical)
	if record.InputHash != hex.EncodeToString(sum[:]) ||
		record.IdempotencyKey != input.IdempotencyKey ||
		record.RepositoryURI != input.RepositoryURI ||
		record.BaseRef != input.BaseRef ||
		record.PublicationMode != input.Publication.Mode {
		return templatecodechange.Input{}, fmt.Errorf("%w: immutable fields", ErrRunBinding)
	}
	if input.Publication.Mode == "pull_request" {
		canonical, err := canonicalGitHubRepository(input.RepositoryURI)
		if err != nil || canonical != input.RepositoryURI {
			return templatecodechange.Input{}, fmt.Errorf("%w: publication repository", ErrRunBinding)
		}
	}
	profileID, err := workerprofile.ParseProfileID(input.WorkerProfile)
	if err != nil {
		return templatecodechange.Input{}, fmt.Errorf("%w: worker profile input", ErrRunBinding)
	}
	if record.WorkerProfile == nil {
		if record.SecretBindings != nil || record.BaseSHA != "" {
			return templatecodechange.Input{}, fmt.Errorf("%w: unresolved worker profile", ErrRunBinding)
		}
		return input, nil
	}
	profile, err := workerprofile.Canonicalize(record.WorkerProfile.Clone())
	if err != nil || !reflect.DeepEqual(profile, *record.WorkerProfile) ||
		profile.Metadata != profileID || len(profile.Secrets) != len(record.SecretBindings) {
		return templatecodechange.Input{}, fmt.Errorf("%w: resolved worker profile", ErrRunBinding)
	}
	for index, requirement := range profile.Secrets {
		binding := record.SecretBindings[index]
		if binding.Capability != requirement.Capability ||
			binding.Revision != requirement.BindingRevision {
			return templatecodechange.Input{}, fmt.Errorf("%w: resolved secret binding", ErrRunBinding)
		}
	}
	return input, nil
}
