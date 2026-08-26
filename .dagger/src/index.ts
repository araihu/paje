import {
  argument,
  Container,
  dag,
  Directory,
  File,
  func,
  object,
  Secret,
} from "@dagger.io/dagger"

const GO_IMAGE =
  "golang:1.27.0-bookworm@sha256:484ef6066fa69acb059fdfeda7ba2b8f7391f2ef6abc6f9b8411e669ebd56466"
const NODE_IMAGE =
  "node:22.13.0-bookworm-slim@sha256:f5a0871ab03b035c58bdb3007c3d177b001c2145c18e81817b71624dcf7d8bff"
const HELM_IMAGE =
  "alpine/helm:3.19.0@sha256:aef9b56f64e866207d9591d0abd8f6d767b36aadd12edf68f8a719716d9d29c9"
const DOCKER_CLI_IMAGE =
  "docker:28.5.2-cli@sha256:625d9431a9f54c5a2bc90f24f0e1c3d55b1349fd857dd85035f98c2c9acbdd4d"
const WRANGLER_VERSION = "4.125.0"

const SOURCE_EXCLUDES = [
  ".git",
  ".git/**",
  ".dagger/node_modules",
  ".dagger/node_modules/**",
  ".dagger/sdk",
  ".dagger/sdk/**",
  ".dagger-inputs",
  ".dagger-inputs/**",
  "site/.next",
  "site/.next/**",
  "site/.wrangler",
  "site/.wrangler/**",
  "site/dist",
  "site/dist/**",
  "site/node_modules",
  "site/node_modules/**",
]

const VERIFY_ASSETS_TAG = String.raw`
const release = process.env.ASSETS_RELEASE
const revision = process.env.ASSETS_REVISION
const headers = {
  Accept: "application/vnd.github+json",
  Authorization: "Bearer " + process.env.GITHUB_TOKEN,
  "X-GitHub-Api-Version": "2022-11-28",
}
const get = async (path) => {
  const response = await fetch("https://api.github.com/repos/araihu/assets/" + path, { headers })
  if (!response.ok) throw new Error("GitHub tag verification failed (HTTP " + response.status + ")")
  return response.json()
}
let object = (await get("git/ref/tags/" + encodeURIComponent(release))).object
if (object.type === "tag") object = (await get("git/tags/" + object.sha)).object
if (object.type !== "commit" || object.sha !== revision) {
  throw new Error("release tag does not resolve to dispatched revision")
}
console.log("verified immutable Assets tag")
`

@object()
export class Paje {
  /** Run required root Go module checks. */
  @func()
  async rootCi(
    @argument({ defaultPath: ".", ignore: SOURCE_EXCLUDES }) source: Directory,
    cacheScope = "local",
  ): Promise<string> {
    return this.goProject(source, cacheScope)
      .withDirectory("/baseline", source)
      .withExec(["go", "mod", "verify"])
      .withExec(["go", "vet", "./..."])
      .withExec(["go", "test", "./...", "-count=1"])
      .withExec([
        "bash", "-euo", "pipefail", "-c",
        "diff -ruN --exclude=.git --exclude=.dagger --exclude=.dagger-inputs /baseline /work",
      ])
      .withExec(["printf", "Paje root CI passed\n"])
      .stdout()
  }

  /** Query npm advisory state freshly; never reuse a cached result. */
  @func({ cache: "never" })
  async siteAudit(
    @argument({ defaultPath: ".", ignore: SOURCE_EXCLUDES }) source: Directory,
    runNonce: string,
  ): Promise<string> {
    this.validateRunNonce(runNonce)
    return this.siteBase(source)
      .withEnvVariable("PAJE_RUN_NONCE", runNonce)
      .withExec([
        "npm", "audit", "--prefix", "/work/.dagger",
        "--package-lock-only", "--omit=dev", "--audit-level=high",
      ])
      .withExec(["npm", "audit", "--package-lock-only", "--audit-level=high"])
      .stdout()
  }

  /** Build, lint, and test the static site, returning its validated dist directory. */
  @func()
  siteBuild(
    @argument({ defaultPath: ".", ignore: SOURCE_EXCLUDES }) source: Directory,
    cacheScope = "local",
  ): Directory {
    return this.siteProject(source, cacheScope)
      .withExec(["npm", "ci"])
      .withExec(["npm", "run", "lint"])
      .withExec(["npm", "test"])
      .directory("/work/site/dist")
  }

