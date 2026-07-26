package auth_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/araihu/paje/internal/submission/auth"
)

type policyCredential struct {
	ID                 string                     `json:"id"`
	SecretHash         string                     `json:"secret_hash"`
	Subject            string                     `json:"subject"`
	UserID             string                     `json:"user_id"`
	AppID              string                     `json:"app_id"`
	Repositories       []string                   `json:"repositories"`
	Actions            []string                   `json:"actions"`
	Harnesses          []string                   `json:"harnesses"`
	Projects           []string                   `json:"projects,omitempty"`
	CommunicationEdges []map[string]string        `json:"communication_edges,omitempty"`
	MaxDepth           int                        `json:"max_depth"`
	ExpiresAt          string                     `json:"expires_at,omitempty"`
	Revoked            bool                       `json:"revoked,omitempty"`
	Extra              map[string]json.RawMessage `json:"-"`
}

func validCredential() policyCredential {
	encodedSecret := strings.TrimPrefix(clearToken, "paje_v1_codex01.")
	secret, err := base64.RawURLEncoding.DecodeString(encodedSecret)
	if err != nil || len(secret) != 32 {
		panic("invalid fixed token fixture")
	}
	sum := sha256.Sum256(secret)
	return policyCredential{
		ID:           "codex01",
		SecretHash:   hex.EncodeToString(sum[:]),
		Subject:      "codex@example.com",
		UserID:       "codex@example.com",
		AppID:        "service",
		Repositories: []string{"https://github.com/example/service.git"},
		Actions: []string{
			"submit:artifact", "read", "cancel",
			"control:create", "task:create", "work:dispatch",
			"work:observe", "work:send", "work:wait",
			"work:interrupt", "work:close", "evidence:write", "control:close",
		},
		Harnesses: []string{"codex"},
		Projects:  []string{"service", "docs"},
		CommunicationEdges: []map[string]string{{
			"from": "service", "to": "docs",
		}},
		MaxDepth:  0,
		ExpiresAt: "2027-01-01T00:00:00Z",
	}
}

func TestLoadPolicyRejectsInvalidCredentialsAndUnknownFields(t *testing.T) {
	valid := validCredential()
	tests := []struct {
		name        string
		credentials []policyCredential
		mutateRaw   func([]byte) []byte
	}{
		{name: "duplicate ids", credentials: []policyCredential{valid, valid}},
		{name: "duplicate hashes", credentials: []policyCredential{valid, func() policyCredential { c := valid; c.ID = "other"; return c }()}},
		{name: "invalid id", credentials: []policyCredential{func() policyCredential { c := valid; c.ID = "Upper"; return c }()}},
		{name: "invalid hash", credentials: []policyCredential{func() policyCredential { c := valid; c.SecretHash = strings.Repeat("z", 64); return c }()}},
		{name: "invalid repository", credentials: []policyCredential{func() policyCredential {
			c := valid
			c.Repositories = []string{"https://github.com/example/service/child.git"}
			return c
		}()}},
		{name: "repository host trailing dot", credentials: []policyCredential{func() policyCredential {
			c := valid
			c.Repositories = []string{"https://github.com./example/service.git"}
			return c
		}()}},
		{name: "repository host empty label", credentials: []policyCredential{func() policyCredential {
			c := valid
			c.Repositories = []string{"https://git..example.com/example/service.git"}
			return c
		}()}},
		{name: "repository credentials", credentials: []policyCredential{func() policyCredential {
			c := valid
			c.Repositories = []string{"https://token@github.com/example/service.git"}
			return c
		}()}},
		{name: "unknown action", credentials: []policyCredential{func() policyCredential { c := valid; c.Actions = append(c.Actions, "admin:all"); return c }()}},
		{name: "duplicate action", credentials: []policyCredential{func() policyCredential { c := valid; c.Actions = append(c.Actions, "read"); return c }()}},
		{name: "invalid harness", credentials: []policyCredential{func() policyCredential { c := valid; c.Harnesses = []string{"Codex"}; return c }()}},
		{name: "unknown project in edge", credentials: []policyCredential{func() policyCredential {
			c := valid
			c.CommunicationEdges = []map[string]string{{"from": "service", "to": "unknown"}}
			return c
		}()}},
		{name: "self communication", credentials: []policyCredential{func() policyCredential {
			c := valid
			c.CommunicationEdges = []map[string]string{{"from": "service", "to": "service"}}
			return c
		}()}},
		{name: "negative depth", credentials: []policyCredential{func() policyCredential { c := valid; c.MaxDepth = -1; return c }()}},
		{name: "depth above v1 maximum", credentials: []policyCredential{func() policyCredential { c := valid; c.MaxDepth = 2; return c }()}},
		{name: "malformed expiry", credentials: []policyCredential{func() policyCredential { c := valid; c.ExpiresAt = "tomorrow"; return c }()}},
		{name: "unknown credential field", credentials: []policyCredential{valid}, mutateRaw: func(raw []byte) []byte {
			return []byte(strings.Replace(string(raw), `"subject":`, `"unknown":true,"subject":`, 1))
		}},
		{name: "unknown policy field", credentials: []policyCredential{valid}, mutateRaw: func(raw []byte) []byte {
			return []byte(strings.Replace(string(raw), `"credentials":`, `"unknown":true,"credentials":`, 1))
		}},
		{name: "duplicate policy field", credentials: []policyCredential{valid}, mutateRaw: func(raw []byte) []byte {
			return []byte(strings.Replace(string(raw), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1))
		}},
		{name: "trailing json", credentials: []policyCredential{valid}, mutateRaw: func(raw []byte) []byte { return append(raw, []byte(` {}`)...) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writePolicy(t, test.credentials, 0o600, test.mutateRaw)
			_, err := auth.LoadPolicy(path, fixedNow)
			if err == nil {
				t.Fatal("LoadPolicy succeeded")
			}
			if strings.Contains(err.Error(), valid.SecretHash) || strings.Contains(err.Error(), clearToken) {
				t.Fatalf("policy error leaks secret material: %q", err)
			}
		})
	}
}

