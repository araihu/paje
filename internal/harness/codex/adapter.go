// Package codex implements the exact Codex CLI worker harness protocol.
package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/harness"
)

const (
	ID               = "codex"
	SupportedVersion = "0.144.5"
	maxPromptBytes   = 1 << 20
)

type Adapter struct {
	version string
}

func New(version string) (*Adapter, error) {
	if version != SupportedVersion {
		return nil, errors.New("unsupported exact Codex harness version")
	}
	return &Adapter{version: version}, nil
}

func (*Adapter) ID() string              { return ID }
func (adapter *Adapter) Version() string { return adapter.version }

func (*Adapter) Probe() executor.Command {
	return executor.Command{
		Executable: "codex",
		Args:       []string{"--version"},
		Directory:  executor.SandboxWorkspaceRoot,
	}
}

func (*Adapter) AgentCommand(prompt string) (executor.Command, error) {
	if strings.TrimSpace(prompt) == "" || len(prompt) > maxPromptBytes || strings.IndexByte(prompt, 0) >= 0 {
		return executor.Command{}, errors.New("Codex agent prompt is invalid")
	}
	return executor.Command{
		Executable: "codex",
		Args: []string{
			"exec", "--json", "--ephemeral", "--ignore-user-config",
			"--sandbox", "workspace-write", prompt,
		},
		Directory: executor.SandboxWorkspaceRoot,
	}, nil
}

func (*Adapter) Parse(result executor.Result) (string, error) {
	if !result.Created || !result.Started || !result.Completed || result.ExitCode != 0 ||
		result.StdoutTruncated || result.StderrTruncated || result.SecretDetected {
		return "", errors.New("Codex execution result is incomplete or unsafe")
	}

	scanner := bufio.NewScanner(bytes.NewReader(result.Stdout))
	scanner.Buffer(make([]byte, 64<<10), maxPromptBytes)
	var lastMessage string
	foundMessage := false
	foundTurnCompleted := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return "", fmt.Errorf("parse Codex JSONL event: %w", err)
		}
		if event.Type == "item.completed" && event.Item.Type == "agent_message" {
			if strings.TrimSpace(event.Item.Text) == "" {
				return "", errors.New("Codex completed agent message is empty")
			}
			lastMessage = event.Item.Text
			foundMessage = true
		}
		if event.Type == "turn.completed" {
			foundTurnCompleted = true
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("parse Codex JSONL stream: %w", err)
	}
	if !foundMessage {
		return "", errors.New("successful Codex JSONL stream contained no completed agent message")
	}
	if !foundTurnCompleted {
		return "", errors.New("successful Codex JSONL stream contained no terminal turn completion")
	}
	return lastMessage, nil
}

func (*Adapter) AcceptsCapability(capability string) bool {
	return capability == "harness.codex-auth"
}

var _ harness.Adapter = (*Adapter)(nil)
