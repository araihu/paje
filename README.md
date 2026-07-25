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

## Requirements

- Go 1.26 or newer for development
- Docker and Helm 3 for packaging
- an existing Hatchet installation
- Git for the real workspace and publisher adapters
- an authenticated Codex CLI or Codex auth Secret when using the Codex runner
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
    "environment_keys": [],
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
    "environment_keys": [],
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
| `PAJE_RUNNER_ADAPTER` | `mock` | `mock`, `local`, or `codex` |
| `PAJE_PUBLISHER_ADAPTER` | `mock` | `mock` or `github` |
| `PAJE_WORKSPACE_ROOT` | `<temp>/paje/workspaces` | repository mirrors and worktrees |
| `PAJE_RUN_ROOT` | `<workspace-root>/runs` | filesystem run store |
| `PAJE_ARTIFACT_ROOT` | `<workspace-root>/artifacts` | filesystem artifact store |
| `PAJE_RUNTIME_ROOT` | `<workspace-root>/runtime` | per-attempt runtime data and publisher credentials |
| `PAJE_ARTIFACT_LIMIT_BYTES` | `10485760` | maximum compressed artifact bytes |
| `PAJE_COMMAND_OUTPUT_LIMIT_BYTES` | `1048576` | maximum captured output per verification command |
| `PAJE_ENV_ALLOWLIST` | `[]` | JSON array of operator-approved non-secret child variable names |
| `PAJE_RUNNER_COMMAND` | `codex` | executable for local/Codex runner |
| `PAJE_RUNNER_ARGS` | `["exec"]` | JSON argument array for the local runner; ignored by Codex runner |
| `MEM0_API_KEY` | empty | required for `mem0`; never passed to agent or verification children |
| `MEM0_BASE_URL` | adapter default | optional Mem0 API origin |
| `GITHUB_TOKEN` | empty | required for GitHub publisher; available only to publisher HTTP/askpass operations |
| `GITHUB_API_URL` | `https://api.github.com` | GitHub API origin |
| `CODEX_HOME` | empty | required for Codex runner; authentication state passed only to agent stage |

Hatchet SDK connection variables include `HATCHET_CLIENT_HOST_PORT`,
`HATCHET_CLIENT_SERVER_URL`, `HATCHET_CLIENT_NAMESPACE`,
`HATCHET_CLIENT_TLS_STRATEGY`, and `HATCHET_CLIENT_LOG_LEVEL`. The Helm values
below render them when set.

`PAJE_ENV_ALLOWLIST` names values that must also exist in the worker process
environment, for example through `extraEnv`. It cannot include Hatchet, Mem0,
Git, SSH, GitHub, `CODEX_HOME`, home/temp, or publisher-managed credential
keys. Baseline path, locale, and certificate variables are selected
automatically.

The beta Helm values are:

| Value | Default | Purpose |
| --- | --- | --- |
| `replicaCount` | `1` | fixed beta replica count; schema rejects any other value |
| `image.repository`, `image.tag`, `image.pullPolicy` | `ghcr.io/araihu/paje`, chart app version, `IfNotPresent` | worker image |
| `imagePullSecrets` | `[]` | private image pull credentials |
| `nameOverride`, `fullnameOverride` | empty | resource naming |
| `serviceAccount.create`, `.automount`, `.annotations`, `.name` | `true`, `false`, `{}`, empty | worker identity; no API token by default |
| `podAnnotations`, `podLabels` | `{}`, `{}` | pod metadata |
| `podSecurityContext` | group `65532` | pod filesystem ownership |
| `securityContext` | non-root UID/GID `65532`, read-only root, no privilege escalation, all capabilities dropped | container boundary |
| `adapters.memory`, `.workspace`, `.runner` | `mock`, `mock`, `mock` | selected adapters |
| `publisher.adapter`, `.githubAPIURL` | `mock`, `https://api.github.com` | publication adapter |
| `workspace.root`, `.sizeLimit` | `/workspace`, `10Gi` | ephemeral data mount |
| `persistence.enabled`, `.existingClaim`, `.size`, `.storageClass`, `.accessModes` | `false`, empty, `10Gi`, empty, `[ReadWriteOnce]` | filesystem durability |
| `limits.artifactBytes`, `.commandOutputBytes` | `10485760`, `1048576` | artifact and command-output limits |
| `environment.allowlist` | `[]` | non-secret child environment names |
| `runner.command`, `.args` | `codex`, `[exec]` | runner executable and local-runner arguments |
| `mem0.baseURL` | empty | optional Mem0 origin |
| `hatchet.hostPort`, `.serverURL`, `.namespace`, `.tlsStrategy`, `.logLevel` | empty, empty, empty, empty, `info` | Hatchet SDK connection |
| `secrets.hatchet`, `.mem0`, `.github` | separate `existingSecret`/`key`/`value` objects | worker service credentials |
| `codexAuth.existingSecret` | empty | required read-only Codex auth seed when runner is `codex` |
| `resources`, `nodeSelector`, `tolerations`, `affinity` | empty | standard pod placement/resources |
| `terminationGracePeriodSeconds` | `60` | bounded worker shutdown |
| `extraEnv` | `[]` | additional worker variables; child access still requires the allowlist |

