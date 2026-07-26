# Pajé Agent-Piloted Submission and Harness Support Design

## Status

Approved by the product direction supplied on 2026-07-25 and grounded in the
independent positioning audit of repository commit `df9137b`. This document
supersedes ambiguous product-positioning language; it does not replace the
durable `code-change@v1` design.

Corrected on 2026-07-25 to make agent-facing orchestration explicit core scope.
The earlier submit/status/wait/cancel proposal remains a required leaf-run
surface, but submission alone is not orchestration. This revision productizes
the durable coordination behavior defined by the installed
`orchestrating-control-planes` protocol while preserving the approved portable
worker, secret-broker, isolated-executor, approval, artifact, and publisher
boundaries.
The installed skill is the behavioral reference used to write this contract,
not a runtime dependency or a provider-specific API imported by Pajé.

The repository state inspected for this design is `86537ae`. The current beta
implementation remains the source of truth for shipped behavior until the
implementation plan for this design is completed.

## Canonical Product Position

Pajé is a self-hosted durable Agent Control Plane for code agents. It lets one
control agent turn a long specification into a dependency graph, coordinate
parallel child sessions across one or more unrelated projects, steer and
integrate their work, survive coordinator restart, and prove that every
placement reached its capability-appropriate terminal state and every
persistent child session was archived. Pajé is implemented in Go, while its
workflow and agent-harness contracts are repository-language-neutral:
`code-change@v1`
supports an explicit `generic` profile for any toolchain available in the
worker image and a first-class `go` profile with Go-specific discovery and
defaults.

Pajé is designed to be piloted by an agent through an installable harness
integration. The current beta is still triggered directly through Hatchet and
executes one Codex process at a time. Codex becomes the first fully integrated
harness only when the plugin, deterministic client, durable Agent Control Plane,
capability-aware agent-work adapter, and live multi-agent acceptance in this
design ship.
Additional dedicated harnesses are a planned extension point, not a currently
shipped capability.

The canonical short form is:

> Pajé is implemented in Go, works with repositories in any language through
> explicit profiles and checks, and provides a durable Agent Control Plane for
> long-running, multi-agent code changes. The beta uses Hatchet as its direct
> leaf-workflow trigger and Codex as its first dedicated execution harness.
> Scoped agent-side orchestration, including capability-aware placement,
> persistent child-session lifecycle, and multi-project coordination, is planned
> by this design.

## Why This Design Exists

The independent audit and subsequent control-plane review found six different
truths being collapsed into one
marketing claim:

1. Pajé currently launches an agent; the agent does not yet have a supported
   Pajé skill, hook, API, or CLI with which to launch Pajé.
2. The workflow is language-neutral, but the worker image does not contain
   every language toolchain and Go remains a privileged built-in profile.
3. Codex is the first named, packaged, deterministic, live-tested execution
   harness, although the low-level `local` runner existed first.
4. The core runner port admits more adapters, but no second dedicated harness
   has been selected, packaged, or accepted.
5. A submit/status/wait/cancel API makes one durable leaf run accessible; it
   does not let a control agent create, message, steer, reconcile, integrate, or
   archive a graph of child sessions.
6. Fixed internal workflow phases and one black-box Codex process do not model
   multi-project ownership, dependency handoffs, restart-safe child identity,
   callback recovery, or graph closure.

The missing product layer is therefore not another workflow template or only a
submission gateway. It is a provider-neutral durable Agent Control Plane
between an agent harness, session runtimes, and existing leaf workflows, plus
an explicit definition of what "supported harness" means.

## Goals

- Let one external control agent deliberately create and resume a durable
  `ControlRun` from a long specification.
- Persist the task graph, exact projects, dependencies, ownership, child
  sessions, placement attempts, scoped messages, evidence, dispositions,
  integration order, and close state independently of one coordinator process.
- Let the control agent discover per-primitive capabilities and dispatch,
  observe, send to, wait for, interrupt/cancel, and close work, including
  persistent child-session archive, without receiving a runtime-provider or
  Hatchet credential.
- Support ready independent children across multiple and unrelated projects
  concurrently, with separate workspaces, credentials, ownership, and scoped
  communication.
- Preserve deliberate submit, inspect, wait, and cancel as the leaf
  `code-change@v1` run surface.
- Ship an installable Codex integration made of focused Pajé skills, bounded
  lifecycle hooks, and a deterministic `paje-agent` client.
- Put a narrow Pajé authorization boundary in front of Hatchet so an agent
  never receives a worker, publisher, memory, or Hatchet service credential.
- Preserve the existing outer Hatchet envelope and the provider-neutral
  `code-change@v1` application service.
- Bind every submission idempotency key to one principal and one canonical
  request for the lifetime of the binding.
- Reject accidental or malicious recursive submission with server-computed
  run depth and fail-closed defaults.
- Define execution certification and full-integration certification in terms
  that can be tested for Codex and reused for another harness.
- Make current and future support status explicit across the root README, Helm
  metadata, site, operator docs, and regression tests.
- Establish an evidence gate for selecting a second dedicated harness without
  naming one based only on architectural preference.

## Non-Goals

- Replacing Hatchet, exposing Hatchet as part of Pajé's public agent protocol,
  or moving Hatchet types into provider-neutral packages.
- Giving an agent approval authority, publisher credentials, direct artifact
  store access, or unrestricted workflow-trigger permissions.
- Automatically inferring that any user prompt should become a Pajé run.
- Allowing repository-owned files or hooks to mint, widen, or read submission
  credentials.
- Adding an arbitrary workflow or shell-command DSL, multiple worker replicas,
  automatic merge, or direct pushes to target branches. The bounded
  `TaskGraph` is an agent-coordination model, not a command DSL.
- Bundling every repository-language toolchain in the standard worker image.
- Calling the generic `local` process adapter a certified harness.
- Selecting or implementing a second dedicated harness before the evidence
  gate in this design has produced a committed decision record.
- Weakening artifact binding, approval binding, restart recovery, publisher
  isolation, cancellation, or credential isolation from the beta design.

## Product Vocabulary

### Trigger

A trigger accepts a validated request to start a durable workflow. Hatchet is
the current trigger provider. The planned submission service is the stable
Pajé-facing boundary over that provider.

### Agent Control Plane

The provider-neutral durable service that owns a `ControlRun`, its `TaskGraph`,
placement attempts, persistent `AgentSession` lifecycles, scoped mailboxes,
evidence, dispositions, and close gate. Hatchet may schedule leaf workflows,
but Hatchet tasks and events are not the public Agent Control Plane model.

### Control run

A durable orchestration instance created from one long goal or specification.
It owns a graph revision, safe principal and policy binding, project
references, event cursor, task, placement-attempt, and persistent-session state,
integration evidence, and close state.

### Project reference

An immutable, principal-scoped repository identity and exact base SHA plus its
workspace and credential policy. Two `ProjectRef` values may name unrelated
repositories. A child receives only the references assigned to its task.

### Task graph and task

`TaskGraph` is a versioned DAG of bounded `Task` records. A task contains one
goal, predecessor IDs, assigned projects, frozen inputs, exclusive ownership,
forbidden paths, acceptance gates, integration order, and current state. Graph
updates use durable compare-and-swap and may not invalidate an active task's
frozen boundary.

### Ownership

A validated set of exclusive mutable paths or resources plus explicit
parent-owned and forbidden paths. Ready tasks with overlapping ownership do
not run concurrently. Shared integration files remain parent-owned unless
exactly one task owns them and every other task is forbidden from editing them.

### Agent session

A persistent, separately addressable logical child bound to one task and one
runtime-returned child ID. It records capability snapshot, dispatch
fingerprint, runtime-ID acknowledgement, lifecycle state, mailbox cursor,
completion state, disposition, and archive receipt. `AgentSession` is created
only for `persistent_session`; an ephemeral subagent, native fan-out item, or
execution-harness process is not automatically an `AgentSession`.

### Placement attempt

A durable execution record created for every task placement. It binds the
selected primitive, capability snapshot, rationale, lifecycle owner, action
IDs, optional runtime work IDs, observation cursor, terminal evidence,
disposition, and primitive-specific close evidence. It never invents a session
or child identity that the harness did not return.

### Mailbox and event cursor

An append-only, control-run-scoped stream of bounded messages and lifecycle
events. Every read and wait accepts an `after_cursor`; every returned event has
a monotonic cursor. A task or session can address only its parent, children,
declared dependencies, and explicitly shared control-run channels.

### Evidence, disposition, and close state

Evidence binds exact branches, SHAs, artifacts, test results, reports, reviews,
and integration gates to their producing task and placement attempt. A
terminal attempt receives exactly one disposition: `integrated`, `handed_off`,
or `discarded` with proof that no unique work remains. A control run closes
only after every task and placement attempt is terminal, every persistent
session is dispositioned and archived, every ephemeral runtime is closed,
every native fan-out is terminally aggregated, no local task remains active,
combined gates pass, and the durable typed pending-work gate is zero.

### Repository profile

A repository profile turns a repository and explicit check declarations into
preflight facts and shell-free verification commands. A profile does not imply
that its toolchain is installed in the worker image.

### Execution adapter

