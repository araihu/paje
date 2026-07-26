package agentharness_test

import (
	"context"
	"errors"
	"testing"

	"github.com/araihu/paje/internal/agentharness"
	"github.com/araihu/paje/internal/agentharness/mock"
)

func TestRegistryResolvesExactHarnessAndValidatesDiscoveryIdentity(t *testing.T) {
	t.Parallel()

	harness := mock.New(validCapabilities("codex"))
	registry, err := agentharness.NewRegistry(map[string]agentharness.AgentHarness{"codex": harness})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve("other"); !errors.Is(err, agentharness.ErrProviderUnavailable) {
		t.Fatalf("Resolve(other) error = %v, want ErrProviderUnavailable", err)
	}
	capabilities, err := registry.Capabilities(
		context.Background(), agentharness.Principal{ID: "principal"}, "codex", "persistent_session",
	)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.HarnessID != "codex" {
		t.Fatalf("Capabilities() harness = %q", capabilities.HarnessID)
	}

	mismatched := mock.New(validCapabilities("foreign"))
	registry, err = agentharness.NewRegistry(map[string]agentharness.AgentHarness{"codex": mismatched})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Capabilities(
		context.Background(), agentharness.Principal{ID: "principal"}, "codex", "persistent_session",
	); !errors.Is(err, agentharness.ErrInvalidCapabilities) {
		t.Fatalf("Capabilities(identity mismatch) error = %v, want ErrInvalidCapabilities", err)
	}
}

func validCapabilities(harnessID string) agentharness.CapabilitySnapshot {
	return agentharness.CapabilitySnapshot{
		HarnessID: harnessID,
		Primitives: map[agentharness.Primitive]agentharness.PrimitiveCapabilities{
			agentharness.PersistentSession: {
				Primitive: agentharness.PersistentSession,
				Capabilities: agentharness.CapabilitySet(
					agentharness.CapDispatch, agentharness.CapObserve, agentharness.CapWait,
					agentharness.CapRuntimeIdentity, agentharness.CapAcknowledge,
					agentharness.CapSend, agentharness.CapCallback, agentharness.CapCursor,
					agentharness.CapInterrupt, agentharness.CapArchive,
					agentharness.CapRestart, agentharness.CapIsolation, agentharness.CapIdempotency,
				),
				ConcurrencyLimit: 1,
			},
		},
	}
}
