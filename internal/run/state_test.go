package run

import (
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
	"github.com/araihu/paje/internal/template"
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
