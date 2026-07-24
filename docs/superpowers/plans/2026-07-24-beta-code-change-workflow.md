# Pajé Beta Code-Change Workflow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and prove `code-change@v1`, which turns an immutable repository
revision and coding task into a verified durable artifact and can publish that
exact artifact as an approval-gated, idempotent GitHub pull request.

**Architecture:** Provider-neutral Go services own template validation, durable
run state, isolated execution, verification, artifacts, approval, and
publication. Hatchet v0.97.0 is a thin five-phase DAG adapter; filesystem,
Git/Codex, GitHub, and Helm implementations remain outside the application
core and are exercised through focused integration tests.

**Tech Stack:** Go 1.26.1, Hatchet Go SDK v0.97.0, Codex CLI 0.144.5, Git
worktrees, GitHub REST API, Mem0 Platform v3, Docker, Kubernetes, Helm 3.

## Global Constraints

- Module path remains exactly `github.com/araihu/paje`.
- The built-in template ID is exactly `code-change@v1`; beta does not add a
  YAML workflow language.
- The default publication mode is `artifact`; `pull_request` is the only other
  accepted value.
- Artifact references use SHA-256 over a canonical uncompressed member stream;
  the default compressed bundle limit is exactly 10 MiB.
- Agent and verification commands never invoke a shell.
- Every memory search/save is scoped by non-empty `user_id` and `app_id` tags;
  Pajé adds the durable `run_id` tag to outcome writes.
- Agent children receive an exact allowlisted environment and never receive
  Hatchet, Mem0, or GitHub credentials.
- Codex always runs with `exec --json --ephemeral --ignore-user-config
  --sandbox workspace-write`.
- The Go profile selects every tracked `go.mod` by default, records
  `go env GOWORK`, and resolves and verifies modules with `GOWORK=off`.
- Required checks must pass; only optional checks may become environment
  warnings.
- Approval is valid only for the matching run ID, base SHA, target branch,
  publication mode, and artifact digest.
- Pull-request publication uses `paje/code-change/<run-id>`, never pushes the
  target branch, never merges, and treats mismatched existing state as a
  conflict.
- A completed or ambiguously interrupted agent execution is never
  automatically retried.
- Filesystem durability uses write-to-temp, file fsync, atomic rename, and
  parent-directory fsync.
- Beta deployment supports exactly one worker replica.
- Existing `paje-agent-run` behavior remains available while
  `paje-code-change-v1` is introduced.
- Keep the current uncommitted Codex compatibility work intact. Tasks 1 and 2
  deliberately absorb and test those files before their commits.

---

## File Map

### Existing execution and composition files

- `internal/runner/runner.go`: black-box request/result contract, including raw
  transcript and completion evidence.
- `internal/executil/*.go`: shared process-group cancellation and bounded output
  primitives used by agents and verification.
- `internal/runner/local/*.go`: exact-environment local process adapter.
- `internal/runner/codex/*.go`: deterministic Codex JSONL adapter.
- `internal/config/config.go`: worker environment parsing and validation.
- `cmd/paje/main.go`: dependency composition and Hatchet worker lifecycle.

### New beta domain files

- `internal/template/*.go`: typed template IDs and registry.
- `internal/template/codechange/*.go`: strict `code-change@v1` input decoding.
- `internal/verification/*.go`: shell-free command specifications, validation,
  execution, limits, and evidence.
- `internal/environment/*.go`: per-stage child environment policy.
- `internal/repository/*.go`: revision resolution and generic/Go profiles.
- `internal/artifact/*.go`: immutable bundles and store contract.
- `internal/artifact/filesystem/*.go`: canonical durable filesystem store.
- `internal/artifact/gitcapture/*.go`: temporary-index binary patch capture and
  reproduction.
- `internal/policy/*.go`: workspace/path and likely-secret denials.
- `internal/run/*.go`: state machine, failure model, and store contract.
- `internal/run/filesystem/*.go`: JSON compare-and-swap run store.
- `internal/publisher/*.go`: provider-neutral publication contract.
- `internal/publisher/gitpr/*.go`: deterministic branch/commit/push engine.
- `internal/publisher/github/*.go`: GitHub pull-request API adapter.
- `internal/workflow/codechange/*.go`: Resolve, Execute, Approval, Publish, and
  Finalize application services.
- `internal/workflow/codechangehatchet/*.go`: Hatchet DAG and durable approval
  event adapter.

### Packaging and acceptance files

- `charts/paje/*`: persistence, credential isolation, Codex auth seed, and beta
  limits.
- `Dockerfile`: pinned Codex-equipped non-root worker image.
- `internal/acceptance/*`: opt-in live Codex and GitHub tests.
- `README.md`: operator inputs, event payloads, artifact inspection, and beta
  limitations.

### Task 1: Deterministic and cancellable local execution

**Files:**
- Modify: `internal/runner/runner.go`
- Create: `internal/executil/process_unix.go`
- Create: `internal/executil/process_other.go`
- Create: `internal/executil/output.go`
- Create: `internal/executil/output_test.go`
- Modify: `internal/runner/local/runner.go`
- Modify: `internal/runner/local/runner_test.go`
- Remove after migration: `internal/runner/local/process_unix.go`
- Remove after migration: `internal/runner/local/process_other.go`

**Interfaces:**
- Produces:
  `executil.Configure(*exec.Cmd)`,
  `executil.NewLimitedBuffer(limit int64) (*LimitedBuffer, error)`,
  `(*LimitedBuffer).Bytes() []byte`, and
  `(*LimitedBuffer).Truncated() bool`.
- Extends `runner.ExecutionResult` with `Transcript string`, `Started bool`,
  `Completed bool`, and `Truncated bool`.
- Keeps `local.New(command string, args ...string) (*Runner, error)`.
- Adds
  `local.NewConfigured(command string, args []string, outputLimit int64)
  (*Runner, error)`.

- [ ] **Step 1: Write failing tests for exact environment, bounded output, and descendant cancellation**

Add a parent-only sentinel before invoking the helper and assert the child
cannot see it:

```go
func TestRunUsesOnlyRequestEnvironment(t *testing.T) {
    t.Setenv("PAJE_PARENT_SECRET", "must-not-leak")
    executor, err := local.New(os.Args[0], "-test.run=TestHelperProcess", "--")
    if err != nil {
        t.Fatal(err)
    }
    result, err := executor.Run(context.Background(), runner.RunRequest{
        TaskDescription: "print-parent-secret",
        WorkspacePath:   t.TempDir(),
        Env: map[string]string{
            "GO_WANT_HELPER_PROCESS": "1",
        },
    })
    if err != nil {
        t.Fatal(err)
    }
    if strings.Contains(result.Output, "must-not-leak") {
        t.Fatalf("ambient secret leaked: %q", result.Output)
    }
}

func TestRunBoundsCombinedOutput(t *testing.T) {
    executor, err := local.NewConfigured(
        os.Args[0],
        []string{"-test.run=TestHelperProcess", "--"},
        32,
    )
    if err != nil {
        t.Fatal(err)
    }
    got, err := executor.Run(context.Background(), runner.RunRequest{
        TaskDescription: "large-output",
        WorkspacePath:   t.TempDir(),
        Env: map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
    })
    if err != nil {
        t.Fatal(err)
    }
    if !got.Truncated || len(got.Transcript) != 32 {
        t.Fatalf("result = %#v, want 32 truncated bytes", got)
    }
}
```

Keep `TestRunCancellationTerminatesDescendantProcesses`, but lower its child
sleep to three seconds and retain the one-second upper bound.

- [ ] **Step 2: Run the focused tests and verify the ambient environment test fails**

Run:

```bash
go test ./internal/runner/local -run 'TestRun(UsesOnlyRequestEnvironment|BoundsCombinedOutput|CancellationTerminatesDescendantProcesses)' -v
```

Expected: FAIL because the current runner merges `os.Environ` and does not
expose bounded transcript metadata.

- [ ] **Step 3: Move process-group handling into `internal/executil`**

On Unix, configure a separate process group, kill the entire group on context
cancellation, and cap `Wait` cleanup:

```go
//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package executil

func Configure(command *exec.Cmd) {
    command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
    command.Cancel = func() error {
        if command.Process == nil {
            return os.ErrProcessDone
        }
        err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
        if errors.Is(err, syscall.ESRCH) {
            return os.ErrProcessDone
        }
        return err
    }
    command.WaitDelay = 5 * time.Second
}
```

The non-Unix file sets only `command.WaitDelay = 5 * time.Second`. Delete the
package-local process files after both local and verification packages import
`executil.Configure`.

- [ ] **Step 4: Implement a concurrency-safe limited writer**

`LimitedBuffer.Write` accepts all input, retains only the first `limit` bytes,
and marks truncation without returning `io.ErrShortWrite`:

```go
func (b *LimitedBuffer) Write(p []byte) (int, error) {
    b.mu.Lock()
    defer b.mu.Unlock()
    original := len(p)
    remaining := b.limit - int64(b.buffer.Len())
    if remaining <= 0 {
        b.truncated = b.truncated || original > 0
        return original, nil
    }
    if int64(len(p)) > remaining {
        p = p[:remaining]
        b.truncated = true
    }
    _, _ = b.buffer.Write(p)
    return original, nil
}
```

Test `limit <= 0`, writes on the boundary, a truncated write, defensive copies
from `Bytes`, and concurrent stdout/stderr writes.

- [ ] **Step 5: Make the local runner use an exact sorted environment and explicit `Start`/`Wait`**

Replace ambient merging with validation and sorting of only `req.Env`:

```go
func exactEnvironment(values map[string]string) ([]string, error) {
    keys := make([]string, 0, len(values))
    for key := range values {
        if key == "" || strings.ContainsRune(key, '=') {
            return nil, fmt.Errorf("run local agent: invalid environment key %q", key)
        }
        keys = append(keys, key)
    }
    sort.Strings(keys)
    result := make([]string, 0, len(keys))
    for _, key := range keys {
        result = append(result, key+"="+values[key])
    }
    return result, nil
}
```

Set the same limited buffer as `Stdout` and `Stderr`, call `Start` and `Wait`,
and set result evidence with these rules:

```go
result := runner.ExecutionResult{
    Transcript: string(output.Bytes()),
    Output:     string(output.Bytes()),
    Duration:   time.Since(started).Seconds(),
    Started:    startErr == nil,
    Truncated:  output.Truncated(),
}
if ctx.Err() == nil {
    result.Completed = waitErr == nil || errors.As(waitErr, &exitErr)
}
```

For a normal nonzero exit, return a completed result with nil error. For
startup, context, or wait failures, return the partial result and wrapped error.
A context-canceled process has `Started=true`, `Completed=false`, which the
workflow treats as ambiguous and non-retryable.

- [ ] **Step 6: Run execution tests and the race detector**

Run:

```bash
go fmt ./internal/executil ./internal/runner/...
go test -race ./internal/executil ./internal/runner/local ./internal/runner/mock
```

Expected: PASS, including cancellation in under one second and no inherited
`PAJE_PARENT_SECRET`.

- [ ] **Step 7: Commit the process foundation**

```bash
git add internal/executil internal/runner/runner.go internal/runner/local
git commit -m "feat: harden local process execution"
```

### Task 2: Deterministic Codex adapter and live orchestration baseline

**Files:**
- Create: `internal/runner/codex/runner.go`
- Create: `internal/runner/codex/runner_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `cmd/paje/main.go`
- Modify: `cmd/paje/main_test.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/workflow/codex_integration_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `runner.Runner` and the Task 1 execution metadata.
- Produces:
  `codex.New(command string) (*Runner, error)` and a `runner.Runner` that
  returns the last completed `agent_message` in `Output` while preserving JSONL
  in `Transcript`.
- Adds `codex` to accepted `PAJE_RUNNER_ADAPTER` values.

- [ ] **Step 1: Write Codex argument and JSONL tests**

The fixture must capture every argument and emit two completed agent messages:

```go
wantArgs := []string{
    "exec",
    "--json",
    "--ephemeral",
    "--ignore-user-config",
    "--sandbox",
    "workspace-write",
    "complete the task",
}
```

Assert `Output` equals the second message, `Transcript` still contains
`thread.started` and both messages, and a successful stream without an
`agent_message` returns an error. Assert nonzero Codex exits preserve the raw
diagnostic and exit code.