  /** Deploy a previously validated site directory to the production Worker. */
  @func({ cache: "never" })
  async deploySite(
    @argument({ defaultPath: ".", ignore: SOURCE_EXCLUDES }) source: Directory,
    dist: Directory,
    cloudflareApiToken: Secret,
    cloudflareAccountId: string,
    runNonce: string,
  ): Promise<string> {
    this.validateRunNonce(runNonce)
    if (!/^[0-9a-f]{32}$/.test(cloudflareAccountId)) {
      throw new Error("Cloudflare account ID must be 32 lowercase hexadecimal characters")
    }
    const prepared = this.siteProject(source, "deploy")
      .withEnvVariable("PAJE_RUN_NONCE", runNonce)
      .withEnvVariable("CLOUDFLARE_ACCOUNT_ID", cloudflareAccountId)
      .withExec(["npm", "ci"])
      .withDirectory("/work/site/dist", dist)
      .withExec([
        "node", "--input-type=module", "--eval",
        `import manifest from "./node_modules/wrangler/package.json" with { type: "json" }; if (manifest.version !== "${WRANGLER_VERSION}") throw new Error("unexpected Wrangler version")`,
      ])

    return prepared
      .withSecretVariable("CLOUDFLARE_API_TOKEN", cloudflareApiToken)
      .withExec(["./node_modules/.bin/wrangler", "deploy"])
      .stdout()
  }

  /** Validate immutable provenance, regenerate fallback assets, and return only allowed outputs. */
  @func({ cache: "never" })
  async updateAraihuAssets(
    @argument({ defaultPath: ".", ignore: SOURCE_EXCLUDES }) source: Directory,
    payload: File,
    githubToken: Secret,
    runNonce: string,
    cacheScope = "assets",
  ): Promise<Directory> {
    this.validateRunNonce(runNonce)
    const input = await this.readStringObject(payload, [
      "assets_repository",
      "assets_revision",
      "release",
      "release_url",
      "release_sha256",
      "release_json_sha256",
    ])
    this.validateAssetsIdentity(input)

    await dag.container()
      .from(NODE_IMAGE)
      .withSecretVariable("GITHUB_TOKEN", githubToken)
      .withEnvVariable("ASSETS_RELEASE", input.release)
      .withEnvVariable("ASSETS_REVISION", input.assets_revision)
      .withEnvVariable("PAJE_RUN_NONCE", runNonce)
      .withExec(["node", "--input-type=module", "--eval", VERIFY_ASSETS_TAG])
      .sync()

    const archive = dag.http(input.release_url, {
      checksum: `sha256:${input.release_sha256}`,
      name: `araihu-assets-${input.release}.tar.gz`,
    })
    const updaterArgs = [
      "run", "./cmd/araihu-assets-update",
      "-repo", ".",
      "-archive", "/tmp/araihu-assets.tar.gz",
      "-assets-repository", input.assets_repository,
      "-assets-revision", input.assets_revision,
      "-release", input.release,
      "-release-url", input.release_url,
      "-release-sha256", input.release_sha256,
      "-release-json-sha256", input.release_json_sha256,
    ]

    const updated = this.goProject(source, cacheScope)
      .withDirectory("/baseline", source)
      .withEnvVariable("PAJE_RUN_NONCE", runNonce)
      .withFile("/tmp/araihu-assets.tar.gz", archive)
      .withExec(["go", ...updaterArgs])
      .withWorkdir("/work/site/generator")
      .withExec(["go", "run", ".", "--out", "../public"])
      .withWorkdir("/work")
      .withExec([
        "bash", "-euo", "pipefail", "-c",
        "status=0; git diff --no-index --check -- /baseline /work >/dev/null || status=$?; test \"$status\" -le 1",
      ])

    return dag.directory()
      .withNewFile("assets-handoff.json", JSON.stringify(input, null, 2) + "\n")
      .withFile("araihu-assets.json", updated.file("/work/araihu-assets.json"))
      .withFile("site/generator/araihu.css", updated.file("/work/site/generator/araihu.css"))
      .withDirectory("site/public", updated.directory("/work/site/public"))
  }

  private goProject(source: Directory, cacheScope: string): Container {
    const scope = this.validateCacheScope(cacheScope)
    // PR providers run on a host-owned isolated Engine. Input cannot select
    // that boundary: all fork/internal/untrusted scopes collapse to "pr".
    const cacheNamespace = this.isUntrustedScope(scope) ? "pr" : scope
    const helm = dag.container().from(HELM_IMAGE).file("/usr/bin/helm")
    const docker = dag.container().from(DOCKER_CLI_IMAGE).file("/usr/local/bin/docker")
    let container = dag.container()
      .from(GO_IMAGE)
      .withFile("/usr/local/bin/helm", helm, { permissions: 0o755 })
      .withFile("/usr/local/bin/docker", docker, { permissions: 0o755 })
      .withDirectory("/work", source)
      .withWorkdir("/work")
      .withEnvVariable("GOWORK", "off")
      .withEnvVariable("GOFLAGS", "-mod=readonly")
      .withEnvVariable("GOMODCACHE", "/go/pkg/mod")
      .withEnvVariable("GOCACHE", "/root/.cache/go-build")
      .withEnvVariable("PAJE_CACHE_SCOPE", scope)
    container = container
      .withMountedCache("/go/pkg/mod", dag.cacheVolume(`paje-${cacheNamespace}-go-mod-v1`))
      .withMountedCache("/root/.cache/go-build", dag.cacheVolume(`paje-${cacheNamespace}-go-build-v1`))
    return container
  }

