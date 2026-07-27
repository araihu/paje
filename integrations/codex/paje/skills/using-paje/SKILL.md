---
name: using-paje
description: Use when the user explicitly asks to delegate a repository code change to the local Pajé runtime, run the durable code-change workflow, or invoke $using-paje.
---

# Using Pajé

Delegate one durable `code-change@v1` leaf run through the installed `paje-agent`. This integration returns an artifact; it does not provide full control-plane graph orchestration.

## Preflight

Run `paje-agent capabilities`. Continue only when `leaf_submission` is `true`. If `control_plane` is `false`, do not claim that Pajé can plan, review, merge, or supervise a multi-agent graph.

Use the canonical HTTPS Git URL and an exact base ref. For this local installation, bind tags to `user_id: guilhermecastro` and `app_id: araihu-paje`. Use `worker_profile: codex-go@1` and `profile: go` for Go repositories. Use `profile: generic` only with explicit bounded checks.

## Submit Once

Construct strict JSON with these fields:

```json
{
  "task_description": "bounded change and acceptance criteria",
  "repository_uri": "https://github.com/araihu/example.git",
  "base_ref": "main",
  "tags": {"user_id": "guilhermecastro", "app_id": "araihu-paje"},
  "worker_profile": "codex-go@1",
  "profile": "go",
  "publication": {"mode": "artifact"}
}
```

Never include secrets, environment values, shell fragments, origin fields, or an idempotency key. Pull-request publication is unavailable through this client.

Submit exactly once with `paje-agent submit --file <absolute-json-path>` or `--file -` for stdin. Record the returned `run_id`. The client derives and binds idempotency and Codex task identity; retries with the same input are safe, but polling must never resubmit.

## Observe

Use `paje-agent wait --run <run_id> --timeout 30m`. For shorter checks use `paje-agent status --run <run_id>`. Cancel only when the user asks or the delegated work is no longer wanted: `paje-agent cancel --run <run_id>`.

Accept success only when the run and nested result are both `succeeded`, every returned run ID matches, and the artifact has a hexadecimal SHA-256 digest and positive size. Report the run ID, artifact digest/size, and verification results. Treat authentication, authorization, conflict, unavailable, timeout, canceled, and workflow-failed exits as distinct outcomes; do not bypass `paje-agent` with service APIs or dashboard credentials.
