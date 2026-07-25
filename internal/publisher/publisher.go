// Package publisher defines provider-neutral change publication contracts.
package publisher

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/araihu/paje/internal/artifact"
)

var (
	// ErrInvalidRequest indicates invalid publication input or output.
	ErrInvalidRequest = errors.New("invalid publication request")
	// ErrConflict indicates existing provider state conflicts with the requested
	// immutable publication.
	ErrConflict = errors.New("publication conflict")
	// ErrProviderUnavailable indicates the configured provider cannot serve the
	// request.
	ErrProviderUnavailable = errors.New("publication provider unavailable")
)

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Request describes an immutable artifact publication.
type Request struct {
	RunID      string             `json:"run_id"`
	Repository string             `json:"repository"`
	BaseSHA    string             `json:"base_sha"`
	TargetRef  string             `json:"target_ref"`
	Branch     string             `json:"branch"`
	Artifact   artifact.Reference `json:"artifact"`
	Title      string             `json:"title"`
	Body       string             `json:"body"`
	Draft      bool               `json:"draft"`
}

// Result identifies an immutable provider publication.
type Result struct {
	Provider       string `json:"provider"`
	Branch         string `json:"branch"`
	CommitSHA      string `json:"commit_sha"`
	PullRequestID  string `json:"pull_request_id"`
	PullRequestURL string `json:"pull_request_url"`
}

// Publisher publishes an immutable change artifact.
type Publisher interface {
	Publish(context.Context, Request) (Result, error)
}

// Validate rejects incomplete, unsafe, or inconsistent publication requests.
func (r Request) Validate() error {
	switch {
	case !validRunID(r.RunID):
		return invalid("run ID is invalid")
	case !present(r.Repository):
		return invalid("repository is required")
	case !validHex(r.BaseSHA, 40):
		return invalid("base SHA must be 40 hexadecimal characters")
	case !present(r.TargetRef):
		return invalid("target ref is required")
	case r.Branch != publicationBranch(r.RunID):
		return invalid("branch must be " + publicationBranch(r.RunID))
	case r.Artifact.RunID != r.RunID:
		return invalid("artifact run ID does not match request")
	case !validHex(r.Artifact.Digest, 64):
		return invalid("artifact digest must be 64 hexadecimal characters")
	case r.Artifact.Size <= 0:
		return invalid("artifact size must be positive")
	case !present(r.Title):
		return invalid("title is required")
	default:
		return nil
	}
}

// Validate rejects provider results that are incomplete or inconsistent with
// req.
func (r Result) Validate(req Request) error {
	if err := req.Validate(); err != nil {
		return fmt.Errorf("%w: result request: %v", ErrInvalidRequest, err)
	}
	if !present(r.Provider) {
		return invalid("provider is required")
	}
	if r.Branch != req.Branch {
		return invalid("result branch does not match request")
	}
	if !validHex(r.CommitSHA, 40) {
		return invalid("commit SHA must be 40 hexadecimal characters")
	}
	if !present(r.PullRequestID) {
		return invalid("pull request ID is required")
	}
	parsed, err := url.Parse(r.PullRequestURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return invalid("pull request URL must be an absolute HTTPS URL")
	}
	return nil
}

// CloneRequest returns an independent copy of req.
func CloneRequest(req Request) Request {
	cloned := req
	cloned.Artifact = artifact.Reference{
		RunID:  req.Artifact.RunID,
		Digest: req.Artifact.Digest,
		Size:   req.Artifact.Size,
	}
	return cloned
}

func invalid(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, message)
}

func publicationBranch(runID string) string {
	return "paje/code-change/" + runID
}

func validRunID(value string) bool {
	return runIDPattern.MatchString(value)
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
