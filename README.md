# Pajé

Pajé is a self-hosted, Go-native orchestration control plane for parallel AI
agents. It sits between event producers and black-box agent executables while
Hatchet provides durable queueing, concurrency control, retries, and workflow
visibility.

The initial worker implements this pipeline:

```text
Retrieve memory -> Prepare Git workspace -> Run agent -> Save result -> Cleanup
```

## Architecture

Pajé uses ports and adapters:

- `internal/memory`: provider-neutral memory store, a concurrent mock, and a
  Mem0 Platform v3 adapter.
- `internal/workspace`: workspace contract, mock, and isolated Git worktree
  manager.
- `internal/runner`: black-box execution contract, mock, local `os/exec`
  runner, and deterministic Codex JSONL adapter.
- `internal/approval`: human approval contract and mock. Approval is not yet
  part of the first pipeline.
- `internal/workflow`: service-free orchestration plus the Hatchet task binding.
- `cmd/paje`: configuration, dependency composition, and Hatchet worker
  lifecycle.

Hatchet is an outer adapter. The application workflow does not import Hatchet
types and its test suite does not require Hatchet Server, PostgreSQL, Mem0, or
Kubernetes.

## Requirements

- Go 1.26 or newer
- Git for the real workspace adapter
- An authenticated Codex CLI when the `codex` runner is selected
- A Hatchet client token for the daemon
- Docker and Helm 3 for container/Kubernetes packaging

## Develop

Run the full test suite:

```bash
go test ./...
```

Build the worker:

```bash
go build ./cmd/paje
```

Run the opt-in integration proof against the installed Codex CLI:

```bash
PAJE_CODEX_INTEGRATION=1 \
  go test -count=1 -run TestCodexOrchestrationIntegration -v \
  ./internal/workflow
```

The proof uses the real orchestration path: it retrieves an in-memory context
record, creates a detached Git worktree, invokes authenticated Codex, extracts
the terminal response, saves that result back to memory, and verifies worktree
cleanup.

Start it with all in-memory adapters:

```bash
export HATCHET_CLIENT_TOKEN="<token>"
go run ./cmd/paje
```

For self-hosted Hatchet, configure the SDK connection variables as needed:

```bash
export HATCHET_CLIENT_HOST_PORT="hatchet-engine.hatchet.svc.cluster.local:7070"
export HATCHET_CLIENT_SERVER_URL="http://hatchet-api.hatchet.svc.cluster.local:8080"
export HATCHET_CLIENT_TLS_STRATEGY="none"
```

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `HATCHET_CLIENT_TOKEN` | required | Hatchet worker authentication |
| `PAJE_MEMORY_ADAPTER` | `mock` | `mock` or `mem0` |
| `PAJE_WORKSPACE_ADAPTER` | `mock` | `mock` or `git` |
| `PAJE_RUNNER_ADAPTER` | `mock` | `mock`, `local`, or `codex` |
| `PAJE_WORKSPACE_ROOT` | OS temp directory | Mirror and worktree storage |
| `PAJE_RUNNER_COMMAND` | `codex` | Local agent executable |
| `PAJE_RUNNER_ARGS` | `["exec"]` | Local-runner arguments inserted before the task prompt |
| `MEM0_API_KEY` | empty | Required when memory is `mem0` |
| `MEM0_BASE_URL` | `https://api.mem0.ai` | Optional Mem0 API origin |

The local runner executes the configured binary directly; it does not invoke a
shell. The task description is appended as the final argument. The Codex
adapter ignores `PAJE_RUNNER_ARGS` and invokes:

```text
codex exec --json --ephemeral --ignore-user-config \
  --sandbox workspace-write <task>
```

It preserves the bounded JSONL transcript, tolerates non-JSON diagnostics,
returns the last completed agent message, and rejects a successful process that
emits no terminal agent response. `--ignore-user-config` prevents ambient user
plugins or hooks from changing an orchestrated run. Authentication can be
supplied explicitly through the runner environment, including `CODEX_HOME`.

Mem0 operations require at least one entity tag in each workflow input:
`user_id`, `agent_id`, `app_id`, or `run_id`. Other tags are persisted and
queried as Mem0 metadata.

## Real adapters

This example enables Mem0, Git worktrees, and Codex:

```bash
export PAJE_MEMORY_ADAPTER="mem0"
export PAJE_WORKSPACE_ADAPTER="git"
export PAJE_RUNNER_ADAPTER="codex"
export MEM0_API_KEY="<mem0-api-key>"
export PAJE_RUNNER_COMMAND="codex"
go run ./cmd/paje
```

The Git adapter maintains one mirror per repository below
`PAJE_WORKSPACE_ROOT` and prepares a detached, ephemeral worktree for every
agent run.

## Container

Build the non-root worker image:

```bash
docker build -t paje:dev .
```

The runtime image contains the Pajé binary, Git, OpenSSH, and CA certificates.
A derived image can add a particular agent CLI for local execution.

## Kubernetes

The Helm chart deploys one outbound-only worker. It does not bundle Hatchet
Server or PostgreSQL; point it at an existing self-hosted Hatchet installation.

Create credentials without placing tokens in Helm command history:

```bash
kubectl create namespace paje
kubectl -n paje create secret generic paje-worker \
  --from-literal=hatchet-client-token="<token>" \
  --from-literal=mem0-api-key="<mem0-api-key>"
```

Install the worker:

```bash
helm upgrade --install paje charts/paje \
  --namespace paje \
  --set secrets.existingSecret=paje-worker \
  --set adapters.memory=mem0 \
  --set adapters.workspace=git \
  --set adapters.runner=codex \
  --set hatchet.hostPort=hatchet-engine.hatchet.svc.cluster.local:7070 \
  --set hatchet.serverURL=http://hatchet-api.hatchet.svc.cluster.local:8080 \
  --set hatchet.tlsStrategy=none
```

Use a derived worker image containing an authenticated Codex CLI when
`adapters.runner=codex`; the base Pajé image intentionally contains only the
worker and Git tooling.

## Guardrails and known gaps

Pajé currently prevents several recurring orchestration failures:

- Git worktrees isolate parallel agents from one another.
- Workflow cleanup runs even after cancellation or stage failure, and local
  runner cancellation terminates the complete agent process group.
- Mem0 context is retrieved before execution and the final result is saved for
  later runs.
- The Codex adapter separates the terminal answer from CLI diagnostics.
- Configuration fails fast for missing Hatchet and Mem0 credentials.
- Tests distinguish code readiness from image and Helm packaging readiness.

The following controls are intentionally still open:

- The initial `paje-agent-run` pipeline still saves only the agent result; the
  durable code-change workflow owns artifact capture, approval, and publication.
- Slack and CLI approval gateway adapters are not implemented yet.
- Hatchet provides the durable execution substrate, but task-specific retry and
  concurrency policies are not configured yet.
- The base container does not bundle or authenticate Codex.

## Current scope

The port surface leaves room for Kubernetes Job runners, Slack/CLI approval
gateways, webhook/CLI/CRD triggers, and more advanced Hatchet workflows. Those
adapters are not part of this initial milestone.
