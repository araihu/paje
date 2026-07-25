package paje_test

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultChartRendersOnlyRequiredGeneratedHatchetSecret(t *testing.T) {
	manifests, rendered := renderChart(t,
		"--set", "secrets.hatchet.value=hatchet-secret-value",
		"--set", "adapters.runner=mock",
	)
	secrets := manifestsOfKind(manifests, "Secret")
	if len(secrets) != 1 {
		t.Fatalf("rendered Secrets = %d, want 1", len(secrets))
	}
	if got := stringValue(mapValue(secrets[0], "metadata"), "name"); got != "paje-hatchet" {
		t.Errorf("generated Hatchet Secret name = %q, want paje-hatchet", got)
	}
	if got := mapValue(secrets[0], "stringData"); !reflect.DeepEqual(got, map[string]any{
		"hatchet-client-token": "hatchet-secret-value",
	}) {
		t.Errorf("generated Hatchet Secret data = %#v", got)
	}
	assertConfigAndPodDoNotContain(t, manifests, "hatchet-secret-value")
	if strings.Count(rendered, "kind: PersistentVolumeClaim") != 0 {
		t.Error("default render unexpectedly contains a PersistentVolumeClaim")
	}
}

func TestBetaChartRendersPersistentCodexWorkerWithSeparatedCredentials(t *testing.T) {
	manifests, _ := renderChart(t,
		"--set", "persistence.enabled=true",
		"--set", "persistence.size=20Gi",
		"--set", "adapters.memory=mem0",
		"--set", "adapters.workspace=git",
		"--set", "adapters.runner=codex",
		"--set", "publisher.adapter=github",
		"--set", "secrets.hatchet.existingSecret=paje-hatchet",
		"--set", "secrets.mem0.existingSecret=paje-mem0",
		"--set", "secrets.github.existingSecret=paje-github",
		"--set", "codexAuth.existingSecret=paje-codex-auth",
	)

	pvcs := manifestsOfKind(manifests, "PersistentVolumeClaim")
	if len(pvcs) != 1 {
		t.Fatalf("rendered PVCs = %d, want 1", len(pvcs))
	}
	requests := mapValue(mapValue(mapValue(pvcs[0], "spec"), "resources"), "requests")
	if got := stringValue(requests, "storage"); got != "20Gi" {
		t.Errorf("PVC storage = %q, want 20Gi", got)
	}

	deployment := oneManifestOfKind(t, manifests, "Deployment")
	spec := mapValue(deployment, "spec")
	if got := intValue(spec, "replicas"); got != 1 {
		t.Errorf("Deployment replicas = %d, want 1", got)
	}
	podSpec := mapValue(mapValue(spec, "template"), "spec")
	container := namedObject(t, sliceValue(podSpec, "containers"), "paje")
	assertEnvValue(t, container, "CODEX_HOME", "/codex-home")
	assertSecretEnv(t, container, "HATCHET_CLIENT_TOKEN", "paje-hatchet", "hatchet-client-token")
	assertSecretEnv(t, container, "MEM0_API_KEY", "paje-mem0", "mem0-api-key")
	assertSecretEnv(t, container, "GITHUB_TOKEN", "paje-github", "github-token")
	assertMount(t, container, "data", "/var/lib/paje", false)
	assertMount(t, container, "runtime", "/run/paje", false)
	assertMount(t, container, "codex-home", "/codex-home", false)

	init := namedObject(t, sliceValue(podSpec, "initContainers"), "seed-codex-home")
	if got, want := stringSlice(init["command"]), []string{
		"/bin/sh", "-ec", "cp -R /codex-auth-seed/. /codex-home/\nchmod -R go-rwx /codex-home\n",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("init command = %#v, want %#v", got, want)
	}
	assertMount(t, init, "codex-auth-seed", "/codex-auth-seed", true)
	assertMount(t, init, "codex-home", "/codex-home", false)
	security := mapValue(init, "securityContext")
	if intValue(security, "runAsUser") != 65532 || intValue(security, "runAsGroup") != 65532 ||
		boolValue(security, "allowPrivilegeEscalation") || !boolValue(security, "runAsNonRoot") {
		t.Errorf("init securityContext = %#v", security)
	}
	capabilities := mapValue(security, "capabilities")
	if got := stringSlice(capabilities["drop"]); !reflect.DeepEqual(got, []string{"ALL"}) {
		t.Errorf("init dropped capabilities = %#v", got)
	}

	volumeNames := map[string]map[string]any{}
	for _, item := range sliceValue(podSpec, "volumes") {
		volume := item.(map[string]any)
		volumeNames[stringValue(volume, "name")] = volume
	}
	if claim := mapValue(volumeNames["data"], "persistentVolumeClaim"); stringValue(claim, "claimName") != "paje" {
		t.Errorf("data volume = %#v", volumeNames["data"])
	}
	if _, ok := volumeNames["runtime"]["emptyDir"]; !ok {
		t.Errorf("runtime volume = %#v, want emptyDir", volumeNames["runtime"])
	}
	if _, ok := volumeNames["codex-home"]["emptyDir"]; !ok {
		t.Errorf("codex-home volume = %#v, want emptyDir", volumeNames["codex-home"])
	}
	seed := mapValue(volumeNames["codex-auth-seed"], "secret")
	if stringValue(seed, "secretName") != "paje-codex-auth" {
		t.Errorf("Codex seed volume = %#v", seed)
	}

	configMap := oneManifestOfKind(t, manifests, "ConfigMap")
	wantData := map[string]string{
		"PAJE_MEMORY_ADAPTER":             "mem0",
		"PAJE_WORKSPACE_ADAPTER":          "git",
		"PAJE_RUNNER_ADAPTER":             "codex",
		"PAJE_PUBLISHER_ADAPTER":          "github",
		"PAJE_WORKSPACE_ROOT":             "/var/lib/paje/workspace",
		"PAJE_RUN_ROOT":                   "/var/lib/paje/runs",
		"PAJE_ARTIFACT_ROOT":              "/var/lib/paje/artifacts",
		"PAJE_RUNTIME_ROOT":               "/run/paje",
		"PAJE_ARTIFACT_LIMIT_BYTES":       "10485760",
		"PAJE_COMMAND_OUTPUT_LIMIT_BYTES": "1048576",
		"PAJE_ENV_ALLOWLIST":              "[]",
	}
	data := mapValue(configMap, "data")
	for key, want := range wantData {
		if got := fmt.Sprint(data[key]); got != want {
			t.Errorf("ConfigMap %s = %q, want %q", key, got, want)
		}
	}
	if _, found := data["CODEX_HOME"]; found {
		t.Error("ConfigMap contains CODEX_HOME; it must be set only on the worker container")
	}
	if _, configured := container["args"]; configured {
		t.Errorf("worker arguments = %#v, want no credential-bearing arguments", container["args"])
	}
}

