package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/araihu/paje/internal/runner"
	"github.com/araihu/paje/internal/verification"
)

func requireOptIn(t *testing.T, key, testName string) {
	t.Helper()
	if os.Getenv(key) != "1" {
		t.Skipf("set %s=1 to run %s", key, testName)
	}
}

func requireEnvironment(t *testing.T, keys ...string) map[string]string {
	t.Helper()
	values := make(map[string]string, len(keys))
	missing := make([]string, 0)
	for _, key := range keys {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			missing = append(missing, key)
			continue
		}
		values[key] = value
	}
	if len(missing) != 0 {
		t.Skipf("set required acceptance variables: %s", strings.Join(missing, ", "))
	}
	return values
}

func existingCodexHome(t *testing.T) string {
	t.Helper()
	if configured := strings.TrimSpace(os.Getenv("CODEX_HOME")); configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("authenticated Codex acceptance requires CODEX_HOME or a user home directory")
	}
	codexHome := filepath.Join(home, ".codex")
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); err != nil {
		t.Skip("authenticated Codex acceptance requires an existing CODEX_HOME auth.json")
	}
	return codexHome
}

func baselineEnvironment() map[string]string {
	keys := []string{
		"PATH", "LANG", "LANGUAGE", "LC_ALL",
		"CURL_CA_BUNDLE", "AWS_CA_BUNDLE", "GIT_SSL_CAINFO", "NIX_SSL_CERT_FILE",
		"NODE_EXTRA_CA_CERTS", "NPM_CONFIG_CAFILE", "PIP_CERT", "REQUESTS_CA_BUNDLE",
		"SSL_CERT_DIR", "SSL_CERT_FILE",
	}
	result := make(map[string]string, len(keys)+4)
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			result[key] = value
		}
	}
	return result
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
}

func gitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output))
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("read directory %q: %v", path, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("directory %q retains entries %q", path, names)
	}
}

func assertNoProcessContains(t *testing.T, marker string) {
	t.Helper()
	output, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		t.Fatalf("inspect process table: %v", err)
	}
	if bytes.Contains(output, []byte(marker)) {
		t.Fatal("agent descendant remains after workflow cleanup")
	}
}

func assertNoProcessGroup(t *testing.T, processGroup int) {
	t.Helper()
	output, err := exec.Command("ps", "-axo", "pid=,pgid=,command=").Output()
	if err != nil {
		t.Fatalf("inspect process groups: %v", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		group, err := strconv.Atoi(fields[1])
		if err == nil && group == processGroup {
			t.Fatal("agent process-group descendant remains after workflow cleanup")
		}
	}
}

type recordingRunner struct {
	delegate runner.Runner
	mu       sync.Mutex
	requests []runner.RunRequest
}

func (r *recordingRunner) Run(ctx context.Context, request runner.RunRequest) (runner.ExecutionResult, error) {
	r.mu.Lock()
	r.requests = append(r.requests, cloneRunRequest(request))
	r.mu.Unlock()
	return r.delegate.Run(ctx, request)
}

func (r *recordingRunner) Requests() []runner.RunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]runner.RunRequest, len(r.requests))
	for index, request := range r.requests {
		result[index] = cloneRunRequest(request)
	}
	return result
}

func cloneRunRequest(request runner.RunRequest) runner.RunRequest {
	cloned := request
	cloned.Env = cloneMap(request.Env)
	return cloned
}

type recordingVerifier struct {
	delegate verification.Runner
	mu       sync.Mutex
	commands []verification.Command
	envs     []map[string]string
}

func (v *recordingVerifier) Run(ctx context.Context, command verification.Command, environment map[string]string) verification.Result {
	v.mu.Lock()
	v.commands = append(v.commands, cloneCommand(command))
	v.envs = append(v.envs, cloneMap(environment))
	v.mu.Unlock()
	return v.delegate.Run(ctx, command, environment)
}

func (v *recordingVerifier) Calls() ([]verification.Command, []map[string]string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	commands := make([]verification.Command, len(v.commands))
	for index, command := range v.commands {
		commands[index] = cloneCommand(command)
	}
	environments := make([]map[string]string, len(v.envs))
	for index, environment := range v.envs {
		environments[index] = cloneMap(environment)
	}
	return commands, environments
}

