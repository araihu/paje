package dockerengine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/sandboxinit"
	"github.com/araihu/paje/internal/workerprofile"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestNewAcceptsOnlyExplicitAbsoluteUnixSocket(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{
		"",
		"tcp://127.0.0.1:2375",
		"ssh://host",
		"http://localhost",
		"unix://relative.sock",
		"unix:///var/run/../docker.sock",
		"unix:///",
		"unix:///var/run/docker.sock?query=forbidden",
		"unix:///var/run/docker.sock#fragment",
		"unix:///var/run/docker.sock%00",
		"unix:///var/run/docker%2Esock",
	} {
		endpoint := endpoint
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			if _, err := New(Config{Endpoint: endpoint}); err == nil {
				t.Fatalf("New(%q) succeeded", endpoint)
			}
		})
	}
}

func TestNewRejectsUnboundedLifecycleTimeouts(t *testing.T) {
	t.Parallel()

	for _, config := range []Config{
		{Endpoint: "unix:///var/run/docker.sock", StopTimeout: -time.Second},
		{Endpoint: "unix:///var/run/docker.sock", KillTimeout: -time.Second},
		{Endpoint: "unix:///var/run/docker.sock", StopTimeout: time.Hour},
		{Endpoint: "unix:///var/run/docker.sock", KillTimeout: time.Hour},
	} {
		if _, err := New(config); err == nil {
			t.Fatalf("New(%#v) succeeded", config)
		}
	}
}

func TestExecuteUsesExactImageAndHardenedOneShotContainer(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()

	result, err := target.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || !result.Started || !result.Completed || result.ExitCode != 0 {
		t.Fatalf("lifecycle evidence = %#v", result)
	}
	if result.SafeFacts["runtime_kind"] != workerprofile.RuntimeOCI ||
		result.SafeFacts["image"] != request.Profile.Runtime.Image ||
		result.SafeFacts["platform"] != request.Profile.Runtime.Platform ||
		result.SafeFacts["isolated"] != "true" {
		t.Fatalf("safe facts = %#v", result.SafeFacts)
	}

	api.mu.Lock()
	operations := slices.Clone(api.operations)
	created := api.created
	pull := api.pull
	imagePlatforms := slices.Clone(api.imagePlatforms)
	api.mu.Unlock()
	if pull.Reference != request.Profile.Runtime.Image ||
		pull.Platform != request.Profile.Runtime.Platform ||
		pull.RegistryAuth != "executor-registry-auth" {
		t.Fatalf("image pull = %#v", pull)
	}
	if len(imagePlatforms) != 2 ||
		imagePlatforms[0].OS != "linux" || imagePlatforms[0].Architecture != "amd64" ||
		imagePlatforms[1].OS != "linux" || imagePlatforms[1].Architecture != "amd64" {
		t.Fatalf("image inspect platforms = %#v", imagePlatforms)
	}
	if strings.Join(operations, ",") != "ping,inspect-image,pull-image,inspect-image,list-networks,list-containers,create-container,attach,start,signal:SIGUSR1,wait" {
		t.Fatalf("engine operations = %v", operations)
	}
	assertHardenedCreate(t, created, request)
}

func TestExecuteRejectsExactImageOrPlatformMismatch(t *testing.T) {
	for _, mutate := range []func(*fakeEngine){
		func(api *fakeEngine) {
			api.image.RepositoryDigests = []string{"example.invalid/other@sha256:" + strings.Repeat("b", 64)}
		},
		func(api *fakeEngine) {
			api.image.Architecture = "arm64"
		},
	} {
		api := newFakeEngine()
		api.imagePresent = true
		mutate(api)
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkNone, nil)
		result, err := target.Execute(context.Background(), request)
		request.Destroy()
		var providerError *executor.ProviderError
		if result.Created || err == nil || !errors.As(err, &providerError) ||
			providerError.Class != "environment" || providerError.CauseCode != "image_mismatch" {
			t.Fatalf("mismatch result = %#v, %v", result, err)
		}
		api.mu.Lock()
		createCount := api.createCount
		api.mu.Unlock()
		if createCount != 0 {
			t.Fatalf("containers created after mismatch = %d", createCount)
		}
	}
}

