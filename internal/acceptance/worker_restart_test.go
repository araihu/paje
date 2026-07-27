package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/approval"
	approvalmock "github.com/araihu/paje/internal/approval/mock"
	artifactfilesystem "github.com/araihu/paje/internal/artifact/filesystem"
	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/executor/dockerengine"
	"github.com/araihu/paje/internal/harness"
	memorymock "github.com/araihu/paje/internal/memory/mock"
	"github.com/araihu/paje/internal/publisher"
	publishermock "github.com/araihu/paje/internal/publisher/mock"
	"github.com/araihu/paje/internal/run"
	runfilesystem "github.com/araihu/paje/internal/run/filesystem"
	"github.com/araihu/paje/internal/secret"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/workerprofile"
	workerprofilemock "github.com/araihu/paje/internal/workerprofile/mock"
	"github.com/araihu/paje/internal/workflow/codechange"
	"github.com/araihu/paje/internal/workspace/gitworktree"
)

const restartAcceptanceHelper = "PAJE_WORKER_RESTART_HELPER_CONFIG"

func TestWorkerRestartInterruptionAcceptance(t *testing.T) {
	if configPath := os.Getenv(restartAcceptanceHelper); configPath != "" {
		runWorkerRestartChild(t, configPath)
		return
	}
	runWorkerRestartInterruptionAcceptance(t)
}

type restartAcceptanceConfig struct {
	Endpoint      string                 `json:"endpoint"`
	Source        string                 `json:"source"`
	WorkspaceRoot string                 `json:"workspace_root"`
	RunRoot       string                 `json:"run_root"`
	ArtifactRoot  string                 `json:"artifact_root"`
	RuntimeRoot   string                 `json:"runtime_root"`
	RunID         string                 `json:"run_id"`
	Profile       workerprofile.Snapshot `json:"profile"`
	HoldAgent     bool                   `json:"hold_agent,omitempty"`
}

type restartHarness struct {
	holdAgent bool
}

func (restartHarness) ID() string      { return "restart-acceptance" }
func (restartHarness) Version() string { return "1.0.0" }
func (restartHarness) Probe() executor.Command {
	return executor.Command{Executable: "node", Args: []string{"-e", `process.stdout.write("1.0.0")`}, Directory: executor.SandboxWorkspaceRoot}
}

func (adapter restartHarness) AgentCommand(string) (executor.Command, error) {
	if adapter.holdAgent {
		return executor.Command{
			Executable: "node",
			Args: []string{"-e", `
process.stdout.write(JSON.stringify({type:"item.completed",item:{type:"agent_message",text:"restart acceptance child started"}})+"\n");
setInterval(() => {}, 1000);
`},
			Directory: executor.SandboxWorkspaceRoot,
		}, nil
	}
	return executor.Command{
		Executable: "node",
		Args: []string{"-e", `
const fs = require("fs");
const target = "module-a/greeting/greeting.go";
const before = fs.readFileSync(target, "utf8");
const after = before.replace('const Message = "before"', 'const Message = "after"');
if (after === before) throw new Error("expected source preimage missing");
fs.writeFileSync(target, after, {mode: 0o644});
process.stdout.write(JSON.stringify({type:"item.completed",item:{type:"agent_message",text:"restart acceptance edit complete"}})+"\n");
process.stdout.write(JSON.stringify({type:"turn.completed"})+"\n");
`},
		Directory: executor.SandboxWorkspaceRoot,
	}, nil
}
func (adapter restartHarness) AgentCommandFor(_ harness.AgentExecutionContext, prompt string) (executor.Command, error) {
	return adapter.AgentCommand(prompt)
}
func (restartHarness) AgentEnvironment([]workerprofile.SecretRequirement) (map[string]string, error) {
	return nil, nil
}
func (restartHarness) Parse(result executor.Result) (string, error) {
	if !result.Started || !result.Completed || result.ExitCode != 0 || len(result.Stdout) == 0 {
		return "", errors.New("restart acceptance agent result is incomplete")
	}
	return "restart acceptance edit complete", nil
}
func (restartHarness) AcceptsCapability(string) bool { return false }

