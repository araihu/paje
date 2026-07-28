# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Engineering teams evaluating or operating durable AI-agent code-change workflows. They need to understand how an agent can progress from a request to a verified artifact or draft pull request without losing durable state.

## Product Purpose

Pajé is a self-hosted, Go-native orchestration worker for durable AI-agent workflows. Its public site explains the beta product and gives visitors a path to the repository documentation.

## Positioning

Pajé keeps workflow definitions, durable state, artifacts, approval, verification, and publication behind provider-neutral Go ports while a harness pilots the work through hooks and skills.

## Operating Context

The beta ships the `code-change@v1` workflow through Hatchet. Artifact mode is default; pull-request mode adds artifact-bound approval and idempotent draft GitHub publication. Codex is the first supported harness.

## Capabilities and Constraints

The public site is static, read-only, and published in English, Brazilian Portuguese, and Spanish. Root requests negotiate `Accept-Language` and redirect to the matching canonical locale. Product claims must remain repository-language-neutral: a generic profile accepts structured checks for any language whose toolchain is in the worker image.

## Brand Commitments

Pajé name and supplied marks are preserved. Current public identity uses a light paper ground, dark ink, precise blue action color, and a documentary workflow-register. This record is inferred from the incumbent site and committed assets.

## Evidence on Hand

Root `README.md` contains beta workflow facts. Existing translated copy and metadata are in `site/app/i18n/catalogs.ts`; supplied marks and social image are in `site/public/`.

## Product Principles

- Agent autonomy needs durable, inspectable workflow state.
- A repository profile should remain language-neutral.
- Approval and publication must bind to verified artifacts and be idempotent.
- The public explanation must distinguish current beta support from planned support.
