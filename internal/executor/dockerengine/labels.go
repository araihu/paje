package dockerengine

import (
	"strconv"
	"time"

	"github.com/araihu/paje/internal/executor"
)

const (
	labelPrefix            = "com.araihu.paje."
	labelExecutor          = labelPrefix + "executor"
	labelResource          = labelPrefix + "resource"
	labelKey               = labelPrefix + "attempt-key"
	labelRunID             = labelPrefix + "run-id"
	labelStage             = labelPrefix + "stage"
	labelAttempt           = labelPrefix + "attempt"
	labelStarted           = labelPrefix + "started-at"
	labelPurpose           = labelPrefix + "purpose"
	labelSequence          = labelPrefix + "sequence"
	labelCommandDigest     = labelPrefix + "command-digest"
	labelEnvironmentDigest = labelPrefix + "environment-digest"
	labelReceiptBinding    = labelPrefix + "child-start-binding"

	resourceContainer = "container"
	resourceNetwork   = "network"
)

func attemptLabels(attempt executor.AttemptID, resource string) map[string]string {
	return map[string]string{
		labelExecutor: "dockerengine-v1",
		labelResource: resource,
		labelKey:      attempt.Key(),
		labelRunID:    attempt.RunID,
		labelStage:    attempt.Stage,
		labelAttempt:  strconv.Itoa(attempt.Attempt),
		labelStarted:  attempt.StartedAt.UTC().Format(time.RFC3339Nano),
		labelPurpose:  string(attempt.Purpose),
		labelSequence: strconv.Itoa(attempt.Sequence),
	}
}

func boundContainerLabels(attempt executor.AttemptID, receipt executor.ChildStartReceipt) map[string]string {
	labels := attemptLabels(attempt, resourceContainer)
	labels[labelCommandDigest] = receipt.CommandDigest
	labels[labelEnvironmentDigest] = receipt.EnvironmentDigest
	labels[labelReceiptBinding] = receipt.BindingDigest()
	return labels
}

func resourceName(attempt executor.AttemptID, suffix string) string {
	key := attempt.Key()
	if len(key) > 32 {
		key = key[:32]
	}
	return "paje-" + key + "-" + suffix
}
