package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/approval"
	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/template"
	"github.com/araihu/paje/internal/workerprofile"
)

func TestTransitionAcceptsLegalStateChanges(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	artifactRef := artifact.Reference{RunID: "run-1", Digest: strings.Repeat("a", 64), Size: 42}
	decision := approval.Result{RunID: "run-1", ArtifactDigest: artifactRef.Digest, Approved: true, Actor: "human", DecidedAt: now}
	publication := publisher.Result{Provider: "github", Branch: "paje/run-1", CommitSHA: strings.Repeat("b", 40), PullRequestID: "1", PullRequestURL: "https://example.test/pull/1"}

	tests := []struct {
		name   string
		record Record
		next   Status
	}{
		{name: "pending to resolving", record: validRecord(StatusPending), next: StatusResolving},
		{name: "resolving to executing", record: validRecord(StatusResolving), next: StatusExecuting},
		{name: "executing to awaiting approval", record: withArtifact(validRecord(StatusExecuting), artifactRef), next: StatusAwaitingApproval},
		{name: "awaiting approval to publishing", record: withApproval(withArtifact(validRecord(StatusAwaitingApproval), artifactRef), decision), next: StatusPublishing},
		{name: "publishing to succeeded", record: withOutcomeSaved(withPublication(withApproval(withArtifact(validRecord(StatusPublishing), artifactRef), decision), publication)), next: StatusSucceeded},
		{name: "artifact only finalize succeeds", record: withOutcomeSaved(withFinalize(withArtifact(withMode(validRecord(StatusExecuting), "artifact"), artifactRef))), next: StatusSucceeded},
		{name: "active fails", record: withFailure(validRecord(StatusExecuting)), next: StatusFailed},
		{name: "active cancels", record: withCanceledFailure(validRecord(StatusResolving)), next: StatusCanceled},
		{name: "approval declines", record: withApproval(withArtifact(validRecord(StatusAwaitingApproval), artifactRef), approval.Result{RunID: "run-1", ArtifactDigest: artifactRef.Digest, Actor: "human", DecidedAt: now, Reason: "no"}), next: StatusDeclined},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Transition(test.record, test.next, now.Add(time.Minute))
			if err != nil {
				t.Fatalf("Transition() error = %v", err)
			}
			if got.Status != test.next || got.Version != test.record.Version {
				t.Fatalf("Transition() = status %q version %d, want %q version %d", got.Status, got.Version, test.next, test.record.Version)
			}
			if !got.UpdatedAt.Equal(now.Add(time.Minute)) {
				t.Fatalf("UpdatedAt = %v", got.UpdatedAt)
			}
		})
	}
}

func TestTransitionRejectsIllegalStateChangesAndBrokenEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	ref := artifact.Reference{RunID: "run-1", Digest: strings.Repeat("a", 64), Size: 1}

	tests := []struct {
		name   string
		record Record
		next   Status
	}{
		{name: "backward", record: validRecord(StatusExecuting), next: StatusResolving},
		{name: "terminal immutable", record: validRecord(StatusSucceeded), next: StatusFailed},
		{name: "awaiting without artifact", record: validRecord(StatusExecuting), next: StatusAwaitingApproval},
		{name: "publishing without artifact", record: validRecord(StatusAwaitingApproval), next: StatusPublishing},
		{name: "pull request success without publication", record: withArtifact(validRecord(StatusPublishing), ref), next: StatusSucceeded},
		{name: "approval digest mismatch", record: withApproval(withArtifact(validRecord(StatusAwaitingApproval), ref), approval.Result{RunID: "run-1", ArtifactDigest: strings.Repeat("b", 64), Approved: true, Actor: "human", DecidedAt: now}), next: StatusPublishing},
		{name: "artifact only success outside finalize", record: withArtifact(withMode(validRecord(StatusExecuting), "artifact"), ref), next: StatusSucceeded},
		{name: "success before outcome memory", record: withPublication(withApproval(withArtifact(validRecord(StatusPublishing), ref), approval.Result{RunID: "run-1", ArtifactDigest: ref.Digest, Approved: true, Actor: "human", DecidedAt: now}), publisher.Result{Provider: "github"}), next: StatusSucceeded},
		{name: "pull request success without approval", record: withOutcomeSaved(withPublication(withArtifact(validRecord(StatusPublishing), ref), publisher.Result{Provider: "github"})), next: StatusSucceeded},
		{name: "failed without failure", record: validRecord(StatusExecuting), next: StatusFailed},
		{name: "retryable failure cannot become terminal", record: withRetryableFailure(validRecord(StatusExecuting)), next: StatusFailed},
		{name: "wrong cancellation class", record: withFailure(validRecord(StatusExecuting)), next: StatusCanceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Transition(test.record, test.next, now); !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("Transition() error = %v, want invalid transition or record", err)
			}
		})
	}
}

