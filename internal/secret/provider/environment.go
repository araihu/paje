package provider

import (
	"context"
	"errors"
	"os"

	"github.com/araihu/paje/internal/secret"
)

type EnvironmentConfig struct {
	AllowedKeys []string
	MaxBytes    int
	Lookup      func(string) (string, bool)
}

type Environment struct {
	allowed  map[string]struct{}
	maxBytes int
	lookup   func(string) (string, bool)
}

func NewEnvironment(config EnvironmentConfig) (*Environment, error) {
	if config.MaxBytes <= 0 || len(config.AllowedKeys) == 0 {
		return nil, errors.New("environment secret allowlist and positive limit are required")
	}
	if config.Lookup == nil {
		config.Lookup = os.LookupEnv
	}
	allowed := make(map[string]struct{}, len(config.AllowedKeys))
	for _, key := range config.AllowedKeys {
		if !validEnvironmentKey(key) {
			return nil, errors.New("environment secret allowlist contains an invalid key")
		}
		if _, duplicate := allowed[key]; duplicate {
			return nil, errors.New("environment secret allowlist contains a duplicate key")
		}
		allowed[key] = struct{}{}
	}
	return &Environment{allowed: allowed, maxBytes: config.MaxBytes, lookup: config.Lookup}, nil
}

func (environment *Environment) Read(ctx context.Context, reference string) (secret.Payload, error) {
	if err := ctx.Err(); err != nil {
		return secret.Payload{}, err
	}
	if _, allowed := environment.allowed[reference]; !allowed || !validEnvironmentKey(reference) {
		return secret.Payload{}, secret.ErrSourceInvalid
	}
	value, ok := environment.lookup(reference)
	if !ok {
		return secret.Payload{}, secret.ErrSourceUnavailable
	}
	if len(value) == 0 {
		return secret.Payload{}, secret.ErrSourceInvalid
	}
	if len(value) > environment.maxBytes {
		return secret.Payload{}, secret.ErrSourceLimit
	}
	return secret.NewValuePayload([]byte(value)), nil
}

func validEnvironmentKey(key string) bool {
	if key == "" || len(key) > 128 || (key[0] != '_' && (key[0] < 'A' || key[0] > 'Z')) {
		return false
	}
	for _, character := range key[1:] {
		if character != '_' && (character < 'A' || character > 'Z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

var _ secret.Provider = (*Environment)(nil)
