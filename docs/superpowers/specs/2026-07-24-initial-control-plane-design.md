# Pajé Initial Control Plane Design

## Status

Approved by the supplied consolidated project specification and the instruction
to continue toward the complete objective.

This document records the first deployable worker slice. Its fixed workflow is
still a supported leaf-execution foundation, but it is not the complete Pajé
product boundary. The canonical product scope is corrected by the
[agent-piloted and Agent Control Plane design](./2026-07-25-agent-piloted-submission-and-harness-support-design.md),
and its execution substrate is refined by the
[portable worker design](./2026-07-25-portable-worker-profiles-and-isolated-execution-design.md).

## Goal

Build the first deployable Pajé worker foundation: a Go-native, self-hosted
durable workflow service that coordinates memory retrieval, isolated Git
workspaces, black-box agent execution, result persistence, and future human
approval without coupling the application core to Hatchet, Mem0, local
processes, or Kubernetes.

Pajé's complete goal is broader: an agent-facing, provider-neutral durable
Agent Control Plane must let one control agent decompose a long specification,
create and steer multiple child agent sessions, coordinate work across multiple
projects, choose the correct parallelism primitive for each task, integrate
evidence, recover after restart, and close only after every placement has a
verified disposition and capability-appropriate terminal evidence.
The centralized service admits many such `ControlRun` values concurrently and
must isolate their identities, projects, cursors, resources, credentials,
evidence, cleanup, and status while applying bounded fair admission across the
installation.

## Product-Scope Correction

The fixed `Retrieve Memory -> Prepare Workspace -> Run Agent -> Save Memory`
sequence and a single-process Codex execution are useful execution kernels, but
they are not sufficient orchestration. They do not model a mutable dependency
graph, exclusive ownership, child-session lifecycle, scoped mailboxes,
capability-aware dispatch and observation, steering, integration order, or a
typed zero-pending-work closure gate.

The canonical Agent Control Plane adds durable `ControlRun`, `TaskGraph`,
`Task`, `ProjectRef`, ownership, `PlacementAttempt`, persistent
`AgentSession`, mailbox/event cursor, evidence, disposition, and close state.
`code-change@v1` remains an admissible durable leaf workflow beneath that
control plane; it is not the control plane itself.

### Parallelism placement

The corrected product does not equate every graph cut with a new worker
process. Before a task becomes ready, Pajé negotiates harness capabilities and
records `execution_placement`, `parallelism_primitive`, placement rationale,
capability requirements, lifecycle owner, and fallback. The exact durable
fields are `execution_placement`, `parallelism_primitive`,
`placement_rationale`, `capability_requirements`, `lifecycle_owner`, and
`fallback`. A task that can outgrow its primitive also records
`promotion_trigger`; a static task records the explicit value `none`.

The provider-neutral choices are a persistent worktree-backed session, an
ephemeral same-session subagent, a bounded harness-native parallel primitive,
or local/sequential execution. Their exact primitive values are
`persistent_session`, `ephemeral_subagent`, `harness_native_parallel`, and
`local_sequential`. The decision weighs duration and complexity,
filesystem/branch/project isolation, ownership independence, shared context,
restart survival, steering and monitoring, creation cost, concurrency limits,
conflict risk, handoff, and audit needs.

For Codex, long, restartable, mutating, isolated, or cross-project work uses a
user-visible persistent task/session; short read-only investigation or review
with strongly shared context may use an ephemeral subagent; homogeneous bounded
fan-out may use another discovered Codex parallel capability; dependent,
conflicting, integration-owned, or uneconomic work stays with the control agent
sequentially.

Placement is re-evaluated when work grows or capabilities change. A growing
subagent is checkpointed and promoted to a persistent session through an
explicit handoff. Missing capabilities follow the recorded safe fallback, and
two subagents or other primitives may never hold overlapping mutable ownership.
These rules are acceptance gates, not scheduling hints.

### Empirical orchestration contract

