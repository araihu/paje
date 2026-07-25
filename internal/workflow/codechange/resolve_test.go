package codechange

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	artifactmock "github.com/araihu/paje/internal/artifact/mock"
	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/policy"
	"github.com/araihu/paje/internal/publisher"
	publishermock "github.com/araihu/paje/internal/publisher/mock"
	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/run"
	runmock "github.com/araihu/paje/internal/run/mock"
	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workspace"
)

func TestResolveIsCanonicalIdempotentAndFreezesMemory(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.mem.result = []memory.Memory{{
		ID: "memory-1", Content: "Preserve compatibility",
		Metadata: map[string]string{"source": "prior-run"},
	}}
	raw := validRawInput("Change docs", "same-key")

	first, err := fixture.service.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve(first) error = %v", err)
	}
	second, err := fixture.service.Resolve(context.Background(), append([]byte("\n  "), raw...))
	if err != nil {
		t.Fatalf("Resolve(second) error = %v", err)
	}
	if first.RunID != "run-123" || second.RunID != first.RunID {
		t.Fatalf("Resolve() run IDs = %q, %q", first.RunID, second.RunID)
	}
	if first.Status != run.StatusExecuting || second.Status != run.StatusExecuting {
		t.Fatalf("Resolve() statuses = %q, %q", first.Status, second.Status)
	}
	if fixture.resolver.calls != 1 || fixture.mem.calls != 1 {
		t.Fatalf("external calls resolver=%d memory=%d, want 1 each", fixture.resolver.calls, fixture.mem.calls)
	}

	record, err := fixture.runs.Load(context.Background(), first.RunID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if record.BaseSHA != fixture.resolver.revision.SHA {
		t.Fatalf("BaseSHA = %q", record.BaseSHA)
	}
	if len(record.MemorySnapshot) != 1 ||
		record.MemorySnapshot[0].ID != "memory-1" ||
		record.MemorySnapshot[0].Content != "Preserve compatibility" ||
		record.MemorySnapshot[0].Metadata["source"] != "prior-run" {
		t.Fatalf("MemorySnapshot = %#v", record.MemorySnapshot)
	}
	fixture.mem.result[0].Content = "mutated"
	fixture.mem.result[0].Metadata["source"] = "mutated"
	reloaded, _ := fixture.runs.Load(context.Background(), first.RunID)
	if reloaded.MemorySnapshot[0].Content != "Preserve compatibility" ||
		reloaded.MemorySnapshot[0].Metadata["source"] != "prior-run" {
		t.Fatalf("stored memory was not deep-frozen: %#v", reloaded.MemorySnapshot)
	}
	sum := sha256.Sum256(record.Input)
	if record.InputHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("InputHash = %q, want SHA-256 of canonical input", record.InputHash)
	}
	var normalized templatecodechange.Input
	if err := json.Unmarshal(record.Input, &normalized); err != nil {
		t.Fatalf("stored input is not JSON: %v", err)
	}
	wantJSON, _ := json.Marshal(normalized)
	wantCanonical, _ := run.CanonicalInput(wantJSON)
	if !bytes.Equal(record.Input, wantCanonical) {
		t.Fatalf("stored input = %s, want canonical %s", record.Input, wantCanonical)
	}

	_, err = fixture.service.Resolve(context.Background(), validRawInput("Different task", "same-key"))
	if !errors.Is(err, run.ErrIdempotencyConflict) {
		t.Fatalf("Resolve(conflict) error = %v, want %v", err, run.ErrIdempotencyConflict)
	}
	if fixture.resolver.calls != 1 || fixture.mem.calls != 1 {
		t.Fatalf("conflict reached external ports: resolver=%d memory=%d", fixture.resolver.calls, fixture.mem.calls)
	}
}

