# Pajé Agent-Piloted Submission and Harness Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a scoped, deterministic agent-side submission path over the
existing Hatchet trigger, make Codex the first fully integrated harness, define
and enforce formal harness certification, and align every product surface with
an explicit current-versus-future support matrix.

**Architecture:** A new provider-neutral `submission.Service` owns canonical
request binding, durable idempotency, principal scope, lineage, and cancellation
semantics. A separate HTTP gateway authenticates narrow Pajé tokens and calls a
Hatchet trigger adapter; a `paje-agent` client and installable Codex plugin
provide the agent-side skill and lifecycle hooks. Existing code-change,
artifact, approval, publisher, and runner ports remain provider-neutral.

**Tech Stack:** Go 1.26.1, Hatchet Go SDK v0.97.0, Codex CLI 0.144.5 or the exact
version pinned when the task is executed, Go `net/http`, filesystem atomic
writes, Docker, Kubernetes, Helm 3, Node.js site regression tests, Codex plugin
skills and command hooks.

**Design:** [Pajé Agent-Piloted Submission and Harness Support Design](../specs/2026-07-25-agent-piloted-submission-and-harness-support-design.md)

## Global Constraints

- Module path remains exactly `github.com/araihu/paje`.
- `internal/workflow/codechange` and its ports must not import Hatchet, HTTP,
  Codex plugin, or submission-credential types.
- The only submit-capable template in v1 is exactly `code-change@v1`, triggered
  as Hatchet workflow `paje-code-change-v1`.
- Direct Hatchet triggering remains supported for operators.
- The agent-facing API exposes only submit, inspect, and cancel for scoped Pajé
  runs; it cannot trigger an arbitrary workflow or event.
- Agent-side processes never receive Hatchet worker/producer, Mem0, GitHub,
  Codex worker-auth, Git, SSH, or publisher credentials.
- Gateway, worker, publisher, and Codex auth use distinct Kubernetes Secrets
  and distinct process environments.
- Submission tokens are high-entropy scoped Pajé credentials. Clear tokens
  never appear in argv, logs, durable records, artifacts, hook state, or Git.
- `Idempotency-Key` is required and is bound to one credential ID and one
  canonical request for the lifetime of the binding.
- Outer `run_id` and nested `codechange.Input.IdempotencyKey` use the stable
  submission identity; existing Hatchet and run-store conflict behavior remains
  authoritative.
- The server computes `root_run_id` and `depth`. Root depth is `0`, default
  credential maximum is `0`, and v1 system maximum is exactly `1`.
- The v1 submission store and gateway support exactly one replica and one
  writable filesystem installation.
- Agent and verification commands never invoke a shell.
- A completed, started-but-ambiguous, timed-out, or canceled agent attempt is
  never automatically retried.
- Existing process-inspection, environment, artifact, approval, publication,
  restart-recovery, bounded-diagnostic, and monotonic-state invariants remain
  acceptance requirements.
- `generic` is the repository-language-neutral profile and requires explicit
  checks; `go` remains a first-class optimized profile.
- A toolchain is supported only when its executable exists in the worker image;
  language-neutral does not mean every runtime is preinstalled.
- `local` remains a low-level runner adapter and must never be labeled a
  certified harness.
- Codex becomes “fully integrated” only after execution certification,
  installable plugin tests, hook/skill tests, and live agent-to-Pajé acceptance
  pass for exact recorded versions.
- No second harness is named or implemented until the evidence gate in Task 12
  produces a committed selection decision.
- Public copy changes a capability from planned to current only in the commit
  that carries its acceptance evidence.

---

## File Map

### Provider-neutral submission domain

- `internal/submission/types.go`: principals, requests, records, status, and
  stable action/status enums.
- `internal/submission/errors.go`: typed public-safe domain errors.
- `internal/submission/trigger.go`: provider-neutral start/inspect/cancel port.
- `internal/submission/store.go`: reservation and durable-record port.
- `internal/submission/service.go`: scope, canonical binding, lineage,
  idempotency, status, and cancellation application service.
- `internal/submission/service_test.go`: domain and adversarial tests.
- `internal/submission/mock/*.go`: deterministic store and trigger doubles.

### Submission adapters and gateway

- `internal/submission/filesystem/*.go`: atomic durable reservation and trigger
  binding store.
- `internal/submission/hatchet/*.go`: Hatchet `RunNoWait`, status/final output,
  and cancellation adapter.
- `internal/submission/auth/*.go`: high-entropy token parsing, hashing, policy
  loading, expiry, and constant-time authentication.
- `internal/submission/httpapi/*.go`: bounded v1 JSON server and middleware.
- `internal/gatewayconfig/*.go`: gateway-only environment configuration.
- `cmd/paje-gateway/main.go`: hardened gateway composition and lifecycle.

### Agent client and Codex integration

- `internal/agentclient/*.go`: deterministic HTTP client, polling, token-file
  policy, hook context, and stable exit classification.
- `cmd/paje-agent/main.go`: submit/status/wait/cancel/hook command surface.
- `integrations/codex/paje/.codex-plugin/plugin.json`: installable plugin
  manifest.
- `integrations/codex/paje/skills/using-paje/SKILL.md`: explicit agent-pilot
  workflow.
- `integrations/codex/paje/skills/using-paje/agents/openai.yaml`: UI and
  invocation metadata.
- `integrations/codex/paje/hooks/hooks.json`: bounded `SessionStart`,
  `UserPromptSubmit`, and `Stop` command hooks.

### Certification, packaging, acceptance, and docs

- `internal/runner/contracttest/*.go`: reusable execution certification suite.
- `internal/runner/codex/*`: Codex adapter ratification against the suite.
- `internal/acceptance/codex_agent_pilot_test.go`: opt-in live originating
  Codex-to-Pajé round trip.
- `internal/acceptance/positioning_test.go`: README, Chart, docs, and matrix
  regression checks.
- `charts/paje/*`: optional gateway Deployment/Service, persistence, scoped
  credentials, and isolation assertions.
- `docs/submission-api.md`: v1 API and error contract.
- `docs/codex-integration.md`: plugin/client install, token provisioning, hook
  trust, and operations.
- `docs/harness-certification.md`: certification criteria and evidence format.
- `docs/second-harness-selection.md`: evidence-gated decision record.
- `README.md`, `site/README.md`, `site/app/page.tsx`, and
  `site/tests/rendered-html.test.mjs`: aligned product truth and regressions.

### Task 1: Define the provider-neutral submission contract and canonical binding

**Files:**
- Create: `internal/submission/types.go`
- Create: `internal/submission/errors.go`
- Create: `internal/submission/trigger.go`
- Create: `internal/submission/store.go`
- Create: `internal/submission/service.go`
- Create: `internal/submission/service_test.go`
- Create: `internal/submission/mock/store.go`
- Create: `internal/submission/mock/trigger.go`

**Interfaces:**
- Produces:
  `submission.New(Dependencies) (*Service, error)`,
  `(*Service).Submit(context.Context, Principal, SubmitRequest) (View, bool, error)`,
  `(*Service).Inspect(context.Context, Principal, string) (View, error)`, and
  `(*Service).Cancel(context.Context, Principal, string) (View, error)`.
- Produces provider-neutral `submission.Store` and `submission.Trigger`.
- Consumes the existing `template.Registry` and strict
  `template/codechange.Decode`.
- Does not import Hatchet, HTTP, filesystem, or bearer-token packages.

- [ ] **Step 1: Write failing tests for strict input, scope overwrite, action scope, and safe errors**

Create table-driven tests that use exact principal and request fixtures:

```go
func testPrincipal() submission.Principal {
    return submission.Principal{
        CredentialID: "cred-codex-service",
        Subject:      "codex@example.com",
        UserID:       "codex@example.com",
        AppID:        "service",
        Repositories: []submission.RepositoryScope{{
            Host: "github.com", Owner: "example", Name: "service",
        }},
        Actions: map[submission.Action]bool{
            submission.ActionSubmitArtifact: true,
            submission.ActionRead:           true,
            submission.ActionCancel:         true,
        },
        Harnesses: map[string]bool{"codex": true},
        MaxDepth: 0,
    }
}

func newTestService(t *testing.T) *submission.Service {
    t.Helper()
    registry, err := template.NewRegistry(templatecodechange.Definition{})
    if err != nil {
        t.Fatal(err)
    }
    service, err := submission.New(submission.Dependencies{
        Templates:      registry,
        Store:          submissionmock.NewStore(),
        Trigger:        submissionmock.NewTrigger(),
        Clock:          func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) },
        SystemMaxDepth: 1,
    })
    if err != nil {
        t.Fatal(err)
    }
    return service
}

func testInput(repositoryURI, userID, mode string) json.RawMessage {
    input := templatecodechange.Input{
        TaskDescription: "change timeout",
        RepositoryURI:   repositoryURI,
        BaseRef:         "main",
        Tags: map[string]string{
            "user_id": userID,
            "app_id":  "service",
        },
        Profile: "generic",
        Checks: []verification.CommandSpec{{
            Name: "test", Directory: ".", Executable: "npm",
            Args: []string{"test"}, Timeout: "10m", Required: true,
        }},
        Publication: templatecodechange.Publication{Mode: mode},
    }
    if mode == "pull_request" {
        input.Publication.Provider = "github"
        input.Publication.TargetBranch = "main"
        input.Publication.Title = "Change timeout"
        input.Publication.Draft = true
    }
    raw, err := json.Marshal(input)
    if err != nil {
        panic(err)
    }
    return raw
}

func TestSubmitBindsPrincipalAndRejectsClientIdempotencyField(t *testing.T) {
    service := newTestService(t)
    raw := json.RawMessage(`{
      "idempotency_key":"client-must-not-set-this",
      "task_description":"change timeout",
      "repository_uri":"https://github.com/example/service.git",
      "base_ref":"main",
      "tags":{"user_id":"codex@example.com","app_id":"service"},
      "profile":"generic",
      "checks":[{
        "name":"test","directory":".","executable":"npm",
        "args":["test"],"timeout":"10m","required":true
      }],
      "publication":{"mode":"artifact"}
    }`)

    _, _, err := service.Submit(context.Background(), testPrincipal(), submission.SubmitRequest{
        IdempotencyKey: strings.Repeat("a", 32),
        Template:       templatecodechange.ID,
        Input:          raw,
        Origin: submission.Origin{
            Harness: "codex", SessionID: "session-1", TurnID: "turn-1",
        },
    })
    if !errors.Is(err, submission.ErrInvalidRequest) {
        t.Fatalf("Submit() error = %v, want invalid request", err)
    }
}

func TestSubmitRejectsChangedIdentityAndRepositoryScope(t *testing.T) {
    tests := []struct {
        name string
        raw  string
        want error
    }{
        {
            name: "identity",
            raw: string(testInput(
                "https://github.com/example/service.git",
                "other@example.com",
                "artifact",
            )),
            want: submission.ErrForbidden,
        },
        {
            name: "repository",
            raw: string(testInput(
                "https://github.com/other/private.git",
                "codex@example.com",
                "artifact",
            )),
            want: submission.ErrForbidden,
        },
        {
            name: "publication",
            raw: string(testInput(
                "https://github.com/example/service.git",
                "codex@example.com",
                "pull_request",
            )),
            want: submission.ErrForbidden,
        },
    }
    for _, test := range tests {
        t.Run(test.name, func(t *testing.T) {
            service := newTestService(t)
            _, _, err := service.Submit(
                context.Background(),
                testPrincipal(),
                submission.SubmitRequest{
                    IdempotencyKey: strings.Repeat("a", 32),
                    Template:       templatecodechange.ID,
                    Input:          json.RawMessage(test.raw),
                    Origin: submission.Origin{
                        Harness: "codex",
                        SessionID: "session-1",
                        TurnID: "turn-1",
                    },
                },
            )
            if !errors.Is(err, test.want) {
                t.Fatalf("Submit() error = %v, want %v", err, test.want)
            }
        })
    }
}
```

