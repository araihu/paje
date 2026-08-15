package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/araihu/paje/internal/secret"
	secretfilesystem "github.com/araihu/paje/internal/secret/filesystem"
	"github.com/araihu/paje/internal/workerprofile"
	workerprofilefilesystem "github.com/araihu/paje/internal/workerprofile/filesystem"
)

const (
	acceptanceCodexVersion  = "0.144.5"
	acceptanceGoVersion     = "1.26.5"
	acceptanceGitVersion    = "2.49.1"
	acceptanceNodeVersion   = "24.4.1"
	acceptanceTaskLabel     = "com.araihu.paje.acceptance.task"
	acceptanceResourceLabel = "com.araihu.paje.acceptance.resource"
)

var acceptanceResourceSequence atomic.Uint64
var configuredWorkerCommandCache sync.Map

type dockerAcceptance struct {
	endpoint       string
	platform       string
	repositoryRoot string
	commit         string
}

type builtImage struct {
	reference string
	config    imageConfig
}

type publishedWorker struct {
	docker  dockerAcceptance
	image   string
	profile workerprofile.Snapshot
}

type imageConfig struct {
	User       string            `json:"User"`
	Entrypoint []string          `json:"Entrypoint"`
	Cmd        []string          `json:"Cmd"`
	Labels     map[string]string `json:"Labels"`
}

type imageInspection struct {
	RepositoryDigests []string    `json:"RepoDigests"`
	OS                string      `json:"Os"`
	Architecture      string      `json:"Architecture"`
	Config            imageConfig `json:"Config"`
}

type workerImageProbe func(string, []string) ([]byte, error)

func validateConfiguredWorkerInspection(
	inspection imageInspection,
	exactReference, platform, commit string,
) error {
	foundDigest := false
	for _, candidate := range inspection.RepositoryDigests {
		if candidate == exactReference {
			foundDigest = true
			break
		}
	}
	if !foundDigest || inspection.OS+"/"+inspection.Architecture != platform ||
		inspection.Config.User != "65532:65532" || len(inspection.Config.Entrypoint) != 0 ||
		len(inspection.Config.Cmd) != 0 {
		return errors.New("configured worker image does not match exact Task 7 identity")
	}
	wantLabels := map[string]string{
		"org.opencontainers.image.revision": commit,
		"org.opencontainers.image.source":   "https://github.com/araihu/paje",
		"io.araihu.paje.codex.version":      acceptanceCodexVersion,
		"io.araihu.paje.go.version":         acceptanceGoVersion,
		"io.araihu.paje.git.version":        acceptanceGitVersion,
		"io.araihu.paje.node.version":       acceptanceNodeVersion,
	}
	for key, value := range wantLabels {
		if inspection.Config.Labels[key] != value {
			return fmt.Errorf("configured worker image label %s does not match Task 7 identity", key)
		}
	}
	return nil
}

func validateConfiguredWorkerCommands(probe workerImageProbe) error {
	checks := []struct {
		executable string
		arguments  []string
		want       string
		contains   bool
	}{
		{executable: "codex", arguments: []string{"--version"}, want: "codex-cli " + acceptanceCodexVersion},
		{executable: "node", arguments: []string{"--version"}, want: "v" + acceptanceNodeVersion},
		{executable: "go", arguments: []string{"version"}, want: "go" + acceptanceGoVersion, contains: true},
		{executable: "git", arguments: []string{"--version"}, want: "git version " + acceptanceGitVersion},
		{
			executable: "/bin/sh",
			arguments: []string{
				"-c", `if command -v "$1" >/dev/null 2>&1; then printf present; else printf absent; fi`,
				"task7-command-boundary", "paje-sandbox-init",
			},
			want: "present",
		},
		{
			executable: "/bin/sh",
			arguments: []string{
				"-c", `if command -v "$1" >/dev/null 2>&1; then printf present; else printf absent; fi`,
				"task7-command-boundary", "paje",
			},
			want: "absent",
		},
	}
	for _, check := range checks {
		output, err := probe(check.executable, check.arguments)
		if err != nil {
			return fmt.Errorf("configured worker image cannot execute %s probe: %w", check.executable, err)
		}
		got := strings.TrimSpace(string(output))
		if check.contains {
			if !strings.Contains(got, check.want) {
				return fmt.Errorf("configured worker image %s probe output does not contain %q", check.executable, check.want)
			}
		} else if got != check.want {
			return fmt.Errorf("configured worker image %s probe output = %q, want %q", check.executable, got, check.want)
		}
	}
	return nil
}

