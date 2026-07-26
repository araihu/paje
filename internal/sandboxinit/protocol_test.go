package sandboxinit

import (
	"fmt"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/executor"
)

func TestDocumentRejectsShellAndEscapingPaths(t *testing.T) {
	document := validDocument()
	document.Command.Executable = "sh"
	if err := document.Validate(); err == nil {
		t.Fatal("shell accepted")
	}
	document = validDocument()
	document.Command.Directory = "/outside"
	if err := document.Validate(); err == nil {
		t.Fatal("outside directory accepted")
	}
	document = validDocument()
	document.WorkspaceRoot = "/different"
	if err := document.Validate(); err == nil {
		t.Fatal("non-fixed workspace root accepted")
	}
	document = validDocument()
	document.EnvironmentFiles["CODEX_TOKEN"] = "/workspace/token"
	if err := document.Validate(); err == nil {
		t.Fatal("environment materialization outside secret root accepted")
	}
	document = validDocument()
	document.Command.Directory = "/workspace/bad\x00directory"
	if err := document.Validate(); err == nil {
		t.Fatal("NUL command directory accepted")
	}
	document = validDocument()
	document.EnvironmentFiles["CODEX_TOKEN"] = "/run/paje/secrets/bad\x00path"
	if err := document.Validate(); err == nil {
		t.Fatal("NUL environment-file path accepted")
	}
}

func TestDocumentAppliesOneAggregateEnvironmentByteLimit(t *testing.T) {
	document := validDocument()
	document.Environment["BASELINE"] = strings.Repeat("a", (1<<19)-len("BASELINE"))
	document.Command.Environment["COMMAND"] = strings.Repeat("b", (1<<19)-len("COMMAND")+1)
	if err := document.Validate(); err == nil {
		t.Fatal("combined oversized environment accepted")
	}

	document = validDocument()
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
	document := validDocument()
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
	valid := `{"workspace_root":"/workspace","command":{"executable":"codex","args":["exec"],"directory":"/workspace"},"environment":{"PATH":"/usr/bin"}}`
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
	document := validDocument()
	document.Command.Environment["PATH"] = "/other"
	if err := document.Validate(); err == nil {
		t.Fatal("command environment collision accepted")
	}
	document = validDocument()
	document.EnvironmentFiles["PATH"] = "/run/paje/secrets/path"
	if err := document.Validate(); err == nil {
		t.Fatal("environment-file collision accepted")
	}
	document = validDocument()
	document.EnvironmentFiles["BAD=KEY"] = "/run/paje/secrets/token"
	if err := document.Validate(); err == nil {
		t.Fatal("invalid environment-file key accepted")
	}
}

func validDocument() Document {
	return Document{
		WorkspaceRoot: executor.SandboxWorkspaceRoot,
		Command: executor.Command{
			Executable: "codex", Args: []string{"exec", "$(touch /tmp/pwn)"},
			Directory:   executor.SandboxWorkspaceRoot,
			Environment: map[string]string{"GOWORK": "off"},
		},
		Environment: map[string]string{"PATH": "/usr/bin:/bin"},
		EnvironmentFiles: map[string]string{
			"CODEX_TOKEN": "/run/paje/secrets/codex-token",
		},
	}
}
