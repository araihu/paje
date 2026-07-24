# Task 7 report — Git change capture and built-in policy

## Implemented

- Added a shell-free Git capturer that validates prepared worktrees and full
  object IDs before passing them to Git. Capture uses a private `0600`
  temporary index outside the worktree, a minimal allowlisted environment, and
  bounded stdout/diagnostic buffers. The source index is checked for staged
  changes and checksum-verified before and after capture.
- Captured patches use Git binary/full-index output and include text, delete,
  rename, untracked, executable, symlink, and NUL-containing binary changes.
  NUL-delimited raw changes are normalized, mode-validated, and sorted into
  Task 6 `artifact.Change` records.
- Added exact application through a private `0600` patch file and
  `git apply --check --index --binary`, followed by application to the target
  index. Both the target index tree and an independent temporary-index
  filesystem reconstruction must equal the recorded tree SHA.
- Added the non-overridable change policy. It denies unsafe paths, gitlinks and
  unsupported modes, sensitive credential file names, private-key headers,
  GitHub token formats, and secret-like assignments. Findings are sorted and
  only expose stable rule IDs, normalized paths, and line numbers.

## TDD evidence

### RED

After adding the integration and policy tests, before production packages
existed:

```text
go test ./internal/artifact/gitcapture ./internal/policy -v
```

failed as expected because both directories had no non-test Go files.

### GREEN

```text
go test ./internal/artifact/gitcapture ./internal/policy -v
go test -race ./internal/artifact/gitcapture ./internal/policy
go test ./...
```

all passed. The integration test proves source-index non-mutation and exact
tree/stage reproduction in real detached worktrees. Policy tests cover unsafe
paths, gitlinks, sensitive files, secret patterns without value leakage,
binary/rename patch handling, ordinary changes, and deterministic findings.

## Self-review

Reviewed the exact Task 7 diff and ran `git diff --check`. Temporary indexes
and patch files are created outside worktrees with mode `0600`, subprocesses
receive no inherited environment, every Git argument is validated before use,
and captured diagnostics are bounded to 4096 bytes. The policy only returns
metadata-safe findings; durable artifact persistence remains downstream of the
policy gate in the workflow task.