func TestExecuteRejectsUnsupportedEngineCapabilities(t *testing.T) {
	for _, info := range []engineInfo{
		{OSType: "darwin", APIVersion: "1.55"},
		{OSType: "linux"},
		{OSType: "linux", APIVersion: "1.43"},
		{OSType: "linux", APIVersion: "1.48"},
		{OSType: "linux", APIVersion: "invalid"},
	} {
		api := newFakeEngine()
		api.ping = info
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkNone, nil)
		result, err := target.Execute(context.Background(), request)
		request.Destroy()
		var providerError *executor.ProviderError
		if result.Created || err == nil || !errors.As(err, &providerError) ||
			providerError.Class != "environment" || providerError.CauseCode != "engine_unsupported" {
			t.Fatalf("engine %#v result = %#v, %v", info, result, err)
		}
	}
}

func TestLifecycleIsIdempotentAndCollisionSafe(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkOutbound, nil)
	defer request.Destroy()

	if _, err := target.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		state, err := target.Inspect(context.Background(), request.Attempt)
		if err != nil || state != executor.StateCompleted {
			t.Fatalf("Inspect() = %q, %v", state, err)
		}
	}
	if _, err := target.Execute(context.Background(), request); !errors.Is(err, executor.ErrAttemptExists) {
		t.Fatalf("identity collision = %v", err)
	}
	for range 2 {
		if err := target.Cancel(context.Background(), request.Attempt); err != nil {
			t.Fatalf("Cancel() = %v", err)
		}
	}
	for range 2 {
		if err := target.Destroy(context.Background(), request.Attempt); err != nil {
			t.Fatalf("Destroy() = %v", err)
		}
	}
	state, err := target.Inspect(context.Background(), request.Attempt)
	if err != nil || state != executor.StateDestroyed {
		t.Fatalf("Inspect() after destroy = %q, %v", state, err)
	}
	api.mu.Lock()
	removedContainer := api.removeContainerCalls
	removedNetwork := api.removeNetworkCalls
	api.mu.Unlock()
	if removedContainer != 1 || removedNetwork != 1 {
		t.Fatalf("removals = container %d network %d", removedContainer, removedNetwork)
	}
}

func TestExecuteReturnsBoundedSeparateStreams(t *testing.T) {
	api := newFakeEngine()
	api.attached = multiplexedOutput(
		bytes.Repeat([]byte("o"), 128),
		bytes.Repeat([]byte("e"), 128),
	)
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	request.OutputLimit = 16
	defer request.Destroy()

	result, err := target.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != strings.Repeat("o", 16) ||
		string(result.Stderr) != strings.Repeat("e", 16) ||
		!result.StdoutTruncated || !result.StderrTruncated {
		t.Fatalf("bounded streams = stdout %q stderr %q %#v", result.Stdout, result.Stderr, result)
	}
}

func TestExecuteClassifiesStartFailureAndTimeout(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		api := newFakeEngine()
		api.startErr = errors.New("provider-detail start failed")
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkNone, nil)
		defer request.Destroy()
		result, err := target.Execute(context.Background(), request)
		var providerError *executor.ProviderError
		if !result.Created || result.Started || result.Completed || err == nil ||
			!errors.As(err, &providerError) || providerError.CauseCode != "start" {
			t.Fatalf("start failure = %#v, %v", result, err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		api := newFakeEngine()
		api.blockWait = true
		target := newExecutorForTest(t, api)
		request := dockerRequest(t, workerprofile.NetworkNone, nil)
		request.Timeout = 20 * time.Millisecond
		defer request.Destroy()
		result, err := target.Execute(context.Background(), request)
		var providerError *executor.ProviderError
		if !result.Started || result.Completed || err == nil ||
			!errors.As(err, &providerError) ||
			providerError.Class != "environment" || providerError.CauseCode != "timeout" {
			t.Fatalf("timeout = %#v, %v", result, err)
		}
	})
}

