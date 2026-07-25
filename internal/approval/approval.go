// Package approval defines artifact-bound human approval contracts.
package approval

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/araihu/paje/internal/verification"
)

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Request contains the immutable evidence and publication bindings shown to an
// approver.
type Request struct {
	RunID             string                `json:"run_id"`
	TemplateID        string                `json:"template_id"`
	Repository        string                `json:"repository"`
	BaseSHA           string                `json:"base_sha"`
	TargetBranch      string                `json:"target_branch"`
	PublicationMode   string                `json:"publication_mode"`
	PublicationBranch string                `json:"publication_branch"`
	ArtifactDigest    string                `json:"artifact_digest"`
	Description       string                `json:"description"`
	AgentSummary      string                `json:"agent_summary"`
	ChangedPaths      []string              `json:"changed_paths"`
	Verification      []verification.Result `json:"verification"`
	Warnings          []string              `json:"warnings"`
}

// Result is a human decision bound to one run and one immutable artifact.
type Result struct {
	RunID          string    `json:"run_id"`
	ArtifactDigest string    `json:"artifact_digest"`
	Approved       bool      `json:"approved"`
	Actor          string    `json:"actor"`
	DecidedAt      time.Time `json:"decided_at"`
	Reason         string    `json:"reason,omitempty"`
}

// Gate requests a human decision for a proposed publication.
type Gate interface {
	RequestApproval(context.Context, Request) (Result, error)
}

// Validate rejects incomplete or inconsistent publication bindings.
func (r Request) Validate() error {
	switch {
	case !validRunID(r.RunID):
		return fmt.Errorf("validate approval request: invalid run ID")
	case !present(r.TemplateID):
		return fmt.Errorf("validate approval request: template ID is required")
	case !present(r.Repository):
		return fmt.Errorf("validate approval request: repository is required")
	case !validHex(r.BaseSHA, 40):
		return fmt.Errorf("validate approval request: base SHA must be 40 hexadecimal characters")
	case !present(r.TargetBranch):
		return fmt.Errorf("validate approval request: target branch is required")
	case !present(r.PublicationMode):
		return fmt.Errorf("validate approval request: publication mode is required")
	case r.PublicationBranch != publicationBranch(r.RunID):
		return fmt.Errorf("validate approval request: publication branch must be %q", publicationBranch(r.RunID))
	case !validHex(r.ArtifactDigest, 64):
		return fmt.Errorf("validate approval request: artifact digest must be 64 hexadecimal characters")
	default:
		return nil
	}
}

// Validate rejects a decision that is incomplete or not bound to req.
func (r Result) Validate(req Request) error {
	if err := req.Validate(); err != nil {
		return fmt.Errorf("validate approval result request: %w", err)
	}
	switch {
	case r.RunID != req.RunID:
		return fmt.Errorf("validate approval result: run ID does not match request")
	case r.ArtifactDigest != req.ArtifactDigest:
		return fmt.Errorf("validate approval result: artifact digest does not match request")
	case !present(r.Actor):
		return fmt.Errorf("validate approval result: actor is required")
	case r.DecidedAt.IsZero():
		return fmt.Errorf("validate approval result: decision time is required")
	case !isUTC(r.DecidedAt):
		return fmt.Errorf("validate approval result: decision time must be UTC")
	case !r.Approved && !present(r.Reason):
		return fmt.Errorf("validate approval result: declined decision requires a reason")
	default:
		return nil
	}
}

// CloneRequest returns an independent copy of req, including verification
// command arguments and environments.
func CloneRequest(req Request) Request {
	cloned := req
	cloned.ChangedPaths = append([]string(nil), req.ChangedPaths...)
	cloned.Warnings = append([]string(nil), req.Warnings...)
	if req.Verification != nil {
		cloned.Verification = make([]verification.Result, len(req.Verification))
		for i, result := range req.Verification {
			cloned.Verification[i] = result
			cloned.Verification[i].Command.Args = append([]string(nil), result.Command.Args...)
			cloned.Verification[i].Command.Environment = cloneMap(result.Command.Environment)
		}
	}
	return cloned
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

func publicationBranch(runID string) string {
	return "paje/code-change/" + runID
}

func validRunID(value string) bool {
	return runIDPattern.MatchString(value)
}

func isUTC(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 0
}

func present(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsRune(value, '\x00')
}

func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if !('0' <= char && char <= '9') &&
			!('a' <= char && char <= 'f') &&
			!('A' <= char && char <= 'F') {
			return false
		}
	}
	return true
}