func TestConfiguredWorkerInspectionRejectsIncompleteIdentity(t *testing.T) {
	exactReference := "registry.example/paje-worker@sha256:" + strings.Repeat("a", 64)
	commit := strings.Repeat("b", 40)
	valid := imageInspection{
		RepositoryDigests: []string{exactReference},
		OS:                "linux",
		Architecture:      "arm64",
		Config: imageConfig{
			User: "65532:65532",
			Labels: map[string]string{
				"org.opencontainers.image.revision": commit,
				"org.opencontainers.image.source":   "https://github.com/araihu/paje",
				"io.araihu.paje.codex.version":      acceptanceCodexVersion,
				"io.araihu.paje.go.version":         acceptanceGoVersion,
				"io.araihu.paje.git.version":        acceptanceGitVersion,
				"io.araihu.paje.node.version":       acceptanceNodeVersion,
			},
		},
	}
	tests := []struct {
		name   string
		mutate func(*imageInspection)
	}{
		{name: "missing source label", mutate: func(value *imageInspection) {
			delete(value.Config.Labels, "org.opencontainers.image.source")
		}},
		{name: "missing Go label", mutate: func(value *imageInspection) {
			delete(value.Config.Labels, "io.araihu.paje.go.version")
		}},
		{name: "missing Codex label", mutate: func(value *imageInspection) {
			delete(value.Config.Labels, "io.araihu.paje.codex.version")
		}},
		{name: "wrong Git label", mutate: func(value *imageInspection) {
			value.Config.Labels["io.araihu.paje.git.version"] = "0.0.0"
		}},
		{name: "missing Node label", mutate: func(value *imageInspection) {
			delete(value.Config.Labels, "io.araihu.paje.node.version")
		}},
		{name: "service entrypoint", mutate: func(value *imageInspection) {
			value.Config.Entrypoint = []string{"/usr/local/bin/paje"}
		}},
		{name: "service command", mutate: func(value *imageInspection) {
			value.Config.Cmd = []string{"serve"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := valid
			inspection.RepositoryDigests = append([]string(nil), valid.RepositoryDigests...)
			inspection.Config.Labels = make(map[string]string, len(valid.Config.Labels))
			for key, value := range valid.Config.Labels {
				inspection.Config.Labels[key] = value
			}
			test.mutate(&inspection)
			if err := validateConfiguredWorkerInspection(
				inspection, exactReference, "linux/arm64", commit,
			); err == nil {
				t.Fatal("configured worker validation accepted an incomplete or service-shaped image")
			}
		})
	}
}

func TestConfiguredWorkerCommandBoundaryRejectsWrongWorkload(t *testing.T) {
	validProbe := func(executable string, arguments []string) ([]byte, error) {
		switch executable {
		case "codex":
			return []byte("codex-cli " + acceptanceCodexVersion + "\n"), nil
		case "node":
			return []byte("v" + acceptanceNodeVersion + "\n"), nil
		case "go":
			return []byte("go version go" + acceptanceGoVersion + " linux/arm64\n"), nil
		case "git":
			return []byte("git version " + acceptanceGitVersion + "\n"), nil
		case "/bin/sh":
			command := arguments[len(arguments)-1]
			if command == "paje-sandbox-init" {
				return []byte("present"), nil
			}
			if command == "paje" {
				return []byte("absent"), nil
			}
		}
		return nil, fmt.Errorf("unexpected configured worker probe: %s %q", executable, arguments)
	}
	if err := validateConfiguredWorkerCommands(validProbe); err != nil {
		t.Fatalf("complete worker command boundary rejected: %v", err)
	}

	tests := []struct {
		name  string
		probe workerImageProbe
	}{
		{
			name: "missing Codex harness",
			probe: func(executable string, arguments []string) ([]byte, error) {
				if executable == "codex" {
					return nil, errors.New("command not found")
				}
				return validProbe(executable, arguments)
			},
		},
		{
			name: "wrong Node version",
			probe: func(executable string, arguments []string) ([]byte, error) {
				if executable == "node" {
					return []byte("v0.0.0\n"), nil
				}
				return validProbe(executable, arguments)
			},
		},
		{
			name: "missing Go toolchain",
			probe: func(executable string, arguments []string) ([]byte, error) {
				if executable == "go" {
					return nil, errors.New("command not found")
				}
				return validProbe(executable, arguments)
			},
		},
		{
			name: "wrong Git version",
			probe: func(executable string, arguments []string) ([]byte, error) {
				if executable == "git" {
					return []byte("git version 0.0.0\n"), nil
				}
				return validProbe(executable, arguments)
			},
		},
		{
			name: "missing sandbox init",
			probe: func(executable string, arguments []string) ([]byte, error) {
				if executable == "/bin/sh" && arguments[len(arguments)-1] == "paje-sandbox-init" {
					return []byte("absent"), nil
				}
				return validProbe(executable, arguments)
			},
		},
		{
			name: "coordinator binary present",
			probe: func(executable string, arguments []string) ([]byte, error) {
				if executable == "/bin/sh" && arguments[len(arguments)-1] == "paje" {
					return []byte("present"), nil
				}
				return validProbe(executable, arguments)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateConfiguredWorkerCommands(test.probe); err == nil {
				t.Fatal("configured worker validation accepted the wrong workload command boundary")
			}
		})
	}
}

func TestDockerBuiltImageCleanupFailureFailsTest(t *testing.T) {
	const childEnvironment = "PAJE_TASK7_CLEANUP_FAILURE_CHILD"
	if os.Getenv(childEnvironment) == "1" {
		directory := t.TempDir()
		dockerPath := filepath.Join(directory, "docker")
		script := `#!/bin/sh
case "$1:$2" in
  build:*) exit 0 ;;
  image:inspect) printf '%s' '{"User":"65532:65532","Entrypoint":[],"Cmd":[],"Labels":{}}'; exit 0 ;;
  image:rm) printf '%s\n' 'forced cleanup failure' >&2; exit 23 ;;
  *) printf 'unexpected fake docker command: %s\n' "$*" >&2; exit 24 ;;
esac
`
		if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
		docker := dockerAcceptance{
			endpoint:       "unix:///fake-task7-docker.sock",
			platform:       "linux/arm64",
			repositoryRoot: repositoryRoot(t),
			commit:         strings.Repeat("a", 40),
		}
		docker.buildImage(t, "Dockerfile.worker-codex", "cleanup-failure")
		return
	}

	command := exec.Command(os.Args[0], "-test.run=^TestDockerBuiltImageCleanupFailureFailsTest$")
	command.Env = append(os.Environ(), childEnvironment+"=1")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("cleanup failure did not fail the child test:\n%s", output)
	}
	if !strings.Contains(string(output), "cleanup Task 7 image") {
		t.Fatalf("child test failed without reporting the owned-resource cleanup error: %v\n%s", err, output)
	}
}