func TestUpsertStageRejectsRegressionAndClonesEvidence(t *testing.T) {
	t.Parallel()
	record := validRecord(StatusExecuting)
	started := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	first := StageResult{Name: "execute", Status: StageRunning, StartedAt: started, Attempts: 1, Evidence: map[string]string{"attempt": "one"}}
	record, err := UpsertStage(record, first)
	if err != nil {
		t.Fatal(err)
	}
	first.Evidence["attempt"] = "mutated"
	if record.Stages[0].Evidence["attempt"] != "one" {
		t.Fatal("UpsertStage() retained caller-owned evidence")
	}
	finished := StageResult{Name: "execute", Status: StageSucceeded, StartedAt: started, FinishedAt: started.Add(time.Minute), Attempts: 1}
	record, err = UpsertStage(record, finished)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertStage(record, first); err == nil {
		t.Fatal("finished stage overwritten by unfinished stage")
	}
	second := StageResult{Name: "execute", Status: StageRunning, StartedAt: started.Add(2 * time.Minute), Attempts: 2, Evidence: map[string]string{"attempt": "two"}}
	record, err = UpsertStage(record, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Stages) != 2 || record.Stages[0].Attempts != 1 || record.Stages[1].Attempts != 2 {
		t.Fatalf("attempt history = %#v, want attempts 1 then 2", record.Stages)
	}
	if _, err := UpsertStage(record, finished); err == nil {
		t.Fatal("historical lower attempt accepted after a newer attempt")
	}
}

func TestSafeDiagnosticStripsControlsAndTruncatesUTF8Safely(t *testing.T) {
	t.Parallel()
	got := SafeDiagnostic("\x00secret\n" + strings.Repeat("é", 3000))
	if strings.ContainsAny(got, "\x00\n") {
		t.Fatalf("SafeDiagnostic() retained control characters: %q", got[:20])
	}
	if len(got) > 4096 {
		t.Fatalf("SafeDiagnostic() length = %d, want <= 4096", len(got))
	}
	if !json.Valid([]byte(`"` + got + `"`)) {
		t.Fatal("SafeDiagnostic() produced invalid UTF-8")
	}
}

func TestRecordTerminal(t *testing.T) {
	t.Parallel()
	for _, status := range []Status{StatusSucceeded, StatusFailed, StatusCanceled, StatusDeclined} {
		if !validRecord(status).Terminal() {
			t.Errorf("%q should be terminal", status)
		}
	}
	for _, status := range []Status{StatusPending, StatusResolving, StatusExecuting, StatusAwaitingApproval, StatusPublishing} {
		if validRecord(status).Terminal() {
			t.Errorf("%q should be active", status)
		}
	}
}

func TestTerminalRecordIsNeverRetryable(t *testing.T) {
	t.Parallel()
	record := withRetryableFailure(validRecord(StatusFailed))
	if record.Retryable() {
		t.Fatal("terminal failed record reported retryable")
	}
	if err := Validate(record); err == nil {
		t.Fatal("terminal record with retryable failure validated")
	}

	canceled := withCanceledFailure(validRecord(StatusCanceled))
	if err := Validate(canceled); err != nil {
		t.Fatalf("valid canceled record rejected: %v", err)
	}
}

func TestPrepareSavePreservesWriteOnceEvidenceAndTerminalRecords(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	ref := artifact.Reference{RunID: "run-1", Digest: strings.Repeat("a", 64), Size: 1}
	decision := approval.Result{RunID: "run-1", ArtifactDigest: ref.Digest, Approved: true, Actor: "human", DecidedAt: now}
	publication := publisher.Result{Provider: "github", Branch: "paje/run-1", CommitSHA: strings.Repeat("b", 40), PullRequestID: "1", PullRequestURL: "https://example.test/pull/1"}
	current := withPublication(withApproval(withArtifact(validRecord(StatusExecuting), ref), decision), publication)
	current.BaseSHA = strings.Repeat("c", 40)
	current.MemorySnapshot = []memory.Memory{{ID: "m-1", Content: "original", Metadata: map[string]string{"scope": "run"}}}

	tests := []struct {
		name   string
		mutate func(*Record)
	}{
		{name: "base SHA", mutate: func(record *Record) { record.BaseSHA = strings.Repeat("d", 40) }},
		{name: "memory", mutate: func(record *Record) { record.MemorySnapshot[0].Content = "rewritten" }},
		{name: "artifact", mutate: func(record *Record) { record.Artifact.Digest = strings.Repeat("e", 64) }},
		{name: "approval", mutate: func(record *Record) { record.Approval.Actor = "other" }},
		{name: "publication", mutate: func(record *Record) { record.Publication.PullRequestID = "2" }},
		{name: "outcome regresses", mutate: func(record *Record) { record.OutcomeMemorySaved = false }},
	}
	current.OutcomeMemorySaved = true
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next := CloneRecord(current)
			test.mutate(&next)
			if _, err := PrepareSave(current, next); err == nil {
				t.Fatal("PrepareSave() accepted rewritten durable evidence")
			}
		})
	}

	terminal := withOutcomeSaved(withPublication(withApproval(withArtifact(validRecord(StatusSucceeded), ref), decision), publication))
	next := CloneRecord(terminal)
	next.Stages = append(next.Stages, StageResult{Name: "late", Status: StageRunning, StartedAt: now, Attempts: 1})
	if _, err := PrepareSave(terminal, next); err == nil {
		t.Fatal("PrepareSave() accepted post-terminal mutation")
	}
}

func TestResolvedProfileAndBindingsAreWriteOnce(t *testing.T) {
	t.Parallel()
	current := resolvedProfileRecord(t)

	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "profile content", mutate: func(fields map[string]any) {
			fields["worker_profile"].(map[string]any)["digest"] = strings.Repeat("f", 64)
		}},
		{name: "binding revision", mutate: func(fields map[string]any) {
			fields["secret_bindings"].([]any)[0].(map[string]any)["revision"] = float64(2)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			next := mutateRecordJSON(t, current, test.mutate)
			if _, err := PrepareSave(current, next); err == nil {
				t.Fatal("PrepareSave() accepted rewritten resolved state")
			}
		})
	}
}