type emptyBindingRegistry struct{}

func (emptyBindingRegistry) Resolve(context.Context, secret.ResolveRequest) (secret.Binding, error) {
	return secret.Binding{}, secret.ErrBindingNotFound
}

type restartFixture struct {
	service   *codechange.Service
	runs      *runfilesystem.Store
	artifacts *artifactfilesystem.Store
}

func runWorkerRestartInterruptionAcceptance(t *testing.T) {
	t.Helper()
	worker := requireDockerAcceptance(t).publishWorker(t)
	registerRunDockerResourceCleanup(t, worker.docker,
		"paje-worker-restart-interruption",
		"paje-worker-restart-unrelated",
		"paje-worker-restart-post-receipt",
	)
	profile := worker.profile.Clone()
	profile.Metadata = workerprofile.ProfileID{Name: "restart-acceptance", Revision: 1}
	profile.Harness = workerprofile.Harness{ID: "restart-acceptance", Version: "1.0.0"}
	profile.Secrets = nil
	profile.Digest = ""
	var err error
	profile, err = workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	source, sourceSHA := newCodexAcceptanceRepository(t)
	config := restartAcceptanceConfig{
		Source: source, WorkspaceRoot: newSharedAcceptanceRoot(t, worker.docker.repositoryRoot, ".paje-worker-restart-"), RunRoot: t.TempDir(),
		ArtifactRoot: t.TempDir(), RuntimeRoot: t.TempDir(),
		RunID: "paje-worker-restart-interruption", Profile: profile,
	}

	proxyEndpoint, agentStart, releaseProxy := startDockerAgentStartGate(t, worker.docker.endpoint)
	config.Endpoint = proxyEndpoint
	configPath := filepath.Join(t.TempDir(), "restart-config.json")
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestWorkerRestartInterruptionAcceptance$", "-test.v")
	command.Env = replacedEnvironment(map[string]string{restartAcceptanceHelper: configPath})
	var childOutput bytes.Buffer
	command.Stdout = &childOutput
	command.Stderr = &childOutput
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case <-agentStart:
	case err := <-waited:
		t.Fatalf("restart child exited before agent start gate: %v\n%s", err, childOutput.String())
	case <-time.After(3 * time.Minute):
		_ = command.Process.Kill()
		t.Fatalf("restart child did not reach real agent start gate\n%s", childOutput.String())
	}

	runs, err := runfilesystem.New(config.RunRoot)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := runs.Load(context.Background(), config.RunID)
	if err != nil || interrupted.Status != run.StatusExecuting {
		t.Fatalf("interrupted durable run = %#v, %v", interrupted, err)
	}
	firstStage, found := executeStageAttempt(interrupted, 1)
	if !found || firstStage.Status != run.StageRunning {
		t.Fatalf("first durable execute attempt = %#v, found=%t", firstStage, found)
	}
	firstAttempt := executor.AttemptID{
		RunID: config.RunID, Stage: "execute", Attempt: 1, StartedAt: firstStage.StartedAt,
		Purpose: executor.PurposeAgent, Sequence: 0,
	}
	if attemptContainerID(t, worker.docker, firstAttempt) == "" {
		t.Fatal("real pre-child container was not created")
	}

	// The unrelated run must complete while the primary coordinator is still
	// alive and blocked at the exact provider start boundary.
	unrelatedConfig := config
	unrelatedConfig.Endpoint = worker.docker.endpoint
	unrelatedFixture := newRestartFixture(t, unrelatedConfig, time.Now, "paje-worker-restart-unrelated")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	unrelatedRaw := restartInput(t, unrelatedConfig, "paje-worker-restart-unrelated")
	unrelated, err := unrelatedFixture.service.Resolve(ctx, unrelatedRaw)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedResult, err := unrelatedFixture.service.Execute(ctx, unrelated.RunID)
	if err != nil || unrelatedResult.Artifact == nil {
		t.Fatalf("simultaneous unrelated Execute = %#v, %v", unrelatedResult, err)
	}
	if _, err := unrelatedFixture.service.Approval(ctx, unrelated.RunID, approvalmock.NewGate(approval.Result{}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := unrelatedFixture.service.Publish(ctx, unrelated.RunID); err != nil {
		t.Fatal(err)
	}
	if completed, err := unrelatedFixture.service.Finalize(ctx, unrelated.RunID); err != nil || completed.Status != run.StatusSucceeded {
		t.Fatalf("simultaneous unrelated Finalize = %#v, %v", completed, err)
	}
	if err := unrelatedFixture.artifacts.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		t.Fatalf("primary coordinator exited during unrelated progress: %v\n%s", err, childOutput.String())
	default:
	}
	stillBlocked, err := runs.Load(context.Background(), config.RunID)
	if err != nil || stillBlocked.Status != run.StatusExecuting {
		t.Fatalf("primary run after unrelated progress = %#v, %v", stillBlocked, err)
	}
	stillRunning, found := executeStageAttempt(stillBlocked, 1)
	if !found || stillRunning.Status != run.StageRunning {
		t.Fatalf("primary attempt after unrelated progress = %#v, found=%t", stillRunning, found)
	}

	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-waited; err == nil {
		t.Fatal("restart child exited successfully instead of being killed")
	}
	releaseProxy()

	config.Endpoint = worker.docker.endpoint
	advanced := time.Now().UTC().Add(36 * time.Minute)
	var clockMu sync.Mutex
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		now := advanced
		advanced = advanced.Add(time.Millisecond)
		return now
	}
	fixture := newRestartFixture(t, config, clock, "paje-worker-restart-unrelated")
	defer fixture.artifacts.Close()
	executed, err := fixture.service.Execute(ctx, config.RunID)
	if err != nil || executed.Artifact == nil {
		t.Fatalf("restarted Execute = %#v, chain=%q", executed, safeErrorChain(err))
	}
	if _, err := fixture.service.Approval(ctx, config.RunID, approvalmock.NewGate(approval.Result{}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Publish(ctx, config.RunID); err != nil {
		t.Fatal(err)
	}
	finalized, err := fixture.service.Finalize(ctx, config.RunID)
	if err != nil || finalized.Status != run.StatusSucceeded {
		t.Fatalf("restarted Finalize = %#v, %v", finalized, err)
	}
	record, err := fixture.runs.Load(ctx, config.RunID)
	if err != nil {
		t.Fatal(err)
	}
	secondStage, found := executeStageAttempt(record, 2)
	if !found || secondStage.Status != run.StageSucceeded {
		t.Fatalf("recovered execute attempt 2 = %#v, found=%t", secondStage, found)
	}
	if _, extra := executeStageAttempt(record, 3); extra {
		t.Fatal("recovery executed the agent more than once")
	}
	if err := verifyAttemptResourcesAbsent(worker.docker, firstAttempt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.artifacts.Close(); err != nil {
		t.Fatal(err)
	}

	runPostReceiptRestartAcceptance(t, worker, config, ctx)

	if gitOutput(t, source, "rev-parse", "HEAD") != sourceSHA || gitOutput(t, source, "status", "--porcelain=v1") != "" {
		t.Fatal("restart acceptance changed source checkout")
	}
	assertDirectoryEmpty(t, filepath.Join(config.WorkspaceRoot, "worktrees"))
	assertDirectoryEmpty(t, config.RuntimeRoot)
	assertRunDockerResourcesAbsent(t, worker.docker, config.RunID)
	assertRunDockerResourcesAbsent(t, worker.docker, unrelated.RunID)
}

func runPostReceiptRestartAcceptance(
	t *testing.T,
	worker publishedWorker,
	baseConfig restartAcceptanceConfig,
	ctx context.Context,
) {
	t.Helper()
	config := baseConfig
	config.Endpoint = worker.docker.endpoint
	config.RunID = "paje-worker-restart-post-receipt"
	config.HoldAgent = true
	configPath := filepath.Join(t.TempDir(), "post-receipt-restart-config.json")
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestWorkerRestartInterruptionAcceptance$", "-test.v")
	command.Env = replacedEnvironment(map[string]string{restartAcceptanceHelper: configPath})
	var childOutput bytes.Buffer
	command.Stdout = &childOutput
	command.Stderr = &childOutput
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	childStopped := false
	t.Cleanup(func() {
		if !childStopped {
			_ = command.Process.Kill()
			<-waited
		}
	})

	runs, err := runfilesystem.New(config.RunRoot)
	if err != nil {
		t.Fatal(err)
	}
	var stage run.StageResult
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		record, loadErr := runs.Load(ctx, config.RunID)
		if loadErr == nil {
			if candidate, found := executeStageAttempt(record, 1); found && candidate.Status == run.StageRunning {
				stage = candidate
				break
			}
		}
		select {
		case err := <-waited:
			childStopped = true
			t.Fatalf("post-receipt coordinator exited before durable attempt: %v\n%s", err, childOutput.String())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if stage.StartedAt.IsZero() {
		t.Fatalf("post-receipt coordinator did not persist attempt 1\n%s", childOutput.String())
	}
	attempt := executor.AttemptID{
		RunID: config.RunID, Stage: "execute", Attempt: 1, StartedAt: stage.StartedAt,
		Purpose: executor.PurposeAgent, Sequence: 0,
	}
	observer, err := dockerengine.New(dockerengine.Config{Endpoint: worker.docker.endpoint})
	if err != nil {
		t.Fatal(err)
	}
	state := executor.StateAbsent
	for time.Now().Before(deadline) {
		state, err = observer.Inspect(ctx, attempt)
		if err == nil && state == executor.StateChildStarted {
			break
		}
		select {
		case childErr := <-waited:
			childStopped = true
			t.Fatalf("post-receipt coordinator exited before child receipt: %v, state=%q inspect=%v\n%s",
				childErr, state, err, childOutput.String())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err != nil || state != executor.StateChildStarted {
		t.Fatalf("real Docker child-start receipt state = %q, %v\n%s", state, err, childOutput.String())
	}
	// A fresh observer must derive child_started from Docker's private receipt,
	// not from process memory or generic provider-running state.
	freshObserver, err := dockerengine.New(dockerengine.Config{Endpoint: worker.docker.endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if freshState, inspectErr := freshObserver.Inspect(ctx, attempt); inspectErr != nil || freshState != executor.StateChildStarted {
		t.Fatalf("fresh authoritative child-start observation = %q, %v", freshState, inspectErr)
	}

	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-waited; err == nil {
		t.Fatal("post-receipt coordinator exited successfully instead of being killed")
	}
	childStopped = true

	advanced := time.Now().UTC().Add(36 * time.Minute)
	var clockMu sync.Mutex
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		now := advanced
		advanced = advanced.Add(time.Millisecond)
		return now
	}
	fixture := newRestartFixture(t, config, clock, "paje-worker-restart-post-receipt-recovery")
	defer fixture.artifacts.Close()
	result, executeErr := fixture.service.Execute(ctx, config.RunID)
	if executeErr == nil || result.Artifact != nil {
		t.Fatalf("post-receipt recovery Execute = %#v, chain=%q", result, safeErrorChain(executeErr))
	}
	record, err := fixture.runs.Load(ctx, config.RunID)
	if err != nil || record.Status != run.StatusFailed || record.Failure == nil ||
		record.Failure.CauseCode != "ambiguous_attempt" || record.Failure.Retryable {
		t.Fatalf("post-receipt durable ambiguity = %#v, %v", record, err)
	}
	failedStage, found := executeStageAttempt(record, 1)
	if !found || failedStage.Status != run.StageFailed || failedStage.Failure == nil ||
		failedStage.Failure.CauseCode != "ambiguous_attempt" {
		t.Fatalf("post-receipt terminal attempt = %#v, found=%t", failedStage, found)
	}
	if _, found := executeStageAttempt(record, 2); found {
		t.Fatal("post-receipt recovery reran the agent")
	}
	version := record.Version
	if replay, replayErr := fixture.service.Execute(ctx, config.RunID); replayErr != nil || replay.Artifact != nil ||
		replay.Status != run.StatusFailed || replay.FailureClass != run.FailureInternal || replay.Retryable {
		t.Fatalf("post-receipt terminal replay = %#v, chain=%q", replay, safeErrorChain(replayErr))
	}
	replayed, err := fixture.runs.Load(ctx, config.RunID)
	if err != nil || replayed.Version != version {
		t.Fatalf("post-receipt replay mutated record version %d -> %d, %v", version, replayed.Version, err)
	}
	if err := verifyAttemptResourcesAbsent(worker.docker, attempt); err != nil {
		t.Fatalf("post-receipt production cleanup: %v", err)
	}
	assertRunDockerResourcesAbsent(t, worker.docker, config.RunID)
	assertDirectoryEmpty(t, filepath.Join(config.WorkspaceRoot, "worktrees"))
	assertDirectoryEmpty(t, config.RuntimeRoot)
}

func runWorkerRestartChild(t *testing.T, configPath string) {
	t.Helper()
	encoded, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config restartAcceptanceConfig
	if err := json.Unmarshal(encoded, &config); err != nil {
		t.Fatal(err)
	}
	fixture := newRestartFixture(t, config, time.Now, config.RunID)
	defer fixture.artifacts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	resolved, err := fixture.service.Resolve(ctx, restartInput(t, config, config.RunID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Execute(ctx, resolved.RunID); err != nil {
		t.Fatalf("restart child Execute returned before interruption: %v", err)
	}
	t.Fatal("restart child Execute unexpectedly completed")
}

func newRestartFixture(t *testing.T, config restartAcceptanceConfig, clock func() time.Time, newID string) restartFixture {
	t.Helper()
	workspaces, err := gitworktree.New(config.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runfilesystem.New(config.RunRoot)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactfilesystem.New(config.ArtifactRoot, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	environments, err := environment.NewPolicy(environment.Config{RuntimeRoot: config.RuntimeRoot, Source: baselineEnvironment()})
	if err != nil {
		t.Fatal(err)
	}
	target, err := dockerengine.New(dockerengine.Config{Endpoint: config.Endpoint})
	if err != nil {
		t.Fatal(err)
	}
	executors, err := executor.NewRegistry(executor.Registration{RuntimeKind: workerprofile.RuntimeOCI, Executor: target})
	if err != nil {
		t.Fatal(err)
	}
	harnesses, err := harness.NewRegistry(restartHarness{holdAgent: config.HoldAgent})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := secret.NewBroker(emptyBindingRegistry{}, nil, secret.BrokerConfig{LeaseTTL: 5 * time.Minute, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	profiles, verifier, capturer, changePolicy, templates := acceptanceWorkflowDependencies(t, config.WorkspaceRoot)
	service, err := codechange.New(codechange.Dependencies{
		Templates: templates, Runs: runs, Memory: memorymock.NewStore(nil), Resolver: workspaces, Workspaces: workspaces,
		Profiles: profiles, WorkerProfiles: workerprofilemock.NewRegistry(config.Profile), SecretBindings: emptyBindingRegistry{},
		Secrets: broker, Executors: executors, Harnesses: harnesses, Environments: environments,
		Agent: &recordingRunner{}, Verifier: verifier, Capturer: capturer, Policy: changePolicy, Artifacts: artifacts,
		Publisher: publishermock.NewPublisher(publisher.Result{}, nil), Clock: clock, NewID: func() string { return newID },
	})
	if err != nil {
		_ = artifacts.Close()
		t.Fatal(err)
	}
	return restartFixture{service: service, runs: runs, artifacts: artifacts}
}

func restartInput(t *testing.T, config restartAcceptanceConfig, idempotency string) []byte {
	t.Helper()
	return marshalJSON(t, templatecodechange.Input{
		IdempotencyKey: idempotency, TaskDescription: "apply deterministic restart acceptance edit",
		RepositoryURI: config.Source, BaseRef: "main", WorkerProfile: config.Profile.Metadata.String(),
		Profile: "go", Tags: map[string]string{"user_id": "acceptance", "app_id": "restart"},
		Publication: templatecodechange.Publication{Mode: "artifact"},
	})
}

func executeStageAttempt(record run.Record, attempt int) (run.StageResult, bool) {
	for _, stage := range record.Stages {
		if stage.Name == "execute" && stage.Attempts == attempt {
			return stage, true
		}
	}
	return run.StageResult{}, false
}

type dockerAgentStartGate struct {
	proxy   *httputil.ReverseProxy
	mu      sync.Mutex
	agentID string
	ready   chan struct{}
	release chan struct{}
	once    sync.Once
}

func startDockerAgentStartGate(t *testing.T, endpoint string) (string, <-chan struct{}, func()) {
	t.Helper()
	realSocket := strings.TrimPrefix(endpoint, "unix://")
	proxyDirectory, err := os.MkdirTemp("/tmp", "paje-docker-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(proxyDirectory); err != nil {
			t.Errorf("remove Docker proxy directory: %v", err)
		}
	})
	proxySocket := filepath.Join(proxyDirectory, "docker.sock")
	listener, err := net.Listen("unix", proxySocket)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", realSocket)
	}}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "docker"})
	proxy.Transport = transport
	gate := &dockerAgentStartGate{proxy: proxy, ready: make(chan struct{}), release: make(chan struct{})}
	server := &http.Server{Handler: gate, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		gate.once.Do(func() { close(gate.release) })
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		transport.CloseIdleConnections()
	})
	return "unix://" + proxySocket, gate.ready, func() { gate.once.Do(func() { close(gate.release) }) }
}

func (gate *dockerAgentStartGate) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if request.Method == http.MethodPost && strings.Contains(path, "/containers/") && strings.HasSuffix(path, "/start") {
		containerID := strings.TrimSuffix(strings.TrimPrefix(path[strings.Index(path, "/containers/"):], "/containers/"), "/start")
		gate.mu.Lock()
		agentID := gate.agentID
		gate.mu.Unlock()
		if agentID != "" && containerID == agentID {
			select {
			case <-gate.ready:
			default:
				close(gate.ready)
			}
			select {
			case <-request.Context().Done():
			case <-gate.release:
			}
			return
		}
	}
	isCreate := request.Method == http.MethodPost && strings.Contains(path, "/containers/create")
	isAgent := false
	if isCreate {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, "read create request", http.StatusBadRequest)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		isAgent = bytes.Contains(body, []byte(`"com.araihu.paje.purpose":"agent"`))
	}
	if !isAgent {
		gate.proxy.ServeHTTP(writer, request)
		return
	}
	proxy := *gate.proxy
	proxy.ModifyResponse = func(response *http.Response) error {
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(body))
		response.ContentLength = int64(len(body))
		var created struct {
			ID string `json:"Id"`
		}
		if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
			return fmt.Errorf("decode agent container create response: %w", err)
		}
		gate.mu.Lock()
		gate.agentID = created.ID
		gate.mu.Unlock()
		return nil
	}
	proxy.ServeHTTP(writer, request)
}

func assertRunDockerResourcesAbsent(t *testing.T, docker dockerAcceptance, runID string) {
	t.Helper()
	for _, query := range [][]string{
		{"ps", "--all", "--filter", "label=com.araihu.paje.run-id=" + runID, "--format", "{{.ID}}"},
		{"network", "ls", "--filter", "label=com.araihu.paje.run-id=" + runID, "--format", "{{.ID}}"},
	} {
		output, err := docker.output(t, 20*time.Second, query...)
		if err != nil || strings.TrimSpace(string(output)) != "" {
			t.Fatalf("run %s retained Docker resources: %v %s", runID, err, output)
		}
	}
}

func registerRunDockerResourceCleanup(t *testing.T, docker dockerAcceptance, runIDs ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, runID := range runIDs {
			containers, _ := docker.outputRaw(20*time.Second,
				"ps", "--all", "--quiet", "--filter", "label=com.araihu.paje.run-id="+runID,
			)
			for _, id := range strings.Fields(string(containers)) {
				_, _ = docker.outputRaw(20*time.Second, "rm", "--force", id)
			}
			networks, _ := docker.outputRaw(20*time.Second,
				"network", "ls", "--quiet", "--filter", "label=com.araihu.paje.run-id="+runID,
			)
			for _, id := range strings.Fields(string(networks)) {
				_, _ = docker.outputRaw(20*time.Second, "network", "rm", id)
			}
		}
	})
}
