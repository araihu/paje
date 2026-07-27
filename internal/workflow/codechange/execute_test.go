package codechange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/artifact/gitcapture"
	artifactmock "github.com/araihu/paje/internal/artifact/mock"
	"github.com/araihu/paje/internal/environment"
	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/harness"
	harnesscodex "github.com/araihu/paje/internal/harness/codex"
	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/policy"
	"github.com/araihu/paje/internal/publisher"
	publishermock "github.com/araihu/paje/internal/publisher/mock"
	"github.com/araihu/paje/internal/repository"
	"github.com/araihu/paje/internal/run"
	runmock "github.com/araihu/paje/internal/run/mock"
	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workerprofile"
	"github.com/araihu/paje/internal/workspace"
	"github.com/araihu/paje/internal/workspace/gitworktree"
)

func TestExecuteUsesDistinctSecretFreeAndAgentSandboxes(t *testing.T) {
	fixture := newServiceFixture(t)
	profile := resolvedWorkerProfile(t)
	profile.Tools = []workerprofile.Tool{{
		Name: "git", Version: "2.53.0",
		Probe: workerprofile.Probe{
			Executable: "git", Args: []string{"--version"}, OutputContains: "2.53.0",
		},
	}}
	profile.Digest = ""
	var err error
	profile, err = workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	fixture.workerProfiles.Set(profile)
	fixture.service.profiles["generic"] = &sandboxBackedProfile{}
	type sandboxObservation struct {
		purpose  executor.Purpose
		secrets  int
		writable bool
	}
	var observations []sandboxObservation
	fixture.executor.SetBeforeExecute(func(_ context.Context, request executor.Request) {
		observations = append(observations, sandboxObservation{
			purpose: request.Attempt.Purpose, secrets: len(request.Secrets), writable: request.Workspace.Writable,
		})
		stdout := []byte("ok")
		switch request.Command.Executable {
		case "codex":
			if request.Attempt.Purpose == executor.PurposeProbe {
				stdout = []byte("codex 0.144.5")
			} else {
				stdout = []byte("agent completed")
			}
		case "git":
			if request.Attempt.Sequence == 1 {
				stdout = []byte("git version 2.53.0")
			}
		}
		fixture.executor.SetResult(request.Attempt, executor.Result{
			Created: true, Started: true, Completed: true, Stdout: stdout,
			SafeFacts: portableSafeFacts(request.Profile),
		}, nil)
	})
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}
	if _, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "isolated")); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Artifact == nil {
		t.Fatal("Execute() artifact = nil")
	}
	requests := fixture.executor.Requests()
	wantPurposes := []executor.Purpose{
		executor.PurposeProbe,
		executor.PurposeProbe,
		executor.PurposeProbe,
		executor.PurposeProbe,
		executor.PurposeAgent,
		executor.PurposeVerification,
	}
	if len(requests) != len(wantPurposes) {
		t.Fatalf("executor requests = %d, want %d: %#v", len(requests), len(wantPurposes), requests)
	}
	seen := make(map[string]struct{}, len(requests))
	for index, request := range requests {
		if request.Attempt.Purpose != wantPurposes[index] {
			t.Fatalf("request %d purpose = %q, want %q", index, request.Attempt.Purpose, wantPurposes[index])
		}
		if request.Attempt.RunID != "run-123" || request.Attempt.Stage != "execute" ||
			request.Attempt.Attempt != 1 || !request.Attempt.StartedAt.Equal(time.Unix(100, 0).UTC()) {
			t.Fatalf("request %d attempt = %#v", index, request.Attempt)
		}
		if _, duplicate := seen[request.Attempt.Key()]; duplicate {
			t.Fatalf("duplicate deterministic attempt identity: %#v", request.Attempt)
		}
		seen[request.Attempt.Key()] = struct{}{}
		observation := observations[index]
		if observation.purpose == executor.PurposeAgent {
			if observation.secrets != 1 || !observation.writable {
				t.Fatalf("agent request secrets=%d writable=%t", observation.secrets, observation.writable)
			}
		} else if observation.secrets != 0 || observation.writable {
			t.Fatalf("%s request received secrets or writable workspace", request.Attempt.Purpose)
		}
	}
	acquisitions := fixture.secrets.Requests()
	if len(acquisitions) != 1 || acquisitions[0].Capability != "harness.codex-auth" ||
		acquisitions[0].Binding != 7 || acquisitions[0].Attempt != 1 ||
		!acquisitions[0].StartedAt.Equal(time.Unix(100, 0).UTC()) {
		t.Fatalf("secret acquisitions = %#v", acquisitions)
	}
	if got := fixture.secrets.Revocations(); !reflect.DeepEqual(got, []string{"fixture-codex-lease"}) {
		t.Fatalf("secret revocations = %#v", got)
	}
}

func TestExecuteBindsCodexHomeAndDerivesSkippedVerificationEvidence(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err != nil || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	var agentRequest executor.Request
	for _, request := range fixture.executor.Requests() {
		if request.Attempt.Purpose == executor.PurposeAgent {
			agentRequest = request
			break
		}
	}
	if got := agentRequest.Command.Environment["CODEX_HOME"]; got != "/run/paje/secrets/codex" {
		t.Fatalf("agent CODEX_HOME = %q", got)
	}
	evidence := loadExecutionEvidence(t, fixture, *result.Artifact)
	if evidence.AgentEnvironmentKeys == nil || !reflect.DeepEqual(
		[]string(*evidence.AgentEnvironmentKeys),
		[]string{"CODEX_HOME", "HOME", "PATH", "TMPDIR"},
	) {
		t.Fatalf("agent environment keys = %#v", evidence.AgentEnvironmentKeys)
	}
	if evidence.VerificationEnvironmentKeys == nil || len(*evidence.VerificationEnvironmentKeys) != 0 {
		t.Fatalf("skipped verification environment keys = %#v", evidence.VerificationEnvironmentKeys)
	}
	for _, attempt := range *evidence.Attempts {
		if attempt.ID.Purpose == executor.PurposeVerification {
			t.Fatalf("skipped verification recorded attempt = %#v", attempt)
		}
	}
	record, loadErr := fixture.runs.Load(context.Background(), "run-123")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	stage, _ := latestStage(record, "execute")
	if stage.Evidence["agent_attempt_started"] != "true" ||
		stage.Evidence["verification_attempt_started"] != "false" ||
		stage.Evidence["verification_environment_keys"] != "[]" {
		t.Fatalf("durable attempt/environment evidence = %#v", stage.Evidence)
	}
}

func TestExecuteRejectsAgentEnvironmentCollisionBeforeAcquireOrExecutor(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.harness.agentEnvironment = func([]workerprofile.SecretRequirement) (map[string]string, error) {
		return map[string]string{"PATH": "/untrusted/bin"}, nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureEnvironment || result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if got := len(fixture.secrets.Requests()); got != 0 {
		t.Fatalf("secret acquisitions = %d, want 0", got)
	}
	if got := len(fixture.executor.Requests()); got != 0 {
		t.Fatalf("executor requests = %d, want 0", got)
	}
}

func TestExecuteProtectsPersistedSecretRequirementsFromAdapterMutation(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.harness.agentEnvironment = func(requirements []workerprofile.SecretRequirement) (map[string]string, error) {
		requirements[0].Target = "/run/paje/secrets/mutated"
		return map[string]string{"CODEX_HOME": "/run/paje/secrets/codex"}, nil
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	if _, err := fixture.service.Execute(context.Background(), "run-123"); err != nil {
		t.Fatal(err)
	}
	record, err := fixture.runs.Load(context.Background(), "run-123")
	if err != nil {
		t.Fatal(err)
	}
	if fixture.harness.agentEnvironmentCalls != 1 || record.WorkerProfile.Secrets[0].Target != "/run/paje/secrets/codex" {
		t.Fatalf("adapter calls=%d persisted requirements=%#v", fixture.harness.agentEnvironmentCalls, record.WorkerProfile.Secrets)
	}
}

func TestExecuteDoesNotClaimAgentEnvironmentBeforeConfirmedChildStart(t *testing.T) {
	tests := map[string]func(*serviceFixture){
		"acquisition failure": func(fixture *serviceFixture) {
			fixture.secrets.SetAcquireResult("harness.codex-auth", secret.Lease{}, errors.New("source unavailable"))
		},
		"bootstrap only": func(fixture *serviceFixture) {
			fixture.executor.SetBeforeExecute(func(_ context.Context, request executor.Request) {
				result := executor.Result{Created: true, Started: true, Completed: true, SafeFacts: portableSafeFacts(request.Profile)}
				if request.Attempt.Purpose == executor.PurposeProbe {
					result.Stdout = []byte("codex 0.144.5")
				}
				if request.Attempt.Purpose == executor.PurposeAgent {
					result = executor.Result{Created: true, BootstrapStarted: true}
				}
				fixture.executor.SetResult(request.Attempt, result, nil)
			})
		},
		"ambiguous receipt": func(fixture *serviceFixture) {
			fixture.executor.SetBeforeExecute(func(_ context.Context, request executor.Request) {
				result := executor.Result{Created: true, Started: true, Completed: true, SafeFacts: portableSafeFacts(request.Profile)}
				if request.Attempt.Purpose == executor.PurposeProbe {
					result.Stdout = []byte("codex 0.144.5")
				}
				if request.Attempt.Purpose == executor.PurposeAgent {
					rebound := request.Attempt
					rebound.Sequence++
					receipt, err := executor.NewRandomChildStartReceipt(rebound, request.Command, request.Environment, nil)
					if err != nil {
						t.Fatal(err)
					}
					result.ChildStartReceipt = &receipt
				}
				fixture.executor.SetResult(request.Attempt, result, nil)
			})
		},
		"missing receipt": func(fixture *serviceFixture) {
			target := &receiptStrippingExecutor{
				Executor: fixture.executor, purpose: executor.PurposeAgent,
			}
			registry, err := executor.NewRegistry(executor.Registration{
				RuntimeKind: workerprofile.RuntimeOCI, Executor: target,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.service.executors = registry
		},
	}
	for name, configure := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			configure(fixture)
			result, err := fixture.service.Execute(context.Background(), "run-123")
			if err == nil || result.Artifact != nil {
				t.Fatalf("Execute() result=%#v error=%v", result, err)
			}
			record, loadErr := fixture.runs.Load(context.Background(), "run-123")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			stage, _ := latestStage(record, "execute")
			if _, claimed := stage.Evidence["agent_environment_keys"]; claimed ||
				stage.Evidence["agent_attempt_started"] != "false" {
				t.Fatalf("unconfirmed agent claimed presentation = %#v", stage.Evidence)
			}
		})
	}
}

func TestExecuteRejectsReceiptEnvironmentDriftBeforeClaimingPresentedKeys(t *testing.T) {
	tests := map[string]struct {
		purpose   executor.Purpose
		configure func(*testing.T, *serviceFixture)
		build     func(executor.Request) (map[string]string, map[string]string)
	}{
		"agent omitted environment materialization": {
			purpose: executor.PurposeAgent,
			configure: func(t *testing.T, fixture *serviceFixture) {
				configureSecondAgentSecret(t, fixture, "receipt-bound-secret")
			},
			build: func(request executor.Request) (map[string]string, map[string]string) {
				return cloneStringMap(request.Environment), nil
			},
		},
		"agent changed environment materialization": {
			purpose: executor.PurposeAgent,
			configure: func(t *testing.T, fixture *serviceFixture) {
				configureSecondAgentSecret(t, fixture, "receipt-bound-secret")
			},
			build: func(request executor.Request) (map[string]string, map[string]string) {
				files := receiptEnvironmentFiles(request)
				files["WORKLOAD_TOKEN"] += "-drift"
				return cloneStringMap(request.Environment), files
			},
		},
		"verification changed baseline environment": {
			purpose: executor.PurposeVerification,
			configure: func(_ *testing.T, fixture *serviceFixture) {
				fixture.profile.result.Commands = []verification.Command{{
					Name: "required", Directory: "/tmp/workspace", Executable: "git",
					Args: []string{"status"}, Timeout: time.Minute, Required: true,
				}}
			},
			build: func(request executor.Request) (map[string]string, map[string]string) {
				environment := cloneStringMap(request.Environment)
				delete(environment, "TMPDIR")
				return environment, nil
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			test.configure(t, fixture)
			target := &receiptEnvironmentDriftExecutor{
				Executor: fixture.executor, purpose: test.purpose, build: test.build,
			}
			registry, err := executor.NewRegistry(executor.Registration{
				RuntimeKind: workerprofile.RuntimeOCI, Executor: target,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.service.executors = registry
			fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
				return capturedChange(), nil
			}
			if _, err := fixture.service.Resolve(
				context.Background(), validRawInput("Change docs", "receipt-environment-drift"),
			); err != nil {
				t.Fatal(err)
			}

			result, err := fixture.service.Execute(context.Background(), "run-123")
			if err == nil || test.purpose == executor.PurposeAgent && result.Artifact != nil {
				t.Fatalf("Execute() result=%#v error=%v", result, err)
			}
			record, loadErr := fixture.runs.Load(context.Background(), "run-123")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			stage, _ := latestStage(record, "execute")
			key := "agent_environment_keys"
			startedKey := "agent_attempt_started"
			if test.purpose == executor.PurposeVerification {
				key = "verification_environment_keys"
				startedKey = "verification_attempt_started"
			}
			claimed := stage.Evidence[key]
			if test.purpose == executor.PurposeAgent {
				if _, exists := stage.Evidence[key]; exists {
					claimed = "unexpected"
				}
			} else if claimed == "[]" {
				claimed = ""
			}
			if claimed != "" || stage.Evidence[startedKey] != "false" {
				t.Fatalf("drifted receipt claimed presentation: %#v", stage.Evidence)
			}
			if test.purpose == executor.PurposeVerification && result.Artifact != nil {
				bundle := fixture.artifacts.Snapshot().Bundles[result.Artifact.Digest]
				if len(bundle.Verification) != 1 ||
					bundle.Verification[0].Command.EnvironmentKeys != nil {
					t.Fatalf("drifted receipt persisted declaration: %#v", bundle.Verification)
				}
			}
		})
	}
}

func TestExecutePersistsReceiptBackedVerificationEnvironmentDeclaration(t *testing.T) {
	tests := map[string]struct {
		command         verification.Command
		wantDeclaration []string
		wantUnion       []string
	}{
		"generic go without GOWORK": {
			command: verification.Command{
				Name: "required", Directory: "/tmp/workspace", Executable: "go",
				Args: []string{"version"}, Timeout: time.Minute, Required: true,
			},
			wantDeclaration: []string{},
			wantUnion:       []string{"HOME", "PATH", "TMPDIR"},
		},
		"Go profile GOWORK": {
			command: verification.Command{
				Name: "required", Directory: "/tmp/workspace", Executable: "go",
				Args: []string{"test", "./..."}, Environment: map[string]string{"GOWORK": "off"},
				Timeout: time.Minute, Required: true,
			},
			wantDeclaration: []string{"GOWORK"},
			wantUnion:       []string{"GOWORK", "HOME", "PATH", "TMPDIR"},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			fixture.profile.result.Commands = []verification.Command{test.command}
			fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
				return capturedChange(), nil
			}

			result, err := fixture.service.Execute(context.Background(), "run-123")
			if err != nil || result.Artifact == nil {
				t.Fatalf("Execute() result=%#v error=%v", result, err)
			}
			bundle := fixture.artifacts.Snapshot().Bundles[result.Artifact.Digest]
			if len(bundle.Verification) != 1 || bundle.Verification[0].Command.Environment != nil ||
				!reflect.DeepEqual(bundle.Verification[0].Command.EnvironmentKeys, test.wantDeclaration) {
				t.Fatalf("verification declaration = %#v", bundle.Verification)
			}
			evidence := loadExecutionEvidence(t, fixture, *result.Artifact)
			if evidence.VerificationEnvironmentKeys == nil || !reflect.DeepEqual(
				[]string(*evidence.VerificationEnvironmentKeys), test.wantUnion,
			) {
				t.Fatalf("verification environment keys = %#v", evidence.VerificationEnvironmentKeys)
			}
			confirmed := 0
			for _, attempt := range *evidence.Attempts {
				if attempt.ID.Purpose == executor.PurposeVerification && attempt.Started {
					confirmed++
				}
			}
			if confirmed != 1 {
				t.Fatalf("confirmed verification attempts = %d", confirmed)
			}
			record, _ := fixture.runs.Load(context.Background(), "run-123")
			stage, _ := latestStage(record, "execute")
			if stage.Evidence["verification_attempt_started"] != "true" {
				t.Fatalf("durable verification state = %#v", stage.Evidence)
			}
		})
	}
}

