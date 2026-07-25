package workerprofile

import (
	"context"
	"errors"
)

var ErrProfileNotFound = errors.New("worker profile not found")

type Registry interface {
	Resolve(context.Context, ProfileID) (Snapshot, error)
}
