package dockerengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/containerd/errdefs"
)

func TestExecuteDistinguishesBootstrapFromBoundChildStart(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, "none", nil)

	result, err := target.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || !result.BootstrapStarted || !result.Started || !result.Completed ||
		result.ChildStartReceipt == nil || result.ChildStartReceipt.Attempt != request.Attempt {
		t.Fatalf("lifecycle result = %#v", result)
	}
	if err := result.ChildStartReceipt.Validate(); err != nil {
		t.Fatalf("child-start receipt = %#v: %v", result.ChildStartReceipt, err)
	}
	api.mu.Lock()
	acknowledgements := api.childStartAcknowledgements
	api.mu.Unlock()
	if acknowledgements != 1 {
		t.Fatalf("child-start acknowledgements = %d, want 1", acknowledgements)
	}
	state, err := target.Inspect(context.Background(), request.Attempt)
	if err != nil || state != executor.StateTerminalComplete {
		t.Fatalf("Inspect() = %q, %v", state, err)
	}
}

func TestInspectUsesCanonicalFullAttemptIdentityAcrossTimeDecoding(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, "none", nil)
	request.Attempt.StartedAt = time.Date(2026, 7, 26, 12, 30, 0, 17, time.FixedZone("test-offset", -3*60*60))
	defer request.Destroy()

	result, err := target.Execute(context.Background(), request)
	if err != nil || !result.Completed {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	state, err := target.Inspect(context.Background(), request.Attempt)
	if err != nil || state != executor.StateTerminalComplete {
		t.Fatalf("Inspect() after JSON time decoding = %q, %v", state, err)
	}
}

func TestInspectUsesObservedReceiptAfterTerminalTmpfsDisappears(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, "none", nil)
	defer request.Destroy()

	result, err := target.Execute(context.Background(), request)
	if err != nil || !result.Completed || result.ChildStartReceipt == nil {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	api.mu.Lock()
	api.receipt = nil
	api.mu.Unlock()
	state, err := target.Inspect(context.Background(), request.Attempt)
	if err != nil || state != executor.StateTerminalComplete {
		t.Fatalf("Inspect() after terminal tmpfs loss = %q, %v", state, err)
	}

	restarted := newExecutorForTest(t, api)
	state, err = restarted.Inspect(context.Background(), request.Attempt)
	if state != executor.StateUnknown || providerCause(err) != "ambiguous_attempt" {
		t.Fatalf("restarted Inspect() without observed receipt = %q, %v", state, err)
	}
}

func TestExecuteWithoutChildReceiptIsAmbiguousAndNeverReportsStarted(t *testing.T) {
	api := newFakeEngine()
	api.receiptMissing = true
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, "none", nil)

	result, err := target.Execute(context.Background(), request)
	if result.Started || result.Completed || result.ChildStartReceipt != nil ||
		!result.BootstrapStarted || providerCause(err) != "ambiguous_attempt" {
		t.Fatalf("missing-receipt lifecycle = %#v, %v", result, err)
	}
}

func TestReceiptAcknowledgementResponseLossPreservesStartedAndForbidsRerun(t *testing.T) {
	api := newFakeEngine()
	api.blockWait = true
	api.childStartAckErr = context.DeadlineExceeded
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, "none", nil)
	defer request.Destroy()

	result, err := target.Execute(context.Background(), request)
	if !result.Created || !result.BootstrapStarted || !result.Started || result.Completed ||
		result.ChildStartReceipt == nil || providerCause(err) != "ambiguous_attempt" {
		t.Fatalf("acknowledgement response loss = %#v, %v", result, err)
	}
	state, inspectErr := target.Inspect(context.Background(), request.Attempt)
	if state != executor.StateUnknown || providerCause(inspectErr) != "ambiguous_attempt" {
		t.Fatalf("Inspect() after acknowledgement loss = %q, %v", state, inspectErr)
	}
	if _, retryErr := target.Execute(context.Background(), request); !errors.Is(retryErr, executor.ErrAttemptExists) {
		t.Fatalf("acknowledgement response loss was rerunnable: %v", retryErr)
	}
}

func TestCancellationAfterPublishedReceiptPreservesBoundChildStart(t *testing.T) {
	api := newFakeEngine()
	api.blockWait = true
	api.releaseReceiptPublication = make(chan struct{})
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, "none", nil)
	defer request.Destroy()

	type outcome struct {
		result executor.Result
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := target.Execute(context.Background(), request)
		finished <- outcome{result: result, err: err}
	}()
	select {
	case <-api.started:
	case <-time.After(time.Second):
		t.Fatal("child-start receipt was not published")
	}
	if err := target.Cancel(context.Background(), request.Attempt); err != nil {
		t.Fatal(err)
	}
	close(api.releaseReceiptPublication)

	got := <-finished
	if !got.result.BootstrapStarted || !got.result.Started || got.result.Completed ||
		got.result.ChildStartReceipt == nil || providerCause(got.err) != "lifecycle_requested" {
		t.Fatalf("post-receipt cancellation = %#v, %v", got.result, got.err)
	}
	expected, err := executor.NewChildStartReceipt(
		request.Attempt,
		request.Command,
		map[string]string{"PATH": executor.CanonicalSandboxPATH},
		nil,
		got.result.ChildStartReceipt.Challenge,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !got.result.ChildStartReceipt.Matches(expected) {
		t.Fatalf("post-receipt cancellation rebound receipt = %#v", got.result.ChildStartReceipt)
	}
	api.mu.Lock()
	acknowledgements := api.childStartAcknowledgements
	api.mu.Unlock()
	if acknowledgements != 1 {
		t.Fatalf("post-receipt acknowledgements = %d, want 1", acknowledgements)
	}
}

