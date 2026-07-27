package leafgatewayconfig_test

import (
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/leafgatewayconfig"
)

func TestLoadDefaultsToBoundedLoopbackGateway(t *testing.T) {
	values := validValues()
	values["HATCHET_CLIENT_TOKEN"] = "worker-token-must-be-ignored"

	cfg, err := leafgatewayconfig.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != "127.0.0.1:8787" || cfg.SubmissionRoot != "/var/lib/paje-leaf/submissions" ||
		cfg.TokenPolicyFile != "/run/paje-leaf/policy.json" || cfg.HatchetProducerToken != "producer-token" {
		t.Fatalf("config = %#v", cfg)
	}
	if cfg.ReadHeaderTimeout != 5*time.Second || cfg.ReadTimeout != 15*time.Second ||
		cfg.WriteTimeout != 30*time.Second || cfg.IdleTimeout != 60*time.Second ||
		cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("timeouts = %#v", cfg)
	}
	if strings.Contains(cfg.HatchetProducerToken, "worker-token") {
		t.Fatal("worker credential entered leaf gateway config")
	}
}

func TestLoadRejectsMissingRequiredValues(t *testing.T) {
	for _, key := range []string{
		"PAJE_LEAF_GATEWAY_HATCHET_TOKEN",
		"PAJE_LEAF_GATEWAY_TOKEN_POLICY_FILE",
		"PAJE_LEAF_GATEWAY_SUBMISSION_ROOT",
	} {
		t.Run(key, func(t *testing.T) {
			values := validValues()
			delete(values, key)
			if _, err := leafgatewayconfig.Load(func(name string) string { return values[name] }); err == nil {
				t.Fatalf("missing %s accepted", key)
			}
		})
	}
}

func TestLoadRejectsUnsafePathsListenAndTimeouts(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "relative root", key: "PAJE_LEAF_GATEWAY_SUBMISSION_ROOT", value: "submissions"},
		{name: "relative policy", key: "PAJE_LEAF_GATEWAY_TOKEN_POLICY_FILE", value: "policy.json"},
		{name: "policy in root", key: "PAJE_LEAF_GATEWAY_TOKEN_POLICY_FILE", value: "/var/lib/paje-leaf/submissions/policy.json"},
		{name: "missing port", key: "PAJE_LEAF_GATEWAY_LISTEN_ADDRESS", value: "127.0.0.1"},
		{name: "invalid port", key: "PAJE_LEAF_GATEWAY_LISTEN_ADDRESS", value: "127.0.0.1:65536"},
		{name: "zero timeout", key: "PAJE_LEAF_GATEWAY_READ_TIMEOUT", value: "0s"},
		{name: "oversized timeout", key: "PAJE_LEAF_GATEWAY_IDLE_TIMEOUT", value: "10m"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validValues()
			values[test.key] = test.value
			if _, err := leafgatewayconfig.Load(func(name string) string { return values[name] }); err == nil {
				t.Fatalf("unsafe %s accepted", test.key)
			}
		})
	}
}

func validValues() map[string]string {
	return map[string]string{
		"PAJE_LEAF_GATEWAY_HATCHET_TOKEN":     "producer-token",
		"PAJE_LEAF_GATEWAY_TOKEN_POLICY_FILE": "/run/paje-leaf/policy.json",
		"PAJE_LEAF_GATEWAY_SUBMISSION_ROOT":   "/var/lib/paje-leaf/submissions",
	}
}