An implementation of `runner.Runner` that invokes an agent command as a black
box. `local` is an execution adapter, but it has no harness-specific protocol,
packaging, authentication policy, or live acceptance promise.

### Execution-certified harness

A named harness adapter that passes every execution certification requirement
in this document, including deterministic invocation, transcript completion,
cancellation, sandboxing, credential isolation, packaging, and live
acceptance.

### Fully integrated harness

An execution-certified harness that also ships an installable agent-side
integration with certified orchestration and leaf skills, hooks, deterministic
control/submission client, recursion protection, and live leaf plus multi-agent
Agent Control Plane acceptance.

## Current and Future Support Matrix

Status terms are deliberately limited to `current`, `planned by this design`,
`future after evidence gate`, and `not planned`.

### Trigger support

| Surface | Status at `86537ae` | Target status | Product boundary |
| --- | --- | --- | --- |
| Direct `paje-code-change-v1` Hatchet workflow trigger | current | current, retained for operators | Provider-specific outer adapter |
| Pajé submission HTTP API | absent | planned by this design | Stable agent and automation boundary |
| `paje-agent` submit/status/wait/cancel client | absent | planned by this design | Deterministic client over the HTTP API |
| Pajé Agent Control Plane API | absent | planned by this design | Durable graph, placement attempt, persistent session, mailbox, evidence, and close boundary |
| `paje-agent` capabilities and work dispatch/observe/send/wait/interrupt/close lifecycle | absent | planned by this design | Deterministic client over provider-neutral control APIs |
| Codex skill-driven leaf submission | absent | planned by this design | Deliberate single-run entrypoint |
| Codex long-spec multi-agent orchestration | absent | planned by this design | First supported Agent Control Plane entrypoint |
| Generic webhook, CRD, or arbitrary event router | absent | not planned in this milestone | Separate future adapters |

### Repository language and profile support

| Profile or claim | Status at `86537ae` | Target status | Qualification |
| --- | --- | --- | --- |
| `generic` profile | current | current | Requires at least one explicit shell-free check |
| `go` profile | current | current | Built-in module discovery and `GOWORK=off` defaults |
| Any repository language | current at contract level | current | Required executables must exist in the worker image |
| Every language runtime preinstalled | absent | not planned | Operators derive an image with required toolchains |
| Repository-owned shell fragments | rejected | rejected | Commands remain executable-plus-arguments structures |

### Harness support

| Harness surface | Status at `86537ae` | Target status | Support label |
| --- | --- | --- | --- |
| Codex worker-side adapter | current, dedicated and live-tested | formally recertified | execution-certified |
| Codex agent-side skills, hooks, and client | absent | planned by this design | required for full integration |
| Codex agent-work lifecycle adapter | absent | planned by this design | capability-aware work operations plus persistent-session specialization |
| Codex end-to-end Agent Control Plane flow | absent | planned by this design | first fully integrated harness |
| `local` runner | current | current | low-level adapter, not a certified harness |
| Second dedicated harness | unselected | future after evidence gate | no support claim before live acceptance |
| Additional unnamed harnesses | architectural extension point | future direction | not current support |

## Invariants Carried Forward

The following constraints are inherited from the beta design and are not open
for reinterpretation:

- `internal/workflow/codechange` and the ports it consumes do not import
  Hatchet, HTTP, Codex plugin, or submission-credential types.
- Hatchet remains a thin outer adapter. Durable IDs and references cross task
  boundaries; workspaces and secret values do not.
- The outer `run_id` owns Hatchet status idempotency. The nested
  `idempotency_key` remains bound to the template and canonical input.
- A completed or ambiguously interrupted agent execution is never
  automatically retried.
- Cancellation preserves `context.Canceled` or `context.DeadlineExceeded`,
  terminates descendants, and records a truthful terminal result.
- Run records and artifacts are monotonic, restart-safe, bounded, redacted, and
  bidirectionally validated.
- Approval remains bound to run, artifact digest, base SHA, target branch, and
  publication mode.
- Publisher credentials appear only after repository-controlled verification
  and only in the isolated publisher process and trusted publisher-owned Git
  configuration.
- Agent and verification children cannot read Hatchet, Mem0, GitHub,
  submission-gateway, runtime-provider service, executor, Git, or SSH
  credentials.
- Token-bearing operations never execute in a repository or worktree that ran
  repository-controlled code.
- The packaged Linux worker fails closed if its process-inspection guard cannot
  be installed.
- The beta remains a single-worker, single-writable-filesystem installation.

## Approaches Considered

### 1. Durable Agent Control Plane, leaf gateway, and installable harness integration

A separate `paje-gateway` process authenticates a narrowly scoped Pajé token,
persists the agent-control graph and placement-attempt lifecycle, validates and
binds leaf submissions, computes lineage and depth, and calls provider-neutral
agent-harness and trigger ports. A Hatchet adapter implements the leaf trigger.
An installable
Codex plugin ships orchestration and leaf skills plus hooks that call a small
`paje-agent` HTTP client and map negotiated placement to discovered runtime
capabilities.

This is the selected approach. It provides a stable Pajé protocol, keeps broad
Hatchet and runtime-provider credentials out of the agent, gives graph,
placement, idempotency, recursion, recovery, and closure policy authoritative
owners, and leaves the code-change and isolated-execution cores unchanged.

### 2. Ship only submit/status/wait/cancel

The plugin and client could expose one durable `code-change@v1` run while
leaving multi-agent coordination to conversation prose. This is rejected
because it has no durable graph, placement decision, session identity,
mailbox/cursor, steering, dependency handoff, integration evidence, restart
recovery, disposition, archival, or close gate. A submission API remains a
required leaf surface, not the orchestration product.

### 3. Let the agent trigger Hatchet directly

The skill could call the Hatchet SDK or API with `paje-code-change-v1`.
This avoids a new server, but exposes Hatchet concepts and credentials to the
agent, cannot express Pajé-specific scopes cleanly, and duplicates
idempotency/depth rules in each harness integration.

This approach is rejected.

### 4. Embed agent submission in the worker process

The worker could expose HTTP alongside its Hatchet listener and reuse the
worker token and in-memory composition. This reduces deployment objects, but it
combines public ingress with the credential-bearing execution process, makes
least-privilege separation difficult, and increases the blast radius of an HTTP
or authentication flaw.

This approach is rejected. The gateway and worker use separate processes,
service accounts, credentials, and Kubernetes workloads.

## Architecture

```text
Codex control conversation
  -> Pajé Codex plugin
       -> orchestration and leaf-run skills
       -> bounded lifecycle hooks
       -> paje-agent client
            -> HTTPS + scoped Pajé bearer token
                 -> paje-gateway HTTP adapter
                      -> token authenticator
                      -> provider-neutral controlplane.Service
                           -> controlplane.Store
                           -> AgentHarness port
                                -> Codex capability-aware work lifecycle
                           -> provider-neutral submission.Service
                                -> submission.Trigger port
                                     -> Hatchet trigger adapter
                                          -> paje-code-change-v1 leaf workflow
                                               -> worker profile + secret broker
                                               -> isolated executor
                                               -> execution harness adapter
```

There are five independently testable planes:

1. **Agent Control Plane:** persists `ControlRun`, `TaskGraph`, `Task`,
   `ProjectRef`, ownership, `PlacementAttempt`, persistent `AgentSession`,
   mailbox/event cursor, evidence, disposition, and close state.
2. **Agent harness plane:** discovers semantic capabilities and dispatches,
   observes, sends to, waits for, interrupts, and closes work according to the
   selected primitive. It does not execute repository commands or manufacture
   session semantics for runtimes that do not expose them.
3. **Submission plane:** authenticates a principal, validates one leaf request,
   binds idempotency and lineage, starts or reuses a workflow, exposes safe
   status, and requests cancellation.
4. **Durable workflow plane:** the existing typed `code-change@v1` phases,
   stores, artifacts, approval, and publication.
5. **Execution plane:** worker profiles, secret broker, executor, and
   execution-harness command/parser adapter for isolated leaf work.

Failure or replacement in one plane must not require provider types in another.

The worker-profile, secret-broker, and isolated-executor work is a required
lower-layer foundation. It is not deleted or redesigned here. It proves how one
leaf command runs safely; it does not prove child-session orchestration.

## Provider-Neutral Durable Agent Control Plane

### Durable model

The core model is provider-neutral and safe to persist:

```go
type ControlRun struct {
    ID              string
    PrincipalID     string
    GoalDigest      string
    GraphRevision   uint64
    Status          ControlStatus
    EventCursor     uint64
    Close           CloseState
}

type TaskGraph struct {
    ControlRunID    string
    Revision        uint64
    Tasks           []Task
    IntegrationOrder []string
    CombinedGates   []Gate
}

type Task struct {
    ID              string
    Goal            string
    DependsOn       []string
    Projects        []ProjectRef
    Ownership       Ownership
    Placement       ExecutionPlacement
    FrozenInputs    []FrozenInput
    Acceptance      []Gate
    State           TaskState
}

type PlacementAttempt struct {
    ID                 string
    TaskID             string
    Primitive          string
    CapabilitySnapshot CapabilitySnapshot
    LifecycleOwner     string
    RuntimeWorkIDs     []string
    LastCursor         string
    State              AttemptState
    TerminalEvidence   EvidenceRef
    CloseEvidence      WorkCloseEvidence
}

type AgentSession struct {
    ID                    string
    TaskID                string
    HarnessID             string
    RuntimeChildID        string
    RuntimeIDAcknowledged bool
    LastCursor            string
    State                 SessionState
    Disposition           Disposition
    ArchiveReceipt        string
}
```

