package paje

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDaggerModulePinsRuntimeAndSeparatesCachesFromFreshEffects(t *testing.T) {
	config := readFile(t, "dagger.json")
	for _, want := range []string{`"engineVersion": "v0.21.8"`, `"source": "typescript"`, `"source": ".dagger"`} {
		if !strings.Contains(config, want) {
			t.Errorf("dagger.json misses %q", want)
		}
	}

	module := readFile(t, filepath.Join(".dagger", "src", "index.ts"))
	for _, want := range []string{
		`golang:1.26.5-bookworm@sha256:`,
		`node:22.13.0-bookworm-slim@sha256:`,
		`const caCertificates = goImage.file("/etc/ssl/certs/ca-certificates.crt")`,
		`.withFile("/etc/ssl/certs/ca-certificates.crt", caCertificates)`,
		`alpine/helm:3.19.0@sha256:aef9b56f64e866207d9591d0abd8f6d767b36aadd12edf68f8a719716d9d29c9`,
		`docker:28.5.2-cli@sha256:625d9431a9f54c5a2bc90f24f0e1c3d55b1349fd857dd85035f98c2c9acbdd4d`,
		`const WRANGLER_VERSION = "4.120.0"`,
		`@func({ cache: "never" })`,
		`siteAudit(`,
		`deploySite(`,
		`updateAraihuAssets(`,
		`run nonce must be github.run_id-github.run_attempt`,
		`const cacheNamespace = this.isUntrustedScope(scope) ? "pr" : scope`,
		`return /^(untrusted|fork|internal)$/.test(value)`,
		`dag.cacheVolume(`,
	} {
		if !strings.Contains(module, want) {
			t.Errorf("Dagger module misses %q", want)
		}
	}
	for _, name := range []string{"siteAudit", "deploySite", "updateAraihuAssets"} {
		body := daggerFunctionSource(t, module, name)
		if !strings.Contains(body, `@func({ cache: "never" })`) || !strings.Contains(body, "runNonce: string") {
			t.Errorf("fresh/effect function %s lacks cache=never and nonce", name)
		}
	}
	for _, name := range []string{"rootCi", "siteBuild"} {
		if strings.Contains(daggerFunctionSource(t, module, name), `cache: "never"`) {
			t.Errorf("deterministic function %s disables Dagger result caching", name)
		}
	}
	for _, name := range []string{"goProject", "siteProject"} {
		body := privateFunctionSource(t, module, name)
		guard := strings.Index(body, `const cacheNamespace = this.isUntrustedScope(scope) ? "pr" : scope`)
		mount := strings.Index(body, ".withMountedCache(")
		if guard < 0 || mount < 0 || guard > mount {
			t.Errorf("%s does not map every PR to stable namespace pr before mounting caches", name)
		}
	}
}

func TestDaggerTypeScriptRuntimePackageContract(t *testing.T) {
	type manifest struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	type lockPackage struct {
		Resolved     string            `json:"resolved"`
		Link         bool              `json:"link"`
		Dependencies map[string]string `json:"dependencies"`
	}
	type lock struct {
		LockfileVersion int                    `json:"lockfileVersion"`
		Packages        map[string]lockPackage `json:"packages"`
	}

	var packageJSON manifest
	readJSON(t, filepath.Join(".dagger", "package.json"), &packageJSON)
	if got := packageJSON.Dependencies["@dagger.io/dagger"]; got != "./sdk" {
		t.Fatalf("Dagger SDK dependency = %q, want generated local ./sdk", got)
	}
	if got := packageJSON.Dependencies["typescript"]; got != "^6.0.3" {
		t.Fatalf("TypeScript runtime dependency = %q, want ^6.0.3", got)
	}
	if _, ok := packageJSON.DevDependencies["typescript"]; ok {
		t.Fatal("TypeScript remains a devDependency")
	}

	var packageLock lock
	readJSON(t, filepath.Join(".dagger", "package-lock.json"), &packageLock)
	if packageLock.LockfileVersion != 3 {
		t.Fatalf("Dagger npm lockfile version = %d, want 3", packageLock.LockfileVersion)
	}
	root := packageLock.Packages[""]
	if root.Dependencies["@dagger.io/dagger"] != "./sdk" || root.Dependencies["typescript"] != "^6.0.3" {
		t.Fatalf("lock root has wrong runtime contract: %+v", root.Dependencies)
	}
	sdk := packageLock.Packages["node_modules/@dagger.io/dagger"]
	if !sdk.Link || sdk.Resolved != "sdk" {
		t.Fatalf("locked SDK is not generated local link: %+v", sdk)
	}
	for _, want := range []string{`".dagger/sdk"`, `".dagger/sdk/**"`} {
		if !strings.Contains(readFile(t, filepath.Join(".dagger", "src", "index.ts")), want) {
			t.Errorf("Dagger source snapshot does not exclude generated SDK %s", want)
		}
	}
	if !strings.Contains(readFile(t, ".gitignore"), "/.dagger/sdk/") {
		t.Fatal("generated Dagger SDK is not ignored")
	}
	provider := readFile(t, filepath.Join(".github", "workflows", "ci.yml"))
	if !strings.Contains(provider, `test -z "$(git ls-files -- .dagger/sdk)"`) {
		t.Fatal("provider does not prove generated SDK bytes are untracked before Dagger snapshots source")
	}
}

