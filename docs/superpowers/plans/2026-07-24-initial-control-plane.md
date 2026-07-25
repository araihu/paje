# Pajé Initial Control Plane Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and verify the first deployable Pajé Hatchet worker with strict
ports, mocks, Mem0/Git/local adapters, a deterministic agent workflow, and
Kubernetes packaging.

**Architecture:** Core contracts remain provider-neutral. A service-free
`workflow.Orchestrator` composes the ports, while a small Hatchet adapter exposes
that service as `paje-agent-run`; `cmd/paje` selects mocks by default and real
adapters through environment configuration.

**Tech Stack:** Go 1.26, Hatchet Go SDK v0.97.0, Mem0 Platform HTTP API v3,
`os/exec`, Git worktrees, Docker, Helm 3.

## Global Constraints

- Module path is exactly `github.com/araihu/paje`.
- Preserve the four port contracts in the consolidated specification exactly.
- Keep Hatchet, HTTP, Git, and process details outside the application core.
- Default daemon wiring uses mocks; real adapters are explicitly selectable.
- Tests must not require Hatchet Server, PostgreSQL, Mem0, or Kubernetes.
- Every implementation cycle starts with a focused failing test.

---

## File Map

- `internal/{memory,workspace,runner,approval}/*.go`: provider-neutral ports.
- `internal/*/mock/*.go`: concurrency-safe in-memory test adapters.
- `internal/memory/mem0/*.go`: Mem0 Platform v3 HTTP adapter.
- `internal/workspace/gitworktree/*.go`: cached mirror and ephemeral worktree adapter.
- `internal/runner/local/*.go`: black-box local process adapter.
- `internal/workflow/orchestrator.go`: deterministic application pipeline.
- `internal/workflow/hatchet.go`: Hatchet task binding only.
- `internal/config/config.go`: environment parsing and validation.
- `cmd/paje/main.go`: dependency composition and worker lifecycle.
- `charts/paje/*`: worker Helm chart.
- `Dockerfile`: multi-stage non-root worker image.

### Task 1: Module, ports, and in-memory mocks

**Files:**
- Create: `go.mod`
- Create: `internal/memory/memory.go`
- Create: `internal/memory/mock/store.go`
- Create: `internal/memory/mock/store_test.go`
- Create: `internal/workspace/workspace.go`
- Create: `internal/workspace/mock/manager.go`
- Create: `internal/workspace/mock/manager_test.go`
- Create: `internal/runner/runner.go`
- Create: `internal/runner/mock/runner.go`
- Create: `internal/runner/mock/runner_test.go`
- Create: `internal/approval/approval.go`
- Create: `internal/approval/mock/gate.go`
- Create: `internal/approval/mock/gate_test.go`

**Interfaces:**
- Produces the exact `memory.Store`, `workspace.Manager`,
  `workspace.Workspace`, `runner.Runner`, and `approval.Gate` contracts from the
  project specification.
- Produces `mock.NewStore`, `mock.NewManager`, `mock.NewRunner`, and
  `mock.NewGate` with immutable snapshots of recorded calls.

- [ ] **Step 1: Initialize the Go module**

Run:

```bash
go mod init github.com/araihu/paje
```

Expected: `go.mod` declares `module github.com/araihu/paje`.

- [ ] **Step 2: Write failing mock behavior tests**

Each mock test must prove one stateful behavior and one defensive-copy behavior.
For example:

```go
func TestStoreSaveAndSearch(t *testing.T) {
    store := mock.NewStore(nil)
    tags := map[string]string{"app_id": "paje"}
    require.NoError(t, store.Save(context.Background(), "agent finished", tags))
    tags["app_id"] = "changed"

    got, err := store.Search(context.Background(), "finished", 10, map[string]string{"app_id": "paje"})
    require.NoError(t, err)
    require.Len(t, got, 1)
    assert.Equal(t, "paje", got[0].Metadata["app_id"])
}
```

Equivalent tests record workspace cleanup, runner requests, and approval
requests.

- [ ] **Step 3: Verify the tests fail for missing packages**

Run:

```bash
go test ./internal/memory/mock ./internal/workspace/mock ./internal/runner/mock ./internal/approval/mock
```

Expected: FAIL because the port and mock packages do not exist.

- [ ] **Step 4: Add the exact ports and minimal mock implementations**

Mocks use a mutex, copy maps and slices on ingress/egress, and expose snapshots:

```go
type Runner struct {
    mu       sync.Mutex
    result   runner.ExecutionResult
    err      error
    requests []runner.RunRequest
}

func (r *Runner) Run(_ context.Context, req runner.RunRequest) (runner.ExecutionResult, error) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.requests = append(r.requests, cloneRequest(req))
    return r.result, r.err
}
```

`memory/mock.Store` performs case-insensitive content matching and exact tag
matching. `workspace/mock.Manager` returns unique logical paths and idempotent
workspaces. Each package contains a compile-time interface assertion.

- [ ] **Step 5: Format and run the package tests**

Run:

```bash
gofmt -w internal
go test ./internal/memory/mock ./internal/workspace/mock ./internal/runner/mock ./internal/approval/mock
```

Expected: PASS.

### Task 2: Local process runner

**Files:**
- Create: `internal/runner/local/runner.go`
- Create: `internal/runner/local/runner_test.go`

**Interfaces:**
- Consumes: `runner.RunRequest`, `runner.ExecutionResult`.
- Produces: `local.New(command string, args ...string) (*Runner, error)` and
  `(*Runner).Run(context.Context, runner.RunRequest)`.

- [ ] **Step 1: Write tests for output, working directory, env, exit code, and cancellation**

Use the test binary helper-process pattern:

```go
func TestRunCapturesNonZeroExit(t *testing.T) {
    r, err := local.New(os.Args[0], "-test.run=TestHelperProcess", "--")
    require.NoError(t, err)
    result, err := r.Run(context.Background(), runner.RunRequest{
        TaskDescription: "exit-7",
        WorkspacePath: t.TempDir(),
        Env: map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
    })
    require.NoError(t, err)
    assert.Equal(t, 7, result.ExitCode)
    assert.Contains(t, result.Output, "exit-7")
    assert.GreaterOrEqual(t, result.Duration, 0.0)
}
```

- [ ] **Step 2: Verify focused tests fail**

Run: `go test ./internal/runner/local -run TestRun -v`

Expected: FAIL because `local.New` is undefined.

- [ ] **Step 3: Implement direct execution without a shell**

Construct `exec.CommandContext` from the configured binary, fixed arguments, and
the task description. Set `Dir`, merge environment variables in sorted order,
and call `CombinedOutput`. Convert `*exec.ExitError` into a normal
`ExecutionResult`; return startup failures and `ctx.Err()` as errors.

- [ ] **Step 4: Run package tests**

Run: `go test ./internal/runner/local -v`

Expected: PASS.

### Task 3: Git worktree workspace manager

**Files:**
- Create: `internal/workspace/gitworktree/manager.go`
- Create: `internal/workspace/gitworktree/manager_test.go`

**Interfaces:**
- Consumes: `workspace.Manager`, `workspace.Workspace`.
- Produces: `gitworktree.New(root string) (*Manager, error)`.

- [ ] **Step 1: Write a real-Git integration test**

Create a temporary source repository with one committed file, prepare two
workspaces from `main`, mutate one, and assert the source and sibling remain
unchanged. Cleanup must remove each worktree and be idempotent.

```go
first, err := manager.Prepare(ctx, source, "main")
require.NoError(t, err)
second, err := manager.Prepare(ctx, source, "main")
require.NoError(t, err)
require.NotEqual(t, first.Path(), second.Path())
require.NoError(t, os.WriteFile(filepath.Join(first.Path(), "file.txt"), []byte("changed"), 0o644))
assert.Equal(t, "original", readFile(t, filepath.Join(second.Path(), "file.txt")))
require.NoError(t, first.Cleanup(ctx))
require.NoError(t, first.Cleanup(ctx))
```

- [ ] **Step 2: Verify the test fails**

Run: `go test ./internal/workspace/gitworktree -v`

Expected: FAIL because `gitworktree.New` is undefined.

- [ ] **Step 3: Implement mirrors, worktrees, and cleanup**

Hash `repoURI` with SHA-256 to derive a stable mirror path. Under a manager
mutex, clone `--mirror` when absent, otherwise fetch `--prune origin`, allocate a
unique worktree path, and run:

```bash
git --git-dir <mirror> worktree add --detach <path> <branch>
```

The returned workspace stores the mirror and path and uses `sync.Once` around:

```bash
git --git-dir <mirror> worktree remove --force <path>
```

- [ ] **Step 4: Run package tests**

Run: `go test ./internal/workspace/gitworktree -v`

Expected: PASS when Git is installed; otherwise the package skips with an
explicit message.

