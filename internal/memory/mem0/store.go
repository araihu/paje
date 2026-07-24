// Package mem0 implements the memory port with the Mem0 Platform HTTP API.
package mem0

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/araihu/paje/internal/memory"
)

const (
	defaultBaseURL      = "https://api.mem0.ai"
	defaultPollInterval = 100 * time.Millisecond
	maxDiagnosticBytes  = 4096
)

var entityKeys = []string{"user_id", "agent_id", "app_id", "run_id"}

// Store persists memories through Mem0 Platform v3.
type Store struct {
	apiKey       string
	baseURL      *url.URL
	httpClient   *http.Client
	pollInterval time.Duration
}

var _ memory.Store = (*Store)(nil)

// Option configures a Store.
type Option func(*Store) error

// WithBaseURL overrides the Mem0 API origin.
func WithBaseURL(rawURL string) Option {
	return func(store *Store) error {
		parsed, err := parseBaseURL(rawURL)
		if err != nil {
			return err
		}
		store.baseURL = parsed
		return nil
	}
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(client *http.Client) Option {
	return func(store *Store) error {
		if client == nil {
			return fmt.Errorf("configure mem0 store: HTTP client is required")
		}
		store.httpClient = client
		return nil
	}
}

// WithPollInterval controls how often asynchronous save events are polled.
func WithPollInterval(interval time.Duration) Option {
	return func(store *Store) error {
		if interval <= 0 {
			return fmt.Errorf("configure mem0 store: poll interval must be positive")
		}
		store.pollInterval = interval
		return nil
	}
}

// New constructs a Mem0 Platform store.
func New(apiKey string, options ...Option) (*Store, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("create mem0 store: API key is required")
	}
	baseURL, err := parseBaseURL(defaultBaseURL)
	if err != nil {
		return nil, err
	}
	store := &Store{
		apiKey:       apiKey,
		baseURL:      baseURL,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		pollInterval: defaultPollInterval,
	}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("create mem0 store: nil option")
		}
		if err := option(store); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// Search retrieves semantically relevant memories scoped by tags.
func (s *Store) Search(
	ctx context.Context,
	query string,
	limit int,
	tags map[string]string,
) ([]memory.Memory, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("search mem0: query is required")
	}
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("search mem0: limit must be between 1 and 1000")
	}
	filters, err := buildFilters(tags)
	if err != nil {
		return nil, fmt.Errorf("search mem0: %w", err)
	}

	request := searchRequest{
		Query:   query,
		Filters: filters,
		TopK:    limit,
	}
	var response searchResponse
	if err := s.doJSON(
		ctx,
		http.MethodPost,
		"/v3/memories/search/",
		request,
		&response,
	); err != nil {
		return nil, fmt.Errorf("search mem0: %w", err)
	}

	result := make([]memory.Memory, len(response.Results))
	for i, item := range response.Results {
		result[i] = memory.Memory{
			ID:       item.ID,
			Content:  item.Memory,
			Metadata: stringifyMetadata(item.Metadata),
		}
	}
	return result, nil
}

// Save persists content verbatim and waits for Mem0's asynchronous event to
// reach a terminal successful state.
func (s *Store) Save(ctx context.Context, content string, tags map[string]string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("save mem0: content is required")
	}
	entities, metadata, err := splitTags(tags)
	if err != nil {
		return fmt.Errorf("save mem0: %w", err)
	}

	request := addRequest{
		Messages: []message{{Role: "user", Content: content}},
		Metadata: metadata,
		Infer:    false,
	}
	request.UserID = entities["user_id"]
	request.AgentID = entities["agent_id"]
	request.AppID = entities["app_id"]
	request.RunID = entities["run_id"]

	var response eventResponse
	if err := s.doJSON(
		ctx,
		http.MethodPost,
		"/v3/memories/add/",
		request,
		&response,
	); err != nil {
		return fmt.Errorf("save mem0: %w", err)
	}
	switch strings.ToUpper(response.Status) {
	case "SUCCEEDED":
		return nil
	case "FAILED":
		return fmt.Errorf("save mem0: memory event failed")
	case "PENDING":
		if strings.TrimSpace(response.EventID) == "" {
			return fmt.Errorf("save mem0: pending response omitted event ID")
		}
		return s.waitForEvent(ctx, response.EventID)
	default:
		return fmt.Errorf("save mem0: unexpected event status %q", response.Status)
	}
}

