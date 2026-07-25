package run

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/approval"
	"github.com/araihu/paje/internal/artifact"
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
		{name: "active cancels", record: withFailure(validRecord(StatusResolving)), next: StatusCanceled},
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
		{name: "failed without failure", record: validRecord(StatusExecuting), next: StatusFailed},
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
	first := StageResult{Name: "execute", Status: StageRunning, StartedAt: started, Attempts: 2, Evidence: map[string]string{"attempt": "two"}}
	record, err := UpsertStage(record, first)
	if err != nil {
		t.Fatal(err)
	}
	first.Evidence["attempt"] = "mutated"
	if record.Stages[0].Evidence["attempt"] != "two" {
		t.Fatal("UpsertStage() retained caller-owned evidence")
	}
	if _, err := UpsertStage(record, StageResult{Name: "execute", Status: StageRunning, StartedAt: started, Attempts: 1}); err == nil {
		t.Fatal("lower attempt accepted")
	}
	finished := StageResult{Name: "execute", Status: StageSucceeded, StartedAt: started, FinishedAt: started.Add(time.Minute), Attempts: 2}
	record, err = UpsertStage(record, finished)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertStage(record, first); err == nil {
		t.Fatal("finished stage overwritten by unfinished stage")
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
func withFinalize(record Record) Record {
	record.Stages = []StageResult{{Name: "finalize", Status: StageSucceeded, StartedAt: record.CreatedAt, FinishedAt: record.UpdatedAt, Attempts: 1}}
	return record
}
func withOutcomeSaved(record Record) Record {
	record.OutcomeMemorySaved = true
	return record
}
