package gitpr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/araihu/paje/internal/executil"
	"github.com/araihu/paje/internal/publisher"
)

const gitDiagnosticLimit int64 = 4096

type gitClient struct {
	command string
	baseEnv map[string]string
}

type gitResult struct {
	output   string
	exitCode int
	err      error
}

func newGitClient() (*gitClient, error) {
	command, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("create Git PR publisher: locate git: %w", err)
	}
	environment := map[string]string{
		"PATH":                     os.Getenv("PATH"),
		"LC_ALL":                   "C",
		"LANG":                     "C",
		"GIT_CONFIG_NOSYSTEM":      "1",
		"GIT_CONFIG_GLOBAL":        "/dev/null",
		"GIT_ATTR_NOSYSTEM":        "1",
		"GIT_TERMINAL_PROMPT":      "0",
		"GIT_OPTIONAL_LOCKS":       "0",
		"GIT_PROTOCOL_FROM_USER":   "0",
		"GIT_ALLOW_PROTOCOL":       "file:https",
		"GIT_CONFIG_SYSTEM":        "/dev/null",
		"GIT_ASKPASS_REQUIRE":      "force",
		"GIT_SSH_COMMAND":          "false",
		"GIT_HTTP_LOW_SPEED_LIMIT": "1",
		"GIT_HTTP_LOW_SPEED_TIME":  "30",
	}
	for _, key := range []string{
		"HOME", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR",
	} {
		if value, ok := os.LookupEnv(key); ok && !strings.ContainsRune(value, '\x00') {
			environment[key] = value
		}
	}
	return &gitClient{command: command, baseEnv: environment}, nil
}

