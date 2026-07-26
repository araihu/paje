package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/run"
	"github.com/araihu/paje/internal/submission"
	"github.com/araihu/paje/internal/submission/auth"
	"github.com/araihu/paje/internal/submission/httpapi"
	submissionmock "github.com/araihu/paje/internal/submission/mock"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

const testToken = "paje_v1_codex01.ERERERERERERERERERERERERERERERERERERERERERE"

type httpFixture struct {
	handler       http.Handler
	authenticator *auth.Authenticator
	service       *submission.Service
	trigger       *submissionmock.Trigger
	secondToken   string
}

type pairedInspectTrigger struct {
	*submissionmock.Trigger
	mu      sync.Mutex
	count   int
	release chan struct{}
}

func newPairedInspectTrigger() *pairedInspectTrigger {
	return &pairedInspectTrigger{
		Trigger: submissionmock.NewTrigger(),
		release: make(chan struct{}),
	}
}

func (t *pairedInspectTrigger) Inspect(
	ctx context.Context,
	reference submission.TriggerReference,
) (submission.TriggerState, error) {
	t.mu.Lock()
	t.count++
	if t.count == 2 {
		close(t.release)
	}
	release := t.release
	t.mu.Unlock()
	select {
	case <-ctx.Done():
		return submission.TriggerState{}, ctx.Err()
	case <-release:
		return t.Trigger.Inspect(ctx, reference)
	}
}

func TestRouteSubmissionLifecycleAndExactReuse(t *testing.T) {
	fixture := newHTTPFixture(t, nil, nil)
	key := strings.Repeat("a", 32)

	first := serve(t, fixture.handler, http.MethodPost, "/v1/submissions", validSubmissionBody("change timeout"), testToken, key)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	var accepted map[string]any
	decodeResponse(t, first, &accepted)
	runID, _ := accepted["run_id"].(string)
	if accepted["api_version"] != "v1" || runID == "" || accepted["reused"] != false || accepted["depth"] != float64(0) {
		t.Fatalf("accepted response = %#v", accepted)
	}
	if _, exists := accepted["result"]; exists {
		t.Fatalf("nonterminal response contains result: %#v", accepted)
	}

	reused := serve(t, fixture.handler, http.MethodPost, "/v1/submissions", validSubmissionBody("change timeout"), testToken, key)
	if reused.Code != http.StatusOK {
		t.Fatalf("reuse status = %d, body = %s", reused.Code, reused.Body.String())
	}
	var reusedBody map[string]any
	decodeResponse(t, reused, &reusedBody)
	if reusedBody["run_id"] != runID || reusedBody["reused"] != true {
		t.Fatalf("reuse response = %#v", reusedBody)
	}

	changed := serve(t, fixture.handler, http.MethodPost, "/v1/submissions", validSubmissionBody("changed request"), testToken, key)
	assertError(t, changed, http.StatusConflict, "idempotency_conflict")

	status := serve(t, fixture.handler, http.MethodGet, "/v1/submissions/"+runID, nil, testToken, "")
	if status.Code != http.StatusOK || status.Header().Get("Retry-After") == "" {
		t.Fatalf("inspect status = %d, retry-after = %q, body = %s", status.Code, status.Header().Get("Retry-After"), status.Body.String())
	}
	invalidCancel := serve(t, fixture.handler, http.MethodPost, "/v1/submissions/"+runID+"/cancel", []byte(`{}`), testToken, strings.Repeat("c", 16)+"\tinside")
	assertError(t, invalidCancel, http.StatusBadRequest, "invalid_request")

	canceled := serve(t, fixture.handler, http.MethodPost, "/v1/submissions/"+runID+"/cancel", []byte(`{}`), testToken, strings.Repeat("c", 32))
	if canceled.Code != http.StatusAccepted {
		t.Fatalf("cancel status = %d, body = %s", canceled.Code, canceled.Body.String())
	}
	repeated := serve(t, fixture.handler, http.MethodPost, "/v1/submissions/"+runID+"/cancel", []byte(`{}`), testToken, strings.Repeat("c", 32))
	if repeated.Code != http.StatusOK {
		t.Fatalf("repeat cancel status = %d, body = %s", repeated.Code, repeated.Body.String())
	}
}

