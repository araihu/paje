package secret

import (
	"testing"

	"github.com/araihu/paje/internal/workerprofile"
)

func TestBindingAuthorizesExactRequirementBindingRevision(t *testing.T) {
	ref := BindingRef{Capability: "harness.codex-auth", Revision: 7}
	binding, err := NewBinding(ref, Authorization{
		ProfileID: workerprofile.ProfileID{Name: "codex-go", Revision: 1},
		Stage:     workerprofile.StageAgent, Delivery: workerprofile.DeliveryDirectory,
		Target: "/run/paje/secrets/codex",
	}, "filesystem", "/etc/paje/secrets/codex")
	if err != nil {
		t.Fatal(err)
	}
	request := ResolveRequest{
		ProfileID: workerprofile.ProfileID{Name: "codex-go", Revision: 1},
		Ref:       ref,
		Requirement: workerprofile.SecretRequirement{
			Capability: "harness.codex-auth", BindingRevision: 7,
			Stage: workerprofile.StageAgent, Delivery: workerprofile.DeliveryDirectory,
			Target: "/run/paje/secrets/codex", Required: true,
		},
	}
	if !binding.Authorizes(request) {
		t.Fatal("exact requirement binding revision was rejected")
	}
	request.Requirement.BindingRevision = 8
	if binding.Authorizes(request) {
		t.Fatal("mismatched requirement binding revision was authorized")
	}
}
