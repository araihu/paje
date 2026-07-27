// Package agentclient implements the narrow Codex-facing Pajé leaf API client.
package agentclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/araihu/paje/internal/run"
	"github.com/araihu/paje/internal/submission"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
)

const defaultMaxBytes int64 = 1 << 20

var (
	tokenPattern = regexp.MustCompile(`^paje_v1_([a-z][a-z0-9-]{0,62})\.([A-Za-z0-9_-]+)$`)
	runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

type SleepFunc func(context.Context, time.Duration) error

type Config struct {
	BaseURL          string
	Token            string
	HTTPClient       *http.Client
	MaxRequestBytes  int64
	MaxResponseBytes int64
	Sleep            SleepFunc
}

type Client struct {
	baseURL          *url.URL
	token            string
	httpClient       *http.Client
	maxRequestBytes  int64
	maxResponseBytes int64
	sleep            SleepFunc
}

type View struct {
	APIVersion string          `json:"api_version"`
	RunID      string          `json:"run_id"`
	Status     string          `json:"status"`
	Reused     *bool           `json:"reused,omitempty"`
	Depth      int             `json:"depth"`
	RootRunID  string          `json:"root_run_id"`
	Result     json.RawMessage `json:"result,omitempty"`
	retryAfter time.Duration
}

type APIError struct {
	StatusCode int
	Code       string
	Message    string
	retryAfter time.Duration
}

func (e *APIError) Error() string {
	if e == nil {
		return "Pajé API request failed"
	}
	return fmt.Sprintf("Pajé API request failed: status=%d code=%s message=%s", e.StatusCode, e.Code, e.Message)
}

func New(cfg Config) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Path != "" && parsed.Path != "/" || !safeTransport(parsed) {
		return nil, errors.New("create Pajé agent client: base URL is invalid")
	}
	parsed.Path = ""
	if !validToken(cfg.Token) {
		return nil, errors.New("create Pajé agent client: token is invalid")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = defaultMaxBytes
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = defaultMaxBytes
	}
	if cfg.MaxRequestBytes <= 0 || cfg.MaxRequestBytes > defaultMaxBytes ||
		cfg.MaxResponseBytes <= 0 || cfg.MaxResponseBytes > defaultMaxBytes {
		return nil, errors.New("create Pajé agent client: byte limits are invalid")
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepContext
	}
	return &Client{
		baseURL: parsed, token: cfg.Token, httpClient: cfg.HTTPClient,
		maxRequestBytes: cfg.MaxRequestBytes, maxResponseBytes: cfg.MaxResponseBytes, sleep: cfg.Sleep,
	}, nil
}

func (c *Client) Submit(ctx context.Context, threadID string, raw json.RawMessage) (View, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" || len(threadID) > 128 || strings.ContainsAny(threadID, "\x00\r\n") {
		return View{}, errors.New("submit Pajé run: Codex thread ID is invalid")
	}
	input, err := templatecodechange.Decode(raw)
	if err != nil {
		return View{}, fmt.Errorf("submit Pajé run: %w", err)
	}
	if input.Publication.Mode != "artifact" {
		return View{}, errors.New("submit Pajé run: leaf client permits artifact publication only")
	}
	if input.IdempotencyKey != "" {
		return View{}, errors.New("submit Pajé run: input idempotency key is client-managed")
	}
	withoutKey, err := canonicalJSON(input)
	if err != nil {
		return View{}, err
	}
	sum := sha256.Sum256([]byte(
		"paje-codex-leaf-v1\x00" + threadID + "\x00" + input.RepositoryURI + "\x00" + input.BaseRef + "\x00" + string(withoutKey),
	))
	key := hex.EncodeToString(sum[:])
	canonicalInput, err := canonicalJSON(input)
	if err != nil {
		return View{}, err
	}
	envelope := struct {
		Template template.ID       `json:"template"`
		Origin   submission.Origin `json:"origin"`
		Input    json.RawMessage   `json:"input"`
	}{
		Template: templatecodechange.ID,
		Origin: submission.Origin{
			Harness: "codex", SessionID: threadID, TurnID: key[:32],
		},
		Input: canonicalInput,
	}
	body, err := canonicalJSON(envelope)
	if err != nil {
		return View{}, err
	}
	return c.request(ctx, http.MethodPost, "/v1/submissions", key, body, "")
}

func (c *Client) Status(ctx context.Context, runID string) (View, error) {
	if !runIDPattern.MatchString(runID) {
		return View{}, errors.New("inspect Pajé run: run ID is invalid")
	}
	return c.request(ctx, http.MethodGet, "/v1/submissions/"+runID, "", nil, runID)
}