func TestImageProbeCleansDelayedAutoRemove(t *testing.T) {
	directory := t.TempDir()
	dockerPath := filepath.Join(directory, "docker")
	statePath := filepath.Join(directory, "listed")
	script := `#!/bin/sh
case "$1" in
  run) printf '%s' 'probe-ok'; exit 0 ;;
  ps)
    if [ ! -e "$PAJE_TASK7_FAKE_DOCKER_STATE" ]; then
      : > "$PAJE_TASK7_FAKE_DOCKER_STATE"
      printf '%s\n' 'probe-container-id'
    fi
    exit 0
    ;;
  rm) exit 0 ;;
  *) printf 'unexpected fake docker command: %s\n' "$*" >&2; exit 24 ;;
esac
`
	if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PAJE_TASK7_FAKE_DOCKER_STATE", statePath)
	docker := dockerAcceptance{
		endpoint:       "unix:///fake-task7-docker.sock",
		repositoryRoot: repositoryRoot(t),
	}
	output, err := docker.runImageCommand(t, "example.invalid/worker@sha256:"+strings.Repeat("a", 64), "node", []string{"--version"})
	if err != nil || string(output) != "probe-ok" {
		t.Fatalf("delayed Docker --rm cleanup = %q, %v", output, err)
	}
}

func TestRevisionBuildStageUsesUniqueOwnedTags(t *testing.T) {
	directory := t.TempDir()
	dockerPath := filepath.Join(directory, "docker")
	argumentsPath := filepath.Join(directory, "build-arguments")
	script := `#!/bin/sh
case "$1:$2" in
  build:*) printf '%s\n' "$*" >> "$PAJE_TASK7_FAKE_DOCKER_ARGUMENTS"; exit 0 ;;
  image:rm) exit 0 ;;
  image:ls) exit 0 ;;
  *) printf 'unexpected fake docker command: %s\n' "$*" >&2; exit 24 ;;
esac
`
	if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PAJE_TASK7_FAKE_DOCKER_ARGUMENTS", argumentsPath)
	docker := dockerAcceptance{
		endpoint:       "unix:///fake-task7-docker.sock",
		platform:       "linux/arm64",
		repositoryRoot: repositoryRoot(t),
		commit:         strings.Repeat("a", 40),
	}
	for range 2 {
		if _, err := docker.buildRevisionStage(t, "Dockerfile.worker-codex", docker.commit); err != nil {
			t.Fatalf("build revision stage: %v", err)
		}
	}
	argumentBytes, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(argumentBytes)), "\n")
	if len(lines) != 2 {
		t.Fatalf("revision build invocations = %d, want 2: %q", len(lines), argumentBytes)
	}
	tags := make(map[string]struct{}, 2)
	for _, line := range lines {
		fields := strings.Fields(line)
		assertCommandArgumentPair(t, fields, "--platform", docker.platform)
		assertCommandArgumentPair(t, fields, "--label", acceptanceTaskLabel+"=task7")
		assertCommandArgumentPair(t, fields, "--label", acceptanceResourceLabel+"=revision-image")
		tag := commandArgumentValue(t, fields, "--tag")
		if !strings.HasPrefix(tag, "paje-task7-revision:") {
			t.Fatalf("revision build tag = %q", tag)
		}
		tags[tag] = struct{}{}
	}
	if len(tags) != 2 {
		t.Fatalf("revision build tags were not unique: %v", tags)
	}
}

func TestRevisionBuildStageCleanupFailureFailsTest(t *testing.T) {
	const childEnvironment = "PAJE_TASK7_REVISION_CLEANUP_FAILURE_CHILD"
	if os.Getenv(childEnvironment) == "1" {
		directory := t.TempDir()
		dockerPath := filepath.Join(directory, "docker")
		script := `#!/bin/sh
case "$1:$2" in
  build:*) exit 0 ;;
  image:rm) printf '%s\n' 'forced revision cleanup failure' >&2; exit 23 ;;
  *) printf 'unexpected fake docker command: %s\n' "$*" >&2; exit 24 ;;
esac
`
		if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
		docker := dockerAcceptance{
			endpoint:       "unix:///fake-task7-docker.sock",
			platform:       "linux/arm64",
			repositoryRoot: repositoryRoot(t),
			commit:         strings.Repeat("a", 40),
		}
		if _, err := docker.buildRevisionStage(t, "Dockerfile.worker-codex", docker.commit); err != nil {
			t.Fatalf("unexpected revision build error: %v", err)
		}
		return
	}

	command := exec.Command(os.Args[0], "-test.run=^TestRevisionBuildStageCleanupFailureFailsTest$")
	command.Env = append(os.Environ(), childEnvironment+"=1")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("revision cleanup failure did not fail the child test:\n%s", output)
	}
	if !strings.Contains(string(output), "cleanup Task 7 revision image") {
		t.Fatalf("child test failed without reporting revision cleanup: %v\n%s", err, output)
	}
}

