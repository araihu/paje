package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/approval"
	approvalmock "github.com/araihu/paje/internal/approval/mock"
	artifactfilesystem "github.com/araihu/paje/internal/artifact/filesystem"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/executor"
	executormock "github.com/araihu/paje/internal/executor/mock"
	"github.com/araihu/paje/internal/harness"
	harnesscodex "github.com/araihu/paje/internal/harness/codex"
	"github.com/araihu/paje/internal/memory"
	memorymock "github.com/araihu/paje/internal/memory/mock"
	"github.com/araihu/paje/internal/policy"
	"github.com/araihu/paje/internal/publisher"
	publishermock "github.com/araihu/paje/internal/publisher/mock"
	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/run"
	runfilesystem "github.com/araihu/paje/internal/run/filesystem"
	"github.com/araihu/paje/internal/runner"
	codexrunner "github.com/araihu/paje/internal/runner/codex"
	"github.com/araihu/paje/internal/secret"
	secretmock "github.com/araihu/paje/internal/secret/mock"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workerprofile"
	workerprofilemock "github.com/araihu/paje/internal/workerprofile/mock"
	"github.com/araihu/paje/internal/workflow/codechange"
	"github.com/araihu/paje/internal/workspace/gitworktree"
)

const (
	codexAcceptanceRunID  = "paje-beta-codex-acceptance"
	codexAcceptanceMarker = "PAJE_BETA_CODEX_ACCEPTANCE_TERMINAL_20260725"
)