func TestDaggerSupplyChainAuditAndDeploySecretBoundary(t *testing.T) {
	module := readFile(t, filepath.Join(".dagger", "src", "index.ts"))
	audit := daggerFunctionSource(t, module, "siteAudit")
	for _, want := range []string{
		`"npm", "audit", "--prefix", "/work/.dagger"`,
		`"--package-lock-only", "--omit=dev", "--audit-level=high"`,
	} {
		if !strings.Contains(audit, want) {
			t.Errorf("site audit misses Dagger production-lock gate %q", want)
		}
	}

	deploy := daggerFunctionSource(t, module, "deploySite")
	install := strings.Index(deploy, `.withExec(["npm", "ci"])`)
	version := strings.Index(deploy, `manifest.version !== "${WRANGLER_VERSION}"`)
	secret := strings.Index(deploy, `.withSecretVariable("CLOUDFLARE_API_TOKEN", cloudflareApiToken)`)
	final := strings.Index(deploy, `.withExec(["./node_modules/.bin/wrangler", "deploy"])`)
	if install < 0 || version < install || secret < version || final < secret {
		t.Fatalf("Cloudflare token is not scoped to the final locked Wrangler deploy: install=%d version=%d secret=%d final=%d", install, version, secret, final)
	}
	if strings.Count(deploy, "withSecretVariable") != 1 || strings.Contains(deploy, `"npm", "exec"`) || strings.Contains(deploy, "--yes") {
		t.Fatal("deploy must attach one secret only after locked dependency preparation, without npm exec downloads")
	}
	packageJSON := readFile(t, filepath.Join("site", "package.json"))
	packageLock := readFile(t, filepath.Join("site", "package-lock.json"))
	for path, content := range map[string]string{"site/package.json": packageJSON, "site/package-lock.json": packageLock} {
		if !strings.Contains(content, `"wrangler": "4.120.0"`) {
			t.Errorf("%s does not lock Wrangler 4.120.0", path)
		}
	}
}

func TestWorkflowsAreThinExactCLIAdapters(t *testing.T) {
	pinnedAction := regexp.MustCompile(`^[0-9a-f]{40}(?:\s+#.*)?$`)
	tests := []struct {
		workflow string
		calls    int
	}{
		{"ci.yml", 1},
		{"site-ci.yml", 2},
		{"deploy-site.yml", 3},
		{"araihu-assets.yml", 1},
	}
	for _, test := range tests {
		workflow := readFile(t, filepath.Join(".github", "workflows", test.workflow))
		if strings.Contains(workflow, "dagger/dagger-for-github") {
			t.Errorf("%s delegates CLI selection to dagger-for-github", test.workflow)
		}
		if got := strings.Count(workflow, "dagger call "); got != test.calls {
			t.Errorf("%s has %d Dagger calls, want %d", test.workflow, got, test.calls)
		}
		if !strings.Contains(workflow, "run: bash .github/scripts/setup-dagger.sh") {
			t.Errorf("%s lacks exact CLI gate", test.workflow)
		}
		firstSetup := strings.Index(workflow, "run: bash .github/scripts/setup-dagger.sh")
		firstCall := strings.Index(workflow, "dagger call ")
		if firstCall >= 0 && firstSetup > firstCall {
			t.Errorf("%s invokes Dagger before exact CLI gate", test.workflow)
		}
		for _, line := range strings.Split(workflow, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "uses:") {
				continue
			}
			at := strings.LastIndex(line, "@")
			if at < 0 || !pinnedAction.MatchString(strings.TrimSpace(line[at+1:])) {
				t.Errorf("%s contains unpinned action: %s", test.workflow, line)
			}
		}
	}

	setup := readFile(t, filepath.Join(".github", "scripts", "setup-dagger.sh"))
	for _, want := range []string{
		`expected_version="v0.21.8"`,
		`dagger_v0.21.8_linux_amd64.tar.gz`,
		`53e226c7da8fb75171e58c35759d736d961ce8b3a12db0baa7b7107954fccc5a`,
		`RUNNER_ENVIRONMENT`,
		`self-hosted`,
		`github-hosted`,
		`sha256sum --check --strict`,
		`resolved_version="$(dagger_version)"`,
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("exact Dagger setup misses %q", want)
		}
	}
}

