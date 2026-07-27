# Pajé

Pajé is a self-hosted, Go-native orchestration worker for durable AI-agent
workflows. Hatchet supplies queueing, retries, concurrency, signals, and
workflow visibility; Pajé keeps template definitions, durable state, artifacts,
approval, verification, and publication behind provider-neutral Go ports.

The beta workflow is the built-in `code-change@v1` template, registered with
Hatchet as `paje-code-change-v1`:

```text
resolve -> execute -> approval -> publish -> finalize
```

Artifact mode is the default. It resolves an immutable revision, retrieves a
scoped memory snapshot, runs Codex in an isolated Git worktree, verifies every
selected module, and persists a content-addressed artifact. Pull-request mode
adds artifact-bound approval and idempotent draft GitHub publication.

## Portable worker support

| Executor | Support |
| --- | --- |
| Local Docker Engine | current |
| Host | development only |
| Kubernetes Jobs | planned |

The Helm chart deploys the coordinator only. Its default portable catalogs are
empty and read-only, and the code-change executor is `mock`; it mounts no
Docker socket and creates no workload `Job`. Configure Local Docker directly
on a trusted host when enabling portable worker execution. See
[worker profiles](docs/worker-profiles.md),
[worker secrets](docs/worker-secrets.md), and
[executors](docs/executors.md).

## Requirements

- Go 1.26 or newer for development
- Docker and Helm 3 for packaging
- an existing Hatchet installation
- Git for the real workspace and publisher adapters
- an authenticated Codex auth directory when using `codex-go@1`
- Mem0 and GitHub credentials only when those adapters are selected

## Workflow input

Start Hatchet workflow `paje-code-change-v1` with a thin outer envelope. Generate
`run_id` once for the logical trigger and reuse it for transport retries. The
nested `input` is the exact `code-change@v1` JSON object; it is unwrapped before
provider-neutral validation. Unknown envelope or template fields, unsupported
versions, arbitrary environment values, and shell command fragments are
rejected. `tags.user_id` and `tags.app_id` are required. A Go profile with no
explicit `checks` discovers every tracked `go.mod` and runs `go test ./...` in
each module with `GOWORK=off`.

Artifact example:

```json
{
  "run_id": "73f659b2-eaee-4c37-8784-48fab274b3e8",
  "input": {
    "idempotency_key": "change-api-timeout-20260725",
    "task_description": "Raise the client timeout and update its tests.",
    "repository_uri": "https://github.com/example/service.git",
    "base_ref": "main",
    "memory_query": "service client timeout conventions",
    "memory_limit": 5,
    "tags": {
      "user_id": "operator@example.com",
      "app_id": "service"
    },
    "profile": "go",
    "worker_profile": "codex-go@1",
    "publication": {
      "mode": "artifact"
    }
  }
}
```

Draft pull-request example:

```json
{
  "run_id": "c7319354-a3d5-4734-a15d-c91a3b4a00ba",
  "input": {
    "idempotency_key": "change-api-timeout-pr-20260725",
    "task_description": "Raise the client timeout and update its tests.",
    "repository_uri": "https://github.com/example/service.git",
    "base_ref": "main",
    "memory_query": "service client timeout conventions",
    "memory_limit": 5,
    "tags": {
      "user_id": "operator@example.com",
      "app_id": "service"
    },
    "profile": "go",
    "worker_profile": "codex-go@1",
    "publication": {
      "mode": "pull_request",
      "provider": "github",
      "target_branch": "main",
      "title": "Raise the API client timeout",
      "draft": true
    }
  }
}
```

`checks` may replace the Go default with shell-free command specifications. A
check has `name`, repository-relative `directory`, one `executable`, an `args`
array, a Go-duration `timeout`, and `required`. `module_exclusions` requires an
exact discovered module path and a non-empty reason; each exclusion becomes an
approval warning.

## Durable phases and locations

The five Hatchet tasks exchange only a run ID and durable references:

1. `resolve` validates and canonicalizes input, reserves the idempotency key,
   resolves `base_ref` to a 40-character commit, builds child capabilities,
   retrieves memory, and persists the snapshot.