func TestResolveWithRunIDEnforcesSingleDurableOwner(t *testing.T) {
	fixture := newServiceFixture(t)
	raw := validRawInput("Change docs", "same-key")

	owner, err := fixture.service.ResolveWithRunID(context.Background(), "run-owner", raw)
	if err != nil {
		t.Fatalf("ResolveWithRunID(owner) error = %v", err)
	}
	if owner.RunID != "run-owner" {
		t.Fatalf("owner run ID = %q", owner.RunID)
	}
	resumed, err := fixture.service.ResolveWithRunID(context.Background(), "run-owner", raw)
	if err != nil || resumed.RunID != owner.RunID {
		t.Fatalf("ResolveWithRunID(resume) result=%#v error=%v", resumed, err)
	}
	observer, err := fixture.service.ResolveWithRunID(context.Background(), "run-observer", raw)
	if !errors.Is(err, run.ErrIdempotencyConflict) {
		t.Fatalf("ResolveWithRunID(observer) result=%#v error=%v, want %v", observer, err, run.ErrIdempotencyConflict)
	}
	if observer.RunID != "run-owner" {
		t.Fatalf("observer conflict run ID = %q, want owner", observer.RunID)
	}
	if fixture.resolver.calls != 1 || fixture.mem.calls != 1 {
		t.Fatalf("observer reached external ports: resolver=%d memory=%d", fixture.resolver.calls, fixture.mem.calls)
	}
}

func TestResolveWithRunIDPreservesBlankTemplateIdempotency(t *testing.T) {
	fixture := newServiceFixture(t)
	raw := validRawInput("Change docs", "")

	first, err := fixture.service.ResolveWithRunID(context.Background(), "run-first", raw)
	if err != nil {
		t.Fatalf("ResolveWithRunID(first) error = %v", err)
	}
	retried, err := fixture.service.ResolveWithRunID(context.Background(), "run-first", raw)
	if err != nil {
		t.Fatalf("ResolveWithRunID(retry first) error = %v", err)
	}
	second, err := fixture.service.ResolveWithRunID(context.Background(), "run-second", raw)
	if err != nil {
		t.Fatalf("ResolveWithRunID(second) error = %v", err)
	}
	if first.RunID != "run-first" || retried.RunID != first.RunID || second.RunID != "run-second" {
		t.Fatalf("blank-key run IDs = %q, %q, %q", first.RunID, retried.RunID, second.RunID)
	}
}

func TestResolveWithRunIDRejectsBlankOwnerBeforePorts(t *testing.T) {
	fixture := newServiceFixture(t)
	if _, err := fixture.service.ResolveWithRunID(context.Background(), "  ", validRawInput("Change docs", "key")); err == nil {
		t.Fatal("ResolveWithRunID(blank) error = nil")
	}
	if fixture.runs.reserveCalls != 0 || fixture.resolver.calls != 0 || fixture.mem.calls != 0 {
		t.Fatalf("blank owner reached ports: reserve=%d resolver=%d memory=%d", fixture.runs.reserveCalls, fixture.resolver.calls, fixture.mem.calls)
	}
}

func TestResolveCanonicalInputPreservesExplicitZeroMemoryLimit(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.mem.result = []memory.Memory{{ID: "must-not-load", Content: "not requested"}}
	var value map[string]any
	if err := json.Unmarshal(validRawInput("Change docs", "zero-memory"), &value); err != nil {
		t.Fatal(err)
	}
	value["memory_limit"] = 0
	raw, _ := json.Marshal(value)
	result, err := fixture.service.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	record, _ := fixture.runs.Load(context.Background(), result.RunID)
	if !bytes.Contains(record.Input, []byte(`"memory_limit":0`)) {
		t.Fatalf("canonical input lost explicit zero memory limit: %s", record.Input)
	}
	if fixture.mem.calls != 0 {
		t.Fatalf("Memory.Search() calls = %d, want 0", fixture.mem.calls)
	}
	if record.MemorySnapshot == nil || len(record.MemorySnapshot) != 0 {
		t.Fatalf("MemorySnapshot = %#v, want frozen empty snapshot", record.MemorySnapshot)
	}
}

