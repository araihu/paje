package hatchet

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalWorkflowDetailsInputNormalizesHatchetJSONStorage(t *testing.T) {
	raw := json.RawMessage(` { "run_id" : "paje_bound", "input" : { "z" : 2, "a" : 1 } } `)
	want := json.RawMessage(`{"input":{"a":1,"z":2},"run_id":"paje_bound"}`)

	got, err := canonicalWorkflowDetailsInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical input = %s, want %s", got, want)
	}
}

func TestCanonicalWorkflowDetailsInputRejectsInvalidProviderData(t *testing.T) {
	tests := map[string]json.RawMessage{
		"empty":     nil,
		"malformed": json.RawMessage(`{"run_id":`),
		"array":     json.RawMessage(`[]`),
		"oversized": json.RawMessage(`{"padding":"` + strings.Repeat("x", maxEnvelopeBytes) + `"}`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := canonicalWorkflowDetailsInput(raw); err == nil {
				t.Fatal("expected provider data rejection")
			}
		})
	}
}
