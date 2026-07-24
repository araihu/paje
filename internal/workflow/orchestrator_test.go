package workflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/workflow"
	"github.com/araihu/paje/internal/workspace"
)

func TestOrchestratorRunsPipelineInOrder(t *testing.T) {
	t.Parallel()

	fixture := newFixture()
	fixture.store.searchResult = []memory.Memory{
		{ID: "one", Content: "Prefer small commits"},
		{ID: "two", Content: "Run tests before completion"},
	}
	fixture.runner.result = runner.ExecutionResult{
		Output:   "agent output",
		ExitCode: 0,
		Duration: 1.5,
	}
	orchestrator := fixture.orchestrator(t)
	input := validInput()

	output, err := orchestrator.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := strings.Join(fixture.calls, ","), "search,prepare,run,save,cleanup"; got != want {
		t.Fatalf("call order = %q, want %q", got, want)
	}
	if output.Output != "agent output" ||
		output.ExitCode != 0 ||
		output.Duration != 1.5 ||
		output.MemoriesLoaded != 2 {
		t.Errorf("Run() output = %#v", output)
	}

	recordedRun := fixture.runner.request
	if recordedRun.WorkspacePath != "/workspace/run-1" {
		t.Errorf("runner workspace = %q", recordedRun.WorkspacePath)
	}
	for _, want := range []string{
		input.TaskDescription,
		"Relevant memory:",
		"Prefer small commits",
		"Run tests before completion",
	} {
		if !strings.Contains(recordedRun.TaskDescription, want) {
			t.Errorf("runner task = %q, want it to contain %q", recordedRun.TaskDescription, want)
		}
	}
	if recordedRun.Env["AGENT_MODE"] != "autonomous" {
		t.Errorf("runner env = %#v", recordedRun.Env)
	}
	if !strings.Contains(fixture.store.savedContent, "agent output") {
		t.Errorf("saved content = %q, want agent output", fixture.store.savedContent)
	}
	if fixture.store.savedTags["app_id"] != "paje" ||
		fixture.store.savedTags["paje_exit_code"] != "0" ||
		fixture.store.savedTags["paje_result"] != "completed" {
		t.Errorf("saved tags = %#v", fixture.store.savedTags)
	}
	if _, exists := input.Tags["paje_result"]; exists {
		t.Errorf("Run() mutated input tags: %#v", input.Tags)
	}
}

func TestOrchestratorDefaultsMemoryQueryAndLimit(t *testing.T) {
	t.Parallel()

	fixture := newFixture()
	orchestrator := fixture.orchestrator(t)
	input := validInput()
	input.MemoryQuery = ""
	input.MemoryLimit = 0

	if _, err := orchestrator.Run(context.Background(), input); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if fixture.store.searchQuery != input.TaskDescription {
		t.Errorf("search query = %q, want task description", fixture.store.searchQuery)
	}
	if fixture.store.searchLimit != 10 {
		t.Errorf("search limit = %d, want 10", fixture.store.searchLimit)
	}
}

func TestOrchestratorReportsStageFailuresAndCleansPreparedWorkspace(t *testing.T) {
	t.Parallel()

	searchErr := errors.New("search failed")
	prepareErr := errors.New("prepare failed")
	runErr := errors.New("run failed")
	saveErr := errors.New("save failed")
	cleanupErr := errors.New("cleanup failed")

	testCases := []struct {
		name      string
		configure func(*fixture)
		wantErr   error
		wantCalls string
	}{
		{
			name: "search",
			configure: func(f *fixture) {
				f.store.searchErr = searchErr
			},
			wantErr:   searchErr,
			wantCalls: "search",
		},
		{
			name: "prepare",
			configure: func(f *fixture) {
				f.manager.err = prepareErr
			},
			wantErr:   prepareErr,
			wantCalls: "search,prepare",
		},
		{
			name: "run",
			configure: func(f *fixture) {
				f.runner.err = runErr
			},
			wantErr:   runErr,
			wantCalls: "search,prepare,run,cleanup",
		},
		{
			name: "save",
			configure: func(f *fixture) {
				f.store.saveErr = saveErr
			},
			wantErr:   saveErr,
			wantCalls: "search,prepare,run,save,cleanup",
		},
		{
			name: "cleanup",
			configure: func(f *fixture) {
				f.workspace.err = cleanupErr
			},
			wantErr:   cleanupErr,
			wantCalls: "search,prepare,run,save,cleanup",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fixture := newFixture()
			testCase.configure(fixture)
			_, err := fixture.orchestrator(t).Run(context.Background(), validInput())
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("Run() error = %v, want %v", err, testCase.wantErr)
			}
			if got := strings.Join(fixture.calls, ","); got != testCase.wantCalls {
				t.Errorf("call order = %q, want %q", got, testCase.wantCalls)
			}
		})
	}
}

