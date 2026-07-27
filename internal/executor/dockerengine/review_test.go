package dockerengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/sandboxinit"
	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
)

func TestCanonicalGoTestGetsExecutableDefaultBuildTempAndPrivateBootstrapTmpfs(t *testing.T) {
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	request.Command.Executable = "go"
	request.Command.Args = []string{"test", "./..."}
	options, err := containerOptions(request, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if options.Config == nil || !options.Config.AttachStdin || !options.Config.OpenStdin ||
		!options.Config.StdinOnce || len(options.Config.Cmd) != 1 ||
		options.Config.Cmd[0] != "--bootstrap-stdin" {
		t.Fatalf("bootstrap container config = %#v", options.Config)
	}
	if options.HostConfig == nil || len(options.HostConfig.Mounts) != 1 ||
		len(options.HostConfig.Tmpfs) != 3 {
		t.Fatalf("tmpfs wire config = %#v", options.HostConfig)
	}
	for _, target := range []string{"/run/paje", "/home/paje"} {
		value := options.HostConfig.Tmpfs[target]
		for _, required := range []string{
			"rw", "nosuid", "nodev", "noexec", "size=67108864",
			"mode=0700", "uid=65532", "gid=65532",
		} {
			if !strings.Contains(value, required) {
				t.Fatalf("tmpfs %s = %q, missing %q", target, value, required)
			}
		}
	}
	temporary := tmpfsOptionSet(options.HostConfig.Tmpfs["/tmp"])
	if temporary["noexec"] || !temporary["exec"] {
		t.Fatalf("canonical go test cannot execute /tmp/go-build binaries: %q", options.HostConfig.Tmpfs["/tmp"])
	}
	encoded, err := json.Marshal(options.HostConfig)
	if err != nil {
		t.Fatal(err)
	}
	var decoded container.HostConfig
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Moby HostConfig wire decode failed: %v", err)
	}
	if len(decoded.Mounts) != 1 || len(decoded.Tmpfs) != 3 {
		t.Fatalf("decoded Moby HostConfig = %#v", decoded)
	}
}

func tmpfsOptionSet(value string) map[string]bool {
	result := make(map[string]bool)
	for _, option := range strings.Split(value, ",") {
		result[option] = true
	}
	return result
}

func TestBuiltArchiveIsAcceptedBySandboxInitBootstrap(t *testing.T) {
	request := secretDockerRequest(t)
	defer request.Destroy()
	archive, err := buildArchive(request)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Destroy()
	root := t.TempDir()
	if err := sandboxinit.ExtractBootstrap(archive.Reader(), root); err != nil {
		t.Fatalf("sandbox-init rejected dockerengine archive: %v", err)
	}
}

func TestBuildArchiveRejectsOversizedValueWithoutDuplicatingIt(t *testing.T) {
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	value := bytes.Repeat([]byte("s"), sandboxinit.MaxBootstrapEntryBytes+1)
	materialization, err := secret.NewValueMaterialization(
		workerprofile.DeliveryFile, sandboxinit.SecretRoot+"/huge", value,
	)
	clear(value)
	if err != nil {
		t.Fatal(err)
	}
	request.Secrets = []secret.Materialization{materialization}
	defer request.Destroy()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	if archive, err := buildArchive(request); err == nil {
		archive.Destroy()
		t.Fatal("oversized archive entry succeeded")
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Fatalf("oversized value was duplicated before rejection: allocated %d bytes", allocated)
	}
}