func assertHardenedCreate(t *testing.T, created client.ContainerCreateOptions, request executor.Request) {
	t.Helper()
	if created.Config == nil || created.HostConfig == nil {
		t.Fatal("container create omitted configuration")
	}
	if created.Image != "" || created.Config.Image != request.Profile.Runtime.Image ||
		!slices.Equal(created.Config.Entrypoint, []string{"/usr/local/bin/paje-sandbox-init"}) ||
		!slices.Equal(created.Config.Cmd, []string{"--bootstrap-stdin"}) ||
		len(created.Config.Env) != 0 || created.Config.User == "" || created.Config.User == "0" ||
		created.Config.User == "root" || created.Config.WorkingDir != "/" ||
		created.Config.Tty || !created.Config.AttachStdin || !created.Config.OpenStdin ||
		!created.Config.StdinOnce || len(created.Config.ExposedPorts) != 0 {
		t.Fatalf("container config = %#v", created.Config)
	}
	host := created.HostConfig
	if !host.ReadonlyRootfs || host.Privileged || host.PublishAllPorts || host.AutoRemove ||
		!slices.Contains(host.CapDrop, "ALL") ||
		!slices.Contains(host.SecurityOpt, "no-new-privileges=true") ||
		len(host.CapAdd) != 0 || len(host.Devices) != 0 || len(host.DeviceRequests) != 0 ||
		len(host.PortBindings) != 0 || len(host.Binds) != 0 || len(host.VolumesFrom) != 0 ||
		host.PidMode.IsHost() || host.IpcMode.IsHost() || host.UTSMode.IsHost() ||
		host.CgroupnsMode.IsHost() || host.UsernsMode.IsHost() ||
		host.Memory != request.Profile.Resources.MemoryBytes ||
		host.NanoCPUs != request.Profile.Resources.CPUMillis*1_000_000 ||
		host.PidsLimit == nil || *host.PidsLimit != request.Profile.Resources.PIDs ||
		!host.NetworkMode.IsNone() {
		t.Fatalf("host config = %#v", host)
	}
	if len(host.Mounts) != 1 || len(host.Tmpfs) != 3 {
		t.Fatalf("mounts = %#v", host.Mounts)
	}
}

func dockerRequest(t *testing.T, network string, requirements []workerprofile.SecretRequirement) executor.Request {
	t.Helper()
	profile, err := workerprofile.Canonicalize(workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "codex-go", Revision: 1},
		Runtime: workerprofile.Runtime{
			Kind: workerprofile.RuntimeOCI, Image: "example.invalid/worker@sha256:" + strings.Repeat("a", 64),
			Platform: "linux/amd64", Network: network, ReadOnlyRoot: true,
		},
		Resources: workerprofile.Resources{CPUMillis: 1500, MemoryBytes: 1 << 30, PIDs: 64},
		Harness:   workerprofile.Harness{ID: "codex", Version: "0.144.5"},
		Secrets:   requirements,
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor.Request{
		Attempt: executor.AttemptID{
			RunID: "run-docker", Stage: "execute", Attempt: 2,
			StartedAt: time.Unix(100, 123).UTC(), Purpose: executor.PurposeVerification, Sequence: 3,
		},
		Profile: profile,
		Command: executor.Command{
			Executable: "go", Args: []string{"test", "./..."},
			Directory:   executor.SandboxWorkspaceRoot,
			Environment: map[string]string{"GOFLAGS": "-mod=readonly"},
		},
		Workspace: executor.Workspace{
			HostPath: t.TempDir(), SandboxPath: executor.SandboxWorkspaceRoot, Writable: true,
		},
		Environment: map[string]string{"PATH": executor.CanonicalSandboxPATH},
		Timeout:     5 * time.Second,
		OutputLimit: 1024,
	}
}