- [ ] **Step 2: Run the Codex package test and verify it fails when the adapter is absent**

Run:

```bash
go test ./internal/runner/codex -v
```

Expected on a clean checkout of the plan: FAIL because the package does not
exist. In the current working tree the candidate adapter already exists; verify
the new `Transcript` assertion fails until Task 1 metadata is wired through.

- [ ] **Step 3: Implement the Codex wrapper**

Construct the local delegate exactly once:

```go
delegate, err := local.New(
    command,
    "exec",
    "--json",
    "--ephemeral",
    "--ignore-user-config",
    "--sandbox",
    "workspace-write",
)
```

After a successful exit, scan `result.Transcript`, decode JSON objects, retain
the last event with `type == "item.completed"` and
`item.type == "agent_message"`, and assign only its text to `result.Output`.
Never clear `Transcript`, `Started`, `Completed`, or `Truncated`.

- [ ] **Step 4: Wire `PAJE_RUNNER_ADAPTER=codex`**

Extend configuration validation:

```go
if err := validateAdapter(
    "runner",
    config.RunnerAdapter,
    "mock",
    "local",
    "codex",
); err != nil {
    return Config{}, err
}
```

In `buildDependencies`, construct `codexrunner.New(cfg.RunnerCommand)`.
Unit tests must assert `codex` succeeds, an unknown adapter fails, and dependency
composition returns a Codex runner.

- [ ] **Step 5: Add the opt-in authenticated baseline test**

Keep the test gated by `PAJE_CODEX_INTEGRATION=1`. It must initialize a temporary
Git repository, seed one memory, use a real Git worktree manager and Codex
runner, ask Codex to read both README and memory, assert the exact terminal
response `PAJE_CODEX_ORCHESTRATED_OK`, assert the outcome memory was saved, and
assert the worktree directory is empty afterward.

- [ ] **Step 6: Run unit and real Codex verification**

Run:

```bash
go fmt ./internal/runner/codex ./internal/config ./cmd/paje ./internal/workflow
go test -race ./internal/runner/codex ./internal/config ./cmd/paje ./internal/workflow
PAJE_CODEX_INTEGRATION=1 go test ./internal/workflow -run TestCodexOrchestrationIntegration -v -count=1
```

Expected: PASS. The authenticated test should report a Codex terminal response
without the Mem0 plugin status line.

- [ ] **Step 7: Commit the Codex baseline**

```bash
git add README.md cmd/paje internal/config internal/runner/codex internal/workflow/codex_integration_test.go
git commit -m "feat: add deterministic Codex execution"
```

### Task 3: Typed template registry and strict `code-change@v1` input

**Files:**
- Create: `internal/template/template.go`
- Create: `internal/template/registry.go`
- Create: `internal/template/registry_test.go`
- Create: `internal/verification/types.go`
- Create: `internal/verification/types_test.go`
- Create: `internal/repository/types.go`
- Create: `internal/template/codechange/input.go`
- Create: `internal/template/codechange/input_test.go`

**Interfaces:**
- Produces `template.ID{Name string, Version int}`,
  `template.Template`, `template.Registry`, and
  `template.NewRegistry(definitions ...Template) (*Registry, error)`.
- Produces `verification.CommandSpec`, `verification.Command`,
  `verification.Result`, and
  `verification.Compile(spec CommandSpec, workspace string, limits Limits)
  (Command, error)`.
- Produces `codechange.ID`, `codechange.Input`, `codechange.Publication`,
  `repository.ModuleExclusion`, `codechange.Decode(json.RawMessage)`, and
  `codechange.Definition`.

- [ ] **Step 1: Write registry tests for duplicate, unknown, and versioned templates**

```go
func TestRegistryResolvesExactVersion(t *testing.T) {
    v1 := stubTemplate{id: template.ID{Name: "code-change", Version: 1}}
    registry, err := template.NewRegistry(v1)
    if err != nil {
        t.Fatal(err)
    }
    got, err := registry.Resolve(v1.id)
    if err != nil {
        t.Fatal(err)
    }
    if got.ID() != v1.id {
        t.Fatalf("Resolve() ID = %#v", got.ID())
    }
}
```

Add tests rejecting an empty name, non-positive version, nil definition,
duplicate ID, unknown name, and unknown version.

- [ ] **Step 2: Write strict input and command validation tests**

Decode this valid minimal input:

```json
{
  "task_description": "update the parser",
  "repository_uri": "https://github.com/araihu/paje.git",
  "base_ref": "main",
  "tags": {
    "user_id": "guilhermecastro",
    "app_id": "araihu-paje"
  },
  "profile": "go",
  "publication": {"mode": "artifact"}
}
```

Then table-test rejection of:

- unknown JSON fields;
- empty task, repository, or base ref;
- missing or blank `tags.user_id` or `tags.app_id`;
- memory limit below zero or above 1000;
- a check with a shell fragment instead of executable plus arguments;
- timeout strings that do not parse or exceed the configured limit;
- profile `generic` without at least one explicit check;
- duplicate module exclusions or an exclusion without a reason;
- `pull_request` without provider `github` and target branch;
- publication modes other than `artifact` and `pull_request`;
- environment keys containing `=` or duplicates.

- [ ] **Step 3: Verify the new packages fail before implementation**

Run:

```bash
go test ./internal/template/... ./internal/verification -v
```

Expected: FAIL because the packages and types are missing.

- [ ] **Step 4: Implement IDs and a concurrency-safe immutable registry**

Use a value map populated in the constructor:

```go
type ID struct {
    Name    string `json:"name"`
    Version int    `json:"version"`
}

type Template interface {
    ID() ID
    Validate(json.RawMessage) error
}

type Registry struct {
    definitions map[ID]Template
}

func (r *Registry) Resolve(id ID) (Template, error) {
    definition, ok := r.definitions[id]
    if !ok {
        return nil, fmt.Errorf("resolve template %s: %w", id, ErrUnknownTemplate)
    }
    return definition, nil
}
```

`ID.String()` returns `name@v<version>`, so the selected template renders
exactly `code-change@v1`.

- [ ] **Step 5: Implement wire-safe command compilation**

`CommandSpec.Timeout` is a Go duration string; `Command.Timeout` is
`time.Duration`:

```go
type CommandSpec struct {
    Name       string   `json:"name"`
    Directory  string   `json:"directory"`
    Executable string   `json:"executable"`
    Args       []string `json:"args"`
    Timeout    string   `json:"timeout"`
    Required   bool     `json:"required"`
}

type Command struct {
    Name       string
    Directory  string
    Executable string
    Args       []string
    Timeout    time.Duration
    Required   bool
}

type Result struct {
    Command      Command       `json:"command"`
    ExitCode     int           `json:"exit_code"`
    Duration     time.Duration `json:"duration"`
    Output       string        `json:"output"`
    Truncated    bool          `json:"truncated"`
    Passed       bool          `json:"passed"`
    Warning      bool          `json:"warning"`
    FailureClass string        `json:"failure_class,omitempty"`
    CauseCode    string        `json:"cause_code,omitempty"`
}
```

Resolve directories with `filepath.Abs` and `filepath.Rel`; reject `..`,
absolute input directories, NUL bytes, empty executables, counts over limits,
and timeouts outside `1s..limits.MaxTimeout`. Never accept a single shell
string.

- [ ] **Step 6: Implement strict `codechange.Decode` and defaults**

Use `json.Decoder.DisallowUnknownFields`, require EOF after the first JSON
value, default memory limit to 10, profile to `generic`, and publication mode to
`artifact`. Sort and deduplicate environment keys and tags before canonical
encoding. `Definition.Validate` calls `Decode` and discards the typed result.
`Input.ModuleExclusions` uses `[]repository.ModuleExclusion`, whose wire fields
are exactly `path` and `reason`.

```go
type Input struct {
    IdempotencyKey   string                       `json:"idempotency_key,omitempty"`
    TaskDescription string                       `json:"task_description"`
    RepositoryURI   string                       `json:"repository_uri"`
    BaseRef         string                       `json:"base_ref"`
    MemoryQuery     string                       `json:"memory_query,omitempty"`
    MemoryLimit     int                          `json:"memory_limit,omitempty"`
    Tags            map[string]string            `json:"tags,omitempty"`
    Profile         string                       `json:"profile,omitempty"`
    Checks          []verification.CommandSpec   `json:"checks,omitempty"`
    ModuleExclusions []repository.ModuleExclusion `json:"module_exclusions,omitempty"`
    EnvironmentKeys []string                     `json:"environment_keys,omitempty"`
    Publication     Publication                  `json:"publication,omitempty"`
}

type Publication struct {
    Mode         string `json:"mode,omitempty"`
    Provider     string `json:"provider,omitempty"`
    TargetBranch string `json:"target_branch,omitempty"`
    Title        string `json:"title,omitempty"`
    Draft        bool   `json:"draft,omitempty"`
}
```

```go
var ID = template.ID{Name: "code-change", Version: 1}

func Decode(raw json.RawMessage) (Input, error) {
    decoder := json.NewDecoder(bytes.NewReader(raw))
    decoder.DisallowUnknownFields()
    var input Input
    if err := decoder.Decode(&input); err != nil {
        return Input{}, fmt.Errorf("decode code-change@v1 input: %w", err)
    }
    if err := requireJSONEOF(decoder); err != nil {
        return Input{}, err
    }
    return normalizeAndValidate(input)
}
```

- [ ] **Step 7: Run tests and commit the typed boundary**

Run:

```bash
go fmt ./internal/template/... ./internal/verification ./internal/repository
go test -race ./internal/template/... ./internal/verification ./internal/repository
```

Expected: PASS.

```bash
git add internal/template internal/verification/types.go internal/verification/types_test.go internal/repository/types.go
git commit -m "feat: define code change template input"
```

### Task 4: Allowlisted environments and bounded verification

**Files:**
- Create: `internal/environment/policy.go`
- Create: `internal/environment/policy_test.go`
- Create: `internal/verification/executor.go`
- Create: `internal/verification/executor_test.go`

**Interfaces:**
- Produces:
  `environment.NewPolicy(Config) (*Policy, error)`,
  `(*Policy).Build(context.Context, Request) (Result, error)`, and
  `(*Policy).Cleanup(context.Context, runID string) error`.
- Produces:
  `verification.NewExecutor(Limits) (*Executor, error)` and
  `(*Executor).Run(context.Context, Command, map[string]string) Result`.
- Consumes `executil.Configure` and `executil.LimitedBuffer` from Task 1.

- [ ] **Step 1: Write policy tests proving secrets are denied**

Construct a source map containing both ordinary and worker credentials:

```go
source := map[string]string{
    "PATH":                 "/tools",
    "LANG":                 "C.UTF-8",
    "CODEX_HOME":           "/auth/codex",
    "SAFE_CACHE":           "/cache",
    "HATCHET_CLIENT_TOKEN": "hatchet-secret",
    "MEM0_API_KEY":         "mem0-secret",
    "GITHUB_TOKEN":         "github-secret",
}
policy, err := environment.NewPolicy(environment.Config{
    RuntimeRoot: t.TempDir(),
    Source:      source,
    Allowed:     []string{"SAFE_CACHE"},
    CodexHome:   "/auth/codex",
})
```

For an agent-stage request containing `SAFE_CACHE`, assert the result contains
`PATH`, a fresh `HOME`, a fresh `TMPDIR`, `LANG`, `SAFE_CACHE`, and
`CODEX_HOME`. Assert it does not contain any of the three worker credentials.
Assert requesting a denied or unknown key fails before process execution.

- [ ] **Step 2: Write executor tests for directory escape, timeout, output limit, and environment failures**

Use the helper-process pattern and compiled commands. Required assertions:

```go
result := executor.Run(ctx, verification.Command{
    Name:       "exit seven",
    Directory:  workspace,
    Executable: os.Args[0],
    Args:       []string{"-test.run=TestVerificationHelper", "--", "exit-7"},
    Timeout:    time.Second,
    Required:   true,
}, map[string]string{"GO_WANT_VERIFY_HELPER": "1"})

if result.ExitCode != 7 || result.FailureClass != "verification" {
    t.Fatalf("result = %#v", result)
}
```

Also prove:

- `../outside` is rejected before `Start`;
- a missing executable is classed `environment`;
- a `docker` command emitting `rootless Docker not found` or
  `Cannot connect to the Docker daemon` is classed `environment` even though
  the process completed nonzero;