func TestRouteConcurrentCancellationHasExactlyOneAcceptedResponse(t *testing.T) {
	registry, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		t.Fatal(err)
	}
	store := submissionmock.NewStore()
	trigger := newPairedInspectTrigger()
	authenticator, _ := testAuthenticator(t)
	newHandler := func() http.Handler {
		service, err := submission.New(submission.Dependencies{
			Templates: registry, Store: store, Trigger: trigger,
			Clock: fixedHTTPNow, SystemMaxDepth: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		handler, err := httpapi.New(httpapi.Dependencies{
			Service: service, Authenticator: authenticator,
		})
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	handlers := []http.Handler{newHandler(), newHandler()}
	accepted := serve(t, handlers[0], http.MethodPost, "/v1/submissions", validSubmissionBody("change timeout"), testToken, strings.Repeat("a", 32))
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, body = %s", accepted.Code, accepted.Body.String())
	}
	var body map[string]any
	decodeResponse(t, accepted, &body)
	runID := body["run_id"].(string)
	reference := submission.TriggerReference{Provider: "mock", ExternalRunID: "mock_" + runID}
	trigger.SetState(reference, submission.TriggerState{Status: submission.StatusRunning})

	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wait sync.WaitGroup
	for index, handler := range handlers {
		wait.Add(1)
		go func(index int, handler http.Handler) {
			defer wait.Done()
			<-start
			responses <- serve(t, handler, http.MethodPost, "/v1/submissions/"+runID+"/cancel", []byte(`{}`), testToken, strings.Repeat(string(rune('c'+index)), 32))
		}(index, handler)
	}
	close(start)
	wait.Wait()
	close(responses)
	statuses := map[int]int{}
	for response := range responses {
		statuses[response.Code]++
	}
	if statuses[http.StatusAccepted] != 1 || statuses[http.StatusOK] != 1 {
		t.Fatalf("cancel statuses = %#v, want one 202 and one 200", statuses)
	}
	if calls := trigger.CancelCalls(reference); calls != 1 {
		t.Fatalf("provider cancel calls = %d, want 1", calls)
	}
}

func TestRouteTerminalCancellationIsReplayResponse(t *testing.T) {
	fixture := newHTTPFixture(t, nil, nil)
	accepted := serve(t, fixture.handler, http.MethodPost, "/v1/submissions", validSubmissionBody("change timeout"), testToken, strings.Repeat("a", 32))
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, body = %s", accepted.Code, accepted.Body.String())
	}
	var body map[string]any
	decodeResponse(t, accepted, &body)
	runID := body["run_id"].(string)
	reference := submission.TriggerReference{Provider: "mock", ExternalRunID: "mock_" + runID}
	fixture.trigger.SetState(reference, submission.TriggerState{
		Status: submission.StatusSucceeded,
		Result: &templatecodechange.Result{RunID: runID, Status: run.StatusSucceeded},
	})

	response := serve(t, fixture.handler, http.MethodPost, "/v1/submissions/"+runID+"/cancel", []byte(`{}`), testToken, strings.Repeat("c", 32))
	if response.Code != http.StatusOK {
		t.Fatalf("terminal cancel status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls := fixture.trigger.CancelCalls(reference); calls != 0 {
		t.Fatalf("terminal provider cancel calls = %d, want 0", calls)
	}
}

func TestRouteAuthenticationScopeHealthAndReadiness(t *testing.T) {
	readyErr := errors.New("not ready")
	fixture := newHTTPFixture(t, nil, func(context.Context) error { return readyErr })

	health := serve(t, fixture.handler, http.MethodGet, "/healthz", nil, "", "")
	if health.Code != http.StatusOK || health.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Fatalf("health = %d %q", health.Code, health.Body.String())
	}
	ready := serve(t, fixture.handler, http.MethodGet, "/readyz", nil, "", "")
	assertError(t, ready, http.StatusServiceUnavailable, "provider_unavailable")

	missing := serve(t, fixture.handler, http.MethodPost, "/v1/submissions", validSubmissionBody("change timeout"), "", strings.Repeat("a", 32))
	assertError(t, missing, http.StatusUnauthorized, "unauthenticated")
	if missing.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("www-authenticate = %q", missing.Header().Get("WWW-Authenticate"))
	}
	wrong := serve(t, fixture.handler, http.MethodPost, "/v1/submissions", validSubmissionBody("change timeout"), testToken+"x", strings.Repeat("a", 32))
	assertError(t, wrong, http.StatusUnauthorized, "unauthenticated")

	first := serve(t, fixture.handler, http.MethodPost, "/v1/submissions", validSubmissionBody("change timeout"), testToken, strings.Repeat("b", 32))
	var accepted map[string]any
	decodeResponse(t, first, &accepted)
	runID := accepted["run_id"].(string)
	foreign := serve(t, fixture.handler, http.MethodGet, "/v1/submissions/"+runID, nil, fixture.secondToken, "")
	assertError(t, foreign, http.StatusNotFound, "not_found")
}

