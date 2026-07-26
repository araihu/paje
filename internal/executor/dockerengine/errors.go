package dockerengine

import (
	"errors"

	"github.com/araihu/paje/internal/executor"
	"github.com/containerd/errdefs"
)

func wrapProvider(class, causeCode string, err error) error {
	if err == nil {
		return nil
	}
	return executor.WrapError(class, causeCode, err)
}

func resourceConflict(kind string) error {
	return wrapProvider("internal", "resource_conflict", errors.New("multiple attempt-owned "+kind+" resources"))
}

func providerNotFound(err error) bool {
	return errdefs.IsNotFound(err) || errors.Is(err, errdefs.ErrNotFound)
}

func providerConflict(err error) bool {
	return errdefs.IsConflict(err) || errors.Is(err, errdefs.ErrConflict)
}
