package dockerengine

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/sandboxinit"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	mobyclient "github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	defaultStopTimeout  = 5 * time.Second
	defaultKillTimeout  = 5 * time.Second
	maxLifecycleTimeout = time.Minute
	maxRegistryAuth     = 1 << 20
)

var errPrivateReceiptOutcomeUncertain = errors.New("Docker private receipt reader outcome is uncertain")

type Config struct {
	Endpoint     string
	RegistryAuth string
	StopTimeout  time.Duration
	KillTimeout  time.Duration
}

type engineInfo struct {
	OSType     string
	APIVersion string
}

type imageInfo struct {
	RepositoryDigests []string
	OS                string
	Architecture      string
	Variant           string
}

type pullRequest struct {
	Reference    string
	Platform     string
	RegistryAuth string
}

type engineContainer struct {
	ID     string
	Labels map[string]string
}

type engineNetwork struct {
	ID string
}

type engineContainerState string

const (
	engineContainerCreated    engineContainerState = "created"
	engineContainerRunning    engineContainerState = "running"
	engineContainerPaused     engineContainerState = "paused"
	engineContainerRestarting engineContainerState = "restarting"
	engineContainerExited     engineContainerState = "exited"
	engineContainerDead       engineContainerState = "dead"
	engineContainerRemoving   engineContainerState = "removing"
)

type engineClient interface {
	Ping(context.Context) (engineInfo, error)
	InspectImage(context.Context, string, ocispec.Platform) (imageInfo, error)
	PullImage(context.Context, pullRequest) error
	ListContainers(context.Context, map[string]string) ([]engineContainer, error)
	CreateContainer(context.Context, mobyclient.ContainerCreateOptions) (string, error)
	AttachContainer(context.Context, string) (attachedContainerIO, error)
	StartContainer(context.Context, string) error
	WaitContainer(context.Context, string) (int64, error)
	InspectContainer(context.Context, string) (engineContainerState, error)
	CopyFile(context.Context, string, string, int64) ([]byte, error)
	SignalContainer(context.Context, string, string) error
	StopContainer(context.Context, string, time.Duration) error
	KillContainer(context.Context, string) error
	RemoveContainer(context.Context, string) error
	ListNetworks(context.Context, map[string]string) ([]engineNetwork, error)
	CreateNetwork(context.Context, string, map[string]string) (string, error)
	RemoveNetwork(context.Context, string) error
	Close() error
}

type attachedContainerIO interface {
	io.Reader
	io.Writer
	io.Closer
	CloseWrite() error
	SetWriteDeadline(time.Time) error
}

type mobyEngine struct {
	client *mobyclient.Client
}

func New(config Config) (executor.Executor, error) {
	if _, err := unixSocketPath(config.Endpoint); err != nil {
		return nil, err
	}
	client, err := mobyclient.New(mobyclient.WithHost(config.Endpoint))
	if err != nil {
		return nil, errors.New("construct Docker Engine client")
	}
	target, err := newWithEngine(config, &mobyEngine{client: client})
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	return target, nil
}

func newWithEngine(config Config, engine engineClient) (*Executor, error) {
	if _, err := unixSocketPath(config.Endpoint); err != nil {
		return nil, err
	}
	if engine == nil {
		return nil, errors.New("Docker Engine client is nil")
	}
	if len(config.RegistryAuth) > maxRegistryAuth || strings.IndexByte(config.RegistryAuth, 0) >= 0 {
		return nil, errors.New("Docker registry authorization is invalid")
	}
	if config.StopTimeout == 0 {
		config.StopTimeout = defaultStopTimeout
	}
	if config.KillTimeout == 0 {
		config.KillTimeout = defaultKillTimeout
	}
	if config.StopTimeout < 0 || config.KillTimeout < 0 ||
		config.StopTimeout > maxLifecycleTimeout || config.KillTimeout > maxLifecycleTimeout {
		return nil, errors.New("Docker Engine lifecycle timeouts are invalid")
	}
	return &Executor{
		engine:         engine,
		registryAuth:   config.RegistryAuth,
		stopTimeout:    config.StopTimeout,
		killTimeout:    config.KillTimeout,
		attempts:       make(map[string]*attemptRecord),
		destroyedOrder: make([]string, 0, destroyedHistoryLimit),
	}, nil
}

