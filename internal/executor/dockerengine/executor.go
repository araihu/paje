// Package dockerengine provides isolated one-shot execution through an
// explicitly configured local Docker Engine Unix socket.
package dockerengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"sync"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	destroyedHistoryLimit   = 1024
	ambiguousCleanupWindow  = 250 * time.Millisecond
	cleanupPollInterval     = 10 * time.Millisecond
	bootstrapTmpfsSize      = 64 << 20
	minimumWorkingTmpfsSize = 64 << 20
	maximumWorkingTmpfsSize = 1 << 30
)

type attemptRecord struct {
	state            executor.State
	receipt          *executor.ChildStartReceipt
	cancelRequested  bool
	destroyRequested bool
	destroyRecorded  bool
	cleanupRequired  bool
}

type Executor struct {
	mu sync.Mutex

	engine       engineClient
	registryAuth string
	stopTimeout  time.Duration
	killTimeout  time.Duration

	attempts       map[string]*attemptRecord
	destroyedOrder []string
}

func (*Executor) ValidateProfile(profile workerprofile.Snapshot) error {
	canonical, err := workerprofile.Canonicalize(profile)
	if err != nil || profile.Digest == "" || !reflect.DeepEqual(canonical, profile) {
		return errors.New("Docker executor profile is not exact and canonical")
	}
	if profile.Runtime.Kind != workerprofile.RuntimeOCI ||
		profile.Runtime.Network != workerprofile.NetworkNone && profile.Runtime.Network != workerprofile.NetworkOutbound ||
		!profile.Runtime.ReadOnlyRoot {
		return errors.New("Docker executor accepts only enforceable OCI profiles")
	}
	return nil
}