`PlacementAttempt` is created for every selected primitive, including
`local_sequential`, and is the durable execution/audit record for placement.
`RuntimeWorkIDs` is empty unless the harness actually returns identities.
`AgentSession` is a persistent specialization referenced only by a
`persistent_session` attempt.

`ProjectRef` binds canonical repository identity, immutable base ref and SHA,
workspace policy, credential scope, and allowed ownership. `Ownership` includes
exclusive mutable paths or resources, parent-owned paths, and forbidden paths.
The real records also bind stable schema versions, request digests, timestamps,
attempts, safe diagnostics, and compare-and-swap versions.

`ControlRun` states are `open`, `closing`, `closed`, `blocked`, and `canceled`.
Task states are `pending`, `ready`, `dispatched`, `active`, `needs_input`,
`completed`, `failed`, and `canceled`. Persistent session state is recorded
independently because a task can require a replacement session without losing
the original session's evidence.

### Graph, readiness, and multi-project isolation

Before dispatch, Pajé validates:

- every predecessor and immutable input is complete;
- every `ProjectRef` resolves to its exact base SHA;
- ready writers have disjoint ownership;
- the task has standalone acceptance, known integration order, and a combined
  parent gate;
- the selected dispatch or local lifecycle is authorized by principal, harness,
  project, and action scope; and
- expected work repays its primitive-specific setup, monitoring, review, and
  cleanup cost.

Tasks in unrelated repositories may run concurrently. Their workspaces,
worker profiles, credentials, messages, evidence namespaces, and ownership are
isolated. A child assigned project A cannot read or address project B merely
because both belong to one control run. Cross-project communication requires a
declared dependency or a parent-authored handoff containing only bounded safe
evidence.

Graph revisions use compare-and-swap. A revision may add a newly discovered
task or dependency, but it cannot rewrite the goal, base SHA, ownership, or
frozen inputs of an active task. A changed contract pauses dependent tasks and
creates a new explicit handoff checkpoint.

### Capability negotiation and execution placement

Every `Task` records a placement decision before it becomes ready:

```go
type ExecutionPlacement struct {
    ParallelismPrimitive   string
    ExecutionPlacement     string
    Rationale              string
    CapabilityRequirements []string
    LifecycleOwner         string
    Fallback               string
}
```

The durable JSON field names are `parallelism_primitive`,
`execution_placement`, `placement_rationale`, `capability_requirements`,
`lifecycle_owner`, and `fallback`. Missing or implied values invalidate
dispatch.

Placement is provider-neutral and selects exactly one primitive:

1. `persistent_session`: a separately addressable, restartable, normally
   worktree-backed child session;
2. `ephemeral_subagent`: bounded work inside the current session with strongly
   shared context and no independent durable lifecycle;
3. `harness_native_parallel`: another bounded fan-out primitive advertised by
   the harness; or
4. `local_sequential`: execution by the control agent without parallel
   dispatch.

The decision evaluates at least:

- expected duration, complexity, and likelihood of scope growth;
- need for a separate filesystem, branch, worktree, or unrelated project;
- ownership independence and conflict risk;
- how much parent context must be shared;
- restart/survival and durable audit requirements;
- communication, steering, monitoring, and handoff needs;
- creation and cleanup cost;
- runtime and principal concurrency limits; and
- integration dependencies and combined-gate order.

The default policy is:

| Primitive | Choose when | Reject when |
| --- | --- | --- |
| `persistent_session` | Work is long, restartable, mutating, independently owned, worktree-isolated, cross-project, communication-heavy, or audit-critical. | Required create/read/send/wait/interrupt/archive or isolation capability is absent and no equivalent certified capability exists. |
| `ephemeral_subagent` | Investigation, short review, read-only analysis, or tightly bounded work benefits from strongly shared context and needs no independent restart. | Work may outlive the parent turn, needs a separate project/worktree, has conflicting mutable ownership, or needs durable steering/archival. |
| `harness_native_parallel` | The harness advertises a bounded homogeneous fan-out with exact inputs, bounded results, and independent proof; setup cost is lower than sessions. | Items mutate overlapping state, require bespoke steering, or the primitive lacks bounded cancellation/result identity. |
| `local_sequential` | Dependencies, shared files, uncertain boundaries, integration ownership, low task value, or capability/setup cost makes parallelism unsafe or uneconomic. | None; this is the fail-safe placement when continuing locally is authorized and safe. |

For Codex, the concrete mapping is:

- a user-visible Codex task/thread with an isolated worktree maps to
  `persistent_session`; it must expose runtime IDs and the discovered
  create/read/send/wait/interrupt/archive lifecycle;
- a Codex local subagent maps to `ephemeral_subagent`; it is preferred for
  short read-only investigation or review with shared context and no conflicting
  writer;
- Codex-advertised bounded parallel tool calls, batch waits, or other
  semantically equivalent fan-out map to `harness_native_parallel` only after
  capability discovery records their identity, limits, cancellation, and
  result semantics; and
- work performed by the current Codex control agent maps to
  `local_sequential`.

Verification must prove the chosen Codex primitive actually satisfies every
recorded capability requirement. A label is not evidence.

Placement is re-evaluated when duration, scope, ownership, dependencies,
capabilities, or concurrency limits change. An ephemeral subagent that grows
into mutating, long-running, cross-project, restartable, or independently
steered work is stopped at a safe checkpoint and promoted to a new persistent
session through an explicit evidence handoff. It does not continue mutating in
parallel with its replacement.

Fallbacks are explicit and fail closed:

- if persistent-session capability is missing, cross-project or
  isolation-required mutation remains pending or blocked; it is never silently
  downgraded to a same-session subagent;
- if harness-native fan-out is unavailable, the task runs sequentially or uses
  persistent sessions when their cost is justified;
- if ephemeral subagents are unavailable, short work runs sequentially; and
- if concurrency is exhausted, ready tasks remain queued in deterministic
  priority order rather than exceeding the limit.

Every mutable ownership unit has at most one active lifecycle owner across all
placements. Ephemeral subagents are read-only by default; a mutating subagent
must hold an exclusive task ownership lease, and two subagents may never mutate
the same files or resources concurrently.

### Capability-aware agent harness boundary

The provider-neutral agent harness is distinct from the execution harness:

```go
type AgentHarness interface {
    Capabilities(context.Context, Principal, string) (HarnessCapabilities, error)
    Dispatch(context.Context, DispatchWorkRequest) (DispatchWorkResult, error)
    Observe(context.Context, ObserveWorkRequest) (WorkEvents, error)
    Send(context.Context, SendWorkRequest) (MessageReceipt, error)
    Wait(context.Context, WaitWorkRequest) (WorkEvents, error)
    Interrupt(context.Context, InterruptWorkRequest) (InterruptReceipt, error)
    Close(context.Context, CloseWorkRequest) (WorkCloseEvidence, error)
}
```

`HarnessCapabilities` is discovered at runtime per primitive. It records
dispatch, observe/read, wait, optional runtime identity, acknowledgement,
send, callback, cursor, interrupt/cancel, idempotency, restart, runtime close,
persistent archive, deterministic aggregation, isolation, and concurrency
semantics. It also records whether local/sequential execution remains
available. `Dispatch`, `Observe`, `Send`, `Wait`, `Interrupt`, and `Close` are
capability-gated operations: a primitive may support only the subset its
recorded contract requires. The service rejects an unsupported operation; it
never simulates one or invents a child/session identity.

Requests contain a `ControlRun` ID, task ID, `ProjectRef` IDs, exact dispatch
prompt digest, ownership and frozen-input digest, stable action ID, and bounded
timeouts. They never contain an executable, shell fragment, raw service
credential, or provider-native object. Repository and verification commands
remain shell-free `executor.Command` values behind the isolated executor
layer.

The four primitive contracts are executable and distinct:

- `persistent_session` requires dispatch/create, an exact runtime-ID handshake,
  cursor-aware observe/read and wait, send, interrupt, completion callback plus
  independent polling, and a confirmed archive receipt. It creates an
  `AgentSession`.
- `ephemeral_subagent` requires bounded dispatch and wait/read observation.
  The parent records a runtime child ID when the harness returns one, but
  acknowledgement, send, cursor, callback, and interrupt are used only when
  advertised. Completion requires terminal result evidence and confirmed
  runtime close; it never requires archive and never becomes an
  `AgentSession`.
- `harness_native_parallel` requires one bounded deterministic dispatch,
  declared concurrency, exact input/result correspondence, deterministic
  terminal aggregation, and defined cancel semantics. Pajé records runtime item
  IDs only when returned and otherwise does not create child or session
  identities. Its close evidence is the terminal aggregate or cancel receipt,
  never archive.
