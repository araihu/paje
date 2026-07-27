package acceptance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifactfilesystem "github.com/araihu/paje/internal/artifact/filesystem"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/harness"
	"github.com/araihu/paje/internal/memory/mock"
	"github.com/araihu/paje/internal/policy"
	"github.com/araihu/paje/internal/publisher"
	publishermock "github.com/araihu/paje/internal/publisher/mock"
	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/run"
	runfilesystem "github.com/araihu/paje/internal/run/filesystem"
	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/secret"
	secretprovider "github.com/araihu/paje/internal/secret/provider"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workerprofile"
	workerprofilemock "github.com/araihu/paje/internal/workerprofile/mock"
	"github.com/araihu/paje/internal/workflow/codechange"
	"github.com/araihu/paje/internal/workspace/gitworktree"
)

func TestWorkerDockerAdversarialIsolationAcceptance(t *testing.T) {
	runWorkerDockerAdversarialIsolationAcceptance(t)
}

func TestWorkerDockerSecretArtifactDenialAcceptance(t *testing.T) {
	runWorkerDockerSecretArtifactDenialAcceptance(t)
}

func runWorkerDockerAdversarialIsolationAcceptance(t *testing.T) {
	t.Helper()
	worker := requireDockerAcceptance(t).publishWorker(t)
	target := newLiveDockerExecutor(t, worker)
	request := newLiveRequest(t, worker, executor.PurposeVerification)
	defer request.Destroy()
	registerAttemptCleanup(t, worker.docker, target, request.Attempt)

	outsideRoot := t.TempDir()
	outsideMarker := filepath.Join(outsideRoot, "host-only-marker")
	writeFile(t, outsideMarker, "must-not-be-visible")
	symlink := filepath.Join(request.Workspace.HostPath, "host-escape")
	if err := os.Symlink(outsideRoot, symlink); err != nil {
		t.Fatal(err)
	}
	request.Timeout = 2 * time.Minute
	request.Command = executor.Command{
		Executable: "node",
		Args: []string{"-e", fmt.Sprintf(`
const fs = require("fs");
let rootWriteDenied = false;
let shadowDenied = false;
try { fs.writeFileSync("/paje-host-escape", "x"); } catch (_) { rootWriteDenied = true; }
try { fs.readFileSync("/etc/shadow"); } catch (_) { shadowDenied = true; }
const probe = {
  uid: process.getuid(),
  gid: process.getgid(),
  root_write_denied: rootWriteDenied,
  shadow_denied: shadowDenied,
  docker_socket: fs.existsSync("/var/run/docker.sock"),
  host_path_visible: fs.existsSync(%q),
  symlink_escape_visible: fs.existsSync("/workspace/host-escape/host-only-marker"),
  source_visible: fs.existsSync("/source"),
  credential_keys: Object.keys(process.env).filter(k => /^(PAJE|HATCHET|MEM0|GITHUB|GH_|DOCKER|REGISTRY)_/.test(k)),
};
fs.writeFileSync("/workspace/isolation.json.tmp", JSON.stringify(probe), {mode: 0o600});
fs.renameSync("/workspace/isolation.json.tmp", "/workspace/isolation.json");
setInterval(() => {}, 1000);
`, outsideMarker)},
		Directory: executor.SandboxWorkspaceRoot,
	}

	ctx, cancel := context.WithCancel(context.Background())
	response := make(chan struct {
		result executor.Result
		err    error
	}, 1)
	go func() {
		result, err := target.Execute(ctx, request)
		response <- struct {
			result executor.Result
			err    error
		}{result: result, err: err}
	}()

	ready := signalWorkspaceFile(t, filepath.Join(request.Workspace.HostPath, "isolation.json"))
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("adversarial workload did not publish isolation evidence")
	}
	containerID := attemptContainerID(t, worker.docker, request.Attempt)
	_, inspection := inspectLiveContainer(t, worker.docker, containerID)
	assertLiveContainerPolicy(t, inspection, request)
	encoded, err := os.ReadFile(filepath.Join(request.Workspace.HostPath, "isolation.json"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var probe struct {
		UID                  int      `json:"uid"`
		GID                  int      `json:"gid"`
		RootWriteDenied      bool     `json:"root_write_denied"`
		ShadowDenied         bool     `json:"shadow_denied"`
		DockerSocket         bool     `json:"docker_socket"`
		HostPathVisible      bool     `json:"host_path_visible"`
		SymlinkEscapeVisible bool     `json:"symlink_escape_visible"`
		SourceVisible        bool     `json:"source_visible"`
		CredentialKeys       []string `json:"credential_keys"`
	}
	if err := json.Unmarshal(encoded, &probe); err != nil {
		cancel()
		t.Fatalf("decode isolation evidence: %v", err)
	}
	if probe.UID != 65532 || probe.GID != 65532 || !probe.RootWriteDenied || !probe.ShadowDenied ||
		probe.DockerSocket || probe.HostPathVisible || probe.SymlinkEscapeVisible || probe.SourceVisible ||
		len(probe.CredentialKeys) != 0 {
		cancel()
		t.Fatalf("adversarial isolation evidence = %#v", probe)
	}
	cancel()
	completed := <-response
	defer completed.result.Destroy()
	if !completed.result.Started || completed.err == nil || !errors.Is(completed.err, context.Canceled) {
		t.Fatalf("adversarial cancellation lifecycle = %#v, %v", completed.result, completed.err)
	}
	destroyAttemptAndAssertAbsent(t, worker.docker, target, request.Attempt)
	if _, err := os.Stat(outsideMarker); err != nil {
		t.Fatalf("host marker changed: %v", err)
	}
}

type secretLeakHarness struct{}

func (secretLeakHarness) ID() string      { return "secret-leak-test" }
func (secretLeakHarness) Version() string { return "1.0.0" }
func (secretLeakHarness) Probe() executor.Command {
	return executor.Command{Executable: "node", Args: []string{"-e", `process.stdout.write("1.0.0")`}, Directory: executor.SandboxWorkspaceRoot}
}
func (secretLeakHarness) AgentCommand(string) (executor.Command, error) {
	return executor.Command{
		Executable: "node",
		Args: []string{"-e", `
const fs = require("fs");
const secret = fs.readFileSync("/run/paje/secrets/acceptance/token", "utf8").trim();
fs.writeFileSync("tracked-secret.txt", secret + "\n", {mode: 0o600});
fs.writeFileSync("tracked-secret-base64.txt", Buffer.from(secret).toString("base64") + "\n", {mode: 0o600});
process.stdout.write(JSON.stringify({type:"item.completed",item:{type:"agent_message",text:"wrote requested files"}})+"\n");
process.stdout.write(JSON.stringify({type:"turn.completed"})+"\n");
`},
		Directory: executor.SandboxWorkspaceRoot,
	}, nil
}
func (adapter secretLeakHarness) AgentCommandFor(_ harness.AgentExecutionContext, prompt string) (executor.Command, error) {
	return adapter.AgentCommand(prompt)
}
func (secretLeakHarness) AgentEnvironment([]workerprofile.SecretRequirement) (map[string]string, error) {
	return nil, nil
}
func (secretLeakHarness) Parse(result executor.Result) (string, error) {
	if !result.Started || !result.Completed || result.ExitCode != 0 {
		return "", errors.New("secret leak harness did not complete")
	}
	return "wrote requested files", nil
}
func (secretLeakHarness) AcceptsCapability(capability string) bool {
	return capability == "harness.acceptance-secret"
}

type exactBindingRegistry struct{ binding secret.Binding }

func (registry exactBindingRegistry) Resolve(ctx context.Context, request secret.ResolveRequest) (secret.Binding, error) {
	if err := ctx.Err(); err != nil {
		return secret.Binding{}, err
	}
	if !registry.binding.Authorizes(request) {
		return secret.Binding{}, secret.ErrBindingUnauthorized
	}
	return registry.binding, nil
}

func runWorkerDockerSecretArtifactDenialAcceptance(t *testing.T) {
	t.Helper()
	worker := requireDockerAcceptance(t).publishWorker(t)
	registerRunDockerResourceCleanup(t, worker.docker, "paje-secret-artifact-denial")
	const rawSecret = "paje-live-secret-artifact-denial-4f5b3de5"
	encodedSecret := base64.StdEncoding.EncodeToString([]byte(rawSecret))

	secretRoot := t.TempDir()
	secretDirectory := filepath.Join(secretRoot, "acceptance")
	if err := os.Mkdir(secretDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(secretDirectory, "token"), rawSecret+"\n")
	if err := os.Chmod(filepath.Join(secretDirectory, "token"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := worker.profile.Clone()
	profile.Harness = workerprofile.Harness{ID: "secret-leak-test", Version: "1.0.0"}
	profile.Secrets = []workerprofile.SecretRequirement{{
		Capability: "harness.acceptance-secret", BindingRevision: 1,
		Stage: workerprofile.StageAgent, Delivery: workerprofile.DeliveryDirectory,
		Target: "/run/paje/secrets/acceptance", Required: true,
	}}
	profile.Digest = ""
	var err error
	profile, err = workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := secret.NewBinding(
		secret.BindingRef{Capability: "harness.acceptance-secret", Revision: 1},
		secret.Authorization{ProfileID: profile.Metadata, Stage: workerprofile.StageAgent,
			Delivery: workerprofile.DeliveryDirectory, Target: "/run/paje/secrets/acceptance"},
		"filesystem", secretDirectory,
	)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := secretprovider.NewFilesystem(secretprovider.FilesystemConfig{
		AllowedRoots: []string{secretRoot}, MaxBytes: 1 << 20, MaxEntries: 16, OwnerUID: os.Getuid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := secret.NewBroker(exactBindingRegistry{binding: binding}, map[string]secret.Provider{"filesystem": provider}, secret.BrokerConfig{
		LeaseTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	target := newLiveDockerExecutor(t, publishedWorker{docker: worker.docker, image: worker.image, profile: profile})
	executors, err := executor.NewRegistry(executor.Registration{RuntimeKind: workerprofile.RuntimeOCI, Executor: target})
	if err != nil {
		t.Fatal(err)
	}
	harnesses, err := harness.NewRegistry(secretLeakHarness{})
	if err != nil {
		t.Fatal(err)
	}

	sourceRepository, sourceSHA := newCodexAcceptanceRepository(t)
	workspaceRoot := newSharedAcceptanceRoot(t, worker.docker.repositoryRoot, ".paje-worker-secret-denial-")
	runRoot, artifactRoot, runtimeRoot := t.TempDir(), t.TempDir(), t.TempDir()
	workspaces, err := gitworktree.New(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runfilesystem.New(runRoot)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactfilesystem.New(artifactRoot, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = artifacts.Close() })
	environments, err := environment.NewPolicy(environment.Config{RuntimeRoot: runtimeRoot, Source: baselineEnvironment()})
	if err != nil {
		t.Fatal(err)
	}
	profiles, verifier, capturer, changePolicy, templates := acceptanceWorkflowDependencies(t, workspaceRoot)
	service, err := codechange.New(codechange.Dependencies{
		Templates: templates, Runs: runs, Memory: mock.NewStore(nil), Resolver: workspaces, Workspaces: workspaces,
		Profiles: profiles, WorkerProfiles: workerprofilemock.NewRegistry(profile), SecretBindings: exactBindingRegistry{binding: binding},
		Secrets: broker, Executors: executors, Harnesses: harnesses, Environments: environments,
		Agent: &recordingRunner{}, Verifier: verifier, Capturer: capturer, Policy: changePolicy, Artifacts: artifacts,
		Publisher: publishermock.NewPublisher(publisher.Result{}, nil), Clock: time.Now,
		NewID: func() string { return "paje-secret-artifact-denial" },
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := marshalJSON(t, templatecodechange.Input{
		IdempotencyKey: "paje-secret-artifact-denial", TaskDescription: "exercise live secret denial",
		RepositoryURI: sourceRepository, BaseRef: "main", WorkerProfile: profile.Metadata.String(),
		Profile: "go", Tags: map[string]string{"user_id": "acceptance", "app_id": "secret-denial"},
		Publication: templatecodechange.Publication{Mode: "artifact"},
	})
	resolved, err := service.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	result, executeErr := service.Execute(context.Background(), resolved.RunID)
	if executeErr == nil || result.Artifact != nil || result.Status != run.StatusFailed || result.FailureClass != run.FailurePolicy {
		t.Fatalf("secret-bearing capture result = %#v, chain=%q", result, safeErrorChain(executeErr))
	}
	record, err := runs.Load(context.Background(), resolved.RunID)
	if err != nil || record.Failure == nil || record.Failure.CauseCode != "secret_detected" {
		t.Fatalf("secret denial record = %#v, %v", record, err)
	}
	if broker.ActiveLeases() != 0 {
		t.Fatalf("active secret leases = %d, want zero", broker.ActiveLeases())
	}
	if gitOutput(t, sourceRepository, "rev-parse", "HEAD") != sourceSHA || gitOutput(t, sourceRepository, "status", "--porcelain=v1") != "" {
		t.Fatal("secret-denial workflow changed source checkout")
	}
	assertDirectoryEmpty(t, filepath.Join(workspaceRoot, "worktrees"))
	assertDirectoryEmpty(t, runtimeRoot)
	assertRunDockerResourcesAbsent(t, worker.docker, resolved.RunID)
	assertTreeDoesNotContain(t, runRoot, rawSecret, encodedSecret)
	assertTreeDoesNotContain(t, artifactRoot, rawSecret, encodedSecret)
}

func newSharedAcceptanceRoot(t *testing.T, parent, pattern string) string {
	t.Helper()
	root, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove shared acceptance root: %v", err)
		}
	})
	return root
}

func safeErrorChain(err error) []string {
	var chain []string
	var visit func(error)
	visit = func(current error) {
		if current == nil {
			return
		}
		chain = append(chain, current.Error())
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				visit(child)
			}
			return
		}
		visit(errors.Unwrap(current))
	}
	visit(err)
	return chain
}

type liveAcceptanceProfile struct{ name string }

func (profile liveAcceptanceProfile) Name() string { return profile.name }

func (profile liveAcceptanceProfile) Inspect(ctx context.Context, request repository.ProfileRequest) (repository.ProfileResult, error) {
	if err := ctx.Err(); err != nil {
		return repository.ProfileResult{}, err
	}
	if strings.TrimSpace(request.Workspace) == "" || request.Commands == nil {
		return repository.ProfileResult{}, errors.New("live acceptance profile requires a prepared workspace and sandbox runner")
	}
	commands := make([]verification.Command, 0, 2)
	for _, module := range []string{"module-a", "module-b"} {
		command, err := verification.Compile(verification.CommandSpec{
			Name: "go test " + module, Directory: module, Executable: "go",
			Args: []string{"test", "./..."}, Timeout: "2m", Required: true,
		}, request.Workspace, verification.DefaultLimits)
		if err != nil {
			return repository.ProfileResult{}, err
		}
		command.Environment = map[string]string{"GOWORK": "off"}
		commands = append(commands, command)
	}
	return repository.ProfileResult{
		Facts: map[string]string{
			"profile": "bounded-live-docker-acceptance",
		},
		Modules:  []string{"module-a", "module-b"},
		Commands: commands,
	}, nil
}

func acceptanceWorkflowDependencies(t *testing.T, workspaceRoot string) (map[string]repository.Profile, verification.Runner, gitcapture.Capturer, policy.Evaluator, *template.Registry) {
	t.Helper()
	verifier, err := verification.NewExecutor(verification.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	changePolicy, err := policy.NewChangePolicy(policy.Config{Workspace: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	templates, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]repository.Profile{
		"generic": liveAcceptanceProfile{name: "generic"},
		"go":      liveAcceptanceProfile{name: "go"},
	}, verifier, capturer, changePolicy, templates
}

func assertTreeDoesNotContain(t *testing.T, root string, values ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, value := range values {
			if strings.Contains(string(contents), value) {
				return fmt.Errorf("durable tree contains forbidden secret material in %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

var _ runner.Runner = (*recordingRunner)(nil)
