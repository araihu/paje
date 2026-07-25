package mock_test

import (
	"context"
	"errors"
	"testing"

	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/runner/mock"
)

func TestRunnerRecordsRequestsAndReturnsConfiguredResult(t *testing.T) {
	t.Parallel()

	wantResult := runner.ExecutionResult{Output: "done", ExitCode: 7, Duration: 1.25}
	wantErr := errors.New("configured failure")
	executor := mock.NewRunner(wantResult, wantErr)
	req := runner.RunRequest{
		TaskDescription: "perform task",
		WorkspacePath:   "/workspace",
		Env:             map[string]string{"TOKEN": "secret"},
	}

	gotResult, gotErr := executor.Run(context.Background(), req)
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("Run() error = %v, want %v", gotErr, wantErr)
	}
	if gotResult != wantResult {
		t.Fatalf("Run() result = %#v, want %#v", gotResult, wantResult)
	}

	req.Env["TOKEN"] = "changed"
	requests := executor.Requests()
	if len(requests) != 1 {
		t.Fatalf("Requests() returned %d requests, want 1", len(requests))
	}
	if requests[0].Env["TOKEN"] != "secret" {
		t.Errorf("recorded request was not copied: %#v", requests[0])
	}

	requests[0].Env["TOKEN"] = "mutated"
	if executor.Requests()[0].Env["TOKEN"] != "secret" {
		t.Error("Requests() exposed internal state")
	}
}
