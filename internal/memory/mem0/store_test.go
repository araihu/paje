package mem0_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/araihu/paje/internal/memory/mem0"
)

func TestSearchUsesV3ContractAndMapsResults(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v3/memories/search/" {
			t.Errorf("path = %q, want /v3/memories/search/", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Token secret" {
			t.Errorf("Authorization = %q, want Token secret", got)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}

		var body struct {
			Query   string                      `json:"query"`
			Filters map[string][]map[string]any `json:"filters"`
			TopK    int                         `json:"top_k"`
		}
		decodeJSON(t, request.Body, &body)
		if body.Query != "agent task" || body.TopK != 5 {
			t.Errorf("search body = %#v", body)
		}
		wantFilters := []map[string]any{
			{"app_id": "paje"},
			{"metadata": map[string]any{"kind": "result"}},
		}
		assertJSONValue(t, body.Filters["AND"], wantFilters)

		writeJSON(t, writer, http.StatusOK, map[string]any{
			"results": []map[string]any{{
				"id":       "memory-1",
				"memory":   "The agent completed the task",
				"metadata": map[string]any{"kind": "result", "attempt": 2},
			}},
		})
	}))
	defer server.Close()

	store, err := mem0.New("secret", mem0.WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got, err := store.Search(
		context.Background(),
		"agent task",
		5,
		map[string]string{"app_id": "paje", "kind": "result"},
	)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search() returned %d memories, want 1", len(got))
	}
	if got[0].ID != "memory-1" || got[0].Content != "The agent completed the task" {
		t.Errorf("Search() memory = %#v", got[0])
	}
	if got[0].Metadata["kind"] != "result" || got[0].Metadata["attempt"] != "2" {
		t.Errorf("Search() metadata = %#v", got[0].Metadata)
	}
}

func TestSaveUsesV3ContractAndWaitsForCompletion(t *testing.T) {
	t.Parallel()

	var eventRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v3/memories/add/":
			if request.Method != http.MethodPost {
				t.Errorf("add method = %s, want POST", request.Method)
			}
			var body struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
				AppID    string            `json:"app_id"`
				Metadata map[string]string `json:"metadata"`
				Infer    bool              `json:"infer"`
			}
			decodeJSON(t, request.Body, &body)
			if len(body.Messages) != 1 ||
				body.Messages[0].Role != "user" ||
				body.Messages[0].Content != "execution complete" {
				t.Errorf("messages = %#v", body.Messages)
			}
			if body.AppID != "paje" {
				t.Errorf("app_id = %q, want paje", body.AppID)
			}
			if body.Metadata["kind"] != "result" {
				t.Errorf("metadata = %#v", body.Metadata)
			}
			if body.Infer {
				t.Error("infer = true, want false")
			}
			writeJSON(t, writer, http.StatusOK, map[string]any{
				"status":   "PENDING",
				"event_id": "event-1",
			})
		case "/v1/event/event-1/":
			eventRequests.Add(1)
			writeJSON(t, writer, http.StatusOK, map[string]any{"status": "SUCCEEDED"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	store, err := mem0.New(
		"secret",
		mem0.WithBaseURL(server.URL),
		mem0.WithPollInterval(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := store.Save(
		context.Background(),
		"execution complete",
		map[string]string{"app_id": "paje", "kind": "result"},
	); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if eventRequests.Load() != 1 {
		t.Errorf("event requests = %d, want 1", eventRequests.Load())
	}
}

func TestStoreRequiresEntityTags(t *testing.T) {
	t.Parallel()

	store, err := mem0.New("secret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	tags := map[string]string{"kind": "result"}
	if _, err := store.Search(context.Background(), "query", 1, tags); err == nil {
		t.Error("Search() error = nil, want missing entity error")
	}
	if err := store.Save(context.Background(), "content", tags); err == nil {
		t.Error("Save() error = nil, want missing entity error")
	}
}

func TestSearchReportsBoundedHTTPFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, strings.Repeat("x", 5000), http.StatusBadGateway)
	}))
	defer server.Close()

	store, err := mem0.New("secret", mem0.WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = store.Search(
		context.Background(),
		"query",
		1,
		map[string]string{"app_id": "paje"},
	)
	if err == nil {
		t.Fatal("Search() error = nil, want HTTP error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("Search() error = %q, want status code", err)
	}
	if len(err.Error()) > 4300 {
		t.Errorf("Search() error length = %d, want bounded diagnostic", len(err.Error()))
	}
}

func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		new  func() error
	}{
		{
			name: "missing API key",
			new: func() error {
				_, err := mem0.New(" ")
				return err
			},
		},
		{
			name: "invalid base URL",
			new: func() error {
				_, err := mem0.New("secret", mem0.WithBaseURL("://bad"))
				return err
			},
		},
		{
			name: "nil HTTP client",
			new: func() error {
				_, err := mem0.New("secret", mem0.WithHTTPClient(nil))
				return err
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if err := testCase.new(); err == nil {
				t.Fatal("New() error = nil, want validation error")
			}
		})
	}
}

func decodeJSON(t *testing.T, source io.Reader, target any) {
	t.Helper()
	if err := json.NewDecoder(source).Decode(target); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
}

func writeJSON(t *testing.T, writer http.ResponseWriter, status int, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("Encode() error = %v", err)
	}
}

func assertJSONValue(t *testing.T, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal(got) error = %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal(want) error = %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("JSON value = %s, want %s", gotJSON, wantJSON)
	}
}

func ExampleStore() {
	store, err := mem0.New("api-key")
	if err != nil {
		panic(err)
	}
	fmt.Printf("%T\n", store)
	// Output: *mem0.Store
}