func (target *Executor) Execute(ctx context.Context, request executor.Request) (result executor.Result, returnedErr error) {
	if target == nil || target.engine == nil {
		return executor.Result{}, errors.New("Docker executor is nil")
	}
	if err := ctx.Err(); err != nil {
		return executor.Result{}, err
	}
	if err := request.Validate(); err != nil {
		return executor.Result{}, err
	}
	if err := target.ValidateProfile(request.Profile); err != nil {
		return executor.Result{}, err
	}
	document, err := newBootstrapDocument(request)
	if err != nil {
		return executor.Result{}, err
	}
	expectedReceipt := document.ChildStartReceipt.Clone()

	key := request.Attempt.Key()
	record := &attemptRecord{state: executor.StateAbsent}
	target.mu.Lock()
	if _, exists := target.attempts[key]; exists {
		target.mu.Unlock()
		return executor.Result{}, executor.ErrAttemptExists
	}
	target.attempts[key] = record
	target.mu.Unlock()
	defer target.releaseReservation(key, record)

	if err := target.verifyImage(ctx, request.Profile); err != nil {
		return executor.Result{}, err
	}
	if target.lifecycleRequested(key, record) {
		return executor.Result{}, lifecycleRequestedError()
	}
	networkLabels := attemptLabels(request.Attempt, resourceNetwork)
	networks, err := target.exactNetworks(ctx, networkLabels)
	if err != nil {
		return executor.Result{}, err
	}
	containers, err := target.exactContainers(ctx, attemptLabels(request.Attempt, resourceContainer))
	if err != nil {
		return executor.Result{}, err
	}
	if len(networks) != 0 || len(containers) != 0 {
		target.setState(key, record, executor.StateUnknown)
		return executor.Result{}, executor.ErrAttemptExists
	}
	if target.lifecycleRequested(key, record) {
		return executor.Result{}, lifecycleRequestedError()
	}

	networkName := ""
	networkID := ""
	if request.Profile.Runtime.Network == workerprofile.NetworkOutbound {
		networkName = resourceName(request.Attempt, resourceNetwork)
		if target.lifecycleRequested(key, record) {
			return executor.Result{}, lifecycleRequestedError()
		}
		networkID, err = target.createNetwork(ctx, networkName, networkLabels)
		if err != nil {
			if isAmbiguousAttempt(err) {
				target.markAmbiguous(key, record)
				if target.lifecycleRequested(key, record) {
					return executor.Result{}, target.cleanupAmbiguousCreateFailure(request.Attempt, err)
				}
			}
			return executor.Result{}, err
		}
		if target.lifecycleRequested(key, record) {
			return executor.Result{}, target.cleanupCreateFailure(request.Attempt, "", networkID,
				lifecycleRequestedError())
		}
	}

	containerLabels := boundContainerLabels(request.Attempt, expectedReceipt)
	options, err := containerOptions(request, networkName, containerLabels)
	if err != nil {
		return executor.Result{}, target.cleanupCreateFailure(request.Attempt, "", networkID,
			wrapProvider("input", "container_config", err))
	}
	if target.lifecycleRequested(key, record) {
		return executor.Result{}, target.cleanupCreateFailure(request.Attempt, "", networkID,
			lifecycleRequestedError())
	}
	containerID, err := target.createContainer(
		ctx,
		options,
		attemptLabels(request.Attempt, resourceContainer),
		expectedReceipt,
	)
	if err != nil {
		if isAmbiguousAttempt(err) {
			target.markAmbiguous(key, record)
			if target.lifecycleRequested(key, record) {
				return executor.Result{}, target.cleanupAmbiguousCreateFailure(request.Attempt, err)
			}
			return executor.Result{}, err
		}
		return executor.Result{}, target.cleanupCreateFailure(request.Attempt, "", networkID,
			err)
	}
	result = executor.Result{Created: true, SafeFacts: safeFacts(request.Profile)}
	target.setState(key, record, executor.StateCreated)
	if target.lifecycleRequested(key, record) {
		return result, target.cleanupCreateFailure(request.Attempt, containerID, networkID,
			lifecycleRequestedError())
	}

	archive, err := buildArchiveForDocument(request, document)
	if err != nil {
		return result, target.cleanupCreateFailure(request.Attempt, containerID, networkID,
			wrapProvider("environment", "materialize", err))
	}
	attached, err := target.engine.AttachContainer(ctx, containerID)
	if err != nil {
		archive.Destroy()
		return result, target.cleanupCreateFailure(request.Attempt, containerID, networkID,
			wrapProvider("environment", "attach", err))
	}
	capture, err := startLogCapture(attached, request.OutputLimit)
	if err != nil {
		_ = attached.Close()
		archive.Destroy()
		return result, target.cleanupCreateFailure(request.Attempt, containerID, networkID,
			wrapProvider("internal", "log_buffer", err))
	}

	executionCtx, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	startedAt := time.Now()
	if target.lifecycleRequested(key, record) {
		archive.Destroy()
		_ = attached.Close()
		_, _, _, _, _ = capture.finish(target.killTimeout)
		return result, lifecycleRequestedError()
	}
	startErr := target.engine.StartContainer(executionCtx, containerID)
	if startErr != nil {
		result.Duration = time.Since(startedAt)
		archive.Destroy()
		_ = attached.Close()
		stdout, stderr, stdoutTruncated, stderrTruncated, _ := capture.finish(target.killTimeout)
		setOutput(&result, stdout, stderr, stdoutTruncated, stderrTruncated, request.Secrets)
		bootstrapStarted, reconciled := target.reconcileStartFailure(executionCtx, request.Attempt, containerID, startErr)
		result.BootstrapStarted = bootstrapStarted
		if isAmbiguousAttempt(reconciled) {
			target.setState(key, record, executor.StateUnknown)
		} else if bootstrapStarted {
			target.setState(key, record, executor.StateBootstrapStarted)
		}
		return result, reconciled
	}
	result.BootstrapStarted = true
	target.setState(key, record, executor.StateBootstrapStarted)
	if target.lifecycleRequested(key, record) {
		archive.Destroy()
		_ = attached.Close()
		_, _, _, _, _ = capture.finish(target.killTimeout)
		return result, target.cleanupCreateFailure(request.Attempt, containerID, networkID,
			lifecycleRequestedError())
	}
	deliveryErr := deliverBootstrap(executionCtx, attached, archive)
	archive.Destroy()
	receipt, receiptErr := target.waitForChildStart(executionCtx, containerID, expectedReceipt)
	if receiptErr != nil && (executionCtx.Err() != nil || target.lifecycleRequested(key, record)) {
		recoveryCtx, recoveryCancel := target.lifecycleContext()
		recoveredReceipt, recoveryErr := target.waitForChildStart(recoveryCtx, containerID, expectedReceipt)
		recoveryCancel()
		if recoveryErr == nil {
			receipt = recoveredReceipt
			receiptErr = nil
		} else {
			receiptErr = errors.Join(receiptErr, recoveryErr)
		}
	}
	if receiptErr != nil {
		result.Duration = time.Since(startedAt)
		_ = attached.Close()
		stdout, stderr, stdoutTruncated, stderrTruncated, logErr := capture.finish(target.killTimeout)
		setOutput(&result, stdout, stderr, stdoutTruncated, stderrTruncated, request.Secrets)
		if target.lifecycleRequested(key, record) {
			return result, target.cleanupCreateFailure(request.Attempt, containerID, networkID,
				lifecycleRequestedError())
		}
		target.setState(key, record, executor.StateUnknown)
		return result, ambiguousAttempt(errors.Join(deliveryErr, receiptErr, logErr))
	}
	result.Started = true
	result.ChildStartReceipt = &receipt
	target.rememberChildStart(key, record, receipt)
	acknowledgementCtx := executionCtx
	acknowledgementCancel := func() {}
	if executionCtx.Err() != nil || target.lifecycleRequested(key, record) {
		acknowledgementCtx, acknowledgementCancel = target.lifecycleContext()
	}
	acknowledgementErr := target.engine.SignalContainer(acknowledgementCtx, containerID, "SIGUSR1")
	acknowledgementCancel()
	if acknowledgementErr != nil {
		result.Duration = time.Since(startedAt)
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.Background(), target.stopTimeout+target.killTimeout+time.Second,
		)
		cancelErr := target.Cancel(cleanupCtx, request.Attempt)
		cleanupCancel()
		_ = attached.Close()
		stdout, stderr, stdoutTruncated, stderrTruncated, logErr := capture.finish(target.killTimeout)
		setOutput(&result, stdout, stderr, stdoutTruncated, stderrTruncated, request.Secrets)
		target.setState(key, record, executor.StateUnknown)
		return result, ambiguousAttempt(errors.Join(
			acknowledgementErr,
			cancelErr,
			logErr,
			errors.New("Docker child-start receipt acknowledgement is uncertain"),
		))
	}
	if target.lifecycleRequested(key, record) {
		_ = attached.Close()
		_, _, _, _, _ = capture.finish(target.killTimeout)
		return result, lifecycleRequestedError()
	}

	exitCode, waitErr := target.engine.WaitContainer(executionCtx, containerID)
	if waitErr == nil && executionCtx.Err() != nil {
		waitErr = executionCtx.Err()
	}
	result.Duration = time.Since(startedAt)
	var compensationErr error
	if waitErr != nil && executionCtx.Err() != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(),
			target.stopTimeout+target.killTimeout+time.Second)
		compensationErr = target.Cancel(cleanupCtx, request.Attempt)
		cleanupCancel()
	}
	stdout, stderr, stdoutTruncated, stderrTruncated, logErr := capture.finish(target.killTimeout)
	setOutput(&result, stdout, stderr, stdoutTruncated, stderrTruncated, request.Secrets)

	if waitErr != nil {
		if compensationErr != nil {
			return result, wrapProvider("cleanup", "cancel_compensation",
				errors.Join(waitErr, compensationErr, logErr))
		}
		if ctx.Err() != nil {
			return result, wrapProvider("canceled", "caller_canceled", errors.Join(waitErr, ctx.Err(), logErr))
		}
		if errors.Is(executionCtx.Err(), context.DeadlineExceeded) {
			return result, wrapProvider("environment", "timeout", errors.Join(waitErr, logErr))
		}
		return result, wrapProvider("internal", "wait", errors.Join(waitErr, logErr))
	}
	if exitCode < 0 || int64(int(exitCode)) != exitCode {
		return result, wrapProvider("internal", "wait_state", errors.New("Docker Engine returned invalid exit status"))
	}
	result.Completed = true
	result.ExitCode = int(exitCode)
	target.setState(key, record, executor.StateCompleted)
	if logErr != nil {
		return result, wrapProvider("internal", "log_stream", logErr)
	}
	return result, nil
}

