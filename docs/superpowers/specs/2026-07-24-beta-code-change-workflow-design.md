# Pajé Beta Code-Change Workflow Design

## Status

Approved in conversation on 2026-07-24.

## Goal

Make Pajé beta-worthy for one complete, reusable coding workflow:
`code-change@v1`. A run must turn an explicit repository revision and task into
a verified, durable change artifact, optionally pause for approval, and
idempotently publish that artifact as a pull request.

The workflow must remain compatible with Codex while preserving Pajé's
hexagonal boundary. Hatchet supplies durable execution, scheduling, and signals,
but template definitions, stage services, artifacts, and policies remain
testable without Hatchet.

This is the first beta subproject. Later `investigate`, `release`, and
`gitops-change` templates will reuse its stage contracts instead of introducing
a general workflow language.

## Why This Is the Beta Anchor

Past work exposed recurring orchestration failures:

- Ambient user configuration or workspace state changed agent behavior.
- Child processes survived cancellation and held output pipes open.
- Local `go.work` overrides made dependency-sensitive checks pass locally while
  fresh consumers and CI used a different module.
- Only one module in a multi-module repository was updated or verified.
- Environment-limited integration failures were mistaken for code regressions.
- Agent changes remained only in a worktree or local branch and were never
  published.
- Direct pushes failed against protected branches.
- Plans and local tests were treated as completion even though remote, release,
  deployment, or health evidence was missing.
- GitOps checks used the wrong renderer or treated slow image pulls and delayed
  Argo refreshes as immediate failures.
- Changes outside the repository were missed because verification only inspected
  the current worktree.

`code-change@v1` addresses the shared foundation: explicit inputs, immutable
revision state, isolated execution, deterministic environment, evidence-bearing
verification, durable artifacts, approval, idempotent publication, and outcome
memory. Specialized release and GitOps templates will add their domain-specific
post-publication checks later.

## Approaches Considered

### 1. Built-in typed and versioned templates

Templates are Go definitions registered by stable name and version. They
compose provider-neutral stage services and expose thin Hatchet bindings.

This is the selected approach. It gives beta a small security surface, compile-
time contracts, deterministic migrations, and tests that do not need Hatchet.
Adding or changing a template requires a Pajé release.

### 2. Repository-owned YAML workflow DSL

Users define arbitrary stages and commands in YAML. This is flexible but would
create a second orchestration language above Hatchet, introduce command-
injection and schema-migration concerns, and make beta behavior difficult to
bound. It is not part of beta.

### 3. Hatchet-native workflows as the application core

Each template is implemented directly as a Hatchet DAG. This offers strong
stage visibility but couples application behavior, types, and tests to Hatchet.
Pajé will use Hatchet as an outer adapter instead.

## Beta Scope

The beta slice includes:

- A typed template registry with `code-change@v1`.
- Durable run records and artifact bundles.
- An allowlisted agent environment.
- Generic and Go repository profiles.
- Shell-free verification commands with timeouts and bounded output.
- Artifact-only output as the default.
- Approval-gated GitHub pull-request publication.
- Idempotent retry behavior for every externally visible stage.
- Hatchet bindings and concurrency keys for the template.
- A persistent-volume option and Codex-auth mount in the Helm chart.
- Unit, integration, live Codex, and opt-in publication acceptance tests.

The beta slice does not include:

- An arbitrary YAML workflow language.
- Automatic merge, direct pushes to protected target branches, or release tags.
- Kubernetes Job runners or multiple worker replicas.
- Forgejo pull-request publication. The publisher port must allow this later.
- `release`, `gitops-change`, or alert-monitoring implementations.
- Automatic remediation after failed verification.

## Architecture

### Template Registry

`internal/template` owns:

```go
type ID struct {
    Name    string
    Version int
}

type Template interface {
    ID() ID
    Validate(input json.RawMessage) error
}

type Registry interface {
    Resolve(id ID) (Template, error)
}
```

The registry rejects unknown names, versions, and fields before a run is
created. `code-change@v1` has a dedicated typed input and result; the registry
does not execute arbitrary user-provided stages.

### Run Records

`internal/run` owns a durable state record:

```go
type Status string

const (
    StatusPending          Status = "pending"
    StatusResolving        Status = "resolving"
    StatusExecuting        Status = "executing"
    StatusAwaitingApproval Status = "awaiting_approval"
    StatusPublishing       Status = "publishing"
    StatusSucceeded        Status = "succeeded"
    StatusFailed           Status = "failed"
    StatusCanceled         Status = "canceled"
    StatusDeclined         Status = "declined"
)

type StageResult struct {
    Name       string
    Status     string
    StartedAt  time.Time
    FinishedAt time.Time
    Attempts   int
    Evidence   map[string]string
    Failure    *Failure
}

type Record struct {
    ID              string
    Template        template.ID
    Status          Status
    RepositoryURI   string
    BaseRef         string
    BaseSHA         string
    MemorySnapshot  []memory.Memory
    Artifact        *artifact.Reference
    Approval        *approval.Result
    Publication     *publisher.Result
    Stages          []StageResult
    CreatedAt       time.Time
    UpdatedAt       time.Time
}
```

`run.Store` persists records with compare-and-swap versioning so retries cannot
move a run backward. The beta adapter stores JSON atomically on a persistent
filesystem; the mock adapter supports deterministic tests.

The resolved memory snapshot is persisted in the access-controlled run record
so Execute retries use identical context and Hatchet passes only the run ID
between phases. Artifact manifests record the memory count and IDs, but omit
memory content.

### Artifact Store

`internal/artifact` owns immutable content-addressed bundles:

```go
type Reference struct {
    RunID  string
    Digest string
    Size   int64
}

type Store interface {
    Save(ctx context.Context, bundle Bundle) (Reference, error)
    Load(ctx context.Context, ref Reference) (Bundle, error)
}
```

The filesystem adapter writes a temporary file, fsyncs it, verifies the digest,
atomically renames it into place, and fsyncs the containing directory. Loading
verifies the digest again. Bundle paths never come from user input.

An artifact bundle contains:

- `manifest.json` with schema version, run ID, template ID, repository, base
  SHA, changed paths, file modes, non-manifest member digests, and sizes.
- `changes.patch`, a Git binary patch containing tracked, untracked, deleted,
  renamed, symlink, and mode changes.
- Agent terminal output and execution metadata.
- Verification command results with exit codes, durations, and bounded output.
- Preflight facts and warnings without secret values.

The store digest is SHA-256 over a versioned, canonical uncompressed member
stream with normalized archive metadata. It is held in `artifact.Reference`,
not embedded in the bytes it authenticates. Compression is a storage detail and
does not change the reference digest.

The Git adapter creates `changes.patch` with a temporary index rooted at the
resolved base SHA. It never stages the agent's actual worktree index:

```text
GIT_INDEX_FILE=<temp> git read-tree <base-sha>
GIT_INDEX_FILE=<temp> git add -A -- .
GIT_INDEX_FILE=<temp> git diff --cached --binary <base-sha>
```

The default compressed bundle limit is 10 MiB and is configurable downward or
upward by the operator. An oversized artifact fails the execute stage before
approval or publication.

### Environment Policy

The current generic local runner inherits the worker process environment. Beta
must replace that behavior for template execution.

`internal/environment.Policy` builds the child environment from:

- A minimal platform baseline: operator-pinned `PATH`, a fresh per-run `HOME`,
  temporary-directory variables, locale, and certificate variables.
- `CODEX_HOME` for authentication when the Codex runner is selected.
- Operator-configured non-secret keys requested by name in the workflow input.
- Explicit secret references mounted by the worker, passed only to stages that
  declare them.

Hatchet, Mem0, publisher, and unrelated worker credentials are denied by
default. Run records and artifacts persist environment key names and redaction
markers, never values.

The Codex adapter continues to use `--ignore-user-config`, ephemeral sessions,
JSONL output, workspace-write sandboxing, and process-group cancellation.

### Repository Profiles

`code-change@v1` requires a profile:

- `generic`: records Git status, resolved base SHA, tool availability, and
  explicitly configured verification commands.