func TestOrchestratorJoinsPrimaryAndCleanupFailures(t *testing.T) {
	t.Parallel()

	runErr := errors.New("run failed")
	cleanupErr := errors.New("cleanup failed")
	fixture := newFixture()
	fixture.runner.err = runErr
	fixture.workspace.err = cleanupErr

	_, err := fixture.orchestrator(t).Run(context.Background(), validInput())
	if !errors.Is(err, runErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Run() error = %v, want both run and cleanup failures", err)
	}
}

func TestNewRejectsNilDependencies(t *testing.T) {
	t.Parallel()

	fixture := newFixture()
	testCases := []struct {
		name       string
		store      memory.Store
		workspaces workspace.Manager
		runner     runner.Runner
	}{
		{name: "memory", workspaces: fixture.manager, runner: fixture.runner},
		{name: "workspace", store: fixture.store, runner: fixture.runner},
		{name: "runner", store: fixture.store, workspaces: fixture.manager},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if _, err := workflow.New(testCase.store, testCase.workspaces, testCase.runner); err == nil {
				t.Fatal("New() error = nil, want validation error")
			}
		})
	}
}

func validInput() workflow.RunInput {
	return workflow.RunInput{
		TaskDescription: "Implement the feature",
		RepositoryURI:   "https://example.test/repository.git",
		Branch:          "main",
		MemoryQuery:     "implementation guidance",
		MemoryLimit:     5,
		Tags:            map[string]string{"app_id": "paje"},
		Env:             map[string]string{"AGENT_MODE": "autonomous"},
	}
}

type fixture struct {
	calls     []string
	store     *recordingStore
	manager   *recordingManager
	workspace *recordingWorkspace
	runner    *recordingRunner
}

func newFixture() *fixture {
	fixture := &fixture{}
	fixture.store = &recordingStore{calls: &fixture.calls}
	fixture.workspace = &recordingWorkspace{calls: &fixture.calls, path: "/workspace/run-1"}
	fixture.manager = &recordingManager{calls: &fixture.calls, workspace: fixture.workspace}
	fixture.runner = &recordingRunner{calls: &fixture.calls}
	return fixture
}

func (f *fixture) orchestrator(t *testing.T) *workflow.Orchestrator {
	t.Helper()
	orchestrator, err := workflow.New(f.store, f.manager, f.runner)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return orchestrator
}

type recordingStore struct {
	calls        *[]string
	searchResult []memory.Memory
	searchErr    error
	saveErr      error
	searchQuery  string
	searchLimit  int
	savedContent string
	savedTags    map[string]string
}

func (s *recordingStore) Search(
	_ context.Context,
	query string,
	limit int,
	_ map[string]string,
) ([]memory.Memory, error) {
	*s.calls = append(*s.calls, "search")
	s.searchQuery = query
	s.searchLimit = limit
	return s.searchResult, s.searchErr
}

func (s *recordingStore) Save(_ context.Context, content string, tags map[string]string) error {
	*s.calls = append(*s.calls, "save")
	s.savedContent = content
	s.savedTags = cloneMap(tags)
	return s.saveErr
}

type recordingManager struct {
	calls     *[]string
	workspace workspace.Workspace
	err       error
}

func (m *recordingManager) Prepare(
	_ context.Context,
	_ string,
	_ string,
) (workspace.Workspace, error) {
	*m.calls = append(*m.calls, "prepare")
	return m.workspace, m.err
}

type recordingWorkspace struct {
	calls *[]string
	path  string
	err   error
}

func (w *recordingWorkspace) Path() string {
	return w.path
}

func (w *recordingWorkspace) Cleanup(_ context.Context) error {
	*w.calls = append(*w.calls, "cleanup")
	return w.err
}

type recordingRunner struct {
	calls   *[]string
	result  runner.ExecutionResult
	err     error
	request runner.RunRequest
}

func (r *recordingRunner) Run(
	_ context.Context,
	request runner.RunRequest,
) (runner.ExecutionResult, error) {
	*r.calls = append(*r.calls, "run")
	r.request = request
	return r.result, r.err
}

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