- `local_sequential` creates no runtime child and calls no harness dispatch.
  The control agent owns the durable `PlacementAttempt`; its terminal evidence
  and inactive local-work marker close the attempt.

Codex is the first `AgentHarness`. The plugin discovers current Codex task,
subagent, and bounded-parallel capabilities semantically and uses `paje-agent`
to durably prepare and record each supported action. If the Codex runtime later
exposes a direct stable server-side API, that adapter may move behind the
gateway without changing the domain contract.

### Runtime identity and primitive-specific acknowledgement

Pajé durably reserves the `PlacementAttempt`, action, and dispatch fingerprint
before invoking the runtime. For `persistent_session`, the actual child ID is
never inferred from parent, source thread, worktree, or prompt metadata:

1. call the discovered create capability once with the reserved action;
2. register the exact runtime-returned child ID immediately;
3. send a registration message containing the exact parent and child IDs;
4. require the child to acknowledge that exact ID; and
5. reject a completion envelope that uses an unregistered or unacknowledged ID.

If the coordinator fails after create but before registration, it does not
blindly create a second child. It reconciles by stable action ID or dispatch
fingerprint through available list/read capabilities. If the runtime cannot
prove the result, Pajé records `ambiguous_create`, blocks the task, and requires
operator disposition.

For `ephemeral_subagent`, Pajé registers the exact runtime child ID only when
one is returned. It requests acknowledgement or sends messages only when those
capabilities were advertised. A native fan-out without returned item identity
is tracked by attempt/action ID and exact item ordinals, not synthetic child
IDs. `local_sequential` performs no child registration.

### Callback, cursor-aware recovery, aggregation, and steering

A persistent-session prompt contains the parent address and a compact
completion envelope with child ID, terminal status, branch, base/head/pushed
SHA, owned paths, test evidence, handoffs, concerns, report, and recommended
parent action. The persistent child sends that callback before its final
response. The parent still stores a monotonic mailbox/event cursor and
independently performs bounded cursor-aware wait/read recovery; callbacks are
primary notification, not the only source of truth.

An ephemeral subagent uses callback, acknowledgement, send, or cursor semantics
only when its capability snapshot advertises them. Its required completion path
is bounded wait/read observation followed by terminal-result and runtime-close
evidence. Native fan-out completes through deterministic aggregation of every
declared item or a durable cancel receipt. Duplicate events are idempotent by
attempt plus runtime event identity when available, otherwise by the stable
action and result digest.

Parent steering is an append-only message event bound to one task revision.
Steering may clarify the goal, provide an approved frozen checkpoint, request
fixes, interrupt obsolete work, or publish a dependency handoff. It may not
silently widen project scope or ownership. Dependency handoffs bind producer
task, exact evidence digest, consumer task, and acknowledgement.

### Evidence, integration, restart recovery, and closure

`Evidence` records exact branch and SHA, owned-path diff digest, artifact or
report reference, command/result summaries, review findings, and integration
gate results. The parent verifies reported state and integrates in graph order;
focused child tests never replace combined gates.

On coordinator restart Pajé reloads the graph, placement attempts, capability
snapshots, action ledger, returned runtime IDs, applicable acknowledgement
flags, cursors, callbacks, aggregation state, dispositions, and close evidence.
It resumes each primitive through its advertised observe/wait contract, never
repeats a completed lifecycle action, and fail-closes an ambiguous dispatch,
interrupt/cancel, runtime-close, aggregate, or persistent archive outcome until
reconciliation.

Closure is a durable transition, not a UI convention. `CloseState` proves:

1. all graph tasks are terminal;
2. every placement attempt has terminal evidence and exactly one disposition;
3. every persistent session has exactly one disposition and confirmed archive
   receipt;
4. all dependency handoffs and combined gates are acknowledged and complete;
5. every ephemeral subagent has terminal-result and runtime-close evidence;
6. every native fan-out has a terminal deterministic aggregate or cancel
   receipt;
7. no local/sequential placement remains active; and
8. the durable typed pending-work gate is exactly zero.

```go
type PendingWorkGate struct {
    PersistentSessionsUnarchived int
    EphemeralAttemptsOpen        int
    NativeFanoutsUnaggregated    int
    LocalAttemptsActive          int
    TotalPendingWork             int
}
```

All five fields must be zero. A missing persistent archive receipt, ephemeral
runtime-close proof, native aggregate/cancel receipt, or inactive-local marker
leaves the run `closing` with `cleanup_incomplete`; Pajé must not claim clean
completion.

## Provider-Neutral Leaf Submission Domain

`internal/submission` owns the stable application contract for one durable leaf
workflow. It is consumed by the Agent Control Plane when a task needs
`code-change@v1`, but it does not own the task graph, placement attempts, or
persistent child-session lifecycle:

```go
type Principal struct {
    CredentialID string
    Subject      string
    UserID       string
    AppID        string
    Repositories []RepositoryScope
    Actions      map[Action]bool
    Harnesses    map[string]bool
    MaxDepth     int
}

type Origin struct {
    Harness     string `json:"harness"`
    SessionID   string `json:"session_id"`
    TurnID      string `json:"turn_id"`
    ParentRunID string `json:"parent_run_id,omitempty"`
}

type SubmitRequest struct {
    IdempotencyKey string
    Template       template.ID
    Input          json.RawMessage
    Origin         Origin
}

type TriggerRequest struct {
    RunID string
    Input json.RawMessage
}

type Trigger interface {
    Start(context.Context, TriggerRequest) (TriggerReference, error)
    Inspect(context.Context, TriggerReference) (TriggerState, error)
    Cancel(context.Context, TriggerReference) error
}

type Store interface {
    Reserve(context.Context, Reservation) (Record, ReserveResult, error)
    BindTrigger(context.Context, string, TriggerReference) (Record, error)
    Load(context.Context, string) (Record, error)
    LoadByKey(context.Context, string, string) (Record, error)
    MarkCancellationRequested(context.Context, string, time.Time) (Record, error)
}
```

`Principal` is already authenticated when it reaches the service. HTTP bearer
parsing and secret comparison remain outer adapters. Repository scopes are
parsed canonical repository identities, never unvalidated string prefixes.

`submission.Service` performs the following work in order:

1. Require an authenticated principal and allowed action.
2. Strictly decode the public envelope and reject unknown fields.
3. Resolve the template from the existing registry and canonicalize its typed
   input.
4. Overwrite `tags.user_id` and `tags.app_id` from the principal; caller values
   may match but cannot widen the scope.
5. Check canonical repository identity, publication mode, and harness against
   the principal.
6. Resolve the parent record, root, and server-computed depth.
7. Canonically encode the complete bound request and calculate its SHA-256
   digest.
8. Reserve the principal-scoped idempotency key.
9. Start Hatchet only for the reservation owner.
10. Persist the Hatchet reference before returning `202 Accepted`.

No HTTP or Hatchet value appears in `codechange.Input`.

## Agent-Facing HTTP Contract

The gateway exposes only versioned JSON endpoints. Agent-control endpoints are:

```text
GET  /v1/capabilities
POST /v1/control-runs
GET  /v1/control-runs/{control_run_id}
POST /v1/control-runs/{control_run_id}/tasks
POST /v1/control-runs/{control_run_id}/tasks/{task_id}/attempts
GET  /v1/control-runs/{control_run_id}/attempts/{attempt_id}?after_cursor=...
POST /v1/control-runs/{control_run_id}/attempts/{attempt_id}/messages
POST /v1/control-runs/{control_run_id}/attempts:wait
POST /v1/control-runs/{control_run_id}/attempts/{attempt_id}/interrupt
POST /v1/control-runs/{control_run_id}/attempts/{attempt_id}/close
POST /v1/control-runs/{control_run_id}/evidence
POST /v1/control-runs/{control_run_id}/close
```

Every mutating request requires a stable action idempotency key. Reads and waits
accept an event cursor when the selected primitive advertises one and return
the next cursor. Attempt creation durably records placement before dispatch.
The action result records only runtime identities actually returned. Message,
interrupt/cancel, runtime-close, aggregate, and persistent archive evidence bind
the action ID, attempt ID, applicable runtime IDs, and resulting cursor or
provider revision. `close` is primitive-specific; it maps to persistent archive,
ephemeral runtime close, native terminal aggregation/cancel, or a local inactive
marker.

The existing leaf-run endpoints remain:

```text
POST /v1/submissions
GET  /v1/submissions/{run_id}
POST /v1/submissions/{run_id}/cancel
GET  /healthz
GET  /readyz
```

There is no generic workflow-name endpoint, arbitrary event endpoint, admin
endpoint, approval endpoint, artifact-file endpoint, or credential-minting
endpoint in the agent-facing server. There is also no endpoint that accepts an
arbitrary executable or command line; command execution remains an executor
responsibility.

### Submit request

Required headers:

```text
Authorization: Bearer <scoped-paje-token>
Content-Type: application/json
Idempotency-Key: <16-to-128-byte-stable-client-key>
```

Body:

```json
{
  "template": {
    "name": "code-change",
    "version": 1
  },
  "origin": {
    "harness": "codex",
    "session_id": "stable-harness-session-id",
    "turn_id": "stable-harness-turn-id"
  },
  "input": {
    "task_description": "Raise the API timeout and update its tests.",
    "repository_uri": "https://github.com/example/service.git",
    "base_ref": "main",
    "memory_query": "service timeout conventions",
    "memory_limit": 5,
    "tags": {
      "user_id": "must-match-token-scope",
      "app_id": "must-match-token-scope"
    },
    "profile": "generic",
    "checks": [
      {
        "name": "test",
        "directory": ".",
        "executable": "npm",
        "args": ["test"],
        "timeout": "10m",
        "required": true
      }
    ],
    "publication": {
      "mode": "artifact"
    }
  }
}
```

The public envelope does not accept `run_id`, `root_run_id`, `depth`,
Hatchet metadata, a raw nested `idempotency_key`, secret values, or arbitrary
environment values.

Successful first submission returns `202`; an exact reuse returns `200`:

```json
{
  "api_version": "v1",
  "run_id": "paje_7q5m7jzsw4k9v3wx7f0v9p6m8c",
  "status": "accepted",
  "reused": false,
  "depth": 0,
  "root_run_id": "paje_7q5m7jzsw4k9v3wx7f0v9p6m8c"
}
```

### Status response

Status is a safe projection of the submission record and Hatchet run details.
Terminal results contain the durable `codechange.Result`, not unbounded task
logs or raw provider diagnostics.

```json
{
  "api_version": "v1",
  "run_id": "paje_7q5m7jzsw4k9v3wx7f0v9p6m8c",
  "status": "succeeded",
  "depth": 0,
  "root_run_id": "paje_7q5m7jzsw4k9v3wx7f0v9p6m8c",
  "result": {
    "run_id": "paje_7q5m7jzsw4k9v3wx7f0v9p6m8c",
    "status": "succeeded"
  }
}
```

The gateway may report `accepted`, `queued`, `running`,
`awaiting_approval`, `cancellation_requested`, `succeeded`, `failed`,
`canceled`, or `declined`. Provider-specific statuses are mapped at the
Hatchet adapter.

### Cancellation

Cancellation is idempotent and principal-scoped:

- A terminal run returns its existing terminal state.
- A first valid request records `cancellation_requested`, calls the trigger
  adapter, and returns `202`.
- A repeat returns the existing request state and does not create another
  provider action.
- `cancellation_requested` is not presented as `canceled`; only the durable
  workflow result can make that terminal claim.
- Cancellation must preserve the existing stage compensation and descendant
  termination behavior.

### HTTP errors

Errors use a bounded stable shape:

```json
{
  "error": {
    "code": "idempotency_conflict",
    "message": "the idempotency key is already bound to different input"
  }
}
```

The stable codes are `invalid_request`, `unauthenticated`, `forbidden`,
`not_found`, `idempotency_conflict`, `depth_exceeded`,
`run_not_cancelable`, `capability_unavailable`, `concurrency_exhausted`,
`placement_invalid`, `ambiguous_create`, `cleanup_incomplete`,
`provider_unavailable`, and `internal`.
Diagnostics never include bearer tokens, service credentials, raw provider
bodies, repository credentials, or unbounded transcripts.

## Scoped Agent Credentials

The gateway uses high-entropy static v1 bearer tokens because the beta is a
single self-hosted installation. This is not an OAuth or multi-tenant identity
system.

A token has the form:

```text
paje_v1_<public-id>.<32-random-bytes-base64url>
```

The gateway Secret stores only:

- public credential ID
- SHA-256 hash of the high-entropy secret
- human subject
- exact `user_id` and `app_id`
- allowed canonical repositories
- allowed leaf actions: `submit:artifact`, `submit:pull_request`, `read`,
  `cancel`
- allowed control actions: `control:create`, `task:create`, `work:dispatch`,
  `work:observe`, `work:send`, `work:wait`, `work:interrupt`, `work:close`,
  `evidence:write`, and `control:close`
- allowed harness IDs
- allowed project identities and cross-project communication edges
- maximum child depth
- optional expiry

Lookup uses the public ID and constant-time hash comparison. Unknown,
expired, malformed, and mismatched credentials return the same
`unauthenticated` response. Clear tokens are issued once through an
operator-only offline command that reads and writes secrets through standard
input/output, never command-line arguments.

The agent receives only this Pajé token. The gateway receives only its own
Hatchet producer credential and token-policy Secret. The worker retains its
separate Hatchet worker, Mem0, GitHub, and Codex credentials. Active Secret
names must remain pairwise distinct.

## Deterministic Idempotency

The Codex client calculates its default client key from stable harness facts:

```text
hex(sha256(
  "paje-codex-submit-v1\0" +
  session_id + "\0" +
  turn_id + "\0" +
  canonical_repository + "\0" +
  base_ref
))
```

The API scopes that key by credential ID. It does not trust the key as a
request digest. Instead it:

1. canonicalizes the authenticated, scope-bound request;
2. computes `request_digest = sha256(canonical_request)`;
3. derives a stable public run ID from
   `sha256("paje-run-v1", credential_id, idempotency_key)`;
4. durably reserves `(credential_id, idempotency_key)` to
   `(run_id, request_digest)`;
5. reuses the record only when both digest and principal match;
6. returns `idempotency_conflict` for any changed request.

The reservation is written before the Hatchet call. A crash after reservation
but before provider binding is reconciled by the same reservation owner; it
never allocates a second run ID. A crash after Hatchet accepts the trigger is
reconciled by the deterministic outer `run_id` and Hatchet's existing
status-based idempotency.

The gateway passes the stable outer `run_id` to Hatchet and sets the nested
`codechange.Input.IdempotencyKey` to the same principal-scoped submission
identity. The existing workflow therefore independently rejects mismatched
owners or changed canonical input.

Idempotency bindings are not automatically garbage-collected in v1. Removing a
run's detailed status must never make its key reusable.

## Recursion and Run Depth

The caller may provide only an optional `origin.parent_run_id`. It cannot
provide depth or root identity.

For a root submission:

```text
depth = 0
root_run_id = run_id
```

For a child submission, the service loads the parent and computes:

```text
depth = parent.depth + 1
root_run_id = parent.root_run_id
```

The service rejects a child when:

- the parent does not exist or is outside the same principal;
- the parent belongs to another harness scope;
- the parent chain is corrupt;
- the credential does not permit child submission;
- the computed depth exceeds the lesser of the credential limit and system
  limit;
- the request tries to use its own deterministic run ID as a parent.

The default credential limit is `0`, which permits root submissions only. The
v1 system maximum is `1`. Raising either value is an explicit operator action.
This lineage depth applies to nested leaf workflow submissions. It does not
limit the number of sibling `AgentSession` records in a `ControlRun`; session
fan-out is instead bounded by principal policy, graph readiness, ownership,
per-project concurrency, and global session quotas.

Defense in depth at the harness layer:

- The Codex skill refuses to submit when it detects a Pajé execution context
  unless an explicit parent is present and the credential permits children.
- Pajé-launched agent and verification processes never receive a submission
  token or gateway credential.
- Non-secret `PAJE_RUN_ID`, `PAJE_ROOT_RUN_ID`, and `PAJE_RUN_DEPTH` may be
  passed as execution context only after environment-policy tests prove they
  cannot widen authority.
- Lifecycle hooks never submit automatically.

## Hatchet Trigger Adapter

`internal/submission/hatchet` implements `submission.Trigger` and owns every
Hatchet SDK type.

`Start` calls `RunNoWait` for exactly `paje-code-change-v1` with:

```json
{
  "run_id": "<stable-paje-run-id>",
  "input": "<strict normalized code-change@v1 object>"
}
```

It stores the returned Hatchet external run ID as `TriggerReference`. `Inspect`
maps Hatchet status and the `finalize` task output into the provider-neutral
`TriggerState`. It rejects a completed Hatchet run whose final output is
missing, malformed, bound to another Pajé run ID, or inconsistent with terminal
status. `Cancel` targets only the stored external run ID.

The adapter uses a Hatchet producer credential distinct from the worker
credential. The credential stays in `paje-gateway`; it is never returned,
logged, persisted in submission records, or forwarded to the agent.

## Gateway Deployment

The Helm chart adds one optional `paje-gateway` Deployment and ClusterIP
Service. It is disabled by default until its ingress, credentials, and
persistent store are configured.

The gateway:

- runs as the same non-root UID/GID and read-only-root security posture as the
  worker;
- has its own ServiceAccount with token automount disabled;
- receives no Mem0, GitHub, Codex runtime-provider service, worker Hatchet,
  repository, executor, Git, or SSH credential;
- receives a distinct Hatchet producer Secret and submission-policy Secret;
- persists control graphs, placement records, action ledgers, event cursors,
  evidence, dispositions, primitive-specific close evidence including
  persistent archive receipts, submission reservations, and provider bindings
  atomically;
- uses distinct non-overlapping control and submission roots on the persistent
  volume;
- supports exactly one replica in v1 because the filesystem store uses
  process-local compare-and-swap;
- exposes only the documented HTTP paths;
- sets request-body, header, timeout, connection, and response-size limits;
- logs request ID, credential ID, safe control/run/task/attempt IDs, optional
  persistent-session ID, action, result code, and duration, never authorization
  headers or input bodies.

