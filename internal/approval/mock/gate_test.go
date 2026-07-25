package mock_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/approval"
	"github.com/araihu/paje/internal/approval/mock"
	"github.com/araihu/paje/internal/verification"
)

func TestGateRecordsDefensiveRequestsAndReturnsConfiguredDecision(t *testing.T) {
	t.Parallel()

	req := validRequest()
	decision := validDecision(req)
	wantErr := errors.New("approval unavailable")
	gate := mock.NewGate(decision, wantErr)

	got, err := gate.RequestApproval(context.Background(), req)
	if got != decision {
		t.Fatalf("RequestApproval() = %#v, want %#v", got, decision)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("RequestApproval() error = %v, want %v", err, wantErr)
	}

	req.ChangedPaths[0] = "mutated-at-caller"
	req.Verification[0].Command.Args[0] = "mutated-at-caller"
	req.Verification[0].Command.Environment["GOWORK"] = "mutated-at-caller"
	req.Warnings[0] = "mutated-at-caller"

	requests := gate.Requests()
	if len(requests) != 1 {
		t.Fatalf("len(Requests()) = %d, want 1", len(requests))
	}
	assertRequestNestedValues(t, requests[0])

	requests[0].ChangedPaths[0] = "mutated-at-reader"
	requests[0].Verification[0].Command.Args[0] = "mutated-at-reader"
	requests[0].Verification[0].Command.Environment["GOWORK"] = "mutated-at-reader"
	requests[0].Warnings[0] = "mutated-at-reader"
	assertRequestNestedValues(t, gate.Requests()[0])
}

func TestGateIsSafeForConcurrentCallers(t *testing.T) {
	t.Parallel()

	const callers = 32
	req := validRequest()
	gate := mock.NewGate(validDecision(req), nil)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := gate.RequestApproval(context.Background(), req); err != nil {
				t.Errorf("RequestApproval() error = %v", err)
			}
			_ = gate.Requests()
		}()
	}
	wg.Wait()

	if got := len(gate.Requests()); got != callers {
		t.Fatalf("len(Requests()) = %d, want %d", got, callers)
	}
}

func TestApprovalValidationBindsDecisionToRequest(t *testing.T) {
	t.Parallel()

	req := validRequest()
	if err := req.Validate(); err != nil {
		t.Fatalf("valid Request.Validate() error = %v", err)
	}
	if err := validDecision(req).Validate(req); err != nil {
		t.Fatalf("valid Result.Validate() error = %v", err)
	}
	zeroOffset := validDecision(req)
	zeroOffset.DecidedAt = time.Unix(1, 0).In(time.FixedZone("zero-offset", 0))
	if err := zeroOffset.Validate(req); err != nil {
		t.Fatalf("zero-offset UTC Result.Validate() error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*approval.Result)
	}{
		{
			name: "different run",
			mutate: func(result *approval.Result) {
				result.RunID = "run-elsewhere"
			},
		},
		{
			name: "different artifact",
			mutate: func(result *approval.Result) {
				result.ArtifactDigest = strings.Repeat("c", 64)
			},
		},
		{
			name: "missing actor",
			mutate: func(result *approval.Result) {
				result.Actor = ""
			},
		},
		{
			name: "zero decision time",
			mutate: func(result *approval.Result) {
				result.DecidedAt = time.Time{}
			},
		},
		{
			name: "non UTC decision time",
			mutate: func(result *approval.Result) {
				result.DecidedAt = time.Unix(1, 0).In(time.FixedZone("elsewhere", 3600))
			},
		},
		{
			name: "decline without reason",
			mutate: func(result *approval.Result) {
				result.Approved = false
				result.Reason = ""
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validDecision(req)
			tt.mutate(&result)
			if err := result.Validate(req); err == nil {
				t.Fatal("Result.Validate() error = nil, want rejection")
			}
		})
	}
}

func TestApprovalRequestValidationRejectsInvalidBindings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*approval.Request)
	}{
		{name: "missing run", mutate: func(req *approval.Request) { req.RunID = "" }},
		{name: "unsafe run", mutate: func(req *approval.Request) { req.RunID = "../run" }},
		{name: "run with invalid ref punctuation", mutate: func(req *approval.Request) { req.RunID = "run.123" }},
		{name: "run too long", mutate: func(req *approval.Request) { req.RunID = strings.Repeat("r", 129) }},
		{name: "missing template", mutate: func(req *approval.Request) { req.TemplateID = "" }},
		{name: "missing repository", mutate: func(req *approval.Request) { req.Repository = "" }},
		{name: "invalid base SHA", mutate: func(req *approval.Request) { req.BaseSHA = strings.Repeat("a", 39) }},
		{name: "missing target", mutate: func(req *approval.Request) { req.TargetBranch = "" }},
		{name: "missing mode", mutate: func(req *approval.Request) { req.PublicationMode = "" }},
		{name: "wrong branch", mutate: func(req *approval.Request) { req.PublicationBranch = "other/run-123" }},
		{name: "invalid digest", mutate: func(req *approval.Request) { req.ArtifactDigest = strings.Repeat("b", 63) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest()
			tt.mutate(&req)
			if err := req.Validate(); err == nil {
				t.Fatal("Request.Validate() error = nil, want rejection")
			}
		})
	}
}

func validRequest() approval.Request {
	return approval.Request{
		RunID:             "run-123",
		TemplateID:        "code-change@v1",
		Repository:        "github.com/araihu/paje",
		BaseSHA:           strings.Repeat("a", 40),
		TargetBranch:      "main",
		PublicationMode:   "pull_request",
		PublicationBranch: "paje/code-change/run-123",
		ArtifactDigest:    strings.Repeat("b", 64),
		Description:       "update parser",
		AgentSummary:      "parser updated",
		ChangedPaths:      []string{"parser.go"},
		Verification: []verification.Result{{
			Command: verification.Command{
				Name:        "unit",
				Args:        []string{"test"},
				Environment: map[string]string{"GOWORK": "off"},
			},
			Passed: true,
		}},
		Warnings: []string{"optional check skipped"},
	}
}

func validDecision(req approval.Request) approval.Result {
	return approval.Result{
		RunID:          req.RunID,
		ArtifactDigest: req.ArtifactDigest,
		Approved:       true,
		Actor:          "reviewer@example.test",
		DecidedAt:      time.Unix(1, 0).UTC(),
	}
}

func assertRequestNestedValues(t *testing.T, got approval.Request) {
	t.Helper()
	if got.ChangedPaths[0] != "parser.go" {
		t.Errorf("ChangedPaths[0] = %q, want parser.go", got.ChangedPaths[0])
	}
	if got.Verification[0].Command.Args[0] != "test" {
		t.Errorf("Verification Command Args[0] = %q, want test", got.Verification[0].Command.Args[0])
	}
	if got.Verification[0].Command.Environment["GOWORK"] != "off" {
		t.Errorf("Verification Command Environment[GOWORK] = %q, want off", got.Verification[0].Command.Environment["GOWORK"])
	}
	if got.Warnings[0] != "optional check skipped" {
		t.Errorf("Warnings[0] = %q, want optional check skipped", got.Warnings[0])
	}
}
