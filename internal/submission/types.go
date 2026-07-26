// Package submission defines the provider-neutral application boundary for
// scoped durable leaf-workflow submissions.
package submission

import (
	"encoding/json"
	"time"

	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

// Action is one independently grantable leaf-submission capability.
type Action string

const (
	ActionSubmitArtifact    Action = "submit:artifact"
	ActionSubmitPullRequest Action = "submit:pull_request"
	ActionRead              Action = "read"
	ActionCancel            Action = "cancel"
)

// RepositoryScope is one parsed repository identity granted to a principal.
type RepositoryScope struct {
	Host  string `json:"host"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// Principal is the authenticated, provider-neutral policy presented to the
// application service. Authentication material never enters this type.
type Principal struct {
	CredentialID string
	Subject      string
	UserID       string
	AppID        string
	Repositories []RepositoryScope
	Actions      map[Action]bool
	Harnesses    map[string]bool
	MaxDepth     int
}

// Origin binds a request to stable harness lifecycle identity.
type Origin struct {
	Harness     string `json:"harness"`
	SessionID   string `json:"session_id"`
	TurnID      string `json:"turn_id"`
	ParentRunID string `json:"parent_run_id,omitempty"`
}

// SubmitRequest is the already-authenticated application request.
type SubmitRequest struct {
	IdempotencyKey string
	Template       template.ID
	Input          json.RawMessage
	Origin         Origin
}

// Status is the provider-neutral public projection of a durable leaf run.
type Status string

const (
	StatusAccepted              Status = "accepted"
	StatusQueued                Status = "queued"
	StatusRunning               Status = "running"
	StatusAwaitingApproval      Status = "awaiting_approval"
	StatusCancellationRequested Status = "cancellation_requested"
	StatusSucceeded             Status = "succeeded"
	StatusFailed                Status = "failed"
	StatusCanceled              Status = "canceled"
	StatusDeclined              Status = "declined"
)

// TriggerReference identifies one provider run without exposing provider SDK
// types or credentials.
type TriggerReference struct {
	Provider      string `json:"provider"`
	ExternalRunID string `json:"external_run_id"`
}

// Record is the lineage-ready durable submission binding. It deliberately
// stores only a digest of the client idempotency key.
type Record struct {
	RunID                 string            `json:"run_id"`
	CredentialID          string            `json:"credential_id"`
	RequestDigest         string            `json:"request_digest"`
	IdempotencyKeyDigest  string            `json:"idempotency_key_digest"`
	Template              template.ID       `json:"template"`
	CanonicalInput        json.RawMessage   `json:"canonical_input"`
	Origin                Origin            `json:"origin"`
	RootRunID             string            `json:"root_run_id"`
	Depth                 int               `json:"depth"`
	Trigger               *TriggerReference `json:"trigger,omitempty"`
	CancellationRequested *time.Time        `json:"cancellation_requested,omitempty"`
	CreatedAt             time.Time         `json:"created_at"`
	UpdatedAt             time.Time         `json:"updated_at"`
}

// View combines a durable binding with its current provider-neutral state.
type View struct {
	Record Record
	Status Status
	Result *templatecodechange.Result
}

// Dependencies is the complete provider-neutral service bundle.
type Dependencies struct {
	Templates      *template.Registry
	Store          Store
	Trigger        Trigger
	Clock          func() time.Time
	SystemMaxDepth int
}
