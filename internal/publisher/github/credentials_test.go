package github

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialsCreatePrivateSecretFreeAskpassAndCleanupCanceled(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "publisher-runtime")
	credentials, err := NewCredentials(runtimeRoot, testToken)
	if err != nil {
		t.Fatalf("NewCredentials() error = %v", err)
	}
	environment, cleanup, err := credentials.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	helper := environment["GIT_ASKPASS"]
	if helper == "" || !filepath.IsAbs(helper) {
		t.Fatalf("GIT_ASKPASS = %q, want absolute helper", helper)
	}
	info, err := os.Lstat(helper)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 || !info.Mode().IsRegular() {
		t.Fatalf("helper mode = %v, want regular 0700", info.Mode())
	}
	contents, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), testToken) {
		t.Fatal("askpass helper embeds token")
	}
	if environment["PAJE_GIT_USERNAME"] != "x-access-token" ||
		environment["PAJE_GIT_PASSWORD"] != testToken ||
		environment["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("credential environment = %#v", environment)
	}
	for key, value := range environment {
		if key != "PAJE_GIT_PASSWORD" && strings.Contains(value, testToken) {
			t.Fatalf("credential %s leaked token in %q", key, value)
		}
	}

	username := runAskpass(t, helper, environment, "Username for 'https://github.com':")
	password := runAskpass(t, helper, environment, "Password for 'https://github.com':")
	if username != "x-access-token" || password != testToken {
		t.Fatalf("askpass outputs username=%q password=%q", username, password)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cleanup(canceled); err != nil {
		t.Fatalf("cleanup(canceled) error = %v", err)
	}
	if _, err := os.Lstat(helper); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper still exists after cleanup: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(helper)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session directory still exists after cleanup: %v", err)
	}
	if environment["PAJE_GIT_PASSWORD"] != "" {
		t.Fatal("cleanup retained password in returned environment")
	}
}

func TestCredentialSessionsAreIndependentAndIdempotentlyCleaned(t *testing.T) {
	credentials, err := NewCredentials(filepath.Join(t.TempDir(), "runtime"), testToken)
	if err != nil {
		t.Fatal(err)
	}
	first, cleanupFirst, err := credentials.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, cleanupSecond, err := credentials.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first["GIT_ASKPASS"] == second["GIT_ASKPASS"] ||
		filepath.Dir(first["GIT_ASKPASS"]) == filepath.Dir(second["GIT_ASKPASS"]) {
		t.Fatalf("sessions share helper paths: %#v %#v", first, second)
	}
	if err := cleanupFirst(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := cleanupFirst(context.Background()); err != nil {
		t.Fatalf("second cleanup of first session = %v", err)
	}
	if _, err := os.Stat(second["GIT_ASKPASS"]); err != nil {
		t.Fatalf("first cleanup affected second session: %v", err)
	}
	if err := cleanupSecond(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialsRejectUnsafeConfiguration(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		root  string
		token string
	}{
		{name: "missing root", token: testToken},
		{name: "missing token", root: filepath.Join(root, "runtime")},
		{name: "token newline", root: filepath.Join(root, "runtime"), token: "secret\ninjection"},
		{name: "symlink root", root: link, token: testToken},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := NewCredentials(test.root, test.token); err == nil {
				t.Fatalf("NewCredentials() = %#v, nil error", got)
			}
		})
	}
}

func runAskpass(t *testing.T, helper string, environment map[string]string, prompt string) string {
	t.Helper()
	command := exec.Command(helper, prompt)
	command.Env = []string{
		"PAJE_GIT_USERNAME=" + environment["PAJE_GIT_USERNAME"],
		"PAJE_GIT_PASSWORD=" + environment["PAJE_GIT_PASSWORD"],
	}
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}