- timeout kills a descendant process within one second and is classed
  `environment`;
- output is truncated exactly at `Limits.MaxOutputBytes`;
- optional environment failures set `Warning=true`;
- required environment failures set `Passed=false` and `Warning=false`.

- [ ] **Step 3: Run focused tests and verify they fail**

Run:

```bash
go test ./internal/environment ./internal/verification -run 'Test(Policy|Executor)' -v
```

Expected: FAIL because neither implementation exists.

- [ ] **Step 4: Implement the stage-aware environment policy**

Use explicit stages:

```go
type Stage string

const (
    StageAgent        Stage = "agent"
    StageVerification Stage = "verification"
    StagePublisher    Stage = "publisher"
)

type Request struct {
    RunID         string
    Stage         Stage
    RequestedKeys []string
}

type Result struct {
    Values   map[string]string
    Keys     []string
    Redacted map[string]bool
}
```

Create `<runtimeRoot>/<validated-run-id>/<stage>/{home,tmp}` with mode `0700`.
Copy only `PATH`, locale, certificate variables, operator-allowed requested
keys, and stage-specific keys. Add `CODEX_HOME` only for `StageAgent`. Add
GitHub credentials only for `StagePublisher`. Evidence exposes sorted key names
and redaction markers, never values. Validate run IDs against
`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$` before joining paths.

- [ ] **Step 5: Implement shell-free bounded verification**

`Executor.Run` creates a child context using `command.Timeout`, configures the
process group, assigns the exact environment, and uses a limited shared
stdout/stderr writer:

```go
childCtx, cancel := context.WithTimeout(ctx, command.Timeout)
defer cancel()

cmd := exec.CommandContext(childCtx, command.Executable, command.Args...)
executil.Configure(cmd)
cmd.Dir = command.Directory
cmd.Env = exactEnvironment(environment)
cmd.Stdout = output
cmd.Stderr = output
```

Populate `verification.Result` with name, directory, executable, arguments,
exit code, duration, bounded output, truncation, passed/warning flags, and
failure class. Classify:

- `exec.ErrNotFound`, permission errors, and context timeout as `environment`;
- a completed nonzero `docker` exit containing one of the bounded,
  case-insensitive diagnostics `rootless Docker not found`,
  `Cannot connect to the Docker daemon`, `error during connect`, or
  `Is the docker daemon running` as `environment`;
- every other completed nonzero exit as `verification`;
- caller cancellation as `canceled`;
- internal validation or wait failures as `internal`.

Never include environment values in the result.

- [ ] **Step 6: Run policy and executor tests under race**

Run:

```bash
go fmt ./internal/environment ./internal/verification
go test -race ./internal/environment ./internal/verification
```

Expected: PASS.

- [ ] **Step 7: Commit the execution policy**

```bash
git add internal/environment internal/verification
git commit -m "feat: add isolated verification environment"
```

### Task 5: Immutable revision resolution and repository profiles

**Files:**
- Create: `internal/repository/repository.go`
- Create: `internal/repository/profile.go`
- Create: `internal/repository/profile_test.go`
- Modify: `internal/workspace/gitworktree/manager.go`
- Modify: `internal/workspace/gitworktree/manager_test.go`

**Interfaces:**
- Produces:
  `repository.Resolver.Resolve(context.Context, uri, ref) (Revision, error)`,
  `repository.Profile.Inspect(context.Context, ProfileRequest)
  (ProfileResult, error)`, `repository.NewGenericProfile`, and
  `repository.NewGoProfile`.
- Extends `gitworktree.Manager` to implement both `repository.Resolver` and
  `workspace.Manager`; `Prepare` accepts an immutable SHA once Resolve
  completes.
- Consumes Task 4's verification executor and exact environment.

```go
type Revision struct {
    RepositoryURI string `json:"repository_uri"`
    Ref           string `json:"ref"`
    SHA           string `json:"sha"`
    SourceDirty   bool   `json:"source_dirty"`
}

type Resolver interface {
    Resolve(context.Context, string, string) (Revision, error)
}

type Profile interface {
    Name() string
    Inspect(context.Context, ProfileRequest) (ProfileResult, error)
}
```

- [ ] **Step 1: Write revision tests for remote SHA and dirty local sources**

Create a local repository with `main`, clone it as a bare remote, and assert:

```go
revision, err := manager.Resolve(ctx, remote, "refs/heads/main")
if err != nil {
    t.Fatal(err)
}
if len(revision.SHA) != 40 || revision.Ref != "refs/heads/main" {
    t.Fatalf("revision = %#v", revision)
}
workspace, err := manager.Prepare(ctx, remote, revision.SHA)
```

For a local checkout, modify a tracked file and assert
`Revision.SourceDirty == true` while SHA resolution still points at `HEAD`.
Reject refs beginning with `-`, missing commits, blank URI, and blank ref.

- [ ] **Step 2: Write generic and multi-module Go profile tests**

Build a fixture with root `go.mod`, `site/go.mod`, a `go.work` that replaces a
dependency, and one explicitly excluded `tools/go.mod`. Assert:

- generic profile compiles only configured commands;
- Go profile discovers all three tracked modules in lexical order;
- without an exclusion, checks are produced for all three;
- with `{Path: "tools", Reason: "generator-only module"}`, the tools module is
  omitted and the exact reason appears in warnings;
- each generated `go test ./...` command carries `GOWORK=off`;
- preflight records the ambient `go env GOWORK` value but module resolution
  uses `GOWORK=off go list -m -json`;
- an exclusion naming an undiscovered module fails validation.

- [ ] **Step 3: Run focused tests and verify failures**

Run:

```bash
go test ./internal/workspace/gitworktree ./internal/repository -v
```

Expected: FAIL because `Resolve` and repository profiles are undefined.

- [ ] **Step 4: Refactor mirror refresh and implement exact SHA resolution**

Under the existing manager mutex, update the mirror, then resolve a commit with:

```go
output, err := m.gitOutput(
    ctx,
    "--git-dir", mirror,
    "rev-parse", "--verify", ref+"^{commit}",
)
```

Validate the output with `^[0-9a-f]{40}$`. For local repository URIs, run
`git -C <path> status --porcelain=v1 -z` and set `SourceDirty` from non-empty
output. Record both requested ref and immutable SHA. Keep mirror and worktree
roots under the configured workspace root.

- [ ] **Step 5: Implement profile types and generic command compilation**

```go
type ProfileRequest struct {
    Workspace       string
    Environment     map[string]string
    Checks          []verification.CommandSpec
    ModuleExclusions []ModuleExclusion
}

type ProfileResult struct {
    Facts    map[string]string
    Warnings []string
    Modules  []string
    Commands []verification.Command
}
```

The generic profile records base SHA, `git status --porcelain=v1`, and tool
availability. It requires at least one explicit check. It compiles every check
through `verification.Compile`.

- [ ] **Step 6: Implement the Go profile with `GOWORK=off`**

Discover modules using Git, not a filesystem walk:

```bash
git -C <workspace> ls-files -z -- go.mod '**/go.mod'
```

Record `go env GOWORK` with the verification environment. Resolve every
selected module with `GOWORK=off go list -m -json`; an unavailable Go binary is
an environment failure. If the input does not provide checks, generate:

```go
verification.CommandSpec{
    Name:       "go test " + modulePath,
    Directory:  modulePath,
    Executable: "go",
    Args:       []string{"test", "./..."},
    Timeout:    "10m",
    Required:   true,
}
```

Apply a provided check list independently in every selected module. Inject
`GOWORK=off` into the environment used by resolution and verification, not into
shell syntax. For each module, join its path with the check's relative
`Directory`; compilation must prove the joined directory remains inside that
module and the workspace.

- [ ] **Step 7: Run repository tests and commit**

Run:

```bash
go fmt ./internal/repository ./internal/workspace/gitworktree
go test -race ./internal/repository ./internal/workspace/gitworktree
```

Expected: PASS.

```bash
git add internal/repository internal/workspace/gitworktree
git commit -m "feat: resolve revisions and repository profiles"
```

### Task 6: Canonical durable artifact store

**Files:**
- Create: `internal/artifact/artifact.go`
- Create: `internal/artifact/filesystem/store.go`
- Create: `internal/artifact/filesystem/store_test.go`
- Create: `internal/artifact/mock/store.go`
- Create: `internal/artifact/mock/store_test.go`

**Interfaces:**
- Produces `artifact.Reference`, `artifact.Manifest`, `artifact.Bundle`,
  `artifact.Store`, `filesystem.New(root string, maxCompressedBytes int64)`,
  `artifact.ReferenceFor(Bundle) (Reference, error)`,
  `artifact.CloneBundle(Bundle) Bundle`, and `artifactmock.NewStore`.
- Consumes `verification.Result` for evidence serialization.

- [ ] **Step 1: Write store tests for determinism, restart, corruption, and limits**

Build one bundle twice and require identical references:

```go
first, err := store.Save(ctx, bundle)
if err != nil {
    t.Fatal(err)
}
second, err := store.Save(ctx, bundle)
if err != nil {
    t.Fatal(err)
}
if first.Digest != second.Digest || first.Size != second.Size {
    t.Fatalf("references differ: %#v %#v", first, second)
}
```

Construct a second store using the same root and assert `Load` returns an equal
bundle. Flip one byte in the stored gzip member and assert `Load` returns
`artifact.ErrDigestMismatch`. Configure a 64-byte limit and assert an oversized
save returns `artifact.ErrTooLarge` without leaving a temp or final bundle.

- [ ] **Step 2: Verify artifact tests fail before implementation**

Run:

```bash
go test ./internal/artifact/... -v
```

Expected: FAIL because the artifact packages do not exist.

- [ ] **Step 3: Define the fixed bundle schema**

```go
type Reference struct {
    RunID  string `json:"run_id"`
    Digest string `json:"digest"`
    Size   int64  `json:"size"`
}

type Manifest struct {
    SchemaVersion int          `json:"schema_version"`
    RunID         string       `json:"run_id"`
    Template      template.ID  `json:"template"`
    Repository    string       `json:"repository"`
    BaseSHA       string       `json:"base_sha"`
    TreeSHA       string       `json:"tree_sha"`
    Changes       []Change     `json:"changes"`
    Members       []Member     `json:"members"`
    MemoryIDs     []string     `json:"memory_ids,omitempty"`
    MemoryCount   int          `json:"memory_count"`
}

type Change struct {
    Path    string `json:"path"`
    OldPath string `json:"old_path,omitempty"`
    Status  string `json:"status"`
    OldMode string `json:"old_mode,omitempty"`
    NewMode string `json:"new_mode,omitempty"`
}

type Member struct {
    Name   string `json:"name"`
    SHA256 string `json:"sha256"`
    Size   int64  `json:"size"`
}

type Bundle struct {
    Manifest          Manifest              `json:"manifest"`
    ChangesPatch      []byte                `json:"-"`
    AgentOutput       []byte                `json:"-"`
    ExecutionMetadata json.RawMessage       `json:"execution_metadata"`
    Verification      []verification.Result `json:"verification"`
    Preflight         map[string]string     `json:"preflight"`
    Warnings          []string              `json:"warnings"`
}

type Store interface {
    Save(context.Context, Bundle) (Reference, error)
    Load(context.Context, Reference) (Bundle, error)
}
```

The only archive members are:
`manifest.json`, `changes.patch`, `agent-output.txt`,
`execution.json`, `verification.json`, `preflight.json`, and `warnings.json`.
`Manifest` includes schema version `1`, run/template/repository/base metadata,
changed paths/modes, memory IDs/count, and SHA-256 plus size for every
non-manifest member.

- [ ] **Step 4: Implement canonical tar generation and gzip storage**

JSON-encode with stable struct fields and sort every map into a key/value slice
before encoding. Emit tar entries in the fixed member order with mode `0600`,
uid/gid `0`, empty names, and Unix epoch timestamps. Hash the uncompressed tar
bytes:

```go
sum := sha256.Sum256(canonicalTar)
digest := hex.EncodeToString(sum[:])
```

Compress with gzip level 6 and zeroed header time/name/comment. Write to a temp
file under `<root>/tmp`, fsync it, reject compressed size over the configured
limit, rename to `<root>/sha256/<first-two>/<digest>.tar.gz`, and fsync both
directories. An existing matching digest is an idempotent success.