func (s *Store) waitForEvent(ctx context.Context, eventID string) error {
	endpoint := "/v1/event/" + url.PathEscape(eventID) + "/"
	for {
		var event eventResponse
		if err := s.doJSON(ctx, http.MethodGet, endpoint, nil, &event); err != nil {
			return fmt.Errorf("poll memory event %q: %w", eventID, err)
		}
		switch strings.ToUpper(event.Status) {
		case "SUCCEEDED":
			return nil
		case "FAILED":
			return fmt.Errorf("poll memory event %q: event failed", eventID)
		case "PENDING":
		default:
			return fmt.Errorf("poll memory event %q: unexpected status %q", eventID, event.Status)
		}

		timer := time.NewTimer(s.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) doJSON(
	ctx context.Context,
	method string,
	endpoint string,
	payload any,
	target any,
) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, s.endpoint(endpoint), body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Authorization", "Token "+s.apiKey)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		diagnostic, readErr := io.ReadAll(io.LimitReader(response.Body, maxDiagnosticBytes))
		if readErr != nil {
			return fmt.Errorf("HTTP %d; read diagnostic: %w", response.StatusCode, readErr)
		}
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(diagnostic)))
	}
	if target == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (s *Store) endpoint(path string) string {
	return strings.TrimRight(s.baseURL.String(), "/") + "/" + strings.TrimLeft(path, "/")
}

func parseBaseURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("configure mem0 store: parse base URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("configure mem0 store: base URL must be an HTTP(S) origin")
	}
	return parsed, nil
}

func buildFilters(tags map[string]string) (map[string]any, error) {
	entities, metadata, err := splitTags(tags)
	if err != nil {
		return nil, err
	}
	clauses := make([]map[string]any, 0, len(entities)+1)
	for _, key := range entityKeys {
		if value := entities[key]; value != "" {
			clauses = append(clauses, map[string]any{key: value})
		}
	}
	if len(metadata) > 0 {
		clauses = append(clauses, map[string]any{"metadata": metadata})
	}
	return map[string]any{"AND": clauses}, nil
}

func splitTags(tags map[string]string) (map[string]string, map[string]string, error) {
	entities := make(map[string]string, len(entityKeys))
	metadata := make(map[string]string)
	for key, value := range tags {
		if isEntityKey(key) {
			if strings.TrimSpace(value) != "" {
				entities[key] = value
			}
			continue
		}
		metadata[key] = value
	}
	if len(entities) == 0 {
		return nil, nil, fmt.Errorf(
			"at least one entity tag is required: user_id, agent_id, app_id, or run_id",
		)
	}
	return entities, metadata, nil
}

func isEntityKey(key string) bool {
	for _, candidate := range entityKeys {
		if key == candidate {
			return true
		}
	}
	return false
}

func stringifyMetadata(source map[string]any) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		if text, ok := value.(string); ok {
			result[key] = text
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			result[key] = fmt.Sprint(value)
			continue
		}
		result[key] = string(encoded)
	}
	return result
}

type searchRequest struct {
	Query   string         `json:"query"`
	Filters map[string]any `json:"filters"`
	TopK    int            `json:"top_k"`
}

type searchResponse struct {
	Results []struct {
		ID       string         `json:"id"`
		Memory   string         `json:"memory"`
		Metadata map[string]any `json:"metadata"`
	} `json:"results"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type addRequest struct {
	Messages []message         `json:"messages"`
	UserID   string            `json:"user_id,omitempty"`
	AgentID  string            `json:"agent_id,omitempty"`
	AppID    string            `json:"app_id,omitempty"`
	RunID    string            `json:"run_id,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Infer    bool              `json:"infer"`
}

type eventResponse struct {
	Status  string `json:"status"`
	EventID string `json:"event_id"`
}
