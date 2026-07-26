package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
	"gopkg.in/yaml.v3"
)

const (
	maxBindingFileBytes = 1 << 20
	kindSecretBindings  = "SecretBindings"
)

type Registry struct {
	directory          string
	environmentTargets map[string]struct{}

	mu       sync.RWMutex
	bindings map[secret.BindingRef]secret.Binding
}

type Config struct {
	AllowedEnvironmentTargets []string
}

func New(directory string, configs ...Config) (*Registry, error) {
	if directory == "" {
		return nil, errors.New("secret binding directory is required")
	}
	if len(configs) > 1 {
		return nil, errors.New("multiple secret binding configurations provided")
	}
	config := Config{}
	if len(configs) == 1 {
		config = configs[0]
	}
	targets := make(map[string]struct{}, len(config.AllowedEnvironmentTargets))
	for _, target := range config.AllowedEnvironmentTargets {
		if err := secret.ValidateEnvironmentTarget(target); err != nil {
			return nil, err
		}
		if _, duplicate := targets[target]; duplicate {
			return nil, errors.New("duplicate secret environment target allowlist entry")
		}
		targets[target] = struct{}{}
	}
	registry := &Registry{
		directory: directory, environmentTargets: targets,
		bindings: make(map[secret.BindingRef]secret.Binding),
	}
	if err := registry.Reload(context.Background()); err != nil {
		return nil, err
	}
	return registry, nil
}

func (registry *Registry) Reload(ctx context.Context) error {
	loaded, err := loadDirectory(ctx, registry.directory, registry.environmentTargets)
	if err != nil {
		return err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	for ref, binding := range loaded {
		if existing, ok := registry.bindings[ref]; ok && !existing.Equal(binding) {
			return errors.New("secret binding revision is immutable")
		}
	}
	next := make(map[secret.BindingRef]secret.Binding, len(registry.bindings)+len(loaded))
	for ref, binding := range registry.bindings {
		next[ref] = binding
	}
	for ref, binding := range loaded {
		next[ref] = binding
	}
	registry.bindings = next
	return nil
}

func (registry *Registry) Resolve(ctx context.Context, request secret.ResolveRequest) (secret.Binding, error) {
	if err := ctx.Err(); err != nil {
		return secret.Binding{}, err
	}
	registry.mu.RLock()
	binding, ok := registry.bindings[request.Ref]
	registry.mu.RUnlock()
	if !ok {
		return secret.Binding{}, secret.ErrBindingNotFound
	}
	if !binding.Authorizes(request) {
		return secret.Binding{}, secret.ErrBindingUnauthorized
	}
	return binding, nil
}

type document struct {
	APIVersion string                     `yaml:"api_version"`
	Kind       string                     `yaml:"kind"`
	Bindings   map[string]bindingDocument `yaml:"bindings"`
}

type bindingDocument struct {
	Revision  uint64                `yaml:"revision"`
	Authorize authorizationDocument `yaml:"authorize"`
	Source    sourceDocument        `yaml:"source"`
}

type authorizationDocument struct {
	Profile  string `yaml:"profile"`
	Stage    string `yaml:"stage"`
	Delivery string `yaml:"delivery"`
	Target   string `yaml:"target"`
}

type sourceDocument struct {
	Provider  string `yaml:"provider"`
	Reference string `yaml:"reference"`
}

func loadDirectory(ctx context.Context, directory string, environmentTargets map[string]struct{}) (map[secret.BindingRef]secret.Binding, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("load secret bindings: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("secret binding directory is invalid")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("load secret bindings: %w", err)
	}
	bindings := make(map[secret.BindingRef]secret.Binding)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension != ".yaml" && extension != ".yml" {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil, errors.New("secret binding file must be regular")
		}
		loaded, err := loadFile(filepath.Join(directory, entry.Name()), environmentTargets)
		if err != nil {
			return nil, err
		}
		for ref, binding := range loaded {
			if _, duplicate := bindings[ref]; duplicate {
				return nil, errors.New("duplicate secret binding revision")
			}
			bindings[ref] = binding
		}
	}
	return bindings, nil
}

func loadFile(filename string, environmentTargets map[string]struct{}) (map[secret.BindingRef]secret.Binding, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, fmt.Errorf("load secret bindings: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxBindingFileBytes {
		return nil, errors.New("secret binding file is invalid")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("load secret bindings: %w", err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(io.LimitReader(file, maxBindingFileBytes+1))
	decoder.KnownFields(true)
	var raw document
	if err := decoder.Decode(&raw); err != nil {
		return nil, errors.New("secret binding document is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("secret binding file must contain exactly one document")
	}
	if raw.APIVersion != workerprofile.APIVersionV1Alpha1 || raw.Kind != kindSecretBindings || len(raw.Bindings) == 0 {
		return nil, errors.New("secret binding document is invalid")
	}

	bindings := make(map[secret.BindingRef]secret.Binding, len(raw.Bindings))
	for capability, entry := range raw.Bindings {
		profileID, err := workerprofile.ParseProfileID(entry.Authorize.Profile)
		if err != nil {
			return nil, errors.New("secret binding document is invalid")
		}
		ref := secret.BindingRef{Capability: capability, Revision: entry.Revision}
		binding, err := secret.NewBinding(ref, secret.Authorization{
			ProfileID: profileID,
			Stage:     entry.Authorize.Stage,
			Delivery:  entry.Authorize.Delivery,
			Target:    entry.Authorize.Target,
		}, entry.Source.Provider, entry.Source.Reference)
		if err != nil {
			return nil, errors.New("secret binding document is invalid")
		}
		if entry.Authorize.Delivery == workerprofile.DeliveryEnvironment {
			if _, allowed := environmentTargets[entry.Authorize.Target]; !allowed {
				return nil, errors.New("secret binding environment target is not allowlisted")
			}
		}
		bindings[ref] = binding
	}
	return bindings, nil
}