func TestResolveRejectsInvalidTemplateInputBeforeExternalPorts(t *testing.T) {
	tests := []json.RawMessage{
		json.RawMessage(`{"task_description":"x","repository_uri":"repo","base_ref":"main","profile":"unknown","tags":{"user_id":"u","app_id":"a"}}`),
		json.RawMessage(`{"task_description":"x","repository_uri":"repo","base_ref":"main","profile":"generic","checks":[{"name":"ok","directory":".","executable":"git","args":["status"],"timeout":"1s","required":true}],"tags":{"user_id":"u","app_id":"a"},"publication":{"mode":"pull_request","provider":"gitlab","target_branch":"main"}}`),
	}
	for _, raw := range tests {
		fixture := newServiceFixture(t)
		if _, err := fixture.service.Resolve(context.Background(), raw); err == nil {
			t.Fatalf("Resolve(%s) error = nil", raw)
		}
		if fixture.runs.reserveCalls != 0 || fixture.resolver.calls != 0 || fixture.mem.calls != 0 {
			t.Fatalf("invalid input reached ports: reserve=%d resolve=%d memory=%d", fixture.runs.reserveCalls, fixture.resolver.calls, fixture.mem.calls)
		}
	}
}

func TestResolveRejectsDirtyLocalSourceAndPersistsFailure(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.resolver.revision.SourceDirty = true

	result, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "dirty"))
	if err == nil {
		t.Fatal("Resolve() error = nil, want dirty source failure")
	}
	if result.Status != run.StatusFailed || result.FailureClass != run.FailureInput || result.Retryable {
		t.Fatalf("Resolve() result = %#v", result)
	}
	record, loadErr := fixture.runs.Load(context.Background(), result.RunID)
	if loadErr != nil {
		t.Fatalf("Load() error = %v", loadErr)
	}
	if record.Failure == nil || record.Failure.CauseCode != "source_dirty" {
		t.Fatalf("record failure = %#v", record.Failure)
	}
	if fixture.mem.calls != 0 {
		t.Fatalf("memory calls = %d, want 0", fixture.mem.calls)
	}
}

func TestResolveValidatesCapabilitiesAndCleansRuntimeBeforeMemory(t *testing.T) {
	fixture := newServiceFixture(t)
	var events []string
	fixture.env.events = &events
	fixture.mem.onSearch = func() { events = append(events, "memory") }

	if _, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "capabilities")); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got, want := strings.Join(events, ","), "build:agent,build:verification,cleanup,memory"; got != want {
		t.Fatalf("event order = %q, want %q", got, want)
	}
	if fixture.env.cleanupCalls != 1 {
		t.Fatalf("environment cleanup calls = %d, want 1", fixture.env.cleanupCalls)
	}
}

func TestResolveCapabilityFailureCleansAndStopsBeforeMemory(t *testing.T) {
	fixture := newServiceFixture(t)
	var events []string
	fixture.env.events = &events
	fixture.mem.onSearch = func() { events = append(events, "memory") }
	fixture.env.build = func(_ context.Context, request environment.Request) (environment.Result, error) {
		if request.Stage == environment.StageVerification {
			return environment.Result{}, errors.New("denied secret-value")
		}
		return environment.Result{Values: map[string]string{"PATH": "/bin", "CODEX_HOME": "/codex"}}, nil
	}

	result, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "capability-failure"))
	if err == nil || result.FailureClass != run.FailurePolicy || result.Retryable {
		t.Fatalf("Resolve() result=%#v error=%v", result, err)
	}
	if got, want := strings.Join(events, ","), "build:agent,build:verification,cleanup"; got != want {
		t.Fatalf("event order = %q, want %q", got, want)
	}
	if fixture.mem.calls != 0 {
		t.Fatalf("Memory.Search() calls = %d, want 0", fixture.mem.calls)
	}
	record, _ := fixture.runs.Load(context.Background(), result.RunID)
	encoded, _ := json.Marshal(record)
	if strings.Contains(string(encoded), "secret-value") {
		t.Fatalf("durable failure leaked capability error: %s", encoded)
	}
}

