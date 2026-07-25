// Package run defines durable workflow run records and their persistence port.
package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/araihu/paje/internal/approval"
	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/memory"
	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/template"
)

var (
	ErrNotFound            = errors.New("run not found")
	ErrAlreadyExists       = errors.New("run already exists")
	ErrVersionConflict     = errors.New("run version conflict")
	ErrIdempotencyConflict = errors.New("run idempotency conflict")
	ErrInvalidRecord       = errors.New("invalid run record")
	ErrInvalidTransition   = errors.New("invalid run transition")
)

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type Status string

const (
	StatusPending          Status = "pending"
	StatusResolving        Status = "resolving"
	StatusExecuting        Status = "executing"
	StatusAwaitingApproval Status = "awaiting_approval"
	StatusPublishing       Status = "publishing"
	StatusSucceeded        Status = "succeeded"
	StatusFailed           Status = "failed"
	StatusCanceled         Status = "canceled"
	StatusDeclined         Status = "declined"
)

type FailureClass string

const (
	FailureInput        FailureClass = "input"
	FailureEnvironment  FailureClass = "environment"
	FailureAgent        FailureClass = "agent"
	FailureVerification FailureClass = "verification"
	FailurePolicy       FailureClass = "policy"
	FailureApproval     FailureClass = "approval"
	FailurePublication  FailureClass = "publication"
	FailureCleanup      FailureClass = "cleanup"
	FailureCanceled     FailureClass = "canceled"
	FailureInternal     FailureClass = "internal"
)

type StageStatus string

const (
	StageRunning   StageStatus = "running"
	StageSucceeded StageStatus = "succeeded"
	StageSkipped   StageStatus = "skipped"
	StageWarning   StageStatus = "warning"
	StageFailed    StageStatus = "failed"
)

type StageResult struct {
	Name       string            `json:"name"`
	Status     StageStatus       `json:"status"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at,omitempty"`
	Attempts   int               `json:"attempts"`
	Evidence   map[string]string `json:"evidence,omitempty"`
	Failure    *Failure          `json:"failure,omitempty"`
}

type Failure struct {
	Stage      string       `json:"stage"`
	Class      FailureClass `json:"class"`
	Retryable  bool         `json:"retryable"`
	Diagnostic string       `json:"diagnostic"`
	CauseCode  string       `json:"cause_code"`
}

