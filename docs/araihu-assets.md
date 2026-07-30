# Arai Hu fallback assets

`araihu-assets.json` pins immutable `araihu/assets`. It allowlists Pajé theme
plus logo/icon plate and transparent fallbacks. `site/generator/araihu.css` is
authoritative; `site/public/araihu.css` is generated, never a second source.

Run `go run ./cmd/araihu-assets-update -repo . -archive release.tar.gz` only
when archive SHA-256 matches manifest. Updater verifies release.json, catalog
roles, file hashes, confinement and symlinks; stage/syncs writes, applies
fallbacks first, manifest last, reverses rollback on failure.

Workflow accepts exact six keys: `assets_repository`, `assets_revision`,
`release`, `release_url`, `release_sha256`, `release_json_sha256`; one download,
versioned PR, existing labels only, no auto-merge. Seasonal SRI/order remains
committed; build never fetches mutable current channel.