### Task 4: Mem0 Platform adapter

**Files:**
- Create: `internal/memory/mem0/store.go`
- Create: `internal/memory/mem0/store_test.go`

**Interfaces:**
- Consumes: `memory.Store`.
- Produces: `mem0.New(apiKey string, opts ...Option) (*Store, error)`,
  `mem0.WithBaseURL(string)`, and `mem0.WithHTTPClient(*http.Client)`.

- [ ] **Step 1: Write HTTP contract tests**

Use `httptest.Server` to assert:

```go
assert.Equal(t, "/v3/memories/search/", r.URL.Path)
assert.Equal(t, "Token secret", r.Header.Get("Authorization"))
assert.JSONEq(t, `{
  "query":"agent task",
  "filters":{"AND":[{"app_id":"paje"},{"metadata":{"kind":"result"}}]},
  "top_k":5
}`, string(body))
```

Return a `results` envelope and verify it maps to `memory.Memory`. Add a save
test for `/v3/memories/add/`, a validation test requiring an entity tag, and a
non-2xx diagnostic test.

- [ ] **Step 2: Verify tests fail**

Run: `go test ./internal/memory/mem0 -v`

Expected: FAIL because the adapter does not exist.

- [ ] **Step 3: Implement the v3 HTTP adapter**

Entity tags `user_id`, `agent_id`, `app_id`, and `run_id` become top-level
fields for save and filter clauses for search. Remaining tags become a single
metadata object. Save sends one user-role message with `infer:false`. Search
sends `query`, logical filters, and `top_k`. Decode only required response
fields and cap error bodies at 4 KiB.

- [ ] **Step 4: Run package tests**

Run: `go test ./internal/memory/mem0 -v`

Expected: PASS.

### Task 5: Application workflow

**Files:**
- Create: `internal/workflow/orchestrator.go`
- Create: `internal/workflow/orchestrator_test.go`

**Interfaces:**
- Consumes: `memory.Store`, `workspace.Manager`, `runner.Runner`.
- Produces: `workflow.New(memory.Store, workspace.Manager, runner.Runner)
  (*Orchestrator, error)` and `(*Orchestrator).Run(context.Context, RunInput)
  (RunOutput, error)`.

- [ ] **Step 1: Write order and output tests**

Use recording test doubles to verify the exact sequence:

```go
assert.Equal(t, []string{"search", "prepare", "run", "save", "cleanup"}, calls)
assert.Equal(t, 2, output.MemoriesLoaded)
assert.Contains(t, recordedRun.TaskDescription, "Relevant memory")
assert.Equal(t, "/workspace/run-1", recordedRun.WorkspacePath)
```

Add table tests for search, prepare, run, save, and cleanup failures. Assert
cleanup occurs for every failure after prepare and that primary plus cleanup
errors remain discoverable through `errors.Is`.

- [ ] **Step 2: Verify tests fail**

Run: `go test ./internal/workflow -run TestOrchestrator -v`

Expected: FAIL because `workflow.New` and `Orchestrator.Run` are undefined.

- [ ] **Step 3: Implement the pipeline**

Validate the input, default `MemoryQuery` to `TaskDescription` and
`MemoryLimit` to 10, call the ports in the specified order, and use:

```go
cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
defer cancel()
```

for cleanup. Clone tags before adding `paje_exit_code` and
`paje_result=completed`. Join cleanup failures with the named return error.

- [ ] **Step 4: Run workflow tests**

Run: `go test ./internal/workflow -run TestOrchestrator -v`

Expected: PASS.

### Task 6: Hatchet binding, configuration, and daemon

**Files:**
- Create: `internal/workflow/hatchet.go`
- Create: `internal/workflow/hatchet_test.go`
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `cmd/paje/main.go`
- Create: `cmd/paje/main_test.go`
- Modify: `go.mod`
- Create: `go.sum`

**Interfaces:**
- Consumes: `workflow.Orchestrator` and all adapter constructors.
- Produces: `workflow.NewHatchetTask(*hatchet.Client, *Orchestrator)
  *hatchet.StandaloneTask`, `config.Load(func(string) string) (Config, error)`,
  and a blocking worker executable.

- [ ] **Step 1: Add the pinned Hatchet dependency**

Run:

```bash
go get github.com/hatchet-dev/hatchet@v0.97.0
```

Expected: Hatchet v0.97.0 appears as a direct requirement.

- [ ] **Step 2: Write configuration and composition tests**