func TestPublisherPortableEnvironmentEvidenceTruthTable(t *testing.T) {
	request := portablePublisherRequest(t)
	verificationAttempt := artifact.AttemptEvidence{
		ID: executor.AttemptID{
			RunID: request.RunID, Stage: "execute", Attempt: 1,
			StartedAt: time.Unix(100, 0).UTC(), Purpose: executor.PurposeVerification, Sequence: 1,
		},
		Created: true, Started: true, Completed: true, Destroyed: true,
	}
	baseline := artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"}
	empty := artifact.EnvironmentKeyList{}
	tests := map[string]struct {
		scheduled bool
		mutate    func(*publisher.Request)
		valid     bool
	}{
		"verification skipped": {mutate: func(request *publisher.Request) {
			request.ExecutionEvidence.VerificationEnvironmentKeys = &empty
		}, valid: true},
		"confirmed empty union rejected": {scheduled: true, mutate: func(request *publisher.Request) {
			*request.ExecutionEvidence.Attempts = append(*request.ExecutionEvidence.Attempts, verificationAttempt)
			request.ExecutionEvidence.VerificationEnvironmentKeys = &empty
		}},
		"confirmed nonempty": {scheduled: true, mutate: func(request *publisher.Request) {
			*request.ExecutionEvidence.Attempts = append(*request.ExecutionEvidence.Attempts, verificationAttempt)
			request.ExecutionEvidence.VerificationEnvironmentKeys = &baseline
		}, valid: true},
		"partial": {scheduled: true, mutate: func(request *publisher.Request) {
			*request.ExecutionEvidence.Attempts = append(*request.ExecutionEvidence.Attempts, verificationAttempt)
			keys := artifact.EnvironmentKeyList{"HOME", "PATH"}
			request.ExecutionEvidence.VerificationEnvironmentKeys = &keys
		}},
		"extra": {scheduled: true, mutate: func(request *publisher.Request) {
			*request.ExecutionEvidence.Attempts = append(*request.ExecutionEvidence.Attempts, verificationAttempt)
			keys := artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR", "TOKEN"}
			request.ExecutionEvidence.VerificationEnvironmentKeys = &keys
		}},
		"inverse": {scheduled: true, mutate: func(request *publisher.Request) {
			request.ExecutionEvidence.VerificationEnvironmentKeys = &baseline
		}},
		"drift": {scheduled: true, mutate: func(request *publisher.Request) {
			drifted := verificationAttempt
			drifted.ID.Attempt++
			*request.ExecutionEvidence.Attempts = append(*request.ExecutionEvidence.Attempts, drifted)
			request.ExecutionEvidence.VerificationEnvironmentKeys = &baseline
		}},
		"missing": {scheduled: true, mutate: func(request *publisher.Request) {
			*request.ExecutionEvidence.Attempts = append(*request.ExecutionEvidence.Attempts, verificationAttempt)
			request.ExecutionEvidence.VerificationEnvironmentKeys = nil
		}},
		"unconfirmed attempt": {scheduled: true, mutate: func(request *publisher.Request) {
			unconfirmed := verificationAttempt
			unconfirmed.Started = false
			unconfirmed.Completed = false
			*request.ExecutionEvidence.Attempts = append(*request.ExecutionEvidence.Attempts, unconfirmed)
			request.ExecutionEvidence.VerificationEnvironmentKeys = &empty
		}},
		"agent missing": {mutate: func(request *publisher.Request) {
			request.ExecutionEvidence.AgentEnvironmentKeys = nil
		}},
		"agent partial": {mutate: func(request *publisher.Request) {
			keys := artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"}
			request.ExecutionEvidence.AgentEnvironmentKeys = &keys
		}},
		"agent inverse": {mutate: func(request *publisher.Request) {
			attempts := *request.ExecutionEvidence.Attempts
			attempts[0].Started = false
			attempts[0].Completed = false
			request.ExecutionEvidence.Started = false
			request.ExecutionEvidence.Completed = false
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := publisher.CloneRequest(request)
			if test.scheduled {
				schedulePortableVerification(t, &candidate, "git")
			}
			test.mutate(&candidate)
			err := candidate.ValidatePortable()
			if test.valid && err != nil {
				t.Fatalf("ValidatePortable() error = %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("ValidatePortable() error = nil")
			}
		})
	}
}

func loadExecutionEvidence(t *testing.T, fixture *serviceFixture, reference artifact.Reference) artifact.ExecutionEvidence {
	t.Helper()
	bundle := fixture.artifacts.Snapshot().Bundles[reference.Digest]
	evidence, err := publisher.DecodeExecutionEvidence(bundle.ExecutionMetadata)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func portablePublisherRequest(t *testing.T) publisher.Request {
	t.Helper()
	profile := resolvedWorkerProfile(t)
	tools := artifact.ToolEvidenceList{}
	attempts := artifact.AttemptEvidenceList{{
		ID: executor.AttemptID{
			RunID: "run-123", Stage: "execute", Attempt: 1,
			StartedAt: time.Unix(100, 0).UTC(), Purpose: executor.PurposeAgent,
		},
		Created: true, Started: true, Completed: true, Destroyed: true,
	}}
	agentKeys := artifact.EnvironmentKeyList{"CODEX_HOME", "HOME", "PATH", "TMPDIR"}
	empty := artifact.EnvironmentKeyList{}
	plan := []verification.Result{}
	manifest := artifact.Manifest{
		RunID: "run-123", Template: template.ID{Name: "code-change", Version: 1},
		Repository: "https://example.test/repository.git",
		BaseSHA:    strings.Repeat("a", 40), TreeSHA: strings.Repeat("c", 40),
		Changes: []artifact.Change{{Path: "changed.txt", Status: "M"}},
	}
	manifest.Members = []artifact.Member{portableVerificationMember(t, manifest, plan)}
	return publisher.Request{
		RunID: "run-123", Repository: "https://example.test/repository.git",
		BaseSHA: strings.Repeat("a", 40), TargetRef: "refs/heads/main",
		Branch:           "paje/code-change/run-123",
		Artifact:         artifact.Reference{RunID: "run-123", Digest: strings.Repeat("b", 64), Size: 1},
		ArtifactManifest: manifest,
		WorkerProfile:    profile,
		ExecutionEvidence: artifact.ExecutionEvidence{
			Started: true, Completed: true,
			Profile: &artifact.WorkerProfileEvidence{
				Name: profile.Metadata.Name, Revision: profile.Metadata.Revision, Digest: profile.Digest,
			},
			Runtime: &artifact.RuntimeEvidence{
				Kind: profile.Runtime.Kind, ImageDigest: strings.Repeat("a", 64),
				Platform: profile.Runtime.Platform, Isolated: true, Certified: true,
			},
			Harness: &artifact.HarnessEvidence{
				ID: profile.Harness.ID, DeclaredVersion: profile.Harness.Version,
				ProbedVersion: profile.Harness.Version, ProbePassed: true,
			},
			Tools: &tools, Attempts: &attempts,
			AgentEnvironmentKeys: &agentKeys, VerificationEnvironmentKeys: &empty,
		},
		Verification: plan, Title: "Portable change",
	}
}

func schedulePortableVerification(t *testing.T, request *publisher.Request, executable string) {
	t.Helper()
	request.Verification = []verification.Result{{
		Command: verification.Command{
			Name: executable + " verification", Directory: ".", Executable: executable,
			Args: []string{"status"}, EnvironmentKeys: []string{}, Timeout: time.Minute, Required: true,
		},
		Passed: true,
	}}
	request.ArtifactManifest.Members = []artifact.Member{
		portableVerificationMember(t, request.ArtifactManifest, request.Verification),
	}
}

func portableVerificationMember(
	t *testing.T,
	manifest artifact.Manifest,
	plan []verification.Result,
) artifact.Member {
	t.Helper()
	manifest.Members = nil
	normalized, _, _, err := artifact.Canonicalize(artifact.Bundle{
		Manifest:     manifest,
		ChangesPatch: []byte("frozen verification plan binding"),
		ExecutionMetadata: json.RawMessage(
			`{"completed":false,"duration":0,"exit_code":0,"started":false,"truncated":false}`,
		),
		Verification: plan,
		Preflight:    map[string]string{},
		Warnings:     []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range normalized.Manifest.Members {
		if member.Name == "verification.json" {
			return member
		}
	}
	t.Fatal("verification plan member is missing")
	return artifact.Member{}
}

func TestExecuteDestroysAgentBeforeReverseLeaseRevocation(t *testing.T) {
	fixture := newServiceFixture(t)
	profile := resolvedWorkerProfile(t)
	profile.Secrets = append(profile.Secrets, workerprofile.SecretRequirement{
		Capability: "workload.api", BindingRevision: 9, Stage: workerprofile.StageAgent,
		Delivery: workerprofile.DeliveryEnvironment, Target: "WORKLOAD_TOKEN", Required: true,
	})
	profile.Digest = ""
	var err error
	profile, err = workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	fixture.workerProfiles.Set(profile)
	secondRef := secret.BindingRef{Capability: "workload.api", Revision: 9}
	secondBinding, err := secret.NewBinding(secondRef, secret.Authorization{
		ProfileID: profile.Metadata, Stage: workerprofile.StageAgent,
		Delivery: workerprofile.DeliveryEnvironment, Target: "WORKLOAD_TOKEN",
	}, "environment", "WORKLOAD_API_SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.secretBindings = &multiSecretRegistry{bindings: map[secret.BindingRef]secret.Binding{
		fixture.secretBindings.binding.Ref(): fixture.secretBindings.binding,
		secondRef:                            secondBinding,
	}}
	secondMaterialization, err := secret.NewValueMaterialization(
		workerprofile.DeliveryEnvironment, "WORKLOAD_TOKEN", []byte("second-secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	secondLease, err := secret.NewLease(
		"fixture-workload-lease", time.Unix(100, 0).UTC().Add(time.Hour), secondMaterialization,
	)
	secondMaterialization.Destroy()
	if err != nil {
		t.Fatal(err)
	}
	fixture.secrets.SetAcquireResult("workload.api", secondLease, nil)
	secondLease.Destroy()

	var events []string
	target := &eventExecutor{Executor: fixture.executor, events: &events}
	registry, err := executor.NewRegistry(executor.Registration{
		RuntimeKind: workerprofile.RuntimeOCI, Executor: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.executors = registry
	fixture.service.secrets = &eventBroker{Broker: fixture.secrets, events: &events}
	fixture.executor.SetBeforeExecute(func(_ context.Context, request executor.Request) {
		stdout := []byte("agent completed")
		if request.Attempt.Purpose == executor.PurposeProbe {
			stdout = []byte("codex 0.144.5")
		}
		fixture.executor.SetResult(request.Attempt, executor.Result{
			Created: true, Started: true, Completed: true, Stdout: stdout,
			SafeFacts: portableSafeFacts(request.Profile),
		}, nil)
	})
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}
	if _, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "cleanup-order")); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err != nil || result.Artifact == nil {
		t.Fatalf("Execute() error = %v", err)
	}
	evidence := loadExecutionEvidence(t, fixture, *result.Artifact)
	if evidence.AgentEnvironmentKeys == nil || !reflect.DeepEqual(
		[]string(*evidence.AgentEnvironmentKeys),
		[]string{"CODEX_HOME", "HOME", "PATH", "TMPDIR", "WORKLOAD_TOKEN"},
	) {
		t.Fatalf("agent presented-key union = %#v", evidence.AgentEnvironmentKeys)
	}
	agentDestroy := slicesIndex(events, "destroy:agent:0")
	secondRevoke := slicesIndex(events, "revoke:fixture-workload-lease")
	firstRevoke := slicesIndex(events, "revoke:fixture-codex-lease")
	if agentDestroy < 0 || secondRevoke <= agentDestroy || firstRevoke <= secondRevoke {
		t.Fatalf("lifecycle events = %#v, want agent destroy then reverse lease revoke", events)
	}
}

func TestExecuteRejectsExactLeasedSecretInOutputOrTransientPatch(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*serviceFixture)
	}{
		{
			name: "agent output",
			configure: func(fixture *serviceFixture) {
				fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
					return runner.ExecutionResult{Output: "leaked fixture-codex-auth", Started: true, Completed: true}, nil
				}
			},
		},
		{
			name: "transient patch",
			configure: func(fixture *serviceFixture) {
				fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
					result := capturedChange()
					result.Patch = []byte("+fixture-codex-auth")
					return result, nil
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			test.configure(fixture)

			result, err := fixture.service.Execute(context.Background(), "run-123")
			if err == nil || result.Status != run.StatusFailed || result.FailureClass != run.FailurePolicy ||
				result.Retryable || result.Artifact != nil || len(fixture.artifacts.Snapshot().Saves) != 0 {
				t.Fatalf("Execute() result=%#v error=%v", result, err)
			}
			record, _ := fixture.runs.Load(context.Background(), "run-123")
			encoded, _ := json.Marshal(record)
			if bytes.Contains(encoded, []byte("fixture-codex-auth")) ||
				record.Failure == nil || record.Failure.CauseCode != "secret_detected" {
				t.Fatalf("durable secret failure = %s", encoded)
			}
			if got := fixture.secrets.Revocations(); !reflect.DeepEqual(got, []string{"fixture-codex-lease"}) {
				t.Fatalf("secret revocations = %#v", got)
			}
		})
	}
}