func TestLoadPolicyRejectsCaseVariantAliasesAndSemanticDuplicates(t *testing.T) {
	valid := validCredential()
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{name: "top-level alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"schema_version":1`, `"SCHEMA_VERSION":1`, 1)
		}},
		{name: "top-level semantic duplicate", mutate: func(raw string) string {
			return strings.Replace(raw, `"schema_version":1`, `"schema_version":1,"SCHEMA_VERSION":1`, 1)
		}},
		{name: "credential alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"secret_hash":`, `"SECRET_HASH":`, 1)
		}},
		{name: "credential semantic duplicate", mutate: func(raw string) string {
			return strings.Replace(raw, `"secret_hash":`, `"SECRET_HASH":"`+valid.SecretHash+`","secret_hash":`, 1)
		}},
		{name: "communication edge alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"from":"service"`, `"From":"service"`, 1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writePolicy(t, []policyCredential{valid}, 0o600, func(raw []byte) []byte {
				return []byte(test.mutate(string(raw)))
			})
			if _, err := auth.LoadPolicy(path, fixedNow); err == nil {
				t.Fatal("LoadPolicy accepted a case-variant JSON field")
			}
		})
	}
}

func TestLoadPolicyRequiresRegularNonsymlink0600File(t *testing.T) {
	valid := validCredential()

	t.Run("mode", func(t *testing.T) {
		path := writePolicy(t, []policyCredential{valid}, 0o640, nil)
		if _, err := auth.LoadPolicy(path, fixedNow); err == nil {
			t.Fatal("LoadPolicy accepted group-readable policy")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		path := writePolicy(t, []policyCredential{valid}, 0o600, nil)
		link := filepath.Join(t.TempDir(), "policy.json")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if _, err := auth.LoadPolicy(link, fixedNow); err == nil {
			t.Fatal("LoadPolicy accepted symlink")
		}
	})

	t.Run("directory", func(t *testing.T) {
		if _, err := auth.LoadPolicy(t.TempDir(), fixedNow); err == nil {
			t.Fatal("LoadPolicy accepted directory")
		}
	})

	t.Run("missing", func(t *testing.T) {
		if _, err := auth.LoadPolicy(filepath.Join(t.TempDir(), "missing"), fixedNow); err == nil {
			t.Fatal("LoadPolicy accepted missing file")
		}
	})
}

func TestLoadPolicyRequiresClockAndNonemptyPolicy(t *testing.T) {
	path := writePolicy(t, []policyCredential{validCredential()}, 0o600, nil)
	if _, err := auth.LoadPolicy(path, nil); err == nil {
		t.Fatal("LoadPolicy accepted nil clock")
	}
	empty := writePolicy(t, nil, 0o600, nil)
	if _, err := auth.LoadPolicy(empty, fixedNow); err == nil {
		t.Fatal("LoadPolicy accepted empty credentials")
	}
}

func loadTestPolicy(t *testing.T, credentials ...policyCredential) *auth.Authenticator {
	t.Helper()
	path := writePolicy(t, credentials, 0o600, nil)
	authenticator, err := auth.LoadPolicy(path, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	return authenticator
}

func writePolicy(t *testing.T, credentials []policyCredential, mode os.FileMode, mutate func([]byte) []byte) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"credentials":    credentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		raw = mutate(raw)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, raw, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuthorizationErrorsAreStableAndBounded(t *testing.T) {
	authenticator := loadTestPolicy(t, validCredential())
	principal, err := authenticator.Authenticate(clearToken)
	if err != nil {
		t.Fatal(err)
	}
	err = authenticator.Authorize(principal, "not-an-action")
	if !errors.Is(err, auth.ErrForbidden) || len(err.Error()) > 128 {
		t.Fatalf("authorization error = %v", err)
	}
}
