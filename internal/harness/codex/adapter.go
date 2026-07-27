// Package codex implements the exact Codex CLI worker harness protocol.
package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/harness"
	"github.com/araihu/paje/internal/workerprofile"
)

const (
	ID                  = "codex"
	SupportedVersion    = "0.144.5"
	maxPromptBytes      = 1 << 20
	codexAuthCapability = "harness.codex-auth"
	codexHomeTarget     = "/run/paje/secrets/codex"
)

var (
	capabilityPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]*(?:\.[a-z][a-z0-9-]*)+$`)
	environmentPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
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

// AgentEnvironment derives only non-secret child environment declarations
// from the exact persisted secret requirements. Secret bytes remain broker-
// owned and are never exposed through this adapter contract.
func (*Adapter) AgentEnvironment(requirements []workerprofile.SecretRequirement) (map[string]string, error) {
	environment := make(map[string]string)
	capabilities := make(map[string]struct{}, len(requirements))
	for index, requirement := range requirements {
		if !capabilityPattern.MatchString(requirement.Capability) || requirement.BindingRevision == 0 ||
			requirement.Stage != workerprofile.StageAgent || !requirement.Required ||
			!validRequirementTarget(requirement) {
			return nil, errors.New("Codex secret requirement is malformed or unsupported")
		}
		if _, duplicate := capabilities[requirement.Capability]; duplicate {
			return nil, errors.New("Codex secret requirement is duplicated")
		}
		capabilities[requirement.Capability] = struct{}{}
		for prior := range index {
			if requirementTargetsCollide(requirements[prior], requirement) {
				return nil, errors.New("Codex secret requirement targets collide")
			}
		}

		switch {
		case requirement.Capability == codexAuthCapability:
			if requirement.Delivery != workerprofile.DeliveryDirectory || requirement.Target != codexHomeTarget {
				return nil, errors.New("Codex auth requirement does not match exact directory binding")
			}
			environment["CODEX_HOME"] = requirement.Target
		case strings.HasPrefix(requirement.Capability, "harness."):
			return nil, errors.New("Codex harness capability is unsupported")
		case requirement.Delivery == workerprofile.DeliveryEnvironment &&
			(requirement.Target == "HOME" || requirement.Target == "PATH" ||
				requirement.Target == "TMPDIR" || requirement.Target == "CODEX_HOME"):
			return nil, errors.New("Codex secret environment target collides with managed environment")
		}
	}
	return environment, nil
}

func validRequirementTarget(requirement workerprofile.SecretRequirement) bool {
	switch requirement.Delivery {
	case workerprofile.DeliveryEnvironment:
		return environmentPattern.MatchString(requirement.Target)
	case workerprofile.DeliveryFile, workerprofile.DeliveryDirectory:
		return strings.HasPrefix(requirement.Target, "/run/paje/secrets/") &&
			path.Clean(requirement.Target) == requirement.Target
	default:
		return false
	}
}

func requirementTargetsCollide(left, right workerprofile.SecretRequirement) bool {
	if left.Delivery == workerprofile.DeliveryEnvironment || right.Delivery == workerprofile.DeliveryEnvironment {
		return left.Delivery == workerprofile.DeliveryEnvironment &&
			right.Delivery == workerprofile.DeliveryEnvironment && left.Target == right.Target
	}
	return pathWithin(left.Target, right.Target) || pathWithin(right.Target, left.Target)
}

func pathWithin(root, candidate string) bool {
	return candidate == root || strings.HasPrefix(candidate, strings.TrimSuffix(root, "/")+"/")
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
	return capability == codexAuthCapability
}

var _ harness.Adapter = (*Adapter)(nil)