func TestExecuteCompensatesPartialSecretAcquisitionInReverse(t *testing.T) {
	fixture := newServiceFixture(t)
	profile := resolvedWorkerProfile(t)
	profile.Secrets = append(profile.Secrets, workerprofile.SecretRequirement{
		Capability: "workload.api", BindingRevision: 9, Stage: workerprofile.StageAgent,
		Delivery: workerprofile.DeliveryEnvironment, Target: "WORKLOAD_TOKEN", Required: true,
	})
	profile.Digest = ""
	var err error
	profile, err = workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	fixture.workerProfiles.Set(profile)
	secondRef := secret.BindingRef{Capability: "workload.api", Revision: 9}
	secondBinding, err := secret.NewBinding(secondRef, secret.Authorization{
		ProfileID: profile.Metadata, Stage: workerprofile.StageAgent,
		Delivery: workerprofile.DeliveryEnvironment, Target: "WORKLOAD_TOKEN",
	}, "environment", "WORKLOAD_API_SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.secretBindings = &multiSecretRegistry{bindings: map[secret.BindingRef]secret.Binding{
		fixture.secretBindings.binding.Ref(): fixture.secretBindings.binding, secondRef: secondBinding,
	}}
	fixture.secrets.SetAcquireResult("workload.api", secret.Lease{}, errors.New("source unavailable"))
	if _, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "partial-secret")); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureEnvironment || !result.Retryable || result.Artifact != nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if got := fixture.secrets.Revocations(); !reflect.DeepEqual(got, []string{"fixture-codex-lease"}) {
		t.Fatalf("partial acquisition revocations = %#v", got)
	}
}

func TestExecuteNormalizesPartialSecretRevocationFailure(t *testing.T) {
	providerUnavailable := errors.New("second secret provider unavailable")
	revokeFailure := errors.New("first lease revocation failed")
	for _, test := range []struct {
		name     string
		cancel   bool
		priorErr error
	}{
		{name: "caller canceled", cancel: true, priorErr: context.Canceled},
		{name: "provider unavailable", priorErr: providerUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			configureSecondAgentSecret(t, fixture, "partial-secret-material")
			fixture.secrets.SetAcquireResult("workload.api", secret.Lease{}, providerUnavailable)
			fixture.secrets.SetRevokeError("fixture-codex-lease", revokeFailure)
			if _, err := fixture.service.Resolve(
				context.Background(), validRawInput("Change docs", "partial-secret-cleanup"),
			); err != nil {
				t.Fatal(err)
			}

			ctx := context.Background()
			if test.cancel {
				cancelCtx, cancel := context.WithCancel(ctx)
				ctx = cancelCtx
				fixture.service.secrets = &cancelingAcquireBroker{
					Broker: fixture.secrets, capability: "workload.api", cancel: cancel,
				}
			}
			result, executeErr := fixture.service.Execute(ctx, "run-123")
			if executeErr == nil || !errors.Is(executeErr, test.priorErr) ||
				!errors.Is(executeErr, revokeFailure) || result.Status != run.StatusFailed ||
				result.FailureClass != run.FailureCleanup || result.Retryable {
				t.Fatalf("Execute() result=%#v error=%v, want terminal cleanup failure preserving causes",
					result, executeErr)
			}
			record, loadErr := fixture.runs.Load(context.Background(), "run-123")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if record.Status != run.StatusFailed || record.Failure == nil ||
				record.Failure.Class != run.FailureCleanup ||
				record.Failure.CauseCode != "cleanup_failed" || record.Failure.Retryable {
				t.Fatalf("durable partial-acquisition cleanup failure = %#v", record)
			}
			if got := fixture.secrets.Revocations(); !reflect.DeepEqual(got, []string{"fixture-codex-lease"}) {
				t.Fatalf("partial acquisition revocations = %#v", got)
			}
			assertRunDoesNotContain(t, fixture, "fixture-codex-auth", "partial-secret-material")
		})
	}
}

func configureSecondAgentSecret(t *testing.T, fixture *serviceFixture, value string) {
	t.Helper()
	profile := resolvedWorkerProfile(t)
	profile.Secrets = append(profile.Secrets, workerprofile.SecretRequirement{
		Capability: "workload.api", BindingRevision: 9, Stage: workerprofile.StageAgent,
		Delivery: workerprofile.DeliveryEnvironment, Target: "WORKLOAD_TOKEN", Required: true,
	})
	profile.Digest = ""
	var err error
	profile, err = workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	fixture.workerProfiles.Set(profile)
	secondRef := secret.BindingRef{Capability: "workload.api", Revision: 9}
	secondBinding, err := secret.NewBinding(secondRef, secret.Authorization{
		ProfileID: profile.Metadata, Stage: workerprofile.StageAgent,
		Delivery: workerprofile.DeliveryEnvironment, Target: "WORKLOAD_TOKEN",
	}, "environment", "WORKLOAD_API_SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.secretBindings = &multiSecretRegistry{bindings: map[secret.BindingRef]secret.Binding{
		fixture.secretBindings.binding.Ref(): fixture.secretBindings.binding, secondRef: secondBinding,
	}}
	materialization, err := secret.NewValueMaterialization(
		workerprofile.DeliveryEnvironment, "WORKLOAD_TOKEN", []byte(value),
	)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := secret.NewLease(
		"fixture-workload-lease", time.Unix(100, 0).UTC().Add(time.Hour), materialization,
	)
	materialization.Destroy()
	if err != nil {
		t.Fatal(err)
	}
	fixture.secrets.SetAcquireResult("workload.api", lease, nil)
	lease.Destroy()
}

func assertRunDoesNotContain(t *testing.T, fixture *serviceFixture, values ...string) {
	t.Helper()
	record, err := fixture.runs.Load(context.Background(), "run-123")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if bytes.Contains(encoded, []byte(value)) {
			t.Fatalf("durable run leaked secret material: %s", encoded)
		}
	}
}

func countPurposeRequests(requests []executor.Request, purpose executor.Purpose) int {
	count := 0
	for _, request := range requests {
		if request.Attempt.Purpose == purpose {
			count++
		}
	}
	return count
}

func TestExecutePreservesExecutorAmbiguityAheadOfHarnessParsing(t *testing.T) {
	providerDetail := errors.New("provider disconnected after start")
	parseDetail := errors.New("strict harness rejected output")
	tests := []struct {
		name          string
		execution     executor.Result
		executorError error
		parse         func(executor.Result) (string, error)
		wantClass     run.FailureClass
		wantCause     string
		wantError     error
	}{
		{
			name: "incomplete execution and executor error with strict adapter",
			execution: executor.Result{
				Created: true, Started: true, Stdout: []byte("partial"),
			},
			executorError: executor.WrapError("internal", "disconnect", providerDetail),
			parse:         func(executor.Result) (string, error) { return "", parseDetail },
			wantClass:     run.FailureInternal,
			wantCause:     "ambiguous_attempt",
			wantError:     providerDetail,
		},
		{
			name: "completed execution and executor error with parse success",
			execution: executor.Result{
				Created: true, Started: true, Completed: true, Stdout: []byte("agent completed"),
			},
			executorError: executor.WrapError("internal", "checkpoint", providerDetail),
			parse:         func(result executor.Result) (string, error) { return string(result.Stdout), nil },
			wantClass:     run.FailureInternal,
			wantCause:     "ambiguous_attempt",
			wantError:     providerDetail,
		},
		{
			name: "parse error without executor ambiguity",
			execution: executor.Result{
				Created: true, Started: true, Completed: true, Stdout: []byte("malformed"),
			},
			parse:     func(executor.Result) (string, error) { return "", parseDetail },
			wantClass: run.FailureAgent,
			wantCause: "agent_protocol",
			wantError: parseDetail,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			fixture.harness.parse = test.parse
			fixture.executor.SetBeforeExecute(func(_ context.Context, request executor.Request) {
				result := executor.Result{
					Created: true, Started: true, Completed: true,
					SafeFacts: portableSafeFacts(request.Profile),
				}
				var executeErr error
				switch request.Attempt.Purpose {
				case executor.PurposeProbe:
					result.Stdout = []byte("codex 0.144.5")
				case executor.PurposeAgent:
					result = test.execution.Clone()
					executeErr = test.executorError
				}
				fixture.executor.SetResult(request.Attempt, result, executeErr)
			})

			result, err := fixture.service.Execute(context.Background(), "run-123")
			if err == nil || !errors.Is(err, test.wantError) ||
				result.Status != run.StatusFailed || result.FailureClass != test.wantClass || result.Retryable {
				t.Fatalf("Execute() result=%#v error=%v, want %s/%s preserving %v",
					result, err, test.wantClass, test.wantCause, test.wantError)
			}
			record, loadErr := fixture.runs.Load(context.Background(), "run-123")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if record.Failure == nil || record.Failure.CauseCode != test.wantCause {
				t.Fatalf("durable failure = %#v, want cause %q", record.Failure, test.wantCause)
			}
		})
	}
}

func TestExecuteProviderAmbiguousNonStartIsTerminalAndCleanupWins(t *testing.T) {
	providerDetail := errors.New("Docker start response was lost")
	cleanupDetail := errors.New("ambiguous Docker attempt cleanup failed")
	for _, test := range []struct {
		name       string
		cleanupErr error
		wantClass  run.FailureClass
		wantCause  string
	}{
		{
			name:      "provider ambiguity is terminal",
			wantClass: run.FailureInternal, wantCause: "ambiguous_attempt",
		},
		{
			name:       "cleanup failure wins",
			cleanupErr: cleanupDetail,
			wantClass:  run.FailureCleanup, wantCause: "cleanup_failed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			parseCalled := false
			fixture.harness.parse = func(executor.Result) (string, error) {
				parseCalled = true
				return "", errors.New("harness must not parse an ambiguous provider result")
			}
			fixture.executor.SetBeforeExecute(func(_ context.Context, request executor.Request) {
				result := executor.Result{
					Created: true, Started: true, Completed: true,
					SafeFacts: portableSafeFacts(request.Profile),
				}
				var executeErr error
				switch request.Attempt.Purpose {
				case executor.PurposeProbe:
					result.Stdout = []byte("codex 0.144.5")
				case executor.PurposeAgent:
					result = executor.Result{Created: true, Started: false, Completed: false}
					executeErr = executor.WrapError("internal", "ambiguous_attempt", providerDetail)
				}
				fixture.executor.SetResult(request.Attempt, result, executeErr)
			})
			if test.cleanupErr != nil {
				target := &cleanupFailureExecutor{
					Executor: fixture.executor, purpose: executor.PurposeAgent,
					destroyErr: test.cleanupErr, state: executor.StateUnknown,
				}
				registry, err := executor.NewRegistry(executor.Registration{
					RuntimeKind: workerprofile.RuntimeOCI, Executor: target,
				})
				if err != nil {
					t.Fatal(err)
				}
				fixture.service.executors = registry
			}

			result, executeErr := fixture.service.Execute(context.Background(), "run-123")
			if executeErr == nil || !errors.Is(executeErr, providerDetail) ||
				test.cleanupErr != nil && !errors.Is(executeErr, test.cleanupErr) ||
				result.Status != run.StatusFailed || result.FailureClass != test.wantClass ||
				result.Retryable {
				t.Fatalf("Execute() result=%#v error=%v, want terminal %s/%s",
					result, executeErr, test.wantClass, test.wantCause)
			}
			if parseCalled {
				t.Fatal("harness Parse was called for provider-reported ambiguous non-start")
			}
			record, loadErr := fixture.runs.Load(context.Background(), "run-123")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if record.Status != run.StatusFailed || record.Failure == nil ||
				record.Failure.Class != test.wantClass ||
				record.Failure.CauseCode != test.wantCause || record.Failure.Retryable {
				t.Fatalf("durable provider ambiguity = %#v", record)
			}
			agentRequests := countPurposeRequests(fixture.executor.Requests(), executor.PurposeAgent)
			again, _ := fixture.service.Execute(context.Background(), "run-123")
			if again.Status != run.StatusFailed || again.Retryable ||
				countPurposeRequests(fixture.executor.Requests(), executor.PurposeAgent) != agentRequests {
				t.Fatalf("terminal replay duplicated agent execution: result=%#v requests=%#v",
					again, fixture.executor.Requests())
			}
		})
	}
}

