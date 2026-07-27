# Executors

Executors implement one provider-neutral, one-shot sandbox lifecycle. The
workflow owns attempt identity, cleanup order and durable evidence; adapters
own only provider operations.

## Support matrix

| Executor | Status |
| --- | --- |
| Local Docker Engine | current |
| Host | development only |
| Kubernetes Jobs | planned |

Local Docker is the current portable-worker backend. Set an explicit local
Unix socket and select the Docker executor:

```bash
export PAJE_CODECHANGE_EXECUTOR=docker
export PAJE_DOCKER_ENDPOINT=unix:///var/run/docker.sock
export PAJE_WORKER_PROFILE_DIR=/etc/paje/worker-profiles
export PAJE_SECRET_BINDING_DIR=/etc/paje/secret-bindings
```

Pajé does not mount the Docker socket into workload containers. The
coordinator alone talks to the Engine. Each attempt uses an exact OCI image
digest, non-root user, read-only root, dropped capabilities, bounded CPU,
memory and PIDs, one writable workspace bind, private tmpfs materializations,
disabled logs and attempt-scoped labels.

Lifecycle evidence distinguishes resource creation, bootstrap start,
confirmed declared-child start, terminal completion, destruction and unknown
state. A private receipt binds the exact attempt, command and environment
declaration. Resource creation or provider `running` state never substitutes
for child-start proof. Unknown or ambiguous attempts are fail-closed and never
silently rerun.

The host executor exists for development diagnostics only:

```bash
export PAJE_CODECHANGE_EXECUTOR=host
export PAJE_HOST_EXECUTOR_ENABLED=true
```

Production-only mode rejects it. Kubernetes Job execution is not implemented;
there is no chart value, runtime kind or fallback that enables it.

## Workspace and cleanup

Git workspaces are standalone detached clones inside the single executor bind.
Their `.git` metadata, common directory and objects stay inside that bind; no
alternate, remote credential config or host-absolute Git link is retained.
Cleanup is inode-bound, atomic and resumable. A rebound replacement survives
while the exact owned workspace is quarantined and removed.

Cancellation destroys the sandbox before reverse secret revocation. Recovery
may retry only a conclusively unstarted attempt. Confirmed start, missing
identity, provider ambiguity or unverifiable terminal state blocks replay.

## Live acceptance

Use an explicit local Unix socket. The Docker suite proves container policy,
secret cleanup, tracked-secret artifact denial and coordinator restart. The
Codex gate additionally requires existing authentication and runs the standard
rendered `codex-go@1` profile; once both opt-ins are set, missing prerequisites
are fatal.

```bash
PAJE_DOCKER_ACCEPTANCE=1 \
PAJE_DOCKER_TEST_ENDPOINT=unix:///var/run/docker.sock \
  go test ./internal/acceptance \
  -run 'TestWorkerDocker|TestWorkerRestart' -count=1 -v

PAJE_DOCKER_ACCEPTANCE=1 \
PAJE_CODEX_ACCEPTANCE=1 \
PAJE_DOCKER_TEST_ENDPOINT=unix:///var/run/docker.sock \
  go test ./internal/acceptance \
  -run TestCodexArtifactAcceptance -count=1 -v
```

Do not label another provider current or certified from the Docker result.
