package sandboxinit

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
)

func TestDocumentRejectsShellAndEscapingPaths(t *testing.T) {
	document := validDocument(t)
	document.Command.Executable = "sh"
	if err := document.Validate(); err == nil {
		t.Fatal("shell accepted")
	}
	document = validDocument(t)
	document.Command.Directory = "/outside"
	if err := document.Validate(); err == nil {
		t.Fatal("outside directory accepted")
	}
	document = validDocument(t)
	document.WorkspaceRoot = "/different"
	if err := document.Validate(); err == nil {
		t.Fatal("non-fixed workspace root accepted")
	}
	document = validDocument(t)
	document.EnvironmentFiles["CODEX_TOKEN"] = "/workspace/token"
	if err := document.Validate(); err == nil {
		t.Fatal("environment materialization outside secret root accepted")
	}
	document = validDocument(t)
	document.Command.Directory = "/workspace/bad\x00directory"
	if err := document.Validate(); err == nil {
		t.Fatal("NUL command directory accepted")
	}
	document = validDocument(t)
	document.EnvironmentFiles["CODEX_TOKEN"] = "/run/paje/secrets/bad\x00path"
	if err := document.Validate(); err == nil {
		t.Fatal("NUL environment-file path accepted")
	}
}

func TestDocumentAppliesOneAggregateEnvironmentByteLimit(t *testing.T) {
	document := validDocument(t)
	document.Environment["BASELINE"] = strings.Repeat("a", (1<<19)-len("BASELINE"))
	document.Command.Environment["COMMAND"] = strings.Repeat("b", (1<<19)-len("COMMAND")+1)
	if err := document.Validate(); err == nil {
		t.Fatal("combined oversized environment accepted")
	}

	document = validDocument(t)
	delete(document.EnvironmentFiles, "CODEX_TOKEN")
	for index := range 500 {
		key := fmt.Sprintf("SECRET_%03d", index)
		document.EnvironmentFiles[key] = "/run/paje/secrets/" + strings.Repeat("x", 2100) + fmt.Sprintf("-%03d", index)
	}
	if err := document.Validate(); err == nil {
		t.Fatal("aggregate oversized environment-file declarations accepted")
	}
}

func TestChildEnvironmentAndListRejectAggregateOverflow(t *testing.T) {
	document := validDocument(t)
	document.Environment["BASELINE"] = strings.Repeat("a", (1<<19)-len("BASELINE"))
	document.Command.Environment["COMMAND"] = strings.Repeat("b", (1<<18)-len("COMMAND"))
	fileValues := map[string]string{"CODEX_TOKEN": strings.Repeat("c", (1<<18)-len("CODEX_TOKEN")+1)}
	if _, err := document.ChildEnvironment(fileValues); err == nil {
		t.Fatal("oversized merged child environment accepted")
	}

	values := map[string]string{
		"FIRST":  strings.Repeat("a", (1<<19)-len("FIRST")),
		"SECOND": strings.Repeat("b", (1<<19)-len("SECOND")+1),
	}
	if _, err := EnvironmentList(values); err == nil {
		t.Fatal("oversized environment list accepted")
	}
}

func TestDecodeIsStrictBoundedAndRejectsTrailingValues(t *testing.T) {
	encoded, err := json.Marshal(validDocument(t))
	if err != nil {
		t.Fatal(err)
	}
	valid := string(encoded)
	if _, err := Decode(strings.NewReader(valid)); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		strings.Replace(valid, `"workspace_root"`, `"unknown":true,"workspace_root"`, 1),
		valid + `{}`,
		strings.Repeat(" ", MaxDocumentBytes+1),
	} {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatal("invalid command document accepted")
		}
	}
}