func TestBuildArchiveRejectsExcessiveTinyFilesBeforeCloningThem(t *testing.T) {
	files := make([]secret.File, 0, sandboxinit.MaxBootstrapEntries+1)
	for index := 0; index < sandboxinit.MaxBootstrapEntries+1; index++ {
		file, err := secret.NewFile(
			fmt.Sprintf("file-%04d", index), 0o600, []byte("x"),
		)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	materialization, err := secret.NewDirectoryMaterialization(
		sandboxinit.SecretRoot+"/tree", files,
	)
	for index := range files {
		files[index].Zero()
	}
	if err != nil {
		t.Fatal(err)
	}
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	request.Secrets = []secret.Materialization{materialization}
	defer request.Destroy()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	if archive, err := buildArchive(request); err == nil {
		archive.Destroy()
		t.Fatal("archive with excessive tiny files succeeded")
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if allocations := after.Mallocs - before.Mallocs; allocations > 100 {
		t.Fatalf("excessive file set was cloned before rejection: %d allocations", allocations)
	}
}

func TestHardCapBufferRejectsOversizedWriteWithoutPartialAllocation(t *testing.T) {
	buffer := &hardCapBuffer{remaining: 4}
	if written, err := buffer.Write([]byte("12345")); err == nil || written != 0 {
		t.Fatalf("oversized write = %d, %v", written, err)
	}
	if buffer.buffer.Len() != 0 || buffer.remaining != 4 {
		t.Fatalf("hard cap mutated after rejection = %d/%d", buffer.buffer.Len(), buffer.remaining)
	}
}

func TestExecuteStreamsBootstrapAfterStartAndOnlyThenReportsStarted(t *testing.T) {
	api := newFakeEngine()
	api.blockBootstrap = make(chan struct{})
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()

	type response struct {
		result executor.Result
		err    error
	}
	finished := make(chan response, 1)
	go func() {
		result, err := target.Execute(context.Background(), request)
		finished <- response{result: result, err: err}
	}()
	select {
	case <-api.containerStarted:
	case <-time.After(time.Second):
		t.Fatal("sandbox-init container did not start")
	}
	select {
	case got := <-finished:
		t.Fatalf("Execute returned before bootstrap delivery: %#v, %v", got.result, got.err)
	default:
	}
	close(api.blockBootstrap)
	got := <-finished
	if got.err != nil || !got.result.Started {
		t.Fatalf("bootstrap lifecycle = %#v, %v", got.result, got.err)
	}
	api.mu.Lock()
	archive := append([]byte(nil), api.archive...)
	operations := append([]string(nil), api.operations...)
	api.mu.Unlock()
	if len(archive) == 0 {
		t.Fatal("attach stdin received no bootstrap archive")
	}
	if strings.Contains(strings.Join(operations, ","), "copy-archive") {
		t.Fatalf("stopped-container archive copy remained: %v", operations)
	}
}

func TestBootstrapDeliveryAmbiguityIsNonretryableAndNeverReportsAgentStarted(t *testing.T) {
	for name, configure := range map[string]func(*fakeEngine){
		"write": func(api *fakeEngine) {
			api.bootstrapWriteErr = io.ErrUnexpectedEOF
		},
		"close write": func(api *fakeEngine) {
			api.closeWriteErr = io.ErrUnexpectedEOF
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeEngine()
			configure(api)
			target := newExecutorForTest(t, api)
			request := dockerRequest(t, workerprofile.NetworkNone, nil)
			defer request.Destroy()
			result, err := target.Execute(context.Background(), request)
			assertProviderCause(t, err, "ambiguous_attempt")
			if !result.Created || result.Started {
				t.Fatalf("ambiguous bootstrap delivery = %#v", result)
			}
			if _, retryErr := target.Execute(context.Background(), request); !errors.Is(retryErr, executor.ErrAttemptExists) {
				t.Fatalf("ambiguous bootstrap delivery was retryable: %v", retryErr)
			}
		})
	}
}

func TestStartResponseLossNeverClaimsProvenNonStart(t *testing.T) {
	t.Run("created is proven non-start", func(t *testing.T) {
		api := newFakeEngine()
		api.startErr = errors.New("definitive start rejection")
		api.startStateAfterError = engineContainerCreated
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkNone, nil)
		defer request.Destroy()
		result, err := target.Execute(context.Background(), request)
		assertProviderCause(t, err, "start")
		if !result.Created || result.Started {
			t.Fatalf("proven start failure = %#v", result)
		}
	})

	for _, state := range []engineContainerState{
		engineContainerRunning, engineContainerExited, engineContainerDead,
	} {
		t.Run(string(state), func(t *testing.T) {
			api := newFakeEngine()
			api.startErr = io.ErrUnexpectedEOF
			api.startStateAfterError = state
			target := newExecutorForTest(t, api)
			request := dockerRequest(t, workerprofile.NetworkNone, nil)
			defer request.Destroy()
			result, err := target.Execute(context.Background(), request)
			assertProviderCause(t, err, "ambiguous_attempt")
			if !result.Created || result.Started {
				t.Fatalf("ambiguous start = %#v", result)
			}
		})
	}
}