func TestMalformedRequestsAndHeadersAreBounded(t *testing.T) {
	fixture := newHTTPFixture(t, nil, nil)
	key := strings.Repeat("a", 32)
	tests := []struct {
		name        string
		method      string
		path        string
		body        []byte
		token       string
		key         string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{name: "unknown field", method: http.MethodPost, path: "/v1/submissions", body: append(bytes.TrimSuffix(validSubmissionBody("change timeout"), []byte("}")), []byte(`,"unknown":true}`)...), token: testToken, key: key, contentType: "application/json", wantStatus: 400, wantCode: "invalid_request"},
		{name: "duplicate top-level field", method: http.MethodPost, path: "/v1/submissions", body: []byte(strings.Replace(string(validSubmissionBody("change timeout")), `"template":`, `"template":{"name":"code-change","version":1},"template":`, 1)), token: testToken, key: key, contentType: "application/json", wantStatus: 400, wantCode: "invalid_request"},
		{name: "trailing value", method: http.MethodPost, path: "/v1/submissions", body: append(validSubmissionBody("change timeout"), []byte(` {}`)...), token: testToken, key: key, contentType: "application/json", wantStatus: 400, wantCode: "invalid_request"},
		{name: "malformed", method: http.MethodPost, path: "/v1/submissions", body: []byte(`{"template":`), token: testToken, key: key, contentType: "application/json", wantStatus: 400, wantCode: "invalid_request"},
		{name: "oversized", method: http.MethodPost, path: "/v1/submissions", body: []byte(`{"padding":"` + strings.Repeat("x", (1<<20)+1) + `"}`), token: testToken, key: key, contentType: "application/json", wantStatus: 413, wantCode: "invalid_request"},
		{name: "missing content type", method: http.MethodPost, path: "/v1/submissions", body: validSubmissionBody("change timeout"), token: testToken, key: key, wantStatus: 415, wantCode: "invalid_request"},
		{name: "wrong content type", method: http.MethodPost, path: "/v1/submissions", body: validSubmissionBody("change timeout"), token: testToken, key: key, contentType: "text/plain", wantStatus: 415, wantCode: "invalid_request"},
		{name: "missing idempotency", method: http.MethodPost, path: "/v1/submissions", body: validSubmissionBody("change timeout"), token: testToken, contentType: "application/json", wantStatus: 400, wantCode: "invalid_request"},
		{name: "short idempotency", method: http.MethodPost, path: "/v1/submissions", body: validSubmissionBody("change timeout"), token: testToken, key: "short", contentType: "application/json", wantStatus: 400, wantCode: "invalid_request"},
		{name: "control character idempotency", method: http.MethodPost, path: "/v1/submissions", body: validSubmissionBody("change timeout"), token: testToken, key: strings.Repeat("a", 16) + "\tinside", contentType: "application/json", wantStatus: 400, wantCode: "invalid_request"},
		{name: "large idempotency", method: http.MethodPost, path: "/v1/submissions", body: validSubmissionBody("change timeout"), token: testToken, key: strings.Repeat("a", 129), contentType: "application/json", wantStatus: 431, wantCode: "invalid_request"},
		{name: "large authorization", method: http.MethodPost, path: "/v1/submissions", body: validSubmissionBody("change timeout"), token: strings.Repeat("a", 513), key: key, contentType: "application/json", wantStatus: 431, wantCode: "invalid_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, bytes.NewReader(test.body))
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			if test.key != "" {
				request.Header.Set("Idempotency-Key", test.key)
			}
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			fixture.handler.ServeHTTP(response, request)
			assertError(t, response, test.wantStatus, test.wantCode)
			if response.Body.Len() > 512 {
				t.Fatalf("error body is unbounded: %d bytes", response.Body.Len())
			}
		})
	}
}