func TestCodexAcceptancePortableCompositionReachesResolve(t *testing.T) {
	sourceRepository, _ := newCodexAcceptanceRepository(t)
	workspaces, err := gitworktree.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runfilesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactfilesystem.New(t.TempDir(), 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = artifacts.Close() })
	environments, err := environment.NewPolicy(environment.Config{
		RuntimeRoot: t.TempDir(), Source: baselineEnvironment(),
	})
	if err != nil {
		t.Fatal(err)
	}
	genericProfile, err := repository.NewGenericProfile(verification.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	goProfile, err := repository.NewGoProfile(verification.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := verification.NewExecutor(verification.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatal(err)
	}
	changePolicy, err := policy.NewChangePolicy(policy.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	templates, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		t.Fatal(err)
	}
	portable := newCodexAcceptancePortableRuntime(t)
	service, err := codechange.New(codechange.Dependencies{
		Templates: templates, Runs: runs, Memory: memorymock.NewStore(nil),
		Resolver: workspaces, Workspaces: workspaces,
		Profiles:       map[string]repository.Profile{"generic": genericProfile, "go": goProfile},
		WorkerProfiles: portable.workerProfiles, SecretBindings: portable.secretBindings,
		Secrets: portable.secrets, Executors: portable.executors, Harnesses: portable.harnesses,
		Environments: environments, Agent: &recordingRunner{}, Verifier: verifier,
		Capturer: capturer, Policy: changePolicy, Artifacts: artifacts,
		Publisher: publishermock.NewPublisher(publisher.Result{}, nil),
		Clock:     time.Now, NewID: func() string { return "portable-codex-construction" },
	})
	if err != nil {
		t.Fatalf("portable acceptance composition failed before Resolve: %v", err)
	}
	raw, err := json.Marshal(templatecodechange.Input{
		IdempotencyKey: "portable-codex-construction", TaskDescription: "prove portable Resolve",
		RepositoryURI: sourceRepository, BaseRef: "main", Profile: "go",
		WorkerProfile: "codex-go@1", Tags: map[string]string{"user_id": "acceptance", "app_id": "portable"},
		Publication: templatecodechange.Publication{Mode: "artifact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := service.Resolve(context.Background(), raw)
	if err != nil || resolved.RunID != "portable-codex-construction" {
		t.Fatalf("portable acceptance Resolve() result=%#v error=%v", resolved, err)
	}
	record, err := runs.Load(context.Background(), resolved.RunID)
	if err != nil || record.WorkerProfile == nil ||
		record.WorkerProfile.Metadata != portable.profile.Metadata ||
		len(record.SecretBindings) != 1 || record.SecretBindings[0].Revision != 1 {
		t.Fatalf("portable acceptance resolved binding=%#v error=%v", record, err)
	}
}

func TestCodexArtifactAcceptance(t *testing.T) {
	requireOptIn(t, "PAJE_CODEX_INTEGRATION", "the authenticated Codex artifact acceptance test")
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatal("authenticated Codex acceptance requires codex on PATH")
	}
	codexHome := existingCodexHome(t)

	sourceRepository, baseSHA := newCodexAcceptanceRepository(t)
	sourceTree := gitOutput(t, sourceRepository, "write-tree")
	sourceStatus := gitOutput(t, sourceRepository, "status", "--porcelain=v1")
	workspaceRoot := t.TempDir()
	runtimeRoot := t.TempDir()
	runRoot := t.TempDir()
	artifactRoot := t.TempDir()
	agentPIDFile := filepath.Join(t.TempDir(), "codex.pid")
	codexWrapper := filepath.Join(t.TempDir(), "codex-acceptance")
	writeFile(t, codexWrapper, "#!/bin/sh\nprintf '%s\\n' \"$$\" > \"$PAJE_ACCEPTANCE_PID_FILE\"\nexec codex \"$@\"\n")
	if err := os.Chmod(codexWrapper, 0o700); err != nil {
		t.Fatalf("make Codex acceptance wrapper executable: %v", err)
	}
	workspaces, err := gitworktree.New(workspaceRoot)
	if err != nil {
		t.Fatalf("create Git worktree manager: %v", err)
	}
	runs, err := runfilesystem.New(runRoot)
	if err != nil {
		t.Fatalf("create filesystem run store: %v", err)
	}
	artifacts, err := artifactfilesystem.New(artifactRoot, 10<<20)
	if err != nil {
		t.Fatalf("create filesystem artifact store: %v", err)
	}
	artifactStoreClosed := false
	t.Cleanup(func() {
		if !artifactStoreClosed {
			_ = artifacts.Close()
		}
	})

	sourceEnvironment := baselineEnvironment()
	sourceEnvironment["HATCHET_CLIENT_TOKEN"] = "acceptance-hatchet-secret"
	sourceEnvironment["MEM0_API_KEY"] = "acceptance-mem0-secret"
	sourceEnvironment["GITHUB_TOKEN"] = "acceptance-github-secret"
	sourceEnvironment["GH_TOKEN"] = "acceptance-gh-secret"
	sourceEnvironment["PAJE_ACCEPTANCE_PID_FILE"] = agentPIDFile
	environments, err := environment.NewPolicy(environment.Config{
		RuntimeRoot: runtimeRoot,
		Source:      sourceEnvironment,
		Allowed:     []string{"PAJE_ACCEPTANCE_PID_FILE"},
		CodexHome:   codexHome,
		CodexAgent:  true,
	})
	if err != nil {
		t.Fatalf("create environment policy: %v", err)
	}

	limits := verification.DefaultLimits
	verifierDelegate, err := verification.NewExecutor(limits)
	if err != nil {
		t.Fatalf("create verification executor: %v", err)
	}
	initialEnvironment := baselineEnvironment()
	initialEnvironment["HOME"] = t.TempDir()
	initialEnvironment["TMPDIR"] = t.TempDir()
	initialEnvironment["TMP"] = initialEnvironment["TMPDIR"]
	initialEnvironment["TEMP"] = initialEnvironment["TMPDIR"]
	for _, module := range []string{"module-a", "module-b"} {
		initial := verifierDelegate.Run(context.Background(), verification.Command{
			Name: "initial go test " + module, Directory: filepath.Join(sourceRepository, module),
			Executable: "go", Args: []string{"test", "./..."},
			Environment: map[string]string{"GOWORK": "off"},
			Timeout:     time.Minute, Required: true,
		}, initialEnvironment)
		if !initial.Passed {
			t.Fatalf("initial fixture verification for %s failed: %s/%s", module, initial.FailureClass, initial.CauseCode)
		}
	}
	verifier := &recordingVerifier{delegate: verifierDelegate}
	genericProfile, err := repository.NewGenericProfile(limits)
	if err != nil {
		t.Fatalf("create generic profile: %v", err)
	}
	goProfile, err := repository.NewGoProfile(limits)
	if err != nil {
		t.Fatalf("create Go profile: %v", err)
	}
	codexDelegate, err := codexrunner.New(codexWrapper)
	if err != nil {
		t.Fatalf("create Codex runner: %v", err)
	}
	agent := &recordingRunner{delegate: codexDelegate}
	portable := newCodexAcceptancePortableRuntime(t)
	configureCodexAcceptanceExecutor(
		portable.target, environments, agent, verifierDelegate, verifier,
	)
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatalf("create Git capture adapter: %v", err)
	}
	changePolicy, err := policy.NewChangePolicy(policy.Config{Workspace: workspaceRoot})
	if err != nil {
		t.Fatalf("create change policy: %v", err)
	}
	registry, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		t.Fatalf("create template registry: %v", err)
	}
	memoryStore := memorymock.NewStore([]memory.Memory{{
		ID:       "paje-beta-acceptance-memory",
		Content:  "Pajé beta acceptance instruction: in module-a/greeting/greeting.go, replace exactly `const Message = \"before\"` with `const Message = \"after\"`. Do not change any other file.",
		Metadata: map[string]string{"user_id": "paje-beta", "app_id": "acceptance"},
	}})
	gate := approvalmock.NewGate(approval.Result{}, nil)
	publicationProvider := publishermock.NewPublisher(publisher.Result{}, nil)
	service, err := codechange.New(codechange.Dependencies{
		Templates: registry, Runs: runs, Memory: memoryStore,
		Resolver: workspaces, Workspaces: workspaces,
		Profiles:       map[string]repository.Profile{"generic": genericProfile, "go": goProfile},
		WorkerProfiles: portable.workerProfiles, SecretBindings: portable.secretBindings,
		Secrets: portable.secrets, Executors: portable.executors, Harnesses: portable.harnesses,
		Environments: environments, Agent: agent, Verifier: verifier,
		Capturer: capturer, Policy: changePolicy, Artifacts: artifacts,
		Publisher: publicationProvider, Clock: time.Now,
		NewID: func() string { return codexAcceptanceRunID },
	})
	if err != nil {
		t.Fatalf("compose code-change service: %v", err)
	}

	input := templatecodechange.Input{
		IdempotencyKey:  codexAcceptanceRunID,
		TaskDescription: "Apply the exact one-line source edit specified by the scoped acceptance memory. Edit no other file. Do not alter tests, module files, or Git metadata. After the edit, reply with exactly " + codexAcceptanceMarker + ".",
		RepositoryURI:   sourceRepository,
		BaseRef:         "main",
		MemoryQuery:     "Pajé beta acceptance instruction",
		MemoryLimit:     1,
		Tags:            map[string]string{"user_id": "paje-beta", "app_id": "acceptance"},
		Profile:         "go",
		WorkerProfile:   portable.profile.Metadata.String(),
		Publication:     templatecodechange.Publication{Mode: "artifact"},
	}
	rawInput, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal code-change input: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	resolved, err := service.Resolve(ctx, rawInput)
	if err != nil || resolved.RunID != codexAcceptanceRunID {
		t.Fatalf("Resolve() result=%#v error=%v", resolved, err)
	}
	executed, err := service.Execute(ctx, resolved.RunID)
	if err != nil || executed.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", executed, err)
	}
	approved, err := service.Approval(ctx, resolved.RunID, gate)
	if err != nil {
		t.Fatalf("Approval() result=%#v error=%v", approved, err)
	}
	published, err := service.Publish(ctx, resolved.RunID)
	if err != nil {
		t.Fatalf("Publish() result=%#v error=%v", published, err)
	}
	result, err := service.Finalize(ctx, resolved.RunID)
	if err != nil || result.Status != run.StatusSucceeded {
		t.Fatalf("Finalize() result=%#v error=%v", result, err)
	}

	requests := agent.Requests()
	if len(requests) != 1 {
		t.Fatalf("real Codex calls = %d, want 1", len(requests))
	}
	if !strings.Contains(requests[0].TaskDescription, "Pajé beta acceptance instruction") ||
		!strings.Contains(requests[0].TaskDescription, "module-a/greeting/greeting.go") {
		t.Fatal("Codex prompt omitted the scoped memory instruction")
	}
	if requests[0].Env["CODEX_HOME"] == "" {
		t.Fatal("Codex environment omitted explicit CODEX_HOME")
	}
	if got := portable.workerProfiles.Requests(); !reflect.DeepEqual(
		got, []workerprofile.ProfileID{portable.profile.Metadata},
	) {
		t.Fatalf("portable worker profile resolutions = %#v", got)
	}
	leaseRequests := portable.secrets.Requests()
	if len(leaseRequests) != 1 ||
		leaseRequests[0].ProfileID != portable.profile.Metadata ||
		leaseRequests[0].Binding != 1 ||
		!reflect.DeepEqual(portable.secrets.Revocations(), []string{"codex-acceptance-lease"}) {
		t.Fatalf("portable exact lease lifecycle = requests %#v revocations %#v",
			leaseRequests, portable.secrets.Revocations())
	}
	pidData, err := os.ReadFile(agentPIDFile)
	if err != nil {
		t.Fatalf("read Codex process-group evidence: %v", err)
	}
	agentProcessGroup, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil || agentProcessGroup <= 0 {
		t.Fatalf("invalid Codex process-group evidence")
	}
	assertDeniedEnvironmentKeys(t, requests[0].Env)

	commands, verificationEnvironments := verifier.Calls()
	if len(commands) != 2 || len(verificationEnvironments) != 2 {
		t.Fatalf("verification calls = %d, want one per module", len(commands))
	}
	wantModules := []string{"module-a", "module-b"}
	for index, command := range commands {
		if filepath.Base(command.Directory) != wantModules[index] ||
			!reflect.DeepEqual(command.Args, []string{"test", "./..."}) ||
			command.Environment["GOWORK"] != "off" {
			t.Fatalf("verification command %d = %#v", index, command)
		}
		assertDeniedEnvironmentKeys(t, verificationEnvironments[index])
	}

	record, err := runs.Load(ctx, result.RunID)
	if err != nil {
		t.Fatalf("load finalized run: %v", err)
	}
	wantStages := map[string]run.StageStatus{
		"resolve": run.StageSucceeded, "execute": run.StageSucceeded,
		"approval": run.StageSkipped, "publish": run.StageSkipped,
		"finalize": run.StageSucceeded,
	}
	for _, stage := range record.Stages {
		if want, found := wantStages[stage.Name]; found {
			if stage.Status != want {
				t.Fatalf("stage %q status = %q, want %q", stage.Name, stage.Status, want)
			}
			delete(wantStages, stage.Name)
		}
		if stage.Name == "execute" {
			assertNoCredentialEvidence(t, stage.Evidence["agent_environment_keys"])
			assertNoCredentialEvidence(t, stage.Evidence["verification_environment_keys"])
		}
	}
	if len(wantStages) != 0 {
		t.Fatalf("missing durable stages: %v", wantStages)
	}
	if got := len(gate.Requests()); got != 0 {
		t.Fatalf("approval calls = %d, want zero in artifact mode", got)
	}
	if got := publicationProvider.CallCount(); got != 0 {
		t.Fatalf("publisher calls = %d, want zero in artifact mode", got)
	}

	if result.Artifact != *executed.Artifact {
		t.Fatalf("final artifact = %#v, execute artifact = %#v", result.Artifact, *executed.Artifact)
	}
	bundle, err := artifacts.Load(ctx, result.Artifact)
	if err != nil {
		t.Fatalf("load artifact before restart: %v", err)
	}
	if string(bundle.AgentOutput) != codexAcceptanceMarker {
		t.Fatalf("Codex terminal output = %q, want exact marker", bundle.AgentOutput)
	}
	if len(bundle.Manifest.Changes) != 1 || bundle.Manifest.Changes[0].Path != "module-a/greeting/greeting.go" ||
		bundle.Manifest.TreeSHA == "" || bundle.Manifest.MemoryCount != 1 ||
		!reflect.DeepEqual(bundle.Manifest.MemoryIDs, []string{"paje-beta-acceptance-memory"}) {
		t.Fatalf("artifact manifest does not prove the requested scoped change: %#v", bundle.Manifest)
	}
	if len(bundle.Verification) != 2 || !bundle.Verification[0].Passed || !bundle.Verification[1].Passed {
		t.Fatalf("artifact verification evidence = %#v", bundle.Verification)
	}

	if err := artifacts.Close(); err != nil {
		t.Fatalf("close artifact store before restart: %v", err)
	}
	artifactStoreClosed = true
	restartedRuns, err := runfilesystem.New(runRoot)
	if err != nil {
		t.Fatalf("reconstruct run store: %v", err)
	}
	restartedArtifacts, err := artifactfilesystem.New(artifactRoot, 10<<20)
	if err != nil {
		t.Fatalf("reconstruct artifact store: %v", err)
	}
	t.Cleanup(func() { _ = restartedArtifacts.Close() })
	reloadedRecord, err := restartedRuns.Load(ctx, result.RunID)
	if err != nil || reloadedRecord.Status != run.StatusSucceeded || !reloadedRecord.OutcomeMemorySaved {
		t.Fatalf("reloaded run=%#v error=%v", reloadedRecord, err)
	}
	reloadedBundle, err := restartedArtifacts.Load(ctx, result.Artifact)
	if err != nil {
		t.Fatalf("reload artifact after store reconstruction: %v", err)
	}
	if reloadedBundle.Manifest.TreeSHA != bundle.Manifest.TreeSHA {
		t.Fatal("reloaded artifact tree binding changed")
	}

	fresh, err := workspaces.Prepare(ctx, sourceRepository, baseSHA)
	if err != nil {
		t.Fatalf("prepare fresh reproduction worktree: %v", err)
	}
	if err := capturer.Apply(ctx, gitcapture.ApplyRequest{
		Workspace: fresh.Path(), BaseSHA: baseSHA,
		Patch: reloadedBundle.ChangesPatch, ExpectedTreeSHA: reloadedBundle.Manifest.TreeSHA,
	}); err != nil {
		t.Fatalf("apply reloaded artifact: %v", err)
	}
	if got := gitOutput(t, fresh.Path(), "write-tree"); got != reloadedBundle.Manifest.TreeSHA {
		t.Fatalf("reproduced tree = %q, want manifest tree %q", got, reloadedBundle.Manifest.TreeSHA)
	}
	if got, err := os.ReadFile(filepath.Join(fresh.Path(), "module-a", "greeting", "greeting.go")); err != nil ||
		string(got) != "package greeting\n\nconst Message = \"after\"\n" {
		t.Fatalf("reproduced requested file = %q, error=%v", got, err)
	}
	if err := fresh.Cleanup(ctx); err != nil {
		t.Fatalf("cleanup reproduction worktree: %v", err)
	}

	if got := gitOutput(t, sourceRepository, "write-tree"); got != sourceTree {
		t.Fatalf("source tree changed: got=%q want=%q", got, sourceTree)
	}
	if got := gitOutput(t, sourceRepository, "rev-parse", "HEAD"); got != baseSHA {
		t.Fatalf("source HEAD changed: got=%q want=%q", got, baseSHA)
	}
	if got := gitOutput(t, sourceRepository, "status", "--porcelain=v1"); got != sourceStatus || got != "" {
		t.Fatalf("source checkout status changed: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(sourceRepository, "module-a", "greeting", "greeting.go")); err != nil ||
		string(got) != "package greeting\n\nconst Message = \"before\"\n" {
		t.Fatalf("source requested file changed: %q, error=%v", got, err)
	}
	assertDirectoryEmpty(t, filepath.Join(workspaceRoot, "worktrees"))
	assertDirectoryEmpty(t, runtimeRoot)
	assertNoProcessGroup(t, agentProcessGroup)
	assertNoProcessContains(t, codexAcceptanceMarker)
	if memories := memoryStore.Memories(); len(memories) != 2 ||
		!strings.Contains(memories[1].Content, result.RunID) ||
		!strings.Contains(memories[1].Content, result.Artifact.Digest) {
		t.Fatalf("durable outcome memory = %#v", memories)
	}
}

type codexAcceptancePortableRuntime struct {
	profile        workerprofile.Snapshot
	workerProfiles *workerprofilemock.Registry
	secretBindings *codexAcceptanceSecretRegistry
	secrets        *secretmock.Broker
	executors      *executor.Registry
	harnesses      *harness.Registry
	target         *executormock.Executor
}

func newCodexAcceptancePortableRuntime(t *testing.T) codexAcceptancePortableRuntime {
	t.Helper()
	profile, err := workerprofile.Canonicalize(workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "codex-go", Revision: 1},
		Runtime: workerprofile.Runtime{
			Kind: workerprofile.RuntimeOCI, Image: "example.invalid/paje-codex@sha256:" + strings.Repeat("a", 64),
			Platform: "linux/amd64", Network: workerprofile.NetworkOutbound, ReadOnlyRoot: true,
		},
		Resources: workerprofile.Resources{CPUMillis: 4000, MemoryBytes: 8 << 30, PIDs: 512},
		Harness:   workerprofile.Harness{ID: harnesscodex.ID, Version: harnesscodex.SupportedVersion},
		Secrets: []workerprofile.SecretRequirement{{
			Capability: "harness.codex-auth", BindingRevision: 1, Stage: workerprofile.StageAgent,
			Delivery: workerprofile.DeliveryDirectory, Target: "/run/paje/secrets/codex", Required: true,
		}},
	})
	if err != nil {
		t.Fatalf("canonicalize Codex acceptance worker profile: %v", err)
	}
	bindingRef := secret.BindingRef{Capability: "harness.codex-auth", Revision: 1}
	binding, err := secret.NewBinding(bindingRef, secret.Authorization{
		ProfileID: profile.Metadata, Stage: workerprofile.StageAgent,
		Delivery: workerprofile.DeliveryDirectory, Target: "/run/paje/secrets/codex",
	}, "filesystem", "/acceptance/codex-auth")
	if err != nil {
		t.Fatalf("create Codex acceptance secret binding: %v", err)
	}
	authFile, err := secret.NewFile(
		"auth.json", 0o600,
		[]byte("paje-codex-acceptance-secret-material-5e2ad1ab"),
	)
	if err != nil {
		t.Fatalf("create Codex acceptance secret file: %v", err)
	}
	materialization, err := secret.NewDirectoryMaterialization(
		"/run/paje/secrets/codex", []secret.File{authFile},
	)
	authFile.Zero()
	if err != nil {
		t.Fatalf("create Codex acceptance secret materialization: %v", err)
	}
	lease, err := secret.NewLease("codex-acceptance-lease", time.Now().Add(time.Hour), materialization)
	materialization.Destroy()
	if err != nil {
		t.Fatalf("create Codex acceptance secret lease: %v", err)
	}
	broker := secretmock.NewBroker()
	broker.SetAcquireResult(bindingRef.Capability, lease, nil)
	lease.Destroy()

	target := executormock.New()
	executors, err := executor.NewRegistry(executor.Registration{
		RuntimeKind: workerprofile.RuntimeOCI, Executor: target,
	})
	if err != nil {
		t.Fatalf("create Codex acceptance executor registry: %v", err)
	}
	adapter, err := harnesscodex.New(harnesscodex.SupportedVersion)
	if err != nil {
		t.Fatalf("create Codex acceptance harness: %v", err)
	}
	harnesses, err := harness.NewRegistry(adapter)
	if err != nil {
		t.Fatalf("create Codex acceptance harness registry: %v", err)
	}
	return codexAcceptancePortableRuntime{
		profile: profile, workerProfiles: workerprofilemock.NewRegistry(profile),
		secretBindings: &codexAcceptanceSecretRegistry{binding: binding},
		secrets:        broker, executors: executors, harnesses: harnesses, target: target,
	}
}

