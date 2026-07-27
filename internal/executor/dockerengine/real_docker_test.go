package dockerengine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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