2. `execute` prepares a fresh worktree at the resolved commit, runs the
   repository profile, Codex, verification, policy checks, and artifact
   capture, then removes the per-attempt worktree and runtime directory.
3. `approval` is durably skipped in artifact mode. Pull-request mode waits for
   an approval event bound to the run and artifact digest.
4. `publish` is durably skipped in artifact mode. Pull-request mode reapplies
   the artifact, reruns required checks, and creates or reuses the deterministic
   branch, commit, and draft pull request.
5. `finalize` saves one run-scoped outcome memory and returns the durable
   `code-change@v1` result.

With the filesystem adapters, run records are stored at
`$PAJE_RUN_ROOT/runs/<run-id>.json`, idempotency bindings under
`$PAJE_RUN_ROOT/idempotency/`, and artifacts at
`$PAJE_ARTIFACT_ROOT/sha256/<first-two-digest-characters>/<digest>.tar.gz`.
Repository mirrors and ephemeral worktrees are below `PAJE_WORKSPACE_ROOT`.
Per-attempt home and temporary directories are below `PAJE_RUNTIME_ROOT` and
are removed when the attempt ends.

Artifact archives have this fixed layout:

```text
manifest.json
changes.patch
agent-output.txt
execution.json
verification.json
preflight.json
warnings.json
```

The reference digest authenticates a canonical uncompressed tar stream; gzip
is only a storage encoding. The manifest binds the run, template, repository,
base commit, resulting Git tree, changed paths, memory IDs, and every member
digest.

## Approval event

For pull-request mode, send a Hatchet event with this exact key:

```text
paje:approval:<run-id>
```

The event body is an `approval.Result` JSON object. `run_id` and
`artifact_digest` must exactly match the approval request, `actor` must be
non-empty, and `decided_at` must be UTC. A decline requires a non-empty reason.

```json
{
  "run_id": "01J00000000000000000000000",
  "artifact_digest": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "approved": true,
  "actor": "operator@example.com",
  "decided_at": "2026-07-25T15:00:00Z",
  "reason": "Reviewed artifact and required checks"
}
```

Approval is immutable and artifact-bound. Changing the run, artifact digest,
base commit, target branch, or publication mode invalidates it. A declined run
finishes as `declined`; Pajé does not call the publisher.

## Idempotency and conflict behavior

Hatchet uses the outer `run_id` as status-based trigger idempotency. The key is
held until the workflow becomes terminal, with a 30-day fallback cap for the
maximum supported beta run lifetime. The first resolve preallocates that exact
durable Pajé run ID; a different Hatchet owner cannot attach to or exhaust its
active stages.

The nested `idempotency_key` remains scoped to `code-change@v1` and bound to a
canonical hash of the complete validated input. Reusing the same outer
`run_id`, key, and input resumes the same run. Reusing a nested key with a
different outer owner or changed input returns an idempotency conflict.
Omitting the nested key preserves the provider-neutral behavior: each newly
generated outer `run_id` allocates a distinct run.

Artifacts are immutable and content addressed. GitHub publication uses branch
`paje/code-change/<run-id>` and a deterministic commit containing these
trailers:

```text
Paje-Run-ID: <run-id>
Paje-Base-SHA: <base-sha>
Paje-Artifact-Digest: <artifact-digest>
```

A retry reuses the branch, commit, and pull request only when all bindings
match. An existing branch, commit, or pull request with different run, base,
artifact, tree, target, or head state is a non-retryable conflict. Pajé never
force-pushes, pushes the target branch, merges, closes, or deletes provider
state.

## Configuration

Pajé reads these worker variables directly:

| Variable | Default | Contract |
| --- | --- | --- |
| `HATCHET_CLIENT_TOKEN` | required | Hatchet worker authentication; never passed to agent or verification children |
| `PAJE_MEMORY_ADAPTER` | `mock` | `mock` or `mem0` |
| `PAJE_WORKSPACE_ADAPTER` | `mock` | `mock` or `git` |
| `PAJE_PUBLISHER_ADAPTER` | `mock` | `mock` or `github` |
| `PAJE_CODECHANGE_EXECUTOR` | `mock` | `mock`, `docker`, or explicitly enabled development-only `host` |
| `PAJE_WORKER_PROFILE_DIR` | required | read-only canonical worker-profile catalog |
| `PAJE_SECRET_BINDING_DIR` | required | read-only canonical secret-binding catalog |
| `PAJE_DOCKER_ENDPOINT` | empty | explicit local Unix socket for the Docker executor |
| `PAJE_DOCKER_REGISTRY_AUTH_FILE` | empty | bounded regular registry-auth file used only by the Docker adapter |
| `PAJE_HOST_EXECUTOR_ENABLED` | `false` | explicit development-only host executor opt-in |
| `PAJE_WORKSPACE_ROOT` | `<temp>/paje/workspaces` | repository mirrors and worktrees |
| `PAJE_RUN_ROOT` | `<workspace-root>/runs` | filesystem run store |
| `PAJE_ARTIFACT_ROOT` | `<workspace-root>/artifacts` | filesystem artifact store |
| `PAJE_RUNTIME_ROOT` | `<workspace-root>/runtime` | per-attempt runtime data and publisher credentials |
| `PAJE_ARTIFACT_LIMIT_BYTES` | `10485760` | maximum compressed artifact bytes |
| `PAJE_COMMAND_OUTPUT_LIMIT_BYTES` | `1048576` | maximum captured output per verification command |
| `PAJE_SECRET_FILESYSTEM_ROOTS` | `[]` | JSON array of exact provider roots |
| `PAJE_SECRET_PROVIDER_MAX_BYTES`, `PAJE_SECRET_PROVIDER_MAX_ENTRIES` | bounded defaults | materialization limits |
| `MEM0_API_KEY` | empty | required for `mem0`; never passed to agent or verification children |
| `MEM0_BASE_URL` | adapter default | optional Mem0 API origin |
| `GITHUB_TOKEN` | empty | required for GitHub publisher; available only to publisher HTTP/askpass operations |
| `GITHUB_API_URL` | `https://api.github.com` | GitHub API origin |

Hatchet SDK connection variables include `HATCHET_CLIENT_HOST_PORT`,
`HATCHET_CLIENT_SERVER_URL`, `HATCHET_CLIENT_NAMESPACE`,
`HATCHET_CLIENT_TLS_STRATEGY`, and `HATCHET_CLIENT_LOG_LEVEL`. The Helm values
below render them when set.

Portable child environments are derived from the fixed baseline, exact harness
declarations and broker-owned secret requirements. Submissions cannot add
environment keys or values.

The beta Helm values are:

| Value | Default | Purpose |
| --- | --- | --- |
| `replicaCount` | `1` | fixed beta replica count; schema rejects any other value |
| `image.repository`, `image.tag`, `image.pullPolicy` | `ghcr.io/araihu/paje`, chart app version, `IfNotPresent` | coordinator image |
| `imagePullSecrets` | `[]` | private image pull credentials |
| `nameOverride`, `fullnameOverride` | empty | resource naming |
| `serviceAccount.create`, `.automount`, `.annotations`, `.name` | `true`, `false`, `{}`, empty | worker identity; no API token by default |
| `podAnnotations`, `podLabels` | `{}`, `{}` | pod metadata |
| `podSecurityContext` | group `65532` | pod filesystem ownership |
| `securityContext` | non-root UID/GID `65532`, read-only root, no privilege escalation, all capabilities dropped | container boundary |
| `adapters.memory`, `.workspace` | `mock`, `mock` | selected coordinator adapters |
| `codeChange.executor` | `mock` | fail-closed chart default; no in-pod Docker access |
| `publisher.adapter`, `.githubAPIURL` | `mock`, `https://api.github.com` | publication adapter |
| `workspace.root`, `.sizeLimit` | `/workspace`, `10Gi` | ephemeral data mount |
| `persistence.enabled`, `.existingClaim`, `.size`, `.storageClass`, `.accessModes` | `false`, empty, `10Gi`, empty, `[ReadWriteOnce]` | filesystem durability |
| `limits.artifactBytes`, `.commandOutputBytes` | `10485760`, `1048576` | artifact and command-output limits |
| `mem0.baseURL` | empty | optional Mem0 origin |
| `hatchet.hostPort`, `.serverURL`, `.namespace`, `.tlsStrategy`, `.logLevel` | empty, empty, empty, empty, `info` | Hatchet SDK connection |
| `secrets.hatchet`, `.mem0`, `.github` | separate `existingSecret`/`key`/`value` objects | worker service credentials |
| `resources`, `nodeSelector`, `tolerations`, `affinity` | empty | standard pod placement/resources |
| `terminationGracePeriodSeconds` | `60` | bounded worker shutdown |
| `extraEnv` | `[]` | additional coordinator variables; never implicit child input |