func TestChartGeneratesThreeSeparateRequiredCredentialSecrets(t *testing.T) {
	manifests, _ := renderChart(t,
		"--set", "adapters.runner=mock",
		"--set", "adapters.memory=mem0",
		"--set", "adapters.workspace=git",
		"--set", "publisher.adapter=github",
		"--set", "secrets.hatchet.value=hatchet-value",
		"--set", "secrets.mem0.value=mem0-value",
		"--set", "secrets.github.value=github-value",
	)
	secrets := manifestsOfKind(manifests, "Secret")
	if len(secrets) != 3 {
		t.Fatalf("rendered Secrets = %d, want 3", len(secrets))
	}
	want := map[string]map[string]any{
		"paje-hatchet": {"hatchet-client-token": "hatchet-value"},
		"paje-mem0":    {"mem0-api-key": "mem0-value"},
		"paje-github":  {"github-token": "github-value"},
	}
	for _, secret := range secrets {
		name := stringValue(mapValue(secret, "metadata"), "name")
		if !reflect.DeepEqual(mapValue(secret, "stringData"), want[name]) {
			t.Errorf("Secret %q data = %#v, want %#v", name, mapValue(secret, "stringData"), want[name])
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Errorf("missing generated Secrets: %v", want)
	}
}

func TestChartSchemaRejectsUnsafeRuntimeSelections(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "multiple replicas", args: []string{"--set", "replicaCount=2", "--set", "secrets.hatchet.value=test"}},
		{name: "zero artifact limit", args: []string{"--set", "limits.artifactBytes=0", "--set", "secrets.hatchet.value=test"}},
		{name: "unknown runner", args: []string{"--set", "adapters.runner=unknown", "--set", "secrets.hatchet.value=test"}},
		{name: "missing Hatchet secret", args: nil},
		{name: "missing Mem0 secret", args: []string{"--set", "adapters.memory=mem0", "--set", "secrets.hatchet.value=test"}},
		{name: "missing GitHub secret", args: []string{"--set", "publisher.adapter=github", "--set", "secrets.hatchet.value=test"}},
		{name: "missing Codex auth seed", args: []string{"--set", "adapters.runner=codex", "--set", "secrets.hatchet.value=test"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := helmTemplate(test.args...); err == nil {
				t.Fatal("helm template error = nil, want schema or required-value error")
			}
		})
	}
}

