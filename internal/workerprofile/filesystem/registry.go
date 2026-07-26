package filesystem

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/araihu/paje/internal/workerprofile"
	"gopkg.in/yaml.v3"
)

const maxWorkerProfileFileBytes = 1 << 20

type Registry struct {
	directory string
	limits    workerprofile.Limits

	mu       sync.RWMutex
	profiles map[workerprofile.ProfileID]workerprofile.Snapshot
	digests  map[workerprofile.ProfileID]string
}

func New(directory string, limits workerprofile.Limits) (*Registry, error) {
	if directory == "" {
		return nil, errors.New("worker profile directory is required")
	}
	registry := &Registry{directory: directory, limits: limits}
	if err := registry.Reload(context.Background()); err != nil {
		return nil, err
	}
	return registry, nil
}

func (registry *Registry) Reload(ctx context.Context) error {
	profiles, err := load(ctx, registry.directory, registry.limits)
	if err != nil {
		return err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	nextDigests := make(map[workerprofile.ProfileID]string, len(registry.digests)+len(profiles))
	for id, digest := range registry.digests {
		nextDigests[id] = digest
	}
	for id, profile := range profiles {
		if digest, ok := nextDigests[id]; ok && digest != profile.Digest {
			return errors.New("worker profile revision is immutable")
		}
		nextDigests[id] = profile.Digest
	}
	registry.profiles = profiles
	registry.digests = nextDigests
	return nil
}

func (registry *Registry) Resolve(ctx context.Context, id workerprofile.ProfileID) (workerprofile.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return workerprofile.Snapshot{}, err
	}
	registry.mu.RLock()
	snapshot, ok := registry.profiles[id]
	registry.mu.RUnlock()
	if !ok {
		return workerprofile.Snapshot{}, workerprofile.ErrProfileNotFound
	}
	return snapshot.Clone(), nil
}

func load(ctx context.Context, directory string, limits workerprofile.Limits) (map[workerprofile.ProfileID]workerprofile.Snapshot, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("load worker profiles: %w", err)
	}
	profiles := make(map[workerprofile.ProfileID]workerprofile.Snapshot)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension != ".yaml" && extension != ".yml" {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil, errors.New("worker profile must be a regular file")
		}
		snapshot, err := decode(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, errors.New("worker profile document is invalid")
		}
		normalized, err := workerprofile.CanonicalizeWithLimits(snapshot, limits)
		if err != nil {
			return nil, errors.New("worker profile document is invalid")
		}
		if _, duplicate := profiles[normalized.Metadata]; duplicate {
			return nil, errors.New("duplicate worker profile identity")
		}
		profiles[normalized.Metadata] = normalized
	}
	return profiles, nil
}

func decode(filename string) (workerprofile.Snapshot, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return workerprofile.Snapshot{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 || info.Size() > maxWorkerProfileFileBytes {
		return workerprofile.Snapshot{}, errors.New("worker profile file is invalid")
	}
	file, err := os.Open(filename)
	if err != nil {
		return workerprofile.Snapshot{}, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) ||
		openedInfo.Size() <= 0 || openedInfo.Size() > maxWorkerProfileFileBytes {
		return workerprofile.Snapshot{}, errors.New("worker profile file is invalid")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxWorkerProfileFileBytes+1))
	if err != nil || len(contents) == 0 || len(contents) > maxWorkerProfileFileBytes {
		return workerprofile.Snapshot{}, errors.New("worker profile file is invalid")
	}
	finalInfo, err := file.Stat()
	if err != nil || !finalInfo.Mode().IsRegular() || !os.SameFile(openedInfo, finalInfo) ||
		finalInfo.Size() != int64(len(contents)) {
		return workerprofile.Snapshot{}, errors.New("worker profile file is invalid")
	}

	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	var snapshot workerprofile.Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return workerprofile.Snapshot{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return workerprofile.Snapshot{}, errors.New("multiple YAML documents")
		}
		return workerprofile.Snapshot{}, err
	}
	return snapshot, nil
}
