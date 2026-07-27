package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/araihu/paje/internal/agentclient"
	"github.com/araihu/paje/internal/leafgatewayconfig"
	submissionhatchet "github.com/araihu/paje/internal/submission/hatchet"
)

const gatewayTestToken = "paje_v1_codex01.ERERERERERERERERERERERERERERERERERERERERERE"

type gatewayFakeHatchet struct{}

func (gatewayFakeHatchet) Start(context.Context, string, map[string]any) (string, error) {
	return "11111111-1111-4111-8111-111111111111", nil
}

func (gatewayFakeHatchet) Details(context.Context, string) (submissionhatchet.Details, error) {
	return submissionhatchet.Details{}, nil
}

func (gatewayFakeHatchet) Cancel(context.Context, string) error { return nil }

func TestBuildHandlerExposesAuthenticatedLeafSubmissionOnly(t *testing.T) {
	root := t.TempDir()
	policy := filepath.Join(root, "policy.json")
	writeGatewayPolicy(t, policy)
	cfg := leafgatewayconfig.Config{
		SubmissionRoot: filepath.Join(root, "submissions"), TokenPolicyFile: policy,
	}
	handler, err := buildHandler(cfg, gatewayFakeHatchet{})
	if err != nil {
		t.Fatal(err)
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/submissions/missing", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	body := []byte(`{"template":{"name":"code-change","version":1},"origin":{"harness":"codex","session_id":"session-1","turn_id":"turn-1"},"input":{"task_description":"update docs","repository_uri":"https://github.com/araihu/paje.git","base_ref":"main","tags":{"user_id":"guilhermecastro","app_id":"araihu-paje"},"worker_profile":"codex-go@1","profile":"go","publication":{"mode":"artifact"}}}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/submissions", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+gatewayTestToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "0123456789abcdef0123456789abcdef")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d body = %s", response.Code, response.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || decoded["status"] != "accepted" {
		t.Fatalf("submit response = %s", response.Body.String())
	}

	control := httptest.NewRecorder()
	controlRequest := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	controlRequest.Header.Set("Authorization", "Bearer "+gatewayTestToken)
	handler.ServeHTTP(control, controlRequest)
	if control.Code != http.StatusNotFound {
		t.Fatalf("control status = %d, want 404 for leaf-only gateway", control.Code)
	}
}

func TestAgentClientSubmitsThroughLeafGateway(t *testing.T) {
	root := t.TempDir()
	policy := filepath.Join(root, "policy.json")
	writeGatewayPolicy(t, policy)
	handler, err := buildHandler(leafgatewayconfig.Config{
		SubmissionRoot: filepath.Join(root, "submissions"), TokenPolicyFile: policy,
	}, gatewayFakeHatchet{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := agentclient.New(agentclient.Config{
		BaseURL: server.URL, Token: gatewayTestToken, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"task_description":"Create only .paje-evaluation/codex-originated-leaf.txt containing exactly: codex-local-leaf-f12b7c2 followed by one newline. Do not modify any other file. Run the repository's standard Go verification and return an artifact only.","repository_uri":"https://github.com/araihu/paje.git","base_ref":"main","tags":{"user_id":"guilhermecastro","app_id":"araihu-paje"},"worker_profile":"codex-go@1","profile":"go","publication":{"mode":"artifact"}}`)
	view, err := client.Submit(context.Background(), "019fa443-94e7-72c2-9efa-f2a76c2b0ab5", raw)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "accepted" || view.RunID == "" {
		t.Fatalf("view = %#v", view)
	}
}

func TestRunHardensBeforeReadingConfiguration(t *testing.T) {
	hardened := false
	getenv := func(string) string {
		if !hardened {
			t.Fatal("configuration read before process hardening")
		}
		return ""
	}
	err := run(context.Background(), getenv, func() error {
		hardened = true
		return nil
	}, nil)
	if err == nil {
		t.Fatal("missing configuration accepted")
	}
}

func TestRequestTrackerRejectsNewWorkAndDrainsActiveHandler(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	tracker := newRequestTracker(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	done := make(chan struct{})
	go func() {
		tracker.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(done)
	}()
	<-started

	tracker.stopAccepting()
	rejected := httptest.NewRecorder()
	tracker.ServeHTTP(rejected, httptest.NewRequest(http.MethodGet, "/", nil))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("new request status = %d", rejected.Code)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := tracker.wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait while active = %v", err)
	}
	close(release)
	<-done
	if err := tracker.wait(context.Background()); err != nil {
		t.Fatalf("wait after release = %v", err)
	}
}

func writeGatewayPolicy(t *testing.T, path string) {
	t.Helper()
	encoded := gatewayTestToken[len("paje_v1_codex01."):]
	secret, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(secret)
	document := map[string]any{
		"schema_version": 1,
		"credentials": []map[string]any{{
			"id": "codex01", "secret_hash": hex.EncodeToString(sum[:]),
			"subject": "codex", "user_id": "guilhermecastro", "app_id": "araihu-paje",
			"repositories": []string{"https://github.com/araihu/paje.git"},
			"actions":      []string{"submit:artifact", "read", "cancel"},
			"harnesses":    []string{"codex"}, "max_depth": 0,
		}},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
