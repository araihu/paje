package agentclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/araihu/paje/internal/agentclient"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

const clientTestToken = "paje_v1_codex01.ERERERERERERERERERERERERERERERERERERERERERE"

func TestSubmitSendsNarrowTokenOnlyInAuthorizationAndDerivesStableIdentity(t *testing.T) {
	var authorization, idempotency string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		idempotency = request.Header.Get("Idempotency-Key")
		gotBody = readAll(t, request)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"api_version":"v1","run_id":"paje_run","status":"accepted","reused":false,"depth":0,"root_run_id":"paje_run"}`))
	}))
	defer server.Close()

	client := newClient(t, server)
	view, err := client.Submit(context.Background(), "thread-1", validInput())
	if err != nil {
		t.Fatal(err)
	}
	if view.RunID != "paje_run" || view.Status != "accepted" {
		t.Fatalf("view = %#v", view)
	}
	if authorization != "Bearer "+clientTestToken || len(idempotency) != 64 {
		t.Fatalf("authorization/idempotency = %q / %q", authorization, idempotency)
	}
	if bytes.Contains(gotBody, []byte(clientTestToken)) {
		t.Fatal("request body contains bearer token")
	}
	var envelope struct {
		Origin struct {
			Harness   string `json:"harness"`
			SessionID string `json:"session_id"`
			TurnID    string `json:"turn_id"`
		} `json:"origin"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(gotBody, &envelope); err != nil {
		t.Fatal(err)
	}
	input, err := templatecodechange.Decode(envelope.Input)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Origin.Harness != "codex" || envelope.Origin.SessionID != "thread-1" ||
		envelope.Origin.TurnID != idempotency[:32] || input.IdempotencyKey != "" {
		t.Fatalf("envelope identity = %#v input key = %q", envelope.Origin, input.IdempotencyKey)
	}

	second, err := client.Submit(context.Background(), "thread-1", validInput())
	if err != nil || second.RunID != view.RunID {
		t.Fatalf("stable retry = %#v, %v", second, err)
	}
}

func TestSubmitRejectsPullRequestModeBeforeNetwork(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	client := newClient(t, server)
	input := bytes.Replace(validInput(), []byte(`"mode":"artifact"`), []byte(`"mode":"pull_request","provider":"github","target_branch":"main"`), 1)
	if _, err := client.Submit(context.Background(), "thread-1", input); err == nil {
		t.Fatal("pull-request mode accepted")
	}
	if called {
		t.Fatal("network called for rejected publication mode")
	}
}

func TestWaitHonorsRetryAfterAndNeverResubmits(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/v1/submissions/paje_run" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			writer.Header().Set("Retry-After", "2")
			_, _ = writer.Write([]byte(`{"api_version":"v1","run_id":"paje_run","status":"running","depth":0,"root_run_id":"paje_run"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"api_version":"v1","run_id":"paje_run","status":"succeeded","depth":0,"root_run_id":"paje_run","result":{"run_id":"paje_run","status":"succeeded"}}`))
	}))
	defer server.Close()
	var slept []time.Duration
	client, err := agentclient.New(agentclient.Config{
		BaseURL: server.URL, Token: clientTestToken, HTTPClient: server.Client(),
		Sleep: func(_ context.Context, duration time.Duration) error {
			slept = append(slept, duration)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := client.Wait(context.Background(), "paje_run")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "succeeded" || requests != 2 || len(slept) != 1 || slept[0] != 2*time.Second {
		t.Fatalf("view/requests/sleep = %#v / %d / %#v", view, requests, slept)
	}
}

func TestWaitRetriesTransientProviderUnavailableWithoutResubmitting(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/v1/submissions/paje_run" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			writer.Header().Set("Retry-After", "3")
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(`{"error":{"code":"provider_unavailable","message":"a required provider is unavailable"}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"api_version":"v1","run_id":"paje_run","status":"succeeded","depth":0,"root_run_id":"paje_run","result":{"run_id":"paje_run","status":"succeeded"}}`))
	}))
	defer server.Close()
	var slept []time.Duration
	client, err := agentclient.New(agentclient.Config{
		BaseURL: server.URL, Token: clientTestToken, HTTPClient: server.Client(),
		Sleep: func(_ context.Context, duration time.Duration) error {
			slept = append(slept, duration)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := client.Wait(context.Background(), "paje_run")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "succeeded" || requests != 2 || len(slept) != 1 || slept[0] != 3*time.Second {
		t.Fatalf("view/requests/sleep = %#v / %d / %#v", view, requests, slept)
	}
}

func TestStatusRejectsMismatchedTerminalResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"api_version":"v1","run_id":"paje_run","status":"succeeded","depth":0,"root_run_id":"paje_run","result":{"run_id":"other","status":"succeeded"}}`))
	}))
	defer server.Close()
	if _, err := newClient(t, server).Status(context.Background(), "paje_run"); err == nil {
		t.Fatal("mismatched terminal result accepted")
	}
}

func newClient(t *testing.T, server *httptest.Server) *agentclient.Client {
	t.Helper()
	client, err := agentclient.New(agentclient.Config{BaseURL: server.URL, Token: clientTestToken, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func validInput() json.RawMessage {
	return json.RawMessage(`{"task_description":"update docs","repository_uri":"https://github.com/araihu/paje.git","base_ref":"main","tags":{"user_id":"guilhermecastro","app_id":"paje"},"worker_profile":"codex-go@1","profile":"go","publication":{"mode":"artifact"}}`)
}

func readAll(t *testing.T, request *http.Request) []byte {
	t.Helper()
	contents, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}
