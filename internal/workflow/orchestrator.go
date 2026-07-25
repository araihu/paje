// Package workflow contains Pajé's provider-neutral application pipelines.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/workspace"
)

const (
	defaultMemoryLimit = 10
	cleanupTimeout     = 30 * time.Second
)

// RunInput is the serializable input for one agent run.
type RunInput struct {
	TaskDescription string            `json:"task_description"`
	RepositoryURI   string            `json:"repository_uri"`
	Branch          string            `json:"branch"`
	MemoryQuery     string            `json:"memory_query"`
	MemoryLimit     int               `json:"memory_limit"`
	Tags            map[string]string `json:"tags"`
	Env             map[string]string `json:"env"`
}

// RunOutput is the serializable result of one agent run.
type RunOutput struct {
	Output         string  `json:"output"`
	ExitCode       int     `json:"exit_code"`
	Duration       float64 `json:"duration"`
	MemoriesLoaded int     `json:"memories_loaded"`
}

// Orchestrator coordinates memory, workspace, and runner ports.
type Orchestrator struct {
	memory     memory.Store
	workspaces workspace.Manager
	runner     runner.Runner
}

// New constructs an Orchestrator from provider-neutral ports.
func New(
	memoryStore memory.Store,
	workspaceManager workspace.Manager,
	agentRunner runner.Runner,
) (*Orchestrator, error) {
	if memoryStore == nil {
		return nil, fmt.Errorf("create orchestrator: memory store is required")
	}
	if workspaceManager == nil {
		return nil, fmt.Errorf("create orchestrator: workspace manager is required")
	}
	if agentRunner == nil {
		return nil, fmt.Errorf("create orchestrator: runner is required")
	}
	return &Orchestrator{
		memory:     memoryStore,
		workspaces: workspaceManager,
		runner:     agentRunner,
	}, nil
}

// Run executes Retrieve Memory -> Prepare Workspace -> Run Agent -> Save Memory.
func (o *Orchestrator) Run(
	ctx context.Context,
	input RunInput,
) (output RunOutput, err error) {
	if validationErr := validateInput(input); validationErr != nil {
		return RunOutput{}, validationErr
	}

	query := strings.TrimSpace(input.MemoryQuery)
	if query == "" {
		query = input.TaskDescription
	}
	limit := input.MemoryLimit
	if limit == 0 {
		limit = defaultMemoryLimit
	}

	memories, err := o.memory.Search(ctx, query, limit, cloneMap(input.Tags))
	if err != nil {
		return RunOutput{}, fmt.Errorf("run workflow: retrieve memory: %w", err)
	}

	prepared, err := o.workspaces.Prepare(ctx, input.RepositoryURI, input.Branch)
	if err != nil {
		return RunOutput{}, fmt.Errorf("run workflow: prepare workspace: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if cleanupErr := prepared.Cleanup(cleanupCtx); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("run workflow: cleanup workspace: %w", cleanupErr))
		}
	}()

	execution, err := o.runner.Run(ctx, runner.RunRequest{
		TaskDescription: buildTaskDescription(input.TaskDescription, memories),
		WorkspacePath:   prepared.Path(),
		Env:             cloneMap(input.Env),
	})
	if err != nil {
		return RunOutput{}, fmt.Errorf("run workflow: execute agent: %w", err)
	}

	resultTags := cloneMap(input.Tags)
	if resultTags == nil {
		resultTags = make(map[string]string)
	}
	resultTags["paje_exit_code"] = strconv.Itoa(execution.ExitCode)
	resultTags["paje_result"] = "completed"
	if err := o.memory.Save(
		ctx,
		formatExecutionMemory(input.TaskDescription, execution),
		resultTags,
	); err != nil {
		return RunOutput{}, fmt.Errorf("run workflow: save memory: %w", err)
	}

	return RunOutput{
		Output:         execution.Output,
		ExitCode:       execution.ExitCode,
		Duration:       execution.Duration,
		MemoriesLoaded: len(memories),
	}, nil
}

func validateInput(input RunInput) error {
	if strings.TrimSpace(input.TaskDescription) == "" {
		return fmt.Errorf("run workflow: task description is required")
	}
	if strings.TrimSpace(input.RepositoryURI) == "" {
		return fmt.Errorf("run workflow: repository URI is required")
	}
	if strings.TrimSpace(input.Branch) == "" {
		return fmt.Errorf("run workflow: branch is required")
	}
	if input.MemoryLimit < 0 || input.MemoryLimit > 1000 {
		return fmt.Errorf("run workflow: memory limit must be between 0 and 1000")
	}
	return nil
}

func buildTaskDescription(task string, memories []memory.Memory) string {
	if len(memories) == 0 {
		return task
	}

	var result strings.Builder
	result.WriteString(task)
	result.WriteString("\n\nRelevant memory:\n")
	for _, item := range memories {
		result.WriteString("- ")
		result.WriteString(item.Content)
		result.WriteByte('\n')
	}
	return strings.TrimSuffix(result.String(), "\n")
}

func formatExecutionMemory(task string, execution runner.ExecutionResult) string {
	return fmt.Sprintf(
		"Task: %s\nExit code: %d\nDuration: %.6f seconds\nOutput:\n%s",
		task,
		execution.ExitCode,
		execution.Duration,
		execution.Output,
	)
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
