package acceptance

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerAcceptanceConfiguredPrerequisiteFailuresAreFatal(t *testing.T) {
	if os.Getenv("PAJE_DOCKER_PREREQUISITE_HELPER") == "1" {
		_ = requireDockerAcceptance(t)
		return
	}

	tests := []struct {
		name      string
		overrides map[string]string
		wantError string
	}{
		{
			name: "missing endpoint",
			overrides: map[string]string{
				"PAJE_DOCKER_ACCEPTANCE":    "1",
				"PAJE_DOCKER_TEST_ENDPOINT": "",
			},
			wantError: "set PAJE_DOCKER_TEST_ENDPOINT",
		},
		{
			name: "unavailable endpoint",
			overrides: map[string]string{
				"PAJE_DOCKER_ACCEPTANCE":    "1",
				"PAJE_DOCKER_TEST_ENDPOINT": "unix://" + filepath.Join(t.TempDir(), "missing.sock"),
			},
			wantError: "configured Docker endpoint is not an available Unix socket",
		},
		{
			name: "missing Docker client",
			overrides: map[string]string{
				"PAJE_DOCKER_ACCEPTANCE":    "1",
				"PAJE_DOCKER_TEST_ENDPOINT": "unix:///does/not/matter.sock",
				"PATH":                      t.TempDir(),
			},
			wantError: "opted-in Docker acceptance requires docker on PATH",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestDockerAcceptanceConfiguredPrerequisiteFailuresAreFatal$")
			overrides := map[string]string{"PAJE_DOCKER_PREREQUISITE_HELPER": "1"}
			for key, value := range test.overrides {
				overrides[key] = value
			}
			command.Env = acceptanceEnvironment(overrides)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("broken opted-in Docker prerequisite exited successfully: %s", output)
			}
			if !strings.Contains(string(output), test.wantError) || strings.Contains(string(output), "SKIP") {
				t.Fatalf("failure output = %q, want fatal prerequisite %q", output, test.wantError)
			}
		})
	}
}

