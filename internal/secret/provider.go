package secret

import (
	"context"
	"errors"
)

var (
	ErrSourceUnavailable = errors.New("secret source unavailable")
	ErrSourceInvalid     = errors.New("secret source invalid")
	ErrSourceLimit       = errors.New("secret source exceeds limits")
)

type Provider interface {
	Read(context.Context, string) (Payload, error)
}