func TestValidateRejectsResolvedProfileAndBindingsOnPending(t *testing.T) {
	t.Parallel()
	record := resolvedProfileRecord(t)
	record.Status = StatusPending

	if err := Validate(record); err == nil {
		t.Fatal("Validate() accepted resolved worker state on pending run")
	}
}

func TestPrepareSaveRequiresActiveResolveAttemptForFirstResolvedProfileAttachment(t *testing.T) {
	t.Parallel()
	resolved := resolvedProfileRecord(t)
	unresolved := CloneRecord(resolved)
	unresolved.WorkerProfile = nil
	unresolved.SecretBindings = nil

	t.Run("missing attempt", func(t *testing.T) {
		if _, err := PrepareSave(unresolved, resolved); err == nil {
			t.Fatal("PrepareSave() attached resolved worker state without resolve attempt")
		}
	})

	failure := Failure{
		Stage: "resolve", Class: FailureInternal, Retryable: true,
		Diagnostic: "worker lost", CauseCode: "worker_lost",
	}
	finished := CloneRecord(unresolved)
	finished.Stages = []StageResult{{
		Name: "resolve", Status: StageFailed,
		StartedAt: finished.UpdatedAt, FinishedAt: finished.UpdatedAt.Add(time.Second),
		Attempts: 1, Failure: &failure,
	}}
	finished.UpdatedAt = finished.UpdatedAt.Add(time.Second)
	finishedNext := CloneRecord(resolved)
	finishedNext.Stages = CloneRecord(finished).Stages
	finishedNext.UpdatedAt = finished.UpdatedAt
	t.Run("finished attempt", func(t *testing.T) {
		if _, err := PrepareSave(finished, finishedNext); err == nil {
			t.Fatal("PrepareSave() attached resolved worker state after resolve attempt finished")
		}
	})

	active := CloneRecord(unresolved)
	active.Stages = []StageResult{{
		Name: "resolve", Status: StageRunning,
		StartedAt: active.UpdatedAt, Attempts: 1,
	}}
	activeNext := CloneRecord(resolved)
	activeNext.Stages = append(CloneRecord(active).Stages, StageResult{
		Name: "resolve", Status: StageRunning,
		StartedAt: active.UpdatedAt.Add(time.Second), Attempts: 2,
	})
	activeNext.UpdatedAt = active.UpdatedAt.Add(time.Second)
	t.Run("different next attempt", func(t *testing.T) {
		if _, err := PrepareSave(active, activeNext); err == nil {
			t.Fatal("PrepareSave() attached resolved worker state to a different resolve attempt")
		}
	})

	matching := CloneRecord(resolved)
	matching.Stages = CloneRecord(active).Stages
	if _, err := PrepareSave(active, matching); err != nil {
		t.Fatalf("PrepareSave() rejected attachment by active resolve attempt: %v", err)
	}
}

func TestCloneRecordDeepClonesResolvedWorkerProfileAndBindings(t *testing.T) {
	t.Parallel()
	record := resolvedProfileRecord(t)
	cloned := CloneRecord(record)

	cloned.WorkerProfile.Secrets[0].Target = "/run/paje/secrets/other"
	cloned.SecretBindings[0].Revision = 2

	if got := record.WorkerProfile.Secrets[0].Target; got != "/run/paje/secrets/codex" {
		t.Fatalf("source worker profile target = %q after clone mutation", got)
	}
	if got := record.SecretBindings[0].Revision; got != 7 {
		t.Fatalf("source binding revision = %d after clone mutation", got)
	}
}

func TestValidateResolvedProfileAndBindingBindsCanonicalInput(t *testing.T) {
	t.Parallel()
	valid := resolvedProfileRecord(t)
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"worker_profile", "secret_bindings"} {
		if !strings.Contains(string(encoded), `"`+field+`"`) {
			t.Fatalf("resolved record omitted %q: %s", field, encoded)
		}
	}

	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "snapshot ID differs from input", mutate: func(fields map[string]any) {
			fields["worker_profile"].(map[string]any)["metadata"].(map[string]any)["name"] = "other"
		}},
		{name: "snapshot digest differs from content", mutate: func(fields map[string]any) {
			fields["worker_profile"].(map[string]any)["digest"] = strings.Repeat("f", 64)
		}},
		{name: "binding missing", mutate: func(fields map[string]any) {
			fields["secret_bindings"] = []any{}
		}},
		{name: "binding capability differs", mutate: func(fields map[string]any) {
			fields["secret_bindings"].([]any)[0].(map[string]any)["capability"] = "workload.other"
		}},
		{name: "binding revision differs", mutate: func(fields map[string]any) {
			fields["secret_bindings"].([]any)[0].(map[string]any)["revision"] = float64(8)
		}},
		{name: "binding duplicated", mutate: func(fields map[string]any) {
			binding := fields["secret_bindings"].([]any)[0]
			fields["secret_bindings"] = []any{binding, binding}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := mutateRecordJSON(t, valid, test.mutate)
			if err := Validate(record); err == nil {
				t.Fatal("Validate() accepted broken resolved binding")
			}
		})
	}
}

