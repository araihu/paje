package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/executor/contracttest"
	"github.com/araihu/paje/internal/executor/dockerengine"
	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
)

type liveContainerInspect struct {
	Config struct {
		User         string            `json:"User"`
		Image        string            `json:"Image"`
		Env          []string          `json:"Env"`
		Entrypoint   []string          `json:"Entrypoint"`
		Cmd          []string          `json:"Cmd"`
		ExposedPorts map[string]any    `json:"ExposedPorts"`
		Labels       map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
		Privileged     bool              `json:"Privileged"`
		PublishAll     bool              `json:"PublishAllPorts"`
		CapAdd         []string          `json:"CapAdd"`
		CapDrop        []string          `json:"CapDrop"`
		SecurityOpt    []string          `json:"SecurityOpt"`
		PortBindings   map[string]any    `json:"PortBindings"`
		Devices        []any             `json:"Devices"`
		Binds          []string          `json:"Binds"`
		Tmpfs          map[string]string `json:"Tmpfs"`
		NetworkMode    string            `json:"NetworkMode"`
		PidMode        string            `json:"PidMode"`
		IpcMode        string            `json:"IpcMode"`
		UTSMode        string            `json:"UTSMode"`
		CgroupnsMode   string            `json:"CgroupnsMode"`
		Memory         int64             `json:"Memory"`
		NanoCPUs       int64             `json:"NanoCpus"`
		PidsLimit      int64             `json:"PidsLimit"`
		LogConfig      struct {
			Type string `json:"Type"`
		} `json:"LogConfig"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
		Propagation string `json:"Propagation"`
	} `json:"Mounts"`
}

type runtimeProbe struct {
	UID               int               `json:"uid"`
	GID               int               `json:"gid"`
	RootReadOnly      bool              `json:"root_read_only"`
	PIDsMax           string            `json:"pids_max"`
	Tmpfs             map[string]string `json:"tmpfs"`
	DockerSocket      bool              `json:"docker_socket"`
	SourceVisible     bool              `json:"source_visible"`
	CredentialVisible bool              `json:"credential_visible"`
}

func TestDisabledDockerLogsProof(t *testing.T) {
	secret := "private-marker"
	tests := []struct {
		name    string
		output  []byte
		err     error
		wantErr bool
	}{
		{
			name:    "arbitrary retrieval failure is rejected",
			output:  []byte("daemon unavailable"),
			err:     errors.New("exit status 1"),
			wantErr: true,
		},
		{
			name:    "unexpected retrieval success is rejected",
			output:  []byte("ordinary workload output"),
			wantErr: true,
		},
		{
			name:   "disabled logging driver response is accepted",
			output: []byte("Error response from daemon: configured logging driver does not support reading"),
			err:    errors.New("exit status 1"),
		},
		{
			name:    "disabled logging driver response containing secret is rejected",
			output:  []byte("configured logging driver does not support reading: " + secret),
			err:     errors.New("exit status 1"),
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateDisabledDockerLogs(test.output, test.err, []string{secret})
			if (err != nil) != test.wantErr {
				t.Fatalf("validate disabled Docker logs = %v, want error %t", err, test.wantErr)
			}
		})
	}
}

func assertDockerLogsDisabled(
	t *testing.T,
	docker dockerAcceptance,
	containerID string,
	secretValues []string,
) {
	t.Helper()
	logs, err := docker.output(t, 20*time.Second, "logs", containerID)
	if err := validateDisabledDockerLogs(logs, err, secretValues); err != nil {
		t.Fatalf("prove Docker log persistence is disabled: %v", err)
	}
}

func validateDisabledDockerLogs(logs []byte, logsErr error, secretValues []string) error {
	diagnostic := string(logs)
	if logsErr != nil {
		diagnostic += "\n" + logsErr.Error()
	}
	for _, secretValue := range secretValues {
		if strings.Contains(diagnostic, secretValue) {
			return errors.New("secret value escaped through disabled Docker logs response")
		}
	}
	if logsErr == nil {
		return errors.New("Docker logs unexpectedly succeeded while log persistence is disabled")
	}
	lower := strings.ToLower(diagnostic)
	if !strings.Contains(lower, "logging driver") ||
		!strings.Contains(lower, "does not support reading") {
		return fmt.Errorf("Docker logs failed for an unrelated reason: %w", logsErr)
	}
	return nil
}

func TestDockerExecutorCommonContractRealProvider(t *testing.T) {
	worker := requireDockerAcceptance(t).publishWorker(t)
	contracttest.Run(t, func(t *testing.T, scenario contracttest.Scenario) contracttest.Fixture {
		t.Helper()
		target := newLiveDockerExecutor(t, worker)
		request := newLiveRequest(t, worker, executor.PurposeVerification)
		fixture := contracttest.Fixture{Executor: target, Request: request}
		registerAttemptCleanup(t, worker.docker, target, request.Attempt)

		switch scenario {
		case contracttest.ScenarioStartFailure:
			request.Profile = liveStartFailureProfile(t, worker)
			fixture.Request = request
		case contracttest.ScenarioNonzero:
			request.Command = executor.Command{
				Executable: "node",
				Args:       []string{"-e", "process.exit(17)"},
				Directory:  executor.SandboxWorkspaceRoot,
			}
			fixture.Request = request
		case contracttest.ScenarioTimeout:
			request.Timeout = 500 * time.Millisecond
			request.Command = executor.Command{
				Executable: "node",
				Args:       []string{"-e", "setInterval(() => {}, 1000)"},
				Directory:  executor.SandboxWorkspaceRoot,
			}
			fixture.Request = request
		case contracttest.ScenarioCancellation, contracttest.ScenarioDescendantDeath:
			startedFile := filepath.Join(request.Workspace.HostPath, "contract.started")
			if scenario == contracttest.ScenarioDescendantDeath {
				request.Command = executor.Command{
					Executable: "node",
					Args: []string{"-e", `
const fs = require("fs");
const {spawn} = require("child_process");
const child = spawn("sleep", ["300"], {stdio: "ignore"});
child.once("spawn", () => {
  fs.writeFileSync("/workspace/contract.started", String(child.pid), {mode: 0o600});
});
setInterval(() => {}, 1000);
`},
					Directory: executor.SandboxWorkspaceRoot,
				}
			} else {
				request.Command = executor.Command{
					Executable: "node",
					Args: []string{"-e", `
const fs = require("fs");
fs.writeFileSync("/workspace/contract.started", String(process.pid), {mode: 0o600});
setInterval(() => {}, 1000);
`},
					Directory: executor.SandboxWorkspaceRoot,
				}
			}
			fixture.Request = request
			fixture.Started = signalWorkspaceFile(t, startedFile)
			if scenario == contracttest.ScenarioDescendantDeath {
				fixture.AssertNoDescendants = func(t *testing.T) {
					t.Helper()
					pid, err := os.ReadFile(startedFile)
					if err != nil || strings.TrimSpace(string(pid)) == "" {
						t.Fatalf("real descendant did not publish a PID: %v %q", err, pid)
					}
					assertAttemptHasNoRunningProcesses(t, worker.docker, request.Attempt)
				}
			}
		case contracttest.ScenarioBoundedOutput:
			request.OutputLimit = 64
			request.Command = executor.Command{
				Executable: "node",
				Args: []string{"-e", `
process.stdout.write("o".repeat(1024));
process.stderr.write("e".repeat(1024));
`},
				Directory: executor.SandboxWorkspaceRoot,
			}
			fixture.Request = request
		case contracttest.ScenarioSecretIsolation:
			request.Destroy()
			secretRequest, _ := newSecretLiveRequest(t, worker)
			registerAttemptCleanup(t, worker.docker, target, secretRequest.Attempt)
			fixture.Request = secretRequest
		}
		return fixture
	})
}

func TestDockerExecutorEnforcesPIDExhaustion(t *testing.T) {
	worker := requireDockerAcceptance(t).publishWorker(t)
	target := newLiveDockerExecutor(t, worker)
	request := newLiveRequest(t, worker, executor.PurposeVerification)
	defer request.Destroy()
	registerAttemptCleanup(t, worker.docker, target, request.Attempt)
	profile := request.Profile.Clone()
	profile.Resources.PIDs = 32
	profile.Digest = ""
	var err error
	profile, err = workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	request.Profile = profile
	request.Timeout = 2 * time.Minute
	request.Command = executor.Command{
		Executable: "node",
		Args: []string{"-e", `
const fs = require("fs");
fs.writeFileSync("/workspace/pids.ready", String(process.pid), {mode: 0o600});
setInterval(() => {
  const events = Object.fromEntries(fs.readFileSync("/sys/fs/cgroup/pids.events", "utf8").trim().split("\n").map((line) => line.split(" ")));
  if (Number(events.max) > 0) {
	const evidence = JSON.stringify({
      pids_current: fs.readFileSync("/sys/fs/cgroup/pids.current", "utf8").trim(),
      pids_max: fs.readFileSync("/sys/fs/cgroup/pids.max", "utf8").trim(),
      pids_events: events,
	});
	fs.writeFileSync("/workspace/pids.json.tmp", evidence, {mode: 0o600});
	fs.renameSync("/workspace/pids.json.tmp", "/workspace/pids.json");
  }
}, 10);
`},
		Directory: executor.SandboxWorkspaceRoot,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type executeResponse struct {
		result executor.Result
		err    error
	}
	response := make(chan executeResponse, 1)
	go func() {
		result, err := target.Execute(ctx, request)
		response <- executeResponse{result: result, err: err}
	}()
	waitForExecutorState(t, target, request.Attempt, executor.StateRunning)
	ready := signalWorkspaceFile(t, filepath.Join(request.Workspace.HostPath, "pids.ready"))
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("PID exhaustion workload did not become ready")
	}
	containerID := attemptContainerID(t, worker.docker, request.Attempt)
	const execAttempts = 48
	execStarted := make(chan struct{})
	execResponses := make(chan error, execAttempts)
	for range execAttempts {
		go func() {
			<-execStarted
			_, err := worker.docker.outputRaw(45*time.Second,
				"exec", containerID, "sleep", "30")
			execResponses <- err
		}()
	}
	close(execStarted)
	var exhaustionErr error
	select {
	case exhaustionErr = <-execResponses:
		if exhaustionErr == nil {
			t.Fatal("a live Docker exec completed before PID exhaustion")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("real cgroup did not reject excess concurrent processes")
	}
	evidenceFile := filepath.Join(request.Workspace.HostPath, "pids.json")
	evidenceReady := signalWorkspaceFile(t, evidenceFile)
	select {
	case <-evidenceReady:
	case <-time.After(10 * time.Second):
		t.Fatalf("PID controller did not publish exhaustion evidence: %v", exhaustionErr)
	}
	top, err := worker.docker.output(t, 20*time.Second, "top", containerID, "-eo", "pid,comm,args")
	if err != nil {
		t.Fatalf("inspect PID-exhausted container processes: %v\n%s", err, top)
	}
	runningSleeps := strings.Count(string(top), "sleep 30")
	if runningSleeps <= 0 || runningSleeps >= execAttempts {
		t.Fatalf("PID exhaustion process count = %d, want between 1 and %d:\n%s",
			runningSleeps, execAttempts-1, top)
	}
	encoded, err := os.ReadFile(evidenceFile)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		PIDsCurrent string            `json:"pids_current"`
		PIDsMax     string            `json:"pids_max"`
		PIDsEvents  map[string]string `json:"pids_events"`
	}
	if err := json.Unmarshal(encoded, &probe); err != nil {
		t.Fatalf("decode PID exhaustion evidence: %v: %s", err, encoded)
	}
	maxEvents, err := strconv.ParseUint(probe.PIDsEvents["max"], 10, 64)
	if err != nil || probe.PIDsMax != "32" || maxEvents == 0 {
		t.Fatalf("PID exhaustion was not enforced by the real cgroup: %#v, %v", probe, err)
	}
	cancel()
	var completed executeResponse
	select {
	case completed = <-response:
	case <-time.After(15 * time.Second):
		t.Fatal("PID exhaustion workload did not stop after cancellation")
	}
	defer completed.result.Destroy()
	if !completed.result.Started || completed.err == nil {
		t.Fatalf("PID exhaustion cancellation lifecycle = %#v, %v", completed.result, completed.err)
	}
	for responses := 1; responses < execAttempts; responses++ {
		select {
		case <-execResponses:
		case <-time.After(15 * time.Second):
			t.Fatal("live Docker exec probe did not terminate after container cancellation")
		}
	}
	destroyAttemptAndAssertAbsent(t, worker.docker, target, request.Attempt)
}

func TestDockerExecutorLifecycleArtifactAndSandboxPolicy(t *testing.T) {
	worker := requireDockerAcceptance(t).publishWorker(t)
	target := newLiveDockerExecutor(t, worker)
	request := newLiveRequest(t, worker, executor.PurposeVerification)
	defer request.Destroy()
	registerAttemptCleanup(t, worker.docker, target, request.Attempt)
	request.Command = executor.Command{
		Executable: "node",
		Args: []string{"-e", `
const fs = require("fs");
const mounts = fs.readFileSync("/proc/mounts", "utf8").trim().split("\n").map((line) => line.split(" "));
const byTarget = Object.fromEntries(mounts.map((fields) => [fields[1], fields]));
for (const target of ["/run/paje", "/home/paje", "/tmp"]) {
  if (!byTarget[target]) throw new Error("missing private tmpfs " + target);
}
fs.writeFileSync("/home/paje/write-probe", "home");
fs.writeFileSync("/tmp/write-probe", "tmp");
const credentialPrefixes = ["PAJE_", "HATCHET_", "MEM0_", "SUBMISSION_", "PUBLISHER_", "DOCKER_", "REGISTRY_"];
const probe = {
  uid: process.getuid(),
  gid: process.getgid(),
  root_read_only: byTarget["/"][3].split(",").includes("ro"),
  pids_max: fs.readFileSync("/sys/fs/cgroup/pids.max", "utf8").trim(),
  tmpfs: Object.fromEntries(["/run/paje", "/home/paje", "/tmp"].map((target) => [target, byTarget[target][2] + ":" + byTarget[target][3]])),
  docker_socket: fs.existsSync("/var/run/docker.sock"),
  source_visible: fs.existsSync("/go.mod") || fs.existsSync("/Dockerfile") || fs.existsSync("/workspace/../go.mod"),
  credential_visible: Object.keys(process.env).some((key) => credentialPrefixes.some((prefix) => key.startsWith(prefix))),
};
fs.writeFileSync("/workspace/artifact.json", JSON.stringify(probe), {mode: 0o600});
process.stdout.write("paje-live-ok\n");
`},
		Directory: executor.SandboxWorkspaceRoot,
	}

	result, err := target.Execute(context.Background(), request)
	defer result.Destroy()
	if err != nil {
		t.Fatalf("real Docker Execute: %v", err)
	}
	if !result.Created || !result.Started || !result.Completed || result.ExitCode != 0 ||
		string(result.Stdout) != "paje-live-ok\n" || len(result.Stderr) != 0 {
		t.Fatalf("real Docker lifecycle result = %#v", result)
	}
	state, err := target.Inspect(context.Background(), request.Attempt)
	if err != nil || state != executor.StateCompleted {
		t.Fatalf("Inspect after real completion = %q, %v", state, err)
	}

	var probe runtimeProbe
	artifact, err := os.ReadFile(filepath.Join(request.Workspace.HostPath, "artifact.json"))
	if err != nil {
		t.Fatalf("read live workspace artifact: %v", err)
	}
	if err := json.Unmarshal(artifact, &probe); err != nil {
		t.Fatalf("decode live workspace artifact: %v: %s", err, artifact)
	}
	if probe.UID != 65532 || probe.GID != 65532 || !probe.RootReadOnly ||
		probe.PIDsMax != strconv.FormatInt(request.Profile.Resources.PIDs, 10) ||
		probe.DockerSocket || probe.SourceVisible || probe.CredentialVisible {
		t.Fatalf("runtime sandbox probe = %#v", probe)
	}
	for target, value := range probe.Tmpfs {
		if !strings.HasPrefix(value, "tmpfs:") {
			t.Fatalf("runtime mount %s = %q, want tmpfs", target, value)
		}
	}

	containerID := attemptContainerID(t, worker.docker, request.Attempt)
	inspectionBytes, inspection := inspectLiveContainer(t, worker.docker, containerID)
	_ = inspectionBytes
	assertLiveContainerPolicy(t, inspection, request)
	destroyAttemptAndAssertAbsent(t, worker.docker, target, request.Attempt)
}

func TestDockerExecutorSecretCleanupBoundsAndNoLeakage(t *testing.T) {
	worker := requireDockerAcceptance(t).publishWorker(t)

	t.Run("secret material is private and output is bounded", func(t *testing.T) {
		target := newLiveDockerExecutor(t, worker)
		request, secretValues := newSecretLiveRequest(t, worker)
		defer request.Destroy()
		registerAttemptCleanup(t, worker.docker, target, request.Attempt)
		request.OutputLimit = 128
		request.Command = executor.Command{
			Executable: "node",
			Args: []string{"-e", `
const fs = require("fs");
const environmentDirectory = "/run/paje/secrets/environment";
const environmentFilesGone = !fs.existsSync(environmentDirectory) || fs.readdirSync(environmentDirectory).length === 0;
const authPresent = fs.readFileSync("/run/paje/secrets/codex/auth.json", "utf8").length > 0;
const ok = Boolean(process.env.WORKLOAD_API_TOKEN) && environmentFilesGone && authPresent;
fs.writeFileSync("/workspace/secret-state.json", JSON.stringify({ok, environment_files_gone: environmentFilesGone, auth_present: authPresent}), {mode: 0o600});
process.stdout.write("o".repeat(4096));
process.stderr.write("e".repeat(4096));
`},
			Directory: executor.SandboxWorkspaceRoot,
		}

		result, err := target.Execute(context.Background(), request)
		defer result.Destroy()
		if err != nil {
			t.Fatalf("execute live bounded secret probe: %v", err)
		}
		if !result.Completed || result.SecretDetected ||
			len(result.Stdout) != int(request.OutputLimit) || len(result.Stderr) != int(request.OutputLimit) ||
			!result.StdoutTruncated || !result.StderrTruncated {
			t.Fatalf("bounded secret result = %#v", result)
		}
		stateBytes, err := os.ReadFile(filepath.Join(request.Workspace.HostPath, "secret-state.json"))
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			OK                   bool `json:"ok"`
			EnvironmentFilesGone bool `json:"environment_files_gone"`
			AuthPresent          bool `json:"auth_present"`
		}
		if err := json.Unmarshal(stateBytes, &state); err != nil || !state.OK ||
			!state.EnvironmentFilesGone || !state.AuthPresent {
			t.Fatalf("secret sandbox state = %#v, %v", state, err)
		}

		containerID := attemptContainerID(t, worker.docker, request.Attempt)
		inspectionBytes, inspection := inspectLiveContainer(t, worker.docker, containerID)
		assertLiveContainerPolicy(t, inspection, request)
		for _, secretValue := range secretValues {
			if strings.Contains(string(inspectionBytes), secretValue) ||
				strings.Contains(string(result.Stdout), secretValue) ||
				strings.Contains(string(result.Stderr), secretValue) {
				t.Fatal("secret value escaped through inspect or bounded output")
			}
		}
		assertDockerLogsDisabled(t, worker.docker, containerID, secretValues)
		destroyAttemptAndAssertAbsent(t, worker.docker, target, request.Attempt)
	})

	t.Run("exact secret output is suppressed", func(t *testing.T) {
		target := newLiveDockerExecutor(t, worker)
		request, secretValues := newSecretLiveRequest(t, worker)
		defer request.Destroy()
		registerAttemptCleanup(t, worker.docker, target, request.Attempt)
		request.Command = executor.Command{
			Executable: "node",
			Args: []string{"-e", `
process.stdout.write(process.env.WORKLOAD_API_TOKEN);
`},
			Directory: executor.SandboxWorkspaceRoot,
		}
		result, err := target.Execute(context.Background(), request)
		defer result.Destroy()
		if err != nil {
			t.Fatalf("execute secret-output probe: %v", err)
		}
		if !result.SecretDetected || len(result.Stdout) != 0 || len(result.Stderr) != 0 {
			t.Fatalf("secret-output result = %#v", result)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		for _, secretValue := range secretValues {
			if strings.Contains(string(encoded), secretValue) {
				t.Fatal("secret value escaped through serialized executor result")
			}
		}
		destroyAttemptAndAssertAbsent(t, worker.docker, target, request.Attempt)
	})

	t.Run("oversized bootstrap archive fails closed", func(t *testing.T) {
		target := newLiveDockerExecutor(t, worker)
		requirement := workerprofile.SecretRequirement{
			Capability: "workload.api-token",
			Stage:      workerprofile.StageAgent,
			Delivery:   workerprofile.DeliveryEnvironment,
			Target:     "WORKLOAD_API_TOKEN",
			Required:   true,
		}
		profile := worker.profile.Clone()
		profile.Secrets = []workerprofile.SecretRequirement{requirement}
		profile.Digest = ""
		var err error
		profile, err = workerprofile.Canonicalize(profile)
		if err != nil {
			t.Fatal(err)
		}
		request := newLiveRequest(t, worker, executor.PurposeAgent)
		request.Profile = profile
		oversized := make([]byte, (8<<20)+1)
		for index := range oversized {
			oversized[index] = 'x'
		}
		materialization, err := secret.NewValueMaterialization(
			workerprofile.DeliveryEnvironment, requirement.Target, oversized,
		)
		clear(oversized)
		if err != nil {
			t.Fatal(err)
		}
		request.Secrets = []secret.Materialization{materialization}
		defer request.Destroy()
		registerAttemptCleanup(t, worker.docker, target, request.Attempt)

		result, executeErr := target.Execute(context.Background(), request)
		defer result.Destroy()
		var providerError *executor.ProviderError
		if executeErr == nil || !result.Created || result.Started ||
			!errors.As(executeErr, &providerError) ||
			providerError.Class != "environment" || providerError.CauseCode != "materialize" {
			t.Fatalf("oversized archive result = %#v, %v", result, executeErr)
		}
		destroyAttemptAndAssertAbsent(t, worker.docker, target, request.Attempt)
	})
}

func TestDockerExecutorCancellationTerminatesDescendants(t *testing.T) {
	worker := requireDockerAcceptance(t).publishWorker(t)
	target := newLiveDockerExecutor(t, worker)
	request := newLiveRequest(t, worker, executor.PurposeVerification)
	defer request.Destroy()
	registerAttemptCleanup(t, worker.docker, target, request.Attempt)
	request.Timeout = 2 * time.Minute
	request.Command = executor.Command{
		Executable: "node",
		Args: []string{"-e", `
const fs = require("fs");
const {spawn} = require("child_process");
const child = spawn("sleep", ["300"], {stdio: "ignore"});
fs.writeFileSync("/workspace/descendant.pid", String(child.pid), {mode: 0o600});
setInterval(() => {}, 1000);
`},
		Directory: executor.SandboxWorkspaceRoot,
	}

	ctx, cancel := context.WithCancel(context.Background())
	type executeResponse struct {
		result executor.Result
		err    error
	}
	response := make(chan executeResponse, 1)
	go func() {
		result, err := target.Execute(ctx, request)
		response <- executeResponse{result: result, err: err}
	}()

	waitForExecutorState(t, target, request.Attempt, executor.StateRunning)
	containerID := attemptContainerID(t, worker.docker, request.Attempt)
	top, err := worker.docker.output(t, 20*time.Second, "top", containerID, "-eo", "pid,comm,args")
	if err != nil || !strings.Contains(string(top), "sleep 300") {
		cancel()
		t.Fatalf("live descendant was not running: %v\n%s", err, top)
	}
	if _, err := os.ReadFile(filepath.Join(request.Workspace.HostPath, "descendant.pid")); err != nil {
		cancel()
		t.Fatalf("descendant did not publish its PID artifact: %v", err)
	}

	cancel()
	var completed executeResponse
	select {
	case completed = <-response:
	case <-time.After(30 * time.Second):
		t.Fatal("canceled real Docker Execute did not return")
	}
	defer completed.result.Destroy()
	if !completed.result.Started || completed.err == nil {
		t.Fatalf("canceled real Docker lifecycle = %#v, %v", completed.result, completed.err)
	}
	if output, err := worker.docker.output(t, 20*time.Second, "top", containerID, "-eo", "pid,comm,args"); err == nil {
		t.Fatalf("container descendants survived cancellation:\n%s", output)
	}
	for range 2 {
		if err := target.Cancel(context.Background(), request.Attempt); err != nil {
			t.Fatalf("idempotent live Cancel: %v", err)
		}
	}
	destroyAttemptAndAssertAbsent(t, worker.docker, target, request.Attempt)
}

func newLiveDockerExecutor(t *testing.T, worker publishedWorker) executor.Executor {
	t.Helper()
	target, err := dockerengine.New(dockerengine.Config{
		Endpoint:    worker.docker.endpoint,
		StopTimeout: 3 * time.Second,
		KillTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("create real Docker executor: %v", err)
	}
	return target
}

func liveStartFailureProfile(t *testing.T, worker publishedWorker) workerprofile.Snapshot {
	t.Helper()
	exactReference := exactLocalImageReference(t, worker.docker, "registry:2.8.3")
	profile := worker.profile.Clone()
	profile.Runtime.Image = exactReference
	profile.Digest = ""
	profile, err := workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatalf("canonicalize real start-failure profile: %v", err)
	}
	return profile
}

func exactLocalImageReference(t *testing.T, docker dockerAcceptance, mutableReference string) string {
	t.Helper()
	inspect := func() ([]byte, error) {
		return docker.output(t, 20*time.Second,
			"image", "inspect", "--format", "{{json .RepoDigests}}", mutableReference)
	}
	output, err := inspect()
	if err != nil {
		if pullOutput, pullErr := docker.output(t, 2*time.Minute, "pull", mutableReference); pullErr != nil {
			t.Fatalf("pull real start-failure image: %v\n%s", pullErr, pullOutput)
		}
		output, err = inspect()
	}
	if err != nil {
		t.Fatalf("inspect real start-failure image: %v", err)
	}
	var repositoryDigests []string
	if err := json.Unmarshal(output, &repositoryDigests); err != nil {
		t.Fatalf("decode real start-failure image digests: %v: %s", err, output)
	}
	for _, candidate := range repositoryDigests {
		if strings.Contains(candidate, "@sha256:") {
			return candidate
		}
	}
	t.Fatalf("real start-failure image has no immutable digest: %q", repositoryDigests)
	return ""
}

func signalWorkspaceFile(t *testing.T, filename string) <-chan struct{} {
	t.Helper()
	started := make(chan struct{})
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(filename); err == nil {
				close(started)
				return
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return started
}

func assertAttemptHasNoRunningProcesses(
	t *testing.T,
	docker dockerAcceptance,
	attempt executor.AttemptID,
) {
	t.Helper()
	output, err := docker.output(t, 20*time.Second,
		"ps", "--all",
		"--filter", "label=com.araihu.paje.attempt-key="+attempt.Key(),
		"--format", "{{.ID}}",
	)
	if err != nil {
		t.Fatalf("discover canceled real contract container: %v", err)
	}
	for _, containerID := range strings.Fields(string(output)) {
		if top, topErr := docker.output(t, 20*time.Second, "top", containerID, "-eo", "pid,comm,args"); topErr == nil {
			t.Fatalf("real contract descendants survived cancellation:\n%s", top)
		}
	}
}

func newLiveRequest(t *testing.T, worker publishedWorker, purpose executor.Purpose) executor.Request {
	t.Helper()
	workspace, err := os.MkdirTemp(worker.docker.repositoryRoot, ".paje-task7-workspace-")
	if err != nil {
		t.Fatalf("create shared Docker acceptance workspace: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(workspace); err != nil {
			t.Errorf("remove Docker acceptance workspace: %v", err)
		}
	})
	return executor.Request{
		Attempt: executor.AttemptID{
			RunID:     fmt.Sprintf("task7-%d-%d", os.Getpid(), acceptanceResourceSequence.Add(1)),
			Stage:     "execute",
			Attempt:   1,
			StartedAt: time.Now().UTC(),
			Purpose:   purpose,
			Sequence:  1,
		},
		Profile: worker.profile.Clone(),
		Command: executor.Command{
			Executable: "node",
			Args:       []string{"--version"},
			Directory:  executor.SandboxWorkspaceRoot,
		},
		Workspace: executor.Workspace{
			HostPath:    workspace,
			SandboxPath: executor.SandboxWorkspaceRoot,
			Writable:    true,
		},
		Environment: map[string]string{
			"HOME":   "/home/paje",
			"PATH":   "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
			"TMPDIR": "/tmp",
		},
		Timeout:     30 * time.Second,
		OutputLimit: 1 << 20,
	}
}

func newSecretLiveRequest(t *testing.T, worker publishedWorker) (executor.Request, []string) {
	t.Helper()
	requirements := []workerprofile.SecretRequirement{
		{
			Capability: "harness.codex-auth",
			Stage:      workerprofile.StageAgent,
			Delivery:   workerprofile.DeliveryDirectory,
			Target:     "/run/paje/secrets/codex",
			Required:   true,
		},
		{
			Capability: "workload.api-token",
			Stage:      workerprofile.StageAgent,
			Delivery:   workerprofile.DeliveryEnvironment,
			Target:     "WORKLOAD_API_TOKEN",
			Required:   true,
		},
	}
	profile := worker.profile.Clone()
	profile.Secrets = requirements
	profile.Digest = ""
	var err error
	profile, err = workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}

	environmentValue := "task7-environment-secret-" + uniqueDockerName(t)
	directoryValue := "task7-directory-secret-" + uniqueDockerName(t)
	environmentMaterialization, err := secret.NewValueMaterialization(
		workerprofile.DeliveryEnvironment, "WORKLOAD_API_TOKEN", []byte(environmentValue),
	)
	if err != nil {
		t.Fatal(err)
	}
	file, err := secret.NewFile("auth.json", 0o600, []byte(directoryValue))
	if err != nil {
		t.Fatal(err)
	}
	directoryMaterialization, err := secret.NewDirectoryMaterialization(
		"/run/paje/secrets/codex", []secret.File{file},
	)
	file.Zero()
	if err != nil {
		t.Fatal(err)
	}
	request := newLiveRequest(t, worker, executor.PurposeAgent)
	request.Profile = profile
	request.Secrets = []secret.Materialization{environmentMaterialization, directoryMaterialization}
	return request, []string{environmentValue, directoryValue}
}

func attemptContainerID(t *testing.T, docker dockerAcceptance, attempt executor.AttemptID) string {
	t.Helper()
	output, err := docker.output(t, 20*time.Second,
		"ps", "--all",
		"--filter", "label=com.araihu.paje.attempt-key="+attempt.Key(),
		"--filter", "label=com.araihu.paje.resource=container",
		"--format", "{{.ID}}",
	)
	if err != nil {
		t.Fatalf("discover attempt container: %v", err)
	}
	ids := strings.Fields(string(output))
	if len(ids) != 1 {
		t.Fatalf("attempt container IDs = %q, want exactly one", ids)
	}
	return ids[0]
}

func inspectLiveContainer(
	t *testing.T,
	docker dockerAcceptance,
	containerID string,
) ([]byte, liveContainerInspect) {
	t.Helper()
	output, err := docker.output(t, 20*time.Second, "container", "inspect", containerID)
	if err != nil {
		t.Fatalf("inspect live attempt container: %v", err)
	}
	var values []liveContainerInspect
	if err := json.Unmarshal(output, &values); err != nil || len(values) != 1 {
		t.Fatalf("decode live attempt container: %v: %s", err, output)
	}
	return output, values[0]
}

func assertLiveContainerPolicy(t *testing.T, inspection liveContainerInspect, request executor.Request) {
	t.Helper()
	config := inspection.Config
	host := inspection.HostConfig
	if config.User != "65532:65532" || config.Image != request.Profile.Runtime.Image ||
		len(config.Entrypoint) != 1 || config.Entrypoint[0] != "/usr/local/bin/paje-sandbox-init" ||
		len(config.Cmd) != 1 || config.Cmd[0] != "--bootstrap-stdin" ||
		len(config.ExposedPorts) != 0 {
		t.Fatalf("live container config = %#v", config)
	}
	wantEnvironment := map[string]string{
		"PATH":         "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
		"NODE_VERSION": acceptanceNodeVersion,
		"YARN_VERSION": "1.22.22",
		"HOME":         "/home/paje",
		"GOCACHE":      "/home/paje/.cache/go-build",
		"GOMODCACHE":   "/home/paje/go/pkg/mod",
		"TMPDIR":       "/tmp",
	}
	gotEnvironment := make(map[string]string, len(config.Env))
	for _, entry := range config.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("live container environment entry = %q", entry)
		}
		if _, duplicate := gotEnvironment[key]; duplicate {
			t.Fatalf("duplicate live container environment key %q", key)
		}
		gotEnvironment[key] = value
	}
	if len(gotEnvironment) != len(wantEnvironment) {
		t.Fatalf("live container environment = %#v", gotEnvironment)
	}
	for key, value := range wantEnvironment {
		if gotEnvironment[key] != value {
			t.Fatalf("live container environment %s = %q, want %q", key, gotEnvironment[key], value)
		}
	}
	if !host.ReadonlyRootfs || host.Privileged || host.PublishAll ||
		len(host.CapAdd) != 0 || !containsFold(host.CapDrop, "ALL") ||
		!containsPrefix(host.SecurityOpt, "no-new-privileges") ||
		len(host.PortBindings) != 0 || len(host.Devices) != 0 || len(host.Binds) != 0 ||
		host.PidMode == "host" || host.IpcMode == "host" || host.UTSMode == "host" ||
		host.CgroupnsMode == "host" ||
		host.Memory != request.Profile.Resources.MemoryBytes ||
		host.NanoCPUs != request.Profile.Resources.CPUMillis*1_000_000 ||
		host.PidsLimit != request.Profile.Resources.PIDs ||
		host.LogConfig.Type != "none" ||
		host.NetworkMode == "host" || host.NetworkMode == "default" {
		t.Fatalf("live host config = %#v", host)
	}
	for _, target := range []string{"/run/paje", "/home/paje", "/tmp"} {
		options, ok := host.Tmpfs[target]
		if !ok || !strings.Contains(options, "nosuid") || !strings.Contains(options, "nodev") ||
			!strings.Contains(options, "size=67108864") || !strings.Contains(options, "mode=0700") {
			t.Fatalf("live tmpfs %s = %q", target, options)
		}
	}
	if !strings.Contains(host.Tmpfs["/run/paje"], "noexec") ||
		!strings.Contains(host.Tmpfs["/home/paje"], "noexec") ||
		strings.Contains(host.Tmpfs["/tmp"], "noexec") {
		t.Fatalf("live tmpfs exec policy = %#v", host.Tmpfs)
	}
	if len(inspection.Mounts) != 1 ||
		inspection.Mounts[0].Type != "bind" ||
		inspection.Mounts[0].Source != request.Workspace.HostPath ||
		inspection.Mounts[0].Destination != executor.SandboxWorkspaceRoot ||
		!inspection.Mounts[0].RW ||
		inspection.Mounts[0].Propagation != "rprivate" {
		t.Fatalf("live mounts = %#v", inspection.Mounts)
	}
}

func registerAttemptCleanup(
	t *testing.T,
	docker dockerAcceptance,
	target executor.Executor,
	attempt executor.AttemptID,
) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := target.Destroy(ctx, attempt); err != nil {
			t.Errorf("cleanup live Docker attempt: %v", err)
			return
		}
		if err := verifyAttemptResourcesAbsent(docker, attempt); err != nil {
			t.Errorf("verify live Docker attempt cleanup: %v", err)
		}
	})
}

func verifyAttemptResourcesAbsent(docker dockerAcceptance, attempt executor.AttemptID) error {
	for _, resource := range []struct {
		kind string
		args []string
	}{
		{
			kind: "container",
			args: []string{
				"ps", "--all",
				"--filter", "label=com.araihu.paje.attempt-key=" + attempt.Key(),
				"--format", "{{.ID}}",
			},
		},
		{
			kind: "network",
			args: []string{
				"network", "ls",
				"--filter", "label=com.araihu.paje.attempt-key=" + attempt.Key(),
				"--format", "{{.ID}}",
			},
		},
	} {
		output, err := docker.outputRaw(20*time.Second, resource.args...)
		if err != nil {
			return fmt.Errorf("query attempt %s: %w: %s", resource.kind, err, output)
		}
		if strings.TrimSpace(string(output)) != "" {
			return fmt.Errorf("attempt %s remains after Destroy: %s", resource.kind, output)
		}
	}
	return nil
}

func destroyAttemptAndAssertAbsent(
	t *testing.T,
	docker dockerAcceptance,
	target executor.Executor,
	attempt executor.AttemptID,
) {
	t.Helper()
	for range 2 {
		if err := target.Destroy(context.Background(), attempt); err != nil {
			t.Fatalf("idempotent live Destroy: %v", err)
		}
	}
	state, err := target.Inspect(context.Background(), attempt)
	if err != nil || state != executor.StateDestroyed {
		t.Fatalf("Inspect after live Destroy = %q, %v", state, err)
	}
	if err := verifyAttemptResourcesAbsent(docker, attempt); err != nil {
		t.Fatal(err)
	}
}

func waitForExecutorState(
	t *testing.T,
	target executor.Executor,
	attempt executor.AttemptID,
	want executor.State,
) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		state, err := target.Inspect(context.Background(), attempt)
		if err == nil && state == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	state, err := target.Inspect(context.Background(), attempt)
	t.Fatalf("executor state = %q, %v, want %q", state, err, want)
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func containsPrefix(values []string, want string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, want) {
			return true
		}
	}
	return false
}