TLS terminates at an operator-controlled ingress or trusted private network
boundary. The chart does not create public ingress by default.

## `paje-agent` Client

`cmd/paje-agent` is the deterministic harness-facing client. It is not a worker
and does not contain Hatchet code.

Commands:

```text
paje-agent capabilities
paje-agent control create --file <path-or->
paje-agent control status --control-run <id> [--after-cursor <cursor>]
paje-agent task create --control-run <id> --file <path-or->
paje-agent work dispatch --control-run <id> --task <id> --file <path-or->
paje-agent work observe --control-run <id> --attempt <id> [--after-cursor <cursor>]
paje-agent work send --control-run <id> --attempt <id> --file <path-or->
paje-agent work wait --control-run <id> --file <path-or->
paje-agent work interrupt --control-run <id> --attempt <id>
paje-agent work close --control-run <id> --attempt <id> --file <path-or->
paje-agent session create|read|send|wait|interrupt|archive ... # persistent-session shortcuts
paje-agent evidence add --control-run <id> --file <path-or->
paje-agent control close --control-run <id>
paje-agent submit --file <path-or-> [--idempotency-key <key>]
paje-agent status --run <run-id>
paje-agent wait --run <run-id> --timeout <duration>
paje-agent cancel --run <run-id>
paje-agent hook session-start
paje-agent hook user-prompt-submit
paje-agent hook stop
```

Lifecycle mutations use explicit two-phase forms: `--prepare` returns the
stable action document, and `--complete --action <id> --file <result-or->`
records the bounded result of the dynamically discovered runtime capability.
Read and wait use the same pattern when the runtime call occurs in the plugin.
The exact action ID binds both phases and prevents implicit repeat.

Configuration:

- `PAJE_API_URL` identifies the gateway.
- `PAJE_AGENT_TOKEN_FILE` points to a regular file owned by the user with mode
  `0600`; the client rejects symlinks and group/world-readable files.
- `PAJE_AGENT_TOKEN` is an explicit CI-only alternative and is never printed.
- Plugin hooks use `PLUGIN_DATA` for non-secret run context.

The client:

- never accepts a token on the command line;
- never stores a token in the repository, hook state, transcript, or request
  body;
- writes JSON to stdout and bounded human diagnostics to stderr;
- uses stable exit codes for success, invalid input, authentication,
  conflict/depth denial, provider unavailability, timeout, cancellation, and
  terminal workflow failure;
- implements bounded polling with jitter and `Retry-After`;
- preserves caller cancellation;
- verifies that every response run ID matches the requested run;
- treats malformed or successful-but-incomplete terminal responses as errors.
- derives a stable action ID from control run, task/attempt, lifecycle verb,
  graph revision, and canonical request digest;
- records only exact runtime-returned IDs and uses acknowledgement only when the
  primitive advertises it, rather than deriving IDs from surrounding metadata;
- passes stored event cursors on observe and wait when advertised, deduplicates
  repeated events, and never substitutes cadence polling for required callback
  recovery;
- never repeats an ambiguous dispatch, interrupt/cancel, runtime-close,
  aggregation, or archive action without reconciliation evidence; and
- refuses `control close` unless the server returns durable combined-gate,
  disposition, primitive-specific close evidence, and a zero typed
  `PendingWorkGate`.

For Codex, lifecycle commands are two-phase. `paje-agent` durably prepares the
action, the skill invokes the matching dynamically discovered runtime
capability, and the client records the bounded result. The client never shells
out to manufacture a runtime action and never receives a Codex service
credential.

## Installable Codex Integration

Codex is integrated through a skills-and-hooks plugin:

```text
integrations/codex/paje/
├── .codex-plugin/
│   └── plugin.json
├── hooks/
│   └── hooks.json
└── skills/
    ├── orchestrating-with-paje/
    │   ├── SKILL.md
    │   └── agents/
    │       └── openai.yaml
    └── using-paje/
        ├── SKILL.md
        └── agents/
            └── openai.yaml
```

The plugin depends on a compatible `paje-agent` binary and a separately
provisioned scoped token. Installation does not imply hook trust; Codex must
show the hooks for review and the user or administrator must trust the exact
definitions.

This layout follows the documented Codex plugin, skill, and hook surfaces:

- skills are focused `SKILL.md` workflows: one for durable multi-agent
  control, one for a deliberate leaf run;
- installable packages use `.codex-plugin/plugin.json`;
- plugin hooks default to `hooks/hooks.json`;
- hook commands receive `PLUGIN_ROOT` and `PLUGIN_DATA`;
- command hooks are reviewed and trusted before they run.

### Codex skill behavior

`orchestrating-with-paje` triggers when a user gives Codex a substantial
multi-step specification and asks Pajé to coordinate it, or invokes the skill
by name. `using-paje` remains the explicit single-leaf-run skill.

The orchestration skill must:

1. Confirm explicit orchestration intent and create or resume one `ControlRun`.
2. Discover the available primitive-specific dispatch, observe/read, send,
   wait, interrupt/cancel, callback, aggregation, runtime-close, and persistent
   archive capabilities semantically; never assume provider tool names.
3. Materialize the complete graph, exact project/base SHAs, ownership,
   frozen inputs, dispatch prompts, primitive-specific identity and completion
   rules, integration order, combined gates, polling cursors, and cleanup owner
   before dispatch.
4. Negotiate capabilities and record each task's
   `execution_placement`, `parallelism_primitive`, rationale, capability
   requirements, lifecycle owner, and fallback using the verified Codex
   mapping; re-evaluate and promote growing subagent work when required.
5. Dispatch only ready tasks with disjoint ownership and standalone proof,
   including tasks in unrelated projects only when credentials and
   communication scopes are separate.
6. For persistent sessions, register and acknowledge every runtime-returned
   child ID and pair callbacks with cursor-aware read/wait recovery. For
   ephemeral subagents, register an ID and use ack/send/callback only when
   advertised, then require wait/read terminal and runtime-close evidence. For
   native fan-out, require deterministic terminal aggregation/cancel without
   invented identities. For local work, create no child.
7. Send parent steering and dependency handoffs through scoped mailboxes and
   durably record acknowledgements.
8. Verify child branches, SHAs, owned diffs, tests, and evidence before assigning
   an `integrated`, `handed_off`, or proven-safe `discarded` disposition.
9. Integrate in graph order, run combined gates, survive client or coordinator
   restart from durable attempts and applicable cursors, and collect the exact
   close evidence required by each primitive.
10. Refuse clean completion until every persistent session has an archive
    receipt, every ephemeral subagent has runtime-close evidence, every native
    fan-out is terminally aggregated or canceled, no local work is active, and
    every field of `PendingWorkGate` is zero.

The leaf `using-paje` skill must:

1. Confirm explicit submission intent; never infer it solely from repository
   presence.
2. Inspect the current repository identity, immutable base ref, available
   toolchain, and requested publication mode.
3. Select `generic` unless the Go-specific defaults are useful; generic
   requires explicit shell-free checks.
4. Refuse to include secret values, shell fragments, or unsupported
   environment keys.
5. Read stable session and turn identity written by the hooks and derive the
   default idempotency key.
6. Detect existing Pajé execution context and apply the recursion rules.
7. Submit once, persist only the returned run ID and safe context, then inspect
   or wait through `paje-agent`.
8. On terminal success, summarize the artifact or publication result and
   continue the user's task with that durable evidence.
9. On failure, cancellation, decline, conflict, or timeout, report the stable
   failure code and safe next action without silently resubmitting.

### Codex lifecycle hooks

Hooks support the skill but never create authority or automatic side effects:

- `SessionStart` loads safe active `ControlRun` and leaf-run context from
  `PLUGIN_DATA`, optionally performs one cursor-aware read, and emits a concise
  resume message.
- `UserPromptSubmit` records only `session_id`, `turn_id`, `cwd`, and active
  control/run IDs and last safe cursor in a mode-`0600` context file. It does
  not store prompt text.
- `Stop` performs at most one bounded control or leaf status check and emits a
  system message. It does not block stop indefinitely and does not dispatch,
  send, interrupt/cancel, close work, archive a persistent session, close a
  control run, submit, cancel, approve, publish, or trust another hook.

Hook state excludes tokens, prompts, raw transcripts, repository credentials,
and workflow input. Malformed state is ignored with a safe warning, not
executed.

## Codex as the First Fully Integrated Harness

Codex currently has:

- a dedicated `runner.Runner` adapter;
- fixed non-interactive arguments;
- JSONL terminal-message parsing;
- exact environment construction;
- process-group cancellation;
- workspace-write sandbox selection;
- a pinned worker-image installation;
- dedicated authentication handling;
- unit, integration, image, and opt-in live acceptance.

That is enough to describe Codex as the first execution-certified harness once
the formal suite below ratifies the evidence. It is not yet enough to describe
Codex as fully integrated.

Codex becomes the first fully integrated harness only when:

1. the execution adapter passes the formal conformance suite;
2. the plugin is installable and discoverable;
3. the skills act only on explicit intent and produce canonical control or
   leaf requests;
