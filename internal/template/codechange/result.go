package codechange

import (
	"github.com/araihu/paje/internal/approval"
	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/run"
	"github.com/araihu/paje/internal/verification"
)

// Result is the durable, provider-neutral outcome of a code-change@v1 run.
type Result struct {
	RunID        string                `json:"run_id"`
	Status       run.Status            `json:"status"`
	BaseSHA      string                `json:"base_sha"`
	Artifact     artifact.Reference    `json:"artifact"`
	Verification []verification.Result `json:"verification,omitempty"`
	Approval     *approval.Result      `json:"approval,omitempty"`
	Publication  *publisher.Result     `json:"publication,omitempty"`
	Failure      *run.Failure          `json:"failure,omitempty"`
}
