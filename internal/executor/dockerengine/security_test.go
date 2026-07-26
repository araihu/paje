package dockerengine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/sandboxinit"
	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestContainerConfigExcludesSecretsAndDeclaredChildCommand(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := secretDockerRequest(t)
	defer request.Destroy()

	if _, err := target.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	created := api.created
	api.mu.Unlock()
	encoded, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"file-secret", "environment-secret", "directory-secret",
		`"codex"`, `"exec"`, "CODEX_HOME=", "WORKLOAD_TOKEN",
	} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("container configuration leaked %q: %s", forbidden, encoded)
		}
	}
	if len(created.HostConfig.Mounts) != 1 || len(created.HostConfig.Tmpfs) != 3 {
		t.Fatalf("mounts = %#v", created.HostConfig.Mounts)
	}
	for _, mounted := range created.HostConfig.Mounts {
		if strings.HasPrefix(mounted.Source, sandboxinit.SecretRoot) ||
			strings.HasPrefix(mounted.Target, sandboxinit.SecretRoot) {
			t.Fatalf("secret bind mount = %#v", mounted)
		}
	}
}

func TestPrivateArchiveContainsCommandAndPrivateSecretMaterial(t *testing.T) {
	request := secretDockerRequest(t)
	defer request.Destroy()
	archive, err := buildArchive(request)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Destroy()

	entries := readArchive(t, archive.Reader())
	commandEntry := entries["run/paje/command.json"]
	if commandEntry == nil {
		t.Fatal("command document missing")
	}
	var document sandboxinit.Document
	if err := json.Unmarshal(commandEntry.contents, &document); err != nil {
		t.Fatal(err)
	}
	if document.Command.Executable != "codex" ||
		!slices.Equal(document.Command.Args, []string{"exec", "task"}) ||
		document.Command.Environment["CODEX_HOME"] != "/home/paje" ||
		document.Environment["PATH"] != "/usr/local/bin:/usr/bin:/bin" {
		t.Fatalf("command document = %#v", document)
	}
	environmentFile := document.EnvironmentFiles["WORKLOAD_TOKEN"]
	if !strings.HasPrefix(environmentFile, sandboxinit.SecretRoot+"/environment/") {
		t.Fatalf("environment materialization path = %q", environmentFile)
	}
	encodedDocument := commandEntry.contents
	for _, forbidden := range []string{"file-secret", "environment-secret", "directory-secret"} {
		if bytes.Contains(encodedDocument, []byte(forbidden)) {
			t.Fatalf("command document leaked %q", forbidden)
		}
	}

	assertArchiveFile(t, entries, "run/paje/secrets/token", 0o400, "file-secret")
	assertArchiveFile(t, entries, strings.TrimPrefix(environmentFile, "/"), 0o400, "environment-secret")
	assertArchiveFile(t, entries, "run/paje/secrets/codex/auth.json", 0o600, "directory-secret")
	for name, entry := range entries {
		if entry.uid != sandboxUID || entry.gid != sandboxGID {
			t.Fatalf("%s ownership = %d:%d", name, entry.uid, entry.gid)
		}
		if entry.mode&0o077 != 0 {
			t.Fatalf("%s mode = %#o", name, entry.mode)
		}
	}
}

