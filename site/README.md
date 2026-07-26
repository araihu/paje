# Pajé site

The public product and documentation site for Pajé, built with vinext and
published with OpenAI Sites.

## Local development

Node.js 22.13 or newer is required.

```bash
npm install
npm run dev
```

The development server starts at `http://localhost:3000` by default.

## Validation

```bash
npm run build
node --test tests/rendered-html.test.mjs
```

The site is intentionally static and read-only. Product and usage claims should
remain aligned with the root project README and the committed beta design.

The product positioning is deliberately language-neutral: Pajé is implemented
in Go, but the `generic` repository profile accepts structured checks for any
language whose toolchain is available in the worker image. The product model
expects the agent to pilot Pajé through harness hooks and skills, but the
current beta still starts the workflow through Hatchet. Codex is the first
supported harness; agent-side integrations and additional harnesses are planned
behind the same execution boundary.

## Locales

The public catalogs have stable URLs:

- English: `/en`
- Brazilian Portuguese: `/pt-br`
- Spanish: `/es`

The root URL negotiates from `Accept-Language` and redirects to one of those
canonical routes without using IP or geolocation. `pt-BR` and `pt-PT` use the
existing Brazilian Portuguese catalog. Bare `pt` also uses that catalog as a
compatibility choice. Other Portuguese variants, unsupported or malformed
preferences, wildcards that resolve to the default, and missing headers use
English. `es` and regional `es-*` preferences use Spanish.

An explicit locale URL always wins over browser preferences. The visible
language controls keep that choice stable across navigation and preserve the
current query string and fragment when switching catalogs. The root redirect
preserves query strings; browsers retain an existing fragment across the
same-origin redirect.

## Deploy

Authenticate the official Cloudflare CLI, build, and deploy the Worker with its
custom domain:

```bash
wrangler login
npm run build
wrangler deploy
```

The deployment configuration binds the Worker to `paje.araihu.com` as a
Cloudflare custom domain.
