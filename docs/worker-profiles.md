# Worker profiles

Worker profiles are immutable operator-owned declarations for portable worker
execution. A submission selects an exact `name@revision`; Pajé resolves and
persists the canonical snapshot before execution. Mutable tags, implicit
latest revisions, and submission-supplied runtime fields are rejected.

## Support matrix

| Executor | Status | Operator contract |
| --- | --- | --- |
| Local Docker Engine | current | Runs the exact OCI image and profile through the isolated Docker executor. |
| Host | development only | Requires an explicit development opt-in and is rejected in production-only mode. |
| Kubernetes Jobs | planned | Not implemented and not accepted by configuration or the Helm chart. |

The first production profile is `codex-go@1`. Its release template is
`deploy/worker-profiles/codex-go-v1.yaml.tmpl`. The rendered profile binds:

- one immutable image digest and exact Linux platform;
- non-root, read-only-root, outbound-network and bounded resource policy;
- exact Git, Go, Node and Codex harness versions and probes;
- one required agent-stage `harness.codex-auth` directory capability.

The profile registry loads canonical YAML from `PAJE_WORKER_PROFILE_DIR`.
Files must be regular, bounded, non-symlink entries. Duplicate identities,
invalid schemas and non-canonical snapshots fail service construction. A run
persists the exact profile snapshot and digest; a later registry change cannot
rebind it.

## Rendering `codex-go@1`

Publish the worker image first, then replace all three template tokens with
the exact registry repository, `sha256:` digest and Docker Engine platform.
Do not use a mutable image tag in the rendered profile.

```bash
IMAGE_REFERENCE='registry.example/paje-worker-codex-go'
IMAGE_DIGEST='sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'
PLATFORM='linux/amd64'

sed \
  -e "s|\${IMAGE_REFERENCE}|$IMAGE_REFERENCE|g" \
  -e "s|\${IMAGE_DIGEST}|$IMAGE_DIGEST|g" \
  -e "s|\${PLATFORM}|$PLATFORM|g" \
  deploy/worker-profiles/codex-go-v1.yaml.tmpl \
  > /etc/paje/worker-profiles/codex-go-v1.yaml
```

Treat the example digest as a placeholder. Verify the registry manifest and
image revision label before enabling submissions.

## Submission

Portable code-change input must include the exact profile identity:

```json
{
  "repository_uri": "https://github.com/example/service.git",
  "base_ref": "main",
  "profile": "go",
  "worker_profile": "codex-go@1"
}
```

Worker profiles do not carry secret values. They declare only capability,
binding revision, delivery type, target and stage. See
[Worker secrets](worker-secrets.md) for the independent binding contract.
