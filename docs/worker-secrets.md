# Worker secrets

Portable workers receive secrets through exact operator-owned bindings and a
short-lived broker lease. Submissions cannot provide secret values, provider
references, environment targets or filesystem paths.

Each worker-profile requirement binds all of these fields:

- capability name and exact binding revision;
- worker profile identity;
- lifecycle stage;
- delivery kind (`environment`, `file` or `directory`);
- exact target;
- required or optional status.

The secret-binding registry must authorize the same tuple. Any mismatch fails
before executor side effects. The broker acquires only the current stage's
requirements, materializes bounded values privately, and revokes in reverse
order after the sandbox is destroyed. Cleanup failure is terminal and cannot
be overwritten by a successful or canceled workload result.

## Codex authentication

`codex-go@1` requires `harness.codex-auth` revision `1` as a directory at
`/run/paje/secrets/codex`. The Codex harness derives only this non-secret child
declaration:

```text
CODEX_HOME=/run/paje/secrets/codex
```

The binding source can be an operator-controlled filesystem directory
containing `auth.json`:

```yaml
api_version: paje.araihu.com/v1alpha1
kind: SecretBindings
bindings:
  harness.codex-auth:
    revision: 1
    authorize:
      profile: codex-go@1
      stage: agent
      delivery: directory
      target: /run/paje/secrets/codex
    source:
      provider: filesystem
      reference: /etc/paje/secrets/codex
```

Configure the provider root and binding catalog separately:

```bash
export PAJE_SECRET_FILESYSTEM_ROOTS='["/etc/paje/secrets"]'
export PAJE_SECRET_BINDING_DIR=/etc/paje/secret-bindings
```

Keep the source directory owner-only. Symlinks, special files, traversal,
oversized trees, duplicate targets and insecure permissions fail closed.

## Durable evidence

Artifacts and run records store capability references, exact profile/binding
revision, attempt identities, and confirmed environment **key names** only.
They never store secret bytes, environment values, provider credentials or
host source paths. Capture policy scans raw and common encoded forms before an
artifact can be committed. A detected tracked secret terminally fails the run,
deletes the candidate workspace and leaves no artifact reference.