func TestInspectRejectsReceiptReboundToAnotherAttempt(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, "none", nil)
	result, err := target.Execute(context.Background(), request)
	if err != nil || result.ChildStartReceipt == nil {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}

	reboundAttempt := request.Attempt
	reboundAttempt.Sequence++
	rebound, err := executor.NewChildStartReceipt(
		reboundAttempt,
		request.Command,
		map[string]string{"PATH": executor.CanonicalSandboxPATH},
		nil,
		strings.Repeat("9", 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	api.receiptOverride, err = json.Marshal(rebound)
	if err != nil {
		t.Fatal(err)
	}
	state, inspectErr := target.Inspect(context.Background(), request.Attempt)
	if state != executor.StateUnknown || providerCause(inspectErr) != "ambiguous_attempt" {
		t.Fatalf("Inspect(rebound receipt) = %q, %v", state, inspectErr)
	}
}

func TestRestartedExecutorMissingAttemptIsAmbiguousAndNotRerunnable(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, "none", nil)
	defer request.Destroy()

	result, err := target.Execute(context.Background(), request)
	if err != nil || !result.Completed {
		t.Fatalf("initial Execute() = %#v, %v", result, err)
	}
	if err := target.Destroy(context.Background(), request.Attempt); err != nil {
		t.Fatal(err)
	}

	restarted := newExecutorForTest(t, api)
	state, inspectErr := restarted.Inspect(context.Background(), request.Attempt)
	if state != executor.StateUnknown || providerCause(inspectErr) != "ambiguous_attempt" {
		t.Fatalf("restarted missing Inspect() = %q, %v", state, inspectErr)
	}
	if _, retryErr := restarted.Execute(context.Background(), request); !errors.Is(retryErr, executor.ErrAttemptExists) {
		t.Fatalf("restarted missing attempt was rerunnable: %v", retryErr)
	}
}

func TestEvictedDockerTombstoneBecomesAmbiguousAndNotRerunnable(t *testing.T) {
	api := newFakeEngine()
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, "none", nil)
	defer request.Destroy()

	first := request.Attempt
	for index := 0; index <= destroyedHistoryLimit; index++ {
		attempt := request.Attempt
		attempt.RunID = fmt.Sprintf("run-evicted-%04d", index)
		attempt.StartedAt = attempt.StartedAt.Add(time.Duration(index) * time.Nanosecond)
		if index == 0 {
			first = attempt
		}
		if err := target.Destroy(context.Background(), attempt); err != nil {
			t.Fatal(err)
		}
	}

	state, inspectErr := target.Inspect(context.Background(), first)
	if state != executor.StateUnknown || providerCause(inspectErr) != "ambiguous_attempt" {
		t.Fatalf("evicted tombstone Inspect() = %q, %v", state, inspectErr)
	}
	request.Attempt = first
	if _, retryErr := target.Execute(context.Background(), request); !errors.Is(retryErr, executor.ErrAttemptExists) {
		t.Fatalf("evicted tombstone was rerunnable: %v", retryErr)
	}
}

func TestCreateResponseLossInspectNotFoundIsStickyAcrossRestart(t *testing.T) {
	api := newFakeEngine()
	api.createErr = context.DeadlineExceeded
	api.createPersistsOnError = true
	api.containerInspectErr = errdefs.ErrNotFound
	target := newExecutorForTest(t, api)
	request := dockerRequest(t, "none", nil)
	defer request.Destroy()

	result, err := target.Execute(context.Background(), request)
	if result.Started || providerCause(err) != "ambiguous_attempt" {
		t.Fatalf("create response loss = %#v, %v", result, err)
	}

	restarted := newExecutorForTest(t, api)
	state, inspectErr := restarted.Inspect(context.Background(), request.Attempt)
	if state != executor.StateUnknown || providerCause(inspectErr) != "ambiguous_attempt" {
		t.Fatalf("restarted inspect-not-found = %q, %v", state, inspectErr)
	}
	restarted.mu.Lock()
	record := restarted.attempts[request.Attempt.Key()]
	sticky := record != nil && record.state == executor.StateUnknown && record.cleanupRequired
	restarted.mu.Unlock()
	if !sticky {
		t.Fatal("inspect-not-found did not persist sticky cleanup-required ambiguity")
	}
	if _, retryErr := restarted.Execute(context.Background(), request); !errors.Is(retryErr, executor.ErrAttemptExists) {
		t.Fatalf("inspect-not-found response loss was rerunnable: %v", retryErr)
	}
	state, inspectErr = restarted.Inspect(context.Background(), request.Attempt)
	if state != executor.StateUnknown || providerCause(inspectErr) != "ambiguous_attempt" {
		t.Fatalf("repeated inspect-not-found = %q, %v", state, inspectErr)
	}
}