func (target *Executor) Inspect(ctx context.Context, attempt executor.AttemptID) (executor.State, error) {
	if target == nil || target.engine == nil {
		return executor.StateUnknown, errors.New("Docker executor is nil")
	}
	if err := ctx.Err(); err != nil {
		return executor.StateUnknown, err
	}
	if err := attempt.Validate(); err != nil {
		return executor.StateUnknown, err
	}
	containers, err := target.exactContainers(ctx, attemptLabels(attempt, resourceContainer))
	if err != nil {
		return executor.StateUnknown, err
	}
	key := attempt.Key()
	if len(containers) == 0 {
		target.mu.Lock()
		record := target.attempts[key]
		if record != nil && record.state == executor.StateDestroyed {
			target.mu.Unlock()
			return executor.StateDestroyed, nil
		}
		if record == nil {
			record = &attemptRecord{}
			target.attempts[key] = record
		}
		record.state = executor.StateUnknown
		record.cleanupRequired = true
		target.mu.Unlock()
		return executor.StateUnknown, ambiguousAttempt(
			errors.New("Docker attempt identity is missing without conclusive non-start proof"),
		)
	}
	container := containers[0]
	state, err := target.engine.InspectContainer(ctx, container.ID)
	if providerNotFound(err) {
		target.rememberAmbiguousInspection(key)
		return executor.StateUnknown, ambiguousAttempt(
			errors.New("Docker container disappeared after exact attempt discovery"),
		)
	}
	if err != nil {
		return executor.StateUnknown, wrapProvider("internal", "inspect", err)
	}
	if state == engineContainerCreated {
		target.rememberInspectedState(key, executor.StateResourceCreated)
		return executor.StateResourceCreated, nil
	}
	receipt, receiptErr := target.readChildStartReceipt(ctx, container.ID)
	if receiptErr != nil {
		target.mu.Lock()
		known := target.attempts[key]
		knownState := executor.StateAbsent
		var observed *executor.ChildStartReceipt
		if known != nil {
			knownState = known.state
			if known.receipt != nil {
				cloned := known.receipt.Clone()
				observed = &cloned
			}
		}
		target.mu.Unlock()
		if state == engineContainerExited && knownState == executor.StateTerminalComplete && observed != nil {
			if observed.Attempt.Key() != attempt.Key() || !observed.Matches(*observed) {
				target.rememberInspectedState(key, executor.StateUnknown)
				return executor.StateUnknown, ambiguousAttempt(errors.New("cached Docker child-start receipt is invalid"))
			}
			if err := validateReceiptLabels(container, *observed); err != nil {
				target.rememberInspectedState(key, executor.StateUnknown)
				return executor.StateUnknown, ambiguousAttempt(err)
			}
			return executor.StateTerminalComplete, nil
		}
		if providerNotFound(receiptErr) && knownState == executor.StateBootstrapStarted &&
			(state == engineContainerRunning || state == engineContainerPaused || state == engineContainerRestarting) {
			return executor.StateBootstrapStarted, nil
		}
		target.rememberInspectedState(key, executor.StateUnknown)
		return executor.StateUnknown, ambiguousAttempt(errors.Join(
			receiptErr,
			errors.New("Docker child-start receipt cannot be validated"),
		))
	}
	if receipt.Attempt.Key() != attempt.Key() {
		target.rememberInspectedState(key, executor.StateUnknown)
		return executor.StateUnknown, ambiguousAttempt(errors.New("Docker child-start receipt attempt was rebound"))
	}
	if err := validateReceiptLabels(container, receipt); err != nil {
		target.rememberInspectedState(key, executor.StateUnknown)
		return executor.StateUnknown, ambiguousAttempt(err)
	}
	var mapped executor.State
	switch state {
	case engineContainerRunning, engineContainerPaused, engineContainerRestarting:
		mapped = executor.StateChildStarted
	case engineContainerExited:
		target.mu.Lock()
		known := target.attempts[key]
		knownTerminal := known != nil && known.state == executor.StateTerminalComplete
		target.mu.Unlock()
		if !knownTerminal {
			target.rememberInspectedState(key, executor.StateUnknown)
			return executor.StateUnknown, ambiguousAttempt(
				errors.New("Docker terminal state is not bound to an observed declared-child exit"),
			)
		}
		mapped = executor.StateTerminalComplete
	case engineContainerDead, engineContainerRemoving:
		target.rememberInspectedState(key, executor.StateUnknown)
		return executor.StateUnknown, ambiguousAttempt(errors.New("Docker container terminal state is uncertain"))
	default:
		return executor.StateUnknown, wrapProvider("internal", "malformed_state", errors.New("unsupported Docker container state"))
	}
	target.rememberInspectedState(key, mapped)
	return mapped, nil
}

