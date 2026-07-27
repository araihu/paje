package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestAttemptIDRequiresCompleteDeterministicIdentity(t *testing.T) {
	valid := validAttemptID()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}

	tests := map[string]func(*AttemptID){
		"run ID":     func(id *AttemptID) { id.RunID = "" },
		"stage":      func(id *AttemptID) { id.Stage = "../execute" },
		"attempt":    func(id *AttemptID) { id.Attempt = 0 },
		"started at": func(id *AttemptID) { id.StartedAt = time.Time{} },
		"purpose":    func(id *AttemptID) { id.Purpose = "publish" },
		"sequence":   func(id *AttemptID) { id.Sequence = -1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			id := valid
			mutate(&id)
			if err := id.Validate(); err == nil {
				t.Fatalf("invalid AttemptID accepted: %#v", id)
			}
		})
	}

	other := valid
	other.Sequence++
	if valid.Key() == other.Key() {
		t.Fatal("distinct complete attempt identities collided")
	}
}

func TestRequestRejectsSecretsOutsideAgent(t *testing.T) {
	request := validOCIRequest(t)
	request.Attempt.Purpose = PurposeVerification
	if err := request.Validate(); err == nil {
		t.Fatal("verification secret accepted")
	}
	request.Destroy()

	request = validOCIRequest(t)
	request.Attempt.Purpose = PurposeProbe
	if err := request.Validate(); err == nil {
		t.Fatal("probe secret accepted")
	}
	request.Destroy()
}

func TestRequestValidationFailsClosed(t *testing.T) {
	tests := map[string]func(*Request){
		"noncanonical profile": func(request *Request) { request.Profile.Digest = "" },
		"relative host path":   func(request *Request) { request.Workspace.HostPath = "relative" },
		"host root":            func(request *Request) { request.Workspace.HostPath = string(filepath.Separator) },
		"relative sandbox root": func(request *Request) {
			request.Workspace.SandboxPath = "workspace"
		},
		"command outside workspace": func(request *Request) { request.Command.Directory = "/outside" },
		"shell command":             func(request *Request) { request.Command.Executable = "sh" },
		"environment collision": func(request *Request) {
			request.Environment["PATH"] = "/one"
			request.Command.Environment["PATH"] = "/two"
		},
		"zero timeout":      func(request *Request) { request.Timeout = 0 },
		"zero output limit": func(request *Request) { request.OutputLimit = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := validHostRequest(t)
			mutate(&request)
			if err := request.Validate(); err == nil {
				t.Fatal("invalid request accepted")
			}
			request.Destroy()
		})
	}
}

func TestRequestRejectsEnvironmentSecretCollisions(t *testing.T) {
	for name, setEnvironment := range map[string]func(*Request){
		"baseline": func(request *Request) { request.Environment["WORKLOAD_TOKEN"] = "public-value" },
		"command":  func(request *Request) { request.Command.Environment["WORKLOAD_TOKEN"] = "public-value" },
	} {
		t.Run(name, func(t *testing.T) {
			request := validEnvironmentSecretRequest(t)
			defer request.Destroy()
			setEnvironment(&request)
			if err := request.Validate(); err == nil {
				t.Fatal("environment-delivered secret collision accepted")
			}
		})
	}
}

func TestRequestAppliesOneAggregateEnvironmentByteLimit(t *testing.T) {
	request := validHostRequest(t)
	request.Environment = map[string]string{"BASELINE": strings.Repeat("a", (1<<19)-len("BASELINE"))}
	request.Command.Environment = map[string]string{"COMMAND": strings.Repeat("b", (1<<19)-len("COMMAND")+1)}
	if err := request.Validate(); err == nil {
		t.Fatal("combined oversized environment accepted")
	}
}

func TestRequestDeniesSerializationAndFormattingLeaks(t *testing.T) {
	request := validOCIRequest(t)
	hostPath := request.Workspace.HostPath
	secretValue := string(request.Secrets[0].Value())
	if _, err := json.Marshal(request); !errors.Is(err, secret.ErrSecretSerialization) {
		t.Fatalf("Marshal() error = %v", err)
	}
	formatted := fmt.Sprintf("%+v", request)
	if strings.Contains(formatted, hostPath) || strings.Contains(formatted, secretValue) {
		t.Fatalf("formatted request leaked transient values: %s", formatted)
	}
	request.Destroy()
}

