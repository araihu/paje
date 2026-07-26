package codex

import (
	"slices"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/executor"
)

func TestAgentCommandIsExact(t *testing.T) {
	adapter, err := New("0.144.5")
	if err != nil {
		t.Fatal(err)
	}
	command, err := adapter.AgentCommand("change the file")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--json", "--ephemeral", "--ignore-user-config", "--sandbox", "workspace-write", "change the file"}
	if command.Executable != "codex" || command.Directory != executor.SandboxWorkspaceRoot || !slices.Equal(command.Args, want) {
		t.Fatalf("command = %#v", command)
	}

	probe := adapter.Probe()
	if probe.Executable != "codex" || !slices.Equal(probe.Args, []string{"--version"}) || probe.Directory != executor.SandboxWorkspaceRoot {
		t.Fatalf("Probe() = %#v", probe)
	}
	if adapter.ID() != "codex" || adapter.Version() != "0.144.5" {
		t.Fatalf("identity = %s@%s", adapter.ID(), adapter.Version())
	}
}

func TestAgentCommandRejectsUnboundedOrMalformedPrompt(t *testing.T) {
	adapter, err := New("0.144.5")
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"", " \n\t", "bad\x00prompt", strings.Repeat("x", (1<<20)+1)} {
		if _, err := adapter.AgentCommand(prompt); err == nil {
			t.Fatalf("AgentCommand(%q) succeeded", prompt[:min(len(prompt), 16)])
		}
	}
	if _, err := New("latest"); err == nil {
		t.Fatal("non-exact Codex version accepted")
	}
}

func TestParseReturnsLastCompletedAgentMessage(t *testing.T) {
	adapter, err := New("0.144.5")
	if err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-1"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"final"}}`,
		`{"type":"turn.completed"}`,
	}, "\n")
	got, err := adapter.Parse(executor.Result{Created: true, Started: true, Completed: true, Stdout: []byte(transcript)})
	if err != nil || got != "final" {
		t.Fatalf("Parse() = %q, %v", got, err)
	}
}

func TestParseFailsClosedOnIncompleteOrMalformedResult(t *testing.T) {
	adapter, err := New("0.144.5")
	if err != nil {
		t.Fatal(err)
	}
	validJSON := []byte("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}\n{\"type\":\"turn.completed\"}")
	tests := []executor.Result{
		{Created: true, Started: true, Completed: false, Stdout: validJSON},
		{Created: true, Started: true, Completed: true, ExitCode: 9, Stdout: validJSON},
		{Created: true, Started: true, Completed: true, StdoutTruncated: true, Stdout: validJSON},
		{Created: true, Started: true, Completed: true, SecretDetected: true, Stdout: validJSON},
		{Created: true, Started: true, Completed: true, Stdout: []byte(`{"type":`)},
		{Created: true, Started: true, Completed: true, Stdout: []byte(`{"type":"thread.started"}`)},
		{Created: true, Started: true, Completed: true, Stdout: []byte("diagnostic\n" + string(validJSON))},
		{Created: true, Started: true, Completed: true, Stdout: []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`)},
		{Created: true, Started: true, Completed: true, Stdout: []byte(`{"type":"turn.completed"}`)},
	}
	for index, result := range tests {
		if _, err := adapter.Parse(result); err == nil {
			t.Fatalf("case %d succeeded", index)
		}
	}
}

func TestAcceptsOnlyCodexAuthCapability(t *testing.T) {
	adapter, err := New("0.144.5")
	if err != nil {
		t.Fatal(err)
	}
	if !adapter.AcceptsCapability("harness.codex-auth") || adapter.AcceptsCapability("harness.other") || adapter.AcceptsCapability("workload.token") {
		t.Fatal("Codex capability boundary is not exact")
	}
}