func TestWorkflowTrustAndArtifactContracts(t *testing.T) {
	root := readFile(t, filepath.Join(".github", "workflows", "ci.yml"))
	for _, want := range []string{`github.event_name == 'pull_request' && 'untrusted'`, `--cache-scope="$CACHE_SCOPE"`} {
		if !strings.Contains(root, want) {
			t.Errorf("root CI misses trust contract %q", want)
		}
	}
	site := readFile(t, filepath.Join(".github", "workflows", "site-ci.yml"))
	for _, want := range []string{
		`CACHE_SCOPE: untrusted`,
		`"hostinger-vps-pr"`,
		`RUN_NONCE: ${{ github.run_id }}-${{ github.run_attempt }}`,
		`actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a`,
	} {
		if !strings.Contains(site, want) {
			t.Errorf("site CI misses %q", want)
		}
	}
	if strings.Contains(site, `"hostinger-vps"`) {
		t.Fatal("legacy shared Hostinger runner label remains")
	}
	for _, workflow := range []string{root, site} {
		if strings.Contains(workflow, "format('pr-") || strings.Contains(workflow, "cache-scope=pr-") {
			t.Fatal("PR workflow creates a persistent PR cache namespace")
		}
	}
	deploy := readFile(t, filepath.Join(".github", "workflows", "deploy-site.yml"))
	for _, want := range []string{
		`group: paje-site-production`,
		`cancel-in-progress: false`,
		`environment:`,
		`name: production`,
		`actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c`,
		`--cloudflare-api-token=env://CLOUDFLARE_API_TOKEN`,
		`--run-nonce="$RUN_NONCE"`,
	} {
		if !strings.Contains(deploy, want) {
			t.Errorf("deploy workflow misses %q", want)
		}
	}
	assets := readFile(t, filepath.Join(".github", "workflows", "araihu-assets.yml"))
	for _, want := range []string{
		`repository_dispatch:`,
		`types: [araihu-assets-released]`,
		`actions/create-github-app-token@fee1f7d63c2ff003460e3d139729b119787bc349`,
		`--payload=.dagger-inputs/assets.json`,
		`--github-token=env://ASSETS_GITHUB_TOKEN`,
		`needs: validate`,
		`assets-handoff.json`,
		`git push origin "$branch"`,
	} {
		if !strings.Contains(assets, want) {
			t.Errorf("assets workflow misses %q", want)
		}
	}
}

func TestDaggerRootRuntimeProvidesRun31562878247Prerequisites(t *testing.T) {
	module := readFile(t, filepath.Join(".dagger", "src", "index.ts"))
	root := privateFunctionSource(t, module, "goProject")
	for _, want := range []string{
		`.withFile("/usr/local/bin/helm", helm, { permissions: 0o755 })`,
		`.withFile("/usr/local/bin/docker", docker, { permissions: 0o755 })`,
	} {
		if !strings.Contains(root, want) {
			t.Errorf("run 31562878247 prerequisite regression missing %q", want)
		}
	}
}

