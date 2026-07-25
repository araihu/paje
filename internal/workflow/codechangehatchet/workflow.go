// Package codechangehatchet binds the provider-neutral code-change service to
// a durable Hatchet workflow declaration.
package codechangehatchet

import (
	"fmt"
	"time"

	"github.com/araihu/paje/internal/workflow/codechange"
	"github.com/hatchet-dev/hatchet/pkg/client/types"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

const workflowName = "paje-code-change-v1"

type taskReference interface {
	GetName() string
}

type namedTask string

func (t namedTask) GetName() string { return string(t) }

type taskOptions struct {
	parents                []string
	retries                int
	retryBackoffFactor     float32
	retryMaxBackoffSeconds int
	executionTimeout       time.Duration
	concurrency            *types.Concurrency
}

type workflowDeclaration interface {
	newTask(name string, handler any, options taskOptions) taskReference
	newDurableTask(name string, handler any, options taskOptions) taskReference
}

type workflowFactory interface {
	newWorkflow(name string) workflowDeclaration
}

// New declares the five-phase code-change workflow on client.
func New(client *hatchet.Client, service *codechange.Service) (*hatchet.Workflow, error) {
	if client == nil {
		return nil, fmt.Errorf("create code-change Hatchet workflow: client is required")
	}
	if service == nil {
		return nil, fmt.Errorf("create code-change Hatchet workflow: service is required")
	}

	factory := &hatchetWorkflowFactory{client: client}
	buildWorkflow(factory, service)
	return factory.workflow, nil
}

func buildWorkflow(factory workflowFactory, service phaseService) {
	declareWorkflow(factory.newWorkflow(workflowName), service)
}

func declareWorkflow(workflow workflowDeclaration, service phaseService) {
	workflow.newTask("resolve", wrapTaskHandler(resolveHandler(service)), taskOptions{
		retries: resolveRetries, retryBackoffFactor: 2,
		retryMaxBackoffSeconds: 60, executionTimeout: 2 * time.Minute,
	})
	workflow.newTask("execute", wrapTaskHandler(executeHandler(service)), taskOptions{
		parents: []string{"resolve"}, retries: executeRetries, retryBackoffFactor: 2,
		retryMaxBackoffSeconds: 60, executionTimeout: 30 * time.Minute,
	})
	workflow.newDurableTask("approval", wrapDurableTaskHandler(approvalHandler(service)), taskOptions{
		parents: []string{"execute"},
	})
	maxRuns := int32(1)
	strategy := types.GroupRoundRobin
	workflow.newTask("publish", wrapTaskHandler(publishHandler(service)), taskOptions{
		parents: []string{"approval"}, retries: publishRetries, retryBackoffFactor: 2,
		retryMaxBackoffSeconds: 60, executionTimeout: 15 * time.Minute,
		concurrency: &types.Concurrency{
			Expression: "input.repository_uri + ':' + input.publication.target_branch",
			MaxRuns:    &maxRuns, LimitStrategy: &strategy,
		},
	})
	workflow.newTask("finalize", wrapTaskHandler(finalizeHandler(service)), taskOptions{
		parents: []string{"publish"}, retries: finalizeRetries, retryBackoffFactor: 2,
		retryMaxBackoffSeconds: 60, executionTimeout: 2 * time.Minute,
	})
}

type hatchetDeclaration struct {
	workflow *hatchet.Workflow
	tasks    map[string]*hatchet.Task
}

type hatchetWorkflowFactory struct {
	client   *hatchet.Client
	workflow *hatchet.Workflow
}

func (f *hatchetWorkflowFactory) newWorkflow(name string) workflowDeclaration {
	f.workflow = f.client.NewWorkflow(name)
	return &hatchetDeclaration{workflow: f.workflow, tasks: make(map[string]*hatchet.Task)}
}

func (d *hatchetDeclaration) newTask(name string, handler any, options taskOptions) taskReference {
	task := d.workflow.NewTask(name, handler, d.options(options)...)
	d.tasks[name] = task
	return task
}

func (d *hatchetDeclaration) newDurableTask(name string, handler any, options taskOptions) taskReference {
	task := d.workflow.NewDurableTask(name, handler, d.options(options)...)
	d.tasks[name] = task
	return task
}

func (d *hatchetDeclaration) options(options taskOptions) []hatchet.TaskOption {
	result := make([]hatchet.TaskOption, 0, 5)
	if len(options.parents) > 0 {
		parents := make([]*hatchet.Task, len(options.parents))
		for index, name := range options.parents {
			parents[index] = d.tasks[name]
		}
		result = append(result, hatchet.WithParents(parents...))
	}
	if options.retries > 0 {
		result = append(result, hatchet.WithRetries(options.retries))
	}
	if options.retryBackoffFactor > 0 {
		result = append(result, hatchet.WithRetryBackoff(
			options.retryBackoffFactor,
			options.retryMaxBackoffSeconds,
		))
	}
	if options.executionTimeout > 0 {
		result = append(result, hatchet.WithExecutionTimeout(options.executionTimeout))
	}
	if options.concurrency != nil {
		result = append(result, hatchet.WithConcurrency(options.concurrency))
	}
	return result
}

func wrapTaskHandler[I, O any](handler func(taskContext, I) (O, error)) func(hatchet.Context, I) (O, error) {
	return func(ctx hatchet.Context, input I) (O, error) {
		return handler(ctx, input)
	}
}

type durableTaskContext interface {
	taskContext
	approvalEventWaiter() approvalEventWaiter
}

type hatchetDurableTaskContext struct{ hatchet.DurableContext }

func (c hatchetDurableTaskContext) approvalEventWaiter() approvalEventWaiter {
	return hatchetEventWaiter{ctx: c.DurableContext}
}

type hatchetEventWaiter struct{ ctx hatchet.DurableContext }

func (w hatchetEventWaiter) WaitForEvent(key, expression string) (hatchet.EventUnmarshaller, error) {
	return w.ctx.WaitForEvent(key, expression)
}

func wrapDurableTaskHandler[I, O any](handler func(durableTaskContext, I) (O, error)) func(hatchet.DurableContext, I) (O, error) {
	return func(ctx hatchet.DurableContext, input I) (O, error) {
		return handler(hatchetDurableTaskContext{DurableContext: ctx}, input)
	}
}
