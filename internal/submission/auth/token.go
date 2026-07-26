// Package auth authenticates high-entropy, statically scoped Pajé credentials.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/araihu/paje/internal/submission"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
)

const tokenPrefix = "paje_v1_"

var publicIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type credential struct {
	principal          submission.Principal
	secretHash         [sha256.Size]byte
	actions            map[string]bool
	projects           map[string]bool
	communicationEdges map[string]bool
	expiresAt          *time.Time
	revoked            bool
}

// Authenticator is an immutable in-memory snapshot of a validated policy.
type Authenticator struct {
	credentials map[string]credential
	now         func() time.Time
	dummyHash   [sha256.Size]byte
}

// Authenticate validates one clear v1 token and returns an isolated principal
// clone. Every credential failure has the same stable public identity.
func (a *Authenticator) Authenticate(token string) (submission.Principal, error) {
	if a == nil || a.now == nil {
		return submission.Principal{}, ErrUnauthenticated
	}
	publicID, secret, ok := parseToken(token)
	if !ok {
		return submission.Principal{}, ErrUnauthenticated
	}
	suppliedHash := sha256.Sum256(secret)
	entry, found := a.credentials[publicID]
	wantHash := a.dummyHash
	if found {
		wantHash = entry.secretHash
	}
	matched := subtle.ConstantTimeCompare(suppliedHash[:], wantHash[:]) == 1
	if !found || !matched {
		return submission.Principal{}, ErrUnauthenticated
	}
	now := a.now()
	if now.IsZero() || entry.revoked || entry.expiresAt != nil && !now.Before(*entry.expiresAt) {
		return submission.Principal{}, ErrUnauthenticated
	}
	return clonePrincipal(entry.principal), nil
}

func parseToken(token string) (string, []byte, bool) {
	if !strings.HasPrefix(token, tokenPrefix) || strings.TrimSpace(token) != token {
		return "", nil, false
	}
	parts := strings.Split(strings.TrimPrefix(token, tokenPrefix), ".")
	if len(parts) != 2 || !publicIDPattern.MatchString(parts[0]) || parts[1] == "" {
		return "", nil, false
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(secret) != sha256.Size {
		return "", nil, false
	}
	return parts[0], secret, true
}

// Authorize checks an independently grantable action against the exact
// authenticated principal snapshot.
func (a *Authenticator) Authorize(principal submission.Principal, action string) error {
	entry, ok := a.entryFor(principal)
	if !ok || !entry.actions[action] {
		return ErrForbidden
	}
	return nil
}

// AllowsProject reports whether the exact project identity is in scope.
func (a *Authenticator) AllowsProject(principal submission.Principal, projectID string) bool {
	entry, ok := a.entryFor(principal)
	return ok && entry.projects[projectID]
}

// AllowsCommunication reports whether the directed cross-project edge is in
// scope. Same-project communication does not need an explicit cross-project
// grant.
func (a *Authenticator) AllowsCommunication(
	principal submission.Principal,
	fromProjectID, toProjectID string,
) bool {
	entry, ok := a.entryFor(principal)
	if !ok || fromProjectID == "" || toProjectID == "" {
		return false
	}
	if fromProjectID == toProjectID {
		return entry.projects[fromProjectID]
	}
	return entry.communicationEdges[edgeKey(fromProjectID, toProjectID)]
}

// Actions returns a sorted clone of all leaf and control actions in scope.
func (a *Authenticator) Actions(principal submission.Principal) []string {
	entry, ok := a.entryFor(principal)
	if !ok {
		return nil
	}
	return sortedKeys(entry.actions)
}

// Projects returns a sorted clone of the project identities in scope.
func (a *Authenticator) Projects(principal submission.Principal) []string {
	entry, ok := a.entryFor(principal)
	if !ok {
		return nil
	}
	return sortedKeys(entry.projects)
}

func (a *Authenticator) entryFor(principal submission.Principal) (credential, bool) {
	if a == nil {
		return credential{}, false
	}
	entry, ok := a.credentials[principal.CredentialID]
	if !ok || !samePrincipal(entry.principal, principal) {
		return credential{}, false
	}
	return entry, true
}

type principalContextKey struct{}

// ContextWithPrincipal binds one already authenticated principal to an HTTP
// request context shared by the combined adapters.
func ContextWithPrincipal(ctx context.Context, principal submission.Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, clonePrincipal(principal))
}

// PrincipalFromContext returns an isolated principal clone.
func PrincipalFromContext(ctx context.Context) (submission.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(submission.Principal)
	if !ok {
		return submission.Principal{}, false
	}
	return clonePrincipal(principal), true
}

func clonePrincipal(source submission.Principal) submission.Principal {
	result := source
	result.Repositories = append([]submission.RepositoryScope(nil), source.Repositories...)
	result.Actions = make(map[submission.Action]bool, len(source.Actions))
	for action, allowed := range source.Actions {
		result.Actions[action] = allowed
	}
	result.Harnesses = make(map[string]bool, len(source.Harnesses))
	for harness, allowed := range source.Harnesses {
		result.Harnesses[harness] = allowed
	}
	return result
}

func samePrincipal(first, second submission.Principal) bool {
	if first.CredentialID != second.CredentialID || first.Subject != second.Subject ||
		first.UserID != second.UserID || first.AppID != second.AppID || first.MaxDepth != second.MaxDepth ||
		len(first.Repositories) != len(second.Repositories) ||
		len(first.Actions) != len(second.Actions) || len(first.Harnesses) != len(second.Harnesses) {
		return false
	}
	for index := range first.Repositories {
		if first.Repositories[index] != second.Repositories[index] {
			return false
		}
	}
	for action, allowed := range first.Actions {
		if second.Actions[action] != allowed {
			return false
		}
	}
	for harness, allowed := range first.Harnesses {
		if second.Harnesses[harness] != allowed {
			return false
		}
	}
	return true
}

func edgeKey(from, to string) string { return from + "\x00" + to }