func TestExecuteNeverMasksProbeVerificationOrAgentCleanupFailureAsCanceled(t *testing.T) {
	cleanupFailure := errors.New("termination was not confirmed")
	tests := []struct {
		name         string
		purpose      executor.Purpose
		cancel       bool
		verification bool
		parseFailure bool
		wantClass    run.FailureClass
		wantCause    string
	}{
		{
			name: "probe cleanup failure", purpose: executor.PurposeProbe, cancel: true,
			wantClass: run.FailureCleanup, wantCause: "cleanup_failed",
		},
		{
			name: "verification cleanup failure", purpose: executor.PurposeVerification, verification: true,
			wantClass: run.FailureCleanup, wantCause: "cleanup_failed",
		},
		{
			name: "cancellation during normal agent cleanup", purpose: executor.PurposeAgent, cancel: true,
			wantClass: run.FailureCleanup, wantCause: "cleanup_failed",
		},
		{
			name:    "primary failure and cancellation during normal agent cleanup",
			purpose: executor.PurposeAgent, cancel: true, parseFailure: true,
			wantClass: run.FailureCleanup, wantCause: "cleanup_failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			if test.verification {
				fixture.profile.result.Commands = []verification.Command{{
					Name: "git status", Directory: ".", Executable: "git",
					Args: []string{"status", "--short"}, Timeout: time.Minute, Required: true,
				}}
			}
			if test.parseFailure {
				fixture.harness.parse = func(executor.Result) (string, error) {
					return "", errors.New("agent protocol failed before cleanup")
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			target := &cleanupFailureExecutor{
				Executor: fixture.executor, purpose: test.purpose,
				destroyErr: cleanupFailure, state: executor.StateUnknown,
			}
			if test.cancel {
				target.beforeDestroy = cancel
			}
			registry, err := executor.NewRegistry(executor.Registration{
				RuntimeKind: workerprofile.RuntimeOCI, Executor: target,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.service.executors = registry

			result, executeErr := fixture.service.Execute(ctx, "run-123")
			if executeErr == nil || result.Status != run.StatusFailed ||
				result.FailureClass != test.wantClass || result.Retryable {
				t.Fatalf("Execute() result=%#v error=%v, want terminal %s failure",
					result, executeErr, test.wantClass)
			}
			record, loadErr := fixture.runs.Load(context.Background(), "run-123")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if record.Failure == nil || record.Failure.CauseCode != test.wantCause ||
				record.Status == run.StatusCanceled {
				t.Fatalf("durable cleanup failure = %#v", record)
			}
		})
	}
}

func TestExecuteBoundsBlockingDestroyAndStillRevokesAgentLease(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.service.cleanupTimeout = 20 * time.Millisecond
	target := &blockingDestroyExecutor{
		Executor: fixture.executor, purpose: executor.PurposeAgent,
		started: make(chan struct{}), release: make(chan struct{}),
	}
	registry, err := executor.NewRegistry(executor.Registration{
		RuntimeKind: workerprofile.RuntimeOCI, Executor: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.executors = registry

	type response struct {
		result PhaseResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := fixture.service.Execute(context.Background(), "run-123")
		done <- response{result: result, err: err}
	}()
	<-target.started
	var completed response
	select {
	case completed = <-done:
	case <-time.After(500 * time.Millisecond):
		close(target.release)
		<-done
		t.Fatal("Execute() did not bound blocking executor Destroy")
	}
	close(target.release)

	if completed.err == nil || completed.result.Status != run.StatusFailed ||
		completed.result.FailureClass != run.FailureCleanup || completed.result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v, want bounded cleanup failure", completed.result, completed.err)
	}
	if got := fixture.secrets.Revocations(); !reflect.DeepEqual(got, []string{"fixture-codex-lease"}) {
		t.Fatalf("revocations after blocking Destroy = %#v", got)
	}
	assertRunDoesNotContain(t, fixture, "fixture-codex-auth")
}

func TestExecuteBoundsEachReverseBrokerRevocationAndAttemptsAll(t *testing.T) {
	fixture := newServiceFixture(t)
	configureSecondAgentSecret(t, fixture, "bounded-second-secret")
	fixture.service.cleanupTimeout = 20 * time.Millisecond
	blocking := &blockingRevokeBroker{
		Broker: fixture.secrets, release: make(chan struct{}), started: make(chan struct{}, 2),
	}
	fixture.service.secrets = blocking
	if _, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "bounded-revoke")); err != nil {
		t.Fatal(err)
	}

	type response struct {
		result PhaseResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := fixture.service.Execute(context.Background(), "run-123")
		done <- response{result: result, err: err}
	}()
	<-blocking.started
	var completed response
	select {
	case completed = <-done:
	case <-time.After(500 * time.Millisecond):
		close(blocking.release)
		<-done
		t.Fatal("Execute() did not bound broker Revoke")
	}
	close(blocking.release)

	if completed.err == nil || completed.result.Status != run.StatusFailed ||
		completed.result.FailureClass != run.FailureCleanup || completed.result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v, want bounded cleanup failure", completed.result, completed.err)
	}
	if got := blocking.Calls(); !reflect.DeepEqual(got, []string{"fixture-workload-lease", "fixture-codex-lease"}) {
		t.Fatalf("bounded reverse revocations = %#v", got)
	}
	assertRunDoesNotContain(t, fixture, "fixture-codex-auth", "bounded-second-secret")
}

