// Package leafgatewayconfig loads the credential-isolated leaf gateway configuration.
package leafgatewayconfig

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"
)

const maxTimeout = 5 * time.Minute

type Config struct {
	ListenAddress        string
	SubmissionRoot       string
	TokenPolicyFile      string
	HatchetProducerToken string
	ReadHeaderTimeout    time.Duration
	ReadTimeout          time.Duration
	WriteTimeout         time.Duration
	IdleTimeout          time.Duration
	ShutdownTimeout      time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("load leaf gateway config: environment reader is required")
	}
	cfg := Config{
		ListenAddress:        valueOrDefault(getenv("PAJE_LEAF_GATEWAY_LISTEN_ADDRESS"), "127.0.0.1:8787"),
		SubmissionRoot:       strings.TrimSpace(getenv("PAJE_LEAF_GATEWAY_SUBMISSION_ROOT")),
		TokenPolicyFile:      strings.TrimSpace(getenv("PAJE_LEAF_GATEWAY_TOKEN_POLICY_FILE")),
		HatchetProducerToken: strings.TrimSpace(getenv("PAJE_LEAF_GATEWAY_HATCHET_TOKEN")),
	}
	if cfg.HatchetProducerToken == "" {
		return Config{}, errors.New("load leaf gateway config: PAJE_LEAF_GATEWAY_HATCHET_TOKEN is required")
	}
	if err := validatePath("PAJE_LEAF_GATEWAY_SUBMISSION_ROOT", cfg.SubmissionRoot); err != nil {
		return Config{}, err
	}
	if err := validatePath("PAJE_LEAF_GATEWAY_TOKEN_POLICY_FILE", cfg.TokenPolicyFile); err != nil {
		return Config{}, err
	}
	if overlaps(cfg.SubmissionRoot, cfg.TokenPolicyFile) {
		return Config{}, errors.New("load leaf gateway config: submission root and token policy must not overlap")
	}
	if err := validateListen(cfg.ListenAddress); err != nil {
		return Config{}, err
	}

	var err error
	if cfg.ReadHeaderTimeout, err = duration(getenv, "PAJE_LEAF_GATEWAY_READ_HEADER_TIMEOUT", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.ReadTimeout, err = duration(getenv, "PAJE_LEAF_GATEWAY_READ_TIMEOUT", 15*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.WriteTimeout, err = duration(getenv, "PAJE_LEAF_GATEWAY_WRITE_TIMEOUT", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.IdleTimeout, err = duration(getenv, "PAJE_LEAF_GATEWAY_IDLE_TIMEOUT", 60*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = duration(getenv, "PAJE_LEAF_GATEWAY_SHUTDOWN_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func valueOrDefault(raw, fallback string) string {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	return strings.TrimSpace(raw)
}

func validatePath(name, value string) error {
	if value == "" {
		return fmt.Errorf("load leaf gateway config: %s is required", name)
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("load leaf gateway config: %s must be a clean absolute path", name)
	}
	return nil
}

func overlaps(first, second string) bool {
	for _, pair := range [][2]string{{first, second}, {second, first}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func validateListen(value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || port == "" {
		return errors.New("load leaf gateway config: listen address must include an explicit host and port")
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return errors.New("load leaf gateway config: listen address must include a valid port")
	}
	return nil
}

func duration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 || parsed > maxTimeout {
		return 0, fmt.Errorf("load leaf gateway config: %s must be positive and at most %s", name, maxTimeout)
	}
	return parsed, nil
}