func TestDocumentRejectsEnvironmentCollisionsAndInvalidFiles(t *testing.T) {
	document := validDocument(t)
	document.Command.Environment["PATH"] = "/other"
	if err := document.Validate(); err == nil {
		t.Fatal("command environment collision accepted")
	}
	document = validDocument(t)
	document.EnvironmentFiles["PATH"] = "/run/paje/secrets/path"
	if err := document.Validate(); err == nil {
		t.Fatal("environment-file collision accepted")
	}
	document = validDocument(t)
	document.EnvironmentFiles["BAD=KEY"] = "/run/paje/secrets/token"
	if err := document.Validate(); err == nil {
		t.Fatal("invalid environment-file key accepted")
	}
}

func TestDocumentRequiresCanonicalPathAndExactChildStartBinding(t *testing.T) {
	document := validDocument(t)
	document.Environment["PATH"] = executor.CanonicalSandboxPATH
	attempt := executor.AttemptID{
		RunID: "run-receipt", Stage: "execute", Attempt: 2,
		StartedAt: time.Unix(100, 7).UTC(), Purpose: executor.PurposeAgent,
	}
	if err := document.BindChildStartReceipt(attempt, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("bound document rejected: %v", err)
	}

	rebound := document
	rebound.Command = document.Command.Clone()
	rebound.Command.Args[0] = "different"
	if err := rebound.Validate(); err == nil {
		t.Fatal("document accepted a child-start receipt rebound to another command")
	}

	noncanonical := document
	noncanonical.Environment = map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin"}
	if err := noncanonical.BindChildStartReceipt(attempt, strings.Repeat("b", 64)); err == nil {
		t.Fatal("document accepted a noncanonical sandbox PATH")
	}
}

func TestDocumentRoundTripPreservesEmptyEnvironmentReceiptBinding(t *testing.T) {
	document := Document{
		WorkspaceRoot: executor.SandboxWorkspaceRoot,
		Command: executor.Command{
			Executable:  "go",
			Args:        []string{"test", "./..."},
			Directory:   executor.SandboxWorkspaceRoot,
			Environment: map[string]string{},
		},
		Environment:      map[string]string{"PATH": executor.CanonicalSandboxPATH},
		EnvironmentFiles: map[string]string{},
	}
	attempt := executor.AttemptID{
		RunID: "run-empty-environment", Stage: "execute", Attempt: 1,
		StartedAt: time.Unix(200, 1).UTC(), Purpose: executor.PurposeVerification,
	}
	if err := document.BindChildStartReceipt(attempt, strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(strings.NewReader(string(encoded))); err != nil {
		t.Fatalf("empty environment declaration failed JSON round trip: %v", err)
	}

	for name, mutate := range map[string]func(*Document){
		"command value": func(document *Document) {
			document.Command.Environment = map[string]string{"GOWORK": "off"}
		},
		"environment file path": func(document *Document) {
			document.EnvironmentFiles = map[string]string{
				"WORKLOAD_TOKEN": "/run/paje/secrets/environment/token",
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rebound := document
			mutate(&rebound)
			encoded, err := json.Marshal(rebound)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Decode(strings.NewReader(string(encoded))); err == nil {
				t.Fatal("document accepted nonempty environment declaration drift")
			}
		})
	}
}

func validDocument(t *testing.T) Document {
	t.Helper()
	document := Document{
		WorkspaceRoot: executor.SandboxWorkspaceRoot,
		Command: executor.Command{
			Executable: "codex", Args: []string{"exec", "$(touch /tmp/pwn)"},
			Directory:   executor.SandboxWorkspaceRoot,
			Environment: map[string]string{"GOWORK": "off"},
		},
		Environment: map[string]string{"PATH": executor.CanonicalSandboxPATH},
		EnvironmentFiles: map[string]string{
			"CODEX_TOKEN": "/run/paje/secrets/codex-token",
		},
	}
	attempt := executor.AttemptID{
		RunID: "run-protocol", Stage: "execute", Attempt: 1,
		StartedAt: time.Unix(100, 1).UTC(), Purpose: executor.PurposeAgent,
	}
	if err := document.BindChildStartReceipt(attempt, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	return document
}