func TestSecretOutputIsSuppressedBeforeResultEscapes(t *testing.T) {
	api := newFakeEngine()
	api.attached = multiplexedOutput([]byte("prefix environment-secret suffix"), []byte("safe"))
	target := newExecutorForTest(t, api)
	request := secretDockerRequest(t)
	defer request.Destroy()

	result, err := target.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.SecretDetected || len(result.Stdout) != 0 || len(result.Stderr) != 0 {
		t.Fatalf("secret-bearing result = %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("environment-secret")) {
		t.Fatalf("serialized result leaked secret: %s", encoded)
	}
}

func TestProviderErrorsExposeOnlyStableDiagnostics(t *testing.T) {
	api := newFakeEngine()
	raw := errors.New("provider-detail unix:///var/run/docker.sock container-id")
	api.pingErr = raw
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()

	_, err := target.Execute(context.Background(), request)
	var providerError *executor.ProviderError
	if err == nil || !errors.As(err, &providerError) || !errors.Is(err, raw) {
		t.Fatalf("provider error = %v", err)
	}
	encoded, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, unsafe := range []string{"provider-detail", "/var/run/docker.sock", "container-id"} {
		if strings.Contains(err.Error(), unsafe) || bytes.Contains(encoded, []byte(unsafe)) {
			t.Fatalf("safe provider error leaked %q: %v %s", unsafe, err, encoded)
		}
	}
}

func TestAttemptLabelsBindEveryIdentityField(t *testing.T) {
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	got := attemptLabels(request.Attempt, resourceContainer)
	want := map[string]string{
		labelExecutor: "dockerengine-v1",
		labelResource: "container",
		labelKey:      request.Attempt.Key(),
		labelRunID:    "run-docker",
		labelStage:    "execute",
		labelAttempt:  "2",
		labelStarted:  "1970-01-01T00:01:40.000000123Z",
		labelPurpose:  "verification",
		labelSequence: "3",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("labels = %#v", got)
	}
	changed := request.Attempt
	changed.Sequence++
	if attemptLabels(changed, resourceContainer)[labelKey] == got[labelKey] ||
		resourceName(changed, resourceContainer) == resourceName(request.Attempt, resourceContainer) {
		t.Fatal("changed complete attempt identity reused provider ownership")
	}
}

func TestResourceDiscoveryRejectsMultipleExactMatches(t *testing.T) {
	for _, configure := range []func(*fakeEngine){
		func(api *fakeEngine) { api.extraContainers = 2 },
		func(api *fakeEngine) { api.extraNetworks = 2 },
	} {
		api := newFakeEngine()
		configure(api)
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkNone, nil)
		_, err := target.Execute(context.Background(), request)
		request.Destroy()
		var providerError *executor.ProviderError
		if err == nil || !errors.As(err, &providerError) ||
			providerError.CauseCode != "resource_conflict" {
			t.Fatalf("resource conflict = %v", err)
		}
	}
}

func TestInspectTreatsPostStartDisappearanceAsUnknown(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	if _, err := target.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.containerExists = false
	api.mu.Unlock()
	state, err := target.Inspect(context.Background(), request.Attempt)
	if err != nil || state != executor.StateUnknown {
		t.Fatalf("Inspect() after disappearance = %q, %v", state, err)
	}
}

func TestCancelTreatsKnownRunningDisappearanceAsAmbiguous(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	target.attempts[request.Attempt.Key()] = &attemptRecord{state: executor.StateRunning}

	err := target.Cancel(context.Background(), request.Attempt)
	var providerError *executor.ProviderError
	if err == nil || !errors.As(err, &providerError) ||
		providerError.Class != "internal" || providerError.CauseCode != "ambiguous_attempt" {
		t.Fatalf("Cancel() = %v", err)
	}
}

func TestCancelIsIdempotentForNonrunningContainerStates(t *testing.T) {
	for _, state := range []engineContainerState{
		engineContainerCreated,
		engineContainerExited,
		engineContainerDead,
	} {
		api := newFakeEngine()
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkNone, nil)
		api.containerExists = true
		api.containerState = state
		api.containerLabels = attemptLabels(request.Attempt, resourceContainer)
		if err := target.Cancel(context.Background(), request.Attempt); err != nil {
			t.Fatalf("Cancel(%q) = %v", state, err)
		}
		api.mu.Lock()
		stopCalls := api.stopCalls
		killCalls := api.killCalls
		api.mu.Unlock()
		request.Destroy()
		if stopCalls != 0 || killCalls != 0 {
			t.Fatalf("Cancel(%q) signaled stop %d kill %d", state, stopCalls, killCalls)
		}
	}
}