Also cover missing/short/oversized `IdempotencyKey`, unknown template,
unknown JSON fields, unsupported harness, blank session/turn IDs, generic
profile without checks, shell-shaped checks, and error messages that omit raw
input.

- [ ] **Step 2: Run the focused tests and verify the package is missing**

Run:

```bash
go test ./internal/submission/... -run 'TestSubmit' -count=1
```

Expected: FAIL because `internal/submission` does not exist.

- [ ] **Step 3: Add exact domain types and stable errors**

Implement:

```go
type Action string

const (
    ActionSubmitArtifact    Action = "submit:artifact"
    ActionSubmitPullRequest Action = "submit:pull_request"
    ActionRead              Action = "read"
    ActionCancel            Action = "cancel"
)

type RepositoryScope struct {
    Host  string
    Owner string
    Name  string
}

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

type Status string

const (
    StatusAccepted              Status = "accepted"
    StatusQueued                Status = "queued"
    StatusRunning               Status = "running"
    StatusAwaitingApproval      Status = "awaiting_approval"
    StatusCancellationRequested Status = "cancellation_requested"
    StatusSucceeded             Status = "succeeded"
    StatusFailed                Status = "failed"
    StatusCanceled              Status = "canceled"
    StatusDeclined              Status = "declined"
)

type TriggerReference struct {
    Provider      string `json:"provider"`
    ExternalRunID string `json:"external_run_id"`
}

type TriggerRequest struct {
    RunID string
    Input json.RawMessage
}

type TriggerState struct {
    Status Status
    Result *templatecodechange.Result
}

type Trigger interface {
    Start(context.Context, TriggerRequest) (TriggerReference, error)
    Inspect(context.Context, TriggerReference) (TriggerState, error)
    Cancel(context.Context, TriggerReference) error
}

type Record struct {
    RunID                 string
    CredentialID          string
    RequestDigest         string
    IdempotencyKeyDigest  string
    Template              template.ID
    CanonicalInput        json.RawMessage
    Origin                Origin
    RootRunID             string
    Depth                 int
    Trigger               *TriggerReference
    CancellationRequested *time.Time
    CreatedAt             time.Time
    UpdatedAt             time.Time
}

type View struct {
    Record Record
    Status Status
    Result *templatecodechange.Result
}

type Dependencies struct {
    Templates      *template.Registry
    Store          Store
    Trigger        Trigger
    Clock          func() time.Time
    SystemMaxDepth int
}

type Reservation struct {
    Record         Record
    IdempotencyKey string
}

type Store interface {
    Reserve(context.Context, Reservation) (Record, bool, error)
    BindTrigger(context.Context, string, TriggerReference) (Record, error)
    Load(context.Context, string) (Record, error)
    LoadByKey(context.Context, string, string) (Record, error)
    MarkCancellationRequested(context.Context, string, time.Time) (Record, error)
}
```

Define `ErrInvalidRequest`, `ErrUnauthenticated`, `ErrForbidden`,
`ErrNotFound`, `ErrIdempotencyConflict`, `ErrDepthExceeded`,
`ErrRunNotCancelable`, and `ErrProviderUnavailable`. Wrap causes while keeping
`errors.Is` stable.

- [ ] **Step 4: Implement strict binding before reservation**

In `service.go`:

1. validate principal fields and requested action;
2. reject nested `idempotency_key` before calling `codechange.Decode`;
3. decode and normalize with the existing template definition;
4. require caller tags to match principal tags, then assign the principal
   values;
5. canonicalize repository identity using a provider-neutral parsed
   `RepositoryScope`;
6. set the nested idempotency key to
   `hex(sha256("paje-input-v1", credential_id, client_key))`;
7. encode with `run.CanonicalInput`;
8. hash the complete template, origin, principal-bound input, root, and depth.

Keep the stable public run ID helper private:

```go
func deriveRunID(credentialID, key string) string {
    sum := sha256.Sum256([]byte(
        "paje-run-v1\x00" + credentialID + "\x00" + key,
    ))
    return "paje_" + strings.ToLower(
        base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16]),
    )
}
```

The output is deterministic and safe for filenames, Hatchet input, and existing
run IDs. It is not treated as an authentication secret.

- [ ] **Step 5: Implement `Submit`, `Inspect`, and `Cancel` over the ports**

`Submit` must call `Store.Reserve` before `Trigger.Start`, call the trigger only
for the reservation owner or an owner record without a trigger reference, bind
the returned reference, and return `reused=true` for exact reuses.

`Inspect` and `Cancel` must require matching `CredentialID` and actions. Never
return a record that belongs to another principal.

`Cancel` records intent, calls the trigger, and keeps
`CancellationRequested` distinct from terminal `canceled`.

- [ ] **Step 6: Run focused tests**

Run:

```bash
go test -race ./internal/submission/... -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/submission
git commit -m "feat: define scoped submission domain"
```

### Task 2: Persist deterministic reservations, lineage, and trigger bindings

**Files:**
- Create: `internal/submission/filesystem/store.go`
- Create: `internal/submission/filesystem/store_test.go`
- Create: `internal/submission/filesystem/atomic.go`
- Create: `internal/submission/filesystem/atomic_test.go`
- Modify: `internal/submission/store.go`
- Modify: `internal/submission/service_test.go`
- Modify: `internal/submission/mock/store.go`

**Interfaces:**
- Produces:
  `filesystem.New(root string) (*Store, error)`.
- `submission.Store` is the durable owner of deterministic idempotency,
  including replay, conflict, and crash-reconciliation semantics.
- `Store.Reserve` atomically binds
  `(credential_id, idempotency_key)` to `(run_id, request_digest)`.
- `Store.BindTrigger` is compare-and-swap and idempotent for an exact provider
  reference.
- `Store.LoadByKey` never makes an old key reusable.

- [ ] **Step 1: Write failing tests for exact reuse, conflict, concurrency, and crash windows**

Use the same test matrix against filesystem and mock stores:

```go
func validReservation() submission.Reservation {
    return submission.Reservation{
        IdempotencyKey: strings.Repeat("a", 32),
        Record: submission.Record{
            RunID:                "paje_abc",
            CredentialID:         "cred-codex-service",
            RequestDigest:        strings.Repeat("1", 64),
            IdempotencyKeyDigest: strings.Repeat("2", 64),
            Template:             templatecodechange.ID,
            CanonicalInput:       json.RawMessage(`{"task_description":"change timeout"}`),
            Origin: submission.Origin{
                Harness: "codex", SessionID: "session-1", TurnID: "turn-1",
            },
            RootRunID: "paje_abc",
            Depth:     0,
            CreatedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
            UpdatedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
        },
    }
}

func TestStoreContract(t *testing.T, newStore func(*testing.T) submission.Store) {
    t.Helper()
    t.Run("exact reuse", func(t *testing.T) {
        store := newStore(t)
        reservation := validReservation()
        first, owned, err := store.Reserve(context.Background(), reservation)
        if err != nil || !owned {
            t.Fatalf("first reserve = %#v, %v, %v", first, owned, err)
        }
        second, owned, err := store.Reserve(context.Background(), reservation)
        if err != nil || owned || second.RunID != first.RunID {
            t.Fatalf("reuse = %#v, %v, %v", second, owned, err)
        }
    })

    t.Run("changed digest conflicts", func(t *testing.T) {
        store := newStore(t)
        reservation := validReservation()
        _, _, _ = store.Reserve(context.Background(), reservation)
        reservation.Record.RequestDigest = strings.Repeat("f", 64)
        _, _, err := store.Reserve(context.Background(), reservation)
        if !errors.Is(err, submission.ErrIdempotencyConflict) {
            t.Fatalf("conflict error = %v", err)
        }
    })
}
```

Add:

- 32 concurrent exact reservations yield one owner and one run ID;
- different requests racing on one key yield one owner and conflicts;
- restart after reservation but before trigger bind preserves ownership;
- exact trigger bind repeats harmlessly;
- changed trigger reference conflicts;
- cancellation-request timestamps are monotonic;
- corrupt, duplicate, directory, symlink, and non-canonical files fail closed;
- `LoadByKey` survives detailed-record pruning through an immutable binding
  tombstone.

- [ ] **Step 2: Run tests and verify the adapter is missing**

Run:

```bash
go test ./internal/submission/filesystem -count=1
```

Expected: FAIL because the filesystem store does not exist.

- [ ] **Step 3: Implement the durable layout**

Use:

```text
<root>/records/<run-id>.json
<root>/idempotency/<sha256(credential-id NUL key)>.json
```

Binding JSON contains only:

```go
type binding struct {
    CredentialID  string `json:"credential_id"`
    KeyDigest     string `json:"key_digest"`
    RunID         string `json:"run_id"`
    RequestDigest string `json:"request_digest"`
}
```

Store the key digest, never the clear idempotency key, in filenames or binding
files. The record may retain the clear client key only if tests prove it is
bounded and non-secret; prefer storing its SHA-256 digest.

- [ ] **Step 4: Reuse the beta durability pattern**

For every record and binding write:

1. create a mode-`0600` temporary file in the destination directory;
2. write canonical JSON;
3. `fsync` the file;
4. close it;
5. atomically rename it without crossing filesystems;
6. `fsync` the parent directory.

Resolve and verify the root once. Reject path escape, symlink roots after
construction, non-regular entries, duplicate logical records, invalid JSON,
unknown fields, and disagreement between record and binding.

- [ ] **Step 5: Implement lineage validation in `submission.Service`**

For a missing parent set:

```go
record.Depth = 0
record.RootRunID = record.RunID
```

For a present parent:

```go
parent, err := store.Load(ctx, request.Origin.ParentRunID)
if err != nil || parent.CredentialID != principal.CredentialID {
    return Record{}, false, ErrForbidden
}
depth := parent.Depth + 1
if depth > min(principal.MaxDepth, s.systemMaxDepth) {
    return Record{}, false, ErrDepthExceeded
}
record.Depth = depth
record.RootRunID = parent.RootRunID
```

Reject blank/corrupt root IDs, self-parent, cross-principal parent, and any
stored parent whose depth/root chain fails recomputation. Set
`systemMaxDepth = 1`.

- [ ] **Step 6: Run store and domain race tests**

Run:

```bash
go test -race ./internal/submission/... -count=1
```

Expected: PASS with one reservation owner in every concurrent test.

- [ ] **Step 7: Commit**

```bash
git add internal/submission
git commit -m "feat: persist submission identity and lineage"
```

### Task 3: Add the Hatchet trigger adapter without leaking provider types

**Files:**
- Create: `internal/submission/hatchet/trigger.go`
- Create: `internal/submission/hatchet/trigger_test.go`
- Create: `internal/submission/hatchet/client.go`
- Modify: `internal/submission/trigger.go`
- Modify: `internal/workflow/codechangehatchet/workflow.go`
- Modify: `internal/workflow/codechangehatchet/workflow_test.go`

**Interfaces:**
- Produces:
  `hatchettrigger.New(client Client) (*Trigger, error)`.
- `Client` contains only the minimum SDK-shaped methods needed by the adapter
  and is mockable without a Hatchet server.
- Consumes `(*hatchet.Client).RunNoWait`,
  `(*hatchet.Client).Runs().GetDetails`, and
  `(*hatchet.Client).Runs().Cancel` only inside this adapter.
- Keeps workflow name `paje-code-change-v1` and the exact outer envelope.

- [ ] **Step 1: Write failing tests for start, collision, inspect, and cancel**

Define a narrow fakeable client:

```go
type Client interface {
    Start(
        context.Context,
        string,
        map[string]any,
    ) (externalRunID string, err error)
    Details(context.Context, string) (Details, error)
    Cancel(context.Context, string) error
}
```

Test exact start input:

```go
func TestStartUsesCanonicalWorkflowEnvelope(t *testing.T) {
    client := &fakeClient{startID: "7ffeb1fe-986b-4a1b-aec1-722b8151c138"}
    trigger, err := hatchettrigger.New(client)
    if err != nil {
        t.Fatal(err)
    }
    input := json.RawMessage(`{"task_description":"change"}`)
    ref, err := trigger.Start(context.Background(), submission.TriggerRequest{
        RunID: "paje_abc", Input: input,
    })
    if err != nil {
        t.Fatal(err)
    }
    if client.workflow != "paje-code-change-v1" ||
        client.envelope["run_id"] != "paje_abc" {
        t.Fatalf("start = %q %#v", client.workflow, client.envelope)
    }
    if ref.Provider != "hatchet" || ref.ExternalRunID != client.startID {
        t.Fatalf("reference = %#v", ref)
    }
}
```

Also test:

- provider idempotency collision reconciles only when details contain the same
  outer Pajé run ID;
- queued/running/failed/canceled/completed mappings;
- completed run requires a `finalize` output;
- final output run ID and status must agree with the provider state;
- malformed, oversized, missing, or contradictory output fails closed;
- cancellation targets only the stored external UUID;
- no provider error body or token reaches the public error.

- [ ] **Step 2: Run tests and verify the adapter is missing**

Run:

```bash
go test ./internal/submission/hatchet -count=1
```

Expected: FAIL because the adapter does not exist.

- [ ] **Step 3: Implement the SDK wrapper**

The concrete wrapper calls:

```go
ref, err := client.RunNoWait(
    ctx,
    "paje-code-change-v1",
    map[string]any{
        "run_id": request.RunID,
        "input":  json.RawMessage(request.Input),
    },
)
```

Persist only `ref.RunId`. Convert SDK details into adapter-local `Details`;
never expose SDK types through `submission.Trigger`.

For cancellation, parse the stored external ID as UUID and call
`Runs().Cancel` with exactly that workflow external ID. Treat “already
terminal/not cancelable” as an inspect-and-reconcile path.

- [ ] **Step 4: Extract the workflow name as one shared adapter constant**

Export a provider-edge constant or function from
`codechangehatchet` so declaration and trigger cannot drift:

```go
const WorkflowName = "paje-code-change-v1"
```

Update the declaration test to assert the exact name, idempotency expression
`input.run_id`, status method, and 30-day TTL.

- [ ] **Step 5: Run adapter and workflow declaration tests**

Run:

```bash
go test -race ./internal/submission/hatchet ./internal/workflow/codechangehatchet -count=1
```

Expected: PASS without a live Hatchet server.

- [ ] **Step 6: Prove the core has no provider import**

Run:

```bash
if rg -n 'hatchet-dev|submission/hatchet|net/http' internal/submission \
  -g '*.go' -g '!hatchet/**' -g '!httpapi/**'; then
  exit 1
fi
```

Expected: exit zero with no matches.

- [ ] **Step 7: Commit**

```bash
git add internal/submission internal/workflow/codechangehatchet
git commit -m "feat: adapt scoped submissions to Hatchet"
```

### Task 4: Authenticate scoped Pajé credentials and expose the bounded v1 API

**Files:**
- Create: `internal/submission/auth/token.go`
- Create: `internal/submission/auth/token_test.go`
- Create: `internal/submission/auth/policy.go`
- Create: `internal/submission/auth/policy_test.go`
- Create: `internal/submission/httpapi/server.go`
- Create: `internal/submission/httpapi/server_test.go`
- Create: `internal/submission/httpapi/limits.go`
- Create: `internal/submission/httpapi/response.go`

**Interfaces:**
- Produces:
  `auth.LoadPolicy(path string, now func() time.Time) (*Authenticator, error)`,
  `(*Authenticator).Authenticate(string) (submission.Principal, error)`, and
  `httpapi.New(Dependencies) (http.Handler, error)`.
- Consumes only `submission.Service` methods from the domain.
- Public endpoints are exactly those listed in the design.

- [ ] **Step 1: Write failing token-policy tests**

Use a fixed high-entropy test token:

```go
const clearToken = "paje_v1_codex01.ERERERERERERERERERERERERERERERERERERERERERE"

func TestAuthenticateReturnsExactScopedPrincipal(t *testing.T) {
    encodedSecret := strings.TrimPrefix(clearToken, "paje_v1_codex01.")
    secret, err := base64.RawURLEncoding.DecodeString(encodedSecret)
    if err != nil || len(secret) != 32 {
        t.Fatalf("decode fixture secret: %v, length %d", err, len(secret))
    }
    sum := sha256.Sum256(secret)
    policyJSON, err := json.Marshal(map[string]any{
        "schema_version": 1,
        "credentials": []map[string]any{{
            "id":          "codex01",
            "secret_hash": hex.EncodeToString(sum[:]),
            "subject":     "codex@example.com",
            "user_id":     "codex@example.com",
            "app_id":      "service",
            "repositories": []string{
                "https://github.com/example/service.git",
            },
            "actions":    []string{"submit:artifact", "read", "cancel"},
            "harnesses":  []string{"codex"},
            "max_depth":  0,
            "expires_at": "2027-01-01T00:00:00Z",
        }},
    })
    if err != nil {
        t.Fatal(err)
    }
    policyPath := filepath.Join(t.TempDir(), "policy.json")
    if err := os.WriteFile(policyPath, policyJSON, 0o600); err != nil {
        t.Fatal(err)
    }
    authenticator, err := auth.LoadPolicy(
        policyPath,
        func() time.Time {
            return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
        },
    )
    if err != nil {
        t.Fatal(err)
    }
    principal, err := authenticator.Authenticate(clearToken)
    if err != nil {
        t.Fatal(err)
    }
    if principal.CredentialID != "codex01" ||
        principal.UserID != "codex@example.com" ||
        principal.MaxDepth != 0 {
        t.Fatalf("principal = %#v", principal)
    }
}
```

Test malformed tokens, unknown public IDs, wrong secret, duplicate IDs/hashes,
expired entries, invalid repository identities, unknown actions/harnesses,
depth below zero or above one, policy symlinks, non-regular files, and policy
mode other than `0600`.

Assert all authentication failures compare as `ErrUnauthenticated` and never
include the token or hash.

- [ ] **Step 2: Write failing HTTP contract and adversarial tests**

Cover:

- required bearer, content type, and idempotency header;
- exact/changed reuse status codes `200`/`409`;
- first accepted submission `202`;
- unknown JSON fields and trailing values `400`;
- body larger than 1 MiB `413`;
- header and idempotency limits;
- principal-safe `404` for another principal's run;
- `GET`, cancel, health, and readiness;
- unsupported methods `405` with `Allow`;
- no unlisted route can provide a workflow name or event key;
- panic recovery produces bounded `internal` without a stack or input body;
- request logs redact authorization and body.

Use `httptest.Server`; do not start Hatchet.

- [ ] **Step 3: Run tests and verify packages are missing**

Run:

```bash
go test ./internal/submission/auth ./internal/submission/httpapi -count=1
```