See `charts/paje/values.yaml` and `charts/paje/values.schema.json` for the exact
shape and schema constraints. Every active Hatchet, Mem0, and GitHub
credential must use a distinct Secret, including generated service Secret
names. Portable worker profiles and bindings are intentionally empty in the
chart default and mounted read-only.

## Kubernetes coordinator deployment

The Helm chart installs the Pajé coordinator, not a Kubernetes workload
runner. It renders one Deployment, no `Job`, no Docker socket, no worker auth
mount and no executable worker profile. The empty profile and binding catalogs
are mounted read-only, so portable submissions fail closed until an operator
chooses a supported executor outside this chart.

```bash
helm upgrade --install paje charts/paje \
  --namespace paje --create-namespace \
  --set persistence.enabled=true \
  --set secrets.hatchet.existingSecret=paje-hatchet
```

When `persistence.enabled=true`, the chart mounts one PVC at `/var/lib/paje`
and uses `/var/lib/paje/workspace`, `/var/lib/paje/runs`, and
`/var/lib/paje/artifacts`. `/run/paje` and `/tmp` remain ephemeral. Kubernetes
Job execution is planned, not current support.

## Inspect and reapply an artifact

Inspect the durable record and archive without executing repository code:

```bash
RUN_ID='<run-id>'
RUN_FILE="$PAJE_RUN_ROOT/runs/$RUN_ID.json"
jq . "$RUN_FILE"

DIGEST="$(jq -r '.artifact.digest' "$RUN_FILE")"
ARCHIVE="$PAJE_ARTIFACT_ROOT/sha256/${DIGEST%${DIGEST#??}}/$DIGEST.tar.gz"
tar -tzf "$ARCHIVE"
tar -xOzf "$ARCHIVE" manifest.json | jq .
tar -xOzf "$ARCHIVE" verification.json | jq .
```

To reproduce it, create a fresh checkout at the recorded base commit, extract
the archive elsewhere, apply the binary patch to the index, and compare the Git
tree to `manifest.tree_sha`:

```bash
BASE_SHA="$(jq -r '.base_sha' "$RUN_FILE")"
REPOSITORY="$(jq -r '.repository_uri' "$RUN_FILE")"
ARTIFACT_DIR="$(mktemp -d)"
WORKTREE_DIR="$(mktemp -d)"
tar -xzf "$ARCHIVE" -C "$ARTIFACT_DIR"
git clone --no-checkout "$REPOSITORY" "$WORKTREE_DIR"
git -C "$WORKTREE_DIR" checkout --detach "$BASE_SHA"
git -C "$WORKTREE_DIR" apply --index --binary "$ARTIFACT_DIR/changes.patch"
test "$(git -C "$WORKTREE_DIR" write-tree)" = \
  "$(jq -r '.tree_sha' "$ARTIFACT_DIR/manifest.json")"
```

The GitHub publisher performs additional patch-path, base/index, filesystem
tree, trusted-repository, credential, and remote-binding checks before it
publishes. Manual reproduction is inspection evidence, not authorization to
push the result.

## Development and acceptance

Run the local suite and build:

```bash
go fmt ./...
go vet ./...
go test -race ./...
go build ./cmd/paje
```

Acceptance tests skip with explicit messages unless opted in:

```bash
go test ./internal/acceptance -v
```

After either live opt-in is set, every missing executable, authentication file,
or required variable is a fatal failure; an opted-in acceptance test never
silently skips.