- [ ] **Step 5: Implement verified loading**

Open only the path derived from the validated 64-character lowercase hex
digest. Limit compressed reads to `maxCompressedBytes` and uncompressed reads
to `16 * maxCompressedBytes`, decompress, recompute the canonical digest,
reject duplicate/unknown/missing members, verify every member digest and size
from the manifest, and return defensive byte/map/slice copies. Constructor
validation rejects a compressed limit whose multiplication would overflow.

```go
func (s *Store) Load(ctx context.Context, ref artifact.Reference) (artifact.Bundle, error) {
    path, err := s.pathFor(ref)
    if err != nil {
        return artifact.Bundle{}, err
    }
    canonicalTar, err := readAndDecompress(ctx, path, s.maxCompressed, s.maxUncompressed)
    if err != nil {
        return artifact.Bundle{}, err
    }
    if digest(canonicalTar) != ref.Digest {
        return artifact.Bundle{}, artifact.ErrDigestMismatch
    }
    bundle, err := decodeCanonicalTar(canonicalTar)
    if err != nil {
        return artifact.Bundle{}, err
    }
    if bundle.Manifest.RunID != ref.RunID {
        return artifact.Bundle{}, artifact.ErrDigestMismatch
    }
    return artifact.CloneBundle(bundle), nil
}
```

- [ ] **Step 6: Implement a concurrency-safe mock**

The mock calculates the same logical digest using the shared canonical encoder,
stores deep copies keyed by digest, records save/load calls, and exposes a
snapshot for phase tests. It can be configured to return deterministic save or
load failures.

```go
func (s *Store) Save(_ context.Context, bundle artifact.Bundle) (artifact.Reference, error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.saveErr != nil {
        return artifact.Reference{}, s.saveErr
    }
    ref, err := artifact.ReferenceFor(bundle)
    if err != nil {
        return artifact.Reference{}, err
    }
    s.bundles[ref.Digest] = artifact.CloneBundle(bundle)
    s.saves = append(s.saves, ref)
    return ref, nil
}
```

- [ ] **Step 7: Run artifact tests and commit**

Run:

```bash
go fmt ./internal/artifact/...
go test -race ./internal/artifact/...
```

Expected: PASS.

```bash
git add internal/artifact
git commit -m "feat: add durable artifact bundles"
```

### Task 7: Git change capture, reproduction, and built-in policy

**Files:**
- Create: `internal/artifact/gitcapture/capture.go`
- Create: `internal/artifact/gitcapture/capture_test.go`
- Create: `internal/policy/change.go`
- Create: `internal/policy/change_test.go`

**Interfaces:**
- Produces:
  `gitcapture.Capturer`,
  `gitcapture.New() (*Git, error)`,
  `(*Git).Capture(context.Context, Request) (Result, error)`, and
  `(*Git).Apply(context.Context, ApplyRequest) error`.
- Produces:
  `policy.Evaluator`,
  `policy.NewChangePolicy(Config) (*ChangePolicy, error)`, and
  `(*ChangePolicy).Evaluate(context.Context, gitcapture.Result) Decision`.
- Consumes Task 6's artifact manifest fields and patch bytes.

```go
type Request struct {
    Workspace string
    BaseSHA   string
    MaxBytes  int64
}

type Result struct {
    Patch   []byte
    Changes []artifact.Change
    TreeSHA string
}

type ApplyRequest struct {
    Workspace      string
    BaseSHA        string
    Patch          []byte
    ExpectedTreeSHA string
}
```

- [ ] **Step 1: Write a full-fidelity patch integration test**

Initialize a repository containing a text file, a file to delete, a file to
rename, a non-executable script, and a binary fixture. In a detached worktree:

- modify the text file;
- delete one file;
- rename one file;
- add an untracked file;
- change the script to mode `0755`;
- create a relative symlink;
- modify binary bytes containing NUL.

Capture from the original base SHA and apply to a second fresh worktree.
Populate independent temporary indexes from each resulting filesystem using
`read-tree <base>` plus `add -A -- .`, then compare `git write-tree` and
`git ls-files --stage -z`. The tree IDs and staged path/mode/object entries must
match exactly. Separately assert the changed source worktree's real index is
unchanged by capture; the reproduction worktree index changes only when
`git apply --index` is intentionally invoked.

- [ ] **Step 2: Write policy denial tests**

Table-test:

- `../escape` or absolute changed paths;
- Gitlink/submodule mode `160000`;
- `.env`, `id_rsa`, `*.pem`, `credentials.json`, and `.npmrc`;
- added patch lines matching private-key headers, GitHub tokens, and common
  `*_SECRET=<value>` assignments;
- an ordinary source patch.

A denial contains stable rule IDs and normalized paths, never the matched
secret value:

```go
if strings.Contains(fmt.Sprint(decision.Findings), "super-secret-value") {
    t.Fatal("policy finding leaked matched value")
}
```

- [ ] **Step 3: Run tests and verify missing-package failures**

Run:

```bash
go test ./internal/artifact/gitcapture ./internal/policy -v
```

Expected: FAIL because capture and policy packages are absent.

- [ ] **Step 4: Capture with a temporary Git index**

Allocate an index file outside the worktree and run each command with
`GIT_INDEX_FILE` set and `Dir` equal to the worktree:

```bash
git read-tree <base-sha>
git add -A -- .
git diff --cached --binary --full-index --no-ext-diff <base-sha> --
git diff --cached --name-status -z <base-sha> --
git ls-files --stage -z
```

Reject a dirty real index by recording its checksum before and after capture.
Parse NUL-delimited output, normalize slash-separated paths, sort manifest
entries, and record old/new modes. Limit Git diagnostics to 4096 bytes and
patch bytes to the operator limit.

Define the port implemented by `*Git`:

```go
type Capturer interface {
    Capture(context.Context, Request) (Result, error)
    Apply(context.Context, ApplyRequest) error
}
```

- [ ] **Step 5: Implement exact patch application**

Validate the requested artifact first, then run:

```bash
git -C <fresh-worktree> apply --index --binary --whitespace=nowarn <patch-file>
```

The patch file lives outside the worktree, is mode `0600`, and is removed with
a non-canceled bounded cleanup context. After application, build a temporary
index tree and compare it with `Result.TreeSHA`; a mismatch returns
`gitcapture.ErrTreeMismatch`.

- [ ] **Step 6: Implement non-overridable beta policy**

`ChangePolicy.Evaluate` first validates every path with `filepath.Rel`, then
checks modes, sensitive basename/suffix rules, and added patch lines. Use
compiled regexes whose findings contain only rule ID, path, and line number:

```go
type Finding struct {
    RuleID string `json:"rule_id"`
    Path   string `json:"path"`
    Line   int    `json:"line,omitempty"`
}

type Decision struct {
    Allowed  bool      `json:"allowed"`
    Findings []Finding `json:"findings"`
}
```

The workflow input has no policy-disable field. A denial is non-retryable and
the patch bytes are not saved to the artifact store.

- [ ] **Step 7: Run integration tests and commit**

Run:

```bash
go fmt ./internal/artifact/gitcapture ./internal/policy
go test -race ./internal/artifact/gitcapture ./internal/policy
```

Expected: PASS.

```bash
git add internal/artifact/gitcapture internal/policy
git commit -m "feat: capture and police Git artifacts"
```

### Task 8: Typed approval and publication contracts

**Files:**
- Modify: `internal/approval/approval.go`
- Modify: `internal/approval/mock/gate.go`
- Modify: `internal/approval/mock/gate_test.go`
- Create: `internal/publisher/publisher.go`
- Create: `internal/publisher/mock/publisher.go`
- Create: `internal/publisher/mock/publisher_test.go`

**Interfaces:**
- Replaces the approval boolean with
  `approval.Gate.RequestApproval(context.Context, Request) (Result, error)`.
- Produces `publisher.Request`, `publisher.Result`, `publisher.Publisher`, and
  `publisher.ErrConflict`.
- Produces concurrency-safe approval and publisher mocks with defensive request
  snapshots and configurable results.

- [ ] **Step 1: Rewrite approval mock tests around immutable binding fields**

```go
request := approval.Request{
    RunID:             "run-123",
    TemplateID:        "code-change@v1",
    Repository:        "github.com/araihu/paje",
    BaseSHA:           strings.Repeat("a", 40),
    TargetBranch:      "main",
    PublicationMode:   "pull_request",
    PublicationBranch: "paje/code-change/run-123",
    ArtifactDigest:    strings.Repeat("b", 64),
    Description:       "update parser",
}
decision := approval.Result{
    RunID:          request.RunID,
    ArtifactDigest: request.ArtifactDigest,
    Approved:       true,
    Actor:          "reviewer@example.test",
    DecidedAt:      time.Unix(1, 0).UTC(),
}
```

Assert the mock returns the configured decision, records a defensive copy, and
propagates configured errors. Add a validation test rejecting a result whose
run ID or artifact digest differs from its request.

- [ ] **Step 2: Write publisher mock tests**

Configure one immutable publication:

```go
want := publisher.Result{
    Provider:       "github",
    Branch:         "paje/code-change/run-123",
    CommitSHA:      strings.Repeat("c", 40),
    PullRequestID:  "42",
    PullRequestURL: "https://github.com/araihu/paje/pull/42",
}
```

Assert request maps and artifact references are copied. Assert the mock call
count stays zero in a declined-approval workflow fixture.

- [ ] **Step 3: Run tests and verify compile failures from the old boolean port**

Run:

```bash
go test ./internal/approval/... ./internal/publisher/... -v
```

Expected: FAIL because the old gate returns `bool` and publisher packages are
missing.

- [ ] **Step 4: Define the typed approval port**

```go
type Request struct {
    RunID             string                `json:"run_id"`
    TemplateID        string                `json:"template_id"`
    Repository        string                `json:"repository"`
    BaseSHA           string                `json:"base_sha"`
    TargetBranch      string                `json:"target_branch"`
    PublicationMode   string                `json:"publication_mode"`
    PublicationBranch string                `json:"publication_branch"`
    ArtifactDigest    string                `json:"artifact_digest"`
    Description       string                `json:"description"`
    AgentSummary      string                `json:"agent_summary"`
    ChangedPaths      []string              `json:"changed_paths"`
    Verification      []verification.Result `json:"verification"`
    Warnings          []string              `json:"warnings"`
}

type Result struct {
    RunID          string    `json:"run_id"`
    ArtifactDigest string    `json:"artifact_digest"`
    Approved       bool      `json:"approved"`
    Actor          string    `json:"actor"`
    DecidedAt      time.Time `json:"decided_at"`
    Reason         string    `json:"reason,omitempty"`
}

type Gate interface {
    RequestApproval(context.Context, Request) (Result, error)
}
```

`Request.Validate` requires every binding field, a 40-character base SHA, a
64-character artifact digest, and the deterministic branch. `Result.Validate`
requires a matching run/digest, non-empty actor, non-zero UTC decision time,
and a reason when declined.

- [ ] **Step 5: Define the provider-neutral publisher port**

```go
type Request struct {
    RunID       string             `json:"run_id"`
    Repository  string             `json:"repository"`
    BaseSHA     string             `json:"base_sha"`
    TargetRef   string             `json:"target_ref"`
    Branch      string             `json:"branch"`
    Artifact    artifact.Reference `json:"artifact"`
    Title       string             `json:"title"`
    Body        string             `json:"body"`
    Draft       bool               `json:"draft"`
}

type Result struct {
    Provider       string `json:"provider"`
    Branch         string `json:"branch"`
    CommitSHA      string `json:"commit_sha"`
    PullRequestID  string `json:"pull_request_id"`
    PullRequestURL string `json:"pull_request_url"`
}
```

Validate run IDs, exact branch `paje/code-change/<run-id>`, base and commit
SHAs, HTTPS pull-request URLs, and non-empty provider IDs. Define sentinel
errors for invalid request, conflict, and unavailable provider.

- [ ] **Step 6: Update mocks and dependent legacy tests**

Make `approval/mock.NewGate(result approval.Result, err error)` return typed
results. Update any legacy test that constructed a boolean gate. Both mocks use
mutexes and clone nested maps/slices/reference values on ingress and egress.

```go
func (g *Gate) RequestApproval(
    _ context.Context,
    req approval.Request,
) (approval.Result, error) {
    g.mu.Lock()
    defer g.mu.Unlock()
    g.requests = append(g.requests, approval.CloneRequest(req))
    return g.result, g.err
}
```

