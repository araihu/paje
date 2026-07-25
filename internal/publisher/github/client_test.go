package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/publisher/gitpr"
)

const (
	testToken = "github_pat_never_log_this"
	testSHA   = "0123456789abcdef0123456789abcdef01234567"
)

func TestClientFindUsesExactGitHubRequestAndSelectsExactPullRequest(t *testing.T) {
	var requestURI string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertGitHubHeaders(t, r)
		requestURI = r.URL.RequestURI()
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		writeJSON(t, w, []map[string]any{
			pullJSON(8, "other", "main", "araihu/paje", testSHA, "open"),
			pullJSON(17, "paje/code-change/run-123", "main", "araihu/paje", testSHA, "open"),
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	got, err := client.Find(context.Background(), pullRequestRequest())
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if requestURI != "/repos/araihu/paje/pulls?state=all&head=araihu:paje%2Fcode-change%2Frun-123&base=main" {
		t.Fatalf("request URI = %q", requestURI)
	}
	want := &gitpr.PullRequest{ID: "17", URL: "https://github.com/araihu/paje/pull/17", HeadSHA: testSHA}
	if got == nil || *got != *want {
		t.Fatalf("Find() = %#v, want %#v", got, want)
	}
}

func TestClientCreateUsesExactGitHubPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertGitHubHeaders(t, r)
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/repos/araihu/paje/pulls" {
			t.Fatalf("request = %s %s", r.Method, r.URL.RequestURI())
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"head": "paje/code-change/run-123", "base": "main",
			"title": "Update value", "body": "Generated safely.", "draft": true,
		}
		if !equalJSON(got, want) {
			t.Fatalf("POST JSON = %#v, want %#v", got, want)
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, pullJSON(17, "paje/code-change/run-123", "main", "araihu/paje", testSHA, "open"))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	got, err := client.Create(context.Background(), pullRequestRequest())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got != (gitpr.PullRequest{ID: "17", URL: "https://github.com/araihu/paje/pull/17", HeadSHA: testSHA}) {
		t.Fatalf("Create() = %#v", got)
	}
}

func TestClientCreateRejectsResponseWithWrongPullRequestBinding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, pullJSON(17, "other-branch", "main", "araihu/paje", testSHA, "open"))
	}))
	defer server.Close()
	client := newTestClient(t, server)
	if got, err := client.Create(context.Background(), pullRequestRequest()); !errors.Is(err, publisher.ErrConflict) {
		t.Fatalf("Create() = %#v, %v; want ErrConflict", got, err)
	}
}

func TestClientCreateRejectsUnexpectedSuccessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, pullJSON(17, "paje/code-change/run-123", "main", "araihu/paje", testSHA, "open"))
	}))
	defer server.Close()
	client := newTestClient(t, server)
	if got, err := client.Create(context.Background(), pullRequestRequest()); err == nil {
		t.Fatalf("Create() = %#v, nil error for HTTP 200", got)
	}
}

func TestClientClassifiesBoundedSanitizedHTTPFailures(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantError error
		retryable bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantError: publisher.ErrProviderUnavailable},
		{name: "forbidden", status: http.StatusForbidden, wantError: publisher.ErrProviderUnavailable},
		{name: "conflict", status: http.StatusConflict, wantError: publisher.ErrConflict},
		{name: "rate limited", status: http.StatusTooManyRequests, retryable: true},
		{name: "server failure", status: http.StatusServiceUnavailable, retryable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, strings.Repeat("diagnostic-", 600)+testToken)
			}))
			defer server.Close()
			client := newTestClient(t, server)
			_, err := client.Find(context.Background(), pullRequestRequest())
			if err == nil {
				t.Fatal("Find() error = nil")
			}
			if tt.wantError != nil && !errors.Is(err, tt.wantError) {
				t.Fatalf("Find() error = %v, want %v", err, tt.wantError)
			}
			if got := gitpr.IsRetryable(err); got != tt.retryable {
				t.Fatalf("IsRetryable(error) = %v, want %v (%v)", got, tt.retryable, err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("error leaked token: %v", err)
			}
			if len(err.Error()) > 4300 {
				t.Fatalf("error diagnostic length = %d, want bounded", len(err.Error()))
			}
		})
	}
}

func TestClientRedactsTokenThatCrossesDiagnosticBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, strings.Repeat("x", maxDiagnosticBytes-6)+testToken)
	}))
	defer server.Close()
	client := newTestClient(t, server)
	_, err := client.Find(context.Background(), pullRequestRequest())
	if err == nil {
		t.Fatal("Find() error = nil")
	}
	if strings.Contains(err.Error(), testToken[:6]) {
		t.Fatalf("diagnostic leaked token prefix across bound: %v", err)
	}
}