Authenticated Codex acceptance uses the existing `CODEX_HOME` (or
`$HOME/.codex`) as a real broker source, builds and publishes the standard
worker image, renders exact `codex-go@1`, and runs it through the real local
Docker executor. Once both opt-ins are present, missing Docker or auth
prerequisites are fatal; the test never converts them to a skip.

```bash
PAJE_DOCKER_ACCEPTANCE=1 \
PAJE_CODEX_ACCEPTANCE=1 \
PAJE_DOCKER_TEST_ENDPOINT=unix:///var/run/docker.sock \
  go test ./internal/acceptance \
  -run TestCodexArtifactAcceptance -v -count=1
```

GitHub acceptance must target a private, dedicated acceptance-only repository.
Supply the token only to the test process; do not print or persist it:

```bash
PAJE_GITHUB_ACCEPTANCE=1 \
PAJE_GITHUB_TOKEN="$(gh auth token)" \
PAJE_GITHUB_TEST_REPOSITORY="https://github.com/example/paje-acceptance.git" \
PAJE_GITHUB_TEST_BASE_REF="main" \
PAJE_GITHUB_TEST_RUN_ID="paje-beta-acceptance-20260725" \
  go test ./internal/acceptance \
  -run TestGitHubPublicationAcceptance -v -count=1
```

The test creates or reuses only
`paje/code-change/$PAJE_GITHUB_TEST_RUN_ID`, its deterministic commit, and one
open draft pull request. It verifies the base ref is unchanged and never
merges, closes, deletes, or force-pushes.

Docker build-argument validation is separately opt-in and exercises the real
Dockerfile validation stage:

```bash
PAJE_DOCKER_ACCEPTANCE=1 \
  go test ./internal/acceptance \
  -run TestDockerRevisionBuildArgumentValidation -v -count=1
```

Optional Kubernetes API validation requires an explicitly verified
non-production context:

```bash
test "$PAJE_KUBE_ACCEPTANCE" = 1
PAJE_KUBE_CONTEXT='<explicitly-approved-non-production-context>'
test "$(kubectl config current-context)" = "$PAJE_KUBE_CONTEXT"
kubectl apply --dry-run=server -f /tmp/paje-beta.yaml
```

Do not run the server-side dry-run merely because a current context exists.

## Security boundary

- Before reading configuration, the worker installs an OS process-inspection
  guard: the packaged Linux worker becomes non-dumpable. Together with the
  chart's dropped capabilities and `noNewPrivileges`, this prevents same-UID
  agent and verification descendants from reading the credential-bearing
  parent through process inspection. The worker fails closed on non-Linux
  platforms or if the guard cannot be installed.
- Codex receives a minimal platform environment, a fresh per-attempt home/temp
  tree, explicit `CODEX_HOME`, and operator-approved non-secret keys only.
- Verification receives neither Codex auth nor Hatchet, Mem0, GitHub, Git, or
  SSH credentials and always runs Go commands with `GOWORK=off`.
- The publisher is the only component configured with GitHub credentials. It
  prepares token-bearing operations only after repository-controlled
  verification, from a fresh publisher-owned, validated bare repository with
  credential helpers, URL rewrites, proxies, hooks, redirects, and ambient Git
  configuration disabled.
- Durable records and artifacts store environment key names and redaction
  markers, never environment values. Diagnostics are bounded and sanitized.
- The publisher uses a normal non-force push and never mutates the target
  branch.

## Beta boundaries and non-goals

The filesystem run/artifact stores use process-local compare-and-swap. Beta is
therefore limited to exactly one worker replica and one writable filesystem
installation. Enable persistence for durable work; an ephemeral pod loses run
records, artifacts, repository mirrors, and outcome state when it is replaced.

The following are explicit non-goals for this beta:

- a repository-owned YAML workflow DSL
- automatic merge or direct target-branch pushes
- tags and releases
- GitOps or monitoring workflows
- Forgejo publication
- Kubernetes Job runners
- multiple worker replicas

Later typed templates may reuse the provider-neutral phase contracts without
expanding `code-change@v1` or weakening these boundaries.
