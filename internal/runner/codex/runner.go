// Package codex adapts the Codex CLI to Pajé's runner port.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/runner/local"
)

// Runner executes Codex with its deterministic non-interactive JSON protocol.
type Runner struct {
	delegate runner.Runner
}

var _ runner.Runner = (*Runner)(nil)

// New constructs a Codex runner using command as the Codex executable.
func New(command string) (*Runner, error) {
	delegate, err := local.New(
		command,
		"exec",
		"--json",
		"--ephemeral",
		"--ignore-user-config",
		"--sandbox",
		"workspace-write",
	)
	if err != nil {
		return nil, fmt.Errorf("create Codex runner: %w", err)
	}
	return &Runner{delegate: delegate}, nil
}

// Run executes Codex and returns its final completed agent message.
func (r *Runner) Run(ctx context.Context, req runner.RunRequest) (runner.ExecutionResult, error) {
	result, err := r.delegate.Run(ctx, req)
	if err != nil || result.ExitCode != 0 {
		return result, err
	}

	message, err := lastCompletedAgentMessage(result.Transcript)
	if err != nil {
		return result, err
	}
	result.Output = message
	return result, nil
}

func lastCompletedAgentMessage(transcript string) (string, error) {
	var lastMessage string
	found := false
	for _, line := range strings.Split(transcript, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return "", fmt.Errorf("run Codex agent: decode JSONL transcript: %w", err)
		}
		if event.Type == "item.completed" && event.Item.Type == "agent_message" {
			lastMessage = event.Item.Text
			found = true
		}
	}
	if !found {
		return "", fmt.Errorf("run Codex agent: successful JSONL stream contained no completed agent message")
	}
	return lastMessage, nil
}