func TestExecuteCancellationRequiresConfirmedDescendantTermination(t *testing.T) {
	for _, test := range []struct {
		name      string
		confirmed bool
		status    run.Status
		class     run.FailureClass
		cause     string
	}{
		{name: "confirmed", confirmed: true, status: run.StatusCanceled, class: run.FailureCanceled, cause: "caller_canceled"},
		{name: "unknown", confirmed: false, status: run.StatusFailed, class: run.FailureInternal, cause: "ambiguous_attempt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			var once sync.Once
			agentStarted := make(chan struct{})
			announce := func() { once.Do(func() { close(agentStarted) }) }
			fixture.agent.run = func(ctx context.Context, _ runner.RunRequest) (runner.ExecutionResult, error) {
				announce()
				<-ctx.Done()
				return runner.ExecutionResult{Started: true}, ctx.Err()
			}
			target := &cancellationExecutor{
				confirmed: test.confirmed, agentStarted: announce,
				state: executor.StateAbsent,
			}
			registry, err := executor.NewRegistry(executor.Registration{
				RuntimeKind: workerprofile.RuntimeOCI, Executor: target,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.service.executors = registry

			ctx, cancel := context.WithCancel(context.Background())
			type response struct {
				result PhaseResult
				err    error
			}
			done := make(chan response, 1)
			go func() {
				result, err := fixture.service.Execute(ctx, "run-123")
				done <- response{result: result, err: err}
			}()
			<-agentStarted
			cancel()
			completed := <-done
			if completed.err == nil || completed.result.Status != test.status ||
				completed.result.FailureClass != test.class || completed.result.Retryable {
				t.Fatalf("Execute(canceled) result=%#v error=%v", completed.result, completed.err)
			}
			record, _ := fixture.runs.Load(context.Background(), "run-123")
			if record.Failure == nil || record.Failure.CauseCode != test.cause {
				t.Fatalf("cancellation failure = %#v", record.Failure)
			}
			if slicesIndex(target.Events(), "cancel:agent:0") < 0 ||
				slicesIndex(target.Events(), "inspect:agent:0") < 0 ||
				slicesIndex(target.Events(), "destroy:agent:0") < 0 {
				t.Fatalf("cancellation lifecycle events = %#v", target.Events())
			}
			if !test.confirmed && target.State() != executor.StateUnknown {
				t.Fatalf("unknown cancellation state = %q, want unknown", target.State())
			}
			eventsBeforeRestart := target.Events()
			restarted, restartErr := fixture.service.Execute(context.Background(), "run-123")
			if restarted.Status != test.status {
				t.Fatalf("Execute(restart) result=%#v error=%v", restarted, restartErr)
			}
			if !reflect.DeepEqual(target.Events(), eventsBeforeRestart) {
				t.Fatalf("terminal cancellation reran executor: before=%#v after=%#v",
					eventsBeforeRestart, target.Events())
			}
		})
	}
}

func TestExecuteNormalizesWorkspaceCleanupFailureAfterCancellation(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	workspaceCleanup := errors.New("workspace cleanup failed after cancellation")
	fixture.service.workspaces = &fakeWorkspaceManager{prepare: func(context.Context, string, string) (workspace.Workspace, error) {
		return &fakeWorkspace{path: "/tmp/workspace", cleanup: func(context.Context) error {
			return workspaceCleanup
		}}, nil
	}}
	started := make(chan struct{})
	var once sync.Once
	target := &cancellationExecutor{
		confirmed: true, state: executor.StateAbsent,
		agentStarted: func() { once.Do(func() { close(started) }) },
	}
	registry, err := executor.NewRegistry(executor.Registration{
		RuntimeKind: workerprofile.RuntimeOCI, Executor: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.executors = registry

	ctx, cancel := context.WithCancel(context.Background())
	type response struct {
		result PhaseResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, executeErr := fixture.service.Execute(ctx, "run-123")
		done <- response{result: result, err: executeErr}
	}()
	<-started
	cancel()
	completed := <-done
	if completed.err == nil || !errors.Is(completed.err, context.Canceled) ||
		!errors.Is(completed.err, workspaceCleanup) || completed.result.Status != run.StatusFailed ||
		completed.result.FailureClass != run.FailureCleanup || completed.result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v, want cancellation normalized to cleanup failure",
			completed.result, completed.err)
	}
	record, loadErr := fixture.runs.Load(context.Background(), "run-123")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if record.Status != run.StatusFailed || record.Failure == nil ||
		record.Failure.Class != run.FailureCleanup ||
		record.Failure.CauseCode != "cleanup_failed" || record.Failure.Retryable {
		t.Fatalf("durable canceled cleanup failure = %#v", record)
	}
}

type sandboxBackedProfile struct{}

func (*sandboxBackedProfile) Name() string { return "generic" }

func (*sandboxBackedProfile) Inspect(ctx context.Context, request repository.ProfileRequest) (repository.ProfileResult, error) {
	if request.Commands == nil {
		return repository.ProfileResult{}, errors.New("sandbox command runner is required")
	}
	for _, command := range []verification.Command{
		{Name: "preflight-git", Directory: ".", Executable: "git", Args: []string{"status"}, Timeout: time.Minute, Required: true},
		{Name: "preflight-tool", Directory: ".", Executable: "go", Args: []string{"env", "GOMOD"}, Timeout: time.Minute, Required: true},
	} {
		if result := request.Commands.Run(ctx, command); !result.Passed {
			return repository.ProfileResult{}, &repository.EnvironmentError{Operation: command.Name}
		}
	}
	return repository.ProfileResult{
		Facts: map[string]string{"base_sha": "0123456789012345678901234567890123456789"},
		Commands: []verification.Command{{
			Name: "verify", Directory: ".", Executable: "go", Args: []string{"test", "./..."},
			Timeout: time.Minute, Required: true,
		}},
	}, nil
}

var _ repository.Profile = (*sandboxBackedProfile)(nil)

type multiSecretRegistry struct {
	bindings map[secret.BindingRef]secret.Binding
}

func (registry *multiSecretRegistry) Resolve(ctx context.Context, request secret.ResolveRequest) (secret.Binding, error) {
	if err := ctx.Err(); err != nil {
		return secret.Binding{}, err
	}
	binding, ok := registry.bindings[request.Ref]
	if !ok {
		return secret.Binding{}, secret.ErrBindingNotFound
	}
	if !binding.Authorizes(request) {
		return secret.Binding{}, secret.ErrBindingUnauthorized
	}
	return binding, nil
}

type eventExecutor struct {
	executor.Executor
	events *[]string
}

func (target *eventExecutor) Execute(ctx context.Context, request executor.Request) (executor.Result, error) {
	result, err := target.Executor.Execute(ctx, request)
	if !result.Started || result.ChildStartReceipt == nil {
		return result, err
	}
	receipt, receiptErr := executor.NewChildStartReceipt(
		request.Attempt,
		request.Command,
		request.Environment,
		receiptEnvironmentFiles(request),
		result.ChildStartReceipt.Challenge,
	)
	if receiptErr != nil {
		return executor.Result{}, receiptErr
	}
	result.ChildStartReceipt = &receipt
	return result, err
}

type receiptStrippingExecutor struct {
	executor.Executor
	purpose executor.Purpose
}

type receiptEnvironmentDriftExecutor struct {
	executor.Executor
	purpose executor.Purpose
	build   func(executor.Request) (map[string]string, map[string]string)
}

func (target *receiptEnvironmentDriftExecutor) Execute(
	ctx context.Context,
	request executor.Request,
) (executor.Result, error) {
	result, err := target.Executor.Execute(ctx, request)
	if request.Attempt.Purpose != target.purpose || !result.Started || result.ChildStartReceipt == nil {
		return result, err
	}
	environment, environmentFiles := target.build(request)
	receipt, receiptErr := executor.NewChildStartReceipt(
		request.Attempt,
		request.Command,
		environment,
		environmentFiles,
		result.ChildStartReceipt.Challenge,
	)
	if receiptErr != nil {
		return executor.Result{}, receiptErr
	}
	result.ChildStartReceipt = &receipt
	return result, err
}

func (target *receiptStrippingExecutor) Execute(ctx context.Context, request executor.Request) (executor.Result, error) {
	result, err := target.Executor.Execute(ctx, request)
	if request.Attempt.Purpose == target.purpose {
		result.ChildStartReceipt = nil
	}
	return result, err
}

func (target *eventExecutor) Destroy(ctx context.Context, attempt executor.AttemptID) error {
	*target.events = append(*target.events, "destroy:"+string(attempt.Purpose)+":"+strconv.Itoa(attempt.Sequence))
	return target.Executor.Destroy(ctx, attempt)
}

type eventBroker struct {
	secret.Broker
	events *[]string
}

type cancelingAcquireBroker struct {
	secret.Broker
	capability string
	cancel     context.CancelFunc
}

func (broker *cancelingAcquireBroker) Acquire(ctx context.Context, request secret.AcquireRequest) (secret.Lease, error) {
	if request.Capability == broker.capability {
		broker.cancel()
		return secret.Lease{}, context.Canceled
	}
	return broker.Broker.Acquire(ctx, request)
}

type cleanupFailureExecutor struct {
	executor.Executor
	purpose       executor.Purpose
	destroyErr    error
	state         executor.State
	beforeDestroy func()
}

func (target *cleanupFailureExecutor) Destroy(ctx context.Context, attempt executor.AttemptID) error {
	if attempt.Purpose != target.purpose {
		return target.Executor.Destroy(ctx, attempt)
	}
	if target.beforeDestroy != nil {
		target.beforeDestroy()
	}
	return target.destroyErr
}

func (target *cleanupFailureExecutor) Inspect(ctx context.Context, attempt executor.AttemptID) (executor.State, error) {
	if attempt.Purpose == target.purpose {
		if err := ctx.Err(); err != nil {
			return executor.StateUnknown, err
		}
		return target.state, nil
	}
	return target.Executor.Inspect(ctx, attempt)
}

type blockingDestroyExecutor struct {
	executor.Executor
	purpose executor.Purpose
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (target *blockingDestroyExecutor) Destroy(ctx context.Context, attempt executor.AttemptID) error {
	if attempt.Purpose != target.purpose {
		return target.Executor.Destroy(ctx, attempt)
	}
	target.once.Do(func() { close(target.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-target.release:
		return errors.New("blocking Destroy released by test")
	}
}

func (target *blockingDestroyExecutor) Inspect(ctx context.Context, attempt executor.AttemptID) (executor.State, error) {
	if attempt.Purpose == target.purpose {
		if err := ctx.Err(); err != nil {
			return executor.StateUnknown, err
		}
		return executor.StateUnknown, nil
	}
	return target.Executor.Inspect(ctx, attempt)
}

type blockingRevokeBroker struct {
	secret.Broker
	mu      sync.Mutex
	release chan struct{}
	started chan struct{}
	calls   []string
}

func (broker *blockingRevokeBroker) Revoke(ctx context.Context, id string) error {
	broker.mu.Lock()
	broker.calls = append(broker.calls, id)
	broker.mu.Unlock()
	broker.started <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-broker.release:
		return errors.New("blocking Revoke released by test")
	}
}

func (broker *blockingRevokeBroker) Calls() []string {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]string(nil), broker.calls...)
}

type cancellationExecutor struct {
	mu           sync.Mutex
	confirmed    bool
	agentStarted func()
	state        executor.State
	events       []string
}

func (target *cancellationExecutor) Execute(ctx context.Context, request executor.Request) (executor.Result, error) {
	if err := request.Validate(); err != nil {
		return executor.Result{}, err
	}
	receipt, err := executor.NewRandomChildStartReceipt(
		request.Attempt, request.Command, request.Environment, nil,
	)
	if err != nil {
		return executor.Result{}, err
	}
	if request.Attempt.Purpose == executor.PurposeProbe {
		return executor.Result{
			Created: true, Started: true, Completed: true, Stdout: []byte("codex 0.144.5"),
			SafeFacts: portableSafeFacts(request.Profile), ChildStartReceipt: &receipt,
		}, nil
	}
	if request.Attempt.Purpose != executor.PurposeAgent {
		return executor.Result{Created: true, Started: true, Completed: true, ChildStartReceipt: &receipt}, nil
	}
	target.agentStarted()
	<-ctx.Done()
	target.mu.Lock()
	target.state = executor.StateRunning
	target.mu.Unlock()
	return executor.Result{Created: true, Started: true, ChildStartReceipt: &receipt}, ctx.Err()
}

func (target *cancellationExecutor) Inspect(ctx context.Context, attempt executor.AttemptID) (executor.State, error) {
	if err := ctx.Err(); err != nil {
		return executor.StateUnknown, err
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	target.events = append(target.events, "inspect:"+string(attempt.Purpose)+":"+strconv.Itoa(attempt.Sequence))
	return target.state, nil
}

func (target *cancellationExecutor) Cancel(ctx context.Context, attempt executor.AttemptID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	target.events = append(target.events, "cancel:"+string(attempt.Purpose)+":"+strconv.Itoa(attempt.Sequence))
	if target.confirmed {
		target.state = executor.StateCompleted
	} else {
		target.state = executor.StateUnknown
	}
	return nil
}

func (target *cancellationExecutor) Destroy(ctx context.Context, attempt executor.AttemptID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	target.events = append(target.events, "destroy:"+string(attempt.Purpose)+":"+strconv.Itoa(attempt.Sequence))
	if target.confirmed {
		target.state = executor.StateDestroyed
	}
	return nil
}

func (target *cancellationExecutor) State() executor.State {
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.state
}

func (target *cancellationExecutor) Events() []string {
	target.mu.Lock()
	defer target.mu.Unlock()
	return append([]string(nil), target.events...)
}

func (broker *eventBroker) Revoke(ctx context.Context, id string) error {
	*broker.events = append(*broker.events, "revoke:"+id)
	return broker.Broker.Revoke(ctx, id)
}

func slicesIndex(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func TestExecuteUsesFreshRealWorktreeAndPersistsCompleteArtifact(t *testing.T) {
	source, baseSHA := createGitSource(t)
	managerRoot := t.TempDir()
	manager, err := gitworktree.New(managerRoot)
	if err != nil {
		t.Fatalf("gitworktree.New() error = %v", err)
	}
	runtimeRoot := t.TempDir()
	codexHome := t.TempDir()
	envPolicy, err := environment.NewPolicy(environment.Config{
		RuntimeRoot: runtimeRoot,
		Source: map[string]string{
			"PATH": os.Getenv("PATH"), "HATCHET_CLIENT_TOKEN": "hatchet-secret",
			"MEM0_API_KEY": "mem0-secret", "GITHUB_TOKEN": "github-secret",
		},
		CodexHome:  codexHome,
		CodexAgent: true,
	})
	if err != nil {
		t.Fatalf("environment.NewPolicy() error = %v", err)
	}
	capturer, err := gitcapture.New()
	if err != nil {
		t.Fatalf("gitcapture.New() error = %v", err)
	}
	changePolicy, err := policy.NewChangePolicy(policy.Config{})
	if err != nil {
		t.Fatalf("policy.NewChangePolicy() error = %v", err)
	}
	registry, _ := template.NewRegistry(templatecodechange.Definition{})
	profile := &workspaceProfile{}
	agent := &writingAgent{}
	verifier := &recordingVerifier{}
	mem := &recordingMemory{result: []memory.Memory{{ID: "memory-1", Content: "Keep the public API stable"}}}
	runs := runmock.NewStore()
	artifacts := artifactmock.NewStore()
	workerProfiles, secretBindings, executors, harnesses, targetExecutor, _ := portableRuntimeDependencies(t)
	configurePortableExecutor(targetExecutor, agent, verifier)
	service, err := New(Dependencies{
		Templates: registry, Runs: runs, Memory: mem, Resolver: manager,
		Workspaces: manager, Profiles: map[string]repository.Profile{
			"generic": profile, "go": &fakeProfile{name: "go"},
		},
		WorkerProfiles: workerProfiles, SecretBindings: secretBindings,
		Secrets: configuredSecretBroker(t), Executors: executors, Harnesses: harnesses,
		Environments: envPolicy, Agent: agent, Verifier: verifier,
		Capturer: capturer, Policy: changePolicy, Artifacts: artifacts,
		Publisher: publishermock.NewPublisher(structPublisherResult(), nil),
		Clock:     func() time.Time { return time.Unix(100, 0).UTC() },
		NewID:     func() string { return "run-123" },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	raw := rawForRepository(source)
	resolved, err := service.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if record, _ := runs.Load(context.Background(), resolved.RunID); record.BaseSHA != baseSHA {
		t.Fatalf("resolved BaseSHA = %q, want %q", record.BaseSHA, baseSHA)
	}

	result, err := service.Execute(context.Background(), resolved.RunID)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != run.StatusExecuting || result.Artifact == nil {
		t.Fatalf("Execute() result = %#v", result)
	}
	requests := agent.Requests()
	if len(requests) != 1 {
		t.Fatalf("agent requests = %d, want 1", len(requests))
	}
	for _, value := range []string{
		"Keep the public API stable", "alpha: first", "zeta: last",
		"Base SHA: " + baseSHA, "Profile: generic",
	} {
		if !strings.Contains(requests[0].TaskDescription, value) {
			t.Errorf("agent prompt does not contain %q:\n%s", value, requests[0].TaskDescription)
		}
	}
	for _, key := range []string{"PATH", "HOME", "TMPDIR"} {
		if requests[0].Env[key] == "" {
			t.Errorf("agent environment lacks %s", key)
		}
	}
	for _, denied := range []string{"HATCHET_CLIENT_TOKEN", "MEM0_API_KEY", "GITHUB_TOKEN", "GH_TOKEN"} {
		if _, exists := requests[0].Env[denied]; exists {
			t.Errorf("agent environment contains %s", denied)
		}
	}
	if got := verifier.Commands(); len(got) != 2 ||
		!strings.HasSuffix(got[0].Directory, "module-a") ||
		!strings.HasSuffix(got[1].Directory, "module-b") {
		t.Fatalf("verification commands = %#v", got)
	}

	snapshot := artifacts.Snapshot()
	if len(snapshot.Saves) != 1 {
		t.Fatalf("artifact saves = %d, want 1", len(snapshot.Saves))
	}
	bundle := snapshot.Bundles[result.Artifact.Digest]
	if len(bundle.ChangesPatch) == 0 || !strings.Contains(string(bundle.AgentOutput), "updated changed.txt") {
		t.Fatalf("artifact patch/output missing: %#v", bundle)
	}
	if bundle.Manifest.BaseSHA != baseSHA || bundle.Manifest.TreeSHA == "" ||
		bundle.Manifest.MemoryCount != 1 || len(bundle.Manifest.MemoryIDs) != 1 ||
		bundle.Manifest.MemoryIDs[0] != "memory-1" {
		t.Fatalf("artifact manifest = %#v", bundle.Manifest)
	}
	if bundle.Preflight["alpha"] != "first" || len(bundle.Verification) != 2 ||
		len(bundle.Warnings) != 1 || !strings.Contains(string(bundle.ExecutionMetadata), `"completed":true`) {
		t.Fatalf("artifact evidence incomplete: %#v", bundle)
	}
	if bundle.Verification[0].Command.Directory != "module-a" ||
		bundle.Verification[1].Command.Directory != "module-b" {
		t.Fatalf("durable verification directories = %#v, want workspace-relative", bundle.Verification)
	}
	record, _ := runs.Load(context.Background(), resolved.RunID)
	serializedBundle, _ := json.Marshal(bundle)
	serializedRecord, _ := json.Marshal(record)
	serialized := strings.Join([]string{
		string(serializedBundle), string(serializedRecord), string(bundle.AgentOutput),
		string(bundle.ChangesPatch),
	}, "\n")
	if strings.Contains(string(serializedBundle), "Keep the public API stable") {
		t.Fatal("artifact leaked memory content")
	}
	for _, ephemeral := range []string{requests[0].WorkspacePath, runtimeRoot, codexHome} {
		if strings.Contains(serialized, ephemeral) {
			t.Fatalf("durable evidence leaked ephemeral prefix %q:\n%s", ephemeral, serialized)
		}
	}
	if _, err := os.Stat(filepath.Join(source, "changed.txt")); !os.IsNotExist(err) {
		t.Fatalf("source repository was changed: %v", err)
	}
	assertDirectoryEmpty(t, filepath.Join(managerRoot, "worktrees"))
	if _, err := os.Stat(filepath.Join(runtimeRoot, "run-123")); !os.IsNotExist(err) {
		t.Fatalf("runtime directory remains: %v", err)
	}

	again, err := service.Execute(context.Background(), resolved.RunID)
	if err != nil {
		t.Fatalf("Execute(second) error = %v", err)
	}
	if again.Artifact == nil || *again.Artifact != *result.Artifact ||
		len(agent.Requests()) != 1 || len(artifacts.Snapshot().Saves) != 1 {
		t.Fatalf("second Execute was not idempotent: result=%#v calls=%d saves=%d", again, len(agent.Requests()), len(artifacts.Snapshot().Saves))
	}
}

func TestExecuteCompletedNonzeroAgentRetainsArtifactWithoutParsing(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	parseCalled := false
	fixture.harness.parse = func(executor.Result) (string, error) {
		parseCalled = true
		return "parsed nonzero output", nil
	}
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{
			Output: "untrusted nonzero output", Transcript: `{"type":"item.completed"}`,
			ExitCode: 7, Started: true, Completed: true,
		}, nil
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil {
		t.Fatal("Execute() error = nil")
	}
	if result.Status != run.StatusFailed || result.FailureClass != run.FailureAgent ||
		result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result = %#v", result)
	}
	bundle := fixture.artifacts.Snapshot().Bundles[result.Artifact.Digest]
	if parseCalled || len(bundle.AgentOutput) != 0 {
		t.Fatalf("nonzero result was parsed: called=%v artifact output=%q", parseCalled, bundle.AgentOutput)
	}
}

func TestExecuteClassifiesNonzeroBeforeRealCodexAdapterParsing(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	adapter, err := harnesscodex.New(harnesscodex.SupportedVersion)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := harness.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.harnesses = registry
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{
			Output: "not valid Codex JSONL", ExitCode: 23, Started: true, Completed: true,
		}, nil
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, executeErr := fixture.service.Execute(context.Background(), "run-123")
	if executeErr == nil || result.Status != run.StatusFailed ||
		result.FailureClass != run.FailureAgent || result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, executeErr)
	}
	record, loadErr := fixture.runs.Load(context.Background(), "run-123")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if record.Failure == nil || record.Failure.CauseCode != "nonzero_exit" {
		t.Fatalf("real Codex nonzero failure = %#v, want nonzero_exit", record.Failure)
	}
}

func TestExecuteRequiredVerificationFailureRetainsEvidenceArtifact(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.profile.result = repository.ProfileResult{
		Facts: map[string]string{"base_sha": fixture.resolver.revision.SHA},
		Commands: []verification.Command{{
			Name: "required", Directory: "/tmp/workspace", Executable: "git",
			Args: []string{"status"}, Timeout: time.Minute, Required: true,
		}},
	}
	fixture.verifier.run = func(_ context.Context, command verification.Command, _ map[string]string) verification.Result {
		return verification.Result{
			Command: command, ExitCode: 1, Output: "tests failed",
			FailureClass: "verification", CauseCode: "nonzero_exit",
		}
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureVerification ||
		result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	bundle := fixture.artifacts.Snapshot().Bundles[result.Artifact.Digest]
	if len(bundle.Verification) != 1 || bundle.Verification[0].CauseCode != "nonzero_exit" {
		t.Fatalf("artifact verification = %#v", bundle.Verification)
	}
}

func TestExecuteVerificationInternalFailureKeepsInternalClass(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.profile.result = repository.ProfileResult{
		Commands: []verification.Command{{
			Name: "invalid", Directory: "/tmp/workspace", Executable: "git",
			Args: []string{"status"}, Timeout: time.Minute, Required: true,
		}},
	}
	fixture.verifier.run = func(_ context.Context, command verification.Command, _ map[string]string) verification.Result {
		return verification.Result{
			Command: command, FailureClass: "internal", CauseCode: "invalid_command",
		}
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureInternal || result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
}

func TestExecuteMissingRequiredToolIsEnvironmentFailureAndDoesNotRunAgent(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.profile.err = &repository.EnvironmentError{Operation: "required tool"}
	calls := 0
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		calls++
		return runner.ExecutionResult{}, nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureEnvironment || result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if calls != 0 {
		t.Fatalf("agent calls = %d, want 0", calls)
	}
}

func TestExecutePromptOverflowFailsAsInputBeforeAgent(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.mem.result = []memory.Memory{{ID: "oversized", Content: strings.Repeat("x", maxAgentPromptBytes)}}
	if _, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "oversized-prompt")); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	calls := 0
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		calls++
		return runner.ExecutionResult{}, nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureInput || result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if calls != 0 {
		t.Fatalf("agent calls = %d, want 0", calls)
	}
}

func TestExecuteRejectsRunBindingMismatchBeforeSideEffects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(run.Record) run.Record
	}{
		{"template", func(record run.Record) run.Record {
			record.Template.Version = 2
			return record
		}},
		{"repository", func(record run.Record) run.Record {
			record.RepositoryURI = "https://other.test/repository.git"
			return record
		}},
		{"base ref", func(record run.Record) run.Record {
			record.BaseRef = "refs/heads/other"
			return record
		}},
		{"publication", func(record run.Record) run.Record {
			record.PublicationMode = "pull_request"
			return record
		}},
		{"input hash", func(record run.Record) run.Record {
			record.InputHash = strings.Repeat("0", 64)
			return record
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			fixture.runs.loadMutate = test.mutate
			calls := 0
			fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
				calls++
				return runner.ExecutionResult{}, nil
			}

			_, err := fixture.service.Execute(context.Background(), "run-123")
			if !errors.Is(err, ErrRunBinding) {
				t.Fatalf("Execute() error = %v, want %v", err, ErrRunBinding)
			}
			if fixture.workspaces.calls != 0 || calls != 0 {
				t.Fatalf("side effects: workspaces=%d agent=%d", fixture.workspaces.calls, calls)
			}
		})
	}
}

