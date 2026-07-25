package gitpr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/araihu/paje/internal/executil"
	"github.com/araihu/paje/internal/publisher"
)

const gitDiagnosticLimit int64 = 4096

const trustedRepositoryConfig = `[core]
	repositoryformatversion = 0
	filemode = true
	bare = true
	logallrefupdates = false
	hooksPath = /dev/null
[credential]
	helper =
[http]
	proxy =
`

const trustedRepositoryHead = "ref: refs/heads/paje-candidate\n"

type gitClient struct {
	command string
	baseEnv map[string]string
}

type gitResult struct {
	output   string
	exitCode int
	err      error
}

type trustedRepository struct {
	root   string
	path   string
	parent string
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
	repository *trustedRepository,
	pushURL, branch string,
	credentials map[string]string,
) (string, bool, error) {
	if err := repository.validate(); err != nil {
		return "", false, err
	}
	ref := "refs/heads/" + branch
	result := g.run(ctx, repository.path, credentials, trustedRemoteArguments(
		"ls-remote", "--heads", "--exit-code", "--", pushURL, ref,
	)...)
	postValidation := repository.validate()
	if result.exitCode == 2 {
		return "", false, postValidation
	}
	if result.err != nil {
		return "", false, errors.Join(result.err, postValidation)
	}
	if postValidation != nil {
		return "", false, postValidation
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
		"-c", "commit.gpgSign=false",
		"-c", "core.fsmonitor=false",
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
	repository *trustedRepository,
	pushURL, branch string,
	credentials map[string]string,
) error {
	if err := repository.validate(); err != nil {
		return err
	}
	result := g.run(
		ctx, repository.path, credentials,
		trustedRemoteArguments("push", pushURL, "HEAD:refs/heads/"+branch)...,
	)
	return errors.Join(result.err, repository.validate())
}

func (g *gitClient) verifyRemoteCommit(
	ctx context.Context,
	repository *trustedRepository,
	pushURL, branch, commitSHA string,
	req publisher.Request,
	treeSHA string,
	credentials map[string]string,
) error {
	if err := repository.validate(); err != nil {
		return err
	}
	verificationRef := "refs/paje/verify/" + req.RunID
	fetch := g.run(
		ctx, repository.path, credentials,
		trustedRemoteArguments(
			"fetch", "--no-tags", "--force", pushURL,
			"refs/heads/"+branch+":"+verificationRef,
		)...,
	)
	if fetch.err != nil {
		return retryRemoteError(errors.Join(
			fmt.Errorf("fetch existing publication branch: %w", fetch.err),
			repository.validate(),
		))
	}
	if err := repository.validate(); err != nil {
		return err
	}
	fetched := g.run(ctx, repository.path, nil, "rev-parse", verificationRef+"^{commit}")
	if fetched.err != nil {
		return conflictUnlessCanceled("inspect existing publication commit", fetched.err)
	}
	if strings.TrimSpace(fetched.output) != commitSHA {
		return fmt.Errorf("%w: publication branch changed during inspection", publisher.ErrConflict)
	}
	return g.verifyLocalCommit(ctx, repository.path, commitSHA, req, treeSHA)
}

func (g *gitClient) importTrusted(
	ctx context.Context,
	source string,
	expectedCommit string,
) (_ *trustedRepository, returnErr error) {
	if !filepath.IsAbs(source) || !gitObjectPattern.MatchString(expectedCommit) {
		return nil, fmt.Errorf("create trusted publication repository: invalid import")
	}
	root, err := os.MkdirTemp("", "paje-publisher-")
	if err != nil {
		return nil, fmt.Errorf("create trusted publication repository: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("create trusted publication repository: canonicalize root: %w", err)
	}
	repository := &trustedRepository{
		root: canonicalRoot, path: filepath.Join(canonicalRoot, "repository.git"),
		parent: filepath.Dir(canonicalRoot),
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, repository.Cleanup(context.WithoutCancel(ctx)))
		}
	}()
	canonicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil, fmt.Errorf("create trusted publication repository: canonicalize source: %w", err)
	}
	if containsFilesystemPath(canonicalSource, canonicalRoot) {
		return nil, fmt.Errorf("create trusted publication repository: trusted root is inside source workspace")
	}
	if err := os.Chmod(repository.root, 0o700); err != nil {
		return nil, fmt.Errorf("create trusted publication repository: secure root: %w", err)
	}
	if err := os.Mkdir(repository.path, 0o700); err != nil {
		return nil, fmt.Errorf("create trusted publication repository: create repository: %w", err)
	}
	if result := g.run(ctx, repository.path, nil, "init", "--bare"); result.err != nil {
		return nil, fmt.Errorf("create trusted publication repository: initialize: %w", result.err)
	}
	if err := os.Chmod(repository.path, 0o700); err != nil {
		return nil, fmt.Errorf("create trusted publication repository: secure repository: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(repository.path, "hooks")); err != nil {
		return nil, fmt.Errorf("create trusted publication repository: remove hooks: %w", err)
	}
	if err := writePrivateFile(filepath.Join(repository.path, "config"), []byte(trustedRepositoryConfig)); err != nil {
		return nil, fmt.Errorf("create trusted publication repository: write config: %w", err)
	}
	if err := writePrivateFile(filepath.Join(repository.path, "HEAD"), []byte(trustedRepositoryHead)); err != nil {
		return nil, fmt.Errorf("create trusted publication repository: write HEAD: %w", err)
	}
	if err := repository.validate(); err != nil {
		return nil, err
	}
	importResult := g.run(
		ctx, repository.path, nil,
		trustedRemoteArguments(
			"fetch", "--no-tags", "--force", "--", source,
			"HEAD:refs/heads/paje-candidate",
		)...,
	)
	if importResult.err != nil {
		return nil, errors.Join(importResult.err, repository.validate())
	}
	if err := repository.validate(); err != nil {
		return nil, err
	}
	imported := g.run(ctx, repository.path, nil, "rev-parse", "HEAD^{commit}")
	if imported.err != nil {
		return nil, imported.err
	}
	if strings.TrimSpace(imported.output) != expectedCommit {
		return nil, fmt.Errorf("%w: trusted import commit does not match candidate", publisher.ErrConflict)
	}
	return repository, nil
}