func TestRequestCloneDefensivelyCopiesAndDestroyClearsSecrets(t *testing.T) {
	request := validOCIRequest(t)
	clone := request.Clone()
	clone.Profile.Tools[0].Probe.Args[0] = "mutated"
	clone.Command.Args[0] = "mutated"
	clone.Command.Environment["GOWORK"] = "on"
	clone.Environment["PATH"] = "/mutated"
	value := clone.Secrets[0].Value()
	value[0] = 'X'

	if request.Profile.Tools[0].Probe.Args[0] == "mutated" ||
		request.Command.Args[0] == "mutated" ||
		request.Command.Environment["GOWORK"] == "on" ||
		request.Environment["PATH"] == "/mutated" ||
		request.Secrets[0].Value()[0] == 'X' {
		t.Fatal("Request.Clone aliases caller-owned values")
	}

	clone.Destroy()
	if clone.Secrets != nil {
		t.Fatalf("Destroy retained secret materializations: %#v", clone.Secrets)
	}
	if got := request.Secrets[0].Value(); string(got) != "top-secret" {
		t.Fatalf("destroying clone mutated original secret: %q", got)
	}
	request.Destroy()
}

func TestResultCloneAndSafeFactsDoNotAlias(t *testing.T) {
	receipt, err := NewRandomChildStartReceipt(
		validAttemptID(),
		Command{Executable: "go", Args: []string{"version"}, Directory: SandboxWorkspaceRoot},
		map[string]string{"PATH": CanonicalSandboxPATH},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	result := Result{
		Created: true, Started: true, Completed: true, ExitCode: 0,
		Stdout: []byte("out"), Stderr: []byte("err"),
		SafeFacts: map[string]string{"certified": "false"}, ChildStartReceipt: &receipt,
	}
	clone := result.Clone()
	clone.Stdout[0] = 'X'
	clone.Stderr[0] = 'Y'
	clone.SafeFacts["certified"] = "true"
	clone.ChildStartReceipt.Challenge = strings.Repeat("f", 64)
	if string(result.Stdout) != "out" || string(result.Stderr) != "err" || result.SafeFacts["certified"] != "false" {
		t.Fatal("Result.Clone aliases caller-owned values")
	}
	if result.ChildStartReceipt.Challenge == clone.ChildStartReceipt.Challenge {
		t.Fatal("Result.Clone aliases child-start receipt")
	}
	originalReceipt := result.ChildStartReceipt
	result.Destroy()
	if result.ChildStartReceipt != nil || !reflect.DeepEqual(*originalReceipt, ChildStartReceipt{}) {
		t.Fatal("Result.Destroy retained private child-start receipt")
	}
}

func TestSecretDetectedResultDestroysAndNeverFormatsOutput(t *testing.T) {
	const stdoutSecret = "stdout-secret-sentinel"
	const stderrSecret = "stderr-secret-sentinel"
	result := Result{
		Created: true, Started: true, Completed: true, ExitCode: 7,
		Stdout: []byte(stdoutSecret), Stderr: []byte(stderrSecret), Duration: 3 * time.Second,
		StdoutTruncated: true, StderrTruncated: true, SecretDetected: true,
		SafeFacts: map[string]string{"runtime": "isolated"},
	}
	clone := result.Clone()
	clone.Stdout[0] = 'X'
	clone.Stderr[0] = 'Y'
	if string(result.Stdout) != stdoutSecret || string(result.Stderr) != stderrSecret {
		t.Fatal("Result.Clone aliases secret-detected output")
	}

	want := `{"created":true,"started":true,"completed":true,"exit_code":7,"duration":3000000000,"stdout_truncated":true,"stderr_truncated":true,"secret_detected":true,"safe_facts":{"runtime":"isolated"}}`
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != want {
		t.Fatalf("Marshal() = %s", encoded)
	}
	for format, expected := range map[string]string{
		"%v": want, "%+v": want, "%#v": want, "%s": want, "%q": fmt.Sprintf("%q", want),
	} {
		if got := fmt.Sprintf(format, result); got != expected {
			t.Fatalf("format %q = %q, want %q", format, got, expected)
		}
	}

	stdoutOwned := result.Stdout
	stderrOwned := result.Stderr
	result.Destroy()
	if result.Stdout != nil || result.Stderr != nil {
		t.Fatalf("Destroy() retained output: %#v", result)
	}
	if string(stdoutOwned) != strings.Repeat("\x00", len(stdoutSecret)) ||
		string(stderrOwned) != strings.Repeat("\x00", len(stderrSecret)) {
		t.Fatal("Destroy() did not zero owned output")
	}
}

func TestProviderErrorSerializesOnlyStableDiagnostics(t *testing.T) {
	providerDetail := errors.New("container id secret-engine-detail")
	err := WrapError("environment", "create", providerDetail)
	if !errors.Is(err, providerDetail) {
		t.Fatal("provider error chain was not preserved")
	}
	encoded, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), providerDetail.Error()) || string(encoded) != `{"class":"environment","cause_code":"create"}` {
		t.Fatalf("serialized provider error leaked detail: %s", encoded)
	}
	want := "executor environment failure (create)"
	for format, expected := range map[string]string{
		"%v": want, "%+v": want, "%#v": want, "%s": want, "%q": fmt.Sprintf("%q", want),
	} {
		got := fmt.Sprintf(format, err)
		if got != expected || strings.Contains(got, providerDetail.Error()) {
			t.Fatalf("format %q = %q, want opaque %q", format, got, expected)
		}
	}
}