func TestResolveDoesNotStartASecondConcurrentStageAttempt(t *testing.T) {
	fixture := newServiceFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	fixture.resolver.resolve = func(call int) (repository.Revision, error) {
		if call == 1 {
			close(started)
			<-release
		}
		return fixture.resolver.revision, nil
	}
	raw := validRawInput("Change docs", "concurrent")
	firstDone := make(chan error, 1)
	go func() {
		_, err := fixture.service.Resolve(context.Background(), raw)
		firstDone <- err
	}()
	<-started

	second, err := fixture.service.Resolve(context.Background(), raw)
	if !errors.Is(err, ErrPhaseInProgress) {
		t.Fatalf("Resolve(second) error = %v, want %v", err, ErrPhaseInProgress)
	}
	if second.Status != run.StatusResolving || !second.Retryable {
		t.Fatalf("Resolve(second) result = %#v, want retryable resolving", second)
	}
	if fixture.resolver.callCount() != 1 {
		t.Fatalf("resolver calls = %d, want 1", fixture.resolver.callCount())
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("Resolve(first) error = %v", err)
	}
}

func TestResolveRecoversExpiredRunningAttemptAsNewAttempt(t *testing.T) {
	fixture := newServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.resolveLease = time.Minute
	started := make(chan struct{})
	release := make(chan struct{})
	fixture.resolver.resolve = func(call int) (repository.Revision, error) {
		if call == 1 {
			close(started)
			<-release
		}
		return fixture.resolver.revision, nil
	}
	raw := validRawInput("Change docs", "expired-resolve")
	firstDone := make(chan error, 1)
	go func() {
		_, err := fixture.service.Resolve(context.Background(), raw)
		firstDone <- err
	}()
	<-started
	clock.Advance(2 * time.Minute)

	second, err := fixture.service.Resolve(context.Background(), raw)
	if err != nil || second.Status != run.StatusExecuting {
		t.Fatalf("Resolve(recovery) result=%#v error=%v", second, err)
	}
	if fixture.resolver.callCount() != 2 {
		t.Fatalf("resolver calls = %d, want 2", fixture.resolver.callCount())
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	if len(record.Stages) != 2 ||
		record.Stages[0].Status != run.StageFailed ||
		record.Stages[0].Failure == nil ||
		record.Stages[0].Failure.CauseCode != "worker_lost" ||
		record.Stages[1].Status != run.StageSucceeded {
		t.Fatalf("resolve stage history = %#v", record.Stages)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("Resolve(original) error = %v", err)
	}
}

func TestResolveExpiredWorkerCannotFinishSuccessorAttempt(t *testing.T) {
	fixture := newServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.resolveLease = time.Minute
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	firstRevision := fixture.resolver.revision
	firstRevision.SHA = strings.Repeat("1", 40)
	secondRevision := fixture.resolver.revision
	secondRevision.SHA = strings.Repeat("2", 40)
	fixture.resolver.resolve = func(call int) (repository.Revision, error) {
		switch call {
		case 1:
			close(firstStarted)
			<-releaseFirst
			return firstRevision, nil
		case 2:
			close(secondStarted)
			<-releaseSecond
			return secondRevision, nil
		default:
			return repository.Revision{}, errors.New("unexpected resolver call")
		}
	}
	raw := validRawInput("Change docs", "fenced-resolve")
	type response struct {
		result PhaseResult
		err    error
	}
	firstDone := make(chan response, 1)
	go func() {
		result, err := fixture.service.Resolve(context.Background(), raw)
		firstDone <- response{result: result, err: err}
	}()
	<-firstStarted
	clock.Advance(2 * time.Minute)
	secondDone := make(chan response, 1)
	go func() {
		result, err := fixture.service.Resolve(context.Background(), raw)
		secondDone <- response{result: result, err: err}
	}()
	<-secondStarted

	close(releaseFirst)
	stale := <-firstDone
	if !errors.Is(stale.err, ErrPhaseInProgress) || stale.result.Status != run.StatusResolving {
		t.Fatalf("stale Resolve() result=%#v error=%v", stale.result, stale.err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	if record.BaseSHA != "" || !runningStageAttempt(record, "resolve", 2) {
		t.Fatalf("stale worker completed successor attempt: %#v", record)
	}

	close(releaseSecond)
	recovered := <-secondDone
	if recovered.err != nil || recovered.result.Status != run.StatusExecuting {
		t.Fatalf("successor Resolve() result=%#v error=%v", recovered.result, recovered.err)
	}
	record, _ = fixture.runs.Load(context.Background(), "run-123")
	if record.BaseSHA != secondRevision.SHA {
		t.Fatalf("BaseSHA = %q, want successor SHA %q", record.BaseSHA, secondRevision.SHA)
	}
}

func TestResolveProviderFailureDoesNotPersistSecretDiagnostic(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.resolver.err = errors.New("remote rejected token top-secret-value")

	result, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "provider-error"))
	if err == nil || !result.Retryable {
		t.Fatalf("Resolve() result=%#v error=%v", result, err)
	}
	record, loadErr := fixture.runs.Load(context.Background(), result.RunID)
	if loadErr != nil {
		t.Fatalf("Load() error = %v", loadErr)
	}
	encoded, _ := json.Marshal(record)
	if strings.Contains(string(encoded), "top-secret-value") || strings.Contains(string(encoded), "token") {
		t.Fatalf("run evidence leaked provider error: %s", encoded)
	}
}

func TestResolveRejectsResolverOutputThatIsNotBoundToTheRequest(t *testing.T) {
	tests := []repository.Revision{
		{RepositoryURI: "https://other.test/repository.git", Ref: "refs/heads/main", SHA: "0123456789012345678901234567890123456789"},
		{RepositoryURI: "https://example.test/repository.git", Ref: "refs/heads/other", SHA: "0123456789012345678901234567890123456789"},
		{RepositoryURI: "https://example.test/repository.git", Ref: "refs/heads/main", SHA: "not-an-immutable-sha"},
	}
	for _, revision := range tests {
		fixture := newServiceFixture(t)
		fixture.resolver.revision = revision
		result, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "bad-revision"))
		if err == nil || result.Status != run.StatusFailed || result.FailureClass != run.FailureInternal {
			t.Fatalf("Resolve(%#v) result=%#v error=%v", revision, result, err)
		}
		if fixture.mem.calls != 0 {
			t.Fatalf("invalid revision reached memory: calls=%d", fixture.mem.calls)
		}
	}
}