func TestExactNamedContainerCleanupIsFailClosed(t *testing.T) {
	const childEnvironment = "PAJE_TASK7_EXACT_CONTAINER_CLEANUP_CHILD"
	if mode := os.Getenv(childEnvironment); mode != "" {
		directory := t.TempDir()
		dockerPath := filepath.Join(directory, "docker")
		statePath := filepath.Join(directory, "container-exists")
		script := `#!/bin/sh
case "$1" in
  ps)
    if [ -e "$PAJE_TASK7_FAKE_DOCKER_STATE" ]; then
      printf '%s\n' 'probe-container-id'
    fi
    exit 0
    ;;
  rm)
    if [ "$PAJE_TASK7_FAKE_DOCKER_MODE" = "remove-fails" ]; then
      printf '%s\n' 'forced exact cleanup failure' >&2
      exit 23
    fi
    rm -f "$PAJE_TASK7_FAKE_DOCKER_STATE"
    exit 0
    ;;
  *) printf 'unexpected fake docker command: %s\n' "$*" >&2; exit 24 ;;
esac
`
		if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("PAJE_TASK7_FAKE_DOCKER_STATE", statePath)
		t.Setenv("PAJE_TASK7_FAKE_DOCKER_MODE", mode)
		docker := dockerAcceptance{
			endpoint:       "unix:///fake-task7-docker.sock",
			repositoryRoot: repositoryRoot(t),
		}
		t.Cleanup(func() {
			if _, err := os.Stat(statePath); !os.IsNotExist(err) {
				t.Errorf("exact named container remains after cleanup: %v", err)
			}
		})
		docker.registerExactContainerCleanup(t, "malformed sandbox-init container", "paje-task7-exact-cleanup")
		if err := os.WriteFile(statePath, []byte("exists"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	for _, test := range []struct {
		name        string
		mode        string
		wantFailure bool
	}{
		{name: "removes early failure leak", mode: "remove-succeeds"},
		{name: "remove failure fails test", mode: "remove-fails", wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestExactNamedContainerCleanupIsFailClosed$")
			command.Env = append(os.Environ(), childEnvironment+"="+test.mode)
			output, err := command.CombinedOutput()
			if test.wantFailure {
				if err == nil {
					t.Fatalf("forced exact cleanup failure passed:\n%s", output)
				}
				if !strings.Contains(string(output), "cleanup Task 7 malformed sandbox-init container") {
					t.Fatalf("cleanup failure was not reported: %v\n%s", err, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("exact cleanup child failed: %v\n%s", err, output)
			}
		})
	}
}

func TestSandboxInitRejectsMalformedBootstrapInRealWorker(t *testing.T) {
	worker := requireDockerAcceptance(t).publishWorker(t)
	name := "paje-task7-malformed-init-" + uniqueDockerName(t)
	worker.docker.registerExactContainerCleanup(t, "malformed sandbox-init container", name)
	args := []string{
		"run", "--rm", "--interactive", "--name", name,
		"--label", acceptanceTaskLabel + "=task7",
		"--label", acceptanceResourceLabel + "=sandbox-init-probe",
		"--network", "none",
		"--read-only",
		"--user", "65532:65532",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "32",
		"--memory", "134217728",
		"--tmpfs", "/run/paje:rw,nosuid,nodev,noexec,size=67108864,mode=0700,uid=65532,gid=65532",
		"--tmpfs", "/home/paje:rw,nosuid,nodev,noexec,size=67108864,mode=0700,uid=65532,gid=65532",
		"--tmpfs", "/tmp:rw,nosuid,nodev,exec,size=67108864,mode=0700,uid=65532,gid=65532",
		"--entrypoint", "/usr/local/bin/paje-sandbox-init",
		worker.image,
		"--bootstrap-stdin",
	}
	secretMarker := "malformed-bootstrap-must-not-echo"
	output, err := dockerOutputWithInput(worker.docker, 30*time.Second, []byte(secretMarker), args...)
	if err == nil {
		t.Fatalf("sandbox init accepted malformed bootstrap: %s", output)
	}
	if got := strings.TrimSpace(string(output)); got != "paje-sandbox-init: initialization failed" {
		t.Fatalf("sandbox init failure output = %q", got)
	}
	if bytes.Contains(output, []byte(secretMarker)) {
		t.Fatal("sandbox init failure echoed private malformed input")
	}
	containers, listErr := worker.docker.output(t, 20*time.Second,
		"ps", "--all", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}",
	)
	if listErr != nil || strings.TrimSpace(string(containers)) != "" {
		t.Fatalf("malformed sandbox-init container remains: %v %q", listErr, containers)
	}
}

func (docker dockerAcceptance) registerExactContainerCleanup(
	t *testing.T,
	description, name string,
) {
	t.Helper()
	t.Cleanup(func() {
		listArgs := []string{
			"ps", "--all", "--filter", "name=^/" + name + "$", "--format", "{{.ID}}",
		}
		remaining, err := docker.outputRaw(20*time.Second, listArgs...)
		if err != nil {
			t.Errorf("verify Task 7 %s cleanup prerequisite: %v\n%s", description, err, remaining)
			return
		}
		if strings.TrimSpace(string(remaining)) == "" {
			return
		}
		output, err := docker.outputRaw(20*time.Second, "rm", "--force", name)
		if err != nil {
			t.Errorf("cleanup Task 7 %s: %v\n%s", description, err, output)
			return
		}
		remaining, err = docker.outputRaw(20*time.Second, listArgs...)
		if err != nil {
			t.Errorf("verify Task 7 %s cleanup: %v\n%s", description, err, remaining)
			return
		}
		if strings.TrimSpace(string(remaining)) != "" {
			t.Errorf("Task 7 %s remains after cleanup: %s", description, remaining)
		}
	})
}

func dockerOutputWithInput(
	docker dockerAcceptance,
	timeout time.Duration,
	input []byte,
	args ...string,
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", args...)
	command.Dir = docker.repositoryRoot
	command.Env = acceptanceEnvironment(map[string]string{
		"DOCKER_HOST":    docker.endpoint,
		"DOCKER_CONTEXT": "",
	})
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return output, ctx.Err()
	}
	return output, err
}
