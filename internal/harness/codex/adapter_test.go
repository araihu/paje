package codex

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/workerprofile"
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

func TestAgentEnvironmentBindsExactPersistedCodexAuthDirectory(t *testing.T) {
	adapter, err := New(SupportedVersion)
	if err != nil {
		t.Fatal(err)
	}
	requirements := []workerprofile.SecretRequirement{{
		Capability: "harness.codex-auth", BindingRevision: 7,
		Stage: workerprofile.StageAgent, Delivery: workerprofile.DeliveryDirectory,
		Target: "/run/paje/secrets/codex", Required: true,
	}, {
		Capability: "workload.api", BindingRevision: 9,
		Stage: workerprofile.StageAgent, Delivery: workerprofile.DeliveryEnvironment,
		Target: "WORKLOAD_TOKEN", Required: true,
	}}

	environment, err := adapter.AgentEnvironment(requirements)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"CODEX_HOME": "/run/paje/secrets/codex"}; !reflect.DeepEqual(environment, want) {
		t.Fatalf("AgentEnvironment() = %#v, want %#v", environment, want)
	}
}

func TestAgentEnvironmentRejectsMalformedUnsupportedDuplicateOrDriftedRequirements(t *testing.T) {
	adapter, err := New(SupportedVersion)
	if err != nil {
		t.Fatal(err)
	}
	exact := workerprofile.SecretRequirement{
		Capability: "harness.codex-auth", BindingRevision: 7,
		Stage: workerprofile.StageAgent, Delivery: workerprofile.DeliveryDirectory,
		Target: "/run/paje/secrets/codex", Required: true,
	}
	tests := map[string][]workerprofile.SecretRequirement{
		"missing binding":      {{Capability: exact.Capability, Stage: exact.Stage, Delivery: exact.Delivery, Target: exact.Target, Required: true}},
		"wrong stage":          {{Capability: exact.Capability, BindingRevision: 7, Stage: "verification", Delivery: exact.Delivery, Target: exact.Target, Required: true}},
		"wrong delivery":       {{Capability: exact.Capability, BindingRevision: 7, Stage: exact.Stage, Delivery: workerprofile.DeliveryFile, Target: exact.Target, Required: true}},
		"wrong target":         {{Capability: exact.Capability, BindingRevision: 7, Stage: exact.Stage, Delivery: exact.Delivery, Target: "/run/paje/secrets/other", Required: true}},
		"optional":             {{Capability: exact.Capability, BindingRevision: 7, Stage: exact.Stage, Delivery: exact.Delivery, Target: exact.Target}},
		"unsupported harness":  {{Capability: "harness.other", BindingRevision: 7, Stage: exact.Stage, Delivery: exact.Delivery, Target: exact.Target, Required: true}},
		"duplicate capability": {exact, exact},
		"malformed workload":   {exact, {Capability: "", BindingRevision: 9, Stage: workerprofile.StageAgent, Delivery: workerprofile.DeliveryEnvironment, Target: "WORKLOAD_TOKEN", Required: true}},
	}
	for name, requirements := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := adapter.AgentEnvironment(requirements); err == nil {
				t.Fatal("AgentEnvironment() error = nil")
			}
		})
	}
}
