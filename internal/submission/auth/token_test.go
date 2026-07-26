package auth_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/submission"
	"github.com/araihu/paje/internal/submission/auth"
)

const clearToken = "paje_v1_codex01.ERERERERERERERERERERERERERERERERERERERERERE"

func TestAuthenticateReturnsExactScopedPrincipal(t *testing.T) {
	authenticator := loadTestPolicy(t, validCredential())

	principal, err := authenticator.Authenticate(clearToken)
	if err != nil {
		t.Fatal(err)
	}
	if principal.CredentialID != "codex01" ||
		principal.Subject != "codex@example.com" ||
		principal.UserID != "codex@example.com" ||
		principal.AppID != "service" || principal.MaxDepth != 0 {
		t.Fatalf("principal = %#v", principal)
	}
	if len(principal.Repositories) != 1 || principal.Repositories[0] != (submission.RepositoryScope{
		Host: "github.com", Owner: "example", Name: "service",
	}) {
		t.Fatalf("repositories = %#v", principal.Repositories)
	}
	if !principal.Actions[submission.ActionSubmitArtifact] ||
		!principal.Actions[submission.ActionRead] ||
		!principal.Actions[submission.ActionCancel] ||
		principal.Actions[submission.ActionSubmitPullRequest] {
		t.Fatalf("actions = %#v", principal.Actions)
	}
	if !principal.Harnesses["codex"] {
		t.Fatalf("harnesses = %#v", principal.Harnesses)
	}
	if err := authenticator.Authorize(principal, auth.ActionControlCreate); err != nil {
		t.Fatalf("authorize control:create: %v", err)
	}
	if err := authenticator.Authorize(principal, auth.ActionTaskCreate); err != nil {
		t.Fatalf("authorize task:create: %v", err)
	}
	if err := authenticator.Authorize(principal, auth.ActionSubmitPullRequest); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("authorize pull request error = %v", err)
	}
	if !authenticator.AllowsProject(principal, "service") {
		t.Fatal("service project is not allowed")
	}
	if !authenticator.AllowsCommunication(principal, "service", "docs") {
		t.Fatal("declared communication edge is not allowed")
	}
	if authenticator.AllowsCommunication(principal, "docs", "service") {
		t.Fatal("reverse communication edge is unexpectedly allowed")
	}

	principal.Repositories[0].Name = "mutated"
	principal.Actions[submission.ActionSubmitPullRequest] = true
	principal.Harnesses["other"] = true
	again, err := authenticator.Authenticate(clearToken)
	if err != nil {
		t.Fatal(err)
	}
	if again.Repositories[0].Name != "service" ||
		again.Actions[submission.ActionSubmitPullRequest] || again.Harnesses["other"] {
		t.Fatalf("authentication returned aliased principal: %#v", again)
	}
}

func TestAuthenticateRejectsEveryInvalidTokenAsUnauthenticated(t *testing.T) {
	credential := validCredential()
	authenticator := loadTestPolicy(t, credential)
	expired := credential
	expired.ID = "expired"
	expired.ExpiresAt = "2026-07-25T11:59:59Z"
	expiredAuthenticator := loadTestPolicy(t, expired)
	revoked := credential
	revoked.ID = "revoked"
	revoked.Revoked = true
	revokedAuthenticator := loadTestPolicy(t, revoked)

	tests := []struct {
		name          string
		authenticator *auth.Authenticator
		token         string
	}{
		{name: "empty", authenticator: authenticator, token: ""},
		{name: "wrong prefix", authenticator: authenticator, token: strings.TrimPrefix(clearToken, "paje_")},
		{name: "missing dot", authenticator: authenticator, token: strings.Replace(clearToken, ".", "", 1)},
		{name: "extra dot", authenticator: authenticator, token: clearToken + ".x"},
		{name: "unknown public id", authenticator: authenticator, token: strings.Replace(clearToken, "codex01", "unknown", 1)},
		{name: "wrong secret", authenticator: authenticator, token: strings.TrimSuffix(clearToken, "E") + "A"},
		{name: "short secret", authenticator: authenticator, token: "paje_v1_codex01.AQ"},
		{name: "padded secret", authenticator: authenticator, token: clearToken + "="},
		{name: "whitespace", authenticator: authenticator, token: clearToken + "\n"},
		{name: "expired", authenticator: expiredAuthenticator, token: strings.Replace(clearToken, "codex01", "expired", 1)},
		{name: "revoked", authenticator: revokedAuthenticator, token: strings.Replace(clearToken, "codex01", "revoked", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.authenticator.Authenticate(test.token)
			if !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatalf("error = %v", err)
			}
			message := err.Error()
			if test.token != "" && strings.Contains(message, test.token) ||
				strings.Contains(message, credential.SecretHash) ||
				strings.Contains(message, "codex01") {
				t.Fatalf("authentication error leaks credential material: %q", message)
			}
		})
	}
}

func TestAuthenticateRejectsPrincipalFromAnotherAuthenticator(t *testing.T) {
	first := loadTestPolicy(t, validCredential())
	secondCredential := validCredential()
	secondCredential.Subject = "other@example.com"
	second := loadTestPolicy(t, secondCredential)
	principal, err := first.Authenticate(clearToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Authorize(principal, auth.ActionControlCreate); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("authorize foreign principal error = %v", err)
	}
}

func TestLoadPolicyCanonicalizesGitHubRepositoryIdentity(t *testing.T) {
	credential := validCredential()
	credential.Repositories = []string{"https://github.com/Example/Service.git"}
	authenticator := loadTestPolicy(t, credential)
	principal, err := authenticator.Authenticate(clearToken)
	if err != nil {
		t.Fatal(err)
	}
	want := submission.RepositoryScope{Host: "github.com", Owner: "example", Name: "service"}
	if len(principal.Repositories) != 1 || principal.Repositories[0] != want {
		t.Fatalf("repository = %#v, want %#v", principal.Repositories, want)
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
}