func TestNewRejectsMissingDependenciesAndCopiesProfiles(t *testing.T) {
	fixture := newServiceFixture(t)
	valid := func() Dependencies {
		return Dependencies{
			Templates: fixture.service.templates, Runs: fixture.service.runs,
			Memory: fixture.service.memory, Resolver: fixture.service.resolver,
			Workspaces: fixture.service.workspaces,
			Profiles: map[string]repository.Profile{
				"generic": fixture.service.profiles["generic"],
				"go":      fixture.service.profiles["go"],
			},
			Environments: fixture.service.environments, Agent: fixture.service.agent,
			Verifier: fixture.service.verifier, Capturer: fixture.service.capturer,
			Policy: fixture.service.policy, Artifacts: fixture.service.artifacts,
			Publisher: fixture.service.publisher, Clock: fixture.service.clock,
			NewID: fixture.service.newID,
		}
	}
	tests := []struct {
		name   string
		mutate func(*Dependencies)
	}{
		{"templates", func(d *Dependencies) { d.Templates = nil }},
		{"runs", func(d *Dependencies) { d.Runs = nil }},
		{"memory", func(d *Dependencies) { d.Memory = nil }},
		{"resolver", func(d *Dependencies) { d.Resolver = nil }},
		{"workspaces", func(d *Dependencies) { d.Workspaces = nil }},
		{"generic profile", func(d *Dependencies) { delete(d.Profiles, "generic") }},
		{"go profile", func(d *Dependencies) { delete(d.Profiles, "go") }},
		{"environments", func(d *Dependencies) { d.Environments = nil }},
		{"agent", func(d *Dependencies) { d.Agent = nil }},
		{"verifier", func(d *Dependencies) { d.Verifier = nil }},
		{"capturer", func(d *Dependencies) { d.Capturer = nil }},
		{"policy", func(d *Dependencies) { d.Policy = nil }},
		{"artifacts", func(d *Dependencies) { d.Artifacts = nil }},
		{"publisher", func(d *Dependencies) { d.Publisher = nil }},
		{"clock", func(d *Dependencies) { d.Clock = nil }},
		{"ID generator", func(d *Dependencies) { d.NewID = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dependencies := valid()
			test.mutate(&dependencies)
			if _, err := New(dependencies); err == nil {
				t.Fatal("New() error = nil")
			}
		})
	}

	dependencies := valid()
	profiles := dependencies.Profiles
	service, err := New(dependencies)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	delete(profiles, "generic")
	if _, exists := service.profiles["generic"]; !exists {
		t.Fatal("New() retained caller-owned profiles map")
	}
}

