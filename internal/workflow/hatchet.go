package workflow

import (
	"fmt"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

const hatchetTaskName = "paje-agent-run"

type standaloneTaskFactory interface {
	NewStandaloneTask(
		name string,
		handler any,
		options ...hatchet.StandaloneTaskOption,
	) *hatchet.StandaloneTask
}

// NewHatchetTask exposes Orchestrator.Run as a Hatchet standalone task.
func NewHatchetTask(
	factory standaloneTaskFactory,
	orchestrator *Orchestrator,
) (*hatchet.StandaloneTask, error) {
	if factory == nil {
		return nil, fmt.Errorf("create Hatchet task: task factory is required")
	}
	if orchestrator == nil {
		return nil, fmt.Errorf("create Hatchet task: orchestrator is required")
	}

	task := factory.NewStandaloneTask(
		hatchetTaskName,
		func(ctx hatchet.Context, input RunInput) (RunOutput, error) {
			return orchestrator.Run(ctx, input)
		},
	)
	if task == nil {
		return nil, fmt.Errorf("create Hatchet task: task factory returned nil")
	}
	return task, nil
}