func containsFilesystemPath(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (g *gitClient) validateEffectiveURL(
	ctx context.Context,
	repository *trustedRepository,
	pushURL string,
) error {
	if err := repository.validate(); err != nil {
		return err
	}
	result := g.run(
		ctx, repository.path, nil,
		trustedRemoteArguments("ls-remote", "--get-url", "--", pushURL)...,
	)
	if result.err != nil {
		return fmt.Errorf("validate publication URL: %w", result.err)
	}
	if strings.TrimSpace(result.output) != pushURL {
		return fmt.Errorf("%w: effective publication URL was rewritten", publisher.ErrConflict)
	}
	return repository.validate()
}

func (g *gitClient) verifyLocalCommit(
	ctx context.Context,
	workspace, commitSHA string,
	req publisher.Request,
	treeSHA string,
) error {
	parent := g.run(ctx, workspace, nil, "show", "-s", "--format=%P", commitSHA)
	if parent.err != nil {
		return conflictUnlessCanceled("inspect publication parent", parent.err)
	}
	parents := strings.Fields(parent.output)
	if len(parents) != 1 || parents[0] != req.BaseSHA {
		return fmt.Errorf("%w: publication commit parent does not match base SHA", publisher.ErrConflict)
	}
	tree := g.run(ctx, workspace, nil, "show", "-s", "--format=%T", commitSHA)
	if tree.err != nil {
		return conflictUnlessCanceled("inspect publication tree", tree.err)
	}
	if strings.TrimSpace(tree.output) != treeSHA {
		return fmt.Errorf("%w: publication commit tree does not match artifact", publisher.ErrConflict)
	}
	message := g.run(ctx, workspace, nil, "show", "-s", "--format=%B", commitSHA)
	if message.err != nil {
		return conflictUnlessCanceled("inspect publication message", message.err)
	}
	if strings.TrimRight(message.output, "\r\n") != commitMessage(req) {
		return fmt.Errorf("%w: publication commit trailers do not match immutable request", publisher.ErrConflict)
	}
	return nil
}

func conflictUnlessCanceled(operation string, err error) error {
	if cancellationError(err) {
		return err
	}
	return fmt.Errorf("%w: %s: %v", publisher.ErrConflict, operation, err)
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
	result.err = errors.Join(
		ctx.Err(),
		fmt.Errorf("git %s failed (exit %s): %s", safeGitOperation(args), strconv.Itoa(result.exitCode), diagnostic),
	)
	return result
}

func trustedRemoteArguments(arguments ...string) []string {
	return append([]string{
		"-c", "credential.helper=",
		"-c", "http.proxy=",
		"-c", "core.hooksPath=/dev/null",
	}, arguments...)
}

func writePrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		file.Close()
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func (r *trustedRepository) validate() error {
	if r == nil || r.root == "" || r.path != filepath.Join(r.root, "repository.git") ||
		filepath.Dir(r.root) != r.parent || !strings.HasPrefix(filepath.Base(r.root), "paje-publisher-") {
		return fmt.Errorf("validate trusted publication repository: invalid identity")
	}
	for _, path := range []string{r.root, r.path} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("validate trusted publication repository: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("validate trusted publication repository: unsafe directory")
		}
	}
	for path, expected := range map[string][]byte{
		filepath.Join(r.path, "config"): []byte(trustedRepositoryConfig),
		filepath.Join(r.path, "HEAD"):   []byte(trustedRepositoryHead),
	} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("validate trusted publication repository: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return fmt.Errorf("validate trusted publication repository: unsafe control file")
		}
		contents, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(contents, expected) {
			return fmt.Errorf("validate trusted publication repository: control file changed")
		}
	}
	return nil
}

func (r *trustedRepository) Cleanup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || r.root == "" || filepath.Dir(r.root) != r.parent ||
		!strings.HasPrefix(filepath.Base(r.root), "paje-publisher-") {
		return fmt.Errorf("cleanup trusted publication repository: invalid identity")
	}
	info, err := os.Lstat(r.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cleanup trusted publication repository: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("cleanup trusted publication repository: unsafe root")
	}
	return os.RemoveAll(r.root)
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
