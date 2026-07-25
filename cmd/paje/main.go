package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	hatchetclient "github.com/hatchet-dev/hatchet/pkg/client"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"github.com/araihu/paje/internal/artifact"
	artifactfilesystem "github.com/araihu/paje/internal/artifact/filesystem"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/config"
	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/memory/mem0"
	memorymock "github.com/araihu/paje/internal/memory/mock"
	"github.com/araihu/paje/internal/policy"
	"github.com/araihu/paje/internal/processguard"
	"github.com/araihu/paje/internal/publisher"
	githubpublisher "github.com/araihu/paje/internal/publisher/github"
	"github.com/araihu/paje/internal/publisher/gitpr"
	publishermock "github.com/araihu/paje/internal/publisher/mock"
	"github.com/araihu/paje/internal/repository"
	runpkg "github.com/araihu/paje/internal/run"
	runfilesystem "github.com/araihu/paje/internal/run/filesystem"
	"github.com/araihu/paje/internal/runner"
	codexrunner "github.com/araihu/paje/internal/runner/codex"
	"github.com/araihu/paje/internal/runner/local"
	runnermock "github.com/araihu/paje/internal/runner/mock"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workflow"
	"github.com/araihu/paje/internal/workflow/codechange"
	"github.com/araihu/paje/internal/workflow/codechangehatchet"
	"github.com/araihu/paje/internal/workspace"
	"github.com/araihu/paje/internal/workspace/gitworktree"
	workspacemock "github.com/araihu/paje/internal/workspace/mock"
)

const workerName = "paje-worker"

type runtimeDependencies struct {
	memory        memory.Store
	workspaces    workspace.Manager
	resolver      repository.Resolver
	runner        runner.Runner
	runs          runpkg.Store
	artifacts     artifact.Store
	environments  environment.Builder
	verifier      verification.Runner
	profiles      map[string]repository.Profile
	templates     *template.Registry
	publisher     publisher.Publisher
	orchestrator  *workflow.Orchestrator
	codeChanges   *codechange.Service
	closeArtifact func() error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runHardened(ctx, os.Getenv, processguard.Harden); err != nil {
		log.Fatalf("paje: %v", err)
	}
}

func runHardened(ctx context.Context, getenv func(string) string, harden func() error) error {
	if harden == nil {
		return fmt.Errorf("harden worker credential boundary: guard is required")
	}
	if err := harden(); err != nil {
		return fmt.Errorf("harden worker credential boundary: %w", err)
	}
	return run(ctx, getenv)
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
	defer func() {
		if closeErr := dependencies.Close(); closeErr != nil {
			log.Printf("paje: close runtime dependencies: %v", closeErr)
		}
	}()

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

	legacyTask, betaWorkflow, err := buildHatchetWorkflows(client, dependencies)
	if err != nil {
		return err
	}
	worker, err := client.NewWorker(
		workerName,
		hatchet.WithWorkflows(legacyTask, betaWorkflow),
	)
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
	dependencies, err := buildAdapters(cfg)
	if err != nil {
		return runtimeDependencies{}, err
	}

	runs, artifacts, closeArtifact, err := buildDurableStores(cfg)
	if err != nil {
		return runtimeDependencies{}, err
	}
	dependencies.runs = runs
	dependencies.artifacts = artifacts
	dependencies.closeArtifact = closeArtifact
	failed := true
	defer func() {
		if failed {
			_ = dependencies.Close()
		}
	}()

	if err := buildBetaServices(cfg, &dependencies); err != nil {
		return runtimeDependencies{}, err
	}
	failed = false
	return dependencies, nil
}

func buildAdapters(cfg config.Config) (runtimeDependencies, error) {
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
		dependencies.resolver = deterministicResolver{}
	case "git":
		manager, err := gitworktree.New(cfg.WorkspaceRoot)
		if err != nil {
			return runtimeDependencies{}, fmt.Errorf("build workspace adapter: %w", err)
		}
		dependencies.workspaces = manager
		dependencies.resolver = manager
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

func buildDurableStores(cfg config.Config) (runpkg.Store, artifact.Store, func() error, error) {
	runs, err := runfilesystem.New(cfg.RunRoot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build run store: %w", err)
	}
	artifacts, err := artifactfilesystem.New(cfg.ArtifactRoot, cfg.ArtifactLimitBytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build artifact store: %w", err)
	}
	return runs, artifacts, artifacts.Close, nil
}

func buildBetaServices(cfg config.Config, dependencies *runtimeDependencies) error {
	environments, err := environment.NewPolicy(environment.Config{
		RuntimeRoot: cfg.RuntimeRoot,
		Source:      cfg.Environment,
		Allowed:     cfg.EnvironmentAllowlist,
		CodexHome:   cfg.CodexHome,
		CodexAgent:  cfg.RunnerAdapter == "codex",
	})
	if err != nil {
		return fmt.Errorf("build environment policy: %w", err)
	}
	dependencies.environments = environments

	limits := verification.DefaultLimits
	limits.MaxOutputBytes = cfg.CommandOutputLimitBytes
	verifier, err := verification.NewExecutor(limits)
	if err != nil {
		return fmt.Errorf("build verification executor: %w", err)
	}
	dependencies.verifier = verifier
	genericProfile, err := repository.NewGenericProfile(limits)
	if err != nil {
		return fmt.Errorf("build generic repository profile: %w", err)
	}
	goProfile, err := repository.NewGoProfile(limits)
	if err != nil {
		return fmt.Errorf("build Go repository profile: %w", err)
	}
	dependencies.profiles = map[string]repository.Profile{
		"generic": genericProfile,
		"go":      goProfile,
	}
	templates, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		return fmt.Errorf("build template registry: %w", err)
	}
	dependencies.templates = templates

	capturer, err := gitcapture.New()
	if err != nil {
		return fmt.Errorf("build Git change adapter: %w", err)
	}
	changePolicy, err := policy.NewChangePolicy(policy.Config{Workspace: cfg.WorkspaceRoot})
	if err != nil {
		return fmt.Errorf("build change policy: %w", err)
	}
	publisherAdapter, err := buildPublisher(cfg, dependencies, capturer)
	if err != nil {
		return err
	}
	dependencies.publisher = publisherAdapter

	orchestrator, err := workflow.New(dependencies.memory, dependencies.workspaces, dependencies.runner)
	if err != nil {
		return fmt.Errorf("compose legacy workflow: %w", err)
	}
	dependencies.orchestrator = orchestrator
	service, err := codechange.New(codechange.Dependencies{
		Templates: templates, Runs: dependencies.runs, Memory: dependencies.memory,
		Resolver: dependencies.resolver, Workspaces: dependencies.workspaces,
		Profiles: dependencies.profiles, Environments: environments,
		Agent: dependencies.runner, Verifier: verifier, Capturer: capturer,
		Policy: changePolicy, Artifacts: dependencies.artifacts,
		Publisher: publisherAdapter, Clock: time.Now, NewID: uuid.NewString,
	})
	if err != nil {
		return fmt.Errorf("compose beta workflow: %w", err)
	}
	dependencies.codeChanges = service
	return nil
}