func TestResolveRetriesCASAtMostThreeTimes(t *testing.T) {
	t.Run("recovers from two conflicts", func(t *testing.T) {
		fixture := newServiceFixture(t)
		fixture.runs.saveConflicts = 2
		if _, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "cas-ok")); err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if fixture.runs.saveCalls < 4 {
			t.Fatalf("Save() calls = %d, want retries plus completion", fixture.runs.saveCalls)
		}
	})
	t.Run("stops after three conflicts", func(t *testing.T) {
		fixture := newServiceFixture(t)
		fixture.runs.saveConflicts = 3
		if _, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "cas-stop")); !errors.Is(err, run.ErrVersionConflict) {
			t.Fatalf("Resolve() error = %v, want %v", err, run.ErrVersionConflict)
		}
		if fixture.runs.saveCalls != 3 || fixture.resolver.callCount() != 0 {
			t.Fatalf("Save() calls=%d resolver calls=%d", fixture.runs.saveCalls, fixture.resolver.callCount())
		}
	})
}

func validRawInput(task, key string) json.RawMessage {
	value := map[string]any{
		"idempotency_key":  key,
		"task_description": task,
		"repository_uri":   "https://example.test/repository.git",
		"base_ref":         "refs/heads/main",
		"tags":             map[string]string{"user_id": "guilhermecastro", "app_id": "araihu-paje"},
		"profile":          "generic",
		"checks": []map[string]any{{
			"name": "git status", "directory": ".", "executable": "git",
			"args": []string{"status", "--short"}, "timeout": "1m", "required": true,
		}},
		"publication": map[string]any{"mode": "artifact"},
	}
	raw, _ := json.Marshal(value)
	return raw
}

