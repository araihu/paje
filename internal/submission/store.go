package submission

import (
	"context"
	"time"
)

// Reservation atomically binds a principal-scoped client key to Record.
// IdempotencyKey is used only as an indexing input and must not be copied into
// the durable Record.
type Reservation struct {
	Record         Record
	IdempotencyKey string
}

// Store persists deterministic reservations, lineage, trigger references, and
// cancellation intent.
type Store interface {
	Reserve(context.Context, Reservation) (Record, bool, error)
	BindTrigger(context.Context, string, TriggerReference) (Record, error)
	Load(context.Context, string) (Record, error)
	LoadByKey(context.Context, string, string) (Record, error)
	MarkCancellationRequested(context.Context, string, time.Time) (Record, error)
}