func TestLogDrainIsBoundedWhenReaderIgnoresClose(t *testing.T) {
	reader := &stuckReadCloser{release: make(chan struct{})}
	capture, err := startLogCapture(reader, 64)
	if err != nil {
		t.Fatal(err)
	}
	type response struct {
		stdout []byte
		stderr []byte
		err    error
	}
	finished := make(chan response, 1)
	go func() {
		stdout, stderr, _, _, err := capture.finish(5 * time.Millisecond)
		finished <- response{stdout: stdout, stderr: stderr, err: err}
	}()
	select {
	case got := <-finished:
		if got.err == nil || len(got.stdout) != 0 || len(got.stderr) != 0 {
			t.Fatalf("bounded drain = %#v", got)
		}
	case <-time.After(100 * time.Millisecond):
		close(reader.release)
		t.Fatal("log drain remained blocked")
	}
	close(reader.release)
}

func TestTimeoutCleanupFailureOverridesEnvironmentFailure(t *testing.T) {
	api := newFakeEngine()
	api.blockWait = true
	api.stopLeavesRunning = true
	api.killLeavesRunning = true
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	request.Timeout = 5 * time.Millisecond
	defer request.Destroy()

	result, err := target.Execute(context.Background(), request)
	var providerError *executor.ProviderError
	if !result.Started || err == nil || !errors.As(err, &providerError) ||
		providerError.Class != "cleanup" || providerError.CauseCode != "cancel_compensation" {
		api.mu.Lock()
		state, stopCalls, killCalls := api.containerState, api.stopCalls, api.killCalls
		api.mu.Unlock()
		t.Fatalf("timeout cleanup failure = %#v, %v; state %q stop %d kill %d",
			result, err, state, stopCalls, killCalls)
	}
}

func secretDockerRequest(t *testing.T) executor.Request {
	t.Helper()
	requirements := []workerprofile.SecretRequirement{
		{
			Capability: "workload.file", Stage: workerprofile.StageAgent,
			Delivery: workerprofile.DeliveryFile, Target: "/run/paje/secrets/token", Required: true,
		},
		{
			Capability: "workload.directory", Stage: workerprofile.StageAgent,
			Delivery: workerprofile.DeliveryDirectory, Target: "/run/paje/secrets/codex", Required: true,
		},
		{
			Capability: "workload.environment", Stage: workerprofile.StageAgent,
			Delivery: workerprofile.DeliveryEnvironment, Target: "WORKLOAD_TOKEN", Required: true,
		},
	}
	request := dockerRequest(t, workerprofile.NetworkNone, requirements)
	request.Attempt.Purpose = executor.PurposeAgent
	request.Command = executor.Command{
		Executable: "codex", Args: []string{"exec", "task"},
		Directory:   executor.SandboxWorkspaceRoot,
		Environment: map[string]string{"CODEX_HOME": "/home/paje"},
	}
	fileMaterialization, err := secret.NewValueMaterialization(
		workerprofile.DeliveryFile, "/run/paje/secrets/token", []byte("file-secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	environmentMaterialization, err := secret.NewValueMaterialization(
		workerprofile.DeliveryEnvironment, "WORKLOAD_TOKEN", []byte("environment-secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	file, err := secret.NewFile("auth.json", 0o600, []byte("directory-secret"))
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
	request.Secrets = []secret.Materialization{
		environmentMaterialization, directoryMaterialization, fileMaterialization,
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	return request
}

type tarEntry struct {
	mode     int64
	uid      int
	gid      int
	contents []byte
}

func readArchive(t *testing.T, reader io.Reader) map[string]*tarEntry {
	t.Helper()
	entries := make(map[string]*tarEntry)
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(header.Name, "/")
		if _, duplicate := entries[name]; duplicate {
			t.Fatalf("duplicate archive entry %q", name)
		}
		contents, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		entries[name] = &tarEntry{
			mode: header.Mode, uid: header.Uid, gid: header.Gid, contents: contents,
		}
	}
	return entries
}

func assertArchiveFile(t *testing.T, entries map[string]*tarEntry, name string, mode int64, contents string) {
	t.Helper()
	entry := entries[name]
	if entry == nil || entry.mode != mode || string(entry.contents) != contents {
		t.Fatalf("%s = %#v", name, entry)
	}
}

type stuckReadCloser struct {
	release chan struct{}
}

func (reader *stuckReadCloser) Read([]byte) (int, error) {
	<-reader.release
	return 0, io.EOF
}

func (*stuckReadCloser) Close() error { return nil }