func TestRegistrySelectsExactRuntimeAndRejectsDuplicates(t *testing.T) {
	host := &stubExecutor{}
	registry, err := NewRegistry(
		Registration{RuntimeKind: workerprofile.RuntimeHost, Executor: host},
		Registration{RuntimeKind: workerprofile.RuntimeOCI, Executor: &stubExecutor{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	profile := validHostProfile(t)
	got, err := registry.Resolve(profile)
	if err != nil || got != host {
		t.Fatalf("Resolve() = %#v, %v", got, err)
	}

	if _, err := NewRegistry(
		Registration{RuntimeKind: workerprofile.RuntimeHost, Executor: host},
		Registration{RuntimeKind: workerprofile.RuntimeHost, Executor: &stubExecutor{}},
	); err == nil {
		t.Fatal("duplicate runtime registration accepted")
	}

	corrupt := profile.Clone()
	corrupt.Digest = strings.Repeat("0", 64)
	if _, err := registry.Resolve(corrupt); err == nil {
		t.Fatal("registry accepted corrupt profile snapshot")
	}
}

func TestRegistryRejectsEveryTypedNilExecutorKind(t *testing.T) {
	tests := map[string]Executor{
		"pointer":  (*stubExecutor)(nil),
		"map":      mapExecutor(nil),
		"slice":    sliceExecutor(nil),
		"function": functionExecutor(nil),
	}
	for name, target := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRegistry(Registration{RuntimeKind: workerprofile.RuntimeHost, Executor: target}); err == nil {
				t.Fatal("typed-nil executor registration accepted")
			}
		})
	}
}

type stubExecutor struct{}

func (*stubExecutor) Execute(_ context.Context, _ Request) (Result, error)  { return Result{}, nil }
func (*stubExecutor) Inspect(_ context.Context, _ AttemptID) (State, error) { return StateAbsent, nil }
func (*stubExecutor) Cancel(_ context.Context, _ AttemptID) error           { return nil }
func (*stubExecutor) Destroy(_ context.Context, _ AttemptID) error          { return nil }

type mapExecutor map[string]string

func (mapExecutor) Execute(context.Context, Request) (Result, error)  { return Result{}, nil }
func (mapExecutor) Inspect(context.Context, AttemptID) (State, error) { return StateAbsent, nil }
func (mapExecutor) Cancel(context.Context, AttemptID) error           { return nil }
func (mapExecutor) Destroy(context.Context, AttemptID) error          { return nil }

type sliceExecutor []string

func (sliceExecutor) Execute(context.Context, Request) (Result, error)  { return Result{}, nil }
func (sliceExecutor) Inspect(context.Context, AttemptID) (State, error) { return StateAbsent, nil }
func (sliceExecutor) Cancel(context.Context, AttemptID) error           { return nil }
func (sliceExecutor) Destroy(context.Context, AttemptID) error          { return nil }

type functionExecutor func()

func (functionExecutor) Execute(context.Context, Request) (Result, error)  { return Result{}, nil }
func (functionExecutor) Inspect(context.Context, AttemptID) (State, error) { return StateAbsent, nil }
func (functionExecutor) Cancel(context.Context, AttemptID) error           { return nil }
func (functionExecutor) Destroy(context.Context, AttemptID) error          { return nil }