- [ ] **Step 7: Run contract tests and commit**

Run:

```bash
go fmt ./internal/approval/... ./internal/publisher/...
go test -race ./internal/approval/... ./internal/publisher/...
```

Expected: PASS.

```bash
git add internal/approval internal/publisher
git commit -m "feat: type approval and publication contracts"
```

### Task 9: Durable run state machine and compare-and-swap store

**Files:**
- Create: `internal/run/run.go`
- Create: `internal/run/state.go`
- Create: `internal/run/state_test.go`
- Create: `internal/run/filesystem/store.go`
- Create: `internal/run/filesystem/store_test.go`
- Create: `internal/run/mock/store.go`
- Create: `internal/run/mock/store_test.go`

**Interfaces:**
- Produces `run.Record`, `run.StageResult`, `run.Failure`,
  `run.Reservation`, `run.Store`, `run.Transition`, and terminal/retry helpers.
- Produces `runfilesystem.New(root string) (*Store, error)` and
  `runmock.NewStore`.
- Consumes template, memory, artifact, approval, publisher, and verification
  value types without importing workflow services.

```go
type Reservation struct {
    NewRunID       string
    Template       template.ID
    IdempotencyKey string
    InputHash      string
    Input          json.RawMessage
    RepositoryURI  string
    BaseRef        string
    PublicationMode string
    CreatedAt      time.Time
}

type Store interface {
    Reserve(context.Context, Reservation) (record Record, created bool, err error)
    Load(context.Context, string) (Record, error)
    Save(context.Context, Record, uint64) (Record, error)
}
```

- [ ] **Step 1: Write table tests for legal and illegal transitions**

Legal sequence:

```text
pending -> resolving -> executing -> awaiting_approval -> publishing -> succeeded
```

Also allow:

- `executing -> succeeded` only during Finalize for a successful artifact-only
  run;
- active state -> `failed`;
- active state -> `canceled`;
- `awaiting_approval -> declined`.

Reject backward moves, changing any terminal state, publishing without an
artifact, awaiting approval without an artifact, succeeded pull-request mode
without publication, and approval whose digest differs from the artifact.

- [ ] **Step 2: Write reservation, CAS, restart, and atomic-file tests**

Reserve the same template/key/input hash twice and assert one run ID is reused.
Reserve the same template/key with another hash and expect
`run.ErrIdempotencyConflict`. Save version 1 twice with expected version 1 and
assert the second returns `run.ErrVersionConflict`. Reopen the filesystem store
and load all nested stage, memory, artifact, approval, and publication fields.
After every write, assert no `.tmp` file remains.

- [ ] **Step 3: Run tests and verify missing-package failures**

Run:

```bash
go test ./internal/run/... -v
```

Expected: FAIL because the run packages do not exist.

- [ ] **Step 4: Define record and failure types**

```go
type Record struct {
    ID              string             `json:"id"`
    Version         uint64             `json:"version"`
    Template        template.ID        `json:"template"`
    IdempotencyKey  string             `json:"idempotency_key,omitempty"`
    InputHash       string             `json:"input_hash"`
    Input           json.RawMessage    `json:"input"`
    Status          Status             `json:"status"`
    PublicationMode string             `json:"publication_mode"`
    RepositoryURI   string             `json:"repository_uri"`
    BaseRef         string             `json:"base_ref"`
    BaseSHA         string             `json:"base_sha,omitempty"`
    MemorySnapshot  []memory.Memory    `json:"memory_snapshot,omitempty"`
    Artifact        *artifact.Reference `json:"artifact,omitempty"`
    Approval        *approval.Result   `json:"approval,omitempty"`
    Publication     *publisher.Result  `json:"publication,omitempty"`
    OutcomeMemorySaved bool            `json:"outcome_memory_saved"`
    Stages          []StageResult      `json:"stages"`
    Failure         *Failure           `json:"failure,omitempty"`
    CreatedAt       time.Time          `json:"created_at"`
    UpdatedAt       time.Time          `json:"updated_at"`
}
```

Define stage and failure evidence explicitly:

```go
type StageStatus string

const (
    StageRunning   StageStatus = "running"
    StageSucceeded StageStatus = "succeeded"
    StageSkipped   StageStatus = "skipped"
    StageWarning   StageStatus = "warning"
    StageFailed    StageStatus = "failed"
)

type StageResult struct {
    Name       string            `json:"name"`
    Status     StageStatus       `json:"status"`
    StartedAt  time.Time         `json:"started_at"`
    FinishedAt time.Time         `json:"finished_at,omitempty"`
    Attempts   int               `json:"attempts"`
    Evidence   map[string]string `json:"evidence,omitempty"`
    Failure    *Failure          `json:"failure,omitempty"`
}

type Failure struct {
    Stage       string       `json:"stage"`
    Class       FailureClass `json:"class"`
    Retryable   bool         `json:"retryable"`
    Diagnostic  string       `json:"diagnostic"`
    CauseCode   string       `json:"cause_code"`
}
```

Define all statuses and failure classes from the design, including
`canceled`. `SafeDiagnostic` strips control characters and truncates to 4096
bytes.

- [ ] **Step 5: Implement state validation and transitions**

`Transition(record, next, now)` clones the record, validates the edge and
state-specific invariants, updates timestamps, and never changes `Version`;
the store owns version increments. `Record.Terminal()` returns true only for
`succeeded`, `failed`, `canceled`, and `declined`.

Provide a stage upsert keyed by stage name and attempt:

```go
func UpsertStage(record Record, result StageResult) (Record, error)
```

It rejects a lower attempt or a finished stage overwritten by an unfinished
one.

- [ ] **Step 6: Implement atomic reservation and CAS persistence**

Filesystem layout:

```text
<root>/runs/<run-id>.json
<root>/idempotency/<sha256(template-id + NUL + key)>.json
```

Within a process mutex, `Reserve` checks or writes the idempotency binding and
creates version 1. `Save(ctx, record, expectedVersion)` reloads the current
record, compares its version, validates immutable identity/input fields,
increments the version, and writes canonical JSON through temp/fsync/rename/
directory-fsync. Blank idempotency keys always create the caller-provided new
run ID and no index file.

- [ ] **Step 7: Implement a behaviorally equivalent mock**

The mock uses the same `run.Validate` and transition rules, deep copies all
nested values, performs version checks, and supports configured method failures
for retry tests.

```go
func (s *Store) Save(
    _ context.Context,
    next run.Record,
    expected uint64,
) (run.Record, error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    current, ok := s.records[next.ID]
    if !ok {
        return run.Record{}, run.ErrNotFound
    }
    if current.Version != expected {
        return run.Record{}, run.ErrVersionConflict
    }
    saved, err := run.PrepareSave(current, next)
    if err != nil {
        return run.Record{}, err
    }
    s.records[next.ID] = run.CloneRecord(saved)
    return run.CloneRecord(saved), nil
}
```

- [ ] **Step 8: Run run-store tests and commit**

Run:

```bash
go fmt ./internal/run/...
go test -race ./internal/run/...
```

Expected: PASS.

```bash
git add internal/run
git commit -m "feat: persist durable workflow runs"
```

### Task 10: Provider-neutral Resolve and Execute phases

**Files:**
- Create: `internal/workflow/codechange/service.go`
- Create: `internal/workflow/codechange/resolve.go`
- Create: `internal/workflow/codechange/resolve_test.go`
- Create: `internal/workflow/codechange/execute.go`
- Create: `internal/workflow/codechange/execute_test.go`
- Create: `internal/workflow/codechange/prompt.go`
- Create: `internal/workflow/codechange/prompt_test.go`

**Interfaces:**
- Produces:
  `codechangeworkflow.New(Dependencies) (*Service, error)`,
  `(*Service).Resolve(context.Context, json.RawMessage) (PhaseResult, error)`,
  and `(*Service).Execute(context.Context, runID string)
  (PhaseResult, error)`.
- Produces
  `(*Service).Exhaust(context.Context, runID, stage string)
  (PhaseResult, error)` so an outer retry adapter can turn the last recorded
  retryable failure into a terminal failed run.
- `Dependencies` consumes the registry, run store, memory store, repository
  resolver/profiles, workspace manager, environment policy, agent runner,
  verification executor, Git capturer, change policy, and artifact store.
- `PhaseResult` contains only durable values: run ID, status, artifact
  reference, failure class, and retryability; it never contains a workspace
  path or secret.

Use one explicit dependency bundle:

```go
type Dependencies struct {
    Templates    *template.Registry
    Runs         run.Store
    Memory       memory.Store
    Resolver     repository.Resolver
    Workspaces   workspace.Manager
    Profiles     map[string]repository.Profile
    Environments environment.Builder
    Agent        runner.Runner
    Verifier     verification.Runner
    Capturer     gitcapture.Capturer
    Policy       policy.Evaluator
    Artifacts    artifact.Store
    Publisher    publisher.Publisher
    Clock        func() time.Time
    NewID        func() string
}

type PhaseResult struct {
    RunID       string              `json:"run_id"`
    Status      run.Status          `json:"status"`
    Artifact    *artifact.Reference `json:"artifact,omitempty"`
    FailureClass run.FailureClass   `json:"failure_class,omitempty"`
    Retryable   bool                `json:"retryable"`
}
```

`New` rejects every nil port, missing profile, clock, or ID generator. Maps are
copied on construction.

- [ ] **Step 1: Add small test ports for deterministic phase tests**

Promote these interfaces in their owning packages before writing service
fixtures:

```go
// internal/environment/policy.go
type Builder interface {
    Build(context.Context, Request) (Result, error)
    Cleanup(context.Context, string) error
}

// internal/verification/executor.go
type Runner interface {
    Run(context.Context, Command, map[string]string) Result
}

// internal/artifact/gitcapture/capture.go
type Capturer interface {
    Capture(context.Context, Request) (Result, error)
    Apply(context.Context, ApplyRequest) error
}

// internal/policy/change.go
type Evaluator interface {
    Evaluate(context.Context, gitcapture.Result) Decision
}
```

Add compile-time assertions for each concrete adapter.

- [ ] **Step 2: Write Resolve tests for canonical idempotency and frozen memory**

Given the same normalized input and idempotency key, call Resolve twice and
assert:

- the same run ID is returned;
- revision resolution occurs once;
- memory search occurs once;
- the persisted base SHA is immutable;
- the exact memory IDs/content are stored in `MemorySnapshot`;
- the canonical input and its hash are stored;
- changing the task with the same key returns `run.ErrIdempotencyConflict`;
- unknown profile/provider/template input fails before any external port call.

Use injected deterministic functions:

```go
Clock: func() time.Time { return time.Unix(100, 0).UTC() },
NewID: func() string { return "run-123" },
```

- [ ] **Step 3: Write Execute tests around a real Git worktree and mock agent**

The happy-path agent writes `changed.txt` in `req.WorkspacePath` and returns:

```go
runner.ExecutionResult{
    Output:     "updated changed.txt",
    Transcript: `{"type":"item.completed"}`,
    ExitCode:   0,
    Started:    true,
    Completed:  true,
}
```

Assert:

- the runner receives memory and preflight facts in its prompt;
- its environment contains `CODEX_HOME` but not Hatchet/Mem0/GitHub keys;
- required commands run in every selected module;
- one artifact is stored with the patch, terminal output, execution metadata,
  verification evidence, facts, warnings, and memory IDs but no memory content;
- the source repository remains unchanged;
- worktree and runtime directories are removed;
- a second Execute call returns the existing artifact without running the
  agent.

- [ ] **Step 4: Write Execute failure and cancellation tests**

Cover these exact outcomes:

- completed agent exit 7 -> `FailureAgent`, non-retryable, artifact captures
  safe diagnostics if policy allows;
- required check exit 1 -> `FailureVerification`, non-retryable, evidence
  artifact retained;
- missing required tool -> `FailureEnvironment`, retryable only when the cause
  code is explicitly transient;
- secret-policy denial -> `FailurePolicy`, non-retryable, no patch artifact
  saved, finding contains no secret value;
- caller cancellation -> terminal `canceled`, descendants/worktree/runtime
  removed with `context.WithoutCancel`;
- cleanup failure is joined to the primary failure and never replaces its
  class.

- [ ] **Step 5: Run tests and verify the service is missing**