func (target *Executor) rememberInspectedState(key string, state executor.State) {
	target.mu.Lock()
	if record := target.attempts[key]; record != nil && record.state != executor.StateDestroyed {
		record.state = state
	}
	target.mu.Unlock()
}

func (target *Executor) rememberAmbiguousInspection(key string) {
	target.mu.Lock()
	record := target.attempts[key]
	if record == nil {
		record = &attemptRecord{}
		target.attempts[key] = record
	}
	if record.state != executor.StateDestroyed {
		record.state = executor.StateUnknown
		record.cleanupRequired = true
	}
	target.mu.Unlock()
}

func (target *Executor) rememberChildStart(
	key string,
	expected *attemptRecord,
	receipt executor.ChildStartReceipt,
) {
	target.mu.Lock()
	if target.attempts[key] == expected && expected.state != executor.StateDestroyed {
		cloned := receipt.Clone()
		expected.receipt = &cloned
		expected.state = executor.StateChildStarted
	}
	target.mu.Unlock()
}

func (target *Executor) Cancel(ctx context.Context, attempt executor.AttemptID) error {
	if target == nil || target.engine == nil {
		return errors.New("Docker executor is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := attempt.Validate(); err != nil {
		return err
	}
	key := attempt.Key()
	target.mu.Lock()
	record := target.attempts[key]
	if record == nil {
		record = &attemptRecord{state: executor.StateAbsent}
		target.attempts[key] = record
	}
	alreadyRequested := record.cancelRequested || record.destroyRequested
	knownState := record.state
	record.cancelRequested = true
	target.mu.Unlock()

	containers, err := target.exactContainers(ctx, attemptLabels(attempt, resourceContainer))
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		// Cancel's desired terminal condition is resource absence. Another
		// concurrent cancellation/compensation may have removed the exact
		// attempt between calls, so absence is an idempotent success here even
		// though Inspect would preserve ambiguity for recovery decisions.
		if !alreadyRequested && (knownState == executor.StateChildStarted || knownState == executor.StateUnknown) {
			return wrapProvider("internal", "ambiguous_attempt",
				errors.New("known started Docker attempt resource disappeared"))
		}
		return nil
	}
	id := containers[0].ID
	state, err := target.engine.InspectContainer(ctx, id)
	if providerNotFound(err) {
		return nil
	}
	if err != nil {
		return wrapProvider("cleanup", "cancel_inspect", err)
	}
	if containerTerminated(state) {
		return nil
	}
	stopErr := target.engine.StopContainer(ctx, id, target.stopTimeout)
	if providerNotFound(stopErr) {
		return nil
	}
	state, inspectErr := target.engine.InspectContainer(ctx, id)
	if inspectErr == nil && containerTerminated(state) {
		return nil
	}
	killErr := target.engine.KillContainer(ctx, id)
	if providerNotFound(killErr) {
		return nil
	}
	deadline := time.Now().Add(target.killTimeout)
	for {
		state, inspectErr = target.engine.InspectContainer(ctx, id)
		if providerNotFound(inspectErr) || inspectErr == nil && containerTerminated(state) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return wrapProvider("cleanup", "cancel_timeout", errors.Join(
				errors.New("Docker container termination was not confirmed"),
				stopErr, inspectErr, killErr,
			))
		}
		timer := time.NewTimer(min(10*time.Millisecond, time.Until(deadline)))
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		}
	}
}