func TestExhaustRejectsRunBindingMismatch(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{}, errors.New("agent unavailable")
	}
	if _, err := fixture.service.Execute(context.Background(), "run-123"); err == nil {
		t.Fatal("Execute() error = nil")
	}
	fixture.runs.loadMutate = func(record run.Record) run.Record {
		record.Template.Version = 2
		return record
	}

	if _, err := fixture.service.Exhaust(context.Background(), "run-123", "execute"); !errors.Is(err, ErrRunBinding) {
		t.Fatalf("Exhaust() error = %v, want %v", err, ErrRunBinding)
	}
}

func TestExecuteDoesNotConsultAmbientEnvironmentBuilder(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	called := false
	fixture.env.build = func(context.Context, environment.Request) (environment.Result, error) {
		called = true
		return environment.Result{}, errors.New("requested GITHUB_TOKEN value super-secret is denied")
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err != nil || result.Artifact == nil || called {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	encoded, _ := json.Marshal(record)
	if strings.Contains(string(encoded), "super-secret") || strings.Contains(string(encoded), "GITHUB_TOKEN") {
		t.Fatalf("run evidence leaked denial details: %s", encoded)
	}
}

func TestExecuteChangePolicyDenialPersistsOnlySafeFindings(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		result := capturedChange()
		result.Patch = []byte("TOP_SECRET=never-persist-this")
		return result, nil
	}
	fixture.policy.decision = policy.Decision{
		Allowed: false,
		Findings: []policy.Finding{{
			RuleID: "secret-assignment", Path: "/tmp/workspace/changed.txt", Line: 1,
		}},
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailurePolicy || result.Artifact != nil ||
		len(fixture.artifacts.Snapshot().Saves) != 0 {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	encoded, _ := json.Marshal(record)
	if strings.Contains(string(encoded), "never-persist-this") ||
		strings.Contains(string(encoded), "/tmp/workspace") ||
		!strings.Contains(string(encoded), "secret-assignment") ||
		!strings.Contains(string(encoded), "changed.txt") {
		t.Fatalf("policy evidence = %s", encoded)
	}
}

func TestExecutePreservesCanonicalCapturePathsAndPatchAgreement(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	path := "docs/tmp/workspace/file.txt"
	patch := []byte(
		"diff --git a/" + path + " b/" + path + "\n" +
			"new file mode 100644\n--- /dev/null\n+++ b/" + path + "\n" +
			"@@ -0,0 +1 @@\n+safe\n",
	)
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return gitcapture.Result{
			Patch: patch,
			Changes: []artifact.Change{{
				Path: path, Status: "A", OldMode: "000000", NewMode: "100644",
			}},
			TreeSHA: "tree-sha",
		}, nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err != nil || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	bundle := fixture.artifacts.Snapshot().Bundles[result.Artifact.Digest]
	if len(bundle.Manifest.Changes) != 1 || bundle.Manifest.Changes[0].Path != path {
		t.Fatalf("manifest paths = %#v, want exact capture path %q", bundle.Manifest.Changes, path)
	}
	if !bytes.Equal(bundle.ChangesPatch, patch) ||
		!bytes.Contains(bundle.ChangesPatch, []byte("+++ b/"+bundle.Manifest.Changes[0].Path)) {
		t.Fatalf("manifest/patch disagreement: change=%#v patch=%q", bundle.Manifest.Changes[0], bundle.ChangesPatch)
	}
}

func TestExecuteRejectsNonCanonicalCapturedOldAndNewPaths(t *testing.T) {
	tests := []artifact.Change{
		{Path: "../escape.txt", Status: "M"},
		{Path: "/absolute.txt", Status: "M"},
		{Path: "dir/../file.txt", Status: "M"},
		{Path: `dir\file.txt`, Status: "M"},
		{Path: "new.txt", OldPath: "../old.txt", Status: "R"},
	}
	for _, change := range tests {
		t.Run(change.Path+"-"+change.OldPath, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
				return gitcapture.Result{
					Patch:   []byte("diff --git a/file.txt b/file.txt\n"),
					Changes: []artifact.Change{change}, TreeSHA: "tree-sha",
				}, nil
			}

			result, err := fixture.service.Execute(context.Background(), "run-123")
			if err == nil || result.FailureClass != run.FailurePolicy ||
				result.Retryable || result.Artifact != nil {
				t.Fatalf("Execute() result=%#v error=%v", result, err)
			}
			if saves := fixture.artifacts.Snapshot().Saves; len(saves) != 0 {
				t.Fatalf("unsafe path reached artifact store: %#v", saves)
			}
		})
	}
}

func TestExecuteRejectsEphemeralAbsolutePathInPatchWithoutArtifact(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.capturer.capture = func(_ context.Context, request gitcapture.Request) (gitcapture.Result, error) {
		return gitcapture.Result{
			Patch: []byte(
				"diff --git a/location.txt b/location.txt\n" +
					"new file mode 100644\n--- /dev/null\n+++ b/location.txt\n" +
					"@@ -0,0 +1 @@\n+" + request.Workspace + "\n",
			),
			Changes: []artifact.Change{{
				Path: "location.txt", Status: "A", OldMode: "000000", NewMode: "100644",
			}},
			TreeSHA: "tree-sha",
		}, nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.Retryable || result.Artifact != nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if result.FailureClass != run.FailurePolicy && result.FailureClass != run.FailureInternal {
		t.Fatalf("FailureClass = %q, want safe policy/internal failure", result.FailureClass)
	}
	if saves := fixture.artifacts.Snapshot().Saves; len(saves) != 0 {
		t.Fatalf("ephemeral patch reached artifact store: %#v", saves)
	}
}

func TestExecuteDoesNotConsultAmbientRuntimePrefixes(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	called := false
	fixture.env.build = func(_ context.Context, request environment.Request) (environment.Result, error) {
		called = true
		root := "/agent"
		if request.Stage == environment.StageVerification {
			root = "/verification"
		}
		return environment.Result{Values: map[string]string{
			"PATH": "/bin", "HOME": root + "/home", "TMPDIR": root + "/tmp",
			"TMP": root + "/tmp", "TEMP": root + "/tmp", "CODEX_HOME": "/safe/codex/home",
		}}, nil
	}
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{
			Output:  "kept /repository/path; used /agent/home/cache",
			Started: true, Completed: true,
		}, nil
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err != nil || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	output := string(fixture.artifacts.Snapshot().Bundles[result.Artifact.Digest].AgentOutput)
	if !strings.Contains(output, "/repository/path") ||
		!strings.Contains(output, "/agent/home") || called {
		t.Fatalf("boundary scrubbed output = %q", output)
	}
}

func TestExecuteIgnoresLegacyAmbientScrubPrefixes(t *testing.T) {
	for _, prefix := range []string{"/", "/tmp", "relative/home"} {
		t.Run(prefix, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			called := false
			fixture.env.build = func(context.Context, environment.Request) (environment.Result, error) {
				called = true
				return environment.Result{Values: map[string]string{
					"PATH": "/bin", "HOME": prefix, "TMPDIR": "/safe/runtime/tmp",
					"TMP": "/safe/runtime/tmp", "TEMP": "/safe/runtime/tmp", "CODEX_HOME": "/safe/codex/home",
				}}, nil
			}
			fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
				return capturedChange(), nil
			}

			result, err := fixture.service.Execute(context.Background(), "run-123")
			if err != nil || result.Artifact == nil || called {
				t.Fatalf("Execute() result=%#v error=%v", result, err)
			}
		})
	}
}

func TestExecuteRejectsDurableEvidenceKeyCollision(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.profile.result.Facts = map[string]string{
		"/tmp/workspace": "workspace-key",
		".":              "literal-key",
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureInternal ||
		result.Retryable || result.Artifact != nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if saves := fixture.artifacts.Snapshot().Saves; len(saves) != 0 {
		t.Fatalf("colliding evidence reached artifact store: %#v", saves)
	}
}

func TestExecuteCancellationCleansWithNonCanceledContext(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	cleanWorkspace := false
	ambientCleanupCalled := false
	fixture.service.workspaces = &fakeWorkspaceManager{prepare: func(context.Context, string, string) (workspace.Workspace, error) {
		return &fakeWorkspace{path: "/tmp/workspace", cleanup: func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Fatalf("workspace cleanup context canceled: %v", ctx.Err())
			}
			cleanWorkspace = true
			return nil
		}}, nil
	}}
	fixture.env.cleanup = func(ctx context.Context, _ string) error {
		ambientCleanupCalled = true
		return nil
	}
	started := make(chan struct{})
	var once sync.Once
	target := &cancellationExecutor{
		confirmed: true, state: executor.StateAbsent,
		agentStarted: func() { once.Do(func() { close(started) }) },
	}
	registry, err := executor.NewRegistry(executor.Registration{
		RuntimeKind: workerprofile.RuntimeOCI, Executor: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.executors = registry
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()

	result, err := fixture.service.Execute(ctx, "run-123")
	if !errors.Is(err, context.Canceled) || result.Status != run.StatusCanceled ||
		result.FailureClass != run.FailureCanceled || result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if !cleanWorkspace || ambientCleanupCalled {
		t.Fatalf("cleanup workspace=%t ambient=%t", cleanWorkspace, ambientCleanupCalled)
	}
}

func TestExecuteArtifactSaveCancellationPersistsTerminalCanceled(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.service.artifacts = artifactmock.NewStore(artifactmock.Config{SaveError: context.Canceled})
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if !errors.Is(err, context.Canceled) || result.Status != run.StatusCanceled ||
		result.FailureClass != run.FailureCanceled || result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
}

func TestExecuteCancellationAfterArtifactSaveCheckpointsArtifact(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.service.artifacts = &artifactStoreFunc{
		Store: fixture.artifacts,
		save: func(ctx context.Context, bundle artifact.Bundle) (artifact.Reference, error) {
			reference, err := fixture.artifacts.Save(ctx, bundle)
			cancel()
			return reference, err
		},
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(ctx, "run-123")
	if !errors.Is(err, context.Canceled) || result.Status != run.StatusCanceled ||
		result.FailureClass != run.FailureCanceled || result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	record, _ := fixture.runs.Store.Load(context.Background(), "run-123")
	if record.Artifact == nil || *record.Artifact != *result.Artifact {
		t.Fatalf("checkpointed artifact = %#v, result = %#v", record.Artifact, result.Artifact)
	}
}

func TestExecuteDoesNotRunAmbientCleanup(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	called := false
	fixture.env.cleanup = func(context.Context, string) error {
		called = true
		cancel()
		return nil
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(ctx, "run-123")
	if err != nil || result.Artifact == nil || called || ctx.Err() != nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
}

func TestExecuteCancellationAtFinalSaveIsCompensatedDurably(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.runs.saveCalls = 0
	fixture.runs.saveHook = func(call int, _ run.Record) {
		if call == 5 {
			cancel()
		}
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(ctx, "run-123")
	if !errors.Is(err, context.Canceled) || result.Status != run.StatusCanceled ||
		result.FailureClass != run.FailureCanceled || result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	record, _ := fixture.runs.Store.Load(context.Background(), "run-123")
	if record.Status != run.StatusCanceled || record.Artifact == nil {
		t.Fatalf("durable record = %#v", record)
	}
}

func TestExecuteCancellationWithFinalSaveErrorStillCompensates(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.runs.saveCalls = 0
	finalSaveErr := errors.New("final save unavailable")
	fixture.runs.saveHook = func(call int, _ run.Record) {
		if call == 5 {
			cancel()
		}
	}
	fixture.runs.saveErrors = map[int]error{5: finalSaveErr}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(ctx, "run-123")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, finalSaveErr) ||
		result.Status != run.StatusCanceled || result.FailureClass != run.FailureCanceled ||
		result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	record, _ := fixture.runs.Store.Load(context.Background(), "run-123")
	latest, _ := latestStage(record, "execute")
	if record.Status != run.StatusCanceled || latest.Attempts != 1 ||
		latest.Status != run.StageFailed || latest.Failure == nil ||
		latest.Failure.Class != run.FailureCanceled {
		t.Fatalf("durable cancellation record = %#v", record)
	}
}

func TestExecuteCancellationAfterExhaustedFinalCASStillCompensates(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.runs.saveCalls = 0
	fixture.runs.saveHook = func(call int, _ run.Record) {
		if call == 5 {
			cancel()
		}
	}
	fixture.runs.saveErrors = map[int]error{
		5: run.ErrVersionConflict,
		6: run.ErrVersionConflict,
		7: run.ErrVersionConflict,
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(ctx, "run-123")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, run.ErrVersionConflict) ||
		result.Status != run.StatusCanceled || result.FailureClass != run.FailureCanceled ||
		result.Retryable || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	record, _ := fixture.runs.Store.Load(context.Background(), "run-123")
	latest, _ := latestStage(record, "execute")
	if record.Status != run.StatusCanceled || latest.Attempts != 1 ||
		latest.Status != run.StageFailed || latest.Failure == nil ||
		latest.Failure.Class != run.FailureCanceled {
		t.Fatalf("durable cancellation record = %#v", record)
	}
}

func TestExecuteCancellationJoinsFinalAndCompensationSaveErrors(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.runs.saveCalls = 0
	finalSaveErr := errors.New("final save unavailable")
	compensationErr := errors.New("compensation save unavailable")
	fixture.runs.saveHook = func(call int, _ run.Record) {
		if call == 5 {
			cancel()
		}
	}
	fixture.runs.saveErrors = map[int]error{5: finalSaveErr, 6: compensationErr}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(ctx, "run-123")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, finalSaveErr) ||
		!errors.Is(err, compensationErr) || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
}

func TestExecuteLateCancellationAfterPersistedFailureWinsDurably(t *testing.T) {
	tests := []struct {
		name         string
		failureClass run.FailureClass
		cancelWins   bool
		configure    func(*serviceFixture)
	}{
		{
			name: "nonretryable agent failure", failureClass: run.FailureAgent, cancelWins: true,
			configure: func(fixture *serviceFixture) {
				fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
					return runner.ExecutionResult{ExitCode: 7, Started: true, Completed: true}, nil
				}
			},
		},
		{
			name: "verification failure", failureClass: run.FailureVerification, cancelWins: true,
			configure: func(fixture *serviceFixture) {
				fixture.profile.result.Commands = []verification.Command{{
					Name: "test", Directory: "/tmp/workspace", Executable: "go",
					Args: []string{"test", "./..."}, Timeout: time.Minute, Required: true,
				}}
				fixture.verifier.run = func(_ context.Context, command verification.Command, _ map[string]string) verification.Result {
					return verification.Result{
						Command: command, FailureClass: "verification",
						CauseCode: "test_failed",
					}
				}
			},
		},
		{
			name: "cleanup failure", failureClass: run.FailureCleanup,
			configure: func(fixture *serviceFixture) {
				fixture.service.workspaces = &fakeWorkspaceManager{
					prepare: func(context.Context, string, string) (workspace.Workspace, error) {
						return &fakeWorkspace{
							path: "/tmp/workspace",
							cleanup: func(context.Context) error {
								return errors.New("cleanup failed")
							},
						}, nil
					},
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			test.configure(fixture)
			fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
				return capturedChange(), nil
			}

			var persistedFailure run.Record
			fixture.runs.saveHook = func(_ int, candidate run.Record) {
				if candidate.Status == run.StatusFailed && persistedFailure.ID == "" {
					persistedFailure = run.CloneRecord(candidate)
					cancel()
				}
			}

			result, err := fixture.service.Execute(ctx, "run-123")
			if persistedFailure.Status != run.StatusFailed ||
				persistedFailure.Failure == nil ||
				persistedFailure.Failure.Class != test.failureClass {
				t.Fatalf("successfully persisted failure = %#v", persistedFailure)
			}

			record, loadErr := fixture.runs.Store.Load(context.Background(), "run-123")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if !test.cancelWins {
				if result.Status != run.StatusFailed ||
					result.FailureClass != run.FailureCleanup || result.Retryable ||
					record.Status != run.StatusFailed || record.Failure == nil ||
					record.Failure.Class != run.FailureCleanup ||
					record.Failure.CauseCode != "cleanup_failed" {
					t.Fatalf("cleanup failure was masked by late cancellation: result=%#v error=%v record=%#v",
						result, err, record)
				}
				for _, stage := range record.Stages {
					if stage.Name == run.ExecuteCancellationStage {
						t.Fatalf("cleanup failure gained false cancellation evidence: %#v", stage)
					}
				}
				return
			}
			if !errors.Is(err, context.Canceled) || result.Status != run.StatusCanceled ||
				result.FailureClass != run.FailureCanceled || result.Retryable {
				t.Fatalf("Execute() result=%#v error=%v", result, err)
			}
			if record.Status != run.StatusCanceled || record.Failure == nil ||
				record.Failure.Class != run.FailureCanceled ||
				record.Artifact == nil || !reflect.DeepEqual(record.Artifact, persistedFailure.Artifact) {
				t.Fatalf("durable cancellation record = %#v", record)
			}
			if len(record.Stages) != len(persistedFailure.Stages)+1 ||
				!reflect.DeepEqual(record.Stages[:len(persistedFailure.Stages)], persistedFailure.Stages) {
				t.Fatalf("prior failure evidence was rewritten:\nbefore=%#v\nafter=%#v",
					persistedFailure.Stages, record.Stages)
			}
			cancellation := record.Stages[len(record.Stages)-1]
			owned, found := latestStage(persistedFailure, "execute")
			if !found || cancellation.Name != run.ExecuteCancellationStage ||
				cancellation.Status != run.StageFailed ||
				cancellation.Failure == nil ||
				cancellation.Failure.Class != run.FailureCanceled ||
				cancellation.Evidence["execute_attempt"] != strconv.Itoa(owned.Attempts) ||
				cancellation.Evidence["execute_started_at"] != owned.StartedAt.UTC().Format(time.RFC3339Nano) {
				t.Fatalf("cancellation evidence = %#v, owned execute = %#v", cancellation, owned)
			}
		})
	}
}

func TestExecuteCleanupFailureNormalizesPrimaryClass(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	primary := errors.New("agent transport failed")
	cleanup := errors.New("workspace cleanup failed")
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{}, primary
	}
	fixture.service.workspaces = &fakeWorkspaceManager{prepare: func(context.Context, string, string) (workspace.Workspace, error) {
		return &fakeWorkspace{path: "/tmp/workspace", cleanup: func(context.Context) error { return cleanup }}, nil
	}}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("Execute() error = %v, want joined primary and cleanup", err)
	}
	if result.Status != run.StatusFailed || result.FailureClass != run.FailureCleanup {
		t.Fatalf("Execute() result = %#v, want terminal cleanup failure", result)
	}
	if result.Retryable {
		t.Fatal("cleanup failure left primary failure retryable")
	}
}