func TestClientCreateReusesExactPullRequestAfterProviderConflict(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusUnprocessableEntity)
			writeJSON(t, w, map[string]any{"message": "already exists"})
			return
		}
		writeJSON(t, w, []map[string]any{
			pullJSON(17, "paje/code-change/run-123", "main", "araihu/paje", testSHA, "open"),
		})
	}))
	defer server.Close()
	client := newTestClient(t, server)

	got, err := client.Create(context.Background(), pullRequestRequest())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got.ID != "17" || got.HeadSHA != testSHA {
		t.Fatalf("Create() = %#v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "POST,GET" {
		t.Fatalf("request methods = %v, want POST then GET", methods)
	}
}

func TestClientRejectsMalformedOrInexactProviderResponses(t *testing.T) {
	tests := []struct {
		name     string
		response any
	}{
		{name: "closed", response: []map[string]any{pullJSON(17, "paje/code-change/run-123", "main", "araihu/paje", testSHA, "closed")}},
		{name: "bad sha", response: []map[string]any{pullJSON(17, "paje/code-change/run-123", "main", "araihu/paje", "abc", "open")}},
		{name: "wrong URL host", response: []map[string]any{func() map[string]any {
			value := pullJSON(17, "paje/code-change/run-123", "main", "araihu/paje", testSHA, "open")
			value["html_url"] = "https://evil.invalid/steal"
			return value
		}()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, tt.response)
			}))
			defer server.Close()
			client := newTestClient(t, server)
			got, err := client.Find(context.Background(), pullRequestRequest())
			if err == nil {
				t.Fatalf("Find() = %#v, nil error; want fail-closed malformed response", got)
			}
		})
	}
}

func TestClientIgnoresPullRequestFromDifferentRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, []map[string]any{
			pullJSON(17, "paje/code-change/run-123", "main", "other/paje", testSHA, "open"),
		})
	}))
	defer server.Close()
	client := newTestClient(t, server)
	got, err := client.Find(context.Background(), pullRequestRequest())
	if err != nil || got != nil {
		t.Fatalf("Find() = %#v, %v; want no exact match", got, err)
	}
}

func TestPushURLNormalizesSupportedGitHubRepositoriesWithoutCredentials(t *testing.T) {
	tests := map[string]string{
		"https://github.com/araihu/paje.git": "https://github.com/araihu/paje.git",
		"https://github.com/araihu/paje":     "https://github.com/araihu/paje.git",
		"git@github.com:araihu/paje.git":     "https://github.com/araihu/paje.git",
		"ssh://git@github.com/araihu/paje":   "https://github.com/araihu/paje.git",
	}
	for input, want := range tests {
		got, err := PushURL(input)
		if err != nil {
			t.Fatalf("PushURL(%q) error = %v", input, err)
		}
		if got != want {
			t.Fatalf("PushURL(%q) = %q, want %q", input, got, want)
		}
		if strings.Contains(got, testToken) || strings.Contains(got, "@github.com:") {
			t.Fatalf("PushURL(%q) returned credential-bearing or SSH URL %q", input, got)
		}
	}
	for _, input := range []string{
		"https://token@github.com/araihu/paje.git",
		"https://example.com/araihu/paje.git",
		"git@github.com:../paje.git",
	} {
		if got, err := PushURL(input); err == nil {
			t.Fatalf("PushURL(%q) = %q, nil error", input, got)
		}
	}
}

func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := NewClient(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func pullRequestRequest() gitpr.PullRequestRequest {
	return gitpr.PullRequestRequest{
		Repository: "https://github.com/araihu/paje.git",
		Head:       "paje/code-change/run-123", Base: "main",
		Title: "Update value", Body: "Generated safely.", Draft: true,
	}
}

func pullJSON(number int, head, base, repository, sha, state string) map[string]any {
	return map[string]any{
		"id": number, "number": number,
		"html_url": "https://github.com/araihu/paje/pull/17",
		"state":    state,
		"head": map[string]any{
			"ref": head, "sha": sha,
			"repo": map[string]any{"full_name": repository},
		},
		"base": map[string]any{
			"ref":  base,
			"repo": map[string]any{"full_name": repository},
		},
	}
}

func assertGitHubHeaders(t *testing.T, request *http.Request) {
	t.Helper()
	if got := request.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", got)
	}
	if got := request.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Fatalf("Accept = %q", got)
	}
	if got := request.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
		t.Fatalf("X-GitHub-Api-Version = %q", got)
	}
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func equalJSON(left, right map[string]any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}
