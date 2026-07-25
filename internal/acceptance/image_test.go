package acceptance

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerRevisionBuildArgumentValidation(t *testing.T) {
	requireOptIn(t, "PAJE_DOCKER_ACCEPTANCE", "the Docker build argument acceptance test")
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("Docker build argument acceptance requires docker on PATH")
	}
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	tests := []struct {
		name    string
		commit  string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "unknown", commit: "unknown", wantErr: true},
		{name: "short", commit: strings.Repeat("a", 39), wantErr: true},
		{name: "uppercase", commit: strings.Repeat("A", 40), wantErr: true},
		{name: "non-hex", commit: strings.Repeat("g", 40), wantErr: true},
		{name: "full lowercase SHA", commit: strings.Repeat("a", 40)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"build", "--target", "revision", "--progress=plain"}
			if test.commit != "" {
				args = append(args, "--build-arg", "PAJE_COMMIT="+test.commit)
			}
			args = append(args, repositoryRoot)
			command := exec.Command("docker", args...)
			output, err := command.CombinedOutput()
			if test.wantErr && err == nil {
				t.Fatalf("docker build accepted PAJE_COMMIT=%q", test.commit)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("docker build rejected full lowercase SHA: %v: %s", err, output)
			}
		})
	}
}
