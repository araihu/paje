package dockerengine

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/workerprofile"
	mobyclient "github.com/moby/moby/client"
)

func TestRealDockerCreateProbe(t *testing.T) {
	endpoint := os.Getenv("PAJE_DOCKER_TEST_ENDPOINT")
	image := os.Getenv("PAJE_DOCKER_TEST_IMAGE")
	if endpoint == "" || image == "" {
		t.Skip("set PAJE_DOCKER_TEST_ENDPOINT and PAJE_DOCKER_TEST_IMAGE to opt in to the real Docker probe")
	}
	if !strings.Contains(image, "@sha256:") {
		t.Fatal("PAJE_DOCKER_TEST_IMAGE must be an exact digest reference")
	}
	socketPath, err := unixSocketPath(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(socketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("configured Docker endpoint is not an available Unix socket: %v", err)
	}

	raw, err := mobyclient.New(mobyclient.WithHost(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	engine := &mobyEngine{client: raw}
	t.Cleanup(func() { _ = engine.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ping, err := engine.Ping(ctx)
	if err != nil || ping.OSType != "linux" || !supportedEngineAPI(ping.APIVersion) {
		t.Fatalf("real Docker Engine is unavailable or unsupported: %#v, %v", ping, err)
	}

	request := dockerRequest(t, workerprofile.NetworkNone, nil)
	defer request.Destroy()
	request.Attempt.RunID = fmt.Sprintf("real-docker-probe-%d", os.Getpid())
	request.Profile.Runtime.Image = image
	request.Profile.Digest = ""
	request.Profile, err = workerprofile.Canonicalize(request.Profile)
	if err != nil {
		t.Fatal(err)
	}
	platform, err := parsePlatform(request.Profile.Runtime.Platform)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := engine.InspectImage(ctx, image, platform)
	if err != nil {
		t.Fatalf("inspect opt-in probe image: %v", err)
	}
	if inspected.OS != platform.OS || inspected.Architecture != platform.Architecture {
		t.Fatalf("probe image platform = %s/%s", inspected.OS, inspected.Architecture)
	}
	options, err := containerOptions(request, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := engine.CreateContainer(ctx, options)
	if err != nil {
		t.Fatalf("real Docker rejected hardened container configuration: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = engine.RemoveContainer(cleanupCtx, id)
	})
}

func TestRealDockerPrivateReceiptReaderRepeated(t *testing.T) {
	endpoint := os.Getenv("PAJE_DOCKER_TEST_ENDPOINT")
	image := os.Getenv("PAJE_DOCKER_TEST_IMAGE")
	if endpoint == "" || image == "" {
		t.Skip("set PAJE_DOCKER_TEST_ENDPOINT and PAJE_DOCKER_TEST_IMAGE to opt in to the real Docker receipt-reader probe")
	}
	if !strings.Contains(image, "@sha256:") {
		t.Fatal("PAJE_DOCKER_TEST_IMAGE must be an exact digest reference")
	}

	raw, err := mobyclient.New(mobyclient.WithHost(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	engine := &mobyEngine{client: raw}
	t.Cleanup(func() { _ = engine.Close() })
	target, err := newWithEngine(Config{
		Endpoint: endpoint, StopTimeout: 5 * time.Second, KillTimeout: 5 * time.Second,
	}, engine)
	if err != nil {
		t.Fatal(err)
	}
	workspaceParent, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	for sequence := 1; sequence <= 20; sequence++ {
		request := dockerRequest(t, workerprofile.NetworkNone, nil)
		workspace, workspaceErr := os.MkdirTemp(workspaceParent, ".paje-receipt-reader-")
		if workspaceErr != nil {
			request.Destroy()
			t.Fatal(workspaceErr)
		}
		request.Workspace.HostPath = workspace
		request.Attempt = executor.AttemptID{
			RunID: fmt.Sprintf("real-docker-receipt-%d-%d", os.Getpid(), sequence),
			Stage: "execute", Attempt: 1, StartedAt: time.Now().UTC(),
			Purpose: executor.PurposeVerification, Sequence: sequence,
		}
		request.Profile.Runtime.Image = image
		request.Profile.Runtime.Platform = "linux/" + runtime.GOARCH
		request.Profile.Digest = ""
		request.Profile, err = workerprofile.Canonicalize(request.Profile)
		if err != nil {
			request.Destroy()
			_ = os.RemoveAll(workspace)
			t.Fatal(err)
		}
		request.Command = executor.Command{
			Executable: "go",
			Args:       []string{"version"},
			Directory:  executor.SandboxWorkspaceRoot,
		}

		result, executeErr := target.Execute(context.Background(), request)
		request.Destroy()
		cleanupErr := target.Destroy(context.Background(), request.Attempt)
		workspaceCleanupErr := os.RemoveAll(workspace)
		if executeErr != nil || !result.Started || !result.Completed || result.ChildStartReceipt == nil {
			t.Fatalf("Execute(sequence %d) = %#v, %v", sequence, result, executeErr)
		}
		if cleanupErr != nil {
			t.Fatalf("Destroy(sequence %d) = %v", sequence, cleanupErr)
		}
		if workspaceCleanupErr != nil {
			t.Fatalf("workspace cleanup(sequence %d) = %v", sequence, workspaceCleanupErr)
		}
		containers, listErr := engine.ListContainers(context.Background(), attemptLabels(request.Attempt, resourceContainer))
		if listErr != nil || len(containers) != 0 {
			t.Fatalf("receipt-reader cleanup(sequence %d) = %#v, %v", sequence, containers, listErr)
		}
		networks, networkErr := engine.ListNetworks(context.Background(), attemptLabels(request.Attempt, resourceNetwork))
		if networkErr != nil || len(networks) != 0 {
			t.Fatalf("receipt-reader network cleanup(sequence %d) = %#v, %v", sequence, networks, networkErr)
		}
		result.Destroy()
	}
}
