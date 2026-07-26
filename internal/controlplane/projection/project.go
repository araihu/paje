// Package projection rebuilds deterministic run and installation views from
// the authoritative typed journal.
package projection

import (
	"fmt"

	"github.com/araihu/paje/internal/controlplane/journal"
)

type Run struct {
	SchemaVersion uint32          `json:"schema_version"`
	ControlRunID  string          `json:"control_run_id"`
	RunSequence   uint64          `json:"run_sequence"`
	Events        []journal.Event `json:"events"`
}

type Installation struct {
	SchemaVersion   uint32                  `json:"schema_version"`
	JournalPosition journal.JournalPosition `json:"journal_position"`
	Events          []journal.Event         `json:"events"`
}

func RebuildRun(events []journal.Event) ([]byte, error) {
	projection := Run{SchemaVersion: journal.SchemaVersion, Events: []journal.Event{}}
	seenIDs := make(map[string]bool, len(events))
	reservedActions := make(map[string]bool)
	actionEvents := make(map[string][]journal.Event)
	var priorPosition journal.JournalPosition
	for index, event := range events {
		if err := journal.ValidateEvent(event, true); err != nil {
			return nil, err
		}
		if index == 0 {
			projection.ControlRunID = event.ControlRunID
		}
		if event.ControlRunID != projection.ControlRunID ||
			event.RunSequence != uint64(index+1) ||
			event.JournalPosition <= priorPosition || seenIDs[event.ID] {
			return nil, fmt.Errorf("%w: corrupt or foreign run event at index %d", journal.ErrInvalidRecord, index)
		}
		seenIDs[event.ID] = true
		if err := validateActionOrder(event, reservedActions, actionEvents); err != nil {
			return nil, err
		}
		priorPosition = event.JournalPosition
		projection.Events = append(projection.Events, event)
		projection.RunSequence = event.RunSequence
	}
	return journal.CanonicalJSON(projection)
}

func RebuildInstallation(events []journal.Event) ([]byte, error) {
	projection := Installation{SchemaVersion: journal.SchemaVersion, Events: []journal.Event{}}
	seenIDs := make(map[string]bool, len(events))
	runSequences := make(map[string]uint64)
	actionRuns := make(map[string]string)
	actionEvents := make(map[string][]journal.Event)
	reservedActions := make(map[string]bool)
	for index, event := range events {
		if err := journal.ValidateEvent(event, true); err != nil {
			return nil, err
		}
		if event.JournalPosition != journal.JournalPosition(index+1) || seenIDs[event.ID] {
			return nil, fmt.Errorf("%w: corrupt installation event at index %d", journal.ErrInvalidRecord, index)
		}
		runSequences[event.ControlRunID]++
		if event.RunSequence != runSequences[event.ControlRunID] {
			return nil, fmt.Errorf("%w: gapped run %q at position %d", journal.ErrInvalidRecord, event.ControlRunID, event.JournalPosition)
		}
		if event.ActionID != "" {
			if run := actionRuns[event.ActionID]; run != "" && run != event.ControlRunID {
				return nil, fmt.Errorf("%w: action %q crosses runs", journal.ErrInvalidRecord, event.ActionID)
			}
			actionRuns[event.ActionID] = event.ControlRunID
			if err := validateActionOrder(event, reservedActions, actionEvents); err != nil {
				return nil, err
			}
		}
		seenIDs[event.ID] = true
		projection.Events = append(projection.Events, event)
		projection.JournalPosition = event.JournalPosition
	}
	return journal.CanonicalJSON(projection)
}

func validateActionOrder(
	event journal.Event,
	reserved map[string]bool,
	history map[string][]journal.Event,
) error {
	if event.ActionID == "" {
		return nil
	}
	switch event.Kind {
	case journal.EventActionReserved, journal.EventMigrationAction:
		if reserved[event.ActionID] {
			return fmt.Errorf("%w: duplicate action reservation %q", journal.ErrInvalidRecord, event.ActionID)
		}
		reserved[event.ActionID] = true
	default:
		if !reserved[event.ActionID] {
			return fmt.Errorf("%w: event for unreserved action %q", journal.ErrInvalidRecord, event.ActionID)
		}
	}
	if err := journal.ValidateOutcomeTransition(history[event.ActionID], event); err != nil {
		return fmt.Errorf("%w: invalid action outcome at position %d", journal.ErrInvalidRecord, event.JournalPosition)
	}
	history[event.ActionID] = append(history[event.ActionID], event)
	return nil
}
