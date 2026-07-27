package dockerengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/sandboxinit"
)

const (
	childReceiptPollInterval = 10 * time.Millisecond
	maxChildReceiptBytes     = 1 << 20
)

var errChildStartReceiptNotObservable = errors.New("Docker child-start receipt is not yet observable")

func (target *Executor) waitForChildStart(
	ctx context.Context,
	containerID string,
	expected executor.ChildStartReceipt,
) (executor.ChildStartReceipt, error) {
	for {
		receipt, err := target.readChildStartReceipt(ctx, containerID)
		if err == nil {
			if !receipt.Matches(expected) {
				return executor.ChildStartReceipt{}, errors.New("Docker child-start receipt was rebound")
			}
			return receipt, nil
		}
		if !providerNotFound(err) && !errors.Is(err, errChildStartReceiptNotObservable) {
			return executor.ChildStartReceipt{}, err
		}
		state, inspectErr := target.engine.InspectContainer(ctx, containerID)
		if inspectErr != nil {
			return executor.ChildStartReceipt{}, errors.Join(err, inspectErr)
		}
		if containerTerminated(state) {
			return executor.ChildStartReceipt{}, errors.Join(err, errors.New("Docker child-start receipt is missing"))
		}
		timer := time.NewTimer(childReceiptPollInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return executor.ChildStartReceipt{}, errors.Join(err, ctx.Err())
		}
	}
}

func (target *Executor) readChildStartReceipt(ctx context.Context, containerID string) (executor.ChildStartReceipt, error) {
	encoded, copyErr := target.engine.CopyFile(ctx, containerID, sandboxinit.ChildStartReceiptPath, maxChildReceiptBytes)
	if copyErr != nil && len(encoded) == 0 {
		if errors.Is(copyErr, errPrivateReceiptOutcomeUncertain) {
			return executor.ChildStartReceipt{}, errChildStartReceiptNotObservable
		}
		return executor.ChildStartReceipt{}, copyErr
	}
	defer clear(encoded)
	invalid := func(cause error) error {
		if errors.Is(copyErr, errPrivateReceiptOutcomeUncertain) {
			return errChildStartReceiptNotObservable
		}
		return cause
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var receipt executor.ChildStartReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return executor.ChildStartReceipt{}, invalid(errors.New("decode Docker child-start receipt"))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return executor.ChildStartReceipt{}, invalid(errors.New("Docker child-start receipt has trailing content"))
	}
	if err := receipt.Validate(); err != nil {
		return executor.ChildStartReceipt{}, invalid(err)
	}
	canonical, err := json.Marshal(receipt)
	if err != nil {
		return executor.ChildStartReceipt{}, invalid(errors.New("encode canonical Docker child-start receipt"))
	}
	defer clear(canonical)
	if !bytes.Equal(encoded, canonical) {
		return executor.ChildStartReceipt{}, invalid(errors.New("Docker child-start receipt is not canonical"))
	}
	return receipt, nil
}

func validateReceiptLabels(container engineContainer, receipt executor.ChildStartReceipt) error {
	if container.Labels[labelCommandDigest] != receipt.CommandDigest ||
		container.Labels[labelEnvironmentDigest] != receipt.EnvironmentDigest ||
		container.Labels[labelReceiptBinding] != receipt.BindingDigest() {
		return errors.New("Docker child-start receipt does not match immutable resource labels")
	}
	return nil
}