func TestMalformedCaseVariantJSONAliasesLeaveSubmissionStateUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{name: "top-level alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"template":`, `"Template":`, 1)
		}},
		{name: "top-level semantic duplicate", mutate: func(raw string) string {
			return strings.Replace(raw, `"template":`, `"Template":{"name":"code-change","version":1},"template":`, 1)
		}},
		{name: "nested template alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"name":"code-change"`, `"Name":"code-change"`, 1)
		}},
		{name: "nested origin alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"harness":"codex"`, `"Harness":"codex"`, 1)
		}},
		{name: "typed input alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"task_description":`, `"TASK_DESCRIPTION":`, 1)
		}},
		{name: "nested publication alias", mutate: func(raw string) string {
			return strings.Replace(raw, `"mode":"artifact"`, `"Mode":"artifact"`, 1)
		}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHTTPFixture(t, nil, nil)
			raw := test.mutate(string(validSubmissionBody("case alias")))
			response := serve(t, fixture.handler, http.MethodPost, "/v1/submissions", []byte(raw), testToken, strings.Repeat(string(rune('a'+index)), 32))
			assertError(t, response, http.StatusBadRequest, "invalid_request")
			if requests := fixture.trigger.StartRequests(); len(requests) != 0 {
				t.Fatalf("case alias started provider work: %#v", requests)
			}
		})
	}
}

func TestRouteMethodsAndUnlistedSurfacesFailClosed(t *testing.T) {
	fixture := newHTTPFixture(t, nil, nil)
	queryInjection := serve(t, fixture.handler, http.MethodPost, "/v1/submissions?workflow_name=paje-code-change-v1", validSubmissionBody("change timeout"), testToken, strings.Repeat("a", 32))
	assertError(t, queryInjection, http.StatusBadRequest, "invalid_request")
	tests := []struct {
		method string
		path   string
		allow  string
	}{
		{method: http.MethodGet, path: "/v1/submissions", allow: http.MethodPost},
		{method: http.MethodPut, path: "/healthz", allow: http.MethodGet},
		{method: http.MethodPost, path: "/v1/submissions/missing", allow: http.MethodGet},
	}
	for _, test := range tests {
		response := serve(t, fixture.handler, test.method, test.path, []byte(`{}`), testToken, strings.Repeat("a", 32))
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != test.allow {
			t.Fatalf("%s %s = %d, allow %q", test.method, test.path, response.Code, response.Header().Get("Allow"))
		}
	}
	for _, path := range []string{
		"/v1/workflows/paje-code-change-v1", "/v1/events/arbitrary", "/v1/admin",
		"/v1/artifacts/file", "/v1/credentials", "/v1/execute",
	} {
		response := serve(t, fixture.handler, http.MethodPost, path, []byte(`{}`), testToken, strings.Repeat("a", 32))
		assertError(t, response, http.StatusNotFound, "not_found")
	}
}

func TestRouteDelegatesControlWithAuthenticatedContextAndRecoversPanic(t *testing.T) {
	var sawPrincipal submission.Principal
	control := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, ok := auth.PrincipalFromContext(request.Context())
		if !ok {
			t.Fatal("control request has no principal")
		}
		sawPrincipal = principal
		if request.URL.Path == "/v1/control-runs/panic" {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{"partial":"body-secret-never-log"}`))
			panic("body-secret-never-log")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"api_version":"v1"}`))
	})
	var logs bytes.Buffer
	fixture := newHTTPFixture(t, control, nil, httpapi.WithLogger(log.New(&logs, "", 0)))

	capabilities := serve(t, fixture.handler, http.MethodGet, "/v1/capabilities", nil, testToken, "")
	if capabilities.Code != http.StatusOK || sawPrincipal.CredentialID != "codex01" {
		t.Fatalf("capabilities = %d, principal = %#v", capabilities.Code, sawPrincipal)
	}
	panicResponse := serve(t, fixture.handler, http.MethodPost, "/v1/control-runs/panic", []byte(`{"secret":"body-secret-never-log"}`), testToken, strings.Repeat("a", 32))
	assertError(t, panicResponse, http.StatusInternalServerError, "internal")
	if strings.Contains(panicResponse.Body.String(), "panic") || strings.Contains(panicResponse.Body.String(), "body-secret") {
		t.Fatalf("panic response leaks diagnostic: %q", panicResponse.Body.String())
	}
	logText := logs.String()
	if strings.Contains(logText, testToken) || strings.Contains(logText, "body-secret") || strings.Contains(logText, "Authorization") {
		t.Fatalf("request log leaks sensitive material: %q", logText)
	}
	if !strings.Contains(logText, "credential_id=codex01") || !strings.Contains(logText, "request_id=") {
		t.Fatalf("request log missing safe fields: %q", logText)
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	fixture := newHTTPFixture(t, nil, nil)
	if _, err := httpapi.New(httpapi.Dependencies{Service: fixture.service}); err == nil {
		t.Fatal("New accepted missing authenticator")
	}
	if _, err := httpapi.New(httpapi.Dependencies{Authenticator: fixture.authenticator}); err == nil {
		t.Fatal("New accepted missing service")
	}
}

