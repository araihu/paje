# Pajé Agent-Piloted Submission and Harness Support Design

## Status

Approved by the product direction supplied on 2026-07-25 and grounded in the
independent positioning audit of repository commit `df9137b`. This document
supersedes ambiguous product-positioning language; it does not replace the
durable `code-change@v1` design.

The repository state inspected for this design is `86537ae`. The current beta
implementation remains the source of truth for shipped behavior until the
implementation plan for this design is completed.

## Canonical Product Position

Pajé is a self-hosted durable orchestration system for code agents. Pajé is
implemented in Go, while its workflow contract is repository-language-neutral:
`code-change@v1` supports an explicit `generic` profile for any toolchain
available in the worker image and a first-class `go` profile with Go-specific
discovery and defaults.

Pajé is designed to be piloted by an agent through an installable harness
integration. The current beta is still triggered directly through Hatchet.
Codex is the first dedicated execution harness and will become the first fully
integrated harness when the agent-side submission skill and lifecycle hooks in
this design ship. Additional dedicated harnesses are a planned extension point,
not a currently shipped capability.

The canonical short form is:

> Pajé is implemented in Go, works with repositories in any language through
> explicit profiles and checks, and makes agent-driven code changes durable.
> The beta uses Hatchet as its direct trigger and Codex as its first dedicated
> execution harness. A scoped agent-side submission surface and additional
> certified harnesses are planned capabilities.

## Why This Design Exists

The independent audit found four different truths being collapsed into one
marketing claim:

1. Pajé currently launches an agent; the agent does not yet have a supported
   Pajé skill, hook, API, or CLI with which to launch Pajé.
2. The workflow is language-neutral, but the worker image does not contain
   every language toolchain and Go remains a privileged built-in profile.
3. Codex is the first named, packaged, deterministic, live-tested execution
   harness, although the low-level `local` runner existed first.
4. The core runner port admits more adapters, but no second dedicated harness
   has been selected, packaged, or accepted.

The missing product layer is therefore not another workflow template. It is a
safe control plane between an agent harness and the existing Hatchet trigger,
plus an explicit definition of what “supported harness” means.

## Goals

- Let an external code agent deliberately submit, inspect, wait for, and cancel
  a Pajé run through its harness integration.
- Ship an installable Codex integration made of a focused skill, lifecycle
  hooks, and a deterministic client.
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
- Adding a general workflow DSL, arbitrary shell commands, multiple worker
  replicas, automatic merge, or direct pushes to target branches.
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
integration with a certified skill, hooks, submission client, recursion
protection, and live agent-to-Pajé acceptance.

## Current and Future Support Matrix

Status terms are deliberately limited to `current`, `planned by this design`,
`future after evidence gate`, and `not planned`.

### Trigger support

| Surface | Status at `86537ae` | Target status | Product boundary |
| --- | --- | --- | --- |
| Direct `paje-code-change-v1` Hatchet workflow trigger | current | current, retained for operators | Provider-specific outer adapter |
| Pajé submission HTTP API | absent | planned by this design | Stable agent and automation boundary |
| `paje-agent` submit/status/wait/cancel client | absent | planned by this design | Deterministic client over the HTTP API |
| Codex skill-driven submission | absent | planned by this design | First supported agent-piloted entrypoint |
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
| Codex agent-side skill and hooks | absent | planned by this design | required for full integration |
| Codex end-to-end agent-piloted flow | absent | planned by this design | first fully integrated harness |
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
  submission-gateway, Git, or SSH credentials.
- Token-bearing operations never execute in a repository or worktree that ran
  repository-controlled code.
- The packaged Linux worker fails closed if its process-inspection guard cannot
  be installed.
- The beta remains a single-worker, single-writable-filesystem installation.

## Approaches Considered

### 1. Pajé submission gateway plus installable harness integration

A separate `paje-gateway` process authenticates a narrowly scoped Pajé token,
validates and binds the submission, computes lineage and depth, and calls a
provider-neutral trigger port. A Hatchet adapter implements that port. An
installable Codex plugin ships a skill and hooks that call a small
`paje-agent` HTTP client.

This is the selected approach. It provides a stable Pajé protocol, keeps broad
Hatchet credentials server-side, gives idempotency and recursion policy one
authoritative owner, and leaves the code-change core unchanged.

### 2. Let the agent trigger Hatchet directly

The skill could call the Hatchet SDK or API with `paje-code-change-v1`.
This avoids a new server, but exposes Hatchet concepts and credentials to the
agent, cannot express Pajé-specific scopes cleanly, and duplicates
idempotency/depth rules in each harness integration.

This approach is rejected.

### 3. Embed agent submission in the worker process