Expected: FAIL because the packages do not exist.

- [ ] **Step 4: Implement strict policy loading and token comparison**

Token parsing must:

1. require `paje_v1_`;
2. split one public ID and one base64url secret;
3. require exactly 32 decoded secret bytes;
4. find the public policy entry;
5. hash the supplied secret;
6. use `subtle.ConstantTimeCompare`;
7. check expiry only after the constant-time comparison;
8. return a cloned principal.

The policy file stores hashes only and is loaded before serving. A runtime
reload is out of scope for v1.

- [ ] **Step 5: Implement the bounded JSON server**

Use `http.MaxBytesReader`, `json.Decoder.DisallowUnknownFields`, a required EOF,
server-generated request IDs, and per-handler contexts.

Map errors exactly:

```go
var errorStatus = map[string]int{
    "invalid_request":      http.StatusBadRequest,
    "unauthenticated":      http.StatusUnauthorized,
    "forbidden":            http.StatusForbidden,
    "not_found":            http.StatusNotFound,
    "idempotency_conflict": http.StatusConflict,
    "depth_exceeded":       http.StatusUnprocessableEntity,
    "run_not_cancelable":   http.StatusConflict,
    "provider_unavailable": http.StatusServiceUnavailable,
    "internal":             http.StatusInternalServerError,
}
```

Return only the stable error code/message. Add `Retry-After` for provider
unavailability and polling responses.

- [ ] **Step 6: Run focused, race, and fuzz-seed tests**

Run:

```bash
go test -race ./internal/submission/auth ./internal/submission/httpapi -count=1
go test ./internal/submission/httpapi -run 'TestMalformed|TestOversized|TestRoute' -count=20
```

Expected: PASS with no token or body in failure output.

- [ ] **Step 7: Commit**

```bash
git add internal/submission/auth internal/submission/httpapi
git commit -m "feat: expose scoped submission API"
```

### Task 5: Compose a hardened, separately credentialed gateway