func TestExecuteWorkspaceCleanupUsesIndependentBudgetWithoutAmbientCleanup(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.service.cleanupTimeout = 20 * time.Millisecond
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{}, errors.New("agent unavailable")
	}
	ambientAttempted := false
	workspaceAttempted := false
	fixture.env.cleanup = func(ctx context.Context, _ string) error {
		ambientAttempted = true
		<-ctx.Done()
		return ctx.Err()
	}
	fixture.service.workspaces = &fakeWorkspaceManager{prepare: func(context.Context, string, string) (workspace.Workspace, error) {
		return &fakeWorkspace{path: "/tmp/workspace", cleanup: func(ctx context.Context) error {
			workspaceAttempted = true
			if ctx.Err() != nil {
				return errors.New("workspace cleanup received starved context")
			}
			return nil
		}}, nil
	}}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.FailureClass != run.FailureAgent || !result.Retryable {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if ambientAttempted || !workspaceAttempted {
		t.Fatalf("cleanup attempts ambient=%t workspace=%t", ambientAttempted, workspaceAttempted)
	}
	if strings.Contains(err.Error(), "starved") {
		t.Fatalf("workspace cleanup reused expired runtime budget: %v", err)
	}
}

func TestExecuteIgnoresAmbientCleanupFailureAndCleansWorktree(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	runtimeErr := errors.New("partial runtime cleanup")
	worktreeCleaned := false
	fixture.env.cleanup = func(context.Context, string) error { return runtimeErr }
	fixture.service.workspaces = &fakeWorkspaceManager{prepare: func(context.Context, string, string) (workspace.Workspace, error) {
		return &fakeWorkspace{path: "/tmp/workspace", cleanup: func(context.Context) error {
			worktreeCleaned = true
			return nil
		}}, nil
	}}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err != nil || result.Artifact == nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if !worktreeCleaned {
		t.Fatal("worktree cleanup was not attempted")
	}
}

func TestExhaustMakesLatestRetryableFailureTerminalAndIsIdempotent(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		return runner.ExecutionResult{}, errors.New("agent unavailable")
	}
	first, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || !first.Retryable || first.Status != run.StatusExecuting {
		t.Fatalf("Execute() result=%#v error=%v", first, err)
	}

	exhausted, err := fixture.service.Exhaust(context.Background(), "run-123", "execute")
	if err != nil || exhausted.Status != run.StatusFailed || exhausted.Retryable {
		t.Fatalf("Exhaust() result=%#v error=%v", exhausted, err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	if record.Failure == nil || record.Failure.CauseCode != "retries_exhausted" {
		t.Fatalf("exhausted failure = %#v", record.Failure)
	}
	again, err := fixture.service.Exhaust(context.Background(), "run-123", "execute")
	if err != nil || again.Status != run.StatusFailed {
		t.Fatalf("Exhaust(second) result=%#v error=%v", again, err)
	}
}

func TestExecuteFinalizesExpiredRunningAttemptWithoutRerunningAgent(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.executeLease = time.Minute
	if _, started, err := fixture.service.beginStage(context.Background(), "run-123", "execute", run.StatusExecuting); err != nil || !started {
		t.Fatalf("beginStage() started=%t error=%v", started, err)
	}
	fixture.executor.SetState(executor.AttemptID{
		RunID: "run-123", Stage: "execute", Attempt: 1,
		StartedAt: time.Unix(100, 0).UTC(), Purpose: executor.PurposeAgent,
	}, executor.StateRunning)
	clock.Advance(2 * time.Minute)
	agentCalls := 0
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		agentCalls++
		return runner.ExecutionResult{}, nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err == nil || result.Status != run.StatusFailed || result.FailureClass != run.FailureInternal || result.Retryable {
		t.Fatalf("Execute(recovery) result=%#v error=%v", result, err)
	}
	if fixture.workspaces.calls != 0 || agentCalls != 0 {
		t.Fatalf("recovery side effects workspaces=%d agent=%d", fixture.workspaces.calls, agentCalls)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	if record.Failure == nil || record.Failure.CauseCode != "ambiguous_attempt" ||
		record.Stages[len(record.Stages)-1].Status != run.StageFailed {
		t.Fatalf("recovery record = %#v", record)
	}
}

func TestExecuteRecoveryRetriesOnlyConclusiveNonStart(t *testing.T) {
	for _, state := range []executor.State{executor.StateAbsent, executor.StateCreated} {
		t.Run(string(state), func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			clock := newMutableClock(time.Unix(100, 0).UTC())
			fixture.service.clock = clock.Now
			fixture.service.executeLease = time.Minute
			if _, started, err := fixture.service.beginStage(
				context.Background(), "run-123", "execute", run.StatusExecuting,
			); err != nil || !started {
				t.Fatalf("beginStage() started=%t error=%v", started, err)
			}
			priorAgent := executor.AttemptID{
				RunID: "run-123", Stage: "execute", Attempt: 1,
				StartedAt: time.Unix(100, 0).UTC(), Purpose: executor.PurposeAgent,
			}
			if state != executor.StateAbsent {
				fixture.executor.SetState(priorAgent, state)
			}
			fixture.executor.SetBeforeExecute(func(_ context.Context, request executor.Request) {
				stdout := []byte("agent completed")
				if request.Attempt.Purpose == executor.PurposeProbe {
					stdout = []byte("codex 0.144.5")
				}
				fixture.executor.SetResult(request.Attempt, executor.Result{
					Created: true, Started: true, Completed: true, Stdout: stdout,
					SafeFacts: portableSafeFacts(request.Profile),
				}, nil)
			})
			fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
				return capturedChange(), nil
			}
			clock.Advance(2 * time.Minute)

			result, err := fixture.service.Execute(context.Background(), "run-123")
			if err != nil || result.Artifact == nil || result.Status != run.StatusExecuting {
				t.Fatalf("Execute(recovery) result=%#v error=%v", result, err)
			}
			requests := fixture.executor.Requests()
			if len(requests) == 0 || requests[len(requests)-1].Attempt.Purpose != executor.PurposeAgent ||
				requests[len(requests)-1].Attempt.Attempt != 2 {
				t.Fatalf("recovery executor requests = %#v", requests)
			}
			inspected, err := fixture.executor.Inspect(context.Background(), priorAgent)
			if err != nil || inspected != executor.StateDestroyed {
				t.Fatalf("prior agent state = %q, %v, want destroyed", inspected, err)
			}
		})
	}
}

func TestExecuteRecoveryTreatsPostStartAndUnknownAsAmbiguous(t *testing.T) {
	tests := []struct {
		name         string
		state        executor.State
		durableStart bool
	}{
		{name: "running", state: executor.StateRunning},
		{name: "completed", state: executor.StateCompleted},
		{name: "unknown", state: executor.StateUnknown},
		{name: "missing after durable start", state: executor.StateAbsent, durableStart: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := resolvedServiceFixture(t)
			clock := newMutableClock(time.Unix(100, 0).UTC())
			fixture.service.clock = clock.Now
			fixture.service.executeLease = time.Minute
			if _, started, err := fixture.service.beginStage(
				context.Background(), "run-123", "execute", run.StatusExecuting,
			); err != nil || !started {
				t.Fatalf("beginStage() started=%t error=%v", started, err)
			}
			priorAgent := executor.AttemptID{
				RunID: "run-123", Stage: "execute", Attempt: 1,
				StartedAt: time.Unix(100, 0).UTC(), Purpose: executor.PurposeAgent,
			}
			if test.state != executor.StateAbsent {
				fixture.executor.SetState(priorAgent, test.state)
			}
			if test.durableStart {
				record, err := fixture.runs.Store.Load(context.Background(), "run-123")
				if err != nil {
					t.Fatal(err)
				}
				latest, _ := latestStage(record, "execute")
				latest.Evidence = map[string]string{"agent_started": "true"}
				record, err = run.UpsertStage(record, latest)
				if err != nil {
					t.Fatal(err)
				}
				record.UpdatedAt = clock.Now()
				if _, err := fixture.runs.Store.Save(context.Background(), record, record.Version); err != nil {
					t.Fatal(err)
				}
			}
			clock.Advance(2 * time.Minute)

			result, err := fixture.service.Execute(context.Background(), "run-123")
			if err == nil || result.Status != run.StatusFailed || result.Retryable {
				t.Fatalf("Execute(recovery) result=%#v error=%v", result, err)
			}
			record, _ := fixture.runs.Store.Load(context.Background(), "run-123")
			if record.Failure == nil || record.Failure.CauseCode != "ambiguous_attempt" ||
				len(fixture.executor.Requests()) != 0 {
				t.Fatalf("ambiguous recovery record=%#v requests=%#v", record, fixture.executor.Requests())
			}
		})
	}
}

func TestExecuteExpiredRunningAttemptPreservesCheckpointedArtifact(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.executeLease = time.Minute
	if _, started, err := fixture.service.beginStage(context.Background(), "run-123", "execute", run.StatusExecuting); err != nil || !started {
		t.Fatalf("beginStage() started=%t error=%v", started, err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	reference := artifact.Reference{RunID: "run-123", Digest: strings.Repeat("a", 64), Size: 1024}
	record.Artifact = &reference
	if _, err := fixture.runs.Save(context.Background(), record, record.Version); err != nil {
		t.Fatalf("checkpoint artifact: %v", err)
	}
	if _, err := fixture.service.Execute(context.Background(), "run-123"); !errors.Is(err, ErrPhaseInProgress) {
		t.Fatalf("Execute(fresh checkpoint) error = %v, want %v", err, ErrPhaseInProgress)
	}
	clock.Advance(2 * time.Minute)

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if err != nil || result.Status != run.StatusExecuting || result.Artifact == nil || *result.Artifact != reference {
		t.Fatalf("Execute(recovery) result=%#v error=%v", result, err)
	}
	recovered, _ := fixture.runs.Load(context.Background(), "run-123")
	latest, _ := latestStage(recovered, "execute")
	if latest.Status != run.StageSucceeded || recovered.Failure != nil || len(fixture.executor.Requests()) != 0 {
		t.Fatalf("checkpoint recovery record=%#v requests=%#v", recovered, fixture.executor.Requests())
	}
}

func TestExecuteArtifactCheckpointSurvivesFinalPersistenceCrash(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.executeLease = time.Minute
	agentCalls := 0
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		agentCalls++
		return runner.ExecutionResult{Output: "changed", ExitCode: 0, Started: true, Completed: true}, nil
	}
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}
	fixture.runs.saveCalls = 0
	finalPersistErr := errors.New("worker lost before final run persistence")
	fixture.runs.saveErrors = map[int]error{5: finalPersistErr}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if !errors.Is(err, finalPersistErr) {
		t.Fatalf("Execute() result=%#v error=%v, want final persistence failure", result, err)
	}
	record, _ := fixture.runs.Store.Load(context.Background(), "run-123")
	if record.Artifact == nil || record.Stages[len(record.Stages)-1].Status != run.StageRunning {
		t.Fatalf("crash checkpoint record = %#v", record)
	}
	checkpoint := *record.Artifact

	fixture.runs.saveErrors = nil
	clock.Advance(2 * time.Minute)
	recovered := *fixture.service
	recovery, err := recovered.Execute(context.Background(), "run-123")
	if err != nil || recovery.Status != run.StatusExecuting || recovery.Artifact == nil ||
		*recovery.Artifact != checkpoint || agentCalls != 1 {
		t.Fatalf("Execute(recovery) result=%#v error=%v agentCalls=%d", recovery, err, agentCalls)
	}
}