func (g *gitClient) remoteBranch(
	ctx context.Context,
	workspace, pushURL, branch string,
	credentials map[string]string,
) (string, bool, error) {
	ref := "refs/heads/" + branch
	result := g.run(ctx, workspace, credentials, "ls-remote", "--heads", "--exit-code", "--", pushURL, ref)
	if result.exitCode == 2 {
		return "", false, nil
	}
	if result.err != nil {
		return "", false, result.err
	}
	lines := strings.Split(strings.TrimSpace(result.output), "\n")
	if len(lines) != 1 {
		return "", false, fmt.Errorf("unexpected ls-remote response")
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || fields[1] != ref || !gitObjectPattern.MatchString(fields[0]) {
		return "", false, fmt.Errorf("invalid ls-remote response")
	}
	return fields[0], true, nil
}

func (g *gitClient) commit(ctx context.Context, workspace, message string) (string, error) {
	environment := map[string]string{
		"GIT_AUTHOR_DATE":    "2000-01-01T00:00:00+00:00",
		"GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00",
	}
	result := g.run(
		ctx, workspace, environment,
		"-c", "user.name=Pajé",
		"-c", "user.email=paje@invalid",
		"-c", "core.hooksPath=/dev/null",
		"commit", "-m", message,
	)
	if result.err != nil {
		return "", result.err
	}
	commit := g.run(ctx, workspace, nil, "rev-parse", "HEAD")
	if commit.err != nil {
		return "", commit.err
	}
	sha := strings.TrimSpace(commit.output)
	if !gitObjectPattern.MatchString(sha) {
		return "", fmt.Errorf("created commit has invalid SHA")
	}
	return sha, nil
}

func (g *gitClient) push(
	ctx context.Context,
	workspace, pushURL, branch string,
	credentials map[string]string,
) error {
	result := g.run(
		ctx, workspace, credentials,
		"-c", "core.hooksPath=/dev/null",
		"push", pushURL, "HEAD:refs/heads/"+branch,
	)
	return result.err
}

func (g *gitClient) verifyRemoteCommit(
	ctx context.Context,
	workspace, pushURL, branch, commitSHA string,
	req publisher.Request,
	treeSHA string,
	credentials map[string]string,
) error {
	verificationRef := "refs/paje/verify/" + req.RunID
	fetch := g.run(
		ctx, workspace, credentials,
		"-c", "core.hooksPath=/dev/null",
		"fetch", "--no-tags", "--force", pushURL,
		"refs/heads/"+branch+":"+verificationRef,
	)
	if fetch.err != nil {
		return markRetryable(fmt.Errorf("fetch existing publication branch: %w", fetch.err))
	}
	fetched := g.run(ctx, workspace, nil, "rev-parse", verificationRef+"^{commit}")
	if fetched.err != nil {
		return fmt.Errorf("%w: inspect existing publication commit: %v", publisher.ErrConflict, fetched.err)
	}
	if strings.TrimSpace(fetched.output) != commitSHA {
		return fmt.Errorf("%w: publication branch changed during inspection", publisher.ErrConflict)
	}
	return g.verifyLocalCommit(ctx, workspace, commitSHA, req, treeSHA)
}

func (g *gitClient) verifyLocalCommit(
	ctx context.Context,
	workspace, commitSHA string,
	req publisher.Request,
	treeSHA string,
) error {
	parent := g.run(ctx, workspace, nil, "show", "-s", "--format=%P", commitSHA)
	if parent.err != nil {
		return fmt.Errorf("%w: inspect publication parent: %v", publisher.ErrConflict, parent.err)
	}
	parents := strings.Fields(parent.output)
	if len(parents) != 1 || parents[0] != req.BaseSHA {
		return fmt.Errorf("%w: publication commit parent does not match base SHA", publisher.ErrConflict)
	}
	tree := g.run(ctx, workspace, nil, "show", "-s", "--format=%T", commitSHA)
	if tree.err != nil {
		return fmt.Errorf("%w: inspect publication tree: %v", publisher.ErrConflict, tree.err)
	}
	if strings.TrimSpace(tree.output) != treeSHA {
		return fmt.Errorf("%w: publication commit tree does not match artifact", publisher.ErrConflict)
	}
	message := g.run(ctx, workspace, nil, "show", "-s", "--format=%B", commitSHA)
	if message.err != nil {
		return fmt.Errorf("%w: inspect publication message: %v", publisher.ErrConflict, message.err)
	}
	if strings.TrimRight(message.output, "\r\n") != commitMessage(req) {
		return fmt.Errorf("%w: publication commit trailers do not match immutable request", publisher.ErrConflict)
	}
	return nil
}

func (g *gitClient) run(
	ctx context.Context,
	workspace string,
	extraEnvironment map[string]string,
	args ...string,
) gitResult {
	if err := ctx.Err(); err != nil {
		return gitResult{exitCode: -1, err: err}
	}
	output, err := executil.NewLimitedBuffer(gitDiagnosticLimit)
	if err != nil {
		return gitResult{exitCode: -1, err: err}
	}
	command := exec.CommandContext(ctx, g.command, append([]string{"-C", workspace}, args...)...)
	executil.Configure(command)
	command.Dir = workspace
	command.Env = gitEnvironment(g.baseEnv, extraEnvironment)
	command.Stdout = output
	command.Stderr = output
	runErr := command.Run()
	result := gitResult{output: string(output.Bytes()), exitCode: 0}
	if runErr == nil {
		if output.Truncated() {
			result.err = errors.New("Git output exceeded diagnostic limit")
		}
		return result
	}
	var exitError *exec.ExitError
	if errors.As(runErr, &exitError) {
		result.exitCode = exitError.ExitCode()
	} else {
		result.exitCode = -1
	}
	diagnostic := redactGitDiagnostic(result.output, extraEnvironment)
	if output.Truncated() {
		diagnostic += " [truncated]"
	}
	if diagnostic == "" {
		diagnostic = "no diagnostic"
	}
	result.err = fmt.Errorf("git %s failed (exit %s): %s", safeGitOperation(args), strconv.Itoa(result.exitCode), diagnostic)
	return result
}

func gitEnvironment(base, extra map[string]string) []string {
	merged := make(map[string]string, len(base)+len(extra))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+merged[key])
	}
	return environment
}

func redactGitDiagnostic(diagnostic string, environment map[string]string) string {
	for _, key := range []string{"PAJE_GIT_PASSWORD"} {
		if secret := environment[key]; secret != "" {
			diagnostic = strings.ReplaceAll(diagnostic, secret, "[REDACTED]")
			for length := len(secret) - 1; length >= 4; length-- {
				if strings.HasSuffix(diagnostic, secret[:length]) {
					diagnostic = strings.TrimSuffix(diagnostic, secret[:length]) + "[REDACTED]"
					break
				}
			}
		}
	}
	diagnostic = strings.TrimSpace(diagnostic)
	if len(diagnostic) > int(gitDiagnosticLimit) {
		diagnostic = diagnostic[:gitDiagnosticLimit]
	}
	return diagnostic
}

func safeGitOperation(args []string) string {
	for _, argument := range args {
		switch argument {
		case "ls-remote", "fetch", "push", "commit", "rev-parse", "show":
			return argument
		}
	}
	return "operation"
}