func TestCreateResponseLossIsReconciledByExactLabels(t *testing.T) {
	t.Run("container recovered", func(t *testing.T) {
		api := newFakeEngine()
		api.createErr = io.ErrUnexpectedEOF
		api.createPersistsOnError = true
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkNone, nil)
		defer request.Destroy()
		result, err := target.Execute(context.Background(), request)
		if err != nil || !result.Completed {
			t.Fatalf("recovered container create = %#v, %v", result, err)
		}
	})

	t.Run("container unknown", func(t *testing.T) {
		api := newFakeEngine()
		api.createErr = io.ErrUnexpectedEOF
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkNone, nil)
		defer request.Destroy()
		result, err := target.Execute(context.Background(), request)
		assertProviderCause(t, err, "ambiguous_attempt")
		if result.Created {
			t.Fatalf("unknown container create = %#v", result)
		}
		if _, retryErr := target.Execute(context.Background(), request); !errors.Is(retryErr, executor.ErrAttemptExists) {
			t.Fatalf("ambiguous create was retryable: %v", retryErr)
		}
	})

	t.Run("network recovered", func(t *testing.T) {
		api := newFakeEngine()
		api.networkCreateErr = io.ErrUnexpectedEOF
		api.networkPersistsOnError = true
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkOutbound, nil)
		defer request.Destroy()
		result, err := target.Execute(context.Background(), request)
		if err != nil || !result.Completed {
			t.Fatalf("recovered network create = %#v, %v", result, err)
		}
	})

	t.Run("network unknown", func(t *testing.T) {
		api := newFakeEngine()
		api.networkCreateErr = io.ErrUnexpectedEOF
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkOutbound, nil)
		defer request.Destroy()
		result, err := target.Execute(context.Background(), request)
		assertProviderCause(t, err, "ambiguous_attempt")
		if result.Created {
			t.Fatalf("unknown network create = %#v", result)
		}
		if _, retryErr := target.Execute(context.Background(), request); !errors.Is(retryErr, executor.ErrAttemptExists) {
			t.Fatalf("ambiguous network create was retryable: %v", retryErr)
		}
	})
}

