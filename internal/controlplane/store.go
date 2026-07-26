package controlplane

import "context"

// Store persists one authoritative Snapshot with compare-and-swap semantics
// and exposes its append-only event stream by monotonic cursor.
type Store interface {
	Create(context.Context, Snapshot) (Snapshot, error)
	Load(context.Context, string) (Snapshot, error)
	Save(context.Context, Snapshot, uint64) (Snapshot, error)
	EventsAfter(context.Context, string, uint64, int) ([]Event, uint64, error)
}