The execution-lifecycle, review/integration, and meta-control-plane analyses
completed on 2026-07-26 are consolidated normatively in the
[empirical orchestration contract](./2026-07-25-agent-piloted-submission-and-harness-support-design.md#empirical-orchestration-contract).
The corresponding dependency order, exact ownership boundaries, frozen inputs,
test-first gates, and placement decisions live in the
[canonical continuation DAG](../plans/2026-07-25-agent-piloted-submission-and-harness-support.md#canonical-continuation-dag).

The durable source of truth is a typed append-only `ControlAction` and
`ControlEvent` journal. `ControlRun`, task, attempt, session, candidate, review,
integration, resource, status, and close views are deterministic projections of
that journal. YAML and prose control records may be rendered for diagnosis or
audit export, but they are never authoritative and may not be patched to drive
state transitions.

Every external effect is reserved before invocation, is bound to one canonical
request digest and one exact result or ambiguity, and is reconciled by provider
observation before a retry. This applies to dispatch, message delivery,
interrupt/cancel, resource allocation and cleanup, verification, integration,
publication, runtime close, persistent archive, and target-tree verification.
Missing identity, capability, authority, evidence, or reconciliation remains a
fail-closed state rather than a weaker fallback.

Central scheduling is scoped by control run plus canonical project or actual
shared-resource namespace. Identical relative paths in unrelated repositories
do not conflict, and unrelated work does not share a global executor, gate, or
integration lock. Per-run/project quotas, fair backpressure, bounded restart
scans, and resource-specific locks ensure that a stalled, awaiting-authority,
failed, or cleanup-incomplete run cannot prevent another ready run from
advancing.

Pajé may deterministically perform validation, replay, cursor supervision,
exact ownership arbitration, immutable evidence binding, gate scheduling,
conflict-free DAG integration, and receipt-backed closure. Choosing review
scope, accepting semantic findings or residual risk, designing corrections,
resolving authored conflicts, using administrative publication authority, and
disposing of unique unmanaged evidence remain policy-assisted decisions that
require an explicit durable authorization. The provider-neutral Agent Control
Plane owns these decisions and receipts above the portable isolated execution
plane; the executor never becomes the orchestration authority.

### 2026-07-26 durable-child refreeze

The canonical continuation is refrozen at repository base
`1a5c3024e9a995103b218f54a4d81886d6e0715c`. The refreeze preserves every
integrated implementation receipt while invalidating rejected ACP-15 and
portable-runtime candidates as implementation inputs. The normative
requirements are defined exactly once in the two linked designs:

- the Agent Control Plane design owns `ACP-M09..ACP-M15` and
  `ACP-HL01..ACP-HL12`; and
- the portable-worker design owns `PW-EX01..PW-EX03`, `PW-WS01`, `PW-EN01`,
  `PW-SC01`, `PW-H01`, `PW-EV01`, `PW-PU01`, and `PW-AC01..PW-AC03`.

The [Agent Control Plane continuation plan](../plans/2026-07-25-agent-piloted-submission-and-harness-support.md#canonical-continuation-dag)
replaces the monolithic ACP-15 writer with disjoint admission, isolation, and
scheduler writers followed by a parent-local combined gate. It also assigns
the empirical Home Lab contract to existing downstream owners rather than
creating overlapping `.1` writers. The
[portable-runtime continuation registry](../plans/2026-07-25-portable-worker-profiles-and-isolated-execution.md#canonical-remaining-work-registry)
replaces the historical fixed-session checklist with dependency-ordered
durable writers, read-only independent review gates, and a local final gate.

Closure is dynamic. It enumerates every canonical requirement ID and every
canonical node, requires one terminal disposition and the primitive-specific
close evidence for each placement, and rejects missing, duplicate, stale, or
unknown entries. No fixed number of sessions, criteria, tasks, or attempts is a
close invariant.

## Approaches Considered

### 1. Application workflow plus Hatchet adapter

Keep the deterministic orchestration sequence in an application service under
`internal/workflow`. Bind that service to a Hatchet task at the outer edge. The
daemon constructs adapters, registers the Hatchet task, and starts the worker.

This is the selected approach for the initial leaf-worker slice. It preserves
the requested hexagonal boundary, makes the workflow testable without Hatchet
or PostgreSQL, and still produces a real Hatchet listener.

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

- The provider-neutral Agent Control Plane owns durable decomposition,
  capability-aware work coordination, persistent child-session specialization,
  cross-project scope, evidence integration, and closure. It does not import
  Hatchet or a specific agent runtime.
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

The phrase "orchestration" in this initial slice describes ordering inside one
leaf workflow. Agent-facing orchestration is the higher-level durable control
loop that dispatches, observes, sends to, waits for, interrupts/cancels, and
closes work through a provider-neutral `AgentHarness`. `AgentSession` and
archive are the specialization for persistent sessions; ephemeral subagents,
native fan-out, and local work retain their own capability-appropriate
lifecycle. Shell-free command execution remains behind the separate executor
layer.

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

These contracts do not become the durable Agent Control Plane by gaining more
fixed phases. The higher-level model and capability-aware work lifecycle are
specified in the agent-piloted design and may invoke this workflow as a leaf
task.

## Data Flow

### Initial leaf workflow

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

### Canonical agent-control flow

1. A control agent opens a durable `ControlRun` from a long specification.
2. Pajé validates a `TaskGraph`, exact `ProjectRef` values, dependency cuts,
   exclusive ownership, frozen inputs, integration order, and combined gates.
3. For each ready task, Pajé creates a durable `PlacementAttempt` and selects
   exactly one advertised primitive. A persistent session completes the exact
   runtime-ID handshake; an ephemeral subagent records a returned runtime ID
   only when one exists; native fan-out uses bounded dispatch and deterministic
   aggregation; local/sequential work creates no child.
4. Persistent sessions use completion callbacks plus independent cursor-aware
   wait/read recovery. Ephemeral subagents use ack/send/callback only when
   advertised and complete through wait/read plus terminal/runtime-close
   evidence. Native fan-out closes with a terminal aggregate or cancel receipt.
5. Parent steering and dependency handoffs are appended to scoped mailboxes;
   unrelated projects may proceed concurrently without sharing workspaces,
   credentials, ownership, or messages.
6. Pajé records exact evidence and one disposition for every task and placement
   attempt, survives coordinator restart without duplicating runtime work, and
   reruns combined gates in integration order.
7. The `ControlRun` closes only when the graph is terminal, every persistent
   session has an archive receipt, every ephemeral runtime is closed, every
   native fan-out is terminally aggregated, no local work is active, and every
   field of the typed pending-work gate is zero.

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
- Agent Control Plane acceptance separately verifies capability negotiation,
  required task placement fields, the concrete Codex primitive mapping,
  subagent-to-session promotion, missing-capability fallback, concurrency
  limits, denial of overlapping mutable subagents, a durable placement attempt
  for all four primitives, persistent runtime-ID/callback/archive semantics,
  optional ephemeral identity with required runtime close, deterministic
  fan-out aggregation/cancel, no local child creation, and typed zero-pending-
  work closure.
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
- Dynamic agent-session concurrency in this first worker milestone.

These were non-goals for the initial worker milestone, not product-wide
exclusions. The later Agent Control Plane design makes bounded multi-agent and
multi-project orchestration core scope without weakening the initial
provider-neutral leaf-workflow contracts.