func TestDestroyReconcilesResourcesThatAppearAfterAmbiguousCreate(t *testing.T) {
	for name, testCase := range map[string]struct {
		network string
		setup   func(*fakeEngine)
		armList func(*fakeEngine) <-chan struct{}
		release func(*fakeEngine)
		done    func(*fakeEngine) <-chan struct{}
		assert  func(*testing.T, *fakeEngine)
	}{
		"container": {
			network: workerprofile.NetworkNone,
			setup: func(api *fakeEngine) {
				api.createErr = io.ErrUnexpectedEOF
				api.delayedCreateRelease = make(chan struct{})
				api.delayedCreateDone = make(chan struct{})
			},
			armList: func(api *fakeEngine) <-chan struct{} {
				api.mu.Lock()
				defer api.mu.Unlock()
				api.containerListEntered = make(chan struct{}, 1)
				return api.containerListEntered
			},
			release: func(api *fakeEngine) { close(api.delayedCreateRelease) },
			done:    func(api *fakeEngine) <-chan struct{} { return api.delayedCreateDone },
			assert: func(t *testing.T, api *fakeEngine) {
				t.Helper()
				api.mu.Lock()
				defer api.mu.Unlock()
				if api.containerExists || api.removeContainerCalls != 1 {
					t.Fatalf("delayed container remains/removals = %v/%d", api.containerExists, api.removeContainerCalls)
				}
			},
		},
		"network": {
			network: workerprofile.NetworkOutbound,
			setup: func(api *fakeEngine) {
				api.networkCreateErr = io.ErrUnexpectedEOF
				api.delayedNetworkRelease = make(chan struct{})
				api.delayedNetworkDone = make(chan struct{})
			},
			armList: func(api *fakeEngine) <-chan struct{} {
				api.mu.Lock()
				defer api.mu.Unlock()
				api.networkListEntered = make(chan struct{}, 1)
				return api.networkListEntered
			},
			release: func(api *fakeEngine) { close(api.delayedNetworkRelease) },
			done:    func(api *fakeEngine) <-chan struct{} { return api.delayedNetworkDone },
			assert: func(t *testing.T, api *fakeEngine) {
				t.Helper()
				api.mu.Lock()
				defer api.mu.Unlock()
				if api.networkExists || api.removeNetworkCalls != 1 {
					t.Fatalf("delayed network remains/removals = %v/%d", api.networkExists, api.removeNetworkCalls)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeEngine()
			testCase.setup(api)
			target := newExecutorForTest(t, api)
			request := dockerRequest(t, testCase.network, nil)
			defer request.Destroy()
			result, err := target.Execute(context.Background(), request)
			assertProviderCause(t, err, "ambiguous_attempt")
			if result.Created || result.Started {
				t.Fatalf("ambiguous create evidence = %#v", result)
			}

			listEntered := testCase.armList(api)
			destroyed := make(chan error, 1)
			go func() { destroyed <- target.Destroy(context.Background(), request.Attempt) }()
			select {
			case <-listEntered:
			case <-time.After(time.Second):
				t.Fatal("Destroy did not begin exact-label reconciliation")
			}
			testCase.release(api)
			select {
			case <-testCase.done(api):
			case <-time.After(time.Second):
				t.Fatal("delayed resource did not materialize")
			}
			if err := <-destroyed; err != nil {
				t.Fatal(err)
			}
			testCase.assert(t, api)
		})
	}
}

func TestLifecycleForUnrelatedAttemptDoesNotWaitBehindBlockedCreate(t *testing.T) {
	api := newFakeEngine()
	api.createEntered = make(chan struct{})
	api.releaseCreate = make(chan struct{})
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	executed := make(chan error, 1)
	go func() {
		_, err := target.Execute(context.Background(), request)
		executed <- err
	}()
	select {
	case <-api.createEntered:
	case <-time.After(time.Second):
		t.Fatal("Execute did not reach blocked container create")
	}

	unrelated := request.Attempt
	unrelated.RunID = "unrelated-run"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	destroyed := make(chan error, 1)
	go func() { destroyed <- target.Destroy(ctx, unrelated) }()
	select {
	case err := <-destroyed:
		if err != nil {
			t.Fatalf("unrelated Destroy() = %v", err)
		}
	case <-ctx.Done():
		close(api.releaseCreate)
		<-executed
		t.Fatal("unrelated blocked create stalled Destroy past its context")
	}
	close(api.releaseCreate)
	if err := <-executed; err != nil {
		t.Fatalf("Execute() = %v", err)
	}
}

func TestLifecycleForUnrelatedAttemptDoesNotWaitBehindBlockedStart(t *testing.T) {
	api := newFakeEngine()
	api.startEntered = make(chan struct{})
	api.releaseStart = make(chan struct{})
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	executed := make(chan error, 1)
	go func() {
		_, err := target.Execute(context.Background(), request)
		executed <- err
	}()
	select {
	case <-api.startEntered:
	case <-time.After(time.Second):
		t.Fatal("Execute did not reach blocked container start")
	}

	unrelated := request.Attempt
	unrelated.RunID = "unrelated-start-run"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	destroyed := make(chan error, 1)
	go func() { destroyed <- target.Destroy(ctx, unrelated) }()
	select {
	case err := <-destroyed:
		if err != nil {
			t.Fatalf("unrelated Destroy() = %v", err)
		}
	case <-ctx.Done():
		close(api.releaseStart)
		<-executed
		t.Fatal("unrelated blocked start stalled Destroy past its context")
	}
	close(api.releaseStart)
	if err := <-executed; err != nil {
		t.Fatalf("Execute() = %v", err)
	}
}

func TestSameAttemptDestroyDuringCreateIsStickyAndCompensates(t *testing.T) {
	api := newFakeEngine()
	api.createEntered = make(chan struct{})
	api.releaseCreate = make(chan struct{})
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	type outcome struct {
		result executor.Result
		err    error
	}
	executed := make(chan outcome, 1)
	go func() {
		result, err := target.Execute(context.Background(), request)
		executed <- outcome{result: result, err: err}
	}()
	select {
	case <-api.createEntered:
	case <-time.After(time.Second):
		t.Fatal("Execute did not reach blocked container create")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	destroyed := make(chan error, 1)
	go func() { destroyed <- target.Destroy(ctx, request.Attempt) }()
	select {
	case err := <-destroyed:
		if err != nil {
			t.Fatalf("same-attempt Destroy() = %v", err)
		}
	case <-ctx.Done():
		close(api.releaseCreate)
		<-executed
		t.Fatal("same-attempt Destroy could not record sticky intent before context expiry")
	}
	close(api.releaseCreate)
	got := <-executed
	assertProviderCause(t, got.err, "lifecycle_requested")
	if got.result.Started {
		t.Fatalf("lifecycle-compensated create reported Started: %#v", got.result)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.containerExists || api.removeContainerCalls != 1 {
		t.Fatalf("post-create compensation = exists %v removals %d", api.containerExists, api.removeContainerCalls)
	}
	if slices.Contains(api.operations, "start") {
		t.Fatalf("sticky destroy allowed start: %v", api.operations)
	}
}

func TestSameAttemptDestroyDuringBootstrapNeverReportsAgentStarted(t *testing.T) {
	api := newFakeEngine()
	api.blockBootstrap = make(chan struct{})
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	type outcome struct {
		result executor.Result
		err    error
	}
	executed := make(chan outcome, 1)
	go func() {
		result, err := target.Execute(context.Background(), request)
		executed <- outcome{result: result, err: err}
	}()
	select {
	case <-api.containerStarted:
	case <-time.After(time.Second):
		t.Fatal("sandbox-init container did not start")
	}
	if err := target.Destroy(context.Background(), request.Attempt); err != nil {
		t.Fatal(err)
	}
	close(api.blockBootstrap)
	got := <-executed
	assertProviderCause(t, got.err, "lifecycle_requested")
	if got.result.Started {
		t.Fatalf("destroyed bootstrap reported agent Started: %#v", got.result)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.containerExists || api.removeContainerCalls != 1 {
		t.Fatalf("bootstrap lifecycle compensation = exists %v removals %d", api.containerExists, api.removeContainerCalls)
	}
}

func TestSameAttemptDestroyCompensatesDelayedAmbiguousCreate(t *testing.T) {
	api := newFakeEngine()
	api.createErr = io.ErrUnexpectedEOF
	api.createEntered = make(chan struct{})
	api.releaseCreate = make(chan struct{})
	api.delayedCreateRelease = make(chan struct{})
	api.delayedCreateDone = make(chan struct{})
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	type outcome struct {
		result executor.Result
		err    error
	}
	executed := make(chan outcome, 1)
	go func() {
		result, err := target.Execute(context.Background(), request)
		executed <- outcome{result: result, err: err}
	}()
	select {
	case <-api.createEntered:
	case <-time.After(time.Second):
		t.Fatal("Execute did not reach blocked container create")
	}
	if err := target.Destroy(context.Background(), request.Attempt); err != nil {
		t.Fatal(err)
	}

	api.mu.Lock()
	api.containerListEntered = make(chan struct{}, 4)
	listEntered := api.containerListEntered
	api.mu.Unlock()
	close(api.releaseCreate)
	select {
	case <-listEntered:
	case <-time.After(time.Second):
		t.Fatal("create response loss was not reconciled")
	}
	select {
	case <-listEntered:
		close(api.delayedCreateRelease)
	case got := <-executed:
		close(api.delayedCreateRelease)
		<-api.delayedCreateDone
		assertProviderCause(t, got.err, "ambiguous_attempt")
		t.Fatal("Execute returned before compensating a lifecycle-raced ambiguous create")
	case <-time.After(time.Second):
		close(api.delayedCreateRelease)
		<-api.delayedCreateDone
		t.Fatal("Execute did not begin post-response compensation")
	}
	select {
	case <-api.delayedCreateDone:
	case <-time.After(time.Second):
		t.Fatal("delayed container did not materialize")
	}
	got := <-executed
	assertProviderCause(t, got.err, "ambiguous_attempt")
	if got.result.Created || got.result.Started {
		t.Fatalf("ambiguous create evidence = %#v", got.result)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.containerExists || api.removeContainerCalls != 1 {
		t.Fatalf("delayed lifecycle-raced container remains/removals = %v/%d", api.containerExists, api.removeContainerCalls)
	}
}

func TestCreateNameCollisionsWithoutExactLabelsAreConflicts(t *testing.T) {
	for name, configure := range map[string]func(*fakeEngine){
		"container": func(api *fakeEngine) { api.createErr = errdefs.ErrConflict },
		"network":   func(api *fakeEngine) { api.networkCreateErr = errdefs.ErrConflict },
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeEngine()
			configure(api)
			target := newExecutorForTest(t, api)
			network := workerprofile.NetworkNone
			if name == "network" {
				network = workerprofile.NetworkOutbound
			}
			request := dockerRequest(t, network, nil)
			defer request.Destroy()
			_, err := target.Execute(context.Background(), request)
			assertProviderCause(t, err, "resource_conflict")
		})
	}
}

func TestCancelAndDestroyAreStickyBeforeExecuteResourceDiscovery(t *testing.T) {
	for name, lifecycle := range map[string]func(*Executor, context.Context, executor.AttemptID) error{
		"cancel":  (*Executor).Cancel,
		"destroy": (*Executor).Destroy,
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeEngine()
			api.pingEntered = make(chan struct{})
			api.releasePing = make(chan struct{})
			target := newExecutorForTest(t, api)
			request := dockerRequest(t, workerprofile.NetworkNone, nil)
			defer request.Destroy()
			finished := make(chan error, 1)
			go func() {
				_, err := target.Execute(context.Background(), request)
				finished <- err
			}()
			select {
			case <-api.pingEntered:
			case <-time.After(time.Second):
				t.Fatal("Execute did not reserve before image verification")
			}
			if err := lifecycle(target, context.Background(), request.Attempt); err != nil {
				t.Fatalf("%s() = %v", name, err)
			}
			close(api.releasePing)
			if err := <-finished; err == nil {
				t.Fatal("sticky lifecycle did not stop Execute")
			}
			api.mu.Lock()
			createCount := api.createCount
			networkExists := api.networkExists
			api.mu.Unlock()
			if createCount != 0 || networkExists {
				t.Fatalf("sticky lifecycle allowed create = %d/%v", createCount, networkExists)
			}
		})
	}
}

func TestCancelTreatsDeadAfterStopAsTerminated(t *testing.T) {
	api := newFakeEngine()
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	api.containerExists = true
	api.containerState = engineContainerRunning
	api.containerLabels = attemptLabels(request.Attempt, resourceContainer)
	api.stopState = engineContainerDead
	target := newExecutorForTest(t, api)
	if err := target.Cancel(context.Background(), request.Attempt); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	killCalls := api.killCalls
	api.mu.Unlock()
	if killCalls != 0 {
		t.Fatalf("dead container was killed %d times", killCalls)
	}
}

func TestDestroyRecordsOnlyOneDestroyedTransition(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	if _, err := target.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := target.Destroy(context.Background(), request.Attempt); err != nil {
			t.Fatal(err)
		}
	}
	target.mu.Lock()
	history := append([]string(nil), target.destroyedOrder...)
	target.mu.Unlock()
	if len(history) != 1 || history[0] != request.Attempt.Key() {
		t.Fatalf("destroyed history = %#v", history)
	}
}

func assertProviderCause(t *testing.T, err error, cause string) {
	t.Helper()
	var providerError *executor.ProviderError
	if err == nil || !errors.As(err, &providerError) || providerError.CauseCode != cause {
		t.Fatalf("provider error = %v, want cause %q", err, cause)
	}
}