4. the agent-harness adapter covers capability discovery and the
   primitive-specific dispatch, observe, send, wait, interrupt/cancel, close,
   aggregation, and persistent archive contracts without exposing a Codex
   service credential;
5. the hooks obey their bounded, non-submitting lifecycle contract;
6. the Agent Control Plane preserves placement attempts, capability-gated
   identity/acknowledgement and messages, required callbacks plus cursor
   recovery, evidence, disposition, restart recovery, and typed zero-pending-
   work closure;
7. a scoped token cannot exceed its project, repository, identity, action,
   harness, or leaf-depth policy;
8. a live Codex control session creates at least three persistent child sessions
   across at least two projects, exercises an ephemeral subagent and native
   fan-out additionally, exchanges capability-supported messages, handles
   parent steering, integrates evidence, resumes after coordinator restart,
   archives every persistent session, and records close evidence for every
   other attempt;
9. the same acceptance completes without any Hatchet, worker, runtime-provider,
   executor, or publisher service-token exposure.

## Formal Harness Certification

Certification is evidence attached to one harness ID, adapter version, harness
binary version, worker image revision, and plugin version. Passing one version
does not automatically certify another.

### Execution certification

#### EC-1: Deterministic non-interactive invocation

- The adapter uses one exact executable and argument vector without a shell.
- Interactive prompts and TTY dependencies are disabled.
- User configuration is ignored or replaced by an operator-controlled,
  versioned configuration.
- The command, protocol mode, sandbox mode, and output limit are unit-tested.

#### EC-2: Transcript and completion semantics

- Machine-readable output has a documented or pinned protocol.
- A terminal success requires an explicit completed agent response.
- Exit zero without terminal completion is a failure.
- Malformed protocol frames, truncation before completion, contradictory
  terminal frames, and extra output after a terminal frame are classified
  explicitly.
- Raw transcript and final response are bounded and remain distinguishable.

#### EC-3: Cancellation and retry safety

- Context cancellation reaches the harness process and every descendant.
- Tests prove descendants terminate within a bounded interval.
- Canceled execution is never marked completed.
- A started, completed, timed-out, or ambiguous agent attempt is not
  automatically retried.
- Cancellation identity survives every error wrapper.

#### EC-4: Sandboxing and workspace boundary

- The adapter selects the narrowest supported workspace-write sandbox.
- The harness cannot write outside the prepared workspace and per-attempt
  runtime directories.
- Unsupported sandbox platforms fail closed.
- Adversarial tests attempt path escape, sibling-worktree mutation, and
  persistence outside the attempt.

#### EC-5: Credential isolation

- The agent receives only harness authentication and approved non-secret
  values required for that stage.
- It cannot read worker, Hatchet, submission, Mem0, publisher, Git, SSH, or
  verification credentials through environment variables, arguments, files,
  process inspection, Git configuration, or inherited descriptors.
- Verification receives no harness authentication.
- Durable records, artifacts, logs, and diagnostics contain neither values nor
  reversible encodings of credentials.

#### EC-6: Packaging and version pinning

- The worker image installs an exact harness version and records it.
- The image is non-root, reproducible from an exact Pajé revision, and
  compatible with the chart security context.
- Harness auth seed material is read-only and copied only into a private
  writable runtime location when the harness requires writes.
- Package/license terms permit the documented installation path.

#### EC-7: Unit and integration conformance

- A reusable contract suite exercises invocation, completion, malformed output,
  output limits, non-zero exit, cancellation, descendant cleanup, sandbox
  escape, and credential denial.
- Adapter-specific tests add only protocol-specific fixtures and assertions.
- Tests run without live credentials unless explicitly marked opt-in.

#### EC-8: Live acceptance

- Opt-in is explicit and missing prerequisites fail when opted in.
- A real harness changes a disposable repository from an immutable base.
- Required checks run, the artifact reproduces the exact changed tree, and the
  source checkout remains unchanged.
- No descendant, worktree, runtime directory, or credential artifact remains.
- The evidence records exact Pajé, image, adapter, and harness versions.

### Agent-pilot certification

#### AP-1: Installable packaging

- The integration has a valid manifest, focused orchestration and leaf-run
  skills, declared hooks, and documented deterministic client dependency.
- Installation, discovery, update, removal, and hook-trust flows are tested.

#### AP-2: Skill behavior

- Explicit and implicit trigger prompts are tested.
- Ambiguous intent does not create a control run or submit a leaf run.
- Canonical profile/check selection, idempotency, status handling, and safe
  failure instructions are tested.
- Capability discovery, graph/ownership validation, primitive-specific runtime
  identity and completion, persistent callback recovery, native aggregation,
  steering, integration, disposition, and typed closure instructions are
  tested.
- The skill never handles worker service credentials.

#### AP-3: Hook behavior

- Each supported event receives recorded fixture input and produces valid
  bounded JSON output.
- Hooks never dispatch, send, interrupt/cancel, close work, archive a persistent
  session, close a control run, submit, approve, publish, or cancel.
- Hook state permissions, corruption handling, concurrent invocation, and
  absence of prompt/transcript/token persistence are tested.
- Changed hooks require a new trust decision; documentation never recommends
  bypassing trust for normal use.

#### AP-4: Idempotency and recursion

- Repeating one Codex turn produces one Pajé run.
- Changing the canonical request under the same key returns conflict.
- Root-only credentials reject child submission.
- Allowed child submission computes depth server-side and stops at the exact
  configured maximum.
- A Pajé-launched agent has no submission credential by default.

#### AP-5: Live end-to-end acceptance

- A real originating harness session invokes the installed skill.
- The skill submits through the gateway with a scoped token.
- Hatchet starts exactly one durable run.
- The certified harness adapter completes the task.
- The originating session obtains and reports the bound terminal result.
- Adversarial probes confirm no worker or gateway service credential appears in
  the originating session, worker child, artifact, logs, or hook state.

#### AP-6: Durable multi-agent control

- One real originating Codex control agent receives a long specification and
  creates at least three acknowledged `persistent_session` children across at
  least two `ProjectRef` values.
- The same graph records all placement fields for one short read-only
  `ephemeral_subagent` task, one bounded `harness_native_parallel` fan-out, and
  one dependency-conflicted `local_sequential` task.
- A fixture grows the ephemeral task beyond its original bounds and proves
  promotion to a persistent session through an explicit handoff with no
  overlapping mutation. A second fixture removes one advertised capability and
  exercises the recorded safe fallback.
- At least two children run concurrently with disjoint ownership; one project
  is unrelated to the other and has separate workspace and credential scope.
- Persistent sessions complete the runtime-ID handshake, exchange scoped
  messages, send completion callbacks, and are independently recovered through
  cursor-aware wait/read. One child-to-child dependency handoff is acknowledged
  and the parent sends at least one steering event.
- The ephemeral subagent records its runtime child ID only if returned, uses
  ack/send/callback only if advertised, and completes through wait/read plus
  terminal/runtime-close evidence without archive.
- Native fan-out proves bounded dispatch, exact result correspondence,
  deterministic aggregation, and cancel semantics without synthetic child or
  session identity. Local/sequential placement creates no child.
- A coordinator restart occurs with children active. The resumed controller
  uses durable placement attempts, returned runtime IDs, and applicable stored
  cursors, creates no duplicate work, and retains all evidence.
- The parent verifies and integrates child evidence in graph order, runs the
  combined gates, assigns every attempt a disposition, archives every
  persistent session, proves runtime closure for subagents, terminal aggregation
  for fan-out, no active local work, and every `PendingWorkGate` field zero
  before close.

## Second Dedicated Harness

The second harness reuses the same boundaries:

- a dedicated `internal/runner/<harness>` adapter;
- exact adapter configuration at the composition root;
- isolated authentication handling in `internal/environment`;
- an exact worker-image version pin or a separately versioned image;
- the reusable execution conformance suite;
- a harness-specific opt-in live acceptance test;
- an installable skill and hooks only if the integration is intended to be
  fully integrated rather than execution-only.

No candidate is selected in this design. Selection requires a committed
evidence record scoring:

1. demonstrated user or deployment demand;
2. stable, documented non-interactive invocation;
3. machine-readable terminal completion semantics;
4. cancellable process behavior;
5. enforceable workspace sandboxing;
6. isolatable authentication and credential transport;
7. exact redistributable or reproducible packaging;
8. license compatibility;
9. ability to run a disposable live acceptance fixture;
10. lifecycle skill/hook surface if full integration is intended.

A candidate fails the gate if any security-critical item lacks evidence. The
decision record must cite observed commands, protocol fixtures, packaging
terms, and live-probe results. Only then may a candidate-specific spec and
implementation plan name directories, configuration keys, image packages, and
acceptance variables.

This design therefore commits to the second-harness architecture and acceptance
bar, not to an unevidenced vendor choice.

## Test Strategy

### Agent Control Plane

- strict `ControlRun`, `TaskGraph`, `Task`, `ProjectRef`, ownership,
  `PlacementAttempt`, persistent `AgentSession`, mailbox/event cursor,
  evidence, disposition, and close-state decoding and canonicalization;
- required execution-placement fields, capability negotiation, concrete Codex
  primitive mapping, concurrency limits, and deterministic fallback;