func buildPublisher(
	cfg config.Config,
	dependencies *runtimeDependencies,
	capturer gitcapture.Capturer,
) (publisher.Publisher, error) {
	switch cfg.PublisherAdapter {
	case "mock":
		return publishermock.NewPublisher(publisher.Result{}, nil), nil
	case "github":
		client, err := githubpublisher.NewClient(cfg.GitHubAPIURL, cfg.GitHubToken, http.DefaultClient)
		if err != nil {
			return nil, fmt.Errorf("build GitHub client: %w", err)
		}
		credentials, err := githubpublisher.NewCredentials(
			filepath.Join(cfg.RuntimeRoot, "publisher-credentials"),
			cfg.GitHubToken,
		)
		if err != nil {
			return nil, fmt.Errorf("build GitHub credentials: %w", err)
		}
		adapter, err := gitpr.New(gitpr.Dependencies{
			Artifacts: dependencies.artifacts, Workspaces: dependencies.workspaces,
			Changes: capturer, Verification: dependencies.verifier,
			VerificationEnvironment: publisherVerificationEnvironment(cfg.Environment),
			PullRequests:            client, Credentials: credentials, PushURL: githubpublisher.PushURL,
		})
		if err != nil {
			return nil, fmt.Errorf("build GitHub Git-PR publisher: %w", err)
		}
		return adapter, nil
	default:
		return nil, fmt.Errorf("build dependencies: unknown publisher adapter %q", cfg.PublisherAdapter)
	}
}

func buildHatchetWorkflows(
	client *hatchet.Client,
	dependencies runtimeDependencies,
) (*hatchet.StandaloneTask, *hatchet.Workflow, error) {
	legacyTask, err := workflow.NewHatchetTask(client, dependencies.orchestrator)
	if err != nil {
		return nil, nil, fmt.Errorf("compose legacy Hatchet task: %w", err)
	}
	betaWorkflow, err := codechangehatchet.New(client, dependencies.codeChanges)
	if err != nil {
		return nil, nil, fmt.Errorf("compose beta Hatchet workflow: %w", err)
	}
	return legacyTask, betaWorkflow, nil
}

// Close releases the descriptor-anchored artifact store. It is safe when a
// constructor failed before the store was installed.
func (d *runtimeDependencies) Close() error {
	if d == nil || d.closeArtifact == nil {
		return nil
	}
	closeArtifact := d.closeArtifact
	d.closeArtifact = nil
	return closeArtifact()
}

type deterministicResolver struct{}

func (deterministicResolver) Resolve(
	ctx context.Context,
	repositoryURI, ref string,
) (repository.Revision, error) {
	if err := ctx.Err(); err != nil {
		return repository.Revision{}, err
	}
	repositoryURI = strings.TrimSpace(repositoryURI)
	ref = strings.TrimSpace(ref)
	if repositoryURI == "" || ref == "" {
		return repository.Revision{}, fmt.Errorf("resolve mock revision: repository URI and ref are required")
	}
	digest := sha1.Sum([]byte(repositoryURI + "\x00" + ref))
	return repository.Revision{
		RepositoryURI: repositoryURI,
		Ref:           ref,
		SHA:           hex.EncodeToString(digest[:]),
	}, nil
}

func publisherVerificationEnvironment(source map[string]string) map[string]string {
	result := make(map[string]string)
	for key, value := range source {
		if strings.HasPrefix(key, "HATCHET_") || strings.HasPrefix(key, "MEM0_") ||
			strings.HasPrefix(key, "GITHUB_") || key == "GH_TOKEN" || key == "CODEX_HOME" {
			continue
		}
		result[key] = value
	}
	return result
}