func newHTTPFixture(t *testing.T, control http.Handler, ready func(context.Context) error, options ...httpapi.Option) httpFixture {
	t.Helper()
	registry, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		t.Fatal(err)
	}
	store := submissionmock.NewStore()
	trigger := submissionmock.NewTrigger()
	service, err := submission.New(submission.Dependencies{
		Templates: registry, Store: store, Trigger: trigger, Clock: fixedHTTPNow, SystemMaxDepth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	authenticator, secondToken := testAuthenticator(t)
	handler, err := httpapi.New(httpapi.Dependencies{
		Service: service, Authenticator: authenticator, Control: control, Ready: ready,
	}, options...)
	if err != nil {
		t.Fatal(err)
	}
	return httpFixture{handler: handler, authenticator: authenticator, service: service, trigger: trigger, secondToken: secondToken}
}

func testAuthenticator(t *testing.T) (*auth.Authenticator, string) {
	t.Helper()
	secondSecret := bytes.Repeat([]byte{0x22}, 32)
	secondToken := "paje_v1_other02." + base64.RawURLEncoding.EncodeToString(secondSecret)
	credential := func(id, token, subject string) map[string]any {
		secret, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(secret)
		return map[string]any{
			"id": id, "secret_hash": hex.EncodeToString(sum[:]), "subject": subject,
			"user_id": subject, "app_id": "service",
			"repositories": []string{"https://github.com/example/service.git"},
			"actions":      []string{"submit:artifact", "read", "cancel", "control:create", "task:create", "work:dispatch", "work:observe", "work:send", "work:wait", "work:interrupt", "work:close", "evidence:write", "control:close"},
			"harnesses":    []string{"codex"}, "projects": []string{"service", "docs"},
			"communication_edges": []map[string]string{{"from": "service", "to": "docs"}},
			"max_depth":           0, "expires_at": "2027-01-01T00:00:00Z",
		}
	}
	raw, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"credentials": []map[string]any{
			credential("codex01", testToken, "codex@example.com"),
			credential("other02", secondToken, "other@example.com"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	authenticator, err := auth.LoadPolicy(path, fixedHTTPNow)
	if err != nil {
		t.Fatal(err)
	}
	return authenticator, secondToken
}

func validSubmissionBody(description string) []byte {
	return []byte(`{
  "template":{"name":"code-change","version":1},
  "origin":{"harness":"codex","session_id":"session-1","turn_id":"turn-1"},
  "input":{
    "task_description":` + mustJSON(description) + `,
    "repository_uri":"https://github.com/example/service.git",
    "base_ref":"main",
    "tags":{"user_id":"codex@example.com","app_id":"service"},
    "worker_profile":"codex-go@1",
    "profile":"generic",
    "checks":[{"name":"test","directory":".","executable":"npm","args":["test"],"timeout":"10m","required":true}],
    "publication":{"mode":"artifact"}
  }
}`)
}

func mustJSON(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func serve(t *testing.T, handler http.Handler, method, path string, body []byte, token, key string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	if body != nil && (method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch) {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), destination); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d, body = %s", response.Code, status, response.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeResponse(t, response, &body)
	if body.Error.Code != code || body.Error.Message == "" || len(body.Error.Message) > 256 {
		t.Fatalf("error = %#v", body.Error)
	}
}

func fixedHTTPNow() time.Time {
	return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
}