func newExecutorForTest(t *testing.T, api *fakeEngine) *Executor {
	t.Helper()
	target, err := newWithEngine(Config{
		Endpoint: "unix:///var/run/docker.sock", RegistryAuth: "executor-registry-auth",
		StopTimeout: 5 * time.Millisecond, KillTimeout: 5 * time.Millisecond,
	}, api)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

type fakeEngine struct {
	mu sync.Mutex

	operations  []string
	ping        engineInfo
	pingErr     error
	pingEntered chan struct{}
	releasePing chan struct{}

	image             imageInfo
	imagePresent      bool
	imageInspectCalls int
	imagePlatforms    []ocispec.Platform
	pull              pullRequest

	created                    client.ContainerCreateOptions
	createCount                int
	createErr                  error
	createPersistsOnError      bool
	createEntered              chan struct{}
	releaseCreate              chan struct{}
	delayedCreateRelease       chan struct{}
	delayedCreateDone          chan struct{}
	containerListEntered       chan struct{}
	archive                    []byte
	attached                   []byte
	blockBootstrap             chan struct{}
	bootstrapWriteErr          error
	closeWriteErr              error
	receiptMissing             bool
	receiptOverride            []byte
	receipt                    []byte
	receiptErr                 error
	receiptTransform           func([]byte) []byte
	releaseReceiptPublication  chan struct{}
	childStartAckErr           error
	childStartAcknowledgements int

	containerExists     bool
	containerState      engineContainerState
	containerLabels     map[string]string
	containerInspectErr error
	extraContainers     int

	networkExists          bool
	networkLabels          map[string]string
	extraNetworks          int
	networkCreateErr       error
	networkPersistsOnError bool
	delayedNetworkRelease  chan struct{}
	delayedNetworkDone     chan struct{}
	networkListEntered     chan struct{}

	startErr             error
	startStateAfterError engineContainerState
	startEntered         chan struct{}
	releaseStart         chan struct{}
	waitCode             int64
	blockWait            bool
	containerStarted     chan struct{}
	started              chan struct{}
	exited               chan struct{}
	containerStartOnce   sync.Once
	agentStartOnce       sync.Once
	exitOnce             sync.Once

	stopLeavesRunning    bool
	stopState            engineContainerState
	killLeavesRunning    bool
	stopCalls            int
	killCalls            int
	removeContainerCalls int
	removeNetworkCalls   int
}

func newFakeEngine() *fakeEngine {
	image := "example.invalid/worker@sha256:" + strings.Repeat("a", 64)
	return &fakeEngine{
		ping: engineInfo{OSType: "linux", APIVersion: "1.55"},
		image: imageInfo{
			RepositoryDigests: []string{image}, OS: "linux", Architecture: "amd64",
		},
		attached:         multiplexedOutput([]byte("stdout"), []byte("stderr")),
		waitCode:         0,
		containerStarted: make(chan struct{}),
		started:          make(chan struct{}),
		exited:           make(chan struct{}),
	}
}

func (api *fakeEngine) record(operation string) {
	api.operations = append(api.operations, operation)
}

func (api *fakeEngine) Ping(context.Context) (engineInfo, error) {
	api.mu.Lock()
	api.record("ping")
	info, err := api.ping, api.pingErr
	entered, release := api.pingEntered, api.releasePing
	api.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		<-release
	}
	return info, err
}

func (api *fakeEngine) InspectImage(_ context.Context, _ string, platform ocispec.Platform) (imageInfo, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.record("inspect-image")
	api.imageInspectCalls++
	api.imagePlatforms = append(api.imagePlatforms, platform)
	if !api.imagePresent {
		return imageInfo{}, errdefs.ErrNotFound
	}
	return api.image, nil
}

func (api *fakeEngine) PullImage(_ context.Context, request pullRequest) error {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.record("pull-image")
	api.pull = request
	api.imagePresent = true
	return nil
}

func (api *fakeEngine) ListContainers(_ context.Context, labels map[string]string) ([]engineContainer, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.record("list-containers")
	if api.containerListEntered != nil {
		select {
		case api.containerListEntered <- struct{}{}:
		default:
		}
	}
	result := make([]engineContainer, 0, api.extraContainers+1)
	for index := 0; index < api.extraContainers; index++ {
		result = append(result, engineContainer{ID: "provider-detail-extra"})
	}
	if api.containerExists && labelsContain(api.containerLabels, labels) {
		result = append(result, engineContainer{ID: "provider-detail-container", Labels: cloneStrings(api.containerLabels)})
	}
	return result, nil
}

func (api *fakeEngine) CreateContainer(_ context.Context, options client.ContainerCreateOptions) (string, error) {
	api.mu.Lock()
	api.record("create-container")
	api.created = options
	api.createCount++
	entered, release := api.createEntered, api.releaseCreate
	createErr := api.createErr
	persists := api.createPersistsOnError
	delayedRelease, delayedDone := api.delayedCreateRelease, api.delayedCreateDone
	labels := cloneStrings(options.Config.Labels)
	api.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		<-release
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if createErr == nil || persists {
		api.containerExists = true
		api.containerState = engineContainerCreated
		api.containerLabels = labels
	}
	if createErr != nil && delayedRelease != nil {
		go func() {
			<-delayedRelease
			api.mu.Lock()
			api.containerExists = true
			api.containerState = engineContainerCreated
			api.containerLabels = labels
			api.mu.Unlock()
			close(delayedDone)
		}()
	}
	if createErr != nil {
		return "", createErr
	}
	return "provider-detail-container", nil
}