**Files:**
- Create: `internal/gatewayconfig/config.go`
- Create: `internal/gatewayconfig/config_test.go`
- Create: `cmd/paje-gateway/main.go`
- Create: `cmd/paje-gateway/main_test.go`
- Modify: `internal/processguard/guard_linux.go`
- Modify: `internal/processguard/guard_linux_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Produces:
  `gatewayconfig.Load(func(string) string) (Config, error)`.
- `cmd/paje-gateway` composes filesystem store, token authenticator, Hatchet
  trigger, submission service, and bounded `http.Server`.
- The gateway and worker use separate Hatchet client instances and environment
  keys.
- The gateway executes no repository-controlled command.

- [ ] **Step 1: Write failing configuration tests**

Define exact gateway configuration:

```go
type Config struct {
    ListenAddress       string
    SubmissionRoot      string
    TokenPolicyFile     string
    HatchetProducerToken string
    ReadHeaderTimeout   time.Duration
    ReadTimeout         time.Duration
    WriteTimeout        time.Duration
    IdleTimeout         time.Duration
    ShutdownTimeout     time.Duration
}
```

Test defaults and requirements:

```go
func TestLoadRequiresOnlyGatewayCredentials(t *testing.T) {
    values := map[string]string{
        "PAJE_GATEWAY_HATCHET_TOKEN":      "producer-token",
        "PAJE_GATEWAY_TOKEN_POLICY_FILE":  "/run/paje-gateway/policy.json",
        "PAJE_GATEWAY_SUBMISSION_ROOT":    "/var/lib/paje/submissions",
    }
    cfg, err := gatewayconfig.Load(func(key string) string { return values[key] })
    if err != nil {
        t.Fatal(err)
    }
    if cfg.ListenAddress != "127.0.0.1:8080" {
        t.Fatalf("listen = %q", cfg.ListenAddress)
    }
}
```

Assert:

- missing producer token, policy file, or submission root fails;
- listen address defaults to loopback, never public wildcard;
- all timeout defaults are positive and bounded;
- submission root and policy file cannot overlap;
- worker keys `HATCHET_CLIENT_TOKEN`, `MEM0_API_KEY`, `GITHUB_TOKEN`,
  `CODEX_HOME`, `GH_TOKEN`, Git/SSH keys, and publisher keys are ignored and
  never copied into the config;
- no generic `extraEnv` or environment allowlist exists for the gateway.

- [ ] **Step 2: Write failing composition and shutdown tests**

Inject constructor functions into `run` so tests prove:

- `processguard.Harden` runs before configuration reads;
- the Hatchet producer token goes only to the gateway Hatchet client;
- the HTTP server receives bounded timeouts and exact handler;
- context cancellation performs bounded `Shutdown`;
- listener/start errors preserve cancellation identity;
- startup closes the Hatchet client and submission store on every failure path;
- logs contain no token-policy content or credentials.

- [ ] **Step 3: Run tests and verify the command/config are missing**

Run:

```bash
go test ./internal/gatewayconfig ./cmd/paje-gateway -count=1
```

Expected: FAIL because both packages are missing.

- [ ] **Step 4: Implement fail-closed gateway configuration**

Use exact environment keys:

```text
PAJE_GATEWAY_LISTEN_ADDRESS
PAJE_GATEWAY_SUBMISSION_ROOT
PAJE_GATEWAY_TOKEN_POLICY_FILE
PAJE_GATEWAY_HATCHET_TOKEN
PAJE_GATEWAY_READ_HEADER_TIMEOUT
PAJE_GATEWAY_READ_TIMEOUT
PAJE_GATEWAY_WRITE_TIMEOUT
PAJE_GATEWAY_IDLE_TIMEOUT
PAJE_GATEWAY_SHUTDOWN_TIMEOUT
```

Defaults:

```text
listen             127.0.0.1:8080
read header        5s
read               15s
write              30s
idle               60s
shutdown           10s
```

Reject wildcard listen addresses unless the operator explicitly sets one; Helm
uses the Pod IP/port behind a ClusterIP Service and does not rely on the local
binary default.

- [ ] **Step 5: Compose dependencies in the security-preserving order**

`main` must:

1. install the process-inspection guard;
2. load and validate gateway config;
3. load hashed token policy;
4. construct the submission filesystem store;
5. construct a Hatchet client with only
   `PAJE_GATEWAY_HATCHET_TOKEN`;
6. construct the trigger adapter and template registry;
7. construct `submission.Service`;
8. construct the HTTP handler and server;
9. serve until signal cancellation;
10. shut down HTTP, close Hatchet, and close stores with bounded
    non-canceled contexts.

Do not reuse `config.Config`, `runtimeDependencies`, or the worker composition
root; their credential sets are intentionally different.

- [ ] **Step 6: Add health and readiness semantics**

`/healthz` proves only that the process loop is alive.

`/readyz` performs bounded read-only checks that:

- token policy loaded successfully;
- submission root is readable and writable through a temporary file inside its
  dedicated readiness directory;
- Hatchet client construction succeeded.

Readiness must not submit a workflow, consume an idempotency key, or reveal
provider details.

- [ ] **Step 7: Run focused and platform tests**

Run:

```bash
go test -race ./internal/gatewayconfig ./cmd/paje-gateway ./internal/processguard -count=1
go build ./cmd/paje-gateway
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/paje-gateway
```

Expected: PASS on the native platform; the Linux cross-build succeeds. A
non-Linux runtime invocation remains fail-closed through `processguard`.

- [ ] **Step 8: Commit**

```bash
git add internal/gatewayconfig internal/processguard cmd/paje-gateway go.mod go.sum
git commit -m "feat: compose hardened submission gateway"
```

### Task 6: Build the deterministic `paje-agent` client

**Files:**
- Create: `internal/agentclient/client.go`
- Create: `internal/agentclient/client_test.go`
- Create: `internal/agentclient/token.go`
- Create: `internal/agentclient/token_test.go`
- Create: `internal/agentclient/context.go`
- Create: `internal/agentclient/context_test.go`
- Create: `internal/agentclient/exit.go`
- Create: `cmd/paje-agent/main.go`
- Create: `cmd/paje-agent/main_test.go`

**Interfaces:**
- Produces:
  `agentclient.New(Config) (*Client, error)`,
  `(*Client).Submit`,
  `(*Client).Status`,
  `(*Client).Wait`, and
  `(*Client).Cancel`.
- Produces exact commands:
  `submit`, `status`, `wait`, `cancel`, and `hook`.
- Consumes only the public v1 HTTP contract, never Hatchet.

- [ ] **Step 1: Write failing HTTP-client tests**

Use `httptest.Server` and assert:

```go
func TestSubmitSendsTokenOnlyInAuthorizationHeader(t *testing.T) {
    var gotHeader string
    var gotBody []byte
    server := httptest.NewServer(http.HandlerFunc(func(
        writer http.ResponseWriter,
        request *http.Request,
    ) {
        gotHeader = request.Header.Get("Authorization")
        gotBody, _ = io.ReadAll(request.Body)
        writer.Header().Set("Content-Type", "application/json")
        writer.WriteHeader(http.StatusAccepted)
        _, _ = writer.Write([]byte(`{
          "api_version":"v1","run_id":"paje_abc",
          "status":"accepted","reused":false,
          "depth":0,"root_run_id":"paje_abc"
        }`))
    }))
    defer server.Close()

    client, err := agentclient.New(agentclient.Config{
        BaseURL:         server.URL,
        Token:           clearToken,
        HTTPClient:      server.Client(),
        MaxRequestBytes: 1 << 20,
        MaxResponseBytes: 1 << 20,
    })
    if err != nil {
        t.Fatal(err)
    }
    _, err = client.Submit(context.Background(), agentclient.SubmitRequest{
        IdempotencyKey: strings.Repeat("a", 64),
        Body: json.RawMessage(`{
          "template":{"name":"code-change","version":1},
          "origin":{
            "harness":"codex","session_id":"session-1","turn_id":"turn-1"
          },
          "input":{
            "task_description":"change timeout",
            "repository_uri":"https://github.com/example/service.git",
            "base_ref":"main",
            "tags":{"user_id":"codex@example.com","app_id":"service"},
            "profile":"generic",
            "checks":[{
              "name":"test","directory":".","executable":"npm",
              "args":["test"],"timeout":"10m","required":true
            }],
            "publication":{"mode":"artifact"}
          }
        }`),
    })
    if err != nil {
        t.Fatal(err)
    }
    if gotHeader != "Bearer "+clearToken {
        t.Fatalf("authorization = %q", gotHeader)
    }
    if bytes.Contains(gotBody, []byte(clearToken)) {
        t.Fatal("request body contains bearer token")
    }
}
```

Also test:

- bounded request/response bodies;
- content type, API version, run/root binding, and terminal result validation;
- `Retry-After` and jittered bounded polling;
- context cancellation and timeout identity;
- stable classification of `401`, `403`, `404`, `409`, `422`, `503`, and
  terminal workflow failure;
- malformed success response is never accepted;
- logs/stdout/stderr never include token values.

- [ ] **Step 2: Write failing token-file and hook-context tests**

Token-file tests require:

- regular file;
- owner-only `0600`;
- no symlink at the file or parent path;
- exactly one valid token line;
- no repository-relative default;
- `PAJE_AGENT_TOKEN` accepted only when
  `PAJE_AGENT_ALLOW_ENV_TOKEN=1`.

Hook context uses:

```go
type HookContext struct {
    SchemaVersion int    `json:"schema_version"`
    SessionID     string `json:"session_id"`
    TurnID        string `json:"turn_id"`
    CWD           string `json:"cwd"`
    ActiveRunID   string `json:"active_run_id,omitempty"`
    UpdatedAt     string `json:"updated_at"`
}
```

It is stored at `$PLUGIN_DATA/context.json` with directory `0700`, file `0600`,
atomic replace, and no prompt, transcript, request body, or token fields.

- [ ] **Step 3: Write failing command tests for JSON output and exit codes**

Exact exit codes:

```go
const (
    ExitOK                = 0
    ExitInvalidInput      = 2
    ExitAuthentication    = 3
    ExitForbidden         = 4
    ExitConflictOrDepth   = 5
    ExitUnavailable       = 6
    ExitTimeout           = 7
    ExitCanceled          = 8
    ExitWorkflowFailed    = 9
    ExitInternal          = 10
)
```

Test each command with injected stdin/stdout/stderr and fake client. Assert
stdout is one JSON object, diagnostics are bounded, and tokens never appear.

- [ ] **Step 4: Run tests and verify packages are missing**

Run:

```bash
go test ./internal/agentclient ./cmd/paje-agent -count=1
```

Expected: FAIL because the client and command do not exist.

- [ ] **Step 5: Implement the client and stable Codex idempotency helper**

Implement:

```go
func CodexIdempotencyKey(
    sessionID, turnID, repository, baseRef string,
) (string, error) {
    values := []string{sessionID, turnID, repository, baseRef}
    for _, value := range values {
        if strings.TrimSpace(value) == "" {
            return "", ErrInvalidInput
        }
    }
    sum := sha256.Sum256([]byte(
        "paje-codex-submit-v1\x00" +
        sessionID + "\x00" +
        turnID + "\x00" +
        canonicalRepository(repository) + "\x00" +
        strings.TrimSpace(baseRef),
    ))
    return hex.EncodeToString(sum[:]), nil
}
```

`submit` reads a strict JSON file or stdin. If no explicit key is supplied, it
requires valid plugin hook context and derives the key above. An explicit key
is required for non-Codex/manual use.

- [ ] **Step 6: Implement bounded wait and terminal handling**

Poll no faster than one second and no slower than 15 seconds, honor
`Retry-After`, add at most 20% jitter, and stop at caller timeout.

Terminal `succeeded` requires a result with matching run ID. `failed`,
`canceled`, and `declined` are successful HTTP exchanges but nonzero client
outcomes. Never resubmit from `wait`.

- [ ] **Step 7: Implement hook subcommands without side effects**

`hook user-prompt-submit` reads hook JSON from stdin, validates
`hook_event_name == "UserPromptSubmit"`, and writes safe context.

`hook session-start` and `hook stop` may perform one status request for
`ActiveRunID`; they output valid hook JSON containing only a concise
`systemMessage`. They never call submit or cancel and never return
`continue:false`.

- [ ] **Step 8: Run focused, race, and cross-build tests**

Run:

```bash
go test -race ./internal/agentclient ./cmd/paje-agent -count=1
go build ./cmd/paje-agent
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/paje-agent
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/paje-agent
```

Expected: PASS. Token-file platform behavior must be explicit; Windows uses an
operator-provisioned secure file and platform-specific permission checks or
fails closed until that check is implemented.

- [ ] **Step 9: Commit**

```bash
git add internal/agentclient cmd/paje-agent
git commit -m "feat: add deterministic agent submission client"
```

### Task 7: Package the Codex skill and lifecycle hooks

**Files:**
- Create: `integrations/codex/paje/.codex-plugin/plugin.json`
- Create: `integrations/codex/paje/skills/using-paje/SKILL.md`
- Create: `integrations/codex/paje/skills/using-paje/agents/openai.yaml`
- Create: `integrations/codex/paje/hooks/hooks.json`
- Create: `integrations/codex/paje/testdata/session-start.json`
- Create: `integrations/codex/paje/testdata/user-prompt-submit.json`
- Create: `integrations/codex/paje/testdata/stop.json`
- Create: `integrations/codex/paje/testdata/install-smoke.txt`
- Create: `integrations/codex/paje/plugin_test.go`
- Modify: `cmd/paje-agent/main.go`
- Modify: `cmd/paje-agent/main_test.go`

**Interfaces:**
- Produces an installable Codex plugin named `paje`.
- Produces one focused skill named `using-paje`.
- Hooks invoke `paje-agent hook <event>` and receive state through
  `PLUGIN_DATA`.
- The plugin never bundles or provisions a clear credential.

- [ ] **Step 1: Refresh the official Codex manual before fixing manifest/hook fields**

Run the repository-documented OpenAI docs helper or fetch the current official
Codex manual. Verify all of:

```text
skills use SKILL.md with name and description
installable plugin manifest is .codex-plugin/plugin.json
plugin hooks use hooks/hooks.json or an explicit manifest path
PLUGIN_ROOT and PLUGIN_DATA are available
SessionStart, UserPromptSubmit, and Stop are supported command-hook events
non-managed hooks require exact-definition trust
```

Record the checked manual date and links in `docs/codex-integration.md` in
Task 11. If any interface changed, update this plan's paths in a docs-only
commit before implementation.

- [ ] **Step 2: Write failing manifest, skill, and hook fixture tests**

`plugin_test.go` must parse JSON/YAML/frontmatter and assert:

- manifest name/version/description and relative skills/hooks paths;
- all manifest paths stay under plugin root;
- exactly one skill with `name: using-paje`;
- skill description requires explicit delegation or explicit `$using-paje`;
- `allow_implicit_invocation` is true only with the narrow description;
- hooks contain only `SessionStart`, `UserPromptSubmit`, and `Stop`;
- every hook is `type: command`, has a timeout no greater than 10 seconds, and
  invokes `paje-agent hook` without a shell-expanded token;
- no file contains `HATCHET_`, `MEM0_`, `GITHUB_TOKEN`, `GH_TOKEN`,
  `CODEX_HOME`, `PAJE_GATEWAY_HATCHET_TOKEN`, or a token-shaped string.

For each JSON hook fixture, run the actual `paje-agent hook` command with a
temporary `PLUGIN_DATA` and assert valid bounded output and mode-`0600` context.

- [ ] **Step 3: Run tests and verify plugin files are missing**

Run:

```bash
go test ./integrations/codex/paje ./cmd/paje-agent -count=1
```

Expected: FAIL because the plugin does not exist.

- [ ] **Step 4: Write the exact plugin manifest and hooks**

Use:

```json
{
  "name": "paje",
  "version": "0.1.0",
  "description": "Submit and follow scoped durable code changes through Pajé",
  "skills": "./skills/",
  "hooks": "./hooks/hooks.json"
}
```

Hook commands use the installed binary by name, do not interpolate repository
content, and use timeouts of 5 seconds:

```json
{
  "description": "Safe Pajé run context for Codex",
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "paje-agent hook session-start",
        "timeout": 5,
        "statusMessage": "Checking Pajé run context"
      }]
    }],
    "UserPromptSubmit": [{
      "hooks": [{
        "type": "command",
        "command": "paje-agent hook user-prompt-submit",
        "timeout": 5
      }]
    }],
    "Stop": [{
      "hooks": [{
        "type": "command",
        "command": "paje-agent hook stop",
        "timeout": 5,
        "statusMessage": "Checking Pajé run status"
      }]
    }]
  }
}
```

- [ ] **Step 5: Write the complete skill workflow**

The `SKILL.md` must instruct Codex to:

1. require explicit user intent to delegate a code change to Pajé;
2. inspect repository URI/base ref and available toolchain;
3. choose `generic` with explicit checks or `go` for Go defaults;
4. reject secrets, raw environment values, shell fragments, and unsupported
   publication modes;
5. detect `PAJE_RUN_ID` or active plugin context and enforce root/depth rules;
6. derive one key from session/turn/repository/base;
7. call `paje-agent submit` once;
8. persist only safe run context;
9. call status or wait without resubmission;
10. handle success, failure, decline, cancellation, conflict, depth denial, and
    timeout with stable next actions.

Include exact artifact and pull-request JSON examples from the public API,
using `profile: generic` in the first example.

- [ ] **Step 6: Test local discovery and trust behavior**

Create a temporary local plugin marketplace or use the current Codex plugin
development command documented by the refreshed manual. Install the plugin in
an isolated `CODEX_HOME`, then verify:

- `$using-paje` appears;
- the three hooks appear as untrusted on first install;
- no hook runs before trust;
- after trusting the exact definition, fixture sessions invoke each hook;
- changing `hooks.json` invalidates prior trust;
- uninstall removes skill and hooks but leaves operator token files outside the
  plugin untouched.

Capture commands and bounded output in
`integrations/codex/paje/testdata/install-smoke.txt`.

- [ ] **Step 7: Run plugin regression tests**

Run:

```bash
go test -race ./integrations/codex/paje ./cmd/paje-agent -count=1
git diff --check
```

Expected: PASS, and the repository contains no clear submission token.

- [ ] **Step 8: Commit**

```bash
git add integrations/codex/paje cmd/paje-agent
git commit -m "feat: package Codex Pajé integration"
```

### Task 8: Deploy the optional gateway with pairwise credential isolation

**Files:**
- Modify: `charts/paje/Chart.yaml`
- Modify: `charts/paje/values.yaml`
- Modify: `charts/paje/values.schema.json`
- Create: `charts/paje/templates/gateway-deployment.yaml`
- Create: `charts/paje/templates/gateway-service.yaml`
- Create: `charts/paje/templates/gateway-secret.yaml`
- Modify: `charts/paje/templates/_helpers.tpl`
- Modify: `charts/paje/templates/NOTES.txt`
- Modify: `charts/paje/render_test.go`
- Modify: `Dockerfile`
- Modify: `internal/acceptance/image_test.go`

**Interfaces:**
- Produces `gateway.enabled`, gateway image/listen/service/storage/timeout
  values, and distinct producer/policy Secret references.
- Keeps `replicaCount: 1` for the worker and requires
  `gateway.replicaCount: 1`.
- Installs both `paje-gateway` and `paje-agent` binaries in the exact-revision
  image; only the gateway binary runs in the gateway Deployment.

- [ ] **Step 1: Write failing Helm render tests**

Add tests for:

- default render has no gateway Deployment or Service;
- enabled render has exactly one gateway replica, one ClusterIP Service,
  non-root/read-only-root security, dropped capabilities, no privilege
  escalation, no service-account token, and bounded probes;
- gateway environment contains only gateway configuration and two Secret
  references;
- gateway contains no worker Hatchet, Mem0, GitHub, Codex, Git, SSH, or
  publisher values;
- worker contains no gateway producer token or submission token-policy value;
- all active worker/gateway/Codex/publisher Secret names are pairwise distinct;
- policy Secret and producer Secret are distinct;
- ingress is absent;
- `gateway.replicaCount != 1` fails schema validation;
- enabled gateway without persistence or exact existing Secret references
  fails rendering.

- [ ] **Step 2: Run the chart tests and verify they fail**

Run:

```bash
go test ./charts/paje -count=1
helm lint charts/paje \
  --set adapters.runner=mock \
  --set secrets.hatchet.value=test