func validAttemptID() AttemptID {
	return AttemptID{
		RunID: "run-1", Stage: "execute", Attempt: 2,
		StartedAt: time.Unix(100, 7).UTC(), Purpose: PurposeVerification, Sequence: 3,
	}
}

func validHostRequest(t *testing.T) Request {
	t.Helper()
	return Request{
		Attempt: AttemptID{
			RunID: "run-1", Stage: "execute", Attempt: 1,
			StartedAt: time.Unix(100, 7).UTC(), Purpose: PurposeAgent,
		},
		Profile: validHostProfile(t),
		Command: Command{
			Executable: "go", Args: []string{"version"}, Directory: "/workspace",
			Environment: map[string]string{"GOWORK": "off"},
		},
		Workspace:   Workspace{HostPath: t.TempDir(), SandboxPath: "/workspace", Writable: true},
		Environment: map[string]string{"PATH": CanonicalSandboxPATH},
		Timeout:     time.Minute,
		OutputLimit: 1 << 20,
	}
}

func validOCIRequest(t *testing.T) Request {
	t.Helper()
	request := validHostRequest(t)
	profile := workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "codex-go", Revision: 1},
		Runtime: workerprofile.Runtime{
			Kind: workerprofile.RuntimeOCI, Image: "example.invalid/worker@sha256:" + strings.Repeat("a", 64),
			Platform: "linux/amd64", Network: workerprofile.NetworkNone, ReadOnlyRoot: true,
		},
		Resources: workerprofile.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 64},
		Harness:   workerprofile.Harness{ID: "codex", Version: "0.144.5"},
		Tools: []workerprofile.Tool{{
			Name: "go", Version: "1.26.1",
			Probe: workerprofile.Probe{Executable: "go", Args: []string{"version"}, OutputContains: "go1.26.1"},
		}},
		Secrets: []workerprofile.SecretRequirement{{
			Capability: "harness.codex-auth", BindingRevision: 1, Stage: workerprofile.StageAgent,
			Delivery: workerprofile.DeliveryFile, Target: "/run/paje/secrets/codex-auth.json", Required: true,
		}},
	}
	canonical, err := workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	materialization, err := secret.NewValueMaterialization(
		workerprofile.DeliveryFile, "/run/paje/secrets/codex-auth.json", []byte("top-secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Profile = canonical
	request.Secrets = []secret.Materialization{materialization}
	return request
}

func validEnvironmentSecretRequest(t *testing.T) Request {
	t.Helper()
	request := validOCIRequest(t)
	request.Destroy()
	request.Profile.Secrets[0].Delivery = workerprofile.DeliveryEnvironment
	request.Profile.Secrets[0].Target = "WORKLOAD_TOKEN"
	request.Profile.Digest = ""
	profile, err := workerprofile.Canonicalize(request.Profile)
	if err != nil {
		t.Fatal(err)
	}
	materialization, err := secret.NewValueMaterialization(
		workerprofile.DeliveryEnvironment, "WORKLOAD_TOKEN", []byte("top-secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Profile = profile
	request.Secrets = []secret.Materialization{materialization}
	return request
}

func validHostProfile(t *testing.T) workerprofile.Snapshot {
	t.Helper()
	profile, err := workerprofile.Canonicalize(workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "host-dev", Revision: 1},
		Runtime:    workerprofile.Runtime{Kind: workerprofile.RuntimeHost},
		Harness:    workerprofile.Harness{ID: "codex", Version: "0.144.5"},
		Tools: []workerprofile.Tool{{
			Name: "go", Version: "1.26.1",
			Probe: workerprofile.Probe{Executable: "go", Args: []string{"version"}, OutputContains: "go1.26.1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestStateValidationCoversStableLifecycle(t *testing.T) {
	want := []State{
		StateAbsent,
		StateResourceCreated,
		StateBootstrapStarted,
		StateChildStarted,
		StateTerminalComplete,
		StateDestroyed,
		StateUnknown,
	}
	for _, state := range want {
		if err := state.Validate(); err != nil {
			t.Fatalf("state %q rejected: %v", state, err)
		}
	}
	if err := State("provider-specific").Validate(); err == nil {
		t.Fatal("provider-specific lifecycle state accepted")
	}
	if !reflect.DeepEqual(want, StableStates()) {
		t.Fatalf("StableStates() = %#v", StableStates())
	}
}

func TestChildStartReceiptBindsAttemptCommandAndEnvironmentDeclaration(t *testing.T) {
	attempt := validAttemptID()
	command := Command{
		Executable: "codex",
		Args:       []string{"exec", "change the repository"},
		Directory:  SandboxWorkspaceRoot,
		Environment: map[string]string{
			"GOWORK": "off",
		},
	}
	environment := map[string]string{
		"HOME": "/home/paje",
		"PATH": CanonicalSandboxPATH,
	}
	environmentFiles := map[string]string{
		"CODEX_TOKEN": "/run/paje/secrets/environment/token",
	}
	challenge := strings.Repeat("a", 64)

	receipt, err := NewChildStartReceipt(attempt, command, environment, environmentFiles, challenge)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("receipt validation failed: %v", err)
	}
	if receipt.Attempt != attempt || receipt.Command.Executable != "codex" ||
		!reflect.DeepEqual(receipt.Command.Args, command.Args) ||
		receipt.Command.Directory != SandboxWorkspaceRoot ||
		receipt.CommandDigest == "" || receipt.EnvironmentDigest == "" ||
		receipt.Challenge != challenge || receipt.BindingDigest() == "" {
		t.Fatalf("receipt = %#v", receipt)
	}

	tests := map[string]func(*AttemptID, *Command, map[string]string, map[string]string, *string){
		"attempt": func(attempt *AttemptID, _ *Command, _ map[string]string, _ map[string]string, _ *string) {
			attempt.Sequence++
		},
		"executable": func(_ *AttemptID, command *Command, _ map[string]string, _ map[string]string, _ *string) {
			command.Executable = "go"
		},
		"argument": func(_ *AttemptID, command *Command, _ map[string]string, _ map[string]string, _ *string) {
			command.Args[1] = "different"
		},
		"baseline environment": func(_ *AttemptID, _ *Command, environment map[string]string, _ map[string]string, _ *string) {
			environment["HOME"] = "/different"
		},
		"command environment": func(_ *AttemptID, command *Command, _ map[string]string, _ map[string]string, _ *string) {
			command.Environment["GOWORK"] = "on"
		},
		"environment materialization": func(_ *AttemptID, _ *Command, _ map[string]string, environmentFiles map[string]string, _ *string) {
			environmentFiles["CODEX_TOKEN"] = "/run/paje/secrets/environment/other"
		},
		"challenge": func(_ *AttemptID, _ *Command, _ map[string]string, _ map[string]string, challenge *string) {
			*challenge = strings.Repeat("b", 64)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			otherAttempt := attempt
			otherCommand := command.Clone()
			otherEnvironment := cloneMap(environment)
			otherEnvironmentFiles := cloneMap(environmentFiles)
			otherChallenge := challenge
			mutate(&otherAttempt, &otherCommand, otherEnvironment, otherEnvironmentFiles, &otherChallenge)
			other, err := NewChildStartReceipt(
				otherAttempt,
				otherCommand,
				otherEnvironment,
				otherEnvironmentFiles,
				otherChallenge,
			)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Matches(other) {
				t.Fatalf("receipt matched rebound %s declaration", name)
			}
		})
	}
}

func TestRequestDeclarationRejectsEnvironmentSecretCollisionBeforeAcquire(t *testing.T) {
	request := validEnvironmentSecretRequest(t)
	request.Destroy()
	request.Environment["WORKLOAD_TOKEN"] = "non-secret-caller-value"

	if err := request.ValidateDeclaration(); err == nil {
		t.Fatal("pre-acquire declaration accepted environment materialization collision")
	}
}

func TestRequestDeclarationRequiresCanonicalSandboxPATHBeforeAcquire(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"missing": func(request *Request) {
			delete(request.Environment, "PATH")
		},
		"arbitrary": func(request *Request) {
			request.Environment["PATH"] = "/tmp/paje-untrusted-bin"
		},
		"command override": func(request *Request) {
			request.Command.Environment["PATH"] = CanonicalSandboxPATH
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := validHostRequest(t)
			defer request.Destroy()
			mutate(&request)
			if err := request.ValidateDeclaration(); err == nil {
				t.Fatal("pre-acquire declaration accepted noncanonical PATH")
			}
		})
	}
}