func (target *Executor) Destroy(ctx context.Context, attempt executor.AttemptID) error {
	if target == nil || target.engine == nil {
		return errors.New("Docker executor is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := attempt.Validate(); err != nil {
		return err
	}
	key := attempt.Key()
	target.mu.Lock()
	record := target.attempts[key]
	if record == nil {
		record = &attemptRecord{state: executor.StateAbsent}
		target.attempts[key] = record
	}
	record.destroyRequested = true
	cleanupRequired := record.cleanupRequired
	target.mu.Unlock()
	if err := target.Cancel(ctx, attempt); err != nil {
		return err
	}

	window := time.Duration(0)
	if cleanupRequired {
		window = ambiguousCleanupWindow
	}
	if err := target.reconcileExactResources(ctx, attempt, window); err != nil {
		return err
	}
	target.mu.Lock()
	if record := target.attempts[key]; record != nil {
		record.cleanupRequired = false
		target.markDestroyedLocked(key, record)
	}
	target.mu.Unlock()
	return nil
}

func (target *Executor) reconcileExactResources(
	ctx context.Context,
	attempt executor.AttemptID,
	window time.Duration,
) error {
	deadline := time.Now().Add(window)
	for {
		if err := target.destroyExactResources(ctx, attempt); err != nil {
			return err
		}
		if window <= 0 || !time.Now().Before(deadline) {
			return nil
		}
		timer := time.NewTimer(min(cleanupPollInterval, time.Until(deadline)))
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		}
	}
}