func TestPrepareSaveAllowsRetryExhaustionToBecomeFailed(t *testing.T) {
	t.Parallel()
	current := withRetryableFailure(validRecord(StatusExecuting))
	finished := current.UpdatedAt.Add(time.Minute)
	stageFailure := *current.Failure
	current.Stages = []StageResult{{
		Name: "execute", Status: StageFailed, StartedAt: current.UpdatedAt,
		FinishedAt: finished, Attempts: 1, Failure: &stageFailure,
	}}
	current.UpdatedAt = finished

	next := CloneRecord(current)
	next.Failure.Retryable = false
	next.Failure.CauseCode = "retries_exhausted"
	next.Stages[0].Failure.Retryable = false
	next.Stages[0].Failure.CauseCode = "retries_exhausted"
	next, err := Transition(next, StatusFailed, finished.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := PrepareSave(current, next)
	if err != nil {
		t.Fatalf("PrepareSave() rejected retry exhaustion: %v", err)
	}
	if saved.Status != StatusFailed || saved.Retryable() {
		t.Fatalf("saved exhausted record = status %q retryable %v", saved.Status, saved.Retryable())
	}
}

func TestPrepareSaveAllowsOnlyFinalizeBookkeepingOnTerminalRecords(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	ref := artifact.Reference{RunID: "run-1", Digest: strings.Repeat("a", 64), Size: 1}
	decline := approval.Result{RunID: "run-1", ArtifactDigest: ref.Digest, Actor: "human", DecidedAt: now, Reason: "no"}
	terminals := []Record{
		withFailure(validRecord(StatusFailed)),
		withCanceledFailure(validRecord(StatusCanceled)),
		withApproval(withArtifact(validRecord(StatusDeclined), ref), decline),
	}
	for _, current := range terminals {
		t.Run(string(current.Status), func(t *testing.T) {
			current.Stages = []StageResult{{
				Name: "execute", Status: StageFailed, StartedAt: now,
				FinishedAt: now.Add(time.Minute), Attempts: 1,
			}}
			current.UpdatedAt = now.Add(time.Minute)
			next := CloneRecord(current)
			next.OutcomeMemorySaved = true
			next.UpdatedAt = now.Add(2 * time.Minute)
			next, err := UpsertStage(next, StageResult{
				Name: "finalize", Status: StageSucceeded, StartedAt: next.UpdatedAt,
				FinishedAt: next.UpdatedAt.Add(time.Minute), Attempts: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			next.UpdatedAt = now.Add(3 * time.Minute)
			saved, err := PrepareSave(current, next)
			if err != nil {
				t.Fatalf("PrepareSave() terminal finalize error = %v", err)
			}
			if saved.Status != current.Status || !saved.OutcomeMemorySaved {
				t.Fatalf("terminal finalize changed status/bookkeeping: %#v", saved)
			}
		})
	}
}

func TestPrepareSaveRejectsUnrelatedTerminalFinalizationMutation(t *testing.T) {
	t.Parallel()
	current := withFailure(validRecord(StatusFailed))
	current.BaseSHA = strings.Repeat("a", 40)
	current.MemorySnapshot = []memory.Memory{{ID: "m-1", Content: "memory", Metadata: map[string]string{"scope": "run"}}}
	ref := artifact.Reference{RunID: current.ID, Digest: strings.Repeat("b", 64), Size: 1}
	current.Artifact = &ref
	approvalResult := approval.Result{RunID: current.ID, ArtifactDigest: ref.Digest, Approved: true, Actor: "human", DecidedAt: current.UpdatedAt}
	current.Approval = &approvalResult
	publication := publisher.Result{Provider: "github", Branch: "paje/run-1", CommitSHA: strings.Repeat("c", 40), PullRequestID: "1", PullRequestURL: "https://example.test/pull/1"}
	current.Publication = &publication
	current.Stages = []StageResult{{Name: "execute", Status: StageFailed, StartedAt: current.CreatedAt, FinishedAt: current.UpdatedAt, Attempts: 1}}

	tests := []struct {
		name   string
		mutate func(*Record)
	}{
		{name: "status", mutate: func(record *Record) { record.Status = StatusCanceled; record.Failure.Class = FailureCanceled }},
		{name: "base SHA", mutate: func(record *Record) { record.BaseSHA = strings.Repeat("d", 40) }},
		{name: "memory", mutate: func(record *Record) { record.MemorySnapshot[0].Content = "changed" }},
		{name: "artifact", mutate: func(record *Record) { record.Artifact.Digest = strings.Repeat("e", 64) }},
		{name: "approval", mutate: func(record *Record) { record.Approval.Actor = "other" }},
		{name: "publication", mutate: func(record *Record) { record.Publication.PullRequestID = "2" }},
		{name: "failure", mutate: func(record *Record) { record.Failure.Diagnostic = "changed" }},
		{name: "non-finalize stage", mutate: func(record *Record) { record.Stages[0].Evidence = map[string]string{"changed": "true"} }},
		{name: "outcome reversal", mutate: func(record *Record) { record.OutcomeMemorySaved = false }},
	}
	current.OutcomeMemorySaved = true
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next := CloneRecord(current)
			next.UpdatedAt = next.UpdatedAt.Add(time.Minute)
			test.mutate(&next)
			if _, err := PrepareSave(current, next); err == nil {
				t.Fatal("PrepareSave() accepted unrelated terminal mutation")
			}
		})
	}
}