type fakeAttachedStream struct {
	api    *fakeEngine
	reader *bytes.Reader
}

func (stream *fakeAttachedStream) Read(value []byte) (int, error) {
	return stream.reader.Read(value)
}

func (stream *fakeAttachedStream) Write(value []byte) (int, error) {
	stream.api.mu.Lock()
	block := stream.api.blockBootstrap
	stream.api.mu.Unlock()
	if block != nil {
		<-block
	}
	stream.api.mu.Lock()
	if stream.api.bootstrapWriteErr != nil {
		err := stream.api.bootstrapWriteErr
		stream.api.mu.Unlock()
		return 0, err
	}
	stream.api.archive = append(stream.api.archive, value...)
	stream.api.mu.Unlock()
	return len(value), nil
}

func (*fakeAttachedStream) Close() error { return nil }
func (stream *fakeAttachedStream) CloseWrite() error {
	stream.api.mu.Lock()
	if stream.api.closeWriteErr == nil {
		stream.api.receipt = receiptFromBootstrapArchive(stream.api.archive)
		stream.api.agentStartOnce.Do(func() { close(stream.api.started) })
	}
	release := stream.api.releaseReceiptPublication
	err := stream.api.closeWriteErr
	stream.api.mu.Unlock()
	if release != nil {
		<-release
	}
	return err
}
func (*fakeAttachedStream) SetWriteDeadline(time.Time) error { return nil }

func (api *fakeEngine) AttachContainer(context.Context, string) (attachedContainerIO, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.record("attach")
	return &fakeAttachedStream{
		api: api, reader: bytes.NewReader(slices.Clone(api.attached)),
	}, nil
}

func (api *fakeEngine) StartContainer(context.Context, string) error {
	api.mu.Lock()
	api.record("start")
	startErr := api.startErr
	startStateAfterError := api.startStateAfterError
	entered, release := api.startEntered, api.releaseStart
	api.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		<-release
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if startErr != nil {
		if startStateAfterError != "" {
			api.containerState = startStateAfterError
		}
		return startErr
	}
	api.containerState = engineContainerRunning
	api.containerStartOnce.Do(func() { close(api.containerStarted) })
	if !api.blockWait {
		api.containerState = engineContainerExited
		api.exitOnce.Do(func() { close(api.exited) })
	}
	return nil
}

