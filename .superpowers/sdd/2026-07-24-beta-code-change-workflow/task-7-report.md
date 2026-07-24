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

## Fix Round 1

### Regressions and fixes

- Replaced prefix-only patch scanning with a unified-diff state machine. It
  enters hunk state only at `@@`, scans every real added line (including source
  content that itself starts with `+`), and never scans binary sections.
  Private-key detection is bounded and matches headers embedded in quoted or
  indented added text.
- Git path association decodes quoted patch paths, mode findings use `OldPath`
  for renames, all finding paths are safe/non-empty, findings are sorted and
  deduplicated, and cancellation returns a deterministic policy denial.
- Workspace handling now rejects `.git` and index symlinks, validates linked
  worktree back-links, confirms Git's toplevel, snapshots the Git executable
  environment at construction, and configures subprocess process groups so
  cancellation kills descendants.
- Operation-private `0700` directories are created alongside, but outside, the
  workspace; indexes and patch files inside remain `0600`. Cleanup runs with a
  non-canceled bounded context and its error is joined into the operation
  result.
- Capture preserves gitlinks for the policy layer, enables rename/copy
  discovery, parses and cross-validates raw/name-status/stage NUL streams, and
  accepts only exact 40- or 64-character lowercase object IDs.
- Apply rejects any porcelain state, validates patch paths, proves the patch in
  a temporary index and expected tree before it can touch the real worktree,
  then retains independent post-apply verification.

### RED evidence

Before the fixes, focused regressions failed for plus-prefixed added secrets,
indented PEM headers, quoted paths, rename old-mode findings, duplicate
findings, untracked Apply mutation, expected-tree mismatch mutation, `.git` /
index symlinks, and gitlink capture. The policy command reported each expected
failure directly; the Git command returned the pre-fix tree-mismatch and
unsafe-mode behavior.

### Verification

```text
go test ./internal/artifact/gitcapture ./internal/policy -v
go test -race ./internal/artifact/gitcapture ./internal/policy
go test ./...
```

All pass, including regressions for descendant cancellation, temporary
directory cleanup, NUL-stream cross-validation, 41-character SHA rejection,
and no-mutation Apply failures.

## Fix Round 2

### Implemented

- Corrected unified-diff parsing so `+++ ` is a file header only before a
  hunk. Inside a hunk every line beginning with `+` is scanned as added source
  content, including physical `+++ ` lines.
- Removed the per-line 16 KiB policy-search truncation. Patch capture is
  already globally bounded, so secret patterns now scan the complete added
  line without a scanner token limit.

### RED and verification

Both new policy regressions failed before the fix: a physical triple-plus
added secret was treated as a header, and a secret after a 17 KiB added prefix
was missed. They now pass with:

```text
go test ./internal/policy -v
go test -race ./internal/artifact/gitcapture ./internal/policy
go test ./...
```

### Fix Round 2 completion

- Replaced `os.RemoveAll` with a bounded, context-aware private-tree traversal
  that checks cancellation before traversal and each removal, does not follow
  symlinks, and joins independent cleanup failures. A live wrapper test observes
  the real `0700` sibling directory and `0600` index outside the workspace
  before proving removal; deterministic injected failures prove cancellation
  stops traversal and multiple cleanup errors remain discoverable.
- Enabled `--find-copies-harder` consistently for patch, raw, and name-status
  capture. Raw new object IDs are retained long enough to validate their exact
  40/64-character form and cross-check them with `ls-files --stage` object IDs
  and modes. The real integration fixture now asserts the complete
  `Result.Changes` matrix for rename, unchanged-source copy, tab/newline/UTF-8
  paths, old paths, and old/new modes, then reproduces exact stage entries.
- Replaced diff-header-only validation with a stateful parser for every
  effective path in `diff --git`, `---`, `+++`, `rename from/to`, and
  `copy from/to`. It decodes quoted Git paths, enforces paired `/dev/null`
  markers and cross-header consistency, ignores path-shaped hunk content, and
  runs before the first Git command.
- Made real application transactional across cancellation and failures. After
  temporary-index proof and a caller-context check, the mutation and
  post-verification run under a 30-second context derived from
  `context.WithoutCancel`. Cancellation, apply failure, timeout, tree mismatch,
  or post-verification failure triggers bounded non-canceled `reset --hard`
  plus `clean -fdx`, followed by base/index/worktree and lock verification; all
  primary, restore, verification, and cleanup errors are joined.
- Preserved the physical `+++` and greater-than-16-KiB secret regressions from
  the first part of this round.

#### TDD evidence

RED failures observed before the corresponding implementation changes:

```text
go test ./internal/artifact/gitcapture -run 'TestCleanupTree' -count=1
# undefined: defaultCleanupOps; undefined: cleanupTree

go test ./internal/artifact/gitcapture -run '^TestCaptureAndApplyReproduceEveryGitChangeWithoutMutatingSourceIndex$' -count=1 -v
# patch does not retain unchanged-source copy metadata

go test ./internal/artifact/gitcapture -run '^TestCrossValidateRejectsMalformedAndMismatchedObjectIDs$' -count=1 -v
# malformed matching values: error = <nil>, want ErrInvalidRequest

go test ./internal/artifact/gitcapture -run 'TestValidatePatchPaths|TestApplyValidatesEveryPatchPathBeforeInvokingGit' -count=1 -v
# unsafe ---/+++/rename/copy paths returned nil and Git was invoked first

go test ./internal/artifact/gitcapture -run '^TestApplyCancellationAfterRealApplyRestoresPristineTarget$' -count=1 -v
# Apply() error = git apply: signal: killed, want context.Canceled joined after rollback
```

GREEN and timing evidence:

```text
go test ./internal/artifact/gitcapture -run 'TestApplyCancellationAfterRealApplyRestoresPristineTarget|TestApplyRestoresTargetAfterApplyOrPostVerifyFailure' -count=10
ok github.com/araihu/paje/internal/artifact/gitcapture 18.840s

go test ./internal/artifact/gitcapture ./internal/policy -count=1 -v
PASS (all focused capture and policy regressions)

go test -race ./internal/artifact/gitcapture ./internal/policy
ok github.com/araihu/paje/internal/artifact/gitcapture 5.654s
ok github.com/araihu/paje/internal/policy 1.679s

go test ./...
PASS (all packages)
```

Files changed: `internal/artifact/gitcapture/capture.go`,
`internal/artifact/gitcapture/capture_test.go`, and
`internal/artifact/gitcapture/capture_internal_test.go`.