func TestAssetsWriteTokenIsMintedOnlyAfterReadOnlyValidation(t *testing.T) {
	workflow := readFile(t, filepath.Join(".github", "workflows", "araihu-assets.yml"))
	validateJob := strings.Index(workflow, "  validate:\n")
	readOnlyCheckout := strings.Index(workflow, "Check out Pajé read-only")
	materialize := strings.Index(workflow, "Materialize exact provenance payload")
	readOnlyProof := strings.Index(workflow, "Validate payload, resolve tag read-only, and regenerate fallbacks")
	upload := strings.Index(workflow, "Upload validated allowlisted handoff")
	publishJob := strings.Index(workflow, "  publish:\n")
	needs := strings.Index(workflow, "    needs: validate")
	appToken := strings.Index(workflow, "Create selected-repository App token after validation")
	writeCheckout := strings.Index(workflow, "Check out Pajé for the validated write")
	apply := strings.Index(workflow, "Materialize validated fallback update")
	write := strings.Index(workflow, "Create versioned pull request")
	ordered := []int{validateJob, readOnlyCheckout, materialize, readOnlyProof, upload, publishJob, needs, appToken, writeCheckout, apply, write}
	for index, position := range ordered {
		if position < 0 || index > 0 && position <= ordered[index-1] {
			t.Fatalf("Assets provider boundary is out of order at index %d: %v", index, ordered)
		}
	}
	validation := workflow[validateJob:publishJob]
	for _, forbidden := range []string{"create-github-app-token", "ARAIHU_ASSETS_APP_PRIVATE_KEY", "permission-contents: write", "git push", "gh pr create"} {
		if strings.Contains(validation, forbidden) {
			t.Errorf("read-only validation job contains write capability %q", forbidden)
		}
	}
	for _, want := range []string{
		`persist-credentials: false`,
		`ASSETS_GITHUB_TOKEN: ${{ github.token }}`,
		`export --path="${{ runner.temp }}/paje-assets-validated"`,
	} {
		if !strings.Contains(validation, want) {
			t.Errorf("read-only validation job misses %q", want)
		}
	}
	module := readFile(t, filepath.Join(".dagger", "src", "index.ts"))
	assetsFunction := daggerFunctionSource(t, module, "updateAraihuAssets")
	for _, want := range []string{
		"this.validateAssetsIdentity(input)",
		`withSecretVariable("GITHUB_TOKEN", githubToken)`,
		`withNewFile("assets-handoff.json", JSON.stringify(input, null, 2) + "\n")`,
	} {
		if !strings.Contains(assetsFunction, want) {
			t.Errorf("validated Assets function misses %q", want)
		}
	}
}

func TestSiteRuntimeChangesReachValidationAndDeployment(t *testing.T) {
	for _, workflow := range []string{"site-ci.yml", "deploy-site.yml"} {
		content := readFile(t, filepath.Join(".github", "workflows", workflow))
		for _, path := range []string{
			`".dagger/**"`,
			`".github/scripts/setup-dagger.sh"`,
			`"dagger.json"`,
			`"dagger_contract_test.go"`,
		} {
			if !strings.Contains(content, path) {
				t.Errorf("%s does not react to %s", workflow, path)
			}
		}
	}
}

func TestSelfHostedDaggerSetupAcceptsOnlyExactVersion(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
		ok      bool
	}{
		{"exact", "v0.21.8", true},
		{"newer", "v0.21.9", false},
		{"older", "v0.21.7", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := t.TempDir()
			dagger := filepath.Join(bin, "dagger")
			if err := os.WriteFile(dagger, []byte("#!/bin/sh\nprintf 'dagger "+test.version+" (test) linux/amd64\\n'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", filepath.Join(".github", "scripts", "setup-dagger.sh"))
			command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "RUNNER_ENVIRONMENT=self-hosted")
			output, err := command.CombinedOutput()
			if test.ok && err != nil {
				t.Fatalf("exact version rejected: %v\n%s", err, output)
			}
			if !test.ok && err == nil {
				t.Fatalf("mismatched version accepted:\n%s", output)
			}
		})
	}
}

func TestCIAuthorialFilesContainNoCodeRabbit(t *testing.T) {
	paths := []string{"dagger.json"}
	for _, pattern := range []string{filepath.Join(".github", "workflows", "*.yml"), filepath.Join(".github", "scripts", "*.sh")} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, matches...)
	}
	if err := filepath.WalkDir(".dagger", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if !entry.IsDir() {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.Contains(strings.ToLower(readFile(t, path)), "coderabbit") {
			t.Errorf("%s retains CodeRabbit integration", path)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(readFile(t, path)), target); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func daggerFunctionSource(t *testing.T, module, name string) string {
	t.Helper()
	start := strings.Index(module, "\n  async "+name+"(")
	if start < 0 {
		start = strings.Index(module, "\n  "+name+"(")
	}
	if start < 0 {
		t.Fatalf("Dagger function %s not found", name)
	}
	annotation := strings.LastIndex(module[:start], "\n  @func")
	if annotation < 0 {
		t.Fatalf("Dagger function %s annotation not found", name)
	}
	endOffset := strings.Index(module[start+1:], "\n  @func")
	if endOffset < 0 {
		endOffset = strings.Index(module[start+1:], "\n  private ")
	}
	if endOffset < 0 {
		endOffset = len(module) - start - 1
	}
	return module[annotation+1 : start+1+endOffset]
}

func privateFunctionSource(t *testing.T, module, name string) string {
	t.Helper()
	start := strings.Index(module, "\n  private "+name+"(")
	if start < 0 {
		t.Fatalf("private Dagger function %s not found", name)
	}
	endOffset := strings.Index(module[start+1:], "\n  private ")
	if endOffset < 0 {
		endOffset = len(module) - start - 1
	}
	return module[start+1 : start+1+endOffset]
}