- `go`: includes the generic checks, enumerates every tracked `go.mod`, records
  `go env GOWORK`, resolves modules with `GOWORK=off`, and runs the configured
  Go checks in every selected module.

The Go profile defaults to selecting every discovered module and running
`go test ./...` in each one. Input may replace the check list. It may exclude a
module only through an explicit path-and-reason entry; every exclusion appears
as a warning in approval evidence.

Profile commands are executable-plus-arguments structures, never shell
fragments:

```go
type Command struct {
    Name       string
    Directory  string
    Executable string
    Args       []string
    Timeout    time.Duration
    Required   bool
}
```

Directories must resolve below the prepared workspace. Timeout, output-size,
command-count, and argument-count limits are validated before execution.
The JSON input uses a separate `CommandSpec` with `Timeout` encoded as a Go
duration string such as `"2m"`; validation compiles it into the domain command
above.

### Approval

The existing `approval.Gate` becomes part of the template. Artifact-only runs do
not require approval because they have no external mutation.

Pull-request runs enter `awaiting_approval` after artifact capture and
verification. The approval request includes:

- Run and template IDs.
- Repository, base SHA, target branch, and proposed publication branch.
- Agent summary.
- Changed paths and patch digest.
- Required and optional verification results.
- Environment and dependency warnings.

Approval is bound to the artifact digest. Changing the artifact, base SHA,
target, or publication mode invalidates the decision. A decline finalizes the
run as `declined`; it is not an error and is never retried.

The Hatchet adapter uses a durable signal for approval. The provider-neutral
service consumes an `approval.Gate`, allowing CLI and mock adapters. The beta
port evolves the approval result from a bare boolean to a typed decision that
echoes the run ID and artifact digest and records the actor, decision time, and
optional reason. Legacy gates can be wrapped, but a publisher accepts only a
decision whose binding fields exactly match its request.

```go
type Result struct {
    RunID          string
    ArtifactDigest string
    Approved       bool
    Actor          string
    DecidedAt      time.Time
    Reason         string
}

type Gate interface {
    RequestApproval(ctx context.Context, req Request) (Result, error)
}
```

### Publisher

`internal/publisher` defines:

```go
type Request struct {
    RunID       string
    Repository  string
    BaseSHA     string
    TargetRef   string
    Branch      string
    Artifact    artifact.Reference
    Title       string
    Body        string
    Draft       bool
}

type Publisher interface {
    Publish(ctx context.Context, req Request) (Result, error)
}
```

Beta ships mock and GitHub adapters. The GitHub adapter:

1. Loads and verifies the approved artifact.
2. Prepares a fresh workspace at the exact base SHA.
3. Applies the binary patch with `git apply --index --binary`.
4. Re-runs required verification commands.
5. Creates a deterministic branch:
   `paje/code-change/<run-id>`.
6. Commits with the run ID in the message and trailers.
7. Pushes the branch.
8. Creates or reuses a pull request targeting the requested branch.
9. Returns immutable commit, branch, pull-request URL, and provider IDs.

The publisher never pushes directly to the target branch and never merges. If a
branch or pull request already exists, it verifies the run ID, base SHA,
artifact digest, and commit before reusing it. Any mismatch is a conflict, not a
retryable success.

### Policy Checks

Before approval, `code-change@v1` applies built-in policies:

- Reject paths escaping the workspace or containing unsupported Git objects.
- Reject a dirty source repository only when the repository URI is a local
  checkout; remote repositories are resolved from the remote ref.
- Detect likely plaintext secret files and high-risk credential patterns in the
  patch. A policy denial cannot be overridden through the workflow input.
- Require all required verification commands to pass.
- Warn when checks are skipped because an external dependency is unavailable,
  and classify the result as `environment` rather than `verification`. A
  required check that cannot run fails the workflow; only optional checks may
  become warnings.
- Require an explicit target branch and publication provider for pull-request
  mode.

The initial secret policy is conservative and path/content based. It records
rule identifiers and paths but never includes matched secret values.

## Input and Output

### `CodeChangeInput`

