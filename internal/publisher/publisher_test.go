package publisher

import (
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/template"
	"github.com/araihu/paje/internal/verification"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestValidatePortableBindsFrozenVerificationPlanAttemptsAndEnvironmentUnion(t *testing.T) {
	plan := []verification.Result{{
		Command: verification.Command{
			Name: "git status", Directory: ".", Executable: "git",
			Args: []string{"status"}, EnvironmentKeys: []string{}, Timeout: time.Minute, Required: true,
		},
		Passed: true,
	}}
	goPlan := []verification.Result{{
		Command: verification.Command{
			Name: "go test", Directory: ".", Executable: "go",
			Args: []string{"test", "./..."}, EnvironmentKeys: []string{"GOWORK"}, Timeout: time.Minute, Required: true,
		},
		Passed: true,
	}}
	genericGoPlan := []verification.Result{{
		Command: verification.Command{
			Name: "generic go", Directory: ".", Executable: "go",
			Args: []string{"version"}, EnvironmentKeys: []string{}, Timeout: time.Minute, Required: true,
		},
		Passed: true,
	}}
	tests := map[string]struct {
		plan     []verification.Result
		attempts []artifact.AttemptEvidence
		keys     artifact.EnvironmentKeyList
		mutate   func(*Request)
		valid    bool
	}{
		"no scheduled attempt and empty union": {
			plan: []verification.Result{}, keys: artifact.EnvironmentKeyList{}, valid: true,
		},
		"missing frozen plan": {
			plan: []verification.Result{}, keys: artifact.EnvironmentKeyList{},
			mutate: func(request *Request) {
				request.Verification = nil
			},
		},
		"unscheduled confirmed empty": {
			plan: []verification.Result{}, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{},
		},
		"confirmed nonempty exact union": {
			plan: plan, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"}, valid: true,
		},
		"stripped confirmed union": {
			plan: plan, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{},
		},
		"generic go exact baseline union": {
			plan: genericGoPlan, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"}, valid: true,
		},
		"confirmed Go declaration exact union": {
			plan: goPlan, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{"GOWORK", "HOME", "PATH", "TMPDIR"}, valid: true,
		},
		"confirmed Go declaration partial": {
			plan: goPlan, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"},
		},
		"scheduled attempt missing": {
			plan: plan, keys: artifact.EnvironmentKeyList{},
		},
		"partial": {
			plan: plan, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{"HOME", "PATH"},
		},
		"extra": {
			plan: plan, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR", "TOKEN"},
		},
		"inverse unconfirmed historical baseline": {
			plan: plan, attempts: []artifact.AttemptEvidence{unconfirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"},
		},
		"changed sequence": {
			plan: plan, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(2)},
			keys: artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"},
		},
		"changed command": {
			plan: plan, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"},
			mutate: func(request *Request) {
				request.Verification[0].Command.Executable = "go"
			},
		},
		"changed declaration": {
			plan: plan, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"},
			mutate: func(request *Request) {
				request.Verification[0].Command.EnvironmentKeys = []string{"GOWORK"}
			},
		},
		"missing declaration": {
			plan: []verification.Result{{Command: verification.Command{
				Name: "git status", Directory: ".", Executable: "git",
				Args: []string{"status"}, Timeout: time.Minute, Required: true,
			}, Passed: true}},
			attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys:     artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"},
		},
		"duplicate declaration": {
			plan: []verification.Result{{Command: verification.Command{
				Name: "go test", Directory: ".", Executable: "go", Args: []string{"test", "./..."},
				EnvironmentKeys: []string{"GOWORK", "GOWORK"}, Timeout: time.Minute, Required: true,
			}, Passed: true}},
			attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys:     artifact.EnvironmentKeyList{"GOWORK", "HOME", "PATH", "TMPDIR"},
		},
		"unsorted unsupported declaration": {
			plan: []verification.Result{{Command: verification.Command{
				Name: "go test", Directory: ".", Executable: "go", Args: []string{"test", "./..."},
				EnvironmentKeys: []string{"TOKEN", "GOWORK"}, Timeout: time.Minute, Required: true,
			}, Passed: true}},
			attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys:     artifact.EnvironmentKeyList{"GOWORK", "HOME", "PATH", "TMPDIR"},
		},
		"replay plan drift": {
			plan: plan, attempts: []artifact.AttemptEvidence{confirmedVerificationAttempt(1)},
			keys: artifact.EnvironmentKeyList{"HOME", "PATH", "TMPDIR"},
			mutate: func(request *Request) {
				request.Verification = append(request.Verification, request.Verification[0])
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			request := portableVerificationRequest(t, test.plan, test.attempts, test.keys)
			if test.mutate != nil {
				test.mutate(&request)
			}
			err := request.ValidatePortable()
			if test.valid && err != nil {
				t.Fatalf("ValidatePortable() error = %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("ValidatePortable() error = nil")
			}
		})
	}
}

func TestCloneVerificationProtectsFrozenPlanSlices(t *testing.T) {
	source := []verification.Result{{Command: verification.Command{
		Executable: "go", Args: []string{"test", "./..."},
		EnvironmentKeys: []string{"GOWORK"},
	}}}

	cloned := CloneVerification(source)
	cloned[0].Command.Args[0] = "vet"
	cloned[0].Command.EnvironmentKeys[0] = "TOKEN"

	if source[0].Command.Args[0] != "test" || source[0].Command.EnvironmentKeys[0] != "GOWORK" {
		t.Fatalf("CloneVerification() mutated source = %#v", source)
	}
}

func portableVerificationRequest(
	t *testing.T,
	plan []verification.Result,
	verificationAttempts []artifact.AttemptEvidence,
	verificationKeys artifact.EnvironmentKeyList,
) Request {
	t.Helper()
	profile, err := workerprofile.Canonicalize(workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "codex-go", Revision: 1},
		Runtime: workerprofile.Runtime{
			Kind: workerprofile.RuntimeOCI, Image: "example.test/worker@sha256:" + strings.Repeat("a", 64),
			Platform: "linux/amd64", Network: workerprofile.NetworkNone, ReadOnlyRoot: true,
		},
		Resources: workerprofile.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 64},
		Harness:   workerprofile.Harness{ID: "codex", Version: "0.144.5"},
		Secrets: []workerprofile.SecretRequirement{{
			Capability: "harness.codex-auth", BindingRevision: 7, Stage: workerprofile.StageAgent,
			Delivery: workerprofile.DeliveryDirectory, Target: "/run/paje/secrets/codex", Required: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentAttempt := artifact.AttemptEvidence{
		ID: executor.AttemptID{
			RunID: "run-123", Stage: "execute", Attempt: 1,
			StartedAt: time.Unix(100, 0).UTC(), Purpose: executor.PurposeAgent,
		},
		Created: true, Started: true, Completed: true, Destroyed: true,
	}
	attempts := artifact.AttemptEvidenceList{agentAttempt}
	attempts = append(attempts, verificationAttempts...)
	tools := artifact.ToolEvidenceList{}
	agentKeys := artifact.EnvironmentKeyList{"CODEX_HOME", "HOME", "PATH", "TMPDIR"}
	plan = append([]verification.Result{}, plan...)
	member := verificationMember(t, plan)
	return Request{
		RunID: "run-123", Repository: "https://example.test/repository.git",
		BaseSHA: strings.Repeat("a", 40), TargetRef: "refs/heads/main",
		Branch: "paje/code-change/run-123",
		Artifact: artifact.Reference{
			RunID: "run-123", Digest: strings.Repeat("b", 64), Size: 1,
		},
		ArtifactManifest: artifact.Manifest{
			RunID: "run-123", Template: template.ID{Name: "code-change", Version: 1},
			Repository: "https://example.test/repository.git",
			BaseSHA:    strings.Repeat("a", 40), TreeSHA: strings.Repeat("c", 40),
			Changes: []artifact.Change{{Path: "changed.txt", Status: "M"}},
			Members: []artifact.Member{member},
		},
		WorkerProfile: profile,
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
			AgentEnvironmentKeys: &agentKeys, VerificationEnvironmentKeys: &verificationKeys,
		},
		Verification: plan,
		Title:        "Portable change",
	}
}

func confirmedVerificationAttempt(sequence int) artifact.AttemptEvidence {
	return artifact.AttemptEvidence{
		ID: executor.AttemptID{
			RunID: "run-123", Stage: "execute", Attempt: 1,
			StartedAt: time.Unix(100, 0).UTC(), Purpose: executor.PurposeVerification,
			Sequence: sequence,
		},
		Created: true, Started: true, Completed: true, Destroyed: true,
	}
}

func unconfirmedVerificationAttempt(sequence int) artifact.AttemptEvidence {
	attempt := confirmedVerificationAttempt(sequence)
	attempt.Started = false
	attempt.Completed = false
	return attempt
}

func verificationMember(t *testing.T, plan []verification.Result) artifact.Member {
	t.Helper()
	member, err := verificationPlanMember(plan, artifact.Manifest{
		RunID: "run-123", Template: template.ID{Name: "code-change", Version: 1},
		Repository: "https://example.test/repository.git",
		BaseSHA:    strings.Repeat("a", 40), TreeSHA: strings.Repeat("c", 40),
		Changes: []artifact.Change{{Path: "changed.txt", Status: "M"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return member
}