type serviceFixture struct {
	service    *Service
	runs       *recordingRunStore
	resolver   *recordingResolver
	mem        *recordingMemory
	profile    *fakeProfile
	env        *fakeEnvironment
	agent      *fakeAgent
	verifier   *fakeVerifier
	capturer   *fakeCapturer
	policy     *fakePolicy
	artifacts  *artifactmock.Store
	workspaces *fakeWorkspaceManager
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	registry, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		t.Fatal(err)
	}
	profile := &fakeProfile{name: "generic"}
	fixture := &serviceFixture{
		runs: &recordingRunStore{Store: runmock.NewStore()},
		resolver: &recordingResolver{revision: repository.Revision{
			RepositoryURI: "https://example.test/repository.git",
			Ref:           "refs/heads/main",
			SHA:           "0123456789012345678901234567890123456789",
		}},
		mem:        &recordingMemory{},
		profile:    profile,
		env:        &fakeEnvironment{},
		agent:      &fakeAgent{},
		verifier:   &fakeVerifier{},
		capturer:   &fakeCapturer{},
		policy:     &fakePolicy{decision: policy.Decision{Allowed: true}},
		artifacts:  artifactmock.NewStore(),
		workspaces: &fakeWorkspaceManager{},
	}
	fixture.service, err = New(Dependencies{
		Templates:    registry,
		Runs:         fixture.runs,
		Memory:       fixture.mem,
		Resolver:     fixture.resolver,
		Workspaces:   fixture.workspaces,
		Profiles:     map[string]repository.Profile{"generic": profile, "go": &fakeProfile{name: "go"}},
		Environments: fixture.env,
		Agent:        fixture.agent,
		Verifier:     fixture.verifier,
		Capturer:     fixture.capturer,
		Policy:       fixture.policy,
		Artifacts:    fixture.artifacts,
		Publisher:    publishermock.NewPublisher(publisher.Result{}, nil),
		Clock:        func() time.Time { return time.Unix(100, 0).UTC() },
		NewID:        func() string { return "run-123" },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return fixture
}

type recordingRunStore struct {
	*runmock.Store
	reserveCalls  int
	saveCalls     int
	saveConflicts int
	saveErrors    map[int]error
	saveHook      func(int, run.Record)
	loadMutate    func(run.Record) run.Record
}

func (s *recordingRunStore) Reserve(ctx context.Context, reservation run.Reservation) (run.Record, bool, error) {
	s.reserveCalls++
	return s.Store.Reserve(ctx, reservation)
}

func (s *recordingRunStore) Load(ctx context.Context, id string) (run.Record, error) {
	record, err := s.Store.Load(ctx, id)
	if err == nil && s.loadMutate != nil {
		record = s.loadMutate(record)
	}
	return record, err
}

func (s *recordingRunStore) Save(ctx context.Context, record run.Record, expected uint64) (run.Record, error) {
	s.saveCalls++
	if s.saveHook != nil {
		s.saveHook(s.saveCalls, record)
	}
	if err := s.saveErrors[s.saveCalls]; err != nil {
		return run.Record{}, err
	}
	if s.saveConflicts > 0 {
		s.saveConflicts--
		return run.Record{}, run.ErrVersionConflict
	}
	return s.Store.Save(ctx, record, expected)
}

type recordingResolver struct {
	mu       sync.Mutex
	calls    int
	revision repository.Revision
	err      error
	resolve  func(int) (repository.Revision, error)
}

func (r *recordingResolver) Resolve(context.Context, string, string) (repository.Revision, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	resolve := r.resolve
	revision := r.revision
	err := r.err
	r.mu.Unlock()
	if resolve != nil {
		return resolve(call)
	}
	return revision, err
}

func (r *recordingResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type recordingMemory struct {
	mu       sync.Mutex
	calls    int
	result   []memory.Memory
	err      error
	onSearch func()
}

func (m *recordingMemory) Search(_ context.Context, _ string, _ int, _ map[string]string) ([]memory.Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.onSearch != nil {
		m.onSearch()
	}
	return cloneTestMemories(m.result), m.err
}

func (m *recordingMemory) Save(context.Context, string, map[string]string) error { return nil }

type fakeProfile struct {
	name   string
	result repository.ProfileResult
	err    error
}

func (p *fakeProfile) Name() string { return p.name }
func (p *fakeProfile) Inspect(context.Context, repository.ProfileRequest) (repository.ProfileResult, error) {
	return p.result, p.err
}

type fakeEnvironment struct {
	build        func(context.Context, environment.Request) (environment.Result, error)
	cleanup      func(context.Context, string) error
	events       *[]string
	cleanupCalls int
}

func (e *fakeEnvironment) Build(ctx context.Context, req environment.Request) (environment.Result, error) {
	if e.events != nil {
		*e.events = append(*e.events, "build:"+string(req.Stage))
	}
	if e.build != nil {
		return e.build(ctx, req)
	}
	return environment.Result{Values: map[string]string{"PATH": "/bin", "CODEX_HOME": "/runtime/codex"}}, nil
}
func (e *fakeEnvironment) Cleanup(ctx context.Context, runID string) error {
	e.cleanupCalls++
	if e.events != nil {
		*e.events = append(*e.events, "cleanup")
	}
	if e.cleanup != nil {
		return e.cleanup(ctx, runID)
	}
	return nil
}

type fakeAgent struct {
	run func(context.Context, runner.RunRequest) (runner.ExecutionResult, error)
}

func (a *fakeAgent) Run(ctx context.Context, req runner.RunRequest) (runner.ExecutionResult, error) {
	if a.run != nil {
		return a.run(ctx, req)
	}
	return runner.ExecutionResult{Started: true, Completed: true}, nil
}

type fakeVerifier struct {
	run func(context.Context, verification.Command, map[string]string) verification.Result
}

func (v *fakeVerifier) Run(ctx context.Context, command verification.Command, env map[string]string) verification.Result {
	if v.run != nil {
		return v.run(ctx, command, env)
	}
	return verification.Result{Command: command, Passed: true}
}

type fakeCapturer struct {
	capture func(context.Context, gitcapture.Request) (gitcapture.Result, error)
	apply   func(context.Context, gitcapture.ApplyRequest) error
}

func (c *fakeCapturer) Capture(ctx context.Context, req gitcapture.Request) (gitcapture.Result, error) {
	if c.capture != nil {
		return c.capture(ctx, req)
	}
	return gitcapture.Result{TreeSHA: "tree"}, nil
}
func (c *fakeCapturer) Apply(ctx context.Context, req gitcapture.ApplyRequest) error {
	if c.apply != nil {
		return c.apply(ctx, req)
	}
	return nil
}

type fakePolicy struct{ decision policy.Decision }

func (p *fakePolicy) Evaluate(context.Context, gitcapture.Result) policy.Decision { return p.decision }

type fakeWorkspaceManager struct {
	prepare func(context.Context, string, string) (workspace.Workspace, error)
	calls   int
}

func (m *fakeWorkspaceManager) Prepare(ctx context.Context, repo, sha string) (workspace.Workspace, error) {
	m.calls++
	if m.prepare != nil {
		return m.prepare(ctx, repo, sha)
	}
	return &fakeWorkspace{path: "/tmp/workspace"}, nil
}

type fakeWorkspace struct {
	path    string
	cleanup func(context.Context) error
}

func (w *fakeWorkspace) Path() string { return w.path }
func (w *fakeWorkspace) Cleanup(ctx context.Context) error {
	if w.cleanup != nil {
		return w.cleanup(ctx)
	}
	return nil
}

func cloneTestMemories(source []memory.Memory) []memory.Memory {
	cloned := make([]memory.Memory, len(source))
	for index, item := range source {
		cloned[index] = item
		cloned[index].Metadata = make(map[string]string, len(item.Metadata))
		for key, value := range item.Metadata {
			cloned[index].Metadata[key] = value
		}
	}
	return cloned
}

type mutableClock struct {
	mu  sync.Mutex
	now time.Time
}

func newMutableClock(now time.Time) *mutableClock { return &mutableClock{now: now} }
func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *mutableClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

var (
	_ artifact.Store      = (*artifactmock.Store)(nil)
	_ publisher.Publisher = (*publishermock.Publisher)(nil)
	_ workspace.Manager   = (*fakeWorkspaceManager)(nil)
	_ environment.Builder = (*fakeEnvironment)(nil)
	_ verification.Runner = (*fakeVerifier)(nil)
	_ gitcapture.Capturer = (*fakeCapturer)(nil)
	_ policy.Evaluator    = (*fakePolicy)(nil)
)
