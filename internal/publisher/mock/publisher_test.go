package mock_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/publisher/mock"
)

func TestPublisherRecordsDefensiveRequestsAndReturnsConfiguredResult(t *testing.T) {
	t.Parallel()

	req := validRequest()
	want := validResult(req)
	wantErr := errors.New("provider failed")
	pub := mock.NewPublisher(want, wantErr)

	got, err := pub.Publish(context.Background(), req)
	if got != want {
		t.Fatalf("Publish() = %#v, want %#v", got, want)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Publish() error = %v, want %v", err, wantErr)
	}
	if got := pub.CallCount(); got != 1 {
		t.Fatalf("CallCount() = %d, want 1", got)
	}

	req.Artifact.RunID = "mutated-at-caller"
	requests := pub.Requests()
	if len(requests) != 1 {
		t.Fatalf("len(Requests()) = %d, want 1", len(requests))
	}
	if requests[0].Artifact.RunID != "run-123" {
		t.Fatalf("recorded Artifact.RunID = %q, want run-123", requests[0].Artifact.RunID)
	}

	requests[0].Artifact.RunID = "mutated-at-reader"
	if got := pub.Requests()[0].Artifact.RunID; got != "run-123" {
		t.Fatalf("second snapshot Artifact.RunID = %q, want run-123", got)
	}
}

func TestPublisherIsSafeForConcurrentCallers(t *testing.T) {
	t.Parallel()

	const callers = 32
	req := validRequest()
	pub := mock.NewPublisher(validResult(req), nil)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pub.Publish(context.Background(), req); err != nil {
				t.Errorf("Publish() error = %v", err)
			}
			_ = pub.Requests()
		}()
	}
	wg.Wait()

	if got := pub.CallCount(); got != callers {
		t.Fatalf("CallCount() = %d, want %d", got, callers)
	}
}

func TestDeclinedApprovalDoesNotInvokePublisher(t *testing.T) {
	t.Parallel()

	pub := mock.NewPublisher(validResult(validRequest()), nil)
	publishAfterApproval := func(approved bool) error {
		if !approved {
			return nil
		}
		_, err := pub.Publish(context.Background(), validRequest())
		return err
	}

	if err := publishAfterApproval(false); err != nil {
		t.Fatalf("declined workflow error = %v", err)
	}
	if got := pub.CallCount(); got != 0 {
		t.Fatalf("CallCount() = %d, want 0", got)
	}
	if got := len(pub.Requests()); got != 0 {
		t.Fatalf("len(Requests()) = %d, want 0", got)
	}
}

func TestPublisherRequestValidation(t *testing.T) {
	t.Parallel()

	if err := validRequest().Validate(); err != nil {
		t.Fatalf("valid Request.Validate() error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*publisher.Request)
	}{
		{name: "missing run", mutate: func(req *publisher.Request) { req.RunID = "" }},
		{name: "unsafe run", mutate: func(req *publisher.Request) { req.RunID = "../run" }},
		{name: "run with invalid ref punctuation", mutate: func(req *publisher.Request) { req.RunID = "run.123" }},
		{name: "run too long", mutate: func(req *publisher.Request) { req.RunID = strings.Repeat("r", 129) }},
		{name: "missing repository", mutate: func(req *publisher.Request) { req.Repository = "" }},
		{name: "invalid base SHA", mutate: func(req *publisher.Request) { req.BaseSHA = strings.Repeat("a", 39) }},
		{name: "missing target", mutate: func(req *publisher.Request) { req.TargetRef = "" }},
		{name: "wrong branch", mutate: func(req *publisher.Request) { req.Branch = "other/run-123" }},
		{name: "artifact run mismatch", mutate: func(req *publisher.Request) { req.Artifact.RunID = "run-other" }},
		{name: "invalid artifact digest", mutate: func(req *publisher.Request) { req.Artifact.Digest = strings.Repeat("b", 63) }},
		{name: "invalid artifact size", mutate: func(req *publisher.Request) { req.Artifact.Size = 0 }},
		{name: "missing title", mutate: func(req *publisher.Request) { req.Title = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest()
			tt.mutate(&req)
			if err := req.Validate(); !errors.Is(err, publisher.ErrInvalidRequest) {
				t.Fatalf("Request.Validate() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestPublisherResultValidation(t *testing.T) {
	t.Parallel()

	req := validRequest()
	if err := validResult(req).Validate(req); err != nil {
		t.Fatalf("valid Result.Validate() error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*publisher.Result)
	}{
		{name: "missing provider", mutate: func(result *publisher.Result) { result.Provider = "" }},
		{name: "wrong branch", mutate: func(result *publisher.Result) { result.Branch = "other/run-123" }},
		{name: "invalid commit SHA", mutate: func(result *publisher.Result) { result.CommitSHA = strings.Repeat("c", 39) }},
		{name: "missing pull request ID", mutate: func(result *publisher.Result) { result.PullRequestID = "" }},
		{name: "HTTP pull request URL", mutate: func(result *publisher.Result) { result.PullRequestURL = "http://github.com/araihu/paje/pull/42" }},
		{name: "relative pull request URL", mutate: func(result *publisher.Result) { result.PullRequestURL = "/pull/42" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validResult(req)
			tt.mutate(&result)
			if err := result.Validate(req); !errors.Is(err, publisher.ErrInvalidRequest) {
				t.Fatalf("Result.Validate() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestPublisherSentinelErrorsAreDistinct(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		publisher.ErrInvalidRequest,
		publisher.ErrConflict,
		publisher.ErrProviderUnavailable,
	}
	for i, sentinel := range sentinels {
		if sentinel == nil {
			t.Fatalf("sentinel %d is nil", i)
		}
		for j, other := range sentinels {
			if i != j && errors.Is(sentinel, other) {
				t.Fatalf("sentinel %d aliases sentinel %d", i, j)
			}
		}
	}
}

func validRequest() publisher.Request {
	return publisher.Request{
		RunID:      "run-123",
		Repository: "github.com/araihu/paje",
		BaseSHA:    strings.Repeat("a", 40),
		TargetRef:  "main",
		Branch:     "paje/code-change/run-123",
		Artifact: artifact.Reference{
			RunID:  "run-123",
			Digest: strings.Repeat("b", 64),
			Size:   1024,
		},
		Title: "Update parser",
		Body:  "Automated change",
		Draft: true,
	}
}

func validResult(req publisher.Request) publisher.Result {
	return publisher.Result{
		Provider:       "github",
		Branch:         req.Branch,
		CommitSHA:      strings.Repeat("c", 40),
		PullRequestID:  "42",
		PullRequestURL: "https://github.com/araihu/paje/pull/42",
	}
}
