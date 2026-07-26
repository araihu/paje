package provider

import (
	"bytes"
	"context"
	"testing"
)

func TestEnvironmentReadsOnlyAllowlistedBoundedValues(t *testing.T) {
	environment, err := NewEnvironment(EnvironmentConfig{
		AllowedKeys: []string{"WORKLOAD_TOKEN"},
		MaxBytes:    32,
		Lookup: func(key string) (string, bool) {
			values := map[string]string{"WORKLOAD_TOKEN": "secret", "OTHER": "forbidden"}
			value, ok := values[key]
			return value, ok
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := environment.Read(context.Background(), "WORKLOAD_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload.Value(), []byte("secret")) {
		t.Fatalf("payload = %q", payload.Value())
	}
	value := payload.Value()
	value[0] = 'X'
	if bytes.Equal(payload.Value(), value) {
		t.Fatal("environment payload aliases caller bytes")
	}
	for _, key := range []string{"OTHER", "MISSING", "bad-key"} {
		if _, err := environment.Read(context.Background(), key); err == nil {
			t.Fatalf("Read(%q) succeeded", key)
		}
	}
}

func TestEnvironmentRejectsEmptyAndOversizedValues(t *testing.T) {
	for name, value := range map[string]string{"empty": "", "oversized": "12345"} {
		t.Run(name, func(t *testing.T) {
			environment, err := NewEnvironment(EnvironmentConfig{
				AllowedKeys: []string{"TOKEN"}, MaxBytes: 4,
				Lookup: func(string) (string, bool) { return value, true },
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := environment.Read(context.Background(), "TOKEN"); err == nil {
				t.Fatal("unsafe environment value accepted")
			}
		})
	}
}
