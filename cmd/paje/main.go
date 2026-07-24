package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	hatchetclient "github.com/hatchet-dev/hatchet/pkg/client"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/araihu/paje/internal/config"
	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/memory/mem0"
	memorymock "github.com/araihu/paje/internal/memory/mock"
	"github.com/araihu/paje/internal/runner"
	codexrunner "github.com/araihu/paje/internal/runner/codex"
	"github.com/araihu/paje/internal/runner/local"
	runnermock "github.com/araihu/paje/internal/runner/mock"
	"github.com/araihu/paje/internal/workflow"
	"github.com/araihu/paje/internal/workspace"
	"github.com/araihu/paje/internal/workspace/gitworktree"
	workspacemock "github.com/araihu/paje/internal/workspace/mock"
)

const workerName = "paje-worker"

type runtimeDependencies struct {
	memory     memory.Store
	workspaces workspace.Manager
	runner     runner.Runner
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Getenv); err != nil {
		log.Fatalf("paje: %v", err)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	dependencies, err := buildDependencies(cfg)
	if err != nil {
		return err
	}
	orchestrator, err := workflow.New(
		dependencies.memory,
		dependencies.workspaces,
		dependencies.runner,
	)
	if err != nil {
		return fmt.Errorf("compose workflow: %w", err)
	}

	client, err := hatchet.NewClient(hatchetclient.WithToken(cfg.HatchetClientToken))
	if err != nil {
		return fmt.Errorf("create Hatchet client: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if closeErr := client.Close(closeCtx); closeErr != nil {
			log.Printf("paje: close Hatchet client: %v", closeErr)
		}
	}()

	task, err := workflow.NewHatchetTask(client, orchestrator)
	if err != nil {
		return err
	}
	worker, err := client.NewWorker(workerName, hatchet.WithWorkflows(task))
	if err != nil {
		return fmt.Errorf("create Hatchet worker: %w", err)
	}

	log.Printf(
		"paje: starting worker %q with memory=%s workspace=%s runner=%s",
		workerName,
		cfg.MemoryAdapter,
		cfg.WorkspaceAdapter,
		cfg.RunnerAdapter,
	)
	if err := worker.StartBlocking(ctx); err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("run Hatchet worker: %w", err)
	}
	return nil
}

func buildDependencies(cfg config.Config) (runtimeDependencies, error) {
	var dependencies runtimeDependencies

	switch cfg.MemoryAdapter {
	case "mock":
		dependencies.memory = memorymock.NewStore(nil)
	case "mem0":
		options := make([]mem0.Option, 0, 1)
		if cfg.Mem0BaseURL != "" {
			options = append(options, mem0.WithBaseURL(cfg.Mem0BaseURL))
		}
		store, err := mem0.New(cfg.Mem0APIKey, options...)
		if err != nil {
			return runtimeDependencies{}, fmt.Errorf("build memory adapter: %w", err)
		}
		dependencies.memory = store
	default:
		return runtimeDependencies{}, fmt.Errorf("build dependencies: unknown memory adapter %q", cfg.MemoryAdapter)
	}

	switch cfg.WorkspaceAdapter {
	case "mock":
		dependencies.workspaces = workspacemock.NewManager(cfg.WorkspaceRoot)
	case "git":
		manager, err := gitworktree.New(cfg.WorkspaceRoot)
		if err != nil {
			return runtimeDependencies{}, fmt.Errorf("build workspace adapter: %w", err)
		}
		dependencies.workspaces = manager
	default:
		return runtimeDependencies{}, fmt.Errorf(
			"build dependencies: unknown workspace adapter %q",
			cfg.WorkspaceAdapter,
		)
	}

	switch cfg.RunnerAdapter {
	case "mock":
		dependencies.runner = runnermock.NewRunner(runner.ExecutionResult{}, nil)
	case "local":
		executor, err := local.New(cfg.RunnerCommand, cfg.RunnerArgs...)
		if err != nil {
			return runtimeDependencies{}, fmt.Errorf("build runner adapter: %w", err)
		}
		dependencies.runner = executor
	case "codex":
		executor, err := codexrunner.New(cfg.RunnerCommand)
		if err != nil {
			return runtimeDependencies{}, fmt.Errorf("build runner adapter: %w", err)
		}
		dependencies.runner = executor
	default:
		return runtimeDependencies{}, fmt.Errorf("build dependencies: unknown runner adapter %q", cfg.RunnerAdapter)
	}

	return dependencies, nil
}