func assertCommandArgumentPair(t *testing.T, arguments []string, name, value string) {
	t.Helper()
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name && arguments[index+1] == value {
			return
		}
	}
	t.Fatalf("command arguments %q do not contain %q %q", arguments, name, value)
}

func commandArgumentValue(t *testing.T, arguments []string, name string) string {
	t.Helper()
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	t.Fatalf("command arguments %q do not contain %q", arguments, name)
	return ""
}

func TestDockerRevisionBuildArgumentValidation(t *testing.T) {
	docker := requireDockerAcceptance(t)
	tests := []struct {
		name    string
		commit  string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "unknown", commit: "unknown", wantErr: true},
		{name: "short", commit: strings.Repeat("a", 39), wantErr: true},
		{name: "uppercase", commit: strings.Repeat("A", 40), wantErr: true},
		{name: "non-hex", commit: strings.Repeat("g", 40), wantErr: true},
		{name: "full lowercase SHA", commit: strings.Repeat("a", 40)},
	}
	for _, dockerfile := range []string{"Dockerfile", "Dockerfile.worker-codex"} {
		t.Run(filepath.Base(dockerfile), func(t *testing.T) {
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					output, err := docker.buildRevisionStage(t, dockerfile, test.commit)
					if test.wantErr && err == nil {
						t.Fatalf("docker build accepted PAJE_COMMIT=%q", test.commit)
					}
					if !test.wantErr && err != nil {
						t.Fatalf("docker build rejected full lowercase SHA: %v\n%s", err, output)
					}
				})
			}
		})
	}
}

func TestCoordinatorAndWorkerImageSeparation(t *testing.T) {
	docker := requireDockerAcceptance(t)
	coordinator := docker.buildImage(t, "Dockerfile", "coordinator")
	worker := docker.buildImage(t, "Dockerfile.worker-codex", "worker")

	for _, command := range []string{"paje", "paje-leaf-gateway", "git", "ssh"} {
		assertCommandPresent(t, docker, coordinator.reference, command)
	}
	for _, command := range []string{"codex", "node", "go", "paje-sandbox-init"} {
		assertCommandMissing(t, docker, coordinator.reference, command)
	}
	for _, command := range []string{"codex", "node", "go", "git", "paje-sandbox-init"} {
		assertCommandPresent(t, docker, worker.reference, command)
	}
	assertCommandMissing(t, docker, worker.reference, "paje")

	if coordinator.config.User != "65532:65532" || worker.config.User != "65532:65532" {
		t.Fatalf("image users = coordinator %q worker %q, want exact 65532:65532",
			coordinator.config.User, worker.config.User)
	}
	if got := coordinator.config.Entrypoint; len(got) != 1 || got[0] != "/usr/local/bin/paje" {
		t.Fatalf("coordinator entrypoint = %q", got)
	}
	if len(worker.config.Entrypoint) != 0 {
		t.Fatalf("worker image has service entrypoint %q", worker.config.Entrypoint)
	}

	assertImageLabels(t, coordinator.config.Labels, docker.commit, false)
	assertImageLabels(t, worker.config.Labels, docker.commit, true)
	assertImageCommandOutput(t, docker, worker.reference, "codex", []string{"--version"}, "codex-cli "+acceptanceCodexVersion)
	assertImageCommandOutput(t, docker, worker.reference, "node", []string{"--version"}, "v"+acceptanceNodeVersion)
	assertImageCommandContains(t, docker, worker.reference, "go", []string{"version"}, "go"+acceptanceGoVersion)
	assertImageCommandOutput(t, docker, worker.reference, "git", []string{"--version"}, "git version "+acceptanceGitVersion)
}