The worker could expose HTTP alongside its Hatchet listener and reuse the
worker token and in-memory composition. This reduces deployment objects, but it
combines public ingress with the credential-bearing execution process, makes
least-privilege separation difficult, and increases the blast radius of an HTTP
or authentication flaw.

This approach is rejected. The gateway and worker use separate processes,
service accounts, credentials, and Kubernetes workloads.

## Architecture

```text
Codex conversation
  -> Pajé Codex plugin
       -> skill
       -> lifecycle hooks
       -> paje-agent client
            -> HTTPS + scoped Pajé bearer token
                 -> paje-gateway HTTP adapter
                      -> token authenticator
                      -> provider-neutral submission.Service
                           -> submission.Store
                           -> submission.Trigger port
                                -> Hatchet trigger adapter
                                     -> paje-code-change-v1
                                          -> provider-neutral code-change service
                                               -> certified runner adapter
```

There are three independently testable planes:

1. **Submission plane:** authenticates a principal, validates a request, binds
   idempotency and lineage, starts or reuses a workflow, exposes safe status,
   and requests cancellation.
2. **Durable workflow plane:** the existing typed `code-change@v1` phases,
   stores, artifacts, approval, and publication.
3. **Harness plane:** worker-side execution adapter plus optional agent-side
   skill, hooks, and client.

Failure or replacement in one plane must not require provider types in another.

## Provider-Neutral Submission Domain

`internal/submission` owns the stable application contract:

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

## Submission HTTP Contract

The gateway exposes only versioned JSON endpoints:

```text
POST /v1/submissions
GET  /v1/submissions/{run_id}
POST /v1/submissions/{run_id}/cancel
GET  /healthz
GET  /readyz
```

There is no generic workflow-name endpoint, arbitrary event endpoint, admin
endpoint, approval endpoint, artifact-file endpoint, or credential-minting
endpoint in the agent-facing server.

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
`run_not_cancelable`, `provider_unavailable`, and `internal`.
Diagnostics never include bearer tokens, service credentials, raw provider
bodies, repository credentials, or unbounded transcripts.

## Scoped Submission Credentials

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
- allowed actions: `submit:artifact`, `submit:pull_request`, `read`, `cancel`
- allowed harness IDs
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
- receives no Mem0, GitHub, Codex, worker Hatchet, repository, Git, or SSH
  credential;
- receives a distinct Hatchet producer Secret and submission-policy Secret;
- persists submission reservations and provider bindings atomically;
- supports exactly one replica in v1 because the filesystem store uses
  process-local compare-and-swap;
- exposes only the documented HTTP paths;
- sets request-body, header, timeout, connection, and response-size limits;
- logs request ID, credential ID, run ID, action, result code, and duration,
  never authorization headers or input bodies.

TLS terminates at an operator-controlled ingress or trusted private network
boundary. The chart does not create public ingress by default.

## `paje-agent` Client

`cmd/paje-agent` is the deterministic harness-facing client. It is not a worker
and does not contain Hatchet code.

Commands:

```text
paje-agent submit --file <path-or-> [--idempotency-key <key>]
paje-agent status --run <run-id>
paje-agent wait --run <run-id> --timeout <duration>
paje-agent cancel --run <run-id>
paje-agent hook session-start
paje-agent hook user-prompt-submit
paje-agent hook stop
```

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

## Installable Codex Integration

Codex is integrated through a skills-and-hooks plugin:

```text
integrations/codex/paje/
├── .codex-plugin/
│   └── plugin.json
├── hooks/
│   └── hooks.json
└── skills/
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

- skills are focused `SKILL.md` workflows;
- installable packages use `.codex-plugin/plugin.json`;
- plugin hooks default to `hooks/hooks.json`;
- hook commands receive `PLUGIN_ROOT` and `PLUGIN_DATA`;
- command hooks are reviewed and trusted before they run.

### Codex skill behavior

`using-paje` triggers when a user explicitly asks Codex to delegate a suitable
code change to Pajé or invokes the skill by name. It must:

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

- `SessionStart` loads safe active-run context from `PLUGIN_DATA`, optionally
  checks status, and emits a concise system message.
- `UserPromptSubmit` records only `session_id`, `turn_id`, `cwd`, and active
  run ID in a mode-`0600` context file. It does not store prompt text.
- `Stop` performs at most one bounded status check for an active run and emits
  a system message. It does not block stop indefinitely and does not submit,
  cancel, approve, publish, or trust another hook.

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
3. the skill submits only on explicit intent and produces a canonical request;
4. the hooks obey their bounded, non-submitting lifecycle contract;
5. a scoped token cannot exceed its repository, identity, action, harness, or
   depth policy;
6. a live Codex session submits through the plugin, Pajé completes the durable
   workflow with the Codex runner, and the originating session observes the
   terminal result without any worker service-token exposure.

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

- The integration has a valid manifest, one focused skill, declared hooks, and
  documented client dependency.
- Installation, discovery, update, removal, and hook-trust flows are tested.

#### AP-2: Skill behavior

- Explicit and implicit trigger prompts are tested.
- Ambiguous intent does not submit.
- Canonical profile/check selection, idempotency, status handling, and safe
  failure instructions are tested.
- The skill never handles worker service credentials.

#### AP-3: Hook behavior

- Each supported event receives recorded fixture input and produces valid
  bounded JSON output.
- Hooks never submit, approve, publish, or cancel.
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
- skill intent, canonical request, wait, resume, conflict, timeout, decline,
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
- a second harness may not be labeled supported until the same required suite
  passes.

### Positioning regression

- README, Chart metadata, site, and docs use the canonical short form.
- The support matrix labels direct Hatchet, generic/Go profiles, Codex
  execution, Codex agent-pilot, local runner, and second harness accurately.
- Regression tests reject `Go-native`, generic “all runtimes included,”
  agent-side hooks presented as already shipped before completion, or `local`
  presented as certified.
- Local Markdown links and public site documentation links resolve.

## Documentation and Public-Surface Alignment

Implementation must update these surfaces in the same release:

- `README.md`: canonical position, support matrix, gateway/client usage,
  credential model, idempotency, depth, and migration from direct Hatchet.
- `charts/paje/Chart.yaml`: replace `Go-native` with the canonical durable
  language-neutral description.
- `charts/paje/values.yaml`, schema, templates, notes, and render tests:
  optional gateway deployment and distinct credentials.
- `site/app/page.tsx`: current-versus-planned matrix and eventual graduation of
  Codex full integration only after live acceptance.
- `site/README.md`: source-of-truth and toolchain qualification.
- `site/tests/rendered-html.test.mjs`: product-positioning regressions.
- `docs/`: submission API, token provisioning, Codex plugin installation,
  hook trust, support definitions, certification evidence, and second-harness
  decision record.

Public copy changes from “planned” to “current” only in the commit that carries
the corresponding acceptance evidence.

## Rollout

### Phase 1: Contract and safe submission

Add the provider-neutral submission service, durable reservation store, scoped
token authentication, Hatchet adapter, HTTP gateway, and client. Keep the
gateway disabled by default. Direct Hatchet triggering remains supported.

### Phase 2: Codex agent pilot

Package the Codex plugin, skill, and bounded hooks. Test local installation and
explicit submission with mock and real gateway environments.

### Phase 3: Formal certification and public graduation

Refactor existing Codex evidence into the shared certification suite, run live
execution and agent-piloted acceptance, then change Codex from
execution-certified/planned-agent-pilot to fully integrated/current.

### Phase 4: Second-harness evidence gate

Collect demand and protocol/package evidence, commit the selection decision,
and write a candidate-specific spec and implementation plan. No support claim
changes in this phase.

## Acceptance Criteria

This design is complete only when evidence proves all of the following:

1. An installed Codex skill can deliberately submit a valid `code-change@v1`
   request without receiving a Hatchet or worker service token.
2. An exact retry from one Codex turn returns the same Pajé run; changed input
   under the same key returns conflict.
3. Scope tests prevent identity, repository, publication-mode, harness, action,
   and depth escalation.
4. Root-only credentials and server-computed depth prevent recursive runs.
5. Gateway restart at every reservation/trigger boundary creates neither a
   duplicate Pajé run nor a lost binding.
6. Cancellation is idempotent, reaches Hatchet and the harness process, kills
   descendants, and becomes terminal only through durable workflow evidence.
7. Agent, verification, gateway, and publisher processes cannot read one
   another's credentials.
8. Codex passes every execution and agent-pilot certification requirement for
   exact recorded versions.
9. A live originating Codex session submits, observes, and reports one durable
   successful result.
10. Direct Hatchet triggers and existing beta artifact/approval/publication
    behavior remain compatible.
11. README, Helm metadata, site, docs, and regression tests present the same
    current-versus-future matrix.
12. The second harness remains unnamed until a committed evidence record passes
    the selection gate.

## External Interface References

The Codex packaging choices in this design are based on the current official
Codex manual:

- [Build skills](https://learn.chatgpt.com/docs/build-skills)
- [Build plugins](https://learn.chatgpt.com/docs/build-plugins)
- [Hooks](https://learn.chatgpt.com/docs/hooks)

Those interfaces are external and may evolve. The implementation plan must
recheck the current manual before finalizing plugin manifests or hook fixtures.
