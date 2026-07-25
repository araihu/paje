package codechangehatchet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/araihu/paje/internal/approval"
	"github.com/araihu/paje/internal/run"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	"github.com/araihu/paje/internal/workflow/codechange"
	"github.com/hatchet-dev/hatchet/pkg/client/create"
)

const (
	resolveRetries  = 2
	executeRetries  = 2
	publishRetries  = 2
	finalizeRetries = 2
)

type phaseService interface {
	ResolveWithRunID(context.Context, string, json.RawMessage) (codechange.PhaseResult, error)
	Execute(context.Context, string) (codechange.PhaseResult, error)
	Approval(context.Context, string, approval.Gate) (codechange.PhaseResult, error)
	Publish(context.Context, string) (codechange.PhaseResult, error)
	Finalize(context.Context, string) (templatecodechange.Result, error)
	Exhaust(context.Context, string, string) (codechange.PhaseResult, error)
}

type taskContext interface {
	context.Context
	ParentOutput(create.NamedTask, any) error
	RetryCount() int
}

func resolveHandler(service phaseService) func(taskContext, map[string]any) (codechange.PhaseResult, error) {
	return func(ctx taskContext, input map[string]any) (codechange.PhaseResult, error) {
		runID, raw, err := decodeWorkflowInput(input)
		if err != nil {
			return codechange.PhaseResult{}, err
		}
		return runPhase(ctx, service, "resolve", resolveRetries, func() (codechange.PhaseResult, error) {
			return service.ResolveWithRunID(ctx, runID, raw)
		})
	}
}

type workflowInput struct {
	RunID string          `json:"run_id"`
	Input json.RawMessage `json:"input"`
}

func decodeWorkflowInput(input map[string]any) (string, json.RawMessage, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", nil, fmt.Errorf("encode Hatchet workflow input: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var envelope workflowInput
	if err := decoder.Decode(&envelope); err != nil {
		return "", nil, fmt.Errorf("decode Hatchet workflow input: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return "", nil, fmt.Errorf("decode Hatchet workflow input: %w", err)
	}
	if strings.TrimSpace(envelope.RunID) == "" {
		return "", nil, fmt.Errorf("decode Hatchet workflow input: run_id is required")
	}
	if len(envelope.Input) == 0 || bytes.Equal(envelope.Input, []byte("null")) {
		return "", nil, fmt.Errorf("decode Hatchet workflow input: input is required")
	}
	return envelope.RunID, envelope.Input, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func executeHandler(service phaseService) func(taskContext, map[string]any) (codechange.PhaseResult, error) {
	return func(ctx taskContext, _ map[string]any) (codechange.PhaseResult, error) {
		parent, err := parentResult(ctx, "resolve")
		if err != nil {
			return codechange.PhaseResult{}, err
		}
		return runPhase(ctx, service, "execute", executeRetries, func() (codechange.PhaseResult, error) {
			return service.Execute(ctx, parent.RunID)
		})
	}
}

func approvalHandler(service phaseService) func(durableTaskContext, map[string]any) (codechange.PhaseResult, error) {
	return func(ctx durableTaskContext, _ map[string]any) (codechange.PhaseResult, error) {
		parent, err := parentResult(ctx, "execute")
		if err != nil {
			return codechange.PhaseResult{}, err
		}
		return runPhase(ctx, service, "approval", 0, func() (codechange.PhaseResult, error) {
			return service.Approval(ctx, parent.RunID, newDurableApprovalGate(ctx.approvalEventWaiter()))
		})
	}
}

func publishHandler(service phaseService) func(taskContext, map[string]any) (codechange.PhaseResult, error) {
	return func(ctx taskContext, _ map[string]any) (codechange.PhaseResult, error) {
		parent, err := parentResult(ctx, "approval")
		if err != nil {
			return codechange.PhaseResult{}, err
		}
		return runPhase(ctx, service, "publish", publishRetries, func() (codechange.PhaseResult, error) {
			return service.Publish(ctx, parent.RunID)
		})
	}
}

func finalizeHandler(service phaseService) func(taskContext, map[string]any) (templatecodechange.Result, error) {
	return func(ctx taskContext, _ map[string]any) (templatecodechange.Result, error) {
		parent, err := parentResult(ctx, "publish")
		if err != nil {
			return templatecodechange.Result{}, err
		}
		result, err := service.Finalize(ctx, parent.RunID)
		if err == nil {
			return result, nil
		}
		if result.Failure == nil || !result.Failure.Retryable {
			if terminal(result.Status) {
				return result, nil
			}
			return templatecodechange.Result{}, err
		}
		if ctx.RetryCount() < finalizeRetries {
			return templatecodechange.Result{}, err
		}
		if _, exhaustErr := service.Exhaust(ctx, parent.RunID, "finalize"); exhaustErr != nil {
			return templatecodechange.Result{}, exhaustErr
		}
		return service.Finalize(ctx, parent.RunID)
	}
}

func parentResult(ctx taskContext, parent string) (codechange.PhaseResult, error) {
	var result codechange.PhaseResult
	if err := ctx.ParentOutput(namedTask(parent), &result); err != nil {
		return codechange.PhaseResult{}, fmt.Errorf("read %s result: %w", parent, err)
	}
	if result.RunID == "" {
		return codechange.PhaseResult{}, fmt.Errorf("read %s result: run ID is required", parent)
	}
	return result, nil
}

func runPhase(
	ctx taskContext,
	service phaseService,
	stage string,
	maxRetries int,
	run func() (codechange.PhaseResult, error),
) (codechange.PhaseResult, error) {
	result, phaseError := run()
	if phaseError == nil {
		return result, nil
	}
	if !result.Retryable {
		if terminal(result.Status) {
			return result, nil
		}
		return codechange.PhaseResult{}, phaseError
	}
	if result.RunID == "" || ctx.RetryCount() < maxRetries {
		return codechange.PhaseResult{}, phaseError
	}
	return service.Exhaust(ctx, result.RunID, stage)
}

func terminal(status run.Status) bool {
	switch status {
	case run.StatusSucceeded, run.StatusFailed, run.StatusCanceled, run.StatusDeclined:
		return true
	default:
		return false
	}
}
