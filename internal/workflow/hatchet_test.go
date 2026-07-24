package workflow_test

import (
	"testing"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	memorymock "github.com/araihu/paje/internal/memory/mock"
	"github.com/araihu/paje/internal/runner"
	runnermock "github.com/araihu/paje/internal/runner/mock"
	"github.com/araihu/paje/internal/workflow"
	workspacemock "github.com/araihu/paje/internal/workspace/mock"
)

func TestNewHatchetTaskRegistersTypedHandler(t *testing.T) {
	t.Parallel()

	orchestrator, err := workflow.New(
		memorymock.NewStore(nil),
		workspacemock.NewManager("/workspace"),
		runnermock.NewRunner(runner.ExecutionResult{}, nil),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	wantTask := new(hatchet.StandaloneTask)
	factory := &recordingTaskFactory{task: wantTask}

	gotTask, err := workflow.NewHatchetTask(factory, orchestrator)
	if err != nil {
		t.Fatalf("NewHatchetTask() error = %v", err)
	}
	if gotTask != wantTask {
		t.Errorf("NewHatchetTask() = %p, want %p", gotTask, wantTask)
	}
	if factory.name != "paje-agent-run" {
		t.Errorf("task name = %q, want paje-agent-run", factory.name)
	}
	if _, ok := factory.handler.(func(hatchet.Context, workflow.RunInput) (workflow.RunOutput, error)); !ok {
		t.Errorf("handler type = %T, want typed Hatchet handler", factory.handler)
	}
}

func TestNewHatchetTaskRejectsNilOrchestrator(t *testing.T) {
	t.Parallel()

	if _, err := workflow.NewHatchetTask(&recordingTaskFactory{}, nil); err == nil {
		t.Fatal("NewHatchetTask() error = nil, want validation error")
	}
}

type recordingTaskFactory struct {
	name    string
	handler any
	task    *hatchet.StandaloneTask
}

func (f *recordingTaskFactory) NewStandaloneTask(
	name string,
	handler any,
	_ ...hatchet.StandaloneTaskOption,
) *hatchet.StandaloneTask {
	f.name = name
	f.handler = handler
	return f.task
}