func TestRetryExhaustionAllowsOnlyExactLatestFailureChange(t *testing.T) {
	t.Parallel()
	current := withRetryableFailure(validRecord(StatusExecuting))
	failure := *current.Failure
	current.Stages = []StageResult{{
		Name: "execute", Status: StageFailed, StartedAt: current.CreatedAt,
		FinishedAt: current.UpdatedAt, Attempts: 1, Failure: &failure,
	}}

	exhausted := func() Record {
		next := CloneRecord(current)
		next.Failure.Retryable = false
		next.Failure.CauseCode = "retries_exhausted"
		next.Stages[0].Failure.Retryable = false
		next.Stages[0].Failure.CauseCode = "retries_exhausted"
		return next
	}
	if _, err := PrepareSave(current, exhausted()); err != nil {
		t.Fatalf("exact retry exhaustion rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Failure)
	}{
		{name: "diagnostic", mutate: func(failure *Failure) { failure.Diagnostic = "rewritten" }},
		{name: "class", mutate: func(failure *Failure) { failure.Class = FailureInternal }},
		{name: "stage", mutate: func(failure *Failure) { failure.Stage = "resolve" }},
		{name: "cause", mutate: func(failure *Failure) { failure.CauseCode = "other" }},
		{name: "still retryable", mutate: func(failure *Failure) { failure.Retryable = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next := exhausted()
			test.mutate(next.Failure)
			test.mutate(next.Stages[0].Failure)
			if _, err := PrepareSave(current, next); err == nil {
				t.Fatal("PrepareSave() accepted imprecise retry exhaustion")
			}
		})
	}

	topOnly := exhausted()
	topOnly.Failure.Diagnostic = "top-only rewrite"
	if _, err := PrepareSave(current, topOnly); err == nil {
		t.Fatal("PrepareSave() accepted top-level failure rewrite")
	}
}

func TestRetryExhaustionRejectsHistoricalAttempt(t *testing.T) {
	t.Parallel()
	current := withRetryableFailure(validRecord(StatusExecuting))
	firstFailure := *current.Failure
	secondFailure := *current.Failure
	current.Stages = []StageResult{
		{Name: "execute", Status: StageFailed, StartedAt: current.CreatedAt, FinishedAt: current.UpdatedAt, Attempts: 1, Failure: &firstFailure},
		{Name: "execute", Status: StageFailed, StartedAt: current.CreatedAt, FinishedAt: current.UpdatedAt, Attempts: 2, Failure: &secondFailure},
	}
	next := CloneRecord(current)
	next.Stages[0].Failure.Retryable = false
	next.Stages[0].Failure.CauseCode = "retries_exhausted"
	if _, err := PrepareSave(current, next); err == nil {
		t.Fatal("PrepareSave() exhausted a non-latest attempt")
	}
}

func TestPrepareSaveBindsNewTopLevelFailureToLatestAttempt(t *testing.T) {
	t.Parallel()
	current := validRecord(StatusExecuting)
	unbound := withRetryableFailure(CloneRecord(current))
	if _, err := PrepareSave(current, unbound); err == nil {
		t.Fatal("PrepareSave() accepted an unbound top-level failure")
	}

	bound := withRetryableFailure(CloneRecord(current))
	stageFailure := *bound.Failure
	bound.Stages = []StageResult{{
		Name: "execute", Status: StageFailed, StartedAt: bound.CreatedAt,
		FinishedAt: bound.UpdatedAt, Attempts: 1, Failure: &stageFailure,
	}}
	if _, err := PrepareSave(current, bound); err != nil {
		t.Fatalf("PrepareSave() rejected latest-attempt-bound failure: %v", err)
	}
}

func TestPrepareSaveRejectsHistoricalFailureResurrection(t *testing.T) {
	t.Parallel()
	historicalFailure := Failure{Stage: "execute", Class: FailureAgent, Diagnostic: "old", CauseCode: "old_exit"}
	current := validRecord(StatusExecuting)
	current.Stages = []StageResult{{
		Name: "execute", Status: StageFailed, StartedAt: current.CreatedAt,
		FinishedAt: current.UpdatedAt, Attempts: 1, Failure: &historicalFailure,
	}}

	resurrected := CloneRecord(current)
	failureCopy := historicalFailure
	resurrected.Failure = &failureCopy
	if _, err := PrepareSave(current, resurrected); err == nil {
		t.Fatal("PrepareSave() resurrected unchanged historical failure")
	}

	terminalized, err := Transition(resurrected, StatusFailed, current.UpdatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSave(current, terminalized); err == nil {
		t.Fatal("PrepareSave() terminalized from resurrected historical failure")
	}
}

func TestPrepareSaveTerminalizesOnlyFromCurrentOrNewLatestFailureEvidence(t *testing.T) {
	t.Parallel()
	oldFailure := Failure{Stage: "execute", Class: FailureAgent, Diagnostic: "old", CauseCode: "old_exit"}
	current := validRecord(StatusExecuting)
	current.Failure = &oldFailure
	current.Stages = []StageResult{
		{Name: "execute", Status: StageFailed, StartedAt: current.CreatedAt, FinishedAt: current.UpdatedAt, Attempts: 1, Failure: &oldFailure},
		{Name: "execute", Status: StageSucceeded, StartedAt: current.CreatedAt, FinishedAt: current.UpdatedAt, Attempts: 2},
	}
	staleTerminal, err := Transition(current, StatusFailed, current.UpdatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSave(current, staleTerminal); err == nil {
		t.Fatal("PrepareSave() terminalized from stale non-latest failure evidence")
	}

	latestFailure := oldFailure
	current.Failure = &latestFailure
	current.Stages[1].Status = StageFailed
	current.Stages[1].Failure = &latestFailure
	latestTerminal, err := Transition(current, StatusFailed, current.UpdatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSave(current, latestTerminal); err != nil {
		t.Fatalf("PrepareSave() rejected current latest terminal evidence: %v", err)
	}
}

func TestPrepareSaveRejectsTerminalFailureEvidenceFromNonFailedStages(t *testing.T) {
	t.Parallel()
	for _, status := range []StageStatus{StageWarning, StageSucceeded, StageSkipped} {
		t.Run(string(status), func(t *testing.T) {
			current := validRecord(StatusExecuting)
			current.Stages = []StageResult{{
				Name: "execute", Status: StageRunning, StartedAt: current.CreatedAt, Attempts: 1,
			}}

			next := CloneRecord(current)
			failure := Failure{
				Stage: "execute", Class: FailureAgent, Diagnostic: "failed", CauseCode: "exit",
			}
			next.Failure = &failure
			next.Stages[0].Status = status
			next.Stages[0].FinishedAt = next.UpdatedAt
			stageFailure := failure
			next.Stages[0].Failure = &stageFailure
			next, err := Transition(next, StatusFailed, next.UpdatedAt.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}

			if _, err := PrepareSave(current, next); err == nil {
				t.Fatalf("PrepareSave() accepted terminal failure evidence from %q stage", status)
			}
		})
	}
}

func TestPrepareSaveAllowsFailureBindingOnlyFromNewOrProgressedLatestAttempt(t *testing.T) {
	t.Parallel()
	baseFailure := Failure{Stage: "execute", Class: FailureAgent, Diagnostic: "failed", CauseCode: "exit"}
	current := validRecord(StatusExecuting)
	current.Stages = []StageResult{{
		Name: "execute", Status: StageFailed, StartedAt: current.CreatedAt,
		FinishedAt: current.UpdatedAt, Attempts: 1, Failure: &baseFailure,
	}}

	newAttempt := CloneRecord(current)
	newFailure := Failure{Stage: "execute", Class: FailureAgent, Retryable: true, Diagnostic: "new", CauseCode: "temporary"}
	newAttempt.Failure = &newFailure
	stageFailure := newFailure
	newAttempt.Stages = append(newAttempt.Stages, StageResult{
		Name: "execute", Status: StageFailed, StartedAt: current.UpdatedAt.Add(time.Minute),
		FinishedAt: current.UpdatedAt.Add(2 * time.Minute), Attempts: 2, Failure: &stageFailure,
	})
	newAttempt.UpdatedAt = current.UpdatedAt.Add(2 * time.Minute)
	if _, err := PrepareSave(current, newAttempt); err != nil {
		t.Fatalf("PrepareSave() rejected new-attempt failure binding: %v", err)
	}

	running := validRecord(StatusExecuting)
	running.Stages = []StageResult{{
		Name: "execute", Status: StageRunning, StartedAt: running.CreatedAt, Attempts: 1,
	}}
	progressed := CloneRecord(running)
	progressedFailure := Failure{Stage: "execute", Class: FailureAgent, Diagnostic: "failed", CauseCode: "exit"}
	progressed.Failure = &progressedFailure
	stageProgressedFailure := progressedFailure
	progressed.Stages[0].Status = StageFailed
	progressed.Stages[0].FinishedAt = progressed.UpdatedAt
	progressed.Stages[0].Failure = &stageProgressedFailure
	if _, err := PrepareSave(running, progressed); err != nil {
		t.Fatalf("PrepareSave() rejected running-to-finished failure binding: %v", err)
	}
}

func TestPrepareSaveRejectsFailureSwitchToUnchangedOtherStage(t *testing.T) {
	t.Parallel()
	resolveFailure := Failure{Stage: "resolve", Class: FailureEnvironment, Diagnostic: "old", CauseCode: "network"}
	executeFailure := Failure{Stage: "execute", Class: FailureAgent, Diagnostic: "current", CauseCode: "exit"}
	current := validRecord(StatusExecuting)
	current.Failure = &executeFailure
	current.Stages = []StageResult{
		{Name: "resolve", Status: StageFailed, StartedAt: current.CreatedAt, FinishedAt: current.UpdatedAt, Attempts: 1, Failure: &resolveFailure},
		{Name: "execute", Status: StageFailed, StartedAt: current.CreatedAt, FinishedAt: current.UpdatedAt, Attempts: 1, Failure: &executeFailure},
	}
	next := CloneRecord(current)
	rebound := resolveFailure
	next.Failure = &rebound
	if _, err := PrepareSave(current, next); err == nil {
		t.Fatal("PrepareSave() rebound top-level failure to unchanged other-stage evidence")
	}
}

func TestPrepareSaveRejectsInsertionIntoHistoricalAttemptGap(t *testing.T) {
	t.Parallel()
	current := validRecord(StatusExecuting)
	current.Stages = []StageResult{
		{Name: "execute", Status: StageSucceeded, StartedAt: current.CreatedAt, FinishedAt: current.UpdatedAt, Attempts: 1},
		{Name: "execute", Status: StageSucceeded, StartedAt: current.CreatedAt, FinishedAt: current.UpdatedAt, Attempts: 3},
	}
	inserted := CloneRecord(current)
	inserted.Stages = []StageResult{current.Stages[0], {
		Name: "execute", Status: StageSucceeded, StartedAt: current.CreatedAt,
		FinishedAt: current.UpdatedAt, Attempts: 2,
	}, current.Stages[1]}
	if _, err := PrepareSave(current, inserted); err == nil {
		t.Fatal("PrepareSave() inserted a missing historical attempt")
	}

	appended := CloneRecord(current)
	appended.Stages = append(appended.Stages, StageResult{
		Name: "execute", Status: StageRunning, StartedAt: current.UpdatedAt.Add(time.Minute), Attempts: 4,
	})
	appended.UpdatedAt = current.UpdatedAt.Add(time.Minute)
	if _, err := PrepareSave(current, appended); err != nil {
		t.Fatalf("PrepareSave() rejected appended higher attempt: %v", err)
	}
}

func TestCompensateExecuteCancellationPreservesTerminalFailureEvidence(t *testing.T) {
	current := withFailure(validRecord(StatusFailed))
	current.Failure.Retryable = false
	current.Artifact = &artifact.Reference{
		RunID: current.ID, Digest: strings.Repeat("a", 64), Size: 42,
	}
	executeFailure := *current.Failure
	current.Stages = []StageResult{{
		Name: "execute", Status: StageFailed, StartedAt: current.CreatedAt,
		FinishedAt: current.UpdatedAt, Attempts: 2, Failure: &executeFailure,
		Evidence: map[string]string{"original": "preserved"},
	}}
	now := current.UpdatedAt.Add(time.Minute)

	next, err := CompensateExecuteCancellation(
		current, 2, current.Stages[0].StartedAt, now,
	)
	if err != nil {
		t.Fatalf("CompensateExecuteCancellation() error = %v", err)
	}
	if next.Status != StatusCanceled || next.Failure == nil ||
		next.Failure.Class != FailureCanceled || len(next.Stages) != 2 ||
		!reflect.DeepEqual(next.Stages[0], current.Stages[0]) ||
		!reflect.DeepEqual(next.Artifact, current.Artifact) {
		t.Fatalf("compensated record = %#v", next)
	}
	if _, err := PrepareSave(current, next); err != nil {
		t.Fatalf("PrepareSave() rejected cancellation compensation: %v", err)
	}
}

func TestCompensateExecuteCancellationRejectsOtherTerminalRunsAndOwnership(t *testing.T) {
	base := withFailure(validRecord(StatusFailed))
	base.Failure.Retryable = false
	stageFailure := *base.Failure
	base.Stages = []StageResult{{
		Name: "execute", Status: StageFailed, StartedAt: base.CreatedAt,
		FinishedAt: base.UpdatedAt, Attempts: 2, Failure: &stageFailure,
	}}
	now := base.UpdatedAt.Add(time.Minute)

	tests := []struct {
		name      string
		record    Record
		attempt   int
		startedAt time.Time
	}{
		{name: "other attempt", record: base, attempt: 1, startedAt: base.CreatedAt},
		{name: "other start", record: base, attempt: 2, startedAt: base.CreatedAt.Add(time.Second)},
		{name: "succeeded", record: validRecord(StatusSucceeded), attempt: 2, startedAt: base.CreatedAt},
		{name: "declined", record: validRecord(StatusDeclined), attempt: 2, startedAt: base.CreatedAt},
		{name: "already canceled", record: withCanceledFailure(validRecord(StatusCanceled)), attempt: 2, startedAt: base.CreatedAt},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := CompensateExecuteCancellation(
				test.record, test.attempt, test.startedAt, now,
			); err == nil {
				t.Fatal("CompensateExecuteCancellation() accepted unauthorized terminal rewrite")
			}
		})
	}
}

func TestPrepareSaveRejectsTamperedFailedToCanceledCompensation(t *testing.T) {
	current := withFailure(validRecord(StatusFailed))
	current.Failure.Retryable = false
	stageFailure := *current.Failure
	current.Stages = []StageResult{{
		Name: "execute", Status: StageFailed, StartedAt: current.CreatedAt,
		FinishedAt: current.UpdatedAt, Attempts: 1, Failure: &stageFailure,
	}}
	next, err := CompensateExecuteCancellation(
		current, 1, current.CreatedAt, current.UpdatedAt.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*Record)
	}{
		{name: "rewritten prior evidence", mutate: func(record *Record) {
			record.Stages[0].Failure.Diagnostic = "rewritten"
		}},
		{name: "wrong ownership evidence", mutate: func(record *Record) {
			record.Stages[1].Evidence["execute_attempt"] = "99"
		}},
		{name: "extra stage", mutate: func(record *Record) {
			record.Stages = append(record.Stages, record.Stages[1])
		}},
		{name: "artifact changed", mutate: func(record *Record) {
			record.Artifact = &artifact.Reference{
				RunID: record.ID, Digest: strings.Repeat("b", 64), Size: 1,
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tampered := CloneRecord(next)
			test.mutate(&tampered)
			if _, err := PrepareSave(current, tampered); err == nil {
				t.Fatal("PrepareSave() accepted tampered terminal cancellation compensation")
			}
		})
	}
}

func TestTerminalBookkeepingRequiresEligibleStatusAndAdvancedTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	failed := withFailure(validRecord(StatusFailed))

	sameTime := CloneRecord(failed)
	sameTime.OutcomeMemorySaved = true
	if _, err := PrepareSave(failed, sameTime); err == nil {
		t.Fatal("PrepareSave() accepted terminal bookkeeping without advancing UpdatedAt")
	}

	updatedOnly := CloneRecord(failed)
	updatedOnly.UpdatedAt = updatedOnly.UpdatedAt.Add(time.Minute)
	if _, err := PrepareSave(failed, updatedOnly); err == nil {
		t.Fatal("PrepareSave() accepted terminal UpdatedAt-only mutation")
	}

	noOp, err := PrepareSave(failed, CloneRecord(failed))
	if err != nil {
		t.Fatalf("PrepareSave() rejected coherent terminal no-op: %v", err)
	}
	if noOp.Version != failed.Version+1 || !noOp.UpdatedAt.Equal(failed.UpdatedAt) {
		t.Fatalf("terminal no-op = version %d updated %v", noOp.Version, noOp.UpdatedAt)
	}

	ref := artifact.Reference{RunID: "run-1", Digest: strings.Repeat("a", 64), Size: 1}
	succeeded := withOutcomeSaved(withFinalize(withArtifact(withMode(validRecord(StatusSucceeded), "artifact"), ref)))
	nextSucceeded := CloneRecord(succeeded)
	nextSucceeded.UpdatedAt = now.Add(time.Minute)
	nextSucceeded.Stages = append(nextSucceeded.Stages, StageResult{
		Name: "finalize", Status: StageSucceeded, StartedAt: now,
		FinishedAt: now.Add(time.Minute), Attempts: 2,
	})
	if _, err := PrepareSave(succeeded, nextSucceeded); err == nil {
		t.Fatal("PrepareSave() accepted succeeded terminal bookkeeping")
	}
}

func TestTransitionValidatesSourceBeforeRepairingStatus(t *testing.T) {
	t.Parallel()
	record := withFailure(validRecord(Status("unknown")))
	if _, err := Transition(record, StatusFailed, record.UpdatedAt.Add(time.Minute)); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Transition() error = %v, want ErrInvalidRecord", err)
	}
}