func (target *Executor) destroyExactResources(ctx context.Context, attempt executor.AttemptID) error {
	containers, err := target.exactContainers(ctx, attemptLabels(attempt, resourceContainer))
	if err != nil {
		return err
	}
	networks, err := target.exactNetworks(ctx, attemptLabels(attempt, resourceNetwork))
	if err != nil {
		return err
	}
	var cleanupErr error
	if len(containers) == 1 {
		if err := target.engine.RemoveContainer(ctx, containers[0].ID); err != nil && !providerNotFound(err) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if len(networks) == 1 {
		if err := target.engine.RemoveNetwork(ctx, networks[0].ID); err != nil && !providerNotFound(err) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if cleanupErr != nil {
		return wrapProvider("cleanup", "destroy", cleanupErr)
	}
	return nil
}

func (target *Executor) exactContainers(ctx context.Context, labels map[string]string) ([]engineContainer, error) {
	containers, err := target.engine.ListContainers(ctx, labels)
	if err != nil {
		return nil, wrapProvider("internal", "container_list", err)
	}
	if len(containers) > 1 {
		return nil, resourceConflict("container")
	}
	return containers, nil
}

func (target *Executor) exactNetworks(ctx context.Context, labels map[string]string) ([]engineNetwork, error) {
	networks, err := target.engine.ListNetworks(ctx, labels)
	if err != nil {
		return nil, wrapProvider("internal", "network_list", err)
	}
	if len(networks) > 1 {
		return nil, resourceConflict("network")
	}
	return networks, nil
}

func (target *Executor) createNetwork(ctx context.Context, name string, labels map[string]string) (string, error) {
	id, createErr := target.engine.CreateNetwork(ctx, name, labels)
	if createErr == nil && id != "" {
		return id, nil
	}
	networks, discoveryErr := target.exactNetworks(ctx, labels)
	if discoveryErr != nil {
		if providerCause(discoveryErr) == "resource_conflict" {
			return "", discoveryErr
		}
		if providerConflict(createErr) {
			return "", resourceConflict("network")
		}
		if createErr != nil && !responseMayBeLost(createErr) {
			return "", wrapProvider("environment", "network_create", errors.Join(createErr, discoveryErr))
		}
		return "", ambiguousAttempt(errors.Join(createErr, discoveryErr))
	}
	if len(networks) == 1 {
		return networks[0].ID, nil
	}
	if providerConflict(createErr) {
		return "", resourceConflict("network")
	}
	if createErr == nil || responseMayBeLost(createErr) {
		return "", ambiguousAttempt(errors.Join(createErr, errors.New("Docker network creation outcome is unknown")))
	}
	return "", wrapProvider("environment", "network_create", createErr)
}

func (target *Executor) createContainer(
	ctx context.Context,
	options client.ContainerCreateOptions,
	labels map[string]string,
	expectedReceipt executor.ChildStartReceipt,
) (string, error) {
	id, createErr := target.engine.CreateContainer(ctx, options)
	if createErr == nil && id != "" {
		return id, nil
	}
	containers, discoveryErr := target.exactContainers(ctx, labels)
	if discoveryErr != nil {
		if providerCause(discoveryErr) == "resource_conflict" {
			return "", discoveryErr
		}
		if providerConflict(createErr) {
			return "", resourceConflict("container")
		}
		if createErr != nil && !responseMayBeLost(createErr) {
			return "", wrapProvider("environment", "create", errors.Join(createErr, discoveryErr))
		}
		return "", ambiguousAttempt(errors.Join(createErr, discoveryErr))
	}
	if len(containers) == 1 {
		if err := validateReceiptLabels(containers[0], expectedReceipt); err != nil {
			return "", ambiguousAttempt(errors.Join(createErr, err))
		}
		state, inspectErr := target.engine.InspectContainer(ctx, containers[0].ID)
		if inspectErr != nil || state != engineContainerCreated {
			return "", ambiguousAttempt(errors.Join(createErr, inspectErr,
				errors.New("reconciled Docker container is not proven unstarted")))
		}
		return containers[0].ID, nil
	}
	if providerConflict(createErr) {
		return "", resourceConflict("container")
	}
	if createErr == nil || responseMayBeLost(createErr) {
		return "", ambiguousAttempt(errors.Join(createErr, errors.New("Docker container creation outcome is unknown")))
	}
	return "", wrapProvider("environment", "create", createErr)
}

func (target *Executor) reconcileStartFailure(
	ctx context.Context,
	attempt executor.AttemptID,
	containerID string,
	startErr error,
) (bool, error) {
	containers, discoveryErr := target.exactContainers(ctx, attemptLabels(attempt, resourceContainer))
	if discoveryErr != nil || len(containers) != 1 || containers[0].ID != containerID {
		return false, ambiguousAttempt(errors.Join(startErr, discoveryErr,
			errors.New("Docker container start outcome cannot be reconciled")))
	}
	state, inspectErr := target.engine.InspectContainer(ctx, containerID)
	if inspectErr == nil && state == engineContainerCreated {
		return false, wrapProvider("environment", "start", startErr)
	}
	started := inspectErr == nil && state != engineContainerCreated
	return started, ambiguousAttempt(errors.Join(startErr, inspectErr,
		errors.New("Docker container may have started")))
}

func deliverBootstrap(ctx context.Context, attached attachedContainerIO, archive *privateArchive) error {
	if archive == nil || attached == nil {
		return errors.New("Docker bootstrap delivery is invalid")
	}
	writeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = attached.SetWriteDeadline(time.Now())
		case <-writeDone:
		}
	}()
	written, copyErr := io.Copy(attached, archive.Reader())
	close(writeDone)
	_ = attached.SetWriteDeadline(time.Time{})
	if copyErr != nil {
		return copyErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if written != int64(len(archive.bytes)) {
		return io.ErrShortWrite
	}
	return attached.CloseWrite()
}

func responseMayBeLost(err error) bool {
	if err == nil {
		return false
	}
	if client.IsErrConnectionFailed(err) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func ambiguousAttempt(err error) error {
	return wrapProvider("internal", "ambiguous_attempt", err)
}

func isAmbiguousAttempt(err error) bool {
	return providerCause(err) == "ambiguous_attempt"
}

func providerCause(err error) string {
	var providerError *executor.ProviderError
	if errors.As(err, &providerError) {
		return providerError.CauseCode
	}
	return ""
}

func lifecycleRequestedError() error {
	return wrapProvider("canceled", "lifecycle_requested",
		errors.New("attempt lifecycle operation suppressed Docker execution"))
}

func (target *Executor) lifecycleRequested(key string, expected *attemptRecord) bool {
	target.mu.Lock()
	defer target.mu.Unlock()
	record := target.attempts[key]
	return record != expected || record.cancelRequested || record.destroyRequested ||
		record.state == executor.StateDestroyed
}

func (target *Executor) lifecycleContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(
		context.Background(),
		target.stopTimeout+target.killTimeout+time.Second,
	)
}

func (target *Executor) releaseReservation(key string, expected *attemptRecord) {
	target.mu.Lock()
	if target.attempts[key] == expected && expected.state == executor.StateAbsent &&
		!expected.cancelRequested && !expected.destroyRequested {
		delete(target.attempts, key)
	}
	target.mu.Unlock()
}

func (target *Executor) cleanupCreateFailure(attempt executor.AttemptID, containerID, networkID string, cause error) error {
	timeout := target.stopTimeout + target.killTimeout + time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var cleanupErr error
	if containerID != "" {
		if err := target.engine.RemoveContainer(ctx, containerID); err != nil && !providerNotFound(err) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if networkID != "" {
		if err := target.engine.RemoveNetwork(ctx, networkID); err != nil && !providerNotFound(err) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if cleanupErr != nil {
		return wrapProvider("cleanup", "create_compensation", errors.Join(cause, cleanupErr))
	}
	target.mu.Lock()
	key := attempt.Key()
	if record := target.attempts[key]; record != nil {
		record.cleanupRequired = false
		switch {
		case record.destroyRequested:
			target.markDestroyedLocked(key, record)
		case record.cancelRequested:
			if record.state != executor.StateDestroyed {
				record.state = executor.StateAbsent
			}
		default:
			delete(target.attempts, key)
		}
	}
	target.mu.Unlock()
	return cause
}

func (target *Executor) cleanupAmbiguousCreateFailure(attempt executor.AttemptID, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), ambiguousCleanupWindow+time.Second)
	defer cancel()
	if err := target.reconcileExactResources(ctx, attempt, ambiguousCleanupWindow); err != nil {
		return wrapProvider("cleanup", "create_compensation", errors.Join(cause, err))
	}
	return target.cleanupCreateFailure(attempt, "", "", cause)
}

func (target *Executor) setState(key string, expected *attemptRecord, state executor.State) {
	target.mu.Lock()
	if target.attempts[key] == expected && expected.state != executor.StateDestroyed {
		expected.state = state
	}
	target.mu.Unlock()
}

func (target *Executor) markAmbiguous(key string, expected *attemptRecord) {
	target.mu.Lock()
	if target.attempts[key] == expected && expected.state != executor.StateDestroyed {
		expected.state = executor.StateUnknown
		expected.cleanupRequired = true
	}
	target.mu.Unlock()
}

func (target *Executor) trimDestroyedHistory() {
	for len(target.destroyedOrder) > destroyedHistoryLimit {
		oldest := target.destroyedOrder[0]
		target.destroyedOrder[0] = ""
		target.destroyedOrder = target.destroyedOrder[1:]
		if record := target.attempts[oldest]; record != nil && record.state == executor.StateDestroyed {
			delete(target.attempts, oldest)
		}
	}
}

func (target *Executor) markDestroyedLocked(key string, record *attemptRecord) {
	if record == nil {
		return
	}
	record.state = executor.StateDestroyed
	if record.receipt != nil {
		*record.receipt = executor.ChildStartReceipt{}
		record.receipt = nil
	}
	if record.destroyRecorded {
		return
	}
	record.destroyRecorded = true
	target.destroyedOrder = append(target.destroyedOrder, key)
	target.trimDestroyedHistory()
}

func containerOptions(request executor.Request, networkName string, labels map[string]string) (client.ContainerCreateOptions, error) {
	platform, err := parsePlatform(request.Profile.Runtime.Platform)
	if err != nil {
		return client.ContainerCreateOptions{}, err
	}
	readOnlyForce := !request.Workspace.Writable
	workingTmpfs := workingTmpfsSize(request.Profile.Resources.MemoryBytes)
	options := client.ContainerCreateOptions{
		Config: &container.Config{
			User:         "65532:65532",
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			OpenStdin:    true,
			StdinOnce:    true,
			Image:        request.Profile.Runtime.Image,
			Entrypoint:   []string{"/usr/local/bin/paje-sandbox-init"},
			Cmd:          []string{"--bootstrap-stdin"},
			WorkingDir:   "/",
			Labels:       cloneStringMap(labels),
		},
		HostConfig: &container.HostConfig{
			LogConfig:      container.LogConfig{Type: "none"},
			RestartPolicy:  container.RestartPolicy{Name: "no"},
			CapDrop:        []string{"ALL"},
			CgroupnsMode:   "private",
			IpcMode:        "private",
			ReadonlyRootfs: true,
			SecurityOpt:    []string{"no-new-privileges=true"},
			NetworkMode:    "none",
			Tmpfs: map[string]string{
				"/run/paje":  privateTmpfsOptions(bootstrapTmpfsSize),
				"/home/paje": privateTmpfsOptions(workingTmpfs),
				"/tmp":       temporaryTmpfsOptions(workingTmpfs),
			},
			Resources: container.Resources{
				Memory:     request.Profile.Resources.MemoryBytes,
				MemorySwap: request.Profile.Resources.MemoryBytes,
				NanoCPUs:   request.Profile.Resources.CPUMillis * 1_000_000,
				PidsLimit:  &request.Profile.Resources.PIDs,
			},
			Mounts: []mount.Mount{
				{
					Type: mount.TypeBind, Source: request.Workspace.HostPath,
					Target: executor.SandboxWorkspaceRoot, ReadOnly: !request.Workspace.Writable,
					BindOptions: &mount.BindOptions{
						Propagation: "rprivate", ReadOnlyForceRecursive: readOnlyForce,
					},
				},
			},
		},
		Platform: &platform,
		Name:     resourceName(request.Attempt, resourceContainer),
	}
	if request.Profile.Runtime.Network == workerprofile.NetworkNone {
		options.Config.NetworkDisabled = true
		return options, nil
	}
	if networkName == "" {
		return client.ContainerCreateOptions{}, errors.New("outbound Docker network identity is missing")
	}
	options.HostConfig.NetworkMode = container.NetworkMode(networkName)
	options.NetworkingConfig = &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{networkName: {}},
	}
	return options, nil
}

