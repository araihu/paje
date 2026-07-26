package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestValidateProfileRejectsNoncanonicalSnapshotOrder(t *testing.T) {
	target, err := New(Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	profile := hostProfile(t)
	profile.Digest = ""
	profile.Tools = []workerprofile.Tool{
		{Name: "go", Version: "1.26.1", Probe: workerprofile.Probe{Executable: "go", Args: []string{"version"}, OutputContains: "go1.26.1"}},
		{Name: "git", Version: "2.53.0", Probe: workerprofile.Probe{Executable: "git", Args: []string{"--version"}, OutputContains: "2.53.0"}},
	}
	profile, err = workerprofile.Canonicalize(profile)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(profile.Tools)
	if err := target.ValidateProfile(profile); err == nil {
		t.Fatal("noncanonical profile order accepted")
	}
}

func TestNilReceiverLifecycleMethodsFailClosed(t *testing.T) {
	var target *Executor
	attempt := hostAttempt(0)
	if state, err := target.Inspect(context.Background(), attempt); err == nil || state != executor.StateUnknown {
		t.Fatalf("Inspect() = %q, %v", state, err)
	}
	if err := target.Cancel(context.Background(), attempt); err == nil {
		t.Fatal("nil Cancel() succeeded")
	}
	if err := target.Destroy(context.Background(), attempt); err == nil {
		t.Fatal("nil Destroy() succeeded")
	}
}

func TestDestroyedAttemptHistoryIsBoundedAndPreservesRecentIdentity(t *testing.T) {
	const historyLimit = 1024
	target, err := New(Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	active := hostAttempt(historyLimit + 10)
	target.attempts[active.Key()] = &attemptRecord{state: executor.StateRunning, done: make(chan struct{})}

	var first, recent executor.AttemptID
	for index := range historyLimit + 5 {
		attempt := hostAttempt(index)
		if index == 0 {
			first = attempt
		}
		recent = attempt
		done := make(chan struct{})
		close(done)
		target.attempts[attempt.Key()] = &attemptRecord{state: executor.StateCompleted, done: done}
		if err := target.Destroy(context.Background(), attempt); err != nil {
			t.Fatal(err)
		}
	}

	target.mu.Lock()
	destroyed := 0
	for _, record := range target.attempts {
		if record.state == executor.StateDestroyed {
			destroyed++
		}
	}
	target.mu.Unlock()
	if destroyed > historyLimit {
		t.Fatalf("destroyed tombstones = %d, want at most %d", destroyed, historyLimit)
	}
	if state, err := target.Inspect(context.Background(), first); err != nil || state != executor.StateAbsent {
		t.Fatalf("oldest Inspect() = %q, %v", state, err)
	}
	if state, err := target.Inspect(context.Background(), recent); err != nil || state != executor.StateDestroyed {
		t.Fatalf("recent Inspect() = %q, %v", state, err)
	}
	if err := target.Destroy(context.Background(), recent); err != nil {
		t.Fatal(err)
	}
	if state, err := target.Inspect(context.Background(), active); err != nil || state != executor.StateRunning {
		t.Fatalf("active Inspect() = %q, %v", state, err)
	}

	request := hostRequest(t)
	request.Attempt = recent
	if _, err := target.Execute(context.Background(), request); !errors.Is(err, executor.ErrAttemptExists) {
		t.Fatalf("recent deterministic identity reuse error = %v", err)
	}
}

func hostAttempt(index int) executor.AttemptID {
	return executor.AttemptID{
		RunID: fmt.Sprintf("run-tombstone-%04d", index), Stage: "execute", Attempt: 1,
		StartedAt: time.Unix(100, int64(index)).UTC(), Purpose: executor.PurposeVerification,
	}
}

func TestHostRejectsOCISecretsAndProductionMode(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("disabled host executor constructed")
	}
	if _, err := New(Config{Enabled: true, ProductionOnly: true}); err == nil {
		t.Fatal("production host executor enabled")
	}
	target, err := New(Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	request := hostRequest(t)
	request.Profile = ociProfile(t, nil)
	if _, err := target.Execute(context.Background(), request); err == nil {
		t.Fatal("OCI request executed on host")
	}
	request.Destroy()

	materialization, err := secret.NewValueMaterialization(
		workerprofile.DeliveryFile, "/run/paje/secrets/token", []byte("secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	request = hostRequest(t)
	request.Profile = ociProfile(t, []workerprofile.SecretRequirement{{
		Capability: "workload.token", BindingRevision: 1, Stage: workerprofile.StageAgent,
		Delivery: workerprofile.DeliveryFile, Target: "/run/paje/secrets/token", Required: true,
	}})
	request.Secrets = []secret.Materialization{materialization}
	if _, err := target.Execute(context.Background(), request); err == nil {
		t.Fatal("secret-bearing request executed on host")
	}
	request.Destroy()
}

func TestValidateProfileReportsDevelopmentOnlySafeFacts(t *testing.T) {
	target, err := New(Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ValidateProfile(hostProfile(t)); err != nil {
		t.Fatal(err)
	}
	request := hostRequest(t)
	request.Command.Executable = "missing-host-executable"
	result, err := target.Execute(context.Background(), request)
	if err == nil || !result.Created || result.Started || result.SafeFacts["certified"] != "false" || result.SafeFacts["isolated"] != "false" {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	request.Destroy()
}

func hostRequest(t *testing.T) executor.Request {
	t.Helper()
	return executor.Request{
		Attempt: executor.AttemptID{
			RunID: "run-host", Stage: "execute", Attempt: 1,
			StartedAt: time.Unix(100, 1).UTC(), Purpose: executor.PurposeVerification,
		},
		Profile: hostProfile(t),
		Command: executor.Command{
			Executable: "paje-host-helper", Directory: executor.SandboxWorkspaceRoot,
		},
		Workspace:   executor.Workspace{HostPath: t.TempDir(), SandboxPath: executor.SandboxWorkspaceRoot, Writable: true},
		Environment: map[string]string{"PATH": "/usr/bin:/bin"},
		Timeout:     5 * time.Second, OutputLimit: 1024,
	}
}

func hostProfile(t *testing.T) workerprofile.Snapshot {
	t.Helper()
	profile, err := workerprofile.Canonicalize(workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "host-dev", Revision: 1},
		Runtime:    workerprofile.Runtime{Kind: workerprofile.RuntimeHost},
		Harness:    workerprofile.Harness{ID: "codex", Version: "0.144.5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func ociProfile(t *testing.T, requirements []workerprofile.SecretRequirement) workerprofile.Snapshot {
	t.Helper()
	profile, err := workerprofile.Canonicalize(workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "oci-test", Revision: 1},
		Runtime: workerprofile.Runtime{
			Kind: workerprofile.RuntimeOCI, Image: "example.invalid/worker@sha256:" + strings.Repeat("a", 64),
			Platform: "linux/amd64", Network: workerprofile.NetworkNone, ReadOnlyRoot: true,
		},
		Resources: workerprofile.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 64},
		Harness:   workerprofile.Harness{ID: "codex", Version: "0.144.5"},
		Secrets:   requirements,
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}