See `charts/paje/values.yaml` and `charts/paje/values.schema.json` for the exact
shape and schema constraints. Every active Hatchet, Mem0, GitHub, and Codex
credential must use a distinct Secret, including generated service Secret
names.

## Kubernetes deployment

Build the beta image with an auditable source revision:

```bash
PAJE_COMMIT="$(git rev-parse --verify 'HEAD^{commit}')"
docker build \
  --build-arg CODEX_VERSION=0.144.5 \
  --build-arg PAJE_COMMIT="$PAJE_COMMIT" \
  -t paje:beta .
```

`PAJE_COMMIT` has no fallback: the build rejects a missing value or anything
other than a full 40-character lowercase hexadecimal commit. The image includes
Codex CLI 0.144.5 and runs as UID/GID 65532. The chart supports a read-only root
filesystem by mounting writable data, runtime, temp, and Codex-home volumes.

Create separate worker credentials without putting values in shell history or
process arguments. This example reads values silently, stores them only in a
private temporary directory, sends file paths to `kubectl`, and removes the
temporary material immediately:

```bash
kubectl create namespace paje

PAJE_SECRET_DIR="$(mktemp -d)"
chmod 700 "$PAJE_SECRET_DIR"
read -rsp 'Hatchet token: ' HATCHET_CLIENT_TOKEN; printf '\n'
printf '%s' "$HATCHET_CLIENT_TOKEN" >"$PAJE_SECRET_DIR/hatchet-client-token"
unset HATCHET_CLIENT_TOKEN
read -rsp 'Mem0 API key: ' MEM0_API_KEY; printf '\n'
printf '%s' "$MEM0_API_KEY" >"$PAJE_SECRET_DIR/mem0-api-key"
unset MEM0_API_KEY
read -rsp 'GitHub token: ' GITHUB_TOKEN; printf '\n'
printf '%s' "$GITHUB_TOKEN" >"$PAJE_SECRET_DIR/github-token"
unset GITHUB_TOKEN
chmod 600 "$PAJE_SECRET_DIR"/*

kubectl -n paje create secret generic paje-hatchet \
  --from-file=hatchet-client-token="$PAJE_SECRET_DIR/hatchet-client-token"
kubectl -n paje create secret generic paje-mem0 \
  --from-file=mem0-api-key="$PAJE_SECRET_DIR/mem0-api-key"
kubectl -n paje create secret generic paje-github \
  --from-file=github-token="$PAJE_SECRET_DIR/github-token"

rm -f "$PAJE_SECRET_DIR/hatchet-client-token" \
  "$PAJE_SECRET_DIR/mem0-api-key" \
  "$PAJE_SECRET_DIR/github-token"
rmdir "$PAJE_SECRET_DIR"
unset PAJE_SECRET_DIR
```

Seed only Codex authentication files. Do not place Hatchet, Mem0, GitHub, SSH,
or Git credentials in this Secret:

```bash
kubectl -n paje create secret generic paje-codex-auth \
  --from-file=auth.json="${CODEX_HOME:-$HOME/.codex}/auth.json"
```

The Secret is mounted read-only into an init container. It copies the seed into
a private writable `emptyDir`, which becomes `CODEX_HOME`; Codex runtime writes
never mutate the Secret.

Push the exact image under an immutable tag, then install that tag. The
repository and tag below are placeholders for an operator-owned registry:

```bash
PAJE_IMAGE_REPOSITORY='<registry>/paje'
PAJE_IMAGE_TAG="$PAJE_COMMIT"
docker tag paje:beta "$PAJE_IMAGE_REPOSITORY:$PAJE_IMAGE_TAG"
docker push "$PAJE_IMAGE_REPOSITORY:$PAJE_IMAGE_TAG"

helm upgrade --install paje charts/paje \
  --namespace paje \
  --set image.repository="$PAJE_IMAGE_REPOSITORY" \
  --set image.tag="$PAJE_IMAGE_TAG" \
  --set persistence.enabled=true \
  --set adapters.memory=mem0 \
  --set adapters.workspace=git \
  --set adapters.runner=codex \
  --set publisher.adapter=github \
  --set secrets.hatchet.existingSecret=paje-hatchet \
  --set secrets.mem0.existingSecret=paje-mem0 \
  --set secrets.github.existingSecret=paje-github \
  --set codexAuth.existingSecret=paje-codex-auth
```

For a local cluster whose nodes already contain `paje:beta`, use
`--set image.repository=paje --set image.tag=beta --set image.pullPolicy=Never`
instead of pushing. In both cases, verify the deployed image has the expected
`org.opencontainers.image.revision` label.

When `persistence.enabled=true`, the chart mounts one PVC at `/var/lib/paje`
and uses `/var/lib/paje/workspace`, `/var/lib/paje/runs`, and
`/var/lib/paje/artifacts`. `/run/paje`, `/tmp`, and the writable Codex home are
ephemeral and intentionally cleared with the pod.

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
`$HOME/.codex`) through the explicit agent-stage policy:

```bash
PAJE_CODEX_INTEGRATION=1 \
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
