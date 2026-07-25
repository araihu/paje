// Package repository defines immutable revision resolution and repository profiles.
package repository

import "context"

// Revision records the requested reference and the immutable commit it resolved to.
type Revision struct {
	RepositoryURI string `json:"repository_uri"`
	Ref           string `json:"ref"`
	SHA           string `json:"sha"`
	SourceDirty   bool   `json:"source_dirty"`
}

// Resolver resolves a repository ref before workspace preparation.
type Resolver interface {
	Resolve(context.Context, string, string) (Revision, error)
}

// Profile inspects a prepared repository and compiles its verification commands.
type Profile interface {
	Name() string
	Inspect(context.Context, ProfileRequest) (ProfileResult, error)
}