Run:

```bash
go test ./internal/workflow/codechange -run 'Test(Resolve|Execute)' -v
```

Expected: FAIL because the phase service does not exist.

- [ ] **Step 6: Implement canonical Resolve**

Resolve performs these operations in order:

1. Resolve `codechange.ID` in the registry and strict-decode the raw input.
2. Canonically JSON-encode normalized input and hash it with SHA-256.
3. Reserve the injected run ID against template, key, and input hash.
4. Return immediately if the reservation already has a resolved SHA or is
   terminal.
5. Transition `pending -> resolving`.
6. Resolve base ref to immutable SHA.
7. Reject a local source checkout when `Revision.SourceDirty` is true; remote
   repositories have no source-dirty check.
8. Validate profile and worker capabilities without preparing a worktree.
9. Search memory using task as the default query and scoped tags.
10. Store canonical input, base SHA, and a deep memory snapshot.
11. Transition to `executing`.

Every CAS write reloads and retries at most three times on
`run.ErrVersionConflict`. A port failure is converted to a safe `run.Failure`,
persisted in its stage, and returned through `PhaseResult`. Retryable failures
leave the run in its active state; non-retryable failures transition it to
`failed`.

```go
func (s *Service) Resolve(ctx context.Context, raw json.RawMessage) (PhaseResult, error) {
    input, canonical, inputHash, err := s.decodeInput(raw)
    if err != nil {
        return PhaseResult{}, err
    }
    record, created, err := s.runs.Reserve(ctx, s.reservation(input, canonical, inputHash))
    if err != nil {
        return phaseResult(record), err
    }
    if !created && (record.BaseSHA != "" || record.Terminal()) {
        return phaseResult(record), nil
    }
    return s.resolveReserved(ctx, record, input)
}
```

- [ ] **Step 7: Build a deterministic agent prompt**

Use fixed headings and sorted facts:

```text
Task
<task description>

Repository
Base SHA: <sha>
Profile: <profile>

Constraints
- Work only inside the current workspace.
- Do not publish, push, merge, tag, or read external credentials.
- Leave all intended file changes in the workspace.

Preflight
- key: value

Relevant memory
- [memory-id] content
```

Escape no content into a shell. Cap the prompt at an operator limit before
agent execution; overflow is `FailureInput`.

- [ ] **Step 8: Implement Execute as a fresh-workspace transaction**

On each eligible attempt:

1. Load the run and strict-decode its persisted canonical input.
2. Return existing artifact or terminal status without side effects.
3. Prepare a detached worktree at `BaseSHA`.
4. Build agent and verification environments.
5. Inspect the selected profile and resolve dependencies.
6. Run the agent exactly once.
7. Run all verification commands and retain every result.
8. Capture the temporary-index change set.
9. Evaluate policy before persisting patch bytes.
10. Build and save the bundle.
11. Persist artifact and stage evidence.
12. Clean runtime and worktree with a 30-second non-canceled context.

If the agent result has `Completed=true`, any later failure records
`Retryable=false`. Required verification failures retain a safe artifact for
inspection but end the run as failed. Policy denials persist only findings and
metadata, not the denied patch.

`Exhaust` loads the most recent failure for the named stage, changes
`Retryable` to false with cause code `retries_exhausted`, and transitions the
run to `failed`. It is idempotent when the run is already terminal.

```go
func (s *Service) Execute(ctx context.Context, runID string) (result PhaseResult, err error) {
    record, input, err := s.loadExecutable(ctx, runID)
    if err != nil || record.Terminal() || record.Artifact != nil {
        return phaseResult(record), err
    }
    prepared, err := s.workspaces.Prepare(ctx, record.RepositoryURI, record.BaseSHA)
    if err != nil {
        return s.recordExecuteFailure(ctx, record, classifyWorkspace(err))
    }
    defer s.cleanupAttempt(ctx, runID, prepared, &result, &err)
    return s.executePrepared(ctx, record, input, prepared.Path())
}
```

- [ ] **Step 9: Run phase tests and commit**

Run:

```bash
go fmt ./internal/workflow/codechange ./internal/environment ./internal/verification ./internal/artifact/gitcapture ./internal/policy
go test -race ./internal/workflow/codechange ./internal/environment ./internal/verification ./internal/artifact/...
```

Expected: PASS.

```bash
git add internal/workflow/codechange internal/environment internal/verification internal/artifact/gitcapture internal/policy
git commit -m "feat: resolve and execute code changes"
```

### Task 11: Idempotent Git and GitHub pull-request publisher

**Files:**
- Create: `internal/publisher/gitpr/publisher.go`
- Create: `internal/publisher/gitpr/git.go`
- Create: `internal/publisher/gitpr/publisher_test.go`
- Create: `internal/publisher/github/client.go`
- Create: `internal/publisher/github/client_test.go`
- Create: `internal/publisher/github/credentials.go`
- Create: `internal/publisher/github/credentials_test.go`

**Interfaces:**
- Produces:
  `gitpr.New(Dependencies) (*Publisher, error)` implementing
  `publisher.Publisher`.
- Produces a small `gitpr.PullRequests` port with `Find` and `Create`.
- `gitpr.Dependencies` also requires
  `PushURL func(repository string) (string, error)`; local tests return the bare
  remote path, while GitHub composition returns a credential-free HTTPS URL.
- Produces:
  `github.NewClient(baseURL, token string, client *http.Client)
  (*Client, error)` implementing `gitpr.PullRequests`.
- Produces `github.NewCredentials(runtimeRoot, token string)` implementing
  `gitpr.Credentials`, which creates a temporary `GIT_ASKPASS` helper without
  placing a token in Git arguments.

```go
type PullRequestRequest struct {
    Repository string
    Head       string
    Base       string
    Title      string
    Body       string
    Draft      bool
}

type PullRequest struct {
    ID      string
    URL     string
    HeadSHA string
}

type PullRequests interface {
    Find(context.Context, PullRequestRequest) (*PullRequest, error)
    Create(context.Context, PullRequestRequest) (PullRequest, error)
}
```

- [ ] **Step 1: Write a local bare-remote publication integration test**

Use a real bare remote, filesystem artifact store, real worktree manager, and
fake pull-request client. Publish one patch and assert:

- branch `paje/code-change/run-123` exists remotely;
- its commit parent is the approved base SHA;
- the committed tree equals the captured artifact tree;
- commit trailers contain exact run ID, base SHA, and artifact digest;
- required verification was rerun after patch application;
- the fake provider received target `main`, branch, title, body, and draft;
- a second identical call returns the same commit and PR with no extra commit
  and no second create call.

Change the artifact digest or remote branch commit and assert
`publisher.ErrConflict`.

- [ ] **Step 2: Write GitHub HTTP contract tests**

Using `httptest.Server`, assert exact requests:

```text
GET  /repos/araihu/paje/pulls?state=all&head=araihu:paje%2Fcode-change%2Frun-123&base=main
POST /repos/araihu/paje/pulls
Authorization: Bearer <redacted-in-test-log>
Accept: application/vnd.github+json
X-GitHub-Api-Version: 2022-11-28
```

The POST body contains `head`, `base`, `title`, `body`, and `draft`. Map 401/403
to unavailable provider, 409/422 to conflict unless an exact existing PR is
found, 429/5xx to retryable errors, and bound response diagnostics to 4096
bytes.

- [ ] **Step 3: Write askpass secrecy tests**

Create credentials, assert the script is mode `0700`, token appears only in the
returned environment value, and neither script content nor Git arguments
contain it. Cleanup must remove the helper even under a canceled caller
context.

- [ ] **Step 4: Run publisher tests and verify missing-package failures**

Run:

```bash
go test ./internal/publisher/gitpr ./internal/publisher/github -v
```

Expected: FAIL because the adapters are missing.

- [ ] **Step 5: Implement deterministic apply, verify, commit, and push**

The publisher:

1. Validates the request and deterministic branch.
2. Loads and verifies the artifact reference.
3. Prepares a fresh worktree at exact base SHA.
4. Applies the binary patch and checks the expected tree.
5. Reconstructs required commands from artifact verification evidence and
   reruns them in the publisher environment.
6. Checks the remote branch before creating a commit.
7. Creates a commit only when no matching branch exists.
8. Pushes only `HEAD:refs/heads/<deterministic-branch>`.
9. Finds or creates the pull request.
10. Cleans all temporary credentials and workspaces.

Commit without global Git configuration:

```bash
git -c user.name=Pajé -c user.email=paje@invalid commit -m <message>
```

Use this message:

```text
<title>

Paje-Run-ID: <run-id>
Paje-Base-SHA: <base-sha>
Paje-Artifact-Digest: <digest>
```

Inspect existing commits with `git show -s --format=%B`; all three trailers,
parent SHA, and tree SHA must match before reuse.

- [ ] **Step 6: Push through a secret-free argument vector**

The askpass script prints `$PAJE_GIT_USERNAME` for username prompts and
`$PAJE_GIT_PASSWORD` otherwise. Run Git with:

```text
GIT_ASKPASS=<helper>
GIT_TERMINAL_PROMPT=0
PAJE_GIT_USERNAME=x-access-token
PAJE_GIT_PASSWORD=<token>
```

Do not include credentials in repository URLs, command arguments, run records,
artifacts, or diagnostics. Redact the token if Git echoes it unexpectedly.

- [ ] **Step 7: Implement GitHub find/create**

Normalize supported HTTPS and SSH GitHub repository URIs into owner/repository.
URL-escape path and query values independently. Return an exact existing PR
only when owner, head branch, base branch, and provider repository match.
Validate response ID, URL host, head SHA, and state before returning.
`github.PushURL` converts either accepted GitHub URI form to
`https://github.com/<owner>/<repository>.git` before using token-based
`GIT_ASKPASS`; never attempt an SSH push with the HTTP token.

```go
func (c *Client) Create(
    ctx context.Context,
    req gitpr.PullRequestRequest,
) (gitpr.PullRequest, error) {
    owner, repository, err := parseRepository(req.Repository)
    if err != nil {
        return gitpr.PullRequest{}, err
    }
    endpoint := fmt.Sprintf(
        "/repos/%s/%s/pulls",
        url.PathEscape(owner),
        url.PathEscape(repository),
    )
    return c.postPullRequest(ctx, endpoint, req)
}
```

- [ ] **Step 8: Run publisher tests and commit**

Run:

```bash
go fmt ./internal/publisher/gitpr ./internal/publisher/github
go test -race ./internal/publisher/...
```

Expected: PASS.

```bash
git add internal/publisher
git commit -m "feat: publish idempotent GitHub pull requests"
```

### Task 12: Approval, Publish, Finalize, and provider-neutral end-to-end flow

**Files:**
- Create: `internal/template/codechange/result.go`
- Create: `internal/workflow/codechange/approval.go`
- Create: `internal/workflow/codechange/publish.go`
- Create: `internal/workflow/codechange/finalize.go`
- Create: `internal/workflow/codechange/service_test.go`

**Interfaces:**
- Extends `codechangeworkflow.Service` with:
  `Approval(context.Context, runID string, gate approval.Gate)`,
  `Publish(context.Context, runID string)`, and
  `Finalize(context.Context, runID string) (codechange.Result, error)`.
- Produces the durable `codechange.Result` from the approved design.
- Consumes only provider-neutral approval, publisher, run, artifact, and memory
  ports.

Define the returned value exactly:

```go
type Result struct {
    RunID        string                `json:"run_id"`
    Status       run.Status            `json:"status"`
    BaseSHA      string                `json:"base_sha"`
    Artifact     artifact.Reference    `json:"artifact"`
    Verification []verification.Result `json:"verification,omitempty"`
    Approval     *approval.Result      `json:"approval,omitempty"`
    Publication  *publisher.Result     `json:"publication,omitempty"`
    Failure      *run.Failure          `json:"failure,omitempty"`
}
```

- [ ] **Step 1: Write artifact-only end-to-end service test**

Drive Resolve -> Execute -> Approval -> Publish -> Finalize using mocks plus a
real Git fixture. Assert:

- Approval and Publish return a skipped stage for artifact mode;
- approval gate and publisher call counts remain zero;
- outcome memory is saved once with run ID, template ID, base SHA, artifact
  digest, stage statuses, and no secret values;
- final status is `succeeded`;
- result reloads after reconstructing the run and artifact stores;
- only run, artifact, and memory stores changed externally.