Test that an absent Hatchet token fails, defaults select all mocks, malformed
runner args fail, and explicit `mem0`, `git`, and `local` selections validate
their required values. Test dependency construction separately from
`StartBlocking` so no external service is contacted.

- [ ] **Step 3: Verify focused tests fail**

Run: `go test ./internal/config ./cmd/paje ./internal/workflow -run 'Test(Load|Build|NewHatchet)' -v`

Expected: FAIL because the config, composition root, and Hatchet binding are
missing.

- [ ] **Step 4: Implement configuration and Hatchet registration**

The binding is deliberately thin:

```go
func NewHatchetTask(client *hatchet.Client, orchestrator *Orchestrator) *hatchet.StandaloneTask {
    return client.NewStandaloneTask("paje-agent-run",
        func(ctx hatchet.Context, input RunInput) (RunOutput, error) {
            return orchestrator.Run(ctx, input)
        },
    )
}
```

`main` loads config, calls `hatchet.NewClient`, constructs selected adapters,
creates the task, registers it with:

```go
worker, err := client.NewWorker(
    "paje-worker",
    hatchet.WithWorkflows(task),
)
```

and starts it with an interrupt-aware context.

- [ ] **Step 5: Run tests and build**

Run:

```bash
go test ./internal/config ./cmd/paje ./internal/workflow -v
go build ./cmd/paje
```

Expected: PASS and a successful build.

### Task 7: Container, Helm chart, and operator documentation

**Files:**
- Create: `Dockerfile`
- Create: `.dockerignore`
- Create: `charts/paje/Chart.yaml`
- Create: `charts/paje/values.yaml`
- Create: `charts/paje/templates/_helpers.tpl`
- Create: `charts/paje/templates/configmap.yaml`
- Create: `charts/paje/templates/secret.yaml`
- Create: `charts/paje/templates/serviceaccount.yaml`
- Create: `charts/paje/templates/deployment.yaml`
- Create: `charts/paje/templates/NOTES.txt`
- Modify: `README.md`

**Interfaces:**
- Consumes: the `cmd/paje` binary and documented environment configuration.
- Produces: a non-root OCI image and a lintable single-worker Helm release.

- [ ] **Step 1: Write packaging assertions**

Add `charts/paje/tests/render_test.yaml` only if the installed Helm version
supports chart tests as assertions; otherwise use exact render commands:

```bash
helm lint charts/paje --set secrets.hatchetClientToken=test
helm template paje charts/paje --set secrets.hatchetClientToken=test > /tmp/paje-rendered.yaml
```

The rendered Deployment must contain one replica, Secret references for tokens,
ConfigMap references for adapter settings, a non-root security context, and no
Service because the worker has no inbound listener.

- [ ] **Step 2: Add Docker and Helm artifacts**

The Docker builder uses Go 1.26 and the runtime installs only certificates, Git,
and OpenSSH. The chart exposes image, resources, service account, adapter,
workspace, runner, Hatchet endpoint/TLS, token, and extra environment values.

- [ ] **Step 3: Expand the README**

Document architecture, local tests, required Hatchet token, mock-default
quickstart, real-adapter environment variables, Docker, and Helm commands.

- [ ] **Step 4: Validate packaging**

Run:

```bash
helm lint charts/paje --set secrets.hatchetClientToken=test
helm template paje charts/paje --set secrets.hatchetClientToken=test
docker build -t paje:dev .
```

Expected: all commands succeed. If Docker is not available, record the exact
environment failure and still validate the Dockerfile with the available
builder frontend.

### Task 8: Completion audit

**Files:**
- Modify only files that fail the audit.

**Interfaces:**
- Consumes every deliverable above.
- Produces evidence that the complete specification is satisfied.

- [ ] **Step 1: Run source quality checks**

```bash
gofmt -w cmd internal
go vet ./...
go test -race ./...
go build ./cmd/paje
git diff --check
```

Expected: all commands succeed with no warnings.

- [ ] **Step 2: Audit the required tree and contracts**

Run:

```bash
rg --files cmd internal charts docs | sort
rg -n 'type (Store|Manager|Runner|Gate) interface' internal
```

Compare every port signature and named artifact against the consolidated
objective.

- [ ] **Step 3: Re-run packaging checks and inspect Git state**

```bash
helm lint charts/paje --set secrets.hatchetClientToken=test
helm template paje charts/paje --set secrets.hatchetClientToken=test
git status --short
```

Expected: lint and render succeed; status contains only the intended milestone
files.