type codexAcceptanceSecretRegistry struct {
	binding secret.Binding
}

func (registry *codexAcceptanceSecretRegistry) Resolve(
	ctx context.Context,
	request secret.ResolveRequest,
) (secret.Binding, error) {
	if err := ctx.Err(); err != nil {
		return secret.Binding{}, err
	}
	if registry == nil || !registry.binding.Authorizes(request) {
		return secret.Binding{}, secret.ErrBindingUnauthorized
	}
	return registry.binding, nil
}

func configureCodexAcceptanceExecutor(
	target *executormock.Executor,
	environments environment.Builder,
	agent runner.Runner,
	preflight verification.Runner,
	verifier verification.Runner,
) {
	target.SetBeforeExecute(func(ctx context.Context, request executor.Request) {
		result, err := executeCodexAcceptanceRequest(
			ctx, request, environments, agent, preflight, verifier,
		)
		target.SetResult(request.Attempt, result, err)
		result.Destroy()
	})
}

func executeCodexAcceptanceRequest(
	ctx context.Context,
	request executor.Request,
	environments environment.Builder,
	agent runner.Runner,
	preflight verification.Runner,
	verifier verification.Runner,
) (executor.Result, error) {
	stage := environment.StageVerification
	requestedKeys := []string(nil)
	if request.Attempt.Purpose == executor.PurposeAgent {
		stage = environment.StageAgent
		requestedKeys = []string{"PAJE_ACCEPTANCE_PID_FILE"}
	}
	built, err := environments.Build(ctx, environment.Request{
		RunID: request.Attempt.RunID, Stage: stage, RequestedKeys: requestedKeys,
	})
	if err != nil {
		return executor.Result{}, err
	}
	var execution executor.Result
	var executeErr error
	switch request.Attempt.Purpose {
	case executor.PurposeAgent:
		if len(request.Command.Args) == 0 {
			executeErr = errors.New("Codex acceptance agent prompt is missing")
			break
		}
		legacy, runErr := agent.Run(ctx, runner.RunRequest{
			TaskDescription: request.Command.Args[len(request.Command.Args)-1],
			WorkspacePath:   request.Workspace.HostPath,
			Env:             built.Values,
		})
		execution = executor.Result{
			Created: legacy.Started, Started: legacy.Started, Completed: legacy.Completed,
			ExitCode: legacy.ExitCode, Stdout: []byte(legacy.Transcript),
			Duration:        time.Duration(legacy.Duration * float64(time.Second)),
			StdoutTruncated: legacy.Truncated,
		}
		executeErr = runErr
	case executor.PurposeProbe, executor.PurposeVerification:
		directory, directoryErr := codexAcceptanceHostDirectory(
			request.Workspace.HostPath, request.Command.Directory,
		)
		if directoryErr != nil {
			executeErr = directoryErr
			break
		}
		command := verification.Command{
			Name: request.Command.Executable, Directory: directory,
			Executable: request.Command.Executable, Args: append([]string(nil), request.Command.Args...),
			Environment: cloneMap(request.Command.Environment),
			Timeout:     request.Timeout, Required: true,
		}
		delegate := preflight
		if request.Attempt.Purpose == executor.PurposeVerification {
			delegate = verifier
		}
		verified := delegate.Run(ctx, command, built.Values)
		execution = executor.Result{
			Created: true, Started: true, Completed: true,
			ExitCode: verified.ExitCode, Stdout: []byte(verified.Output), Duration: verified.Duration,
			StdoutTruncated: verified.Truncated,
		}
		if !verified.Passed && execution.ExitCode == 0 {
			execution.ExitCode = 1
		}
		if request.Attempt.Purpose == executor.PurposeProbe && request.Attempt.Sequence == 0 {
			execution.SafeFacts = map[string]string{
				"runtime_kind": workerprofile.RuntimeOCI,
				"image":        request.Profile.Runtime.Image,
				"platform":     request.Profile.Runtime.Platform,
				"isolated":     "true",
				"certified":    "false",
			}
		}
	default:
		executeErr = errors.New("unsupported Codex acceptance executor purpose")
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	cleanupErr := environments.Cleanup(cleanupCtx, request.Attempt.RunID)
	cancel()
	return execution, errors.Join(executeErr, cleanupErr)
}

func codexAcceptanceHostDirectory(workspacePath, sandboxDirectory string) (string, error) {
	const root = executor.SandboxWorkspaceRoot
	relative := "."
	switch {
	case sandboxDirectory == root:
	case strings.HasPrefix(sandboxDirectory, root+"/"):
		relative = strings.TrimPrefix(sandboxDirectory, root+"/")
	default:
		return "", errors.New("Codex acceptance sandbox directory is outside the workspace")
	}
	directory := filepath.Clean(filepath.Join(workspacePath, filepath.FromSlash(relative)))
	contained, err := filepath.Rel(filepath.Clean(workspacePath), directory)
	if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
		return "", errors.New("Codex acceptance host directory escapes the workspace")
	}
	return directory, nil
}

