package dockerengine

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/araihu/paje/internal/workerprofile"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func (target *Executor) verifyImage(ctx context.Context, profile workerprofile.Snapshot) error {
	platform, err := parsePlatform(profile.Runtime.Platform)
	if err != nil {
		return wrapProvider("input", "platform", err)
	}
	info, err := target.engine.Ping(ctx)
	if err != nil {
		return wrapProvider("environment", "engine_unavailable", err)
	}
	if info.OSType != "linux" || !supportedEngineAPI(info.APIVersion) {
		return wrapProvider("environment", "engine_unsupported", errors.New("Docker Engine capabilities are unsupported"))
	}

	image, err := target.engine.InspectImage(ctx, profile.Runtime.Image, platform)
	if providerNotFound(err) {
		err = target.engine.PullImage(ctx, pullRequest{
			Reference: profile.Runtime.Image, Platform: profile.Runtime.Platform, RegistryAuth: target.registryAuth,
		})
		if err != nil {
			return wrapProvider("environment", "image_pull", err)
		}
		image, err = target.engine.InspectImage(ctx, profile.Runtime.Image, platform)
	}
	if err != nil {
		return wrapProvider("environment", "image_inspect", err)
	}
	if !slices.Contains(image.RepositoryDigests, profile.Runtime.Image) ||
		image.OS != platform.OS || image.Architecture != platform.Architecture ||
		image.Variant != platform.Variant {
		return wrapProvider("environment", "image_mismatch", errors.New("inspected image identity does not match profile"))
	}
	return nil
}

func supportedEngineAPI(version string) bool {
	majorText, minorText, ok := strings.Cut(version, ".")
	if !ok || strings.Contains(minorText, ".") {
		return false
	}
	major, majorErr := strconv.Atoi(majorText)
	minor, minorErr := strconv.Atoi(minorText)
	if majorErr != nil || minorErr != nil || major < 0 || minor < 0 {
		return false
	}
	return major > 1 || major == 1 && minor >= 49
}

func parsePlatform(value string) (ocispec.Platform, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] != "linux" || parts[1] == "" {
		return ocispec.Platform{}, errors.New("unsupported Docker platform")
	}
	return ocispec.Platform{OS: parts[0], Architecture: parts[1]}, nil
}