func workingTmpfsSize(memoryBytes int64) int64 {
	size := memoryBytes / 4
	if size < minimumWorkingTmpfsSize {
		return minimumWorkingTmpfsSize
	}
	if size > maximumWorkingTmpfsSize {
		return maximumWorkingTmpfsSize
	}
	return size
}

func privateTmpfsOptions(size int64) string {
	return fmt.Sprintf("rw,nosuid,nodev,noexec,size=%d,mode=0700,uid=65532,gid=65532", size)
}

func temporaryTmpfsOptions(size int64) string {
	return fmt.Sprintf("rw,nosuid,nodev,exec,size=%d,mode=0700,uid=65532,gid=65532", size)
}

func mapContainerState(state engineContainerState) (executor.State, bool) {
	switch state {
	case engineContainerCreated:
		return executor.StateCreated, true
	case engineContainerRunning, engineContainerPaused, engineContainerRestarting:
		return executor.StateRunning, true
	case engineContainerExited:
		return executor.StateCompleted, true
	case engineContainerDead, engineContainerRemoving:
		return executor.StateUnknown, true
	default:
		return executor.StateUnknown, false
	}
}

func containerTerminated(state engineContainerState) bool {
	return state == engineContainerCreated ||
		state == engineContainerExited ||
		state == engineContainerDead
}