  private siteProject(source: Directory, cacheScope: string): Container {
    const scope = this.validateCacheScope(cacheScope)
    const cacheNamespace = this.isUntrustedScope(scope) ? "pr" : scope
    let container = this.siteBase(source).withEnvVariable("PAJE_CACHE_SCOPE", scope)
    container = container
      .withMountedCache("/root/.npm", dag.cacheVolume(`paje-${cacheNamespace}-npm-v1`))
      .withMountedCache("/go/pkg/mod", dag.cacheVolume(`paje-${cacheNamespace}-site-go-mod-v1`))
      .withMountedCache("/root/.cache/go-build", dag.cacheVolume(`paje-${cacheNamespace}-site-go-build-v1`))
    return container
  }

  private siteBase(source: Directory): Container {
    const goImage = dag.container().from(GO_IMAGE)
    const goDistribution = goImage.directory("/usr/local/go")
    const caCertificates = goImage.file("/etc/ssl/certs/ca-certificates.crt")
    return dag.container()
      .from(NODE_IMAGE)
      .withDirectory("/usr/local/go", goDistribution)
      .withFile("/etc/ssl/certs/ca-certificates.crt", caCertificates)
      .withDirectory("/work", source)
      .withWorkdir("/work/site")
      .withEnvVariable("PATH", "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
      .withEnvVariable("GOTOOLCHAIN", "auto")
      .withEnvVariable("GOMODCACHE", "/go/pkg/mod")
      .withEnvVariable("GOCACHE", "/root/.cache/go-build")
      .withEnvVariable("npm_config_cache", "/root/.npm")
  }

  private validateCacheScope(value: string): string {
    if (!/^(untrusted|fork|internal|local|main|manual|assets|deploy)$/.test(value)) {
      throw new Error(`unsafe cache scope: ${value}`)
    }
    return value
  }

  private isUntrustedScope(value: string): boolean {
    return /^(untrusted|fork|internal)$/.test(value)
  }

  private validateRunNonce(value: string): void {
    if (!/^[1-9][0-9]*-[1-9][0-9]*$/.test(value)) {
      throw new Error("run nonce must be github.run_id-github.run_attempt")
    }
  }

  private async readStringObject(
    payload: File,
    expectedKeys: readonly string[],
  ): Promise<Record<string, string>> {
    let parsed: unknown
    try {
      parsed = JSON.parse(await payload.contents())
    } catch {
      throw new Error("Dagger input payload must be valid JSON")
    }
    if (parsed === null || Array.isArray(parsed) || typeof parsed !== "object") {
      throw new Error("Dagger input payload must be a JSON object")
    }
    const record = parsed as Record<string, unknown>
    const actualKeys = Object.keys(record).sort()
    const requiredKeys = [...expectedKeys].sort()
    if (
      actualKeys.length !== requiredKeys.length ||
      actualKeys.some((key, index) => key !== requiredKeys[index])
    ) {
      throw new Error("Dagger input payload does not match the exact schema")
    }
    for (const key of requiredKeys) {
      if (typeof record[key] !== "string") {
        throw new Error(`Dagger input field ${key} must be a string`)
      }
    }
    return record as Record<string, string>
  }

  private validateAssetsIdentity(input: Record<string, string>): void {
    if (input.assets_repository !== "araihu/assets") {
      throw new Error("assets repository must be araihu/assets")
    }
    if (!/^[0-9a-f]{40}$/.test(input.assets_revision)) {
      throw new Error("assets revision must be a full lowercase Git SHA-1")
    }
    if (!/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(input.release)) {
      throw new Error("assets release must be a stable vMAJOR.MINOR.PATCH tag")
    }
    if (!/^[0-9a-f]{64}$/.test(input.release_sha256)) {
      throw new Error("release archive SHA-256 must be lowercase hexadecimal")
    }
    if (!/^[0-9a-f]{64}$/.test(input.release_json_sha256)) {
      throw new Error("release.json SHA-256 must be lowercase hexadecimal")
    }
    const expected = `https://github.com/araihu/assets/releases/download/${input.release}/araihu-assets-${input.release}.tar.gz`
    if (input.release_url !== expected) {
      throw new Error("release URL does not match immutable Arai Hu release shape")
    }
  }
}