func (api *fakeEngine) WaitContainer(ctx context.Context, _ string) (int64, error) {
	api.mu.Lock()
	api.record("wait")
	exited := api.exited
	code := api.waitCode
	api.mu.Unlock()
	select {
	case <-exited:
		return code, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (api *fakeEngine) InspectContainer(context.Context, string) (engineContainerState, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.containerInspectErr != nil {
		return "", api.containerInspectErr
	}
	if !api.containerExists {
		return "", errdefs.ErrNotFound
	}
	return api.containerState, nil
}

func (api *fakeEngine) CopyFile(_ context.Context, _ string, sourcePath string, _ int64) ([]byte, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if !api.containerExists || sourcePath != sandboxinit.ChildStartReceiptPath || api.receiptMissing {
		return nil, errdefs.ErrNotFound
	}
	if api.receiptOverride != nil {
		return slices.Clone(api.receiptOverride), api.receiptErr
	}
	if api.receipt == nil {
		return nil, errdefs.ErrNotFound
	}
	receipt := slices.Clone(api.receipt)
	if api.receiptTransform != nil {
		receipt = api.receiptTransform(receipt)
	}
	return receipt, api.receiptErr
}

func (api *fakeEngine) StopContainer(context.Context, string, time.Duration) error {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.stopCalls++
	if !api.containerExists || api.containerState == engineContainerExited {
		return nil
	}
	if !api.stopLeavesRunning {
		api.containerState = api.stopState
		if api.containerState == "" {
			api.containerState = engineContainerExited
		}
		api.exitOnce.Do(func() { close(api.exited) })
	}
	return nil
}

func (api *fakeEngine) SignalContainer(_ context.Context, _ string, signal string) error {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.record("signal:" + signal)
	if signal == "SIGUSR1" {
		api.childStartAcknowledgements++
		return api.childStartAckErr
	}
	if signal == "SIGKILL" {
		api.killCalls++
		if api.containerExists && !api.killLeavesRunning {
			api.containerState = engineContainerExited
			api.exitOnce.Do(func() { close(api.exited) })
		}
		return nil
	}
	return errors.New("unsupported fake Docker signal")
}

func (api *fakeEngine) KillContainer(ctx context.Context, id string) error {
	return api.SignalContainer(ctx, id, "SIGKILL")
}

func (api *fakeEngine) RemoveContainer(context.Context, string) error {
	api.mu.Lock()
	defer api.mu.Unlock()
	if !api.containerExists {
		return errdefs.ErrNotFound
	}
	api.containerExists = false
	api.removeContainerCalls++
	return nil
}

func (api *fakeEngine) ListNetworks(_ context.Context, labels map[string]string) ([]engineNetwork, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.record("list-networks")
	if api.networkListEntered != nil {
		select {
		case api.networkListEntered <- struct{}{}:
		default:
		}
	}
	result := make([]engineNetwork, 0, api.extraNetworks+1)
	for index := 0; index < api.extraNetworks; index++ {
		result = append(result, engineNetwork{ID: "provider-detail-extra-network"})
	}
	if api.networkExists && labelsContain(api.networkLabels, labels) {
		result = append(result, engineNetwork{ID: "provider-detail-network"})
	}
	return result, nil
}

func (api *fakeEngine) CreateNetwork(_ context.Context, _ string, labels map[string]string) (string, error) {
	api.mu.Lock()
	api.record("create-network")
	createErr := api.networkCreateErr
	persists := api.networkPersistsOnError
	delayedRelease, delayedDone := api.delayedNetworkRelease, api.delayedNetworkDone
	labels = cloneStrings(labels)
	if createErr == nil || persists {
		api.networkExists = true
		api.networkLabels = labels
	}
	api.mu.Unlock()
	if createErr != nil && delayedRelease != nil {
		go func() {
			<-delayedRelease
			api.mu.Lock()
			api.networkExists = true
			api.networkLabels = labels
			api.mu.Unlock()
			close(delayedDone)
		}()
	}
	if createErr != nil {
		return "", createErr
	}
	return "provider-detail-network", nil
}

func (api *fakeEngine) RemoveNetwork(context.Context, string) error {
	api.mu.Lock()
	defer api.mu.Unlock()
	if !api.networkExists {
		return errdefs.ErrNotFound
	}
	api.networkExists = false
	api.removeNetworkCalls++
	return nil
}

func (api *fakeEngine) Close() error { return nil }

func multiplexedOutput(stdout, stderr []byte) []byte {
	var encoded bytes.Buffer
	writeMultiplexedFrame(&encoded, 1, stdout)
	writeMultiplexedFrame(&encoded, 2, stderr)
	return encoded.Bytes()
}

func writeMultiplexedFrame(target *bytes.Buffer, stream byte, contents []byte) {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(contents)))
	_, _ = target.Write(header)
	_, _ = target.Write(contents)
}

func labelsContain(values, subset map[string]string) bool {
	for key, value := range subset {
		if values[key] != value {
			return false
		}
	}
	return true
}

func cloneStrings(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func receiptFromBootstrapArchive(encoded []byte) []byte {
	archive := tar.NewReader(bytes.NewReader(encoded))
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil || header == nil {
			return nil
		}
		if header.Name != "run/paje/command.json" {
			continue
		}
		value, err := io.ReadAll(archive)
		if err != nil {
			return nil
		}
		var document sandboxinit.Document
		if err := json.Unmarshal(value, &document); err != nil {
			return nil
		}
		receipt, err := json.Marshal(document.ChildStartReceipt)
		if err != nil {
			return nil
		}
		return receipt
	}
}

var _ engineClient = (*fakeEngine)(nil)