```

Expected: FAIL because gateway values/templates do not exist.

- [ ] **Step 3: Add exact values and schema**

Use:

```yaml
gateway:
  enabled: false
  replicaCount: 1
  listenAddress: 0.0.0.0:8080
  service:
    port: 8080
  persistence:
    existingClaim: ""
  hatchet:
    existingSecret: ""
    key: producer-token
  tokenPolicy:
    existingSecret: ""
    key: policy.json
  timeouts:
    readHeader: 5s
    read: 15s
    write: 30s
    idle: 60s
    shutdown: 10s
```

The chart requires an existing claim when enabled so gateway reservation state
survives replacement. It does not place clear token values in Helm values.

- [ ] **Step 4: Render the separate workload and mounts**

The gateway Deployment uses:

- the chart's security contexts;
- a dedicated ServiceAccount;
- producer token through one Secret key;
- policy file mounted read-only;
- submission root mounted read-write;
- runtime and temp `emptyDir`;
- `/healthz` liveness and `/readyz` readiness;
- no shared process namespace;
- no worker envFrom ConfigMap.

If worker and gateway share one PVC, use separate fixed subdirectories and
document the storage-mode constraint. The gateway never mounts worker runtime,
Codex home, publisher credentials, or artifacts.

- [ ] **Step 5: Update the image and exact-revision inspection**

Build all three commands:

```text
/usr/local/bin/paje
/usr/local/bin/paje-gateway
/usr/local/bin/paje-agent
```

Retain the exact 40-character `PAJE_COMMIT` validation and pinned Codex version.
Image tests assert file ownership, executable modes, non-root UID, and that the
gateway help/version path requires no worker credentials.

- [ ] **Step 6: Run Helm and image-static checks**

Run:

```bash
go test -race ./charts/paje ./internal/acceptance -run 'Test.*Render|TestDocker' -count=1
helm lint charts/paje \
  --set adapters.runner=mock \
  --set secrets.hatchet.value=test
helm template paje charts/paje \
  --set adapters.runner=mock \
  --set secrets.hatchet.value=test \
  >/tmp/paje-default.yaml
helm template paje charts/paje \
  --set adapters.runner=mock \
  --set secrets.hatchet.existingSecret=worker-hatchet \
  --set gateway.enabled=true \
  --set gateway.persistence.existingClaim=paje-data \
  --set gateway.hatchet.existingSecret=gateway-producer \
  --set gateway.tokenPolicy.existingSecret=gateway-policy \
  >/tmp/paje-gateway.yaml
```

Expected: PASS. The enabled render has no credential alias and no Ingress.

- [ ] **Step 7: Commit**

```bash
git add charts/paje Dockerfile internal/acceptance/image_test.go
git commit -m "feat: package isolated submission gateway"
```

### Task 9: Formalize execution certification and ratify Codex

**Files:**
- Create: `internal/runner/contracttest/suite.go`
- Create: `internal/runner/contracttest/suite_test.go`
- Create: `internal/runner/contracttest/helper_test.go`
- Modify: `internal/runner/codex/runner.go`
- Modify: `internal/runner/codex/runner_test.go`
- Modify: `internal/runner/local/runner_test.go`
- Modify: `internal/executil/process_unix.go`
- Modify: `internal/executil/process_unix_test.go`
- Modify: `internal/environment/policy_test.go`
- Modify: `internal/processguard/guard_linux_test.go`
- Create: `docs/harness-certification.md`

**Interfaces:**
- Produces:
  `contracttest.Run(t *testing.T, fixture Fixture)`.
- `Fixture` identifies exact executable/args/protocol/sandbox/auth behavior for
  one dedicated adapter.
- Codex remains the only adapter registered as execution-certified.
- `local` uses low-level process tests but is excluded from certification.

- [ ] **Step 1: Write a failing reusable contract suite**

Define:

```go
type Fixture struct {
    HarnessID       string
    HarnessVersion  string
    New             func(*testing.T, string) runner.Runner
    HelperCommand   string
    SuccessEvents   []string
    MissingTerminal []string
    MalformedEvents []string
    AllowedAfterTerminal []string
    ExpectedArgs    []string
}

func Run(t *testing.T, fixture Fixture) {
    t.Helper()
    tests := []struct {
        name string
        run  func(*testing.T, Fixture)
    }{
        {"deterministic invocation", testDeterministicInvocation},
        {"terminal completion", testTerminalCompletion},
        {"missing terminal", testMissingTerminal},
        {"malformed transcript", testMalformedTranscript},
        {"bounded output", testBoundedOutput},
        {"cancellation", testCancellation},
    }
    for _, test := range tests {
        t.Run(test.name, func(t *testing.T) {
            test.run(t, fixture)
        })
    }
}
```

Implement every named test function in `suite.go`. Exact assertions are:

- deterministic invocation: observed args equal `ExpectedArgs`, the child sees
  only fixture environment, stdin is closed, and no TTY or shell exists;
- terminal completion: every `SuccessEvents` line is consumed and the exact
  final message is returned with `Started` and `Completed`;
- missing terminal: exit zero plus `MissingTerminal` returns a protocol error
  and `Completed` is not treated as a successful response;
- malformed transcript: each `MalformedEvents` case returns a bounded protocol
  error and never a response;
- bounded output: the helper exceeds the configured limit, `Truncated` is true,
  and completion after truncation is not accepted;
- cancellation: a helper parent and grandchild start, context is canceled,
  `errors.Is(err, context.Canceled)` is true, `Started` is true, `Completed` is
  false, and both PIDs disappear within one second.

- [ ] **Step 2: Add Codex protocol fixtures**

Use recorded sanitized JSONL fixtures with:

- valid `item.completed` agent message and accepted trailing telemetry;
- exit zero with no completed agent message;
- invalid JSON object;
- completed non-agent item only;
- multiple agent messages with the exact documented “last completed message”
  rule;
- truncation before the terminal frame;
- nonzero exit;
- context cancellation before start and after child start.

Assert the exact Codex vector:

```text
exec --json --ephemeral --ignore-user-config --sandbox workspace-write
```

No current working-directory, prompt, auth path, or operator config is embedded
in the fixed args.

- [ ] **Step 3: Run the suite and observe the first unmet requirement**

Run:

```bash
go test ./internal/runner/contracttest ./internal/runner/codex \
  -run 'TestContract|TestCodex' -count=1
```

Expected: FAIL until the suite exists and Codex's transcript behavior satisfies
every formal assertion.

- [ ] **Step 4: Make transcript semantics explicit**

Refactor `lastCompletedAgentMessage` into a parser that returns:

```go
type Completion struct {
    Message       string
    Completed     bool
    EventCount    int
    TerminalIndex int
}
```

Keep the existing last-completed-agent-message rule if the refreshed Codex
protocol evidence confirms it. Explicitly allow only known telemetry after the
last completed agent message. Reject malformed JSON frames that claim to be
protocol events, missing completion, and truncation before completion.

Do not reject ordinary non-JSON subprocess diagnostics unless they exceed the
bounded transcript; classify them as transcript evidence, not completion.

- [ ] **Step 5: Add adversarial cancellation, sandbox, and credential probes**

Tests must prove:

- process-group cancellation terminates a helper grandchild within one second;
- canceled execution returns `context.Canceled`, `Started=true`,
  `Completed=false`;
- workspace-write args cannot be replaced by input;
- exact environment excludes parent sentinels;
- Codex auth appears only in agent-stage environment;
- verification excludes Codex and all service credentials;
- Linux process guard blocks same-UID reads of worker `/proc/<pid>/environ`,
  `/proc/<pid>/mem`, and credential-bearing descriptors when capabilities are
  dropped.

Keep platform-specific tests opt-in or build-tagged where the OS cannot provide
the boundary; production remains fail-closed.

- [ ] **Step 6: Write the certification evidence format**

`docs/harness-certification.md` must reproduce EC-1 through EC-8 and AP-1
through AP-5 from the design, then define the evidence fields and validation:

| Field | Validation |
| --- | --- |
| `harness` | exact registered harness ID |
| `support_level` | `execution-certified` or `fully-integrated` |
| `paje_commit` | exactly 40 lowercase hexadecimal characters |
| `worker_image_digest` | `sha256:` plus 64 lowercase hexadecimal characters |
| `adapter_package` | repository-relative Go package below `internal/runner` |
| `harness_version` | exact non-empty binary version output |
| `tests.unit` | exact unit-test command |
| `tests.race` | exact race-test command |
| `tests.live` | exact opt-in live command |
| `recorded_at` | UTC RFC3339 timestamp |

Generated evidence files contain concrete values and are created only by
acceptance runs.

- [ ] **Step 7: Run certification and existing security tests**

Run:

```bash
go test -race \
  ./internal/runner/contracttest \
  ./internal/runner/codex \
  ./internal/runner/local \
  ./internal/executil \
  ./internal/environment \
  ./internal/processguard \
  -count=1
