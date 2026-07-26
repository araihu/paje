package secret

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/araihu/paje/internal/workerprofile"
)

var (
	ErrBindingNotFound     = errors.New("secret binding not found")
	ErrBindingUnauthorized = errors.New("secret binding is not authorized")
)

var (
	capabilityPartPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	providerNamePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	environmentKeyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
)

var reservedCapabilityNamespaces = map[string]struct{}{
	"paje": {}, "hatchet": {}, "mem0": {}, "submission": {},
	"publisher": {}, "git": {}, "ssh": {}, "registry": {}, "executor": {},
}

var reservedEnvironmentKeys = map[string]struct{}{
	"HOME": {}, "PATH": {}, "TMP": {}, "TEMP": {}, "TMPDIR": {},
	"PWD": {}, "OLDPWD": {}, "SHELL": {}, "USER": {}, "LOGNAME": {},
	"LANG": {}, "CODEX_HOME": {}, "GIT_ASKPASS": {}, "SSH_AUTH_SOCK": {},
}

type Authorization struct {
	ProfileID workerprofile.ProfileID
	Stage     string
	Delivery  string
	Target    string
}

type Binding struct {
	ref           BindingRef
	authorization Authorization
	provider      string
	reference     string
}

func NewBinding(ref BindingRef, authorization Authorization, provider, reference string) (Binding, error) {
	if !validCapability(ref.Capability) || ref.Revision == 0 {
		return Binding{}, errors.New("invalid secret binding identity")
	}
	parsed, err := workerprofile.ParseProfileID(authorization.ProfileID.String())
	if err != nil || parsed != authorization.ProfileID {
		return Binding{}, errors.New("invalid secret binding profile authorization")
	}
	if authorization.Stage != workerprofile.StageAgent ||
		!validDeliveryTarget(authorization.Delivery, authorization.Target) {
		return Binding{}, errors.New("invalid secret binding delivery authorization")
	}
	if !providerNamePattern.MatchString(provider) || !validSourceReference(provider, reference) {
		return Binding{}, errors.New("invalid secret binding source")
	}
	return Binding{ref: ref, authorization: authorization, provider: provider, reference: reference}, nil
}

func (binding Binding) Ref() BindingRef              { return binding.ref }
func (binding Binding) Authorization() Authorization { return binding.authorization }
func (binding Binding) Source() (provider, reference string) {
	return binding.provider, binding.reference
}
func (Binding) String() string { return "[secret binding]" }
func (Binding) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[secret binding]"))
}
func (Binding) MarshalJSON() ([]byte, error) { return nil, ErrSecretSerialization }
func (Binding) MarshalText() ([]byte, error) { return nil, ErrSecretSerialization }

func (binding Binding) Equal(other Binding) bool {
	return binding.ref == other.ref && binding.authorization == other.authorization &&
		binding.provider == other.provider && binding.reference == other.reference
}

type ResolveRequest struct {
	ProfileID   workerprofile.ProfileID
	Ref         BindingRef
	Requirement workerprofile.SecretRequirement
}

type Registry interface {
	Resolve(context.Context, ResolveRequest) (Binding, error)
}

func (binding Binding) Authorizes(request ResolveRequest) bool {
	return request.Ref == binding.ref && request.ProfileID == binding.authorization.ProfileID &&
		request.Requirement.Capability == binding.ref.Capability &&
		request.Requirement.BindingRevision == binding.ref.Revision && request.Requirement.Required &&
		request.Requirement.Stage == binding.authorization.Stage &&
		request.Requirement.Delivery == binding.authorization.Delivery &&
		request.Requirement.Target == binding.authorization.Target
}

func validCapability(capability string) bool {
	parts := strings.Split(capability, ".")
	if len(parts) < 2 {
		return false
	}
	if _, reserved := reservedCapabilityNamespaces[parts[0]]; reserved {
		return false
	}
	for _, part := range parts {
		if !capabilityPartPattern.MatchString(part) {
			return false
		}
	}
	return true
}

func validDeliveryTarget(delivery, target string) bool {
	switch delivery {
	case workerprofile.DeliveryFile, workerprofile.DeliveryDirectory:
		return target != "" && strings.HasPrefix(target, "/run/paje/secrets/") && path.Clean(target) == target
	case workerprofile.DeliveryEnvironment:
		if !environmentKeyPattern.MatchString(target) {
			return false
		}
		if _, reserved := reservedEnvironmentKeys[target]; reserved {
			return false
		}
		for _, prefix := range []string{
			"PAJE_", "HATCHET_", "MEM0_", "SUBMISSION_", "PUBLISHER_",
			"GIT_", "SSH_", "DOCKER_", "REGISTRY_", "EXECUTOR_",
		} {
			if strings.HasPrefix(target, prefix) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func ValidateEnvironmentTarget(target string) error {
	if !validDeliveryTarget(workerprofile.DeliveryEnvironment, target) {
		return errors.New("invalid secret environment target")
	}
	return nil
}

func validSourceReference(provider, reference string) bool {
	switch provider {
	case "filesystem":
		return reference != "" && strings.HasPrefix(reference, "/") && path.Clean(reference) == reference
	case "environment":
		return environmentKeyPattern.MatchString(reference)
	default:
		return false
	}
}

var _ json.Marshaler = Binding{}