func unixSocketPath(endpoint string) (string, error) {
	if endpoint == "" || !strings.HasPrefix(endpoint, "unix://") {
		return "", errors.New("Docker Engine endpoint must be an explicit local Unix socket")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "unix" || parsed.Host != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" || parsed.RawPath != "" ||
		parsed.Path == "" || !filepath.IsAbs(parsed.Path) ||
		strings.IndexByte(parsed.Path, 0) >= 0 ||
		filepath.Clean(parsed.Path) != parsed.Path || parsed.Path == string(filepath.Separator) {
		return "", errors.New("Docker Engine endpoint must be an absolute local Unix socket")
	}
	return parsed.Path, nil
}

func (engine *mobyEngine) Ping(ctx context.Context) (engineInfo, error) {
	result, err := engine.client.Ping(ctx, mobyclient.PingOptions{NegotiateAPIVersion: true})
	return engineInfo{OSType: result.OSType, APIVersion: result.APIVersion}, err
}

func (engine *mobyEngine) InspectImage(ctx context.Context, reference string, platform ocispec.Platform) (imageInfo, error) {
	result, err := engine.client.ImageInspect(ctx, reference, mobyclient.ImageInspectWithPlatform(&platform))
	if err != nil {
		return imageInfo{}, err
	}
	return imageInfo{
		RepositoryDigests: append([]string(nil), result.RepoDigests...),
		OS:                result.Os,
		Architecture:      result.Architecture,
		Variant:           result.Variant,
	}, nil
}

func (engine *mobyEngine) PullImage(ctx context.Context, request pullRequest) (returnedErr error) {
	platform, err := parsePlatform(request.Platform)
	if err != nil {
		return err
	}
	response, err := engine.client.ImagePull(ctx, request.Reference, mobyclient.ImagePullOptions{
		RegistryAuth: request.RegistryAuth,
		Platforms:    []ocispec.Platform{platform},
	})
	if err != nil {
		return err
	}
	defer func() {
		returnedErr = errors.Join(returnedErr, response.Close())
	}()
	return response.Wait(ctx)
}

func (engine *mobyEngine) ListContainers(ctx context.Context, labels map[string]string) ([]engineContainer, error) {
	filters := mobyclient.Filters{}
	for _, label := range sortedLabelFilters(labels) {
		filters = filters.Add("label", label)
	}
	result, err := engine.client.ContainerList(ctx, mobyclient.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}
	containers := make([]engineContainer, len(result.Items))
	for index := range result.Items {
		containers[index] = engineContainer{ID: result.Items[index].ID, Labels: cloneStringMap(result.Items[index].Labels)}
	}
	return containers, nil
}

func (engine *mobyEngine) CreateContainer(ctx context.Context, options mobyclient.ContainerCreateOptions) (string, error) {
	result, err := engine.client.ContainerCreate(ctx, options)
	return result.ID, err
}

type attachedStream struct {
	response *mobyclient.HijackedResponse
}

func (stream *attachedStream) Read(buffer []byte) (int, error) {
	return stream.response.Reader.Read(buffer)
}

func (stream *attachedStream) Write(buffer []byte) (int, error) {
	return stream.response.Conn.Write(buffer)
}

func (stream *attachedStream) CloseWrite() error {
	return stream.response.CloseWrite()
}

func (stream *attachedStream) SetWriteDeadline(deadline time.Time) error {
	return stream.response.Conn.SetWriteDeadline(deadline)
}

func (stream *attachedStream) Close() error {
	stream.response.Close()
	return nil
}

func (engine *mobyEngine) AttachContainer(ctx context.Context, id string) (attachedContainerIO, error) {
	result, err := engine.client.ContainerAttach(ctx, id, mobyclient.ContainerAttachOptions{
		Stream: true, Stdin: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return nil, err
	}
	if result.Reader == nil || result.Conn == nil {
		result.Close()
		return nil, errors.New("Docker Engine attach returned no stream")
	}
	return &attachedStream{response: &result.HijackedResponse}, nil
}

func (engine *mobyEngine) StartContainer(ctx context.Context, id string) error {
	_, err := engine.client.ContainerStart(ctx, id, mobyclient.ContainerStartOptions{})
	return err
}

func (engine *mobyEngine) WaitContainer(ctx context.Context, id string) (int64, error) {
	result := engine.client.ContainerWait(ctx, id, mobyclient.ContainerWaitOptions{Condition: "not-running"})
	select {
	case response, ok := <-result.Result:
		if !ok {
			return 0, errors.New("Docker Engine wait result closed")
		}
		if response.Error != nil {
			return response.StatusCode, errors.New("Docker Engine wait failed")
		}
		return response.StatusCode, nil
	case err, ok := <-result.Error:
		if !ok {
			return 0, errors.New("Docker Engine wait error closed")
		}
		return 0, err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (engine *mobyEngine) InspectContainer(ctx context.Context, id string) (engineContainerState, error) {
	result, err := engine.client.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	if result.Container.State == nil {
		return "", nil
	}
	return engineContainerState(result.Container.State.Status), nil
}

func (engine *mobyEngine) CopyFile(ctx context.Context, id, sourcePath string, limit int64) ([]byte, error) {
	if limit <= 0 || sourcePath != sandboxinit.ChildStartReceiptPath || !filepath.IsAbs(sourcePath) ||
		filepath.Clean(sourcePath) != sourcePath || strings.IndexByte(sourcePath, 0) >= 0 {
		return nil, errors.New("Docker copy limit is invalid")
	}
	created, err := engine.client.ExecCreate(ctx, id, mobyclient.ExecCreateOptions{
		User:         "65532:65532",
		AttachStdout: true,
		AttachStderr: true,
		Env:          []string{"LC_ALL=C"},
		Cmd:          []string{"/usr/local/bin/paje-sandbox-init", "--emit-child-start-receipt"},
	})
	if err != nil {
		return nil, err
	}
	if created.ID == "" {
		return nil, errors.New("Docker receipt reader exec has no identity")
	}
	attached, err := engine.client.ExecAttach(ctx, created.ID, mobyclient.ExecAttachOptions{})
	if err != nil {
		return nil, err
	}
	defer attached.Close()
	return readPrivateReceiptExecOutput(attached.Reader, limit, func() (mobyclient.ExecInspectResult, error) {
		return engine.client.ExecInspect(ctx, created.ID, mobyclient.ExecInspectOptions{})
	})
}

func readPrivateReceiptExecOutput(
	reader io.Reader,
	limit int64,
	inspect func() (mobyclient.ExecInspectResult, error),
) ([]byte, error) {
	stdout, stderr, stdoutTruncated, stderrTruncated, streamErr := readPrivateReceiptStreams(reader, limit)
	if streamErr != nil && !benignReceiptStreamClose(streamErr) {
		return nil, errors.New("read Docker private receipt")
	}
	candidate := stdout
	inspected, inspectErr := inspect()
	if inspectErr != nil {
		if len(candidate) == 0 || stdoutTruncated || stderrTruncated || len(stderr) != 0 {
			return nil, errors.New("Docker private receipt reader outcome is uncertain")
		}
		return candidate, errors.Join(
			errPrivateReceiptOutcomeUncertain,
			inspectErr,
		)
	}
	if inspected.Running {
		return nil, errors.New("Docker private receipt reader remains running")
	}
	if inspected.ExitCode == 1 {
		if len(candidate) == 0 && !stdoutTruncated && !stderrTruncated {
			return nil, errdefs.ErrNotFound
		}
		return nil, errors.New("Docker private receipt reader failed")
	}
	if stderrTruncated || len(stderr) != 0 || stdoutTruncated {
		return nil, errors.New("Docker private receipt reader failed")
	}
	if inspected.ExitCode != 0 {
		return nil, errors.New("Docker private receipt reader failed")
	}
	if len(candidate) == 0 {
		return nil, errors.New("Docker private receipt reader returned no receipt")
	}
	if streamErr != nil {
		return candidate, errors.Join(
			errPrivateReceiptOutcomeUncertain,
			errors.New("Docker private receipt stream ended after output"),
			streamErr,
		)
	}
	return candidate, nil
}

func readPrivateReceiptStreams(reader io.Reader, stdoutLimit int64) (
	stdout []byte,
	stderr []byte,
	stdoutTruncated bool,
	stderrTruncated bool,
	returnedErr error,
) {
	if stdoutLimit <= 0 || stdoutLimit > maxChildReceiptBytes {
		return nil, nil, false, false, errors.New("Docker private receipt limit is invalid")
	}
	const (
		frameHeaderBytes = 8
		stderrLimit      = 4096
	)
	var header [frameHeaderBytes]byte
	for {
		_, err := io.ReadFull(reader, header[:])
		if errors.Is(err, io.EOF) {
			return stdout, stderr, false, false, nil
		}
		if err != nil {
			return stdout, stderr, false, false, err
		}
		if header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return stdout, stderr, false, false, errors.New("Docker private receipt stream header is invalid")
		}
		frameSize := int64(binary.BigEndian.Uint32(header[4:]))
		var target *[]byte
		switch stdcopy.StdType(header[0]) {
		case stdcopy.Stdout:
			if frameSize > stdoutLimit-int64(len(stdout)) {
				return stdout, stderr, true, false, errors.New("Docker private receipt stdout exceeds limit")
			}
			target = &stdout
		case stdcopy.Stderr:
			if frameSize > stderrLimit-int64(len(stderr)) {
				return stdout, stderr, false, true, errors.New("Docker private receipt stderr exceeds limit")
			}
			target = &stderr
		default:
			return stdout, stderr, false, false, errors.New("Docker private receipt stream type is invalid")
		}
		start := len(*target)
		*target = append(*target, make([]byte, int(frameSize))...)
		if _, err := io.ReadFull(reader, (*target)[start:]); err != nil {
			return stdout, stderr, false, false, err
		}
	}
}

func benignReceiptStreamClose(err error) bool {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	if err == nil {
		return false
	}
	const closedNetworkConnection = "use of closed network connection"
	return err.Error() == closedNetworkConnection ||
		strings.HasSuffix(err.Error(), ": "+closedNetworkConnection)
}

func (engine *mobyEngine) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	seconds := int(timeout / time.Second)
	if timeout > 0 && timeout%time.Second != 0 {
		seconds++
	}
	_, err := engine.client.ContainerStop(ctx, id, mobyclient.ContainerStopOptions{Timeout: &seconds})
	return err
}

func (engine *mobyEngine) SignalContainer(ctx context.Context, id, signal string) error {
	if signal == "" || strings.IndexByte(signal, 0) >= 0 {
		return errors.New("Docker container signal is invalid")
	}
	_, err := engine.client.ContainerKill(ctx, id, mobyclient.ContainerKillOptions{Signal: signal})
	return err
}

func (engine *mobyEngine) KillContainer(ctx context.Context, id string) error {
	return engine.SignalContainer(ctx, id, "SIGKILL")
}

func (engine *mobyEngine) RemoveContainer(ctx context.Context, id string) error {
	_, err := engine.client.ContainerRemove(ctx, id, mobyclient.ContainerRemoveOptions{
		Force: true, RemoveVolumes: true,
	})
	return err
}

func (engine *mobyEngine) ListNetworks(ctx context.Context, labels map[string]string) ([]engineNetwork, error) {
	filters := mobyclient.Filters{}
	for _, label := range sortedLabelFilters(labels) {
		filters = filters.Add("label", label)
	}
	result, err := engine.client.NetworkList(ctx, mobyclient.NetworkListOptions{Filters: filters})
	if err != nil {
		return nil, err
	}
	networks := make([]engineNetwork, len(result.Items))
	for index := range result.Items {
		networks[index] = engineNetwork{ID: result.Items[index].ID}
	}
	return networks, nil
}

func (engine *mobyEngine) CreateNetwork(ctx context.Context, name string, labels map[string]string) (string, error) {
	result, err := engine.client.NetworkCreate(ctx, name, mobyclient.NetworkCreateOptions{
		Driver: "bridge", Scope: "local", Labels: labels,
	})
	return result.ID, err
}

func (engine *mobyEngine) RemoveNetwork(ctx context.Context, id string) error {
	_, err := engine.client.NetworkRemove(ctx, id, mobyclient.NetworkRemoveOptions{})
	return err
}

func (engine *mobyEngine) Close() error { return engine.client.Close() }

func sortedLabelFilters(labels map[string]string) []string {
	result := make([]string, 0, len(labels))
	for key, value := range labels {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}

var _ engineClient = (*mobyEngine)(nil)