func (c *Client) Wait(ctx context.Context, runID string) (View, error) {
	for {
		view, err := c.Status(ctx, runID)
		if err != nil {
			var apiError *APIError
			if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusServiceUnavailable || apiError.Code != "provider_unavailable" {
				return View{}, err
			}
			if err := c.sleep(ctx, boundedRetry(apiError.retryAfter)); err != nil {
				return View{}, err
			}
			continue
		}
		if terminal(view.Status) {
			return view, nil
		}
		if err := c.sleep(ctx, boundedRetry(view.retryAfter)); err != nil {
			return View{}, err
		}
	}
}

func (c *Client) Cancel(ctx context.Context, runID string) (View, error) {
	if !runIDPattern.MatchString(runID) {
		return View{}, errors.New("cancel Pajé run: run ID is invalid")
	}
	sum := sha256.Sum256([]byte("paje-codex-cancel-v1\x00" + runID))
	return c.request(ctx, http.MethodPost, "/v1/submissions/"+runID+"/cancel", hex.EncodeToString(sum[:]), []byte("{}"), runID)
}

func (c *Client) request(
	ctx context.Context,
	method, path, idempotency string,
	body []byte,
	wantRunID string,
) (View, error) {
	if int64(len(body)) > c.maxRequestBytes {
		return View{}, errors.New("Pajé request exceeds byte limit")
	}
	target := *c.baseURL
	target.Path = path
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return View{}, errors.New("build Pajé request")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return View{}, fmt.Errorf("Pajé request unavailable: %w", err)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil || int64(len(contents)) > c.maxResponseBytes {
		return View{}, errors.New("Pajé response exceeds byte limit")
	}
	mediaType, _, contentTypeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if contentTypeErr != nil || mediaType != "application/json" {
		return View{}, errors.New("Pajé response content type is invalid")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(contents, &failure) != nil || failure.Error.Code == "" || failure.Error.Message == "" {
			return View{}, &APIError{StatusCode: response.StatusCode, Code: "invalid_response", Message: "gateway returned an invalid error"}
		}
		return View{}, &APIError{
			StatusCode: response.StatusCode, Code: failure.Error.Code, Message: failure.Error.Message,
			retryAfter: parseRetryAfter(response.Header.Get("Retry-After")),
		}
	}
	var view View
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&view); err != nil || view.APIVersion != "v1" || !runIDPattern.MatchString(view.RunID) ||
		view.RootRunID == "" || view.Depth < 0 || view.Depth > 1 || wantRunID != "" && view.RunID != wantRunID {
		return View{}, errors.New("Pajé response is invalid")
	}
	if terminal(view.Status) {
		if len(view.Result) == 0 {
			return View{}, errors.New("Pajé terminal response is missing result")
		}
		var binding struct {
			RunID  string `json:"run_id"`
			Status string `json:"status"`
		}
		if json.Unmarshal(view.Result, &binding) != nil || binding.RunID != view.RunID || binding.Status != view.Status {
			return View{}, errors.New("Pajé terminal result binding is invalid")
		}
	} else if !nonterminal(view.Status) {
		return View{}, errors.New("Pajé response status is invalid")
	}
	view.retryAfter = parseRetryAfter(response.Header.Get("Retry-After"))
	return view, nil
}

func canonicalJSON(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("encode Pajé request")
	}
	canonical, err := run.CanonicalInput(raw)
	if err != nil {
		return nil, errors.New("canonicalize Pajé request")
	}
	return canonical, nil
}

func validToken(token string) bool {
	matches := tokenPattern.FindStringSubmatch(token)
	if len(matches) != 3 || strings.TrimSpace(token) != token {
		return false
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(matches[2])
	return err == nil && len(secret) == sha256.Size
}

func safeTransport(target *url.URL) bool {
	if target.Scheme == "https" {
		return true
	}
	if target.Scheme != "http" {
		return false
	}
	host := target.Hostname()
	if host == "localhost" {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

func terminal(status string) bool {
	switch status {
	case "succeeded", "failed", "canceled", "declined":
		return true
	default:
		return false
	}
}

func nonterminal(status string) bool {
	switch status {
	case "accepted", "queued", "running", "awaiting_approval", "cancellation_requested":
		return true
	default:
		return false
	}
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return time.Second
	}
	return time.Duration(seconds) * time.Second
}

func boundedRetry(delay time.Duration) time.Duration {
	if delay < time.Second {
		return time.Second
	}
	if delay > 15*time.Second {
		return 15 * time.Second
	}
	return delay
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
