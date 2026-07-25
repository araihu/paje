package mock

import (
	"context"
	"sync"

	"github.com/araihu/paje/internal/workerprofile"
)

type Registry struct {
	mu        sync.Mutex
	snapshots map[workerprofile.ProfileID]workerprofile.Snapshot
	errors    map[workerprofile.ProfileID]error
	requests  []workerprofile.ProfileID
}

func NewRegistry(snapshots ...workerprofile.Snapshot) *Registry {
	registry := &Registry{
		snapshots: make(map[workerprofile.ProfileID]workerprofile.Snapshot, len(snapshots)),
		errors:    make(map[workerprofile.ProfileID]error),
	}
	for _, snapshot := range snapshots {
		registry.snapshots[snapshot.Metadata] = snapshot.Clone()
	}
	return registry
}

func (registry *Registry) Resolve(ctx context.Context, id workerprofile.ProfileID) (workerprofile.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return workerprofile.Snapshot{}, err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.requests = append(registry.requests, id)
	if err := registry.errors[id]; err != nil {
		return workerprofile.Snapshot{}, err
	}
	snapshot, ok := registry.snapshots[id]
	if !ok {
		return workerprofile.Snapshot{}, workerprofile.ErrProfileNotFound
	}
	return snapshot.Clone(), nil
}

func (registry *Registry) Set(snapshot workerprofile.Snapshot) {
	registry.mu.Lock()
	registry.snapshots[snapshot.Metadata] = snapshot.Clone()
	registry.mu.Unlock()
}

func (registry *Registry) SetError(id workerprofile.ProfileID, err error) {
	registry.mu.Lock()
	if err == nil {
		delete(registry.errors, id)
	} else {
		registry.errors[id] = err
	}
	registry.mu.Unlock()
}

func (registry *Registry) Requests() []workerprofile.ProfileID {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	requests := make([]workerprofile.ProfileID, len(registry.requests))
	copy(requests, registry.requests)
	return requests
}