func TestCanonicalInputNormalizesObjectOrderAndWhitespace(t *testing.T) {
	t.Parallel()
	got, err := CanonicalInput(json.RawMessage(" { \"z\" : 2, \"a\" : 1 } \n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := json.RawMessage(`{"a":1,"z":2}`); !reflect.DeepEqual(got, want) {
		t.Fatalf("CanonicalInput() = %s, want %s", got, want)
	}
}

func validRecord(status Status) Record {
	now := time.Date(2026, 7, 24, 11, 0, 0, 0, time.UTC)
	return Record{
		ID: "run-1", Version: 1, Template: template.ID{Name: "code-change", Version: 1},
		InputHash: strings.Repeat("1", 64), Input: json.RawMessage(`{"task":"change"}`),
		Status: status, PublicationMode: "pull_request", RepositoryURI: "https://example.test/repo.git",
		BaseRef: "main", CreatedAt: now, UpdatedAt: now,
	}
}

func withArtifact(record Record, ref artifact.Reference) Record {
	record.Artifact = &ref
	return record
}
func withApproval(record Record, result approval.Result) Record {
	record.Approval = &result
	return record
}
func withPublication(record Record, result publisher.Result) Record {
	record.Publication = &result
	return record
}
func withMode(record Record, mode string) Record {
	record.PublicationMode = mode
	return record
}
func withFailure(record Record) Record {
	record.Failure = &Failure{Stage: "execute", Class: FailureAgent, Diagnostic: "failed", CauseCode: "exit"}
	return record
}
func withRetryableFailure(record Record) Record {
	record.Failure = &Failure{Stage: "execute", Class: FailureAgent, Retryable: true, Diagnostic: "transient", CauseCode: "temporary"}
	return record
}
func withCanceledFailure(record Record) Record {
	record.Failure = &Failure{Stage: "execute", Class: FailureCanceled, Diagnostic: "canceled", CauseCode: "context_canceled"}
	return record
}
func withFinalize(record Record) Record {
	record.Stages = []StageResult{{Name: "finalize", Status: StageSucceeded, StartedAt: record.CreatedAt, FinishedAt: record.UpdatedAt, Attempts: 1}}
	return record
}
func withOutcomeSaved(record Record) Record {
	record.OutcomeMemorySaved = true
	return record
}

func resolvedProfileRecord(t *testing.T) Record {
	t.Helper()
	record := validRecord(StatusResolving)
	input, err := CanonicalInput(json.RawMessage(`{
		"task_description":"change",
		"repository_uri":"https://example.test/repo.git",
		"base_ref":"main",
		"tags":{"app_id":"araihu-paje","user_id":"guilhermecastro"},
		"worker_profile":"codex-go@1",
		"profile":"go",
		"publication":{"mode":"pull_request","provider":"github","target_branch":"main"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	record.Input = input
	sum := sha256.Sum256(input)
	record.InputHash = hex.EncodeToString(sum[:])

	profile, err := workerprofile.Canonicalize(workerprofile.Snapshot{
		APIVersion: workerprofile.APIVersionV1Alpha1,
		Kind:       workerprofile.KindWorkerProfile,
		Metadata:   workerprofile.ProfileID{Name: "codex-go", Revision: 1},
		Runtime: workerprofile.Runtime{
			Kind: workerprofile.RuntimeOCI, Image: "example.test/worker@sha256:" + strings.Repeat("a", 64),
			Platform: "linux/amd64", Network: workerprofile.NetworkNone, ReadOnlyRoot: true,
		},
		Resources: workerprofile.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 64},
		Harness:   workerprofile.Harness{ID: "codex", Version: "0.144.5"},
		Secrets: []workerprofile.SecretRequirement{{
			Capability: "harness.codex-auth", BindingRevision: 7, Stage: workerprofile.StageAgent,
			Delivery: workerprofile.DeliveryDirectory, Target: "/run/paje/secrets/codex", Required: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved := mutateRecordJSON(t, record, func(fields map[string]any) {
		fields["worker_profile"] = profile
		fields["secret_bindings"] = []secret.BindingRef{{
			Capability: "harness.codex-auth", Revision: 7,
		}}
	})
	encoded, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"worker_profile"`) ||
		!strings.Contains(string(encoded), `"secret_bindings"`) {
		t.Fatalf("run.Record did not retain resolved state: %s", encoded)
	}
	return resolved
}

func mutateRecordJSON(t *testing.T, record Record, mutate func(map[string]any)) Record {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	mutate(fields)
	encoded, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var result Record
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