func cloneCommand(command verification.Command) verification.Command {
	cloned := command
	cloned.Args = append([]string(nil), command.Args...)
	cloned.Environment = cloneMap(command.Environment)
	return cloned
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type githubAPI struct {
	baseURL    string
	repository string
	token      string
	client     *http.Client
}

func (api githubAPI) get(t *testing.T, endpoint string, destination any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, strings.TrimRight(api.baseURL, "/")+endpoint, nil)
	if err != nil {
		t.Fatalf("create GitHub inspection request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+api.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := api.client.Do(request)
	if err != nil {
		t.Fatalf("GitHub inspection request failed (%T)", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		t.Fatalf("GitHub inspection request returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(destination); err != nil {
		t.Fatalf("decode GitHub inspection response: %v", err)
	}
}

type remotePublication struct {
	branchRef      string
	commitSHA      string
	commitMessage  string
	pullRequestID  string
	pullRequestURL string
	baseSHA        string
}

func (api githubAPI) inspectPublication(t *testing.T, branch, baseRef, baseSHA string) remotePublication {
	t.Helper()
	owner, name := splitGitHubRepository(t, api.repository)
	repositoryPath := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	branchPath := escapePathSegments(branch)

	var references []struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	api.get(t, repositoryPath+"/git/matching-refs/heads/"+branchPath, &references)
	if len(references) != 1 || references[0].Ref != "refs/heads/"+branch {
		t.Fatalf("matching publication branches = %d, want exactly one exact branch", len(references))
	}

	var commit struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	api.get(t, repositoryPath+"/commits/"+url.PathEscape(references[0].Object.SHA), &commit)
	if commit.SHA != references[0].Object.SHA || len(commit.Parents) != 1 || commit.Parents[0].SHA != baseSHA {
		t.Fatal("publication branch does not contain exactly one commit above the requested base")
	}

	query := url.Values{
		"state": {"all"},
		"head":  {owner + ":" + branch},
		"base":  {baseRef},
	}
	var pulls []struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Draft   bool   `json:"draft"`
		Head    struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	api.get(t, repositoryPath+"/pulls?"+query.Encode(), &pulls)
	if len(pulls) != 1 {
		t.Fatalf("matching pull requests = %d, want exactly one", len(pulls))
	}
	pull := pulls[0]
	if pull.ID <= 0 || pull.State != "open" || !pull.Draft || pull.Head.Ref != branch ||
		pull.Head.SHA != commit.SHA || pull.Base.Ref != baseRef {
		t.Fatal("publication pull request is not an exact open draft binding")
	}
	return remotePublication{
		branchRef: "refs/heads/" + branch, commitSHA: commit.SHA,
		commitMessage: commit.Commit.Message,
		pullRequestID: strconv.FormatInt(pull.ID, 10), pullRequestURL: pull.HTMLURL,
		baseSHA: baseSHA,
	}
}

func (api githubAPI) refSHA(t *testing.T, ref string) string {
	t.Helper()
	owner, name := splitGitHubRepository(t, api.repository)
	var response struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	api.get(t, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/git/ref/heads/"+escapePathSegments(ref), &response)
	if response.Ref != "refs/heads/"+ref || response.Object.SHA == "" {
		t.Fatal("GitHub base ref response is not exact")
	}
	return response.Object.SHA
}

func splitGitHubRepository(t *testing.T, repository string) (string, string) {
	t.Helper()
	parsed, err := url.Parse(repository)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") {
		t.Fatal("PAJE_GITHUB_TEST_REPOSITORY must be a github.com repository URL")
	}
	components := strings.Split(strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/"), "/")
	if len(components) != 2 || components[0] == "" || components[1] == "" {
		t.Fatal("PAJE_GITHUB_TEST_REPOSITORY must identify one owner and repository")
	}
	return components[0], components[1]
}

func escapePathSegments(value string) string {
	segments := strings.Split(value, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

func marshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode acceptance fixture: %v", err)
	}
	return encoded
}

func safeError(t *testing.T, operation string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s failed (%s)", operation, fmt.Sprintf("%T", err))
	}
}
