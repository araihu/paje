package projection_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/araihu/paje/internal/controlplane/journal"
	"github.com/araihu/paje/internal/controlplane/projection"
)

func TestProjectionRebuildIsByteStableAcrossCheckpointBoundaries(t *testing.T) {
	t.Parallel()

	feed := []journal.Event{
		projectionEvent("event-1", "run-a", 1, 1),
		projectionEvent("event-2", "run-b", 1, 2),
		projectionEvent("event-3", "run-a", 2, 3),
	}
	first, err := projection.RebuildInstallation(feed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := projection.RebuildInstallation(append(append([]journal.Event(nil), feed[:1]...), feed[1:]...))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("rebuild bytes differ:\n%s\n%s", first, second)
	}
	run, err := projection.RebuildRun([]journal.Event{feed[0], feed[2]})
	if err != nil {
		t.Fatal(err)
	}
	runAgain, err := projection.RebuildRun(append([]journal.Event(nil), feed[0], feed[2]))
	if err != nil {
		t.Fatal(err)
	}
	if string(run) != string(runAgain) {
		t.Fatalf("run rebuild bytes differ:\n%s\n%s", run, runAgain)
	}
}

func TestProjectionFailsClosedForCorruptDuplicateReorderedGappedAndForeignEvents(t *testing.T) {
	t.Parallel()

	valid := []journal.Event{
		projectionEvent("event-1", "run-a", 1, 1),
		projectionEvent("event-2", "run-b", 1, 2),
		projectionEvent("event-3", "run-a", 2, 3),
	}
	tests := []struct {
		name   string
		mutate func([]journal.Event) []journal.Event
	}{
		{"corrupt", func(events []journal.Event) []journal.Event {
			events[1].PayloadDigest = "corrupt"
			return events
		}},
		{"duplicate ID", func(events []journal.Event) []journal.Event {
			events[1].ID = events[0].ID
			return events
		}},
		{"reordered", func(events []journal.Event) []journal.Event {
			events[0], events[1] = events[1], events[0]
			return events
		}},
		{"global gap", func(events []journal.Event) []journal.Event {
			events[2].JournalPosition = 4
			return events
		}},
		{"run gap", func(events []journal.Event) []journal.Event {
			events[2].RunSequence = 3
			return events
		}},
		{"unreserved action outcome", func(events []journal.Event) []journal.Event {
			events[0].ActionID = "action-a"
			events[0].Kind = journal.EventActionResult
			return events
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := append([]journal.Event(nil), valid...)
			if _, err := projection.RebuildInstallation(test.mutate(candidate)); !errors.Is(err, journal.ErrInvalidRecord) {
				t.Fatalf("RebuildInstallation(%s) error = %v, want ErrInvalidRecord", test.name, err)
			}
		})
	}

	foreign := []journal.Event{valid[0], valid[2]}
	foreign[1].ControlRunID = "run-b"
	if _, err := projection.RebuildRun(foreign); !errors.Is(err, journal.ErrInvalidRecord) {
		t.Fatalf("RebuildRun(foreign) error = %v, want ErrInvalidRecord", err)
	}
}

func projectionEvent(id, runID string, sequence uint64, position journal.JournalPosition) journal.Event {
	return journal.Event{
		ID: id, ControlRunID: runID, RunSequence: sequence, JournalPosition: position,
		Kind: journal.EventProjectionUpdated, PayloadDigest: projectionDigest(id),
		OccurredAt: time.Date(2026, time.July, 26, 0, 0, int(position), 0, time.UTC),
	}
}

func projectionDigest(value string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
}
