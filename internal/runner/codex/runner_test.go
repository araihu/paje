package codex_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/runner/codex"
)

func TestRunPassesDeterministicCodexArgumentsAndReturnsLastAgentMessage(t *testing.T) {
	t.Parallel()

	executor, err := codex.New(os.Args[0])
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	workspace := t.TempDir()
	result, err := executor.Run(context.Background(), runner.RunRequest{
		TaskDescription: "complete the task",
		WorkspacePath:   workspace,
		Env: map[string]string{
			"GO_WANT_CODEX_HELPER": "1",
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	encodedArgs, err := os.ReadFile(filepath.Join(workspace, "args.json"))
	if err != nil {
		t.Fatalf("read captured arguments: %v", err)
	}
	var gotArgs []string
	if err := json.Unmarshal(encodedArgs, &gotArgs); err != nil {
		t.Fatalf("decode captured arguments: %v", err)
	}
	wantArgs := []string{
		"exec",
		"--json",
		"--ephemeral",
		"--ignore-user-config",
		"--sandbox",
		"workspace-write",
		"complete the task",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("arguments = %#v, want %#v", gotArgs, wantArgs)
	}
	if result.Output != "second completed message" {
		t.Errorf("Output = %q, want last agent message", result.Output)
	}
	for _, want := range []string{"thread.started", "first completed message", "second completed message"} {
		if !strings.Contains(result.Transcript, want) {
			t.Errorf("Transcript = %q, want it to contain %q", result.Transcript, want)
		}
	}
}

func TestRunRejectsSuccessfulStreamWithoutAgentMessage(t *testing.T) {
	t.Parallel()

	executor, err := codex.New(os.Args[0])
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	result, err := executor.Run(context.Background(), runner.RunRequest{
		TaskDescription: "no-agent-message",
		WorkspacePath:   t.TempDir(),
		Env:             map[string]string{"GO_WANT_CODEX_HELPER": "1"},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want missing agent message error")
	}
	if !strings.Contains(result.Transcript, "thread.started") {
		t.Errorf("Transcript = %q, want original JSONL", result.Transcript)
	}
}

func TestRunIgnoresNonJSONDiagnosticsAroundCodexEvents(t *testing.T) {
	t.Parallel()

	executor, err := codex.New(os.Args[0])
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	result, err := executor.Run(context.Background(), runner.RunRequest{
		TaskDescription: "diagnostic-before-json",
		WorkspacePath:   t.TempDir(),
		Env:             map[string]string{"GO_WANT_CODEX_HELPER": "1"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "completed after diagnostic" {
		t.Errorf("Output = %q, want agent message", result.Output)
	}
	if !strings.Contains(result.Transcript, "Reading additional input from stdin") {
		t.Errorf("Transcript = %q, want raw diagnostic preserved", result.Transcript)
	}
}

func TestRunPreservesNonzeroCodexDiagnostic(t *testing.T) {
	t.Parallel()

	executor, err := codex.New(os.Args[0])
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	result, err := executor.Run(context.Background(), runner.RunRequest{
		TaskDescription: "exit-9",
		WorkspacePath:   t.TempDir(),
		Env:             map[string]string{"GO_WANT_CODEX_HELPER": "1"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for Codex process exit", err)
	}
	if result.ExitCode != 9 {
		t.Errorf("ExitCode = %d, want 9", result.ExitCode)
	}
	if !strings.Contains(result.Output, "codex: refused to execute") {
		t.Errorf("Output = %q, want raw Codex diagnostic", result.Output)
	}
	if result.Output != result.Transcript {
		t.Errorf("nonzero result output = %q, want raw transcript %q", result.Output, result.Transcript)
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CODEX_HELPER") != "1" {
		return
	}

	task := os.Args[len(os.Args)-1]
	if task == "complete the task" {
		for index, arg := range os.Args {
			if arg != "exec" {
				continue
			}
			encodedArgs, err := json.Marshal(os.Args[index:])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			if err := os.WriteFile("args.json", encodedArgs, 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			break
		}
	}

	switch task {
	case "complete the task":
		fmt.Println(`{"type":"thread.started","thread_id":"thread-1"}`)
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"first completed message"}}`)
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"second completed message"}}`)
	case "no-agent-message":
		fmt.Println(`{"type":"thread.started","thread_id":"thread-1"}`)
		fmt.Println(`{"type":"item.completed","item":{"type":"reasoning","text":"internal"}}`)
	case "diagnostic-before-json":
		fmt.Fprintln(os.Stderr, "Reading additional input from stdin...")
		fmt.Println(`{"type":"thread.started","thread_id":"thread-1"}`)
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"completed after diagnostic"}}`)
	case "exit-9":
		fmt.Fprintln(os.Stderr, "codex: refused to execute")
		os.Exit(9)
	}
	os.Exit(0)
}