- promotion of a growing ephemeral subagent to a persistent session with a
  durable handoff and no overlapping writer;
- denial of two subagents or mixed primitives attempting to mutate the same
  ownership unit;
- graph revision compare-and-swap, dependency readiness, ownership overlap
  denial, immutable active-task boundaries, and integration ordering;
- per-primitive capability snapshots and exact dispatch/observe/send/wait/
  interrupt/close action idempotency;
- persistent runtime-ID registration/acknowledgement, callback plus polling, and
  archive receipt; optional ephemeral identity/ack/send/callback plus required
  wait/read terminal and runtime-close proof;
- bounded native fan-out dispatch, deterministic aggregation/cancel, no
  synthetic identity, and local placement with no child creation;
- cursor-aware recovery where supported, parent steering, dependency handoff
  scope, multi-project message denial, and per-project concurrency limits;
- restart at every action/result boundary with no duplicate work, lost
  evidence, repeated interrupt/close, or cursor regression; and
- disposition proof, primitive-specific close evidence, combined gates, and
  every field of the typed pending-work gate exactly zero.

### Submission domain

- strict decoding, normalization, principal overwrites, repository scope, mode
  scope, and harness scope;
- deterministic run-ID derivation and canonical request digests;
- exact reuse, changed-input conflict, concurrent reservation ownership, and
  restart recovery before and after provider binding;
- parent/root/depth computation, cross-principal denial, corrupt-chain denial,
  and exact boundary values;
- cancellation request idempotency and terminal-state preservation.

### Authentication and HTTP

- malformed, unknown, expired, wrong-hash, wrong-scope, and revoked tokens;
- constant response shape for authentication failures;
- body/header/timeout limits and unknown-field rejection;
- safe error projection, request logging, and redaction;
- one endpoint cannot invoke an unlisted workflow or arbitrary event.

### Hatchet adapter

- exact workflow name and outer envelope;
- no Hatchet types cross the port;
- accepted-trigger reference persistence;
- status mapping, final-result binding, missing-finalize denial, provider
  collision reconciliation, and cancellation;
- live duplicate transport produces one workflow owner.

### Client and Codex plugin

- stable JSON/exit-code behavior and no token on argv/stdout/stderr;
- file ownership, mode, and symlink rejection for token files;
- session/turn idempotency derivation;
- orchestration skill intent, capability discovery, graph/ownership validation,
  primitive-specific runtime identity, scoped send/wait/steering, deterministic
  aggregation, restart resume, disposition/close, and typed close-gate fixtures;
- leaf skill canonical request, wait, resume, conflict, timeout, decline,
  cancellation, and terminal failure fixtures;
- hook input/output fixtures, state permissions, corruption, concurrency, and
  bounded runtime;
- local plugin install/discovery/trust smoke test.

### Harness certification

- shared execution contract suite against Codex fixtures;
- adversarial process, filesystem, environment, descriptor, and transcript
  probes;
- opt-in live Codex execution;
- opt-in live Codex agent-piloted round trip;
- opt-in live Codex multi-agent control with at least three persistent sessions,
  two projects, additional ephemeral and native fan-out attempts, steering,
  coordinator restart, evidence integration, persistent archival, subagent
  runtime close, native terminal aggregation, and no active local work;
- a second harness may not be labeled supported until the same required suite
  passes.

### Positioning regression

- README, Chart metadata, site, and docs use the canonical short form.
- The support matrix labels direct Hatchet, generic/Go profiles, Codex
  execution, Codex leaf agent-pilot, Codex Agent Control Plane, local runner,
  and second harness accurately.
- Regression tests reject `Go-native`, generic “all runtimes included,”
  agent-side hooks presented as already shipped before completion, or `local`
  presented as certified.
- Local Markdown links and public site documentation links resolve.

## Documentation and Public-Surface Alignment

Implementation must update these surfaces in the same release:

- `README.md`: canonical position, support matrix, control and leaf gateway/
  client usage, credential model, project and message scope, idempotency,
  closure, depth, and migration from direct Hatchet.
- `charts/paje/Chart.yaml`: replace `Go-native` with the canonical durable
  language-neutral description.
- `charts/paje/values.yaml`, schema, templates, notes, and render tests:
  optional gateway deployment and distinct credentials.
- `site/app/page.tsx`: current-versus-planned matrix and eventual graduation of
  Codex full integration only after live acceptance.
- `site/README.md`: source-of-truth and toolchain qualification.
- `site/tests/rendered-html.test.mjs`: product-positioning regressions.
- `docs/`: Agent Control Plane and leaf submission APIs, token provisioning,
  Codex plugin installation, hook trust, support definitions, certification
  evidence, restart/cleanup operations, and second-harness decision record.

Public copy changes from “planned” to “current” only in the commit that carries
the corresponding acceptance evidence.

## Rollout

### Phase 1: Contract and safe submission

Add the provider-neutral Agent Control Plane and capability-aware agent-harness
lifecycle
contract alongside the leaf submission service, durable stores, scoped token
authentication, Hatchet adapter, HTTP gateway, and client. Keep the gateway
disabled by default. Direct Hatchet triggering remains supported.

### Phase 2: Codex agent pilot

Package the Codex plugin, orchestration and leaf skills, bounded hooks, and
Codex agent-work adapter. Test local installation, explicit control-run
creation, all four placement lifecycles including persistent sessions, and leaf
submission with mock and real gateway environments.

### Phase 3: Formal certification and public graduation

Refactor existing Codex evidence into the shared certification suite, run live
execution, leaf agent-pilot, and durable multi-agent control acceptance, then
change Codex from
execution-certified/planned-agent-pilot to fully integrated/current.

### Phase 4: Second-harness evidence gate

Collect demand and protocol/package evidence, commit the selection decision,
and write a candidate-specific spec and implementation plan. No support claim
changes in this phase.

## Acceptance Criteria

This design is complete only when evidence proves all of the following:

1. An installed Codex orchestration skill can deliberately create or resume a
   `ControlRun` without receiving Hatchet, worker, executor, publisher, or
   Codex service credentials.
2. `ControlRun`, `TaskGraph`, `Task`, `ProjectRef`, ownership,
   `PlacementAttempt`, persistent `AgentSession`, mailbox/event cursor,
   evidence, disposition, and close state are durable, provider-neutral,
   strictly validated, and restart-safe.
3. `AgentHarness` discovers and enforces primitive-specific capabilities over
   dispatch, observe, send, wait, interrupt/cancel, and close; command execution
   remains behind the isolated executor.
4. Persistent sessions use exact runtime-ID registration/acknowledgement,
   callbacks plus cursor-aware recovery, and archive receipts. Ephemeral
   subagents use only advertised identity/ack/send/callback capabilities and
   require wait/read terminal plus runtime-close evidence. Native fan-out uses
   deterministic aggregation/cancel without invented identity, and local work
   creates no child. Restart creates no duplicate work.
5. Parent steering, dependency handoffs, integration order, combined gates,
   multi-project isolation, and scoped communication are enforced.
6. Every task records execution placement, parallelism primitive, rationale,
   capability requirements, lifecycle owner, and fallback; Codex placement,
   promotion, missing-capability fallback, concurrency limits, and overlapping
   subagent mutation denial pass acceptance.
7. A live control agent receives a long specification, creates at least three
   persistent child sessions across at least two projects, additionally
   exercises an ephemeral subagent and native fan-out, exchanges
   capability-supported messages, handles one steering event, integrates
   evidence, survives coordinator restart, archives every persistent session,
   closes every nonpersistent attempt with its required evidence, and proves
   the typed pending-work gate is zero.
8. An installed Codex leaf skill can deliberately submit a valid
   `code-change@v1` request without receiving a Hatchet or worker service token.
9. An exact retry from one Codex turn returns the same Pajé run; changed input
   under the same key returns conflict.
10. Scope tests prevent identity, project, repository, communication,
   publication-mode, harness, action,
   and depth escalation.
11. Root-only credentials and server-computed depth prevent recursive leaf
   runs.
12. Gateway restart at every reservation/trigger boundary creates neither a
   duplicate Pajé run nor a lost binding.
13. Cancellation is idempotent, reaches Hatchet and the execution harness
   process, kills
   descendants, and becomes terminal only through durable workflow evidence.
14. Agent, verification, gateway, and publisher processes cannot read one
   another's credentials.
15. Codex passes every execution and agent-pilot certification requirement for
   exact recorded versions.
16. A live originating Codex session submits, observes, and reports one durable
   successful result.
17. Direct Hatchet triggers and existing beta artifact/approval/publication
   behavior remain compatible.
18. README, Helm metadata, site, docs, and regression tests present the same
   current-versus-future matrix.
19. The second harness remains unnamed until a committed evidence record passes
   the selection gate.

## External Interface References

The Codex packaging choices in this design are based on the current official
Codex manual:

- [Build skills](https://learn.chatgpt.com/docs/build-skills)
- [Build plugins](https://learn.chatgpt.com/docs/build-plugins)
- [Hooks](https://learn.chatgpt.com/docs/hooks)

Those interfaces are external and may evolve. The implementation plan must
recheck the current manual before finalizing plugin manifests or hook fixtures.