type Record struct {
	ID                 string              `json:"id"`
	Version            uint64              `json:"version"`
	Template           template.ID         `json:"template"`
	IdempotencyKey     string              `json:"idempotency_key,omitempty"`
	InputHash          string              `json:"input_hash"`
	Input              json.RawMessage     `json:"input"`
	Status             Status              `json:"status"`
	PublicationMode    string              `json:"publication_mode"`
	RepositoryURI      string              `json:"repository_uri"`
	BaseRef            string              `json:"base_ref"`
	BaseSHA            string              `json:"base_sha,omitempty"`
	MemorySnapshot     []memory.Memory     `json:"memory_snapshot,omitempty"`
	Artifact           *artifact.Reference `json:"artifact,omitempty"`
	Approval           *approval.Result    `json:"approval,omitempty"`
	Publication        *publisher.Result   `json:"publication,omitempty"`
	OutcomeMemorySaved bool                `json:"outcome_memory_saved"`
	Stages             []StageResult       `json:"stages"`
	Failure            *Failure            `json:"failure,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	UpdatedAt          time.Time           `json:"updated_at"`
}

type Reservation struct {
	NewRunID        string
	Template        template.ID
	IdempotencyKey  string
	InputHash       string
	Input           json.RawMessage
	RepositoryURI   string
	BaseRef         string
	PublicationMode string
	CreatedAt       time.Time
}

type Store interface {
	Reserve(context.Context, Reservation) (record Record, created bool, err error)
	Load(context.Context, string) (Record, error)
	Save(context.Context, Record, uint64) (Record, error)
}

func (r Record) Terminal() bool {
	switch r.Status {
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusDeclined:
		return true
	default:
		return false
	}
}

func (r Record) Retryable() bool {
	return !r.Terminal() && r.Failure != nil && r.Failure.Retryable
}

func SafeDiagnostic(value string) string {
	var builder strings.Builder
	builder.Grow(min(len(value), 4096))
	for _, character := range value {
		if unicode.IsControl(character) {
			continue
		}
		builder.WriteRune(character)
		if builder.Len() > 4096 {
			break
		}
	}
	result := builder.String()
	if len(result) <= 4096 {
		return result
	}
	result = result[:4096]
	for !utf8.ValidString(result) {
		result = result[:len(result)-1]
	}
	return result
}

func NewRecord(reservation Reservation) (Record, error) {
	if err := ValidateReservation(reservation); err != nil {
		return Record{}, err
	}
	canonical, err := CanonicalInput(reservation.Input)
	if err != nil {
		return Record{}, err
	}
	record := Record{
		ID: reservation.NewRunID, Version: 1, Template: reservation.Template,
		IdempotencyKey: reservation.IdempotencyKey, InputHash: reservation.InputHash,
		Input: canonical, Status: StatusPending,
		PublicationMode: reservation.PublicationMode, RepositoryURI: reservation.RepositoryURI,
		BaseRef: reservation.BaseRef, Stages: []StageResult{},
		CreatedAt: reservation.CreatedAt, UpdatedAt: reservation.CreatedAt,
	}
	if err := Validate(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func ValidateReservation(reservation Reservation) error {
	if _, err := CanonicalInput(reservation.Input); err != nil {
		return err
	}
	switch {
	case !runIDPattern.MatchString(reservation.NewRunID):
		return invalidRecord("run ID is invalid")
	case strings.TrimSpace(reservation.Template.Name) == "" || reservation.Template.Version <= 0:
		return invalidRecord("template is invalid")
	case strings.TrimSpace(reservation.InputHash) == "":
		return invalidRecord("input hash is required")
	case strings.TrimSpace(reservation.RepositoryURI) == "":
		return invalidRecord("repository URI is required")
	case strings.TrimSpace(reservation.BaseRef) == "":
		return invalidRecord("base ref is required")
	case reservation.PublicationMode != "artifact" && reservation.PublicationMode != "pull_request":
		return invalidRecord("publication mode is invalid")
	case reservation.CreatedAt.IsZero():
		return invalidRecord("created time is required")
	default:
		return nil
	}
}

// CanonicalInput returns deterministic JSON with insignificant whitespace and
// object-key ordering normalized without converting numbers through float64.
func CanonicalInput(input json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 {
		return nil, invalidRecord("input must be valid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, invalidRecord("input must be valid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, invalidRecord("input must contain one JSON value")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, invalidRecord("input cannot be canonicalized")
	}
	return json.RawMessage(canonical), nil
}

func CloneRecord(source Record) Record {
	cloned := source
	cloned.Input = append(json.RawMessage(nil), source.Input...)
	if source.MemorySnapshot != nil {
		cloned.MemorySnapshot = make([]memory.Memory, len(source.MemorySnapshot))
		for index, item := range source.MemorySnapshot {
			cloned.MemorySnapshot[index] = item
			cloned.MemorySnapshot[index].Metadata = cloneMap(item.Metadata)
		}
	}
	if source.Artifact != nil {
		value := *source.Artifact
		cloned.Artifact = &value
	}
	if source.Approval != nil {
		value := *source.Approval
		cloned.Approval = &value
	}
	if source.Publication != nil {
		value := *source.Publication
		cloned.Publication = &value
	}
	if source.Failure != nil {
		value := *source.Failure
		cloned.Failure = &value
	}
	if source.Stages != nil {
		cloned.Stages = make([]StageResult, len(source.Stages))
		for index, stage := range source.Stages {
			cloned.Stages[index] = cloneStage(stage)
		}
	}
	return cloned
}

func SameImmutableIdentity(current, next Record) bool {
	return current.ID == next.ID &&
		current.Template == next.Template &&
		current.IdempotencyKey == next.IdempotencyKey &&
		current.InputHash == next.InputHash &&
		bytes.Equal(current.Input, next.Input) &&
		current.PublicationMode == next.PublicationMode &&
		current.RepositoryURI == next.RepositoryURI &&
		current.BaseRef == next.BaseRef &&
		current.CreatedAt.Equal(next.CreatedAt)
}

func invalidRecord(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidRecord, message)
}

func cloneStage(stage StageResult) StageResult {
	stage.Evidence = cloneMap(stage.Evidence)
	if stage.Failure != nil {
		failure := *stage.Failure
		stage.Failure = &failure
	}
	return stage
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
