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