- [ ] **Step 2: Write pull-request approval and decline tests**

For approval, assert the request includes run/template IDs, repository, base,
target, deterministic branch, agent summary, changed paths, digest, checks, and
warnings. Configure a matching decision and assert exactly one publisher call.

Then table-test:

- mismatched decision run ID;
- mismatched artifact digest;
- changed target branch after decision;
- explicit decline with actor/reason;
- gate error;
- publisher conflict;
- retryable publisher outage followed by success.

Decline must end in `declined`, return no workflow error, and never call the
publisher.

- [ ] **Step 3: Write finalization memory-failure tests**

If outcome memory save fails transiently, return a retryable finalize error and
leave the run non-terminal. If retries are exhausted by the Hatchet adapter,
record a bounded `outcome_memory_failed` stage diagnostic and terminal
`failed` status without pretending the memory write succeeded. Repeating
Finalize after a successful memory write must not save a duplicate outcome.
Simulate a process restart after the memory write but before the run-record
flag update; Finalize must recover by finding the exact run-scoped memory.

- [ ] **Step 4: Run tests and verify missing methods**

Run:

```bash
go test ./internal/workflow/codechange -run 'TestService' -v
```

Expected: FAIL because Approval, Publish, Finalize, and result mapping do not
exist.

- [ ] **Step 5: Implement artifact-bound Approval**

Artifact mode upserts a skipped approval stage and returns. Pull-request mode:

1. Load and verify the artifact.
2. Transition `executing -> awaiting_approval`.
3. Build and validate the immutable approval request.
4. Call the supplied gate.
5. Validate decision binding.
6. Persist approval.
7. Transition decline to terminal `declined`; keep approval success eligible
   for publishing.

A binding mismatch is `FailureApproval`, non-retryable. A provider transport
failure is retryable only when its typed cause says so.

```go
func (s *Service) Approval(
    ctx context.Context,
    runID string,
    gate approval.Gate,
) (PhaseResult, error) {
    record, input, bundle, err := s.loadApprovalState(ctx, runID)
    if err != nil {
        return phaseResult(record), err
    }
    if input.Publication.Mode == "artifact" {
        return s.skipStage(ctx, record, "approval")
    }
    return s.requestAndPersistApproval(ctx, record, input, bundle, gate)
}
```

- [ ] **Step 6: Implement idempotent Publish**

Artifact mode upserts a skipped publication stage and returns. Pull-request
mode requires a matching approval, transitions to `publishing`, constructs
`publisher.Request`, and calls the injected publisher. If publication already
exists, validate all immutable fields and return it without another call.
Persist provider result through CAS before returning.

```go
func (s *Service) Publish(ctx context.Context, runID string) (PhaseResult, error) {
    record, input, err := s.loadPublishable(ctx, runID)
    if err != nil {
        return phaseResult(record), err
    }
    if input.Publication.Mode == "artifact" {
        return s.skipStage(ctx, record, "publish")
    }
    if err := validateApprovalBinding(record, input); err != nil {
        return s.failPublish(ctx, record, err)
    }
    return s.publishApproved(ctx, record, input)
}
```

- [ ] **Step 7: Implement Finalize and result mapping**

Build outcome memory from safe fields only:

```text
Pajé run <run-id> completed
Template: code-change@v1
Base SHA: <sha>
Artifact: sha256:<digest>
Stages: resolve=..., execute=..., approval=..., publish=..., finalize=...
Failure: <class/cause-or-none>
Publication: <URL-or-none>
```

Preserve scoped `user_id` and `app_id`, add entity tag `run_id`, and add
metadata tags `paje_run_id`, `paje_template`, `paje_base_sha`,
`paje_artifact_digest`, and `paje_status`. After memory save or an explicitly
recorded exhausted failure, transition successful artifact runs from
`executing` and successful PR runs from `publishing` to `succeeded`. Return a
deep-copy `codechange.Result`.

Before `Save`, search with the exact `user_id`, `app_id`, and `run_id` tags and
query `Pajé run <run-id> completed`. If an existing memory equals the canonical
outcome content, set `OutcomeMemorySaved=true` without another write. A
process-local keyed mutex prevents concurrent Finalize calls for the same run;
the scoped search closes the restart window after Mem0's successful event has
become visible.

```go
func (s *Service) Finalize(
    ctx context.Context,
    runID string,
) (codechange.Result, error) {
    unlock := s.finalizeLocks.Lock(runID)
    defer unlock()
    record, err := s.runs.Load(ctx, runID)
    if err != nil {
        return codechange.Result{}, err
    }
    record, err = s.persistOutcomeOnce(ctx, record)
    if err != nil {
        return resultFromRecord(record), err
    }
    record, err = s.finishTerminal(ctx, record)
    return resultFromRecord(record), err
}
```

- [ ] **Step 8: Run service tests and commit**

Run:

```bash
go fmt ./internal/template/codechange ./internal/workflow/codechange
go test -race ./internal/workflow/codechange ./internal/template/codechange
```

Expected: PASS.

```bash
git add internal/template/codechange internal/workflow/codechange
git commit -m "feat: complete code change application flow"
```

### Task 13: Five-phase Hatchet DAG and durable approval event

**Files:**
- Create: `internal/workflow/codechangehatchet/workflow.go`
- Create: `internal/workflow/codechangehatchet/handlers.go`
- Create: `internal/workflow/codechangehatchet/approval.go`
- Create: `internal/workflow/codechangehatchet/workflow_test.go`
- Create: `internal/workflow/codechangehatchet/approval_test.go`

**Interfaces:**
- Produces:
  `codechangehatchet.New(client *hatchet.Client, service *codechange.Service)
  (*hatchet.Workflow, error)`.
- Produces a durable approval gate that waits for event key
  `paje:approval:<run-id>` and decodes `approval.Result`.
- Registers the workflow name exactly `paje-code-change-v1` with visible tasks
  `resolve`, `execute`, `approval`, `publish`, and `finalize`.

- [ ] **Step 1: Write handler tests without Hatchet Server**

Expose unexported handler constructors and call them with fake contexts that
implement only the needed `ParentOutput` behavior. Assert:

- resolve marshals the complete raw field map and preserves unknown fields for
  strict service rejection;
- each downstream handler passes only the parent run ID;
- non-retryable terminal phase failures return a successful Hatchet handler
  result so later no-op/finalize tasks can run;
- retryable failures return an error for Hatchet retry;
- the last retry calls `Service.Exhaust` and returns a successful handler
  result so Finalize can return the durable failed run;
- Finalize returns the durable `codechange.Result`.

- [ ] **Step 2: Write durable approval event tests**

Use a fake durable waiter returning this payload:

```json
{
  "run_id": "run-123",
  "artifact_digest": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "approved": true,
  "actor": "reviewer@example.test",
  "decided_at": "2026-07-24T12:00:00Z"
}
```

Assert the gate waits on `paje:approval:run-123`, uses an empty filter
expression, decodes the payload, and validates request binding. Test malformed,
declined, mismatched, and canceled waits.

- [ ] **Step 3: Run tests and verify the adapter is absent**

Run:

```bash
go test ./internal/workflow/codechangehatchet -v
```

Expected: FAIL because the package does not exist.

- [ ] **Step 4: Implement the DAG declaration through a testable builder**

Wrap `*hatchet.Workflow` behind an internal declaration interface so tests can
record task names, parents, handlers, and options without a server. Production
constructs:

```go
workflow := client.NewWorkflow("paje-code-change-v1")
resolve := workflow.NewTask("resolve", resolveHandler(service),
    hatchet.WithRetries(2),
    hatchet.WithRetryBackoff(2, 60),
    hatchet.WithExecutionTimeout(2*time.Minute),
)
execute := workflow.NewTask("execute", executeHandler(service),
    hatchet.WithParents(resolve),
    hatchet.WithRetries(2),
    hatchet.WithRetryBackoff(2, 60),
    hatchet.WithExecutionTimeout(30*time.Minute),
)
approval := workflow.NewDurableTask("approval", approvalHandler(service),
    hatchet.WithParents(execute),
)
publish := workflow.NewTask("publish", publishHandler(service),
    hatchet.WithParents(approval),
    hatchet.WithRetries(2),
    hatchet.WithRetryBackoff(2, 60),
    hatchet.WithExecutionTimeout(15*time.Minute),
)
_ = workflow.NewTask("finalize", finalizeHandler(service),
    hatchet.WithParents(publish),
    hatchet.WithRetries(2),
    hatchet.WithRetryBackoff(2, 60),
    hatchet.WithExecutionTimeout(2*time.Minute),
)
```

Artifact-only Approval and Publish handlers call the service and persist
skipped stage results; they do not call external approval/publisher ports.

Use constants `resolveRetries = 2`, `executeRetries = 2`,
`publishRetries = 2`, and `finalizeRetries = 2` in both options and handlers.
After a retryable phase result:

```go
if ctx.RetryCount() < maxRetries {
    return PhaseResult{}, phaseError
}
return service.Exhaust(ctx, result.RunID, stageName)
```

This prevents a Hatchet-exhausted phase from leaving the durable run in an
active state. A non-retryable result is returned without a Hatchet error.

- [ ] **Step 5: Add per-repository publication concurrency**

Configure publish concurrency with max runs `1` and round-robin strategy:

```go
maxRuns := int32(1)
strategy := types.GroupRoundRobin
hatchet.WithConcurrency(&types.Concurrency{
    Expression: "input.repository_uri + ':' + input.publication.target_branch",
    MaxRuns: &maxRuns,
    LimitStrategy: &strategy,
})
```

The workflow test dumps the declaration and asserts this expression, max,
`GROUP_ROUND_ROBIN` strategy, parent chain, retries, and execution timeouts.

- [ ] **Step 6: Implement durable event decoding**

The gate holds `hatchet.DurableContext`:

```go
event, err := g.ctx.WaitForEvent("paje:approval:"+req.RunID, "")
if err != nil {
    return approval.Result{}, fmt.Errorf("wait for approval event: %w", err)
}
var result approval.Result
if err := hatchet.EventInto(event, &result); err != nil {
    return approval.Result{}, fmt.Errorf("decode approval event: %w", err)
}
if err := approval.ValidateDecision(req, result); err != nil {
    return approval.Result{}, err
}
return result, nil
```

- [ ] **Step 7: Run declaration tests and commit**

Run:

```bash
go fmt ./internal/workflow/codechangehatchet
go test -race ./internal/workflow/codechangehatchet
```

Expected: PASS without a Hatchet server.

```bash
git add internal/workflow/codechangehatchet
git commit -m "feat: bind code changes to Hatchet"
```

### Task 14: Runtime composition, persistent Helm deployment, and Codex image

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `cmd/paje/main.go`
- Modify: `cmd/paje/main_test.go`
- Modify: `charts/paje/values.yaml`
- Create: `charts/paje/values.schema.json`
- Modify: `charts/paje/templates/configmap.yaml`
- Modify: `charts/paje/templates/deployment.yaml`
- Modify: `charts/paje/templates/secret.yaml`
- Create: `charts/paje/templates/pvc.yaml`
- Modify: `charts/paje/templates/NOTES.txt`
- Modify: `Dockerfile`

**Interfaces:**
- Extends `config.Config` with beta roots, limits, allowlists, publisher,
  GitHub, and Codex-auth settings.
- Composes filesystem/mock adapters, registry, profiles, phase service, legacy
  standalone task, and beta Hatchet DAG.
- Produces a chart that supports generated or existing separated secrets,
  optional PVC persistence, and writable seeded `CODEX_HOME`.

- [ ] **Step 1: Write configuration tests for exact defaults and secret requirements**

With only `HATCHET_CLIENT_TOKEN`, assert:

```text
PAJE_RUN_ROOT=<workspace-root>/runs
PAJE_ARTIFACT_ROOT=<workspace-root>/artifacts
PAJE_RUNTIME_ROOT=<workspace-root>/runtime
PAJE_ARTIFACT_LIMIT_BYTES=10485760
PAJE_COMMAND_OUTPUT_LIMIT_BYTES=1048576
PAJE_PUBLISHER_ADAPTER=mock
PAJE_ENV_ALLOWLIST=[]
```

Test JSON string-array parsing for `PAJE_ENV_ALLOWLIST`, positive byte/time
limits, and these validation rules:

- `publisher=github` requires `GITHUB_TOKEN`;
- `runner=codex` requires a non-empty `CODEX_HOME`;
- run/artifact/runtime roots must be absolute and distinct;
- unsupported adapters and non-positive limits fail;
- secret values never appear in formatted errors.

- [ ] **Step 2: Write composition tests with mocks**

Refactor `buildDependencies` into focused builders and assert a fully mocked
configuration constructs:

- template registry containing `code-change@v1`;
- legacy orchestrator;
- beta phase service;
- legacy standalone Hatchet task;
- beta five-phase workflow.

For filesystem configuration, reconstruct builders using the same roots and
assert an existing run/artifact can be loaded.

- [ ] **Step 3: Write Helm render assertions before changing templates**

Render these cases to temporary files:

```bash
helm template paje charts/paje \
  --set secrets.hatchet.value=test \
  --set adapters.runner=mock

helm template paje charts/paje \
  --set persistence.enabled=true \
  --set persistence.size=20Gi \
  --set adapters.memory=mem0 \
  --set adapters.workspace=git \
  --set adapters.runner=codex \
  --set publisher.adapter=github \
  --set secrets.hatchet.existingSecret=paje-hatchet \
  --set secrets.mem0.existingSecret=paje-mem0 \
  --set secrets.github.existingSecret=paje-github \
  --set codexAuth.existingSecret=paje-codex-auth
```

Assert the second render has one PVC, persistent data mount, read-only Codex
seed mount, writable codex-home `emptyDir`, init container, distinct GitHub/
Mem0/Hatchet secret keys, beta limits, and `replicas: 1`. Assert no secret value
is rendered into ConfigMap or pod arguments.

- [ ] **Step 4: Run focused tests and capture failures**

Run:

```bash
go test ./internal/config ./cmd/paje -v
helm lint charts/paje --set secrets.hatchet.value=test
```

Expected: FAIL because beta configuration and chart fields are missing.

- [ ] **Step 5: Implement configuration and dependency composition**

Add config fields for:

```go
RunRoot, ArtifactRoot, RuntimeRoot string
ArtifactLimitBytes, CommandOutputLimitBytes int64
EnvironmentAllowlist []string
PublisherAdapter, GitHubToken, GitHubAPIURL string
CodexHome string
```

Build, in dependency order:

1. memory, workspace/resolver, and Codex/local runner;
2. run and artifact stores;
3. environment policy and verification executor;
4. generic and Go profiles;
5. template registry;
6. mock or GitHub Git-PR publisher;
7. code-change service;
8. legacy standalone task and beta workflow.

Use `uuid.NewString` from the already pinned
`github.com/google/uuid@v1.6.0` as the production run-ID generator; `go mod
tidy` promotes it from indirect to direct.

Register both in one worker:

```go
worker, err := client.NewWorker(
    workerName,
    hatchet.WithWorkflows(legacyTask, betaWorkflow),
)
```

Keep constructor functions independently unit-testable without creating a live
Hatchet client.

- [ ] **Step 6: Add chart schema, persistence, and separated credentials**

`values.schema.json` enforces `replicaCount: 1`, positive limits, valid adapter
enums, and conditional secret requirements. When persistence is enabled, mount
one PVC at `/var/lib/paje` and configure:

```text
PAJE_WORKSPACE_ROOT=/var/lib/paje/workspace
PAJE_RUN_ROOT=/var/lib/paje/runs
PAJE_ARTIFACT_ROOT=/var/lib/paje/artifacts
PAJE_RUNTIME_ROOT=/run/paje
```

Runtime stays an `emptyDir`. These values fields reference separate Secret
objects:
`secrets.hatchet.existingSecret`, `secrets.mem0.existingSecret`, and
`secrets.github.existingSecret`. Their default data keys are
`hatchet-client-token`,
`mem0-api-key`, and `github-token`. Generated values render as three separate
Secret resources and only for credentials required by selected adapters.

- [ ] **Step 7: Seed a private writable Codex home**

When `adapters.runner=codex`, require `codexAuth.existingSecret`. Mount it
read-only at `/codex-auth-seed`, mount an `emptyDir` at `/codex-home`, and add
an init container using the worker image:

```yaml
command:
  - /bin/sh
  - -ec
  - |
    cp -R /codex-auth-seed/. /codex-home/
    chmod -R go-rwx /codex-home
```

Set `CODEX_HOME=/codex-home` only on the worker. The environment policy passes
it to Codex but withholds all worker service credentials. Apply the same
non-root uid/gid `65532`, dropped capabilities, and no-privilege-escalation
security context to the init container.

- [ ] **Step 8: Build a pinned Codex-equipped non-root image**

Keep `golang:1.26.1-alpine` as the Go build stage and use
`node:24.4.1-alpine3.22` as the runtime stage. Install exactly:

```dockerfile
FROM node:24.4.1-alpine3.22

ARG CODEX_VERSION=0.144.5
RUN npm install --global "@openai/codex=${CODEX_VERSION}" \
    && npm cache clean --force \
    && codex --version
```

Install `ca-certificates`, `git`, and `openssh-client`, retain uid/gid `65532`,
read-only-root compatibility, `/tmp` and `/run/paje` writable mounts, and no
baked credentials. Add OCI labels recording Pajé commit and Codex version.

- [ ] **Step 9: Run config, chart, and image verification**

Run:

```bash
go fmt ./internal/config ./cmd/paje
go mod tidy
go test -race ./internal/config ./cmd/paje
helm lint charts/paje --set secrets.hatchet.value=test
helm template paje charts/paje --set secrets.hatchet.value=test >/tmp/paje-default.yaml
helm template paje charts/paje \
  --set persistence.enabled=true \
  --set adapters.memory=mem0 \
  --set adapters.workspace=git \
  --set adapters.runner=codex \
  --set publisher.adapter=github \
  --set secrets.hatchet.existingSecret=paje-hatchet \
  --set secrets.mem0.existingSecret=paje-mem0 \
  --set secrets.github.existingSecret=paje-github \
  --set codexAuth.existingSecret=paje-codex-auth >/tmp/paje-beta.yaml
docker build --no-cache --build-arg CODEX_VERSION=0.144.5 -t paje:beta .
docker run --rm --entrypoint /bin/sh paje:beta -c 'id -u; codex --version'
```

Expected: tests/lint/render/build pass; container prints uid `65532` and
`codex-cli 0.144.5`.

- [ ] **Step 10: Commit runtime packaging**

```bash
git add internal/config cmd/paje charts/paje Dockerfile go.mod go.sum
git commit -m "feat: package the beta worker"
```

### Task 15: Beta acceptance suite, operator documentation, and final audit

**Files:**
- Create: `internal/acceptance/codex_test.go`
- Create: `internal/acceptance/github_test.go`
- Create: `internal/acceptance/helpers_test.go`
- Modify: `README.md`
- Modify: `charts/paje/templates/NOTES.txt`

**Interfaces:**
- Produces opt-in acceptance tests gated by
  `PAJE_CODEX_INTEGRATION=1` and `PAJE_GITHUB_ACCEPTANCE=1`.
- Documents the exact Hatchet workflow input, approval event, artifact layout,
  deployment configuration, test commands, security boundary, and beta
  exclusions.

- [ ] **Step 1: Write the real Codex artifact acceptance test**

Gate on `PAJE_CODEX_INTEGRATION=1`. Create a repository with two Go modules and
a test that initially passes. Seed scoped memory instructing a deterministic
small change. Run all five provider-neutral phases in artifact mode with real
Codex, Git worktrees, Go profile, filesystem run/artifact stores, and mock
approval/publisher.

Assert:

- Codex reads repository and memory and modifies the requested file;
- `go test ./...` passes in both modules with `GOWORK=off`;
- source checkout is unchanged;
- no worktree, runtime directory, or descendant process remains;
- artifact reloads after store reconstruction;
- applying it to a fresh worktree produces the exact manifest tree;
- approval and publisher call counts are zero;
- agent environment evidence excludes Hatchet/Mem0/GitHub keys.

- [ ] **Step 2: Write the opt-in GitHub idempotency acceptance test**

Require:

```text
PAJE_GITHUB_ACCEPTANCE=1
PAJE_GITHUB_TOKEN
PAJE_GITHUB_TEST_REPOSITORY
PAJE_GITHUB_TEST_BASE_REF
PAJE_GITHUB_TEST_RUN_ID
```

Use the supplied stable run ID to publish a draft PR in the dedicated test
repository. Run Publish twice and query GitHub after each. Assert exactly one
branch, one commit with matching trailers, and one PR exist and the second
result reuses all IDs. Never merge, close, delete, or mutate the target branch.

- [ ] **Step 3: Run unit tests first and confirm opt-in tests skip cleanly**

Run:

```bash
go test ./internal/acceptance -v
```

Expected: PASS with explicit skip messages when opt-in variables are absent.

- [ ] **Step 4: Document the beta operator contract**

README must include:

- `code-change@v1` JSON input with artifact and pull-request examples;
- the five phase names and durable run/artifact locations;
- event key `paje:approval:<run-id>` and exact `approval.Result` JSON;
- all environment variables and Helm values;
- how to seed the Codex auth Secret without placing GitHub/Mem0/Hatchet
  credentials in it;
- how to inspect and reapply an artifact;
- idempotency-key behavior and conflict semantics;
- required opt-in acceptance variables;
- one-replica and filesystem-store beta limitation;
- explicit non-goals: YAML DSL, auto-merge, tags/releases, GitOps/monitoring,
  Forgejo, Kubernetes Jobs, and multiple replicas.

- [ ] **Step 5: Run the complete local quality gate**

Run:

```bash
go fmt ./...
go vet ./...
go test -race ./...
go build ./cmd/paje
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/paje-linux-amd64 ./cmd/paje
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/paje-linux-arm64 ./cmd/paje
helm lint charts/paje --set secrets.hatchet.value=test
helm template paje charts/paje --set secrets.hatchet.value=test >/tmp/paje-default.yaml
helm template paje charts/paje \
  --set persistence.enabled=true \
  --set adapters.memory=mem0 \
  --set adapters.workspace=git \
  --set adapters.runner=codex \
  --set publisher.adapter=github \
  --set secrets.hatchet.existingSecret=paje-hatchet \
  --set secrets.mem0.existingSecret=paje-mem0 \
  --set secrets.github.existingSecret=paje-github \
  --set codexAuth.existingSecret=paje-codex-auth >/tmp/paje-beta.yaml
docker build --no-cache --build-arg CODEX_VERSION=0.144.5 -t paje:beta .
docker run --rm --entrypoint /bin/sh paje:beta -c 'test "$(id -u)" = 65532; test "$(codex --version)" = "codex-cli 0.144.5"'
```

Expected: every command exits zero.

- [ ] **Step 6: Run live acceptance gates**

Run authenticated Codex:

```bash
PAJE_CODEX_INTEGRATION=1 \
go test ./internal/acceptance -run TestCodexArtifactAcceptance -v -count=1
```

Run against the dedicated GitHub test repository only after all five
`PAJE_GITHUB_*` variables are set:

```bash
PAJE_GITHUB_ACCEPTANCE=1 \
go test ./internal/acceptance -run TestGitHubPublicationAcceptance -v -count=1
```

Expected: Codex creates and reproduces a verified artifact; GitHub reruns reuse
the same branch, commit, and draft pull request.

With `PAJE_KUBE_ACCEPTANCE=1` and an explicitly selected non-production
Kubernetes context, render existing-secret references and run:

```bash
kubectl apply --dry-run=server -f /tmp/paje-beta.yaml
```

Expected: the API server accepts every rendered resource without persisting
one. Record the exact context and command output in the completion audit; do
not run this command against an unverified current context.

- [ ] **Step 7: Perform the completion audit**

For each of the ten acceptance criteria in
`docs/superpowers/specs/2026-07-24-beta-code-change-workflow-design.md`, record
the exact test name or command output that proves it. Recheck:

```bash
git status --short --branch
git log --oneline --decorate -15
git diff --check
```

Expected: no uncommitted implementation files, no unchecked plan item, and
evidence for all ten criteria. Do not claim beta if the live Codex or opt-in
GitHub gate was skipped.

- [ ] **Step 8: Commit documentation and acceptance evidence**

```bash
git add README.md charts/paje/templates/NOTES.txt internal/acceptance
git commit -m "test: prove code change beta workflow"
```
