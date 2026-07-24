# Pajé Initial Control Plane Design

## Status

Approved by the supplied consolidated project specification and the instruction
to continue toward the complete objective.

## Goal

Build the first deployable Pajé worker: a Go-native, self-hosted orchestration
control plane that coordinates memory retrieval, isolated Git workspaces,
black-box agent execution, result persistence, and future human approval without
coupling the application core to Hatchet, Mem0, local processes, or Kubernetes.

## Approaches Considered

### 1. Application workflow plus Hatchet adapter

Keep the deterministic orchestration sequence in an application service under
`internal/workflow`. Bind that service to a Hatchet task at the outer edge. The
daemon constructs adapters, registers the Hatchet task, and starts the worker.

This is the selected approach. It preserves the requested hexagonal boundary,
makes the workflow testable without Hatchet or PostgreSQL, and still produces a
real Hatchet listener.

### 2. Hatchet-native DAG as the application core

Model memory retrieval, workspace preparation, execution, and persistence as
four Hatchet DAG tasks. This exposes each stage in Hatchet, but Hatchet context
and serialization types would become the primary application API. Tests would
also need to reproduce more SDK behavior.

This approach is deferred until operational evidence shows that independent
retries or observability for each stage are more valuable than the clean core
boundary.

### 3. Mock-only worker placeholder

Build only the ports and a local coordinator, leaving Hatchet registration for
a later milestone. This is simple but does not satisfy the requirement that
`cmd/paje` start a Hatchet worker.

## Architecture

Pajé uses ports and adapters:

- Core contracts live in `internal/memory`, `internal/workspace`,
  `internal/runner`, and `internal/approval`.
- In-memory mocks implement every port and are safe for concurrent tests.
- The Mem0 adapter implements the memory port over Mem0's HTTP API.
- The Git worktree adapter implements workspace preparation and cleanup through
  the `git` executable.
- The local runner executes a configured black-box command with `os/exec`.
- `internal/workflow.Orchestrator` owns the service-free application sequence.
- A Hatchet binding converts a Hatchet task input into an orchestration request
  and registers the resulting task with a real worker.
- `cmd/paje` is the composition root. It validates configuration, constructs the
  current adapters, registers the workflow, and blocks until shutdown.

Hatchet remains an outer orchestration and queueing adapter. The application
workflow does not import the Hatchet SDK.

## Core Contracts

The four interfaces and data structures in the consolidated specification are
preserved exactly. Mock implementations add constructors and deterministic
inspection helpers without changing the ports.

The application workflow adds:

```go
type RunInput struct {
    TaskDescription string            `json:"task_description"`
    RepositoryURI   string            `json:"repository_uri"`
    Branch          string            `json:"branch"`
    MemoryQuery     string            `json:"memory_query"`
    MemoryLimit     int               `json:"memory_limit"`
    Tags            map[string]string `json:"tags"`
    Env             map[string]string `json:"env"`
}

type RunOutput struct {
    Output         string  `json:"output"`
    ExitCode       int     `json:"exit_code"`
    Duration       float64 `json:"duration"`
    MemoriesLoaded int     `json:"memories_loaded"`
}
```

`Orchestrator.Run` retrieves memory, prepares a workspace, executes the agent,
saves the execution result, and always attempts workspace cleanup after a
successful prepare.

## Data Flow

1. An event producer starts the Hatchet `paje-agent-run` task with `RunInput`.
2. The Hatchet handler calls `Orchestrator.Run`.
3. The orchestrator searches memory with the supplied query, limit, and tags.
4. It prepares a Git worktree for the requested repository and branch.
5. It builds an agent task description containing the original task and the
   retrieved memory context.
6. It invokes the runner inside the workspace with the requested environment.
7. It saves a concise execution record to memory using the same tags plus the
   workflow result metadata.
8. It cleans up the workspace.
9. It returns a serializable `RunOutput` to Hatchet.

Approval is part of the initial port surface and mock set, but it is not inserted
into the first workflow because the specification defines the first pipeline as
Retrieve Memory -> Prepare Workspace -> Run Agent -> Save Memory. A later
workflow can wait for a Hatchet signal and call the approval gate without
changing the existing ports.

## Error Handling

- Every adapter validates required input and wraps failures with its operation.
- The workflow stops at the first failed primary stage.
- Workspace cleanup runs with a non-canceled context after preparation so a
  canceled run does not strand worktrees.
- If both the primary operation and cleanup fail, both errors are retained.
- Local execution returns captured combined output, exit code, and duration for
  ordinary non-zero exits; startup and context failures are returned as errors.
- The Mem0 adapter treats non-2xx responses as errors and caps response bodies
  included in diagnostics.
- The daemon fails fast on invalid configuration or a missing Hatchet token.
- Hatchet worker shutdown follows process cancellation.

## Configuration

The initial daemon reads:

- `HATCHET_CLIENT_TOKEN` for the Hatchet SDK.
- Hatchet's existing SDK environment variables for self-hosted endpoint and TLS
  behavior.
- `PAJE_RUNNER_COMMAND`, defaulting to `codex`.
- `PAJE_RUNNER_ARGS`, a JSON string array defaulting to `["exec"]`.
- `PAJE_WORKSPACE_ROOT`, defaulting to the operating system temporary directory.
- `MEM0_API_KEY` and optional `MEM0_BASE_URL` when the Mem0 adapter is selected.
- `PAJE_MEMORY_ADAPTER`, supporting `mock` by default and `mem0` explicitly.
- `PAJE_WORKSPACE_ADAPTER`, supporting `mock` by default and `git` explicitly.
- `PAJE_RUNNER_ADAPTER`, supporting `mock` by default and `local` explicitly.

The default mock adapters let a newly built worker start with only a Hatchet
token, matching the initial scaffold requirement, while explicit configuration
enables the production-ready Mem0, Git worktree, and local process adapters.

## Deployment

The repository includes:

- A multi-stage, non-root Docker image containing the Pajé binary and Git.
- A Helm chart for one worker Deployment, ConfigMap-driven non-secret settings,
  Secret-driven Hatchet and Mem0 tokens, service account, resource settings, and
  optional PostgreSQL/Hatchet endpoint values.
- No bundled Hatchet Server or PostgreSQL chart dependency. Pajé connects to a
  separately managed self-hosted Hatchet installation, which keeps this chart's
  ownership focused on the worker daemon.

## Testing

- Port compile-time assertions prove each adapter implements its contract.
- Mock tests cover concurrency-safe state, deterministic responses, and
  configured failures.
- Mem0 tests use `httptest.Server`.
- Git workspace tests use temporary local repositories and real Git commands.
- Local runner tests execute the Go test helper process.
- Workflow tests verify order, context propagation, cleanup, persistence, and
  stage-specific error behavior.
- Hatchet binding tests cover input conversion without starting an external
  service; compilation against the pinned SDK validates registration.
- Configuration tests cover defaults, required values, and adapter selection.
- `go test -race ./...`, `go vet ./...`, `go build ./cmd/paje`, Helm lint/template,
  and a Docker build provide final evidence.

## Non-Goals for This Milestone

- A Kubernetes Job runner implementation.
- Slack or interactive CLI approval adapters.
- A webhook, CLI submission client, or CRD controller.
- Bundling Hatchet Server or PostgreSQL inside the Pajé Helm chart.
- Dynamic concurrency policies beyond what can be added to the registered
  Hatchet task later.

These remain explicit future adapters or triggers; none require changes to the
initial core contracts.