func safeFacts(profile workerprofile.Snapshot) map[string]string {
	return map[string]string{
		"runtime_kind": workerprofile.RuntimeOCI,
		"image":        profile.Runtime.Image,
		"platform":     profile.Runtime.Platform,
		"network":      profile.Runtime.Network,
		"isolated":     "true",
	}
}

func setOutput(result *executor.Result, stdout, stderr []byte, stdoutTruncated, stderrTruncated bool, secrets []secret.Materialization) {
	result.Stdout = stdout
	result.Stderr = stderr
	result.StdoutTruncated = stdoutTruncated
	result.StderrTruncated = stderrTruncated
	if outputContainsSecret(stdout, stderr, secrets) {
		clear(result.Stdout)
		clear(result.Stderr)
		result.Stdout = nil
		result.Stderr = nil
		result.SecretDetected = true
	}
}

func outputContainsSecret(stdout, stderr []byte, materializations []secret.Materialization) bool {
	for _, materialization := range materializations {
		if value := materialization.Value(); len(value) != 0 {
			detected := bytes.Contains(stdout, value) || bytes.Contains(stderr, value)
			clear(value)
			if detected {
				return true
			}
		}
		for _, file := range materialization.Files() {
			value := file.Bytes()
			detected := bytes.Contains(stdout, value) || bytes.Contains(stderr, value)
			clear(value)
			file.Zero()
			if detected {
				return true
			}
		}
	}
	return false
}

var (
	_ executor.Executor         = (*Executor)(nil)
	_ executor.ProfileValidator = (*Executor)(nil)
)