```go
type CodeChangeInput struct {
    IdempotencyKey   string
    TaskDescription string
    RepositoryURI    string
    BaseRef          string
    MemoryQuery      string
    MemoryLimit      int
    Tags             map[string]string
    Profile          string
    Checks           []verification.CommandSpec
    ModuleExclusions []ModuleExclusion
    EnvironmentKeys  []string
    Publication      Publication
}

type ModuleExclusion struct {
    Path   string
    Reason string
}

type Publication struct {
    Mode         string // artifact or pull_request
    Provider     string
    TargetBranch string
    Title        string
    Draft        bool
}
```

`artifact` is the default publication mode. Secret values and arbitrary
environment values are not accepted in workflow input.

When `IdempotencyKey` is set, Pajé binds it to the template ID and a canonical
hash of the validated input. Reuse with the same hash resumes the run; reuse
with different input is a conflict. An omitted key creates a new run.

### `CodeChangeResult`

```go
type CodeChangeResult struct {
    RunID        string
    Status       run.Status
    BaseSHA      string
    Artifact     artifact.Reference
    Verification []verification.Result
    Approval     *approval.Result
    Publication  *publisher.Result
    Failure      *run.Failure
}
```

All result references are durable and sufficient to inspect the run without the
ephemeral worktree.

## Data Flow

### Resolve Phase

1. Validate the template ID and typed input.
2. Allocate or resume the idempotent run ID.
3. Resolve `BaseRef` to an immutable SHA.
4. Apply the environment policy and check required capabilities.
5. Validate the requested repository profile and worker capabilities.
6. Retrieve scoped memory.
7. Persist the resolved run record.

### Execute Phase

1. Prepare a fresh worktree at `BaseSHA`.
2. Run repository profile preflight and dependency discovery.
3. Build the agent task from the user task, memory, preflight facts, and explicit
   constraints.
4. Execute Codex in the filtered environment.
5. Run required and optional verification commands.
6. Classify failures and warnings.
7. Capture, digest, and persist the artifact bundle.
8. Persist the stage result.
9. Clean the worktree with a non-canceled bounded context.

All workspace-bound operations stay in this phase. A retry starts from a new
worktree and either produces the same digest or records a new attempt; it never
reuses partial files.

### Approval Phase

1. Skip for artifact-only mode.
2. Load and verify the artifact.
3. Persist `awaiting_approval`.
4. Request a decision bound to the artifact digest.
5. Persist approved or declined state.

### Publish Phase

1. Require a matching approval.
2. Call the configured publisher with the deterministic branch.
3. Re-run required checks after applying the artifact.
4. Create or reuse the branch and pull request.
5. Persist publication identifiers.

### Finalize Phase

1. Save a concise outcome to memory, including run ID, template, base SHA,
   artifact digest, stage statuses, failure class, and publication URL.
2. Mark the run terminal only after memory persistence or an explicitly
   recorded memory-store failure.
3. Return `CodeChangeResult`.

## Failure Model

```go
type FailureClass string

const (
    FailureInput       FailureClass = "input"
    FailureEnvironment FailureClass = "environment"
    FailureAgent       FailureClass = "agent"
    FailureVerification FailureClass = "verification"
    FailurePolicy      FailureClass = "policy"
    FailureApproval    FailureClass = "approval"
    FailurePublication FailureClass = "publication"
    FailureCleanup     FailureClass = "cleanup"
    FailureCanceled    FailureClass = "canceled"
    FailureInternal    FailureClass = "internal"
)
```

Every failure records stage, class, retryability, a safe diagnostic, and a
cause code. Diagnostics are bounded and redacted.

- Input, policy, decline, and deterministic verification failures are not
  retried.
- Transient repository, memory, artifact-store, Hatchet, and GitHub failures
  may be retried with capped exponential backoff.
- Agent execution is retried automatically only when Pajé can prove it did not
  start or did not produce a completed response. After a completed response,
  file mutation, timeout, or ambiguous transport failure, an operator must
  request a new attempt.
- Caller cancellation finalizes the run as `canceled` and is not retried
  automatically.
- Publication retries rely on deterministic branch names and provider-side
  reconciliation.
- Cleanup failure is joined with the primary failure and never masks it.

## Hatchet Binding

Hatchet remains an outer adapter with five visible tasks:

```text
resolve -> execute -> approval -> publish -> finalize
```

`approval` and `publish` are skipped for artifact-only runs. The adapter:

- Uses the run ID as the idempotency key.
- Applies per-stage timeouts and retry policies.
- Limits concurrent `publish` tasks to one per repository and target branch.
- Allows parallel `execute` tasks because Git worktrees are isolated.
- Passes durable IDs and artifact references between stages, not workspace
  paths or secret values.

The provider-neutral services expose each phase as ordinary Go methods so their
tests do not require Hatchet.

## Deployment

The beta chart adds:

- An optional persistent volume for run records, artifacts, repository mirrors,
  and worktrees. Beta supports one worker replica.
- A read-only Codex authentication Secret used as seed material. An init
  container copies only the required files into a private writable `emptyDir`;
  that directory becomes `CODEX_HOME`, so Codex can maintain runtime state
  without mutating the Secret volume.
- Separate Secret references for Hatchet, Mem0, and GitHub.
- Configuration for artifact limits, command limits, stage timeouts, and
  environment allowlists.
- A beta Codex worker image with a pinned Codex CLI version.

The Codex child process does not receive Hatchet, Mem0, or GitHub credentials.
Only the publisher process receives GitHub credentials.

## Testing

### Unit Tests

- Template registration, version resolution, unknown-field rejection, and
  input validation.
- Run-store compare-and-swap transitions and terminal-state invariants.
- Environment allowlist and secret-deny behavior.
- Profile discovery, multi-module Go checks, `GOWORK=off`, skipped-module
  warnings, timeouts, cancellation, and output limits.
- Failure classification and retryability.
- Approval binding to artifact digest.
- Publisher idempotency and conflict detection.

### Integration Tests

- Capture and reapply a binary patch containing modified, new, deleted, renamed,
  executable, symlink, and binary files.
- Persist artifacts and run records across service reconstruction.
- Run Codex against a temporary repository, read memory, modify files, execute
  verification, capture the artifact, and clean the workspace.
- Apply the artifact to a fresh worktree and prove the resulting tree matches
  the captured manifest.
- Push to a local bare remote and prove repeated publication is idempotent.
- Exercise the GitHub API adapter through `httptest.Server`.
- Verify a denied approval never invokes the publisher.
- Verify cancellation leaves no agent descendants or worktrees.

### Opt-In Acceptance Tests

- Authenticated Codex produces a real verified artifact.
- A dedicated GitHub test repository receives a draft pull request; rerunning
  with the same run ID reuses it without another commit or PR.
- The beta image starts as non-root with mounted Codex auth.
- The Helm chart renders and server-side dry-runs with persistent storage and
  isolated credentials.

## Beta Acceptance Criteria

Pajé is beta-worthy for `code-change@v1` only when current evidence proves all
of the following:

1. A real Codex run reads repository and memory context, changes files, and
   emits a terminal response.
2. Required verification runs in every selected module with a filtered
   environment.
3. The source checkout is unchanged and no worktree or descendant process is
   left behind.
4. The durable artifact reproduces the exact changed Git tree in a fresh
   workspace.
5. A restarted process can load the run and artifact.
6. Artifact-only mode performs no source-control or publication-provider
   mutation; durable run, artifact, and outcome-memory writes are expected.
7. Pull-request mode cannot publish without approval bound to the artifact.
8. Repeating publication does not create another branch, commit, or pull
   request.
9. Agent processes cannot read Hatchet, Mem0, or GitHub credentials.
10. Code, race, vet, cross-build, chart, image, live Codex, and opt-in GitHub
    acceptance checks pass.

## Later Templates

- `investigate@v1`: reuse resolve and execute with read-only Codex, evidence
  artifacts, no approval, and no publisher.
- `release@v1`: add tag/release assets, protected-branch follow-ups,
  distribution checks, and downstream dependency pins.
- `gitops-change@v1`: add renderer selection, client/server dry-runs, GitOps
  publication, reconciliation/refresh, rollout windows, and health probes.

These templates are separate follow-up specs. They reuse the beta stage
contracts but do not expand the first implementation plan.
