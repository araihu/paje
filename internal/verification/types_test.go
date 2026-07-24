package verification_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/araihu/paje/internal/verification"
)

func TestCompile(t *testing.T) {
	workspace := t.TempDir()
	got, err := verification.Compile(verification.CommandSpec{
		Name:       "go test",
		Directory:  "module",
		Executable: "go",
		Args:       []string{"test", "./..."},
		Timeout:    "2m",
		Required:   true,
	}, workspace, verification.Limits{MaxArguments: 2, MaxTimeout: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if got.Directory != filepath.Join(workspace, "module") {
		t.Fatalf("Directory = %q", got.Directory)
	}
	if got.Timeout != 2*time.Minute || got.Executable != "go" || len(got.Args) != 2 || !got.Required {
		t.Fatalf("Command = %#v", got)
	}
}

func TestCompileRejectsUnsafeOrInvalidSpec(t *testing.T) {
	workspace := t.TempDir()
	base := verification.CommandSpec{
		Name:       "go test",
		Directory:  ".",
		Executable: "go",
		Args:       []string{"test"},
		Timeout:    "1s",
	}

	tests := []struct {
		name   string
		spec   verification.CommandSpec
		limits verification.Limits
	}{
		{name: "empty executable", spec: withSpec(base, func(s *verification.CommandSpec) { s.Executable = "" })},
		{name: "shell fragment executable", spec: withSpec(base, func(s *verification.CommandSpec) { s.Executable = "go test ./..." })},
		{name: "absolute directory", spec: withSpec(base, func(s *verification.CommandSpec) { s.Directory = filepath.Join(workspace, "nested") })},
		{name: "directory escape", spec: withSpec(base, func(s *verification.CommandSpec) { s.Directory = "../outside" })},
		{name: "nul executable", spec: withSpec(base, func(s *verification.CommandSpec) { s.Executable = "go\x00" })},
		{name: "nul argument", spec: withSpec(base, func(s *verification.CommandSpec) { s.Args = []string{"test\x00"} })},
		{name: "bad timeout", spec: withSpec(base, func(s *verification.CommandSpec) { s.Timeout = "soon" })},
		{name: "minimum timeout", spec: withSpec(base, func(s *verification.CommandSpec) { s.Timeout = "999ms" })},
		{name: "maximum timeout", spec: withSpec(base, func(s *verification.CommandSpec) { s.Timeout = "6m" }), limits: verification.Limits{MaxTimeout: 5 * time.Minute}},
		{name: "too many arguments", spec: withSpec(base, func(s *verification.CommandSpec) { s.Args = []string{"test", "./..."} }), limits: verification.Limits{MaxArguments: 1, MaxTimeout: time.Minute}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := tt.limits
			if limits.MaxTimeout == 0 {
				limits = verification.Limits{MaxArguments: 8, MaxTimeout: 5 * time.Minute}
			}
			if _, err := verification.Compile(tt.spec, workspace, limits); err == nil {
				t.Fatal("Compile() error = nil")
			}
		})
	}
}

func withSpec(spec verification.CommandSpec, mutate func(*verification.CommandSpec)) verification.CommandSpec {
	mutate(&spec)
	return spec
}