func TestExhaustWaitsForFreshRunningAttemptThenFailClosesExpiredLease(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.executeLease = time.Minute
	if _, started, err := fixture.service.beginStage(context.Background(), "run-123", "execute", run.StatusExecuting); err != nil || !started {
		t.Fatalf("beginStage() started=%t error=%v", started, err)
	}

	fresh, err := fixture.service.Exhaust(context.Background(), "run-123", "execute")
	if !errors.Is(err, ErrPhaseInProgress) || fresh.Status != run.StatusExecuting || !fresh.Retryable {
		t.Fatalf("Exhaust(fresh) result=%#v error=%v", fresh, err)
	}
	record, _ := fixture.runs.Load(context.Background(), "run-123")
	if !runningStageAttempt(record, "execute", 1) {
		t.Fatalf("fresh Exhaust changed running stage: %#v", record.Stages)
	}

	clock.Advance(2 * time.Minute)
	result, err := fixture.service.Exhaust(context.Background(), "run-123", "execute")
	if err != nil || result.Status != run.StatusFailed || result.Retryable {
		t.Fatalf("Exhaust(expired) result=%#v error=%v", result, err)
	}
	record, _ = fixture.runs.Load(context.Background(), "run-123")
	if record.Failure == nil || record.Failure.CauseCode != "ambiguous_attempt" {
		t.Fatalf("Exhaust() failure = %#v", record.Failure)
	}
}

func TestExecuteTakeoverDuringAgentFencesStaleWorkerBeforeVerificationAndArtifact(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.executeLease = time.Minute
	agentStarted := make(chan struct{})
	releaseAgent := make(chan struct{})
	fixture.agent.run = func(context.Context, runner.RunRequest) (runner.ExecutionResult, error) {
		close(agentStarted)
		<-releaseAgent
		return runner.ExecutionResult{Output: "stale", Started: true, Completed: true}, nil
	}
	verifyCalls := 0
	fixture.profile.result.Commands = []verification.Command{{
		Name: "must-not-run", Directory: "/tmp/workspace", Executable: "git",
		Timeout: time.Minute, Required: true,
	}}
	fixture.verifier.run = func(context.Context, verification.Command, map[string]string) verification.Result {
		verifyCalls++
		return verification.Result{Passed: true}
	}
	captureCalls := 0
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		captureCalls++
		return capturedChange(), nil
	}
	type response struct {
		result PhaseResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := fixture.service.Execute(context.Background(), "run-123")
		done <- response{result: result, err: err}
	}()
	<-agentStarted
	clock.Advance(2 * time.Minute)
	taken, err := fixture.service.Exhaust(context.Background(), "run-123", "execute")
	if err != nil || taken.Status != run.StatusFailed {
		t.Fatalf("Exhaust(takeover) result=%#v error=%v", taken, err)
	}
	close(releaseAgent)

	stale := <-done
	if stale.err == nil || stale.result.Status != run.StatusFailed {
		t.Fatalf("stale Execute() result=%#v error=%v", stale.result, stale.err)
	}
	if verifyCalls != 0 || captureCalls != 0 || len(fixture.artifacts.Snapshot().Saves) != 0 {
		t.Fatalf("stale side effects verification=%d capture=%d artifacts=%d",
			verifyCalls, captureCalls, len(fixture.artifacts.Snapshot().Saves))
	}
	record, _ := fixture.runs.Store.Load(context.Background(), "run-123")
	if record.Failure == nil || record.Failure.CauseCode != "ambiguous_attempt" || record.Artifact != nil {
		t.Fatalf("terminal takeover record = %#v", record)
	}
}

func TestExecuteTakeoverBeforeArtifactSaveProducesNoOrphanArtifact(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.executeLease = time.Minute
	var takeover PhaseResult
	var takeoverErr error
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		clock.Advance(2 * time.Minute)
		takeover, takeoverErr = fixture.service.Exhaust(context.Background(), "run-123", "execute")
		return capturedChange(), nil
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if takeoverErr != nil || takeover.Status != run.StatusFailed {
		t.Fatalf("Exhaust(takeover) result=%#v error=%v", takeover, takeoverErr)
	}
	if err == nil || result.Status != run.StatusFailed {
		t.Fatalf("stale Execute() result=%#v error=%v", result, err)
	}
	if saves := fixture.artifacts.Snapshot().Saves; len(saves) != 0 {
		t.Fatalf("stale Execute persisted orphan artifacts: %#v", saves)
	}
}

func TestExecuteArtifactWriteSubleaseBlocksTakeoverAndExhaustUntilCheckpoint(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.executeLease = time.Minute
	fixture.service.artifactSaveTimeout = 5 * time.Minute
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	saveDeadline := make(chan time.Time, 1)
	fixture.service.artifacts = &artifactStoreFunc{
		Store: fixture.artifacts,
		save: func(ctx context.Context, bundle artifact.Bundle) (artifact.Reference, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				return artifact.Reference{}, errors.New("artifact Save context is unbounded")
			}
			saveDeadline <- deadline
			close(saveStarted)
			<-releaseSave
			return fixture.artifacts.Save(ctx, bundle)
		},
	}

	type response struct {
		result PhaseResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := fixture.service.Execute(context.Background(), "run-123")
		done <- response{result: result, err: err}
	}()
	<-saveStarted

	record, err := fixture.runs.Store.Load(context.Background(), "run-123")
	if err != nil {
		t.Fatal(err)
	}
	owned, found := latestStage(record, "execute")
	if !found || record.ArtifactWriteLease == nil ||
		record.ArtifactWriteLease.Attempt != owned.Attempts ||
		!record.ArtifactWriteLease.StartedAt.Equal(owned.StartedAt) ||
		!record.ArtifactWriteLease.Deadline.After(clock.Now().Add(fixture.service.artifactSaveTimeout)) {
		t.Fatalf("artifact-write sublease = %#v, execute = %#v", record.ArtifactWriteLease, owned)
	}
	if deadline := <-saveDeadline; time.Until(deadline) > fixture.service.artifactSaveTimeout ||
		time.Until(deadline) < fixture.service.artifactSaveTimeout-time.Second {
		t.Fatalf("artifact Save deadline = %v, timeout = %v", deadline, fixture.service.artifactSaveTimeout)
	}

	clock.Advance(2 * time.Minute)
	exhausted, exhaustErr := fixture.service.Exhaust(context.Background(), "run-123", "execute")
	if !errors.Is(exhaustErr, ErrPhaseInProgress) || exhausted.Status != run.StatusExecuting {
		t.Fatalf("Exhaust(during Save) result=%#v error=%v", exhausted, exhaustErr)
	}
	taken, takeoverErr := fixture.service.Execute(context.Background(), "run-123")
	if !errors.Is(takeoverErr, ErrPhaseInProgress) || taken.Status != run.StatusExecuting {
		t.Fatalf("Execute(takeover during Save) result=%#v error=%v", taken, takeoverErr)
	}

	close(releaseSave)
	completed := <-done
	if completed.err != nil || completed.result.Artifact == nil ||
		completed.result.Status != run.StatusExecuting {
		t.Fatalf("Execute() result=%#v error=%v", completed.result, completed.err)
	}
	record, err = fixture.runs.Store.Load(context.Background(), "run-123")
	if err != nil {
		t.Fatal(err)
	}
	if record.ArtifactWriteLease != nil || record.Artifact == nil ||
		*record.Artifact != *completed.result.Artifact ||
		len(fixture.artifacts.Snapshot().Saves) != 1 {
		t.Fatalf("checkpoint record=%#v saves=%#v", record, fixture.artifacts.Snapshot().Saves)
	}
}

func TestExecutePostSaveOwnershipLossReturnsDurableOutcomeNotCheckpointFailure(t *testing.T) {
	fixture := resolvedServiceFixture(t)
	clock := newMutableClock(time.Unix(100, 0).UTC())
	fixture.service.clock = clock.Now
	fixture.service.executeLease = time.Minute
	fixture.service.artifactSaveTimeout = 5 * time.Minute
	fixture.capturer.capture = func(context.Context, gitcapture.Request) (gitcapture.Result, error) {
		return capturedChange(), nil
	}

	var exhausted PhaseResult
	var exhaustErr error
	fixture.service.artifacts = &artifactStoreFunc{
		Store: fixture.artifacts,
		save: func(ctx context.Context, bundle artifact.Bundle) (artifact.Reference, error) {
			reference, err := fixture.artifacts.Save(ctx, bundle)
			if err != nil {
				return artifact.Reference{}, err
			}
			record, loadErr := fixture.runs.Store.Load(context.Background(), "run-123")
			if loadErr != nil {
				return artifact.Reference{}, loadErr
			}
			clock.Advance(record.ArtifactWriteLease.Deadline.Sub(clock.Now()) + time.Second)
			exhausted, exhaustErr = fixture.service.Exhaust(context.Background(), "run-123", "execute")
			return reference, nil
		},
	}

	result, err := fixture.service.Execute(context.Background(), "run-123")
	if exhaustErr != nil || exhausted.Status != run.StatusFailed {
		t.Fatalf("Exhaust(after Save) result=%#v error=%v", exhausted, exhaustErr)
	}
	if err == nil || result.Status != run.StatusFailed ||
		result.FailureClass != run.FailureInternal || result.Artifact != nil {
		t.Fatalf("Execute() result=%#v error=%v", result, err)
	}
	if strings.Contains(err.Error(), "artifact_checkpoint") ||
		strings.Contains(err.Error(), "checkpoint artifact reference failed") {
		t.Fatalf("ownership loss misclassified as checkpoint corruption: %v", err)
	}
	record, loadErr := fixture.runs.Store.Load(context.Background(), "run-123")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if record.Failure == nil || record.Failure.CauseCode != "ambiguous_attempt" ||
		record.Artifact != nil || record.ArtifactWriteLease != nil ||
		len(fixture.artifacts.Snapshot().Saves) != 1 {
		t.Fatalf("durable ownership-loss boundary record=%#v saves=%#v",
			record, fixture.artifacts.Snapshot().Saves)
	}
}

func resolvedServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := newServiceFixture(t)
	fixture.profile.result = repository.ProfileResult{Facts: map[string]string{"base_sha": fixture.resolver.revision.SHA}}
	if _, err := fixture.service.Resolve(context.Background(), validRawInput("Change docs", "execute")); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	return fixture
}

type workspaceProfile struct{}

func (*workspaceProfile) Name() string { return "generic" }
func (*workspaceProfile) Inspect(_ context.Context, request repository.ProfileRequest) (repository.ProfileResult, error) {
	return repository.ProfileResult{
		Facts: map[string]string{
			"zeta": "last", "alpha": "first",
			"workspace": request.Workspace,
		},
		Warnings: []string{"optional dependency unavailable"},
		Modules:  []string{"module-a", "module-b"},
		Commands: []verification.Command{
			{Name: "module-a", Directory: filepath.Join(request.Workspace, "module-a"), Executable: "git", Args: []string{"status", "--short"}, Timeout: time.Minute, Required: true},
			{Name: "module-b", Directory: filepath.Join(request.Workspace, "module-b"), Executable: "git", Args: []string{"status", "--short"}, Timeout: time.Minute, Required: true},
		},
	}, nil
}

type writingAgent struct {
	mu       sync.Mutex
	requests []runner.RunRequest
}

func (a *writingAgent) Run(_ context.Context, request runner.RunRequest) (runner.ExecutionResult, error) {
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.mu.Unlock()
	if err := os.WriteFile(filepath.Join(request.WorkspacePath, "changed.txt"), []byte("changed\n"), 0o644); err != nil {
		return runner.ExecutionResult{}, err
	}
	return runner.ExecutionResult{
		Output: "updated changed.txt in " + request.WorkspacePath, Transcript: `{"type":"item.completed"}`,
		ExitCode: 0, Started: true, Completed: true,
	}, nil
}
func (a *writingAgent) Requests() []runner.RunRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]runner.RunRequest(nil), a.requests...)
}

type recordingVerifier struct {
	mu       sync.Mutex
	commands []verification.Command
}

func (v *recordingVerifier) Run(_ context.Context, command verification.Command, _ map[string]string) verification.Result {
	v.mu.Lock()
	v.commands = append(v.commands, command)
	v.mu.Unlock()
	return verification.Result{Command: command, Output: "checked " + command.Directory, Passed: true}
}
func (v *recordingVerifier) Commands() []verification.Command {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]verification.Command(nil), v.commands...)
}

func rawForRepository(repositoryURI string) json.RawMessage {
	value := map[string]any{
		"idempotency_key": "real-worktree", "task_description": "Add changed.txt",
		"repository_uri": repositoryURI, "base_ref": "HEAD",
		"tags":           map[string]string{"user_id": "guilhermecastro", "app_id": "araihu-paje"},
		"worker_profile": "codex-go@1",
		"profile":        "generic",
		"checks": []map[string]any{{
			"name": "git status", "directory": ".", "executable": "git",
			"args": []string{"status", "--short"}, "timeout": "1m", "required": true,
		}},
		"publication": map[string]any{"mode": "artifact"},
	}
	raw, _ := json.Marshal(value)
	return raw
}

func createGitSource(t *testing.T) (string, string) {
	t.Helper()
	source := t.TempDir()
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "paje@example.test")
	runGit(t, source, "config", "user.name", "Paje Test")
	if err := os.MkdirAll(filepath.Join(source, "module-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "module-b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "module-a", "base.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "module-b", "base.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "base")
	return source, strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func assertDirectoryEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", directory, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%q entries = %#v, want empty", directory, entries)
	}
}

func capturedChange() gitcapture.Result {
	return gitcapture.Result{
		Patch:   []byte("diff --git a/changed.txt b/changed.txt\n"),
		Changes: []artifact.Change{{Path: "changed.txt", Status: "A", NewMode: "100644"}},
		TreeSHA: "tree-sha",
	}
}

type artifactStoreFunc struct {
	artifact.Store
	save func(context.Context, artifact.Bundle) (artifact.Reference, error)
}

func (s *artifactStoreFunc) Save(ctx context.Context, bundle artifact.Bundle) (artifact.Reference, error) {
	return s.save(ctx, bundle)
}

func structPublisherResult() publisher.Result { return publisher.Result{} }
