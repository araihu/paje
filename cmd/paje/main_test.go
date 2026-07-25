package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	artifactfilesystem "github.com/araihu/paje/internal/artifact/filesystem"
	"github.com/araihu/paje/internal/config"
	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/memory/mem0"
	memorymock "github.com/araihu/paje/internal/memory/mock"
	publishermock "github.com/araihu/paje/internal/publisher/mock"
	runpkg "github.com/araihu/paje/internal/run"
	runfilesystem "github.com/araihu/paje/internal/run/filesystem"
	codexrunner "github.com/araihu/paje/internal/runner/codex"
	"github.com/araihu/paje/internal/runner/local"
	runnermock "github.com/araihu/paje/internal/runner/mock"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workspace/gitworktree"
	workspacemock "github.com/araihu/paje/internal/workspace/mock"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

func TestRunHardenedFailsBeforeReadingConfiguration(t *testing.T) {
	want := errors.New("guard unavailable")
	getenvCalled := false
	err := runHardened(context.Background(), func(string) string {
		getenvCalled = true
		return ""
	}, func() error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("runHardened() error = %v, want %v", err, want)
	}
	if getenvCalled {
		t.Fatal("runHardened() read credential-bearing configuration before installing the process guard")
	}
}

func TestRunHardenedInstallsGuardBeforeReadingConfiguration(t *testing.T) {
	guardInstalled := false
	err := runHardened(context.Background(), func(string) string {
		if !guardInstalled {
			t.Fatal("runHardened() read configuration before installing the process guard")
		}
		return ""
	}, func() error {
		guardInstalled = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "HATCHET_CLIENT_TOKEN is required") {
		t.Fatalf("runHardened() error = %v, want post-guard configuration error", err)
	}
}

func TestBuildDependenciesUsesMocksByDefault(t *testing.T) {
	t.Parallel()

	cfg := mockConfig(t)

	dependencies, err := buildDependencies(cfg)
	if err != nil {
		t.Fatalf("buildDependencies() error = %v", err)
	}
	if _, ok := dependencies.memory.(*memorymock.Store); !ok {
		t.Errorf("memory dependency = %T, want *memory/mock.Store", dependencies.memory)
	}
	if _, ok := dependencies.workspaces.(*workspacemock.Manager); !ok {
		t.Errorf("workspace dependency = %T, want *workspace/mock.Manager", dependencies.workspaces)
	}
	if _, ok := dependencies.runner.(*runnermock.Runner); !ok {
		t.Errorf("runner dependency = %T, want *runner/mock.Runner", dependencies.runner)
	}
	if _, ok := dependencies.runs.(*runfilesystem.Store); !ok {
		t.Errorf("run dependency = %T, want *run/filesystem.Store", dependencies.runs)
	}
	if _, ok := dependencies.artifacts.(*artifactfilesystem.Store); !ok {
		t.Errorf("artifact dependency = %T, want *artifact/filesystem.Store", dependencies.artifacts)
	}
	if _, ok := dependencies.publisher.(*publishermock.Publisher); !ok {
		t.Errorf("publisher dependency = %T, want *publisher/mock.Publisher", dependencies.publisher)
	}
	if dependencies.orchestrator == nil || dependencies.codeChanges == nil {
		t.Fatalf("workflow services = legacy:%p beta:%p", dependencies.orchestrator, dependencies.codeChanges)
	}
	if _, err := dependencies.templates.Resolve(templatecodechange.ID); err != nil {
		t.Errorf("resolve code-change@v1: %v", err)
	}
}

func TestBuildDependenciesUsesConfiguredRealAdapters(t *testing.T) {
	t.Parallel()

	cfg := mockConfig(t)
	cfg.MemoryAdapter = "mem0"
	cfg.WorkspaceAdapter = "git"
	cfg.RunnerAdapter = "local"
	cfg.Mem0APIKey = "secret"
	cfg.Mem0BaseURL = "https://mem0.example.test"
	cfg.RunnerCommand = os.Args[0]
	cfg.RunnerArgs = []string{"-test.run=TestNonexistent"}

	dependencies, err := buildDependencies(cfg)
	if err != nil {
		t.Fatalf("buildDependencies() error = %v", err)
	}
	if _, ok := dependencies.memory.(*mem0.Store); !ok {
		t.Errorf("memory dependency = %T, want *mem0.Store", dependencies.memory)
	}
	if _, ok := dependencies.workspaces.(*gitworktree.Manager); !ok {
		t.Errorf("workspace dependency = %T, want *gitworktree.Manager", dependencies.workspaces)
	}
	if _, ok := dependencies.runner.(*local.Runner); !ok {
		t.Errorf("runner dependency = %T, want *local.Runner", dependencies.runner)
	}
}

func TestBuildDependenciesUsesCodexRunner(t *testing.T) {
	t.Parallel()

	dependencies, err := buildDependencies(config.Config{
		MemoryAdapter:           "mock",
		WorkspaceAdapter:        "mock",
		RunnerAdapter:           "codex",
		PublisherAdapter:        "mock",
		WorkspaceRoot:           filepathForTestRoot(t, "workspaces"),
		RunRoot:                 filepathForTestRoot(t, "runs"),
		ArtifactRoot:            filepathForTestRoot(t, "artifacts"),
		RuntimeRoot:             filepathForTestRoot(t, "runtime"),
		ArtifactLimitBytes:      10 << 20,
		CommandOutputLimitBytes: 1 << 20,
		RunnerCommand:           os.Args[0],
		CodexHome:               "/codex-home",
	})
	if err != nil {
		t.Fatalf("buildDependencies() error = %v", err)
	}
	if _, ok := dependencies.runner.(*codexrunner.Runner); !ok {
		t.Errorf("runner dependency = %T, want *codex.Runner", dependencies.runner)
	}
}

func TestBuildDependenciesIsolatesAgentEnvironment(t *testing.T) {
	t.Parallel()

	cfg := mockConfig(t)
	cfg.RunnerAdapter = "codex"
	cfg.RunnerCommand = os.Args[0]
	cfg.CodexHome = "/codex-home"
	cfg.HatchetClientToken = "hatchet-secret"
	cfg.Mem0APIKey = "mem0-secret"
	cfg.GitHubToken = "github-secret"
	cfg.Environment = map[string]string{
		"PATH":                 "/usr/bin",
		"HATCHET_CLIENT_TOKEN": cfg.HatchetClientToken,
		"MEM0_API_KEY":         cfg.Mem0APIKey,
		"GITHUB_TOKEN":         cfg.GitHubToken,
	}

	dependencies, err := buildDependencies(cfg)
	if err != nil {
		t.Fatalf("buildDependencies() error = %v", err)
	}
	result, err := dependencies.environments.Build(context.Background(), environment.Request{
		RunID: "run-environment", Stage: environment.StageAgent,
	})
	if err != nil {
		t.Fatalf("Build(agent environment) error = %v", err)
	}
	if result.Values["CODEX_HOME"] != "/codex-home" {
		t.Errorf("CODEX_HOME = %q, want /codex-home", result.Values["CODEX_HOME"])
	}
	for _, key := range []string{"HATCHET_CLIENT_TOKEN", "MEM0_API_KEY", "GITHUB_TOKEN"} {
		if _, found := result.Values[key]; found {
			t.Errorf("agent environment contains worker credential %s", key)
		}
	}
}

func TestBuildDependenciesMockAgentDoesNotRequireCodexHome(t *testing.T) {
	t.Parallel()

	dependencies, err := buildDependencies(mockConfig(t))
	if err != nil {
		t.Fatalf("buildDependencies() error = %v", err)
	}
	t.Cleanup(func() { _ = dependencies.Close() })

	result, err := dependencies.environments.Build(context.Background(), environment.Request{
		RunID: "run-mock-agent", Stage: environment.StageAgent,
	})
	if err != nil {
		t.Fatalf("Build(agent environment) error = %v", err)
	}
	if _, found := result.Values["CODEX_HOME"]; found {
		t.Error("mock agent environment contains CODEX_HOME")
	}
}

func TestBuildDependenciesLocalAgentCannotReceiveCodexHome(t *testing.T) {
	t.Parallel()

	cfg := mockConfig(t)
	cfg.RunnerAdapter = "local"
	cfg.RunnerCommand = os.Args[0]
	cfg.CodexHome = "/codex-home"
	dependencies, err := buildDependencies(cfg)
	if err != nil {
		t.Fatalf("buildDependencies() error = %v", err)
	}
	t.Cleanup(func() { _ = dependencies.Close() })

	result, err := dependencies.environments.Build(context.Background(), environment.Request{
		RunID: "run-local-agent", Stage: environment.StageAgent,
	})
	if err != nil {
		t.Fatalf("Build(agent environment) error = %v", err)
	}
	if _, found := result.Values["CODEX_HOME"]; found {
		t.Error("local agent environment contains CODEX_HOME")
	}
}

func TestBuildDependenciesReconstructsFilesystemStores(t *testing.T) {
	t.Parallel()

	cfg := mockConfig(t)
	first, err := buildDependencies(cfg)
	if err != nil {
		t.Fatalf("first buildDependencies() error = %v", err)
	}
	record, _, err := first.runs.Reserve(context.Background(), runpkg.Reservation{
		NewRunID: "run-persisted", Template: templatecodechange.ID,
		InputHash: "input-hash", Input: json.RawMessage(`{"task":"persist"}`),
		RepositoryURI: "https://example.test/repository.git", BaseRef: "main",
		PublicationMode: "artifact", CreatedAt: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	bundle := persistentTestBundle()
	reference, err := first.artifacts.Save(context.Background(), bundle)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	second, err := buildDependencies(cfg)
	if err != nil {
		t.Fatalf("second buildDependencies() error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	loadedRecord, err := second.runs.Load(context.Background(), record.ID)
	if err != nil || loadedRecord.ID != record.ID {
		t.Fatalf("Load(run) = %#v, %v", loadedRecord, err)
	}
	loadedBundle, err := second.artifacts.Load(context.Background(), reference)
	if err != nil || loadedBundle.Manifest.RunID != bundle.Manifest.RunID {
		t.Fatalf("Load(artifact) run = %q, error = %v", loadedBundle.Manifest.RunID, err)
	}
}

func TestBuildHatchetWorkflowsConstructsLegacyAndBetaDeclarationsWithoutServer(t *testing.T) {
	client := newOfflineHatchetClient(t)
	dependencies, err := buildDependencies(mockConfig(t))
	if err != nil {
		t.Fatalf("buildDependencies() error = %v", err)
	}
	t.Cleanup(func() { _ = dependencies.Close() })

	legacyTask, betaWorkflow, err := buildHatchetWorkflows(client, dependencies)
	if err != nil {
		t.Fatalf("buildHatchetWorkflows() error = %v", err)
	}
	if legacyTask == nil || betaWorkflow == nil {
		t.Fatalf("Hatchet declarations = legacy:%p beta:%p", legacyTask, betaWorkflow)
	}
	dump, handlers, durableHandlers, _ := betaWorkflow.Dump()
	if dump.GetName() != "paje-code-change-v1" || len(dump.GetTasks()) != 5 ||
		len(handlers) != 4 || len(durableHandlers) != 1 {
		t.Fatalf("beta declaration = name:%q tasks:%d handlers:%d durable:%d", dump.GetName(), len(dump.GetTasks()), len(handlers), len(durableHandlers))
	}
}

func TestBuildDependenciesRejectsUnknownSelection(t *testing.T) {
	t.Parallel()

	if _, err := buildDependencies(config.Config{
		MemoryAdapter:    "unknown",
		WorkspaceAdapter: "mock",
		RunnerAdapter:    "mock",
		PublisherAdapter: "mock",
		WorkspaceRoot:    t.TempDir(),
	}); err == nil {
		t.Fatal("buildDependencies() error = nil, want unknown adapter error")
	}
}

func TestBuildDependenciesRejectsUnknownRunnerSelection(t *testing.T) {
	t.Parallel()

	if _, err := buildDependencies(config.Config{
		MemoryAdapter:    "mock",
		WorkspaceAdapter: "mock",
		RunnerAdapter:    "unknown",
		PublisherAdapter: "mock",
		WorkspaceRoot:    t.TempDir(),
	}); err == nil {
		t.Fatal("buildDependencies() error = nil, want unknown adapter error")
	}
}

func mockConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	return config.Config{
		HatchetClientToken:      "hatchet-token",
		MemoryAdapter:           "mock",
		WorkspaceAdapter:        "mock",
		RunnerAdapter:           "mock",
		PublisherAdapter:        "mock",
		WorkspaceRoot:           root + "/workspaces",
		RunRoot:                 root + "/runs",
		ArtifactRoot:            root + "/artifacts",
		RuntimeRoot:             root + "/runtime",
		ArtifactLimitBytes:      10 << 20,
		CommandOutputLimitBytes: 1 << 20,
		RunnerCommand:           "codex",
		RunnerArgs:              []string{"exec"},
		EnvironmentAllowlist:    []string{},
		Environment:             map[string]string{"PATH": os.Getenv("PATH")},
		GitHubAPIURL:            "https://api.github.com",
	}
}

func filepathForTestRoot(t *testing.T, name string) string {
	t.Helper()
	return t.TempDir() + "/" + name
}

func persistentTestBundle() artifact.Bundle {
	return artifact.Bundle{
		Manifest: artifact.Manifest{
			SchemaVersion: 1, RunID: "run-persisted", Template: templatecodechange.ID,
			Repository: "https://example.test/repository.git",
			BaseSHA:    strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40),
			Changes: []artifact.Change{{Path: "main.go", Status: "modified", OldMode: "100644", NewMode: "100644"}},
		},
		ChangesPatch:      []byte("diff --git a/main.go b/main.go\n"),
		AgentOutput:       []byte("done"),
		ExecutionMetadata: json.RawMessage(`{"exit_code":0,"duration":1,"started":true,"completed":true,"truncated":false}`),
		Verification: []verification.Result{{
			Command: verification.Command{Name: "test", Directory: ".", Executable: "go", Timeout: time.Minute, Required: true},
			Passed:  true,
		}},
		Preflight: map[string]string{"base_sha": strings.Repeat("a", 40)},
		Warnings:  []string{},
	}
}

func newOfflineHatchetClient(t *testing.T) *hatchet.Client {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{
  "server_url":"http://127.0.0.1:1",
  "grpc_broadcast_address":"127.0.0.1:1",
  "exp":4102444800,
  "sub":"00000000-0000-0000-0000-000000000001"
}`))
	t.Setenv("HATCHET_CLIENT_TOKEN", "e30."+payload+".test-signature")
	t.Setenv("HATCHET_CLIENT_HOST_PORT", "127.0.0.1:1")
	t.Setenv("HATCHET_CLIENT_SERVER_URL", "http://127.0.0.1:1")
	t.Setenv("HATCHET_CLIENT_TLS_STRATEGY", "none")
	t.Setenv("HATCHET_CLIENT_NO_RETRY", "true")
	t.Setenv("HATCHET_CLIENT_LOG_LEVEL", "error")
	client, err := hatchet.NewClient()
	if err != nil {
		t.Fatalf("new offline Hatchet client: %v", err)
	}
	return client
}
