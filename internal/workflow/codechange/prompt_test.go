package codechange

import (
	"errors"
	"testing"

	"github.com/araihu/paje/internal/memory"
)

func TestBuildPromptIsDeterministicAndSorted(t *testing.T) {
	want := `Task
Update the docs

Repository
Base SHA: 0123456789012345678901234567890123456789
Profile: go

Constraints
- Work only inside the current workspace.
- Do not publish, push, merge, tag, or read external credentials.
- Leave all intended file changes in the workspace.

Preflight
- alpha: first
- zeta: last

Relevant memory
- [memory-2] keep tests small
- [memory-1] preserve compatibility`

	got, err := buildPrompt(promptInput{
		Task:    "Update the docs",
		BaseSHA: "0123456789012345678901234567890123456789",
		Profile: "go",
		Facts:   map[string]string{"zeta": "last", "alpha": "first"},
		Memory: []memory.Memory{
			{ID: "memory-2", Content: "keep tests small"},
			{ID: "memory-1", Content: "preserve compatibility"},
		},
	}, len(want))
	if err != nil {
		t.Fatalf("buildPrompt() error = %v", err)
	}
	if got != want {
		t.Fatalf("buildPrompt() =\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildPromptRejectsOverflowBeforeExecution(t *testing.T) {
	_, err := buildPrompt(promptInput{
		Task:    "Update the docs",
		BaseSHA: "0123456789012345678901234567890123456789",
		Profile: "generic",
	}, 32)
	if !errors.Is(err, errPromptTooLarge) {
		t.Fatalf("buildPrompt() error = %v, want %v", err, errPromptTooLarge)
	}
}
