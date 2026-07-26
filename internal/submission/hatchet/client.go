package hatchet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	legacyclient "github.com/hatchet-dev/hatchet/pkg/client"
	"github.com/hatchet-dev/hatchet/pkg/client/rest"
	hatchetsdk "github.com/hatchet-dev/hatchet/sdks/go"
)

// Client is the minimum fakeable Hatchet surface required by Trigger. It
// deliberately contains no Hatchet SDK types.
type Client interface {
	Start(context.Context, string, map[string]any) (externalRunID string, err error)
	Details(context.Context, string) (Details, error)
	Cancel(context.Context, string) error
}

// RunStatus is the adapter-local projection of a Hatchet workflow or task
// status.
type RunStatus string

const (
	RunStatusQueued    RunStatus = "QUEUED"
	RunStatusRunning   RunStatus = "RUNNING"
	RunStatusFailed    RunStatus = "FAILED"
	RunStatusCanceled  RunStatus = "CANCELLED"
	RunStatusCompleted RunStatus = "COMPLETED"
)

// TaskDetails contains only the task fields needed to validate finalization.
type TaskDetails struct {
	Status RunStatus
	Output json.RawMessage
}

// Details contains only the workflow fields needed by the provider-neutral
// trigger adapter.
type Details struct {
	ExternalRunID string
	Status        RunStatus
	Input         json.RawMessage
	Finalize      *TaskDetails
	Done          bool
}

// IdempotencyCollisionError identifies the existing workflow run that owns a
// provider idempotency key. Its public message deliberately omits provider
// response data.
type IdempotencyCollisionError struct {
	ExternalRunID string
}

func (e *IdempotencyCollisionError) Error() string {
	return "Hatchet workflow idempotency collision"
}

// SDKClient confines every Hatchet SDK type and call to this adapter package.
type SDKClient struct {
	client *hatchetsdk.Client
}

// NewSDKClient adapts a configured producer-only Hatchet client.
func NewSDKClient(client *hatchetsdk.Client) (*SDKClient, error) {
	if client == nil {
		return nil, fmt.Errorf("adapt Hatchet client: client is required")
	}
	return &SDKClient{client: client}, nil
}

func (c *SDKClient) Start(
	ctx context.Context,
	workflow string,
	envelope map[string]any,
) (string, error) {
	ref, err := c.client.RunNoWait(ctx, workflow, envelope)
	if err != nil {
		var collision *hatchetsdk.IdempotencyCollisionError
		if errors.As(err, &collision) {
			return "", &IdempotencyCollisionError{
				ExternalRunID: collision.ExistingRunExternalId,
			}
		}
		var legacyCollision *legacyclient.IdempotencyViolationErr
		if errors.As(err, &legacyCollision) {
			return "", &IdempotencyCollisionError{
				ExternalRunID: legacyCollision.ExistingRunExternalId,
			}
		}
		return "", err
	}
	if ref == nil {
		return "", fmt.Errorf("Hatchet returned an empty workflow reference")
	}
	return ref.RunId, nil
}

func (c *SDKClient) Details(ctx context.Context, externalRunID string) (Details, error) {
	runID, err := uuid.Parse(externalRunID)
	if err != nil {
		return Details{}, fmt.Errorf("parse Hatchet workflow run ID: %w", err)
	}
	details, err := c.client.Runs().GetDetails(ctx, runID)
	if err != nil {
		return Details{}, err
	}
	if details == nil {
		return Details{}, fmt.Errorf("Hatchet returned empty workflow details")
	}

	result := Details{
		ExternalRunID: details.ExternalId.String(),
		Status:        RunStatus(details.Status),
		Input:         append(json.RawMessage(nil), details.Input...),
		Done:          details.Done,
	}
	if finalize, exists := details.TaskRuns["finalize"]; exists && finalize != nil {
		result.Finalize = &TaskDetails{
			Status: RunStatus(finalize.Status),
			Output: append(json.RawMessage(nil), finalize.Output...),
		}
	}
	return result, nil
}

func (c *SDKClient) Cancel(ctx context.Context, externalRunID string) error {
	runID, err := uuid.Parse(externalRunID)
	if err != nil {
		return fmt.Errorf("parse Hatchet workflow run ID: %w", err)
	}
	externalIDs := []uuid.UUID{runID}
	_, err = c.client.Runs().Cancel(ctx, rest.V1CancelTaskRequest{
		ExternalIds: &externalIDs,
	})
	return err
}

var _ Client = (*SDKClient)(nil)
