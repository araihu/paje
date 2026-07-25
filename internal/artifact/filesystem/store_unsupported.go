//go:build !darwin && !linux

// Package filesystem exposes a compile-safe unsupported boundary outside the
// Linux beta runtime and Darwin development environment.
package filesystem

import (
	"context"
	"errors"

	"github.com/araihu/paje/internal/artifact"
)

var errUnsupportedPlatform = errors.New("artifact filesystem store is unsupported on this platform")

// Store is unavailable on unsupported platforms.
type Store struct{}

var _ artifact.Store = (*Store)(nil)

// New reports that the descriptor-anchored filesystem store is unavailable.
func New(string, int64) (*Store, error) { return nil, errUnsupportedPlatform }

// Close is a no-op for the unavailable store.
func (*Store) Close() error { return nil }

// Save reports that the filesystem store is unavailable.
func (*Store) Save(context.Context, artifact.Bundle) (artifact.Reference, error) {
	return artifact.Reference{}, errUnsupportedPlatform
}

// Load reports that the filesystem store is unavailable.
func (*Store) Load(context.Context, artifact.Reference) (artifact.Bundle, error) {
	return artifact.Bundle{}, errUnsupportedPlatform
}