func TestChartRejectsSharedActiveCredentialSecrets(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "Hatchet and Mem0",
			args: []string{
				"--set", "adapters.runner=mock",
				"--set", "adapters.memory=mem0",
				"--set", "secrets.hatchet.existingSecret=shared-credentials",
				"--set", "secrets.mem0.existingSecret=shared-credentials",
			},
		},
		{
			name: "Hatchet and GitHub",
			args: []string{
				"--set", "adapters.runner=mock",
				"--set", "publisher.adapter=github",
				"--set", "secrets.hatchet.existingSecret=shared-credentials",
				"--set", "secrets.github.existingSecret=shared-credentials",
			},
		},
		{
			name: "Mem0 and GitHub",
			args: []string{
				"--set", "adapters.runner=mock",
				"--set", "adapters.memory=mem0",
				"--set", "publisher.adapter=github",
				"--set", "secrets.hatchet.existingSecret=hatchet-credentials",
				"--set", "secrets.mem0.existingSecret=shared-credentials",
				"--set", "secrets.github.existingSecret=shared-credentials",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, err := helmTemplate(test.args...)
			if err == nil {
				t.Fatal("helm template error = nil, want shared active Secret rejection")
			}
			if !strings.Contains(output, "active credentials must use distinct Secrets") {
				t.Fatalf("helm template output = %q, want separation error", output)
			}
		})
	}
}

func renderChart(t *testing.T, args ...string) ([]map[string]any, string) {
	t.Helper()
	output, err := helmTemplate(args...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, output)
	}
	decoder := yaml.NewDecoder(strings.NewReader(output))
	var manifests []map[string]any
	for {
		var manifest map[string]any
		err := decoder.Decode(&manifest)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode rendered manifest: %v", err)
		}
		if len(manifest) != 0 {
			manifests = append(manifests, manifest)
		}
	}
	return manifests, output
}

func helmTemplate(args ...string) (string, error) {
	commandArgs := append([]string{"template", "paje", "."}, args...)
	command := exec.Command("helm", commandArgs...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	return output.String(), err
}

func manifestsOfKind(manifests []map[string]any, kind string) []map[string]any {
	var result []map[string]any
	for _, manifest := range manifests {
		if stringValue(manifest, "kind") == kind {
			result = append(result, manifest)
		}
	}
	return result
}

func oneManifestOfKind(t *testing.T, manifests []map[string]any, kind string) map[string]any {
	t.Helper()
	matching := manifestsOfKind(manifests, kind)
	if len(matching) != 1 {
		t.Fatalf("rendered %s manifests = %d, want 1", kind, len(matching))
	}
	return matching[0]
}

func mapValue(object map[string]any, key string) map[string]any {
	value, _ := object[key].(map[string]any)
	return value
}

func sliceValue(object map[string]any, key string) []any {
	value, _ := object[key].([]any)
	return value
}

func stringValue(object map[string]any, key string) string { return fmt.Sprint(object[key]) }

func intValue(object map[string]any, key string) int {
	value, _ := object[key].(int)
	return value
}

func boolValue(object map[string]any, key string) bool {
	value, _ := object[key].(bool)
	return value
}

func stringSlice(value any) []string {
	items, _ := value.([]any)
	result := make([]string, len(items))
	for index, item := range items {
		result[index] = fmt.Sprint(item)
	}
	return result
}

func namedObject(t *testing.T, objects []any, name string) map[string]any {
	t.Helper()
	for _, item := range objects {
		object := item.(map[string]any)
		if stringValue(object, "name") == name {
			return object
		}
	}
	t.Fatalf("object named %q not found", name)
	return nil
}

func assertEnvValue(t *testing.T, container map[string]any, name, want string) {
	t.Helper()
	environment := namedObject(t, sliceValue(container, "env"), name)
	if got := stringValue(environment, "value"); got != want {
		t.Errorf("environment %s = %q, want %q", name, got, want)
	}
}

func assertSecretEnv(t *testing.T, container map[string]any, envName, secretName, key string) {
	t.Helper()
	environment := namedObject(t, sliceValue(container, "env"), envName)
	reference := mapValue(mapValue(environment, "valueFrom"), "secretKeyRef")
	if gotName, gotKey := stringValue(reference, "name"), stringValue(reference, "key"); gotName != secretName || gotKey != key {
		t.Errorf("environment %s secret ref = %q/%q, want %q/%q", envName, gotName, gotKey, secretName, key)
	}
}

func assertMount(t *testing.T, container map[string]any, name, path string, readOnly bool) {
	t.Helper()
	mount := namedObject(t, sliceValue(container, "volumeMounts"), name)
	if got := stringValue(mount, "mountPath"); got != path {
		t.Errorf("mount %s path = %q, want %q", name, got, path)
	}
	if got, _ := mount["readOnly"].(bool); got != readOnly {
		t.Errorf("mount %s readOnly = %t, want %t", name, got, readOnly)
	}
}

func assertConfigAndPodDoNotContain(t *testing.T, manifests []map[string]any, values ...string) {
	t.Helper()
	for _, kind := range []string{"ConfigMap", "Deployment"} {
		for _, manifest := range manifestsOfKind(manifests, kind) {
			encoded, err := yaml.Marshal(manifest)
			if err != nil {
				t.Fatalf("encode %s: %v", kind, err)
			}
			for _, value := range values {
				if strings.Contains(string(encoded), value) {
					t.Errorf("%s contains secret value %q", kind, value)
				}
			}
		}
	}
}