```

Expected: PASS. `local` remains absent from any certified-harness registry or
documentation claim.

- [ ] **Step 8: Commit**

```bash
git add internal/runner internal/executil internal/environment \
  internal/processguard docs/harness-certification.md
git commit -m "test: formalize Codex harness certification"
```

### Task 10: Prove live Codex execution and agent-piloted round-trip acceptance

**Files:**
- Modify: `internal/acceptance/codex_test.go`
- Create: `internal/acceptance/codex_agent_pilot_test.go`
- Modify: `internal/acceptance/helpers_test.go`
- Modify: `internal/acceptance/prerequisites_test.go`
- Create: `internal/acceptance/testdata/agent-pilot-task.json`
- Modify: `integrations/codex/paje/plugin_test.go`
- Create after live pass: `docs/evidence/codex-execution.yaml`
- Create after live pass: `docs/evidence/codex-agent-pilot.yaml`

**Interfaces:**
- Keeps `PAJE_CODEX_INTEGRATION=1` for worker-side live execution.
- Adds `PAJE_CODEX_AGENT_PILOT_ACCEPTANCE=1` for the full originating-session
  round trip.
- Produces concrete certification evidence only after a successful opt-in run.

- [ ] **Step 1: Extend the existing live Codex test with formal assertions**

Keep its disposable two-module repository and exact one-line edit. Add:

- exact Codex binary version capture;
- exact Pajé commit and optional image digest;
- contract-suite protocol version;
- no submission/gateway credential in agent or verification environment;
- no submission token in artifact members, durable evidence, logs, or process
  arguments;
- source tree/status unchanged;
- process group gone;
- worktree and runtime directories empty;
- exact artifact reproduction.

- [ ] **Step 2: Write a failing opt-in agent-pilot acceptance test**

The test requires:

```text
PAJE_CODEX_AGENT_PILOT_ACCEPTANCE=1
PAJE_CODEX_AGENT_PILOT_GATEWAY_URL
PAJE_CODEX_AGENT_PILOT_TOKEN_FILE
PAJE_CODEX_AGENT_PILOT_REPOSITORY
PAJE_CODEX_AGENT_PILOT_BASE_REF
PAJE_CODEX_AGENT_PILOT_APP_ID
CODEX_HOME
```

When opted in, any missing variable, binary, plugin, authentication file, or
reachable gateway is fatal rather than skipped.

The test:

1. verifies the target repository is an explicitly dedicated disposable
   acceptance repository;
2. installs the local plugin into an isolated Codex home and trusts only its
   exact hook hash through the documented test-only mechanism;
3. invokes a real originating `codex exec --json --ephemeral` session with an
   explicit `$using-paje` prompt and a unique marker;
4. requires the skill to submit `artifact` mode through `paje-agent`;
5. extracts one Pajé run ID from structured client output;
6. waits for one terminal durable result;
7. verifies the changed artifact and terminal marker;
8. reruns status, not submit, and proves no second Hatchet/Pajé run exists;
9. scans originating transcript, hook state, gateway logs supplied by the
   fixture, workflow evidence, and artifact for credential sentinels;
10. removes the isolated plugin home without touching the operator token file.

- [ ] **Step 3: Add an adversarial recursion acceptance case**

Submit a task whose worker-side Codex prompt asks it to invoke `$using-paje`
again. Prove:

- the worker environment has no gateway token or token-file path;
- the plugin/skill detects `PAJE_RUN_ID` when context is present;
- a root-only credential returns `depth_exceeded` or `forbidden` before
  Hatchet;
- exactly one root run exists.

The test uses artifact mode and a disposable repository; it never publishes a
pull request.

- [ ] **Step 4: Run default acceptance and verify explicit skips**

Run:

```bash
go test ./internal/acceptance -v -count=1
```

Expected: PASS with explicit skip messages for live Codex, live agent-pilot,
GitHub, Docker, and Kubernetes checks when their opt-in variables are absent.

- [ ] **Step 5: Commit the acceptance harness before live execution**

Run:

```bash
git add internal/acceptance integrations/codex/paje
git commit -m "test: add Codex agent-pilot acceptance"
```

Record the resulting 40-character commit. Live evidence in the next steps must
name that exact commit; do not run release acceptance against uncommitted
production or acceptance code.

- [ ] **Step 6: Run worker-side live Codex acceptance**

Run only with authenticated Codex and explicit opt-in:

```bash
PAJE_CODEX_INTEGRATION=1 \
  go test ./internal/acceptance \
  -run TestCodexArtifactAcceptance -v -count=1
```

Expected: PASS and write one concrete execution-certification evidence file to
the test's temporary output location. Copy it into committed documentation only
after redaction review and exact revision verification.

- [ ] **Step 7: Run full agent-pilot acceptance**

Run only against the explicitly provisioned non-production gateway and
disposable repository:

```bash
PAJE_CODEX_AGENT_PILOT_ACCEPTANCE=1 \
PAJE_CODEX_AGENT_PILOT_GATEWAY_URL="$PAJE_CODEX_AGENT_PILOT_GATEWAY_URL" \
PAJE_CODEX_AGENT_PILOT_TOKEN_FILE="$PAJE_CODEX_AGENT_PILOT_TOKEN_FILE" \
PAJE_CODEX_AGENT_PILOT_REPOSITORY="$PAJE_CODEX_AGENT_PILOT_REPOSITORY" \
PAJE_CODEX_AGENT_PILOT_BASE_REF="$PAJE_CODEX_AGENT_PILOT_BASE_REF" \
PAJE_CODEX_AGENT_PILOT_APP_ID="$PAJE_CODEX_AGENT_PILOT_APP_ID" \
  go test ./internal/acceptance \
  -run TestCodexAgentPilotAcceptance -v -count=1
```

Expected: PASS with one run, one reproducible artifact, a terminal originating
Codex response, and no credential sentinel.

- [ ] **Step 8: Commit redacted exact evidence**

Copy only the schema-valid, redacted evidence for the exact Step 5 commit into
`docs/evidence/codex-execution.yaml` and
`docs/evidence/codex-agent-pilot.yaml`. Never commit credentials or unredacted
live logs:

```bash
git add docs/evidence/codex-execution.yaml \
  docs/evidence/codex-agent-pilot.yaml
git commit -m "test: record Codex agent-pilot evidence"
```

### Task 11: Align README, Helm metadata, site, docs, and positioning regressions

**Files:**
- Modify: `README.md`
- Modify: `charts/paje/Chart.yaml`
- Modify: `charts/paje/templates/NOTES.txt`
- Modify: `site/README.md`
- Modify: `site/app/page.tsx`
- Modify: `site/tests/rendered-html.test.mjs`
- Create: `docs/submission-api.md`
- Create: `docs/codex-integration.md`
- Create: `internal/acceptance/positioning_test.go`
- Create: `internal/acceptance/docs_links_test.go`

**Interfaces:**
- Makes the design's three support matrices canonical public documentation.
- Links root/site/operator docs to the design, API, integration, and
  certification docs.
- Changes Codex agent-pilot from planned to current only if Task 10 live
  acceptance passed at the exact commit being documented.

- [ ] **Step 1: Write failing positioning tests before changing copy**

`positioning_test.go` reads repository files from a resolved root and asserts:

```go
func TestCanonicalProductPositioning(t *testing.T) {
    root := repositoryRoot(t)
    readme := readFile(t, filepath.Join(root, "README.md"))
    chart := readFile(t, filepath.Join(root, "charts/paje/Chart.yaml"))

    requireAll(t, readme,
        "implemented in Go",
        "`generic`",
        "`go`",
        "Codex",
        "direct Hatchet",
        "submission API",
        "local",
        "not a certified harness",
    )
    rejectAll(t, readme+"\n"+chart,
        "Go-native",
        "every runtime is included",
        "local is a supported harness",
    )
}
```

Also assert:

- matrices contain trigger/profile/harness dimensions;
- current/planned labels agree with Task 10 evidence;
- Chart description says durable, language-neutral, and implemented in Go;
- README states toolchains must exist in the image;
- second harness remains unselected or cites the committed Task 12 decision.

- [ ] **Step 2: Extend rendered-site tests before editing the page**

Add assertions for:

- direct Hatchet is current;
- scoped API/client and Codex plugin are current only after Task 10;
- generic and Go profile qualifications;
- local runner is not certified;
- second harness is behind the evidence gate;
- toolchains are operator-provided;
- no `Go-native` or unsupported multi-harness claim.

Run:

```bash
cd site
npm test
```

Expected: FAIL on the new support matrix until the page is aligned.

- [ ] **Step 3: Write complete API and Codex integration docs**

`docs/submission-api.md` includes:

- exact endpoints, headers, request/response/error schemas;
- token scopes and policy-file schema;
- canonical idempotency and crash reconciliation;
- parent/root/depth behavior;
- cancellation semantics;
- curl examples that read the token from a protected file without placing it
  in shell history;
- direct Hatchet compatibility and migration.

`docs/codex-integration.md` includes:

- exact `paje-agent` installation/version verification;
- plugin install and removal;
- skill discovery and explicit `$using-paje` invocation;
- hook review/trust and changed-hash behavior;
- API URL and token-file provisioning;
- artifact and pull-request examples;
- status/wait/cancel;
- recursion behavior and safe troubleshooting;
- the official Codex manual links and date verified in Task 7.

- [ ] **Step 4: Align root README and Helm metadata**

Replace the opening with the canonical position from the design. Add compact
support matrices before operator setup. Keep detailed beta durability,
approval, publication, deployment, and security documentation intact.

Document:

- direct Hatchet and gateway paths;
- separate gateway/worker credentials;
- gateway single-replica/persistent-store limit;
- token issuance and revocation;
- deterministic key behavior;
- Codex support level;
- second-harness gate.

Do not remove existing exact beta commands or security warnings.

- [ ] **Step 5: Align the site and site README**

Render current and future capabilities separately. If Task 10 passed, the page
may say “Codex is the first fully integrated harness” and describe skill/hook
installation as available. Otherwise it must retain “designed for” and
“planned” wording.

Keep `generic` examples valid and state the worker-image toolchain requirement.

- [ ] **Step 6: Implement local Markdown link validation**

`docs_links_test.go` scans Markdown links in:

```text
README.md
docs/**/*.md
site/README.md
```

For relative links:

- strip fragments;
- URL-decode;
- resolve below repository root;
- reject missing targets and path escape.

For absolute HTTPS links, validate syntax in default tests. A separate opt-in
`PAJE_LINK_ACCEPTANCE=1` test performs bounded `HEAD`, then `GET` fallback, for
the official Codex, GitHub, and public site links.

- [ ] **Step 7: Run documentation and site regressions**

Run:

```bash
go test ./internal/acceptance \
  -run 'TestCanonicalProductPositioning|TestDocumentationLinks' \
  -count=1
