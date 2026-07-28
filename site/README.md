# Pajé site

The public product and documentation site for Pajé. A small Go generator uses
Goshtoso's version-matched dependencies and components to write static locale
documents; a Cloudflare Worker only negotiates the root locale and serves those
files. Published with OpenAI Sites.

## Local development

Go 1.26.5 or newer and Node.js 22.13 or newer are required.

```bash
npm install
npm run build
```

`npm run build` regenerates `public/en`, `public/pt-br`, and `public/es`, then
packages the static Worker and asset tree. `npm run dev` serves the generated
files locally after one build.

The page uses Goshtoso's supported minimal dependency component with Alpine and
HTMX disabled because the generated documents contain no interactive widgets.
Goshtoso v0.0.13 does not expose a CSS-only head option, so the minimal
component still emits its small first-party `combobox.js` helper; removing that
last script requires an upstream Goshtoso API rather than hand-written tags.

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
language controls link directly to the canonical locale routes. The root
redirect preserves query strings; browsers retain an existing fragment across
the same-origin redirect.

## Deploy

Authenticate the official Cloudflare CLI, build, and deploy the Worker with its
custom domain:

```bash
wrangler login
npm run build
npm run deploy
```

The deployment configuration binds the Worker to `paje.araihu.com` as a
Cloudflare custom domain.

Production deployment is automated by
`.github/workflows/deploy-site.yml`. Pull requests that touch the site run its
lint and test gates. A push to `main` after those gates deploys with Wrangler;
the workflow can also be started manually.

The repository must provide:

- Actions variable `CLOUDFLARE_ACCOUNT_ID` with the target account ID.
- Actions secret `CLOUDFLARE_API_TOKEN` with `Workers Scripts: Edit` and, for
  the configured custom domain, `Workers Routes: Edit` on the relevant zone.

The token is passed only to the deploy job. Pull-request validation does not
receive Cloudflare credentials.
