package host

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/executor/contracttest"
	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestExecutorContract(t *testing.T) {
	contracttest.Run(t, func(t *testing.T, scenario contracttest.Scenario) contracttest.Fixture {
		t.Helper()
		target, err := New(Config{Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		target.resolve = func(name, _ string, _ map[string]string) (string, error) {
			if name == "missing-host-executable" {
				return "", errors.New("missing host executable")
			}
			return os.Args[0], nil
		}
		request := helperRequest(t, string(scenario))
		fixture := contracttest.Fixture{Executor: target, Request: request}
		switch scenario {
		case contracttest.ScenarioStartFailure:
			request.Command.Executable = "missing-host-executable"
			fixture.Request = request
		case contracttest.ScenarioTimeout:
			request.Command.Args = helperArgs("block")
			request.Timeout = 100 * time.Millisecond
			fixture.Request = request
		case contracttest.ScenarioCancellation:
			request.Command.Args = helperArgs("block", "started")
			fixture.Request = request
			fixture.Started = fileSignal(request.Workspace.HostPath, "started")
		case contracttest.ScenarioDescendantDeath:
			request.Command.Args = helperArgs("descendant", "started", "child.pid")
			fixture.Request = request
			fixture.Started = fileSignal(request.Workspace.HostPath, "started")
			fixture.AssertNoDescendants = assertProcessGone(request.Workspace.HostPath, "child.pid")
		case contracttest.ScenarioBoundedOutput:
			request.Command.Args = helperArgs("bounded-output")
			request.OutputLimit = 128
			fixture.Request = request
		case contracttest.ScenarioSecretIsolation:
			fixture.Request = secretRequest(t)
		}
		return fixture
	})
}

func helperRequest(t *testing.T, scenario string) executor.Request {
	t.Helper()
	request := hostRequest(t)
	request.Environment = map[string]string{
		"PATH": executor.CanonicalSandboxPATH, "GO_WANT_HOST_HELPER": "1",
	}
	request.Command.Args = helperArgs(scenario)
	return request
}

func helperArgs(scenario string, values ...string) []string {
	args := []string{"-test.run=TestHostHelperProcess", "--", scenario}
	return append(args, values...)
}

func secretRequest(t *testing.T) executor.Request {
	t.Helper()
	request := helperRequest(t, "complete")
	requirement := workerprofile.SecretRequirement{
		Capability: "workload.token", BindingRevision: 1, Stage: workerprofile.StageAgent,
		Delivery: workerprofile.DeliveryFile, Target: "/run/paje/secrets/token", Required: true,
	}
	request.Profile = ociProfile(t, []workerprofile.SecretRequirement{requirement})
	request.Attempt.Purpose = executor.PurposeAgent
	materialization, err := secret.NewValueMaterialization(requirement.Delivery, requirement.Target, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	request.Secrets = []secret.Materialization{materialization}
	return request
}

func fileSignal(directory, name string) <-chan struct{} {
	ready := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(directory, name)); err == nil {
				close(ready)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return ready
}

func assertProcessGone(directory, name string) func(*testing.T) {
	return func(t *testing.T) {
		t.Helper()
		encoded, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(encoded)))
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			err = syscall.Kill(pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("descendant process %d remains alive: %v", pid, err)
	}
}

func TestHostHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HOST_HELPER") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(2)
	}
	scenario := os.Args[separator+1]
	values := os.Args[separator+2:]
	switch scenario {
	case string(contracttest.ScenarioComplete):
		fmt.Fprint(os.Stdout, "stdout")
		fmt.Fprint(os.Stderr, "stderr")
	case string(contracttest.ScenarioNonzero):
		os.Exit(7)
	case "bounded-output":
		fmt.Fprint(os.Stdout, strings.Repeat("o", 1024))
		fmt.Fprint(os.Stderr, strings.Repeat("e", 1024))
	case "block":
		if len(values) > 0 {
			_ = os.WriteFile(values[0], []byte("started"), 0o600)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "descendant":
		if len(values) != 2 {
			os.Exit(2)
		}
		child := exec.Command(os.Args[0], "-test.run=TestHostGrandchildProcess")
		child.Env = append(os.Environ(), "GO_WANT_HOST_GRANDCHILD=1")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(values[1], []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		_ = os.WriteFile(values[0], []byte("started"), 0o600)
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestHostGrandchildProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HOST_GRANDCHILD") != "1" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}