cd site
npm ci
npm run lint
npm test
```

Expected: PASS. The rendered page and every local Markdown link resolve.

- [ ] **Step 8: Commit**

```bash
git add README.md charts/paje/Chart.yaml charts/paje/templates/NOTES.txt \
  site docs internal/acceptance/positioning_test.go \
  internal/acceptance/docs_links_test.go
git commit -m "docs: align Pajé product support claims"
```

### Task 12: Run the evidence gate for a second dedicated harness

**Files:**
- Create: `docs/second-harness-selection.md`
- Modify: `docs/harness-certification.md`
- Modify: `README.md`
- Modify: `site/app/page.tsx`
- Modify: `site/tests/rendered-html.test.mjs`

**Interfaces:**
- Produces one committed decision: a named candidate passed the evidence gate,
  or no candidate is selected.
- Produces no production adapter, auth, image, or plugin code.
- A named passing candidate triggers a separate candidate-specific design and
  implementation plan before code work.

- [ ] **Step 1: Collect repository and user evidence before candidate research**

Search:

```bash
git log --all --oneline -- README.md docs internal site
rg -n -i \
  'harness|codex|claude|gemini|opencode|agent runner|non-interactive' \
  README.md docs internal site
```

Record concrete requests, past decisions, adapter experiments, deployment
constraints, and acceptance infrastructure. If this evidence names no desired
second harness, state that absence; do not convert popularity into user demand.

- [ ] **Step 2: Evaluate only evidence-supported candidates**

For each candidate justified by Step 1, collect primary evidence for:

1. stable non-interactive command and exact args;
2. machine-readable terminal protocol;
3. cancellation and descendant behavior;
4. workspace sandbox;
5. authentication files/environment and isolation feasibility;
6. exact package/version and redistribution terms;
7. license compatibility;
8. disposable live acceptance feasibility;
9. installable skill/hook or equivalent lifecycle surface;
10. user/deployment demand.

Run safe local `--help`/`--version` probes when installed. Do not authenticate,
submit work, install a candidate, or change external state during this docs
gate.

- [ ] **Step 3: Write the fixed decision record**

`docs/second-harness-selection.md` starts with:

```markdown
# Pajé Second Harness Selection

## Decision

The first sentence starts with `Selected:` and includes the evidence-backed
candidate name and evidence date, or is exactly:
`No selection: no candidate currently satisfies every security-critical gate.`

## Repository and User Evidence
## Candidate Evidence
## Certification Gap Analysis
## Required Candidate-Specific Design
## Support-Matrix Impact
```

The committed record contains one concrete decision sentence and no unresolved
marker.

Security-critical gates EC-1 through EC-6 and live-acceptance feasibility are
all-or-nothing. Missing evidence produces “No selection.”

- [ ] **Step 4: Apply the decision without overstating support**

If no candidate passes:

- README/site continue to say “additional harnesses are planned after an
  evidence gate”;
- link the no-selection record;
- do not add config values or code directories.

If a candidate passes:

- README/site may say “selected for implementation,” never “supported”;
- create and get approval for a design under `docs/superpowers/specs` whose
  filename contains the decision date and selected canonical harness ID;
- after approval, create the matching detailed implementation plan;
- do not implement it as part of this task or this plan.

- [ ] **Step 5: Run positioning regressions**

Run:

```bash
go test ./internal/acceptance \
  -run 'TestCanonicalProductPositioning|TestDocumentationLinks' \
  -count=1
cd site
npm test
```

Expected: PASS with no claim that two harnesses are currently supported.

- [ ] **Step 6: Commit**

```bash
git add docs/second-harness-selection.md docs/harness-certification.md \
  README.md site/app/page.tsx site/tests/rendered-html.test.mjs
git commit -m "docs: record second harness evidence gate"
```

### Task 13: Run full security, compatibility, and completion gates

**Files:**
- Modify as findings require: files introduced or changed in Tasks 1-12 only
- Modify: this plan, checking completed task boxes

**Interfaces:**
- Produces exact verification evidence for one implementation commit.
- Does not broaden scope to another workflow template or second harness.

- [ ] **Step 1: Audit requirements against implementation**

Create a temporary checklist mapping every design acceptance criterion to:

- implementation file;
- focused test;
- live/opt-in evidence when required;
- public documentation.

Any missing mapping blocks completion and is fixed in the owning task's files.

- [ ] **Step 2: Run focused security tests**

Run:

```bash
go test -race \
  ./internal/submission/... \
  ./internal/gatewayconfig \
  ./internal/agentclient \
  ./cmd/paje-gateway \
  ./cmd/paje-agent \
  ./internal/runner/... \
  ./internal/environment \
  ./internal/processguard \
  ./integrations/codex/paje \
  -count=1
```

Expected: PASS with no race, credential sentinel, leaked child, duplicate
reservation owner, or untrusted-hook bypass.

- [ ] **Step 3: Run the full Go quality gate**

Run:

```bash
go test -race ./... -count=1
go test ./... -count=1
go vet ./...
go build ./cmd/paje ./cmd/paje-gateway ./cmd/paje-agent
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build ./cmd/paje ./cmd/paje-gateway ./cmd/paje-agent
```

Expected: every command exits zero.

- [ ] **Step 4: Run Helm and image gates**

Run:

```bash
go test ./charts/paje ./internal/acceptance -count=1
helm lint charts/paje \
  --set adapters.runner=mock \
  --set secrets.hatchet.value=test
helm template paje charts/paje \
  --set adapters.runner=mock \
  --set secrets.hatchet.existingSecret=worker-hatchet \
  --set gateway.enabled=true \
  --set gateway.persistence.existingClaim=paje-data \
  --set gateway.hatchet.existingSecret=gateway-producer \
  --set gateway.tokenPolicy.existingSecret=gateway-policy \
  >/tmp/paje-final.yaml
```

If Docker acceptance is explicitly enabled, build from the exact current
40-character commit with `--no-cache`, inspect all three binaries, UID/GID,
Codex version, read-only-root operation, and absence of credentials in layers
or image config.

Expected: lint/render/tests pass; optional exact-image acceptance passes when
opted in.

- [ ] **Step 5: Run site and documentation gates**

Run:

```bash
cd site
npm ci
npm run lint
npm test
cd ..
git diff --check
```

Expected: site build, rendered HTML regressions, local link checks, and diff
hygiene pass.

- [ ] **Step 6: Run live acceptance gates**

With explicit non-production prerequisites, run:

```bash
PAJE_CODEX_INTEGRATION=1 \
  go test ./internal/acceptance \
  -run TestCodexArtifactAcceptance -v -count=1

PAJE_CODEX_AGENT_PILOT_ACCEPTANCE=1 \
PAJE_CODEX_AGENT_PILOT_GATEWAY_URL="$PAJE_CODEX_AGENT_PILOT_GATEWAY_URL" \
PAJE_CODEX_AGENT_PILOT_TOKEN_FILE="$PAJE_CODEX_AGENT_PILOT_TOKEN_FILE" \
PAJE_CODEX_AGENT_PILOT_REPOSITORY="$PAJE_CODEX_AGENT_PILOT_REPOSITORY" \
PAJE_CODEX_AGENT_PILOT_BASE_REF="$PAJE_CODEX_AGENT_PILOT_BASE_REF" \
PAJE_CODEX_AGENT_PILOT_APP_ID="$PAJE_CODEX_AGENT_PILOT_APP_ID" \
  go test ./internal/acceptance \
  -run TestCodexAgentPilotAcceptance -v -count=1
```

Run GitHub publication and Kubernetes server-side dry-run acceptance only with
their existing explicit opt-ins and verified disposable/non-production
targets.

Expected: live Codex execution and live agent-pilot acceptance pass at the exact
implementation commit. Any opted-in missing prerequisite is fatal.

- [ ] **Step 7: Perform an independent adversarial review**

Review the complete implementation range for:

- provider leakage into core ports;
- token or service-credential exposure;
- scope widening;
- idempotency split-brain and crash windows;
- lineage forgery or depth bypass;
- cancellation lies and automatic retry;
- malformed transcript false success;
- hook auto-submission or trust bypass;
- unsafe filesystem roots/symlinks/modes;
- gateway/worker Secret aliasing;
- support-matrix claims ahead of acceptance evidence.

Resolve every blocking or important finding and rerun the affected focused
gate plus Steps 2-6.

- [ ] **Step 8: Mark the plan complete and verify diff hygiene**

Check every completed checkbox in this plan, then run:

```bash
git diff --check
git status --short
git log --oneline --decorate -15
```

Expected: no unchecked implementation task, no uncommitted implementation
file, and a reviewable commit sequence matching Tasks 1-12.

- [ ] **Step 9: Commit final review fixes and evidence**

```bash
git add internal cmd integrations charts docs README.md site Dockerfile \
  go.mod go.sum
git commit -m "test: close agent-piloted support acceptance"
```

If Step 7 required no changes, do not create an empty commit. Record the exact
verified commit in the release/PR description.