func newCodexAcceptanceRepository(t *testing.T) (string, string) {
	t.Helper()
	repository := t.TempDir()
	gitOutput(t, repository, "init", "-b", "main")
	gitOutput(t, repository, "config", "user.name", "Pajé acceptance")
	gitOutput(t, repository, "config", "user.email", "paje-acceptance@example.invalid")
	writeFile(t, filepath.Join(repository, "module-a", "go.mod"), "module example.invalid/paje-acceptance/module-a\n\ngo 1.26.0\n")
	writeFile(t, filepath.Join(repository, "module-a", "greeting", "greeting.go"), "package greeting\n\nconst Message = \"before\"\n")
	writeFile(t, filepath.Join(repository, "module-a", "greeting", "greeting_test.go"), "package greeting\n\nimport \"testing\"\n\nfunc TestMessageIsPresent(t *testing.T) {\n\tif Message == \"\" {\n\t\tt.Fatal(\"Message is empty\")\n\t}\n}\n")
	writeFile(t, filepath.Join(repository, "module-b", "go.mod"), "module example.invalid/paje-acceptance/module-b\n\ngo 1.26.0\n")
	writeFile(t, filepath.Join(repository, "module-b", "status", "status.go"), "package status\n\nconst Ready = true\n")
	writeFile(t, filepath.Join(repository, "module-b", "status", "status_test.go"), "package status\n\nimport \"testing\"\n\nfunc TestReady(t *testing.T) {\n\tif !Ready {\n\t\tt.Fatal(\"not ready\")\n\t}\n}\n")
	gitOutput(t, repository, "add", ".")
	gitOutput(t, repository, "commit", "-m", "seed two-module acceptance fixture")
	return repository, gitOutput(t, repository, "rev-parse", "HEAD")
}

func assertDeniedEnvironmentKeys(t *testing.T, values map[string]string) {
	t.Helper()
	for _, key := range []string{
		"HATCHET_CLIENT_TOKEN", "MEM0_API_KEY", "GITHUB_TOKEN", "GH_TOKEN",
		"GIT_ASKPASS", "PAJE_GIT_PASSWORD",
	} {
		if _, found := values[key]; found {
			t.Fatalf("filtered child environment contains denied key %q", key)
		}
	}
}

func assertNoCredentialEvidence(t *testing.T, evidence string) {
	t.Helper()
	for _, key := range []string{"HATCHET_", "MEM0_", "GITHUB_", "GH_TOKEN", "GIT_ASKPASS", "PAJE_GIT_"} {
		if strings.Contains(evidence, key) {
			t.Fatalf("durable child-environment evidence contains credential key class %q", key)
		}
	}
}
