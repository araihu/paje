// Package github provides GitHub pull-request and credential adapters.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/araihu/paje/internal/publisher"
	"github.com/araihu/paje/internal/publisher/gitpr"
)

const (
	apiVersion              = "2022-11-28"
	acceptHeader            = "application/vnd.github+json"
	maxDiagnosticBytes      = 4096
	maxSuccessResponseBytes = 1 << 20
)

var repositoryComponent = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var responseSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Client implements GitHub's pull-request HTTP contract.
type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

var _ gitpr.PullRequests = (*Client)(nil)

// NewClient constructs a bounded GitHub HTTP client.
func NewClient(baseURL, token string, client *http.Client) (*Client, error) {
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\x00\r\n") {
		return nil, fmt.Errorf("create GitHub client: token is invalid")
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("create GitHub client: base URL is invalid")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname())) {
		return nil, fmt.Errorf("create GitHub client: base URL must use HTTPS")
	}
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return &Client{baseURL: parsed, token: token, http: &copyClient}, nil
}

// Find returns only an exact open pull request for req.
func (c *Client) Find(ctx context.Context, req gitpr.PullRequestRequest) (*gitpr.PullRequest, error) {
	owner, repository, err := parseRepository(req.Repository)
	if err != nil {
		return nil, err
	}
	if err := validatePullRequestRequest(req); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf(
		"/repos/%s/%s/pulls",
		url.PathEscape(owner), url.PathEscape(repository),
	)
	query := "state=all&head=" + escapeQuery(owner+":"+req.Head) + "&base=" + escapeQuery(req.Base)
	body, err := c.do(ctx, http.MethodGet, endpoint+"?"+query, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var responses []apiPullRequest
	if err := json.Unmarshal(body, &responses); err != nil {
		return nil, fmt.Errorf("%w: decode GitHub pull requests", publisher.ErrProviderUnavailable)
	}
	fullName := owner + "/" + repository
	for _, response := range responses {
		if response.Head.Ref != req.Head || response.Base.Ref != req.Base ||
			response.Head.Repository.FullName != fullName ||
			response.Base.Repository.FullName != fullName {
			continue
		}
		pullRequest, err := validateAPIResponse(response, owner, repository, req.Head, req.Base)
		if err != nil {
			return nil, err
		}
		return &pullRequest, nil
	}
	return nil, nil
}

// Create creates req, or verifies and returns an exact pull request after a
// provider conflict caused by a concurrent publisher.
func (c *Client) Create(ctx context.Context, req gitpr.PullRequestRequest) (gitpr.PullRequest, error) {
	owner, repository, err := parseRepository(req.Repository)
	if err != nil {
		return gitpr.PullRequest{}, err
	}
	if err := validatePullRequestRequest(req); err != nil {
		return gitpr.PullRequest{}, err
	}
	endpoint := fmt.Sprintf(
		"/repos/%s/%s/pulls",
		url.PathEscape(owner), url.PathEscape(repository),
	)
	payload, err := json.Marshal(struct {
		Head  string `json:"head"`
		Base  string `json:"base"`
		Title string `json:"title"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}{
		Head: req.Head, Base: req.Base, Title: req.Title, Body: req.Body, Draft: req.Draft,
	})
	if err != nil {
		return gitpr.PullRequest{}, fmt.Errorf("encode GitHub pull request: %w", err)
	}
	body, err := c.do(ctx, http.MethodPost, endpoint, payload, http.StatusCreated)
	if err != nil {
		if errors.Is(err, publisher.ErrConflict) {
			existing, findErr := c.Find(ctx, req)
			if findErr != nil {
				return gitpr.PullRequest{}, errors.Join(err, findErr)
			}
			if existing != nil {
				return *existing, nil
			}
		}
		return gitpr.PullRequest{}, err
	}
	var response apiPullRequest
	if err := json.Unmarshal(body, &response); err != nil {
		return gitpr.PullRequest{}, fmt.Errorf("%w: decode created GitHub pull request", publisher.ErrProviderUnavailable)
	}
	return validateAPIResponse(response, owner, repository, req.Head, req.Base)
}

func (c *Client) do(
	ctx context.Context,
	method, endpoint string,
	payload []byte,
	successStatuses ...int,
) ([]byte, error) {
	target := *c.baseURL
	target.Path = strings.TrimRight(c.baseURL.Path, "/") + strings.SplitN(endpoint, "?", 2)[0]
	if query := strings.SplitN(endpoint, "?", 2); len(query) == 2 {
		target.RawQuery = query[1]
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create GitHub request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", acceptHeader)
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, retryableProviderError{error: fmt.Errorf("%w: GitHub transport failed", publisher.ErrProviderUnavailable)}
	}
	defer response.Body.Close()
	for _, status := range successStatuses {
		if response.StatusCode == status {
			body, tooLarge, err := readBounded(response.Body, maxSuccessResponseBytes)
			if err != nil {
				return nil, retryableProviderError{error: fmt.Errorf("%w: read GitHub response", publisher.ErrProviderUnavailable)}
			}
			if tooLarge {
				return nil, fmt.Errorf("%w: GitHub response exceeded limit", publisher.ErrProviderUnavailable)
			}
			return body, nil
		}
	}
	diagnostic, _, readErr := readBounded(response.Body, maxDiagnosticBytes+int64(len(c.token)))
	if readErr != nil {
		diagnostic = []byte("unreadable response")
	}
	message := sanitizeDiagnostic(string(diagnostic), c.token)
	if len(message) > maxDiagnosticBytes {
		message = message[:maxDiagnosticBytes]
	}
	base := fmt.Errorf("GitHub HTTP %d: %s", response.StatusCode, message)
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: %v", publisher.ErrProviderUnavailable, base)
	case response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusUnprocessableEntity:
		return nil, fmt.Errorf("%w: %v", publisher.ErrConflict, base)
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return nil, retryableProviderError{error: fmt.Errorf("%w: %v", publisher.ErrProviderUnavailable, base)}
	default:
		return nil, fmt.Errorf("%w: %v", publisher.ErrProviderUnavailable, base)
	}
}

type apiPullRequest struct {
	ID      int64  `json:"id"`
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Head    struct {
		Ref        string `json:"ref"`
		SHA        string `json:"sha"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref        string `json:"ref"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

func validateAPIResponse(response apiPullRequest, owner, repository, head, base string) (gitpr.PullRequest, error) {
	fullName := owner + "/" + repository
	parsed, err := url.Parse(response.HTMLURL)
	switch {
	case response.ID <= 0:
		return gitpr.PullRequest{}, fmt.Errorf("%w: GitHub pull request ID is invalid", publisher.ErrConflict)
	case response.State != "open":
		return gitpr.PullRequest{}, fmt.Errorf("%w: GitHub pull request is not open", publisher.ErrConflict)
	case response.Head.Ref != head || response.Base.Ref != base:
		return gitpr.PullRequest{}, fmt.Errorf("%w: GitHub pull request branch binding does not match", publisher.ErrConflict)
	case response.Head.Repository.FullName != fullName || response.Base.Repository.FullName != fullName:
		return gitpr.PullRequest{}, fmt.Errorf("%w: GitHub pull request repository does not match", publisher.ErrConflict)
	case !responseSHA.MatchString(response.Head.SHA):
		return gitpr.PullRequest{}, fmt.Errorf("%w: GitHub pull request head SHA is invalid", publisher.ErrConflict)
	case err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.User != nil:
		return gitpr.PullRequest{}, fmt.Errorf("%w: GitHub pull request URL is invalid", publisher.ErrConflict)
	}
	return gitpr.PullRequest{
		ID: strconv.FormatInt(response.ID, 10), URL: response.HTMLURL, HeadSHA: response.Head.SHA,
	}, nil
}

func validatePullRequestRequest(request gitpr.PullRequestRequest) error {
	for name, value := range map[string]string{
		"head": request.Head, "base": request.Base, "title": request.Title,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%w: pull request %s is invalid", publisher.ErrInvalidRequest, name)
		}
	}
	if strings.ContainsRune(request.Body, '\x00') {
		return fmt.Errorf("%w: pull request body is invalid", publisher.ErrInvalidRequest)
	}
	return nil
}

// PushURL converts supported GitHub repository URIs into a credential-free
// HTTPS Git URL suitable for token-based GIT_ASKPASS authentication.
func PushURL(repository string) (string, error) {
	owner, name, err := parseRepository(repository)
	if err != nil {
		return "", err
	}
	return "https://github.com/" + owner + "/" + name + ".git", nil
}

func parseRepository(repository string) (string, string, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" || strings.ContainsAny(repository, "\x00\r\n") {
		return "", "", fmt.Errorf("%w: GitHub repository is invalid", publisher.ErrInvalidRequest)
	}
	var owner, name string
	switch {
	case strings.HasPrefix(repository, "git@github.com:"):
		path := strings.TrimPrefix(repository, "git@github.com:")
		owner, name = splitRepositoryPath(path)
	default:
		parsed, err := url.Parse(repository)
		if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", "", fmt.Errorf("%w: GitHub repository is invalid", publisher.ErrInvalidRequest)
		}
		switch parsed.Scheme {
		case "https":
			if parsed.User != nil || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.Port() != "" {
				return "", "", fmt.Errorf("%w: GitHub HTTPS repository is invalid", publisher.ErrInvalidRequest)
			}
		case "ssh":
			if parsed.User == nil || parsed.User.Username() != "git" || parsed.User.String() != "git" ||
				!strings.EqualFold(parsed.Hostname(), "github.com") || parsed.Port() != "" {
				return "", "", fmt.Errorf("%w: GitHub SSH repository is invalid", publisher.ErrInvalidRequest)
			}
		default:
			return "", "", fmt.Errorf("%w: unsupported GitHub repository scheme", publisher.ErrInvalidRequest)
		}
		owner, name = splitRepositoryPath(strings.TrimPrefix(parsed.EscapedPath(), "/"))
		if decodedOwner, err := url.PathUnescape(owner); err == nil {
			owner = decodedOwner
		}
		if decodedName, err := url.PathUnescape(name); err == nil {
			name = decodedName
		}
	}
	name = strings.TrimSuffix(name, ".git")
	if !validRepositoryComponent(owner) || !validRepositoryComponent(name) {
		return "", "", fmt.Errorf("%w: GitHub repository owner or name is invalid", publisher.ErrInvalidRequest)
	}
	return owner, name, nil
}

func splitRepositoryPath(path string) (string, string) {
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func validRepositoryComponent(value string) bool {
	return value != "" && value != "." && value != ".." && repositoryComponent.MatchString(value)
}

func escapeQuery(value string) string {
	escaped := url.QueryEscape(value)
	escaped = strings.ReplaceAll(escaped, "%3A", ":")
	escaped = strings.ReplaceAll(escaped, "%3a", ":")
	return escaped
}

func readBounded(reader io.Reader, limit int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

func sanitizeDiagnostic(value, token string) string {
	value = strings.ReplaceAll(value, token, "[REDACTED]")
	value = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\t' || character >= 0x20 {
			return character
		}
		return '?'
	}, value)
	value = strings.TrimSpace(value)
	if value == "" {
		return "no diagnostic"
	}
	return value
}

func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() || strings.EqualFold(host, "localhost")
}

type retryableProviderError struct{ error }

func (retryableProviderError) Retryable() bool { return true }