func TestWorkerProfileAndSecretBindingExamples(t *testing.T) {
	repositoryRoot := repositoryRoot(t)
	profileTemplate, err := os.ReadFile(filepath.Join(
		repositoryRoot, "deploy", "worker-profiles", "codex-go-v1.yaml.tmpl",
	))
	if err != nil {
		t.Fatalf("read worker profile template: %v", err)
	}
	rendered := strings.NewReplacer(
		"${IMAGE_REFERENCE}", "registry.example/paje-worker-codex-go",
		"${IMAGE_DIGEST}", "sha256:"+strings.Repeat("a", 64),
		"${PLATFORM}", "linux/arm64",
	).Replace(string(profileTemplate))
	if strings.Contains(rendered, "${") {
		t.Fatalf("rendered worker profile retains a template token:\n%s", rendered)
	}

	profileDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDirectory, "codex-go-v1.yaml"), []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles, err := workerprofilefilesystem.New(profileDirectory, workerprofile.LimitsForTests())
	if err != nil {
		t.Fatalf("load rendered worker profile: %v", err)
	}
	profile, err := profiles.Resolve(context.Background(), workerprofile.ProfileID{Name: "codex-go", Revision: 1})
	if err != nil {
		t.Fatalf("resolve rendered worker profile: %v", err)
	}
	if profile.Runtime.Image != "registry.example/paje-worker-codex-go@sha256:"+strings.Repeat("a", 64) ||
		profile.Runtime.Platform != "linux/arm64" ||
		profile.Harness != (workerprofile.Harness{ID: "codex", Version: acceptanceCodexVersion}) ||
		profile.Digest == "" {
		t.Fatalf("rendered worker profile identity = %#v", profile)
	}
	assertToolDeclaration(t, profile, "git", acceptanceGitVersion, "git version "+acceptanceGitVersion)
	assertToolDeclaration(t, profile, "go", acceptanceGoVersion, "go"+acceptanceGoVersion)
	assertToolDeclaration(t, profile, "node", acceptanceNodeVersion, "v"+acceptanceNodeVersion)

	bindingDirectory := t.TempDir()
	bindingBytes, err := os.ReadFile(filepath.Join(
		repositoryRoot, "deploy", "secret-bindings", "example.yaml",
	))
	if err != nil {
		t.Fatalf("read example secret binding: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bindingDirectory, "example.yaml"), bindingBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	bindings, err := secretfilesystem.New(bindingDirectory)
	if err != nil {
		t.Fatalf("load example secret binding: %v", err)
	}
	requirement := workerprofile.SecretRequirement{
		Capability:      "harness.codex-auth",
		BindingRevision: 1,
		Stage:           workerprofile.StageAgent,
		Delivery:        workerprofile.DeliveryDirectory,
		Target:          "/run/paje/secrets/codex",
		Required:        true,
	}
	binding, err := bindings.Resolve(context.Background(), secret.ResolveRequest{
		ProfileID: profile.Metadata,
		Ref: secret.BindingRef{
			Capability: requirement.Capability,
			Revision:   1,
		},
		Requirement: requirement,
	})
	if err != nil {
		t.Fatalf("resolve example secret binding: %v", err)
	}
	provider, reference := binding.Source()
	if provider != "filesystem" || reference != "/etc/paje/secrets/codex" {
		t.Fatalf("example secret binding source = %q %q", provider, reference)
	}
}

func requireDockerAcceptance(t *testing.T) dockerAcceptance {
	t.Helper()
	requireOptIn(t, "PAJE_DOCKER_ACCEPTANCE", "the real Docker portable-runtime acceptance suite")
	endpoint := strings.TrimSpace(os.Getenv("PAJE_DOCKER_TEST_ENDPOINT"))
	if endpoint == "" {
		t.Fatal("set PAJE_DOCKER_TEST_ENDPOINT to an explicit local Unix socket")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("opted-in Docker acceptance requires docker on PATH")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "unix" || parsed.Host != "" || parsed.Path == "" ||
		!filepath.IsAbs(parsed.Path) || filepath.Clean(parsed.Path) != parsed.Path {
		t.Fatal("PAJE_DOCKER_TEST_ENDPOINT must be an explicit absolute local Unix socket")
	}
	info, err := os.Stat(parsed.Path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("configured Docker endpoint is not an available Unix socket: %v", err)
	}

	result := dockerAcceptance{
		endpoint:       endpoint,
		repositoryRoot: repositoryRoot(t),
	}
	version, err := result.output(t, 20*time.Second, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
	if err != nil {
		t.Fatalf("configured Docker Engine is unavailable: %v", err)
	}
	result.platform = strings.TrimSpace(string(version))
	if !strings.HasPrefix(result.platform, "linux/") {
		t.Fatalf("configured Docker Engine platform = %q, want linux architecture", result.platform)
	}
	commit, err := commandOutput(t, 10*time.Second, result.repositoryRoot,
		nil, "git", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		t.Fatalf("resolve source revision: %v", err)
	}
	result.commit = strings.TrimSpace(string(commit))
	if len(result.commit) != 40 {
		t.Fatalf("source revision = %q, want full Git commit", result.commit)
	}
	result.registerOwnedResourceLeakCheck(t)
	return result
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(root)
}

func (docker dockerAcceptance) buildImage(t *testing.T, dockerfile, purpose string) builtImage {
	t.Helper()
	tag := "paje-task7-" + sanitizeDockerName(purpose) + ":" + uniqueDockerName(t)
	output, err := docker.output(t, 8*time.Minute,
		"build",
		"--platform", docker.platform,
		"--progress=plain",
		"--file", filepath.Join(docker.repositoryRoot, dockerfile),
		"--build-arg", "PAJE_COMMIT="+docker.commit,
		"--label", acceptanceTaskLabel+"=task7",
		"--label", acceptanceResourceLabel+"=image",
		"--tag", tag,
		docker.repositoryRoot,
	)
	if err != nil {
		t.Fatalf("build %s: %v\n%s", dockerfile, err, output)
	}
	docker.registerOwnedResourceCleanup(t, "image "+tag,
		[]string{"image", "rm", "--force", tag},
		[]string{"image", "ls", "--filter", "reference=" + tag, "--format", "{{.ID}}"},
	)
	configBytes, err := docker.output(t, 20*time.Second, "image", "inspect", "--format", "{{json .Config}}", tag)
	if err != nil {
		t.Fatalf("inspect built image %q: %v", tag, err)
	}
	var config imageConfig
	if err := json.Unmarshal(configBytes, &config); err != nil {
		t.Fatalf("decode built image configuration: %v: %s", err, configBytes)
	}
	return builtImage{reference: tag, config: config}
}

func (docker dockerAcceptance) buildRevisionStage(
	t *testing.T,
	dockerfile, commit string,
) ([]byte, error) {
	t.Helper()
	tag := "paje-task7-revision:" + uniqueDockerName(t)
	args := []string{
		"build",
		"--platform", docker.platform,
		"--target", "revision",
		"--progress=plain",
		"--file", filepath.Join(docker.repositoryRoot, dockerfile),
		"--label", acceptanceTaskLabel + "=task7",
		"--label", acceptanceResourceLabel + "=revision-image",
		"--tag", tag,
	}
	if commit != "" {
		args = append(args, "--build-arg", "PAJE_COMMIT="+commit)
	}
	args = append(args, docker.repositoryRoot)
	output, err := docker.output(t, 2*time.Minute, args...)
	if err != nil {
		return output, err
	}
	docker.registerOwnedResourceCleanup(t, "revision image "+tag,
		[]string{"image", "rm", "--force", tag},
		[]string{"image", "ls", "--filter", "reference=" + tag, "--format", "{{.ID}}"},
	)
	return output, nil
}

func (docker dockerAcceptance) publishWorker(t *testing.T) publishedWorker {
	t.Helper()
	if configured := strings.TrimSpace(os.Getenv("PAJE_DOCKER_TEST_IMAGE")); configured != "" {
		return docker.configuredWorker(t, configured)
	}
	worker := docker.buildImage(t, "Dockerfile.worker-codex", "published-worker")
	registryName := "paje-task7-registry-" + uniqueDockerName(t)
	output, err := docker.output(t, 2*time.Minute,
		"run", "--detach", "--name", registryName,
		"--label", acceptanceTaskLabel+"=task7",
		"--label", acceptanceResourceLabel+"=registry",
		"--publish", "127.0.0.1::5000",
		"--tmpfs", "/var/lib/registry:rw,nosuid,nodev,noexec,size=2147483648",
		"registry:2.8.3",
	)
	if err != nil {
		t.Fatalf("start temporary local registry: %v\n%s", err, output)
	}
	docker.registerOwnedResourceCleanup(t, "registry container "+registryName,
		[]string{"rm", "--force", registryName},
		[]string{"ps", "--all", "--filter", "name=^/" + registryName + "$", "--format", "{{.ID}}"},
	)

	portOutput, err := docker.output(t, 20*time.Second, "port", registryName, "5000/tcp")
	if err != nil {
		t.Fatalf("resolve temporary registry port: %v", err)
	}
	hostPort := strings.TrimSpace(string(portOutput))
	index := strings.LastIndexByte(hostPort, ':')
	if index < 0 {
		t.Fatalf("temporary registry port = %q", hostPort)
	}
	if _, err := strconv.Atoi(hostPort[index+1:]); err != nil {
		t.Fatalf("temporary registry port = %q", hostPort)
	}
	repository := "127.0.0.1:" + hostPort[index+1:] + "/paje-worker-codex-go"
	mutableReference := repository + ":acceptance"
	if output, err := docker.output(t, 30*time.Second, "tag", worker.reference, mutableReference); err != nil {
		t.Fatalf("tag temporary worker image: %v\n%s", err, output)
	}
	docker.registerOwnedResourceCleanup(t, "image tag "+mutableReference,
		[]string{"image", "rm", "--force", mutableReference},
		[]string{"image", "ls", "--filter", "reference=" + mutableReference, "--format", "{{.ID}}"},
	)
	var pushOutput []byte
	var pushErr error
	for attempt := 0; attempt < 5; attempt++ {
		pushOutput, pushErr = docker.output(t, 2*time.Minute, "push", mutableReference)
		if pushErr == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if pushErr != nil {
		t.Fatalf("push worker to temporary registry: %v\n%s", pushErr, pushOutput)
	}
	digests, err := docker.output(t, 20*time.Second,
		"image", "inspect", "--format", "{{json .RepoDigests}}", mutableReference)
	if err != nil {
		t.Fatalf("inspect temporary worker repository digests: %v", err)
	}
	var repositoryDigests []string
	if err := json.Unmarshal(digests, &repositoryDigests); err != nil {
		t.Fatalf("decode temporary worker repository digests: %v: %s", err, digests)
	}
	exactReference := ""
	for _, candidate := range repositoryDigests {
		if strings.HasPrefix(candidate, repository+"@sha256:") {
			exactReference = candidate
			break
		}
	}
	if exactReference == "" {
		t.Fatalf("temporary worker has no immutable repository digest: %q", repositoryDigests)
	}

	profile := renderWorkerProfile(t, docker.repositoryRoot, repository,
		strings.TrimPrefix(exactReference, repository+"@"), docker.platform)
	if profile.Runtime.Image != exactReference {
		t.Fatalf("rendered live profile image = %q, want %q", profile.Runtime.Image, exactReference)
	}
	return publishedWorker{docker: docker, image: exactReference, profile: profile}
}

func (docker dockerAcceptance) configuredWorker(t *testing.T, exactReference string) publishedWorker {
	t.Helper()
	repository, digest, ok := strings.Cut(exactReference, "@")
	if !ok || repository == "" || !strings.HasPrefix(digest, "sha256:") {
		t.Fatal("PAJE_DOCKER_TEST_IMAGE must be an exact repository digest")
	}
	output, err := docker.output(t, 20*time.Second, "image", "inspect", exactReference)
	if err != nil {
		t.Fatalf("inspect configured exact worker image: %v", err)
	}
	var values []imageInspection
	if err := json.Unmarshal(output, &values); err != nil || len(values) != 1 {
		t.Fatalf("decode configured worker image inspection: %v: %s", err, output)
	}
	inspection := values[0]
	if err := validateConfiguredWorkerInspection(
		inspection, exactReference, docker.platform, docker.commit,
	); err != nil {
		t.Fatalf("configured worker image does not match exact Task 7 identity: %v: %#v", err, inspection)
	}
	commandCacheKey := strings.Join([]string{
		docker.endpoint, exactReference, docker.platform, docker.commit,
	}, "\x00")
	if _, validated := configuredWorkerCommandCache.Load(commandCacheKey); !validated {
		if err := validateConfiguredWorkerCommands(func(executable string, arguments []string) ([]byte, error) {
			return docker.runImageCommand(t, exactReference, executable, arguments)
		}); err != nil {
			t.Fatalf("configured worker image does not match the Task 7 workload boundary: %v", err)
		}
		configuredWorkerCommandCache.Store(commandCacheKey, struct{}{})
	}
	profile := renderWorkerProfile(t, docker.repositoryRoot, repository, digest, docker.platform)
	return publishedWorker{docker: docker, image: exactReference, profile: profile}
}

func renderWorkerProfile(t *testing.T, repositoryRoot, imageReference, imageDigest, platform string) workerprofile.Snapshot {
	t.Helper()
	templateBytes, err := os.ReadFile(filepath.Join(
		repositoryRoot, "deploy", "worker-profiles", "codex-go-v1.yaml.tmpl",
	))
	if err != nil {
		t.Fatalf("read worker profile template: %v", err)
	}
	rendered := strings.NewReplacer(
		"${IMAGE_REFERENCE}", imageReference,
		"${IMAGE_DIGEST}", imageDigest,
		"${PLATFORM}", platform,
	).Replace(string(templateBytes))
	if strings.Contains(rendered, "${") {
		t.Fatalf("live worker profile retains a template token:\n%s", rendered)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "codex-go-v1.yaml"), []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := workerprofilefilesystem.New(directory, workerprofile.LimitsForTests())
	if err != nil {
		t.Fatalf("load rendered live worker profile: %v", err)
	}
	profile, err := registry.Resolve(context.Background(), workerprofile.ProfileID{Name: "codex-go", Revision: 1})
	if err != nil {
		t.Fatalf("resolve rendered live worker profile: %v", err)
	}
	return profile
}

func (docker dockerAcceptance) output(t *testing.T, timeout time.Duration, args ...string) ([]byte, error) {
	t.Helper()
	return docker.outputRaw(timeout, args...)
}

func (docker dockerAcceptance) outputRaw(timeout time.Duration, args ...string) ([]byte, error) {
	return commandOutput(nil, timeout, docker.repositoryRoot,
		map[string]string{
			"DOCKER_HOST":    docker.endpoint,
			"DOCKER_CONTEXT": "",
		},
		"docker", args...,
	)
}

func (docker dockerAcceptance) registerOwnedResourceCleanup(
	t *testing.T,
	description string,
	removeArgs, verifyArgs []string,
) {
	t.Helper()
	t.Cleanup(func() {
		output, err := docker.outputRaw(45*time.Second, removeArgs...)
		if err != nil {
			t.Errorf("cleanup Task 7 %s: %v\n%s", description, err, output)
			return
		}
		remaining, err := docker.outputRaw(20*time.Second, verifyArgs...)
		if err != nil {
			t.Errorf("verify Task 7 %s cleanup: %v\n%s", description, err, remaining)
			return
		}
		if strings.TrimSpace(string(remaining)) != "" {
			t.Errorf("Task 7 %s remains after cleanup: %s", description, remaining)
		}
	})
}

func (docker dockerAcceptance) registerOwnedResourceLeakCheck(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		checks := []struct {
			kind string
			args []string
		}{
			{kind: "containers", args: []string{"ps", "--all", "--filter", "label=" + acceptanceTaskLabel + "=task7", "--format", "{{.ID}} {{.Names}}"}},
			{kind: "images", args: []string{"image", "ls", "--filter", "label=" + acceptanceTaskLabel + "=task7", "--format", "{{.ID}} {{.Repository}}:{{.Tag}}"}},
			{kind: "networks", args: []string{"network", "ls", "--filter", "label=" + acceptanceTaskLabel + "=task7", "--format", "{{.ID}} {{.Name}}"}},
		}
		for _, check := range checks {
			output, err := docker.outputRaw(20*time.Second, check.args...)
			if err != nil {
				t.Errorf("verify Task 7 owned %s are absent: %v\n%s", check.kind, err, output)
				continue
			}
			if strings.TrimSpace(string(output)) != "" {
				t.Errorf("Task 7 owned %s remain after cleanup: %s", check.kind, output)
			}
		}
	})
}

func (docker dockerAcceptance) runImageCommand(
	t *testing.T,
	image, executable string,
	arguments []string,
) ([]byte, error) {
	t.Helper()
	containerName := "paje-task7-image-probe-" + uniqueDockerName(t)
	commandArguments := append([]string{
		"run", "--rm", "--name", containerName,
		"--label", acceptanceTaskLabel + "=task7",
		"--label", acceptanceResourceLabel + "=image-probe",
		"--network", "none", "--read-only",
		"--tmpfs", "/tmp:rw,nosuid,nodev,exec,size=16m",
		"--tmpfs", "/home/paje:rw,nosuid,nodev,noexec,size=16m,uid=65532,gid=65532",
		"--tmpfs", "/run/paje:rw,nosuid,nodev,noexec,size=16m,uid=65532,gid=65532",
		"--entrypoint", executable, image,
	}, arguments...)
	output, runErr := docker.output(t, 30*time.Second, commandArguments...)
	remaining, listErr := docker.outputRaw(20*time.Second,
		"ps", "--all", "--filter", "name=^/"+containerName+"$", "--format", "{{.ID}}",
	)
	if listErr != nil {
		return output, fmt.Errorf("verify Task 7 image probe cleanup: %w", listErr)
	}
	if strings.TrimSpace(string(remaining)) != "" {
		cleanupOutput, cleanupErr := docker.outputRaw(20*time.Second, "rm", "--force", containerName)
		if cleanupErr != nil {
			return output, fmt.Errorf("cleanup Task 7 image probe: %w: %s", cleanupErr, cleanupOutput)
		}
		remaining, listErr = docker.outputRaw(20*time.Second,
			"ps", "--all", "--filter", "name=^/"+containerName+"$", "--format", "{{.ID}}",
		)
		if listErr != nil {
			return output, fmt.Errorf("verify explicit Task 7 image probe cleanup: %w", listErr)
		}
		if strings.TrimSpace(string(remaining)) != "" {
			return output, errors.New("Task 7 image probe container remains after explicit cleanup")
		}
	}
	return output, runErr
}

func commandOutput(
	t *testing.T,
	timeout time.Duration,
	directory string,
	environment map[string]string,
	name string,
	args ...string,
) ([]byte, error) {
	if t != nil {
		t.Helper()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	if environment != nil {
		command.Env = acceptanceEnvironment(environment)
	}
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return output, fmt.Errorf("%s timed out: %w", name, ctx.Err())
	}
	if err != nil {
		return output, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return output, nil
}

func assertCommandPresent(t *testing.T, docker dockerAcceptance, image, command string) {
	t.Helper()
	script := `command -v "$1" >/dev/null`
	output, err := docker.runImageCommand(t, image, "/bin/sh", []string{"-c", script, "assert-command", command})
	if err != nil {
		t.Fatalf("image %q is missing command %q: %v\n%s", image, command, err, output)
	}
}

func assertCommandMissing(t *testing.T, docker dockerAcceptance, image, command string) {
	t.Helper()
	script := `! command -v "$1" >/dev/null`
	output, err := docker.runImageCommand(t, image, "/bin/sh", []string{"-c", script, "assert-command", command})
	if err != nil {
		t.Fatalf("image %q unexpectedly contains command %q: %v\n%s", image, command, err, output)
	}
}

func assertImageCommandOutput(
	t *testing.T,
	docker dockerAcceptance,
	image, command string,
	args []string,
	want string,
) {
	t.Helper()
	output, err := docker.runImageCommand(t, image, command, args)
	if err != nil {
		t.Fatalf("run %s in image %q: %v\n%s", command, image, err, output)
	}
	if got := strings.TrimSpace(string(output)); got != want {
		t.Fatalf("%s output = %q, want %q", command, got, want)
	}
}

func assertImageCommandContains(
	t *testing.T,
	docker dockerAcceptance,
	image, command string,
	args []string,
	want string,
) {
	t.Helper()
	output, err := docker.runImageCommand(t, image, command, args)
	if err != nil {
		t.Fatalf("run %s in image %q: %v\n%s", command, image, err, output)
	}
	if got := strings.TrimSpace(string(output)); !strings.Contains(got, want) {
		t.Fatalf("%s output = %q, want substring %q", command, got, want)
	}
}

func assertImageLabels(t *testing.T, labels map[string]string, commit string, worker bool) {
	t.Helper()
	want := map[string]string{
		"org.opencontainers.image.revision": commit,
		"org.opencontainers.image.source":   "https://github.com/araihu/paje",
	}
	if worker {
		want["io.araihu.paje.codex.version"] = acceptanceCodexVersion
		want["io.araihu.paje.go.version"] = acceptanceGoVersion
		want["io.araihu.paje.git.version"] = acceptanceGitVersion
		want["io.araihu.paje.node.version"] = acceptanceNodeVersion
	}
	for key, value := range want {
		if labels[key] != value {
			t.Fatalf("image label %s = %q, want %q", key, labels[key], value)
		}
	}
}

func assertToolDeclaration(t *testing.T, profile workerprofile.Snapshot, name, version, output string) {
	t.Helper()
	for _, tool := range profile.Tools {
		if tool.Name == name {
			if tool.Version != version || tool.Probe.OutputContains != output {
				t.Fatalf("worker tool %q = %#v", name, tool)
			}
			return
		}
	}
	t.Fatalf("worker profile is missing tool %q", name)
}

func uniqueDockerName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%d-%d-%s", os.Getpid(), acceptanceResourceSequence.Add(1), sanitizeDockerName(t.Name()))
}

func sanitizeDockerName(value string) string {
	var result strings.Builder
	for _, character := range strings.ToLower(value) {
		switch {
		case character >= 'a' && character <= 'z':
			result.WriteRune(character)
		case character >= '0' && character <= '9':
			result.WriteRune(character)
		default:
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-")
}

func acceptanceEnvironment(overrides map[string]string) []string {
	result := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := overrides[key]; !replaced {
			result = append(result, item)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}
