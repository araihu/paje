// Package gitcapture captures and reproduces Git changes without staging a
// capture worktree's real index.
package gitcapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/executil"
)

var (
	// ErrInvalidRequest indicates an unsafe or incomplete capture/apply request.
	ErrInvalidRequest = errors.New("invalid git capture request")
	// ErrPatchTooLarge indicates a patch or Git metadata stream exceeded its bound.
	ErrPatchTooLarge = errors.New("git patch exceeds configured limit")
	// ErrTreeMismatch indicates an applied patch did not produce the recorded tree.
	ErrTreeMismatch = errors.New("applied patch tree does not match expected tree")
	// ErrDirtyIndex indicates the capture worktree already had staged changes.
	ErrDirtyIndex = errors.New("capture worktree index is dirty")
	// ErrIndexChanged indicates capture unexpectedly changed the real worktree index.
	ErrIndexChanged = errors.New("capture changed worktree index")
)

const diagnosticLimit int64 = 4096

var shaPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Request identifies a prepared worktree and the immutable commit it was based on.
type Request struct {
	Workspace string
	BaseSHA   string
	MaxBytes  int64
}

// Result is the complete, reproducible change set rooted at BaseSHA.
type Result struct {
	Patch   []byte
	Changes []artifact.Change
	TreeSHA string
}

// ApplyRequest supplies an independently captured artifact to reproduce.
type ApplyRequest struct {
	Workspace       string
	BaseSHA         string
	Patch           []byte
	ExpectedTreeSHA string
}

// Capturer is the port used by the code-change workflow.
type Capturer interface {
	Capture(context.Context, Request) (Result, error)
	Apply(context.Context, ApplyRequest) error
}

// Git is a shell-free Git implementation of Capturer.
type Git struct {
	command string
	env     []string
}

var _ Capturer = (*Git)(nil)

// New locates Git once so all invocations use a known executable.
func New() (*Git, error) {
	operatorPath := os.Getenv("PATH")
	command, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("locate git: %w", err)
	}
	return &Git{command: command, env: []string{
		"PATH=" + operatorPath,
		"LC_ALL=C",
		"LANG=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
	}}, nil
}

// Capture builds a binary patch and manifest from a private temporary index.
func (g *Git) Capture(ctx context.Context, request Request) (result Result, returnErr error) {
	workspace, base, err := validateCaptureRequest(request)
	if err != nil {
		return Result{}, err
	}
	realIndex, err := realIndexPath(workspace)
	if err != nil {
		return Result{}, fmt.Errorf("find worktree index: %w", err)
	}
	before, err := indexChecksum(realIndex)
	if err != nil {
		return Result{}, fmt.Errorf("checksum worktree index: %w", err)
	}
	if err := g.assertBaseAndCleanIndex(ctx, workspace, realIndex, base); err != nil {
		return Result{}, err
	}
	temporaryDirectory, cleanupDirectory, err := temporaryDirectory(workspace)
	if err != nil {
		return Result{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, cleanupDirectory(context.WithoutCancel(ctx))) }()
	temporaryIndex, err := temporaryFile(temporaryDirectory, "index")
	if err != nil {
		return Result{}, err
	}

	if _, truncated, err := g.run(ctx, workspace, temporaryIndex, request.MaxBytes, "read-tree", base); err != nil || truncated {
		return Result{}, boundedGitError(err, truncated)
	}
	if err := privateFile(temporaryIndex); err != nil {
		return Result{}, err
	}
	if _, truncated, err := g.run(ctx, workspace, temporaryIndex, request.MaxBytes, "add", "-A", "--", "."); err != nil || truncated {
		return Result{}, boundedGitError(err, truncated)
	}
	if err := privateFile(temporaryIndex); err != nil {
		return Result{}, err
	}

	patch, truncated, err := g.run(ctx, workspace, temporaryIndex, request.MaxBytes, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--find-renames", base, "--")
	if err != nil || truncated {
		return Result{}, boundedGitError(err, truncated)
	}
	rawChanges, truncated, err := g.run(ctx, workspace, temporaryIndex, request.MaxBytes, "diff", "--cached", "--raw", "-z", "--no-abbrev", "--find-renames", "--find-copies", base, "--")
	if err != nil || truncated {
		return Result{}, boundedGitError(err, truncated)
	}
	// These NUL-delimited Git views are deliberately captured as part of the
	// contract. Raw output supplies modes; name-status and stage output detect
	// malformed or unexpectedly large index views without relying on line parsing.
	nameStatus, truncated, err := g.run(ctx, workspace, temporaryIndex, request.MaxBytes, "diff", "--cached", "--name-status", "-z", "--find-renames", "--find-copies", base, "--")
	if err != nil || truncated {
		return Result{}, boundedGitError(err, truncated)
	}
	stages, truncated, err := g.run(ctx, workspace, temporaryIndex, request.MaxBytes, "ls-files", "--stage", "-z")
	if err != nil || truncated {
		return Result{}, boundedGitError(err, truncated)
	}
	changes, err := parseRawChanges(rawChanges)
	if err != nil {
		return Result{}, err
	}
	if err := crossValidateChanges(changes, nameStatus, stages); err != nil {
		return Result{}, err
	}
	tree, truncated, err := g.run(ctx, workspace, temporaryIndex, diagnosticLimit, "write-tree")
	if err != nil || truncated {
		return Result{}, boundedGitError(err, truncated)
	}
	treeSHA := strings.TrimSpace(string(tree))
	if !shaPattern.MatchString(treeSHA) {
		return Result{}, fmt.Errorf("%w: invalid captured tree", ErrInvalidRequest)
	}
	after, err := indexChecksum(realIndex)
	if err != nil {
		return Result{}, fmt.Errorf("checksum worktree index after capture: %w", err)
	}
	if before != after {
		return Result{}, ErrIndexChanged
	}
	return Result{Patch: patch, Changes: changes, TreeSHA: treeSHA}, nil
}

// Apply validates and applies a binary patch to the target worktree's real
// index, then independently reconstructs and verifies its tree.
func (g *Git) Apply(ctx context.Context, request ApplyRequest) (returnErr error) {
	workspace, base, expected, err := validateApplyRequest(request)
	if err != nil {
		return err
	}
	realIndex, err := realIndexPath(workspace)
	if err != nil {
		return fmt.Errorf("find worktree index: %w", err)
	}
	if err := g.assertBaseAndCleanIndex(ctx, workspace, realIndex, base); err != nil {
		return err
	}
	status, truncated, err := g.run(ctx, workspace, realIndex, diagnosticLimit, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil || truncated {
		return boundedGitError(err, truncated)
	}
	if len(status) != 0 {
		return ErrDirtyIndex
	}
	if err := validatePatchPaths(request.Patch); err != nil {
		return err
	}
	temporaryDirectory, cleanupDirectory, err := temporaryDirectory(workspace)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, cleanupDirectory(context.WithoutCancel(ctx))) }()
	patchPath, err := temporaryFile(temporaryDirectory, "patch")
	if err != nil {
		return err
	}
	if err := os.WriteFile(patchPath, request.Patch, 0o600); err != nil {
		return fmt.Errorf("write temporary patch: %w", err)
	}
	proofIndex, err := temporaryFile(temporaryDirectory, "proof-index")
	if err != nil {
		return err
	}
	if _, truncated, err := g.run(ctx, workspace, proofIndex, diagnosticLimit, "read-tree", base); err != nil || truncated {
		return boundedGitError(err, truncated)
	}
	if err := privateFile(proofIndex); err != nil {
		return err
	}
	if _, _, err := g.run(ctx, workspace, proofIndex, diagnosticLimit, "apply", "--cached", "--binary", "--whitespace=nowarn", patchPath); err != nil {
		return err
	}
	proofTree, truncated, err := g.run(ctx, workspace, proofIndex, diagnosticLimit, "write-tree")
	if err != nil || truncated {
		return boundedGitError(err, truncated)
	}
	if strings.TrimSpace(string(proofTree)) != expected {
		return ErrTreeMismatch
	}
	if _, _, err := g.run(ctx, workspace, realIndex, diagnosticLimit, "apply", "--index", "--binary", "--whitespace=nowarn", patchPath); err != nil {
		return err
	}

	stagedTree, truncated, err := g.run(ctx, workspace, realIndex, diagnosticLimit, "write-tree")
	if err != nil || truncated {
		return boundedGitError(err, truncated)
	}
	if strings.TrimSpace(string(stagedTree)) != expected {
		return ErrTreeMismatch
	}
	temporaryIndex, err := temporaryFile(temporaryDirectory, "verify-index")
	if err != nil {
		return err
	}
	if _, truncated, err := g.run(ctx, workspace, temporaryIndex, diagnosticLimit, "read-tree", base); err != nil || truncated {
		return boundedGitError(err, truncated)
	}
	if err := privateFile(temporaryIndex); err != nil {
		return err
	}
	if _, truncated, err := g.run(ctx, workspace, temporaryIndex, diagnosticLimit, "add", "-A", "--", "."); err != nil || truncated {
		return boundedGitError(err, truncated)
	}
	if err := privateFile(temporaryIndex); err != nil {
		return err
	}
	filesystemTree, truncated, err := g.run(ctx, workspace, temporaryIndex, diagnosticLimit, "write-tree")
	if err != nil || truncated {
		return boundedGitError(err, truncated)
	}
	if strings.TrimSpace(string(filesystemTree)) != expected {
		return ErrTreeMismatch
	}
	return nil
}

func validateCaptureRequest(request Request) (string, string, error) {
	workspace, err := validateWorkspace(request.Workspace)
	if err != nil {
		return "", "", err
	}
	base, err := validateSHA(request.BaseSHA)
	if err != nil {
		return "", "", err
	}
	if request.MaxBytes <= 0 {
		return "", "", fmt.Errorf("%w: maximum patch bytes must be positive", ErrInvalidRequest)
	}
	return workspace, base, nil
}

func validateApplyRequest(request ApplyRequest) (string, string, string, error) {
	workspace, err := validateWorkspace(request.Workspace)
	if err != nil {
		return "", "", "", err
	}
	base, err := validateSHA(request.BaseSHA)
	if err != nil {
		return "", "", "", err
	}
	expected, err := validateSHA(request.ExpectedTreeSHA)
	if err != nil {
		return "", "", "", err
	}
	if len(request.Patch) == 0 {
		return "", "", "", fmt.Errorf("%w: patch is empty", ErrInvalidRequest)
	}
	return workspace, base, expected, nil
}

func validateWorkspace(workspace string) (string, error) {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("%w: workspace must be an absolute path", ErrInvalidRequest)
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("%w: resolve workspace: %v", ErrInvalidRequest, err)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: workspace is not a directory", ErrInvalidRequest)
	}
	return resolved, nil
}

func validateSHA(value string) (string, error) {
	if !shaPattern.MatchString(value) {
		return "", fmt.Errorf("%w: expected a full hexadecimal object ID", ErrInvalidRequest)
	}
	return value, nil
}

func (g *Git) assertBaseAndCleanIndex(ctx context.Context, workspace, index, base string) error {
	if err := g.assertBase(ctx, workspace, index, base); err != nil {
		return err
	}
	_, _, err := g.run(ctx, workspace, index, diagnosticLimit, "diff", "--quiet", "--cached", "--")
	if err == nil {
		return nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return ErrDirtyIndex
	}
	return err
}

func (g *Git) assertBase(ctx context.Context, workspace, index, base string) error {
	topLevel, truncated, err := g.run(ctx, workspace, index, diagnosticLimit, "rev-parse", "--show-toplevel")
	if err != nil || truncated || filepath.Clean(strings.TrimSpace(string(topLevel))) != workspace {
		return fmt.Errorf("%w: workspace is not Git toplevel", ErrInvalidRequest)
	}
	head, truncated, err := g.run(ctx, workspace, index, diagnosticLimit, "rev-parse", "HEAD")
	if err != nil || truncated {
		return boundedGitError(err, truncated)
	}
	if strings.TrimSpace(string(head)) != base {
		return fmt.Errorf("%w: worktree HEAD does not match base SHA", ErrInvalidRequest)
	}
	commit, truncated, err := g.run(ctx, workspace, index, diagnosticLimit, "rev-parse", "--verify", base+"^{commit}")
	if err != nil || truncated {
		return boundedGitError(err, truncated)
	}
	if strings.TrimSpace(string(commit)) != base {
		return fmt.Errorf("%w: base SHA is not a commit", ErrInvalidRequest)
	}
	return nil
}

func (g *Git) run(ctx context.Context, workspace, index string, limit int64, args ...string) ([]byte, bool, error) {
	stdout, err := executil.NewLimitedBuffer(limit)
	if err != nil {
		return nil, false, err
	}
	stderr, err := executil.NewLimitedBuffer(diagnosticLimit)
	if err != nil {
		return nil, false, err
	}
	command := exec.CommandContext(ctx, g.command, args...)
	executil.Configure(command)
	command.Dir = workspace
	command.Env = append(append([]string(nil), g.env...), "GIT_INDEX_FILE="+index)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		diagnostic := strings.TrimSpace(string(stderr.Bytes()))
		if diagnostic == "" {
			return nil, stdout.Truncated(), fmt.Errorf("git %s: %w", args[0], err)
		}
		return nil, stdout.Truncated(), fmt.Errorf("git %s: %w: %s", args[0], err, diagnostic)
	}
	return stdout.Bytes(), stdout.Truncated(), nil
}

func boundedGitError(err error, truncated bool) error {
	if truncated {
		return ErrPatchTooLarge
	}
	return err
}

func temporaryDirectory(workspace string) (string, func(context.Context) error, error) {
	directory, err := os.MkdirTemp(filepath.Dir(workspace), ".paje-git-")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary git directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return "", nil, fmt.Errorf("secure temporary git directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || pathContains(workspace, resolved) {
		_ = os.RemoveAll(directory)
		return "", nil, fmt.Errorf("%w: temporary directory is not outside workspace", ErrInvalidRequest)
	}
	return resolved, func(ctx context.Context) error {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if cleanupContext.Err() != nil {
			return cleanupContext.Err()
		}
		if err := os.RemoveAll(resolved); err != nil {
			return err
		}
		return cleanupContext.Err()
	}, nil
}

func temporaryFile(directory, name string) (string, error) {
	file, err := os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return "", fmt.Errorf("create temporary git file: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("secure temporary git file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close temporary git file: %w", err)
	}
	return path, nil
}

func privateFile(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure temporary git file: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect temporary git file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("secure temporary git file: unexpected mode %o", info.Mode().Perm())
	}
	return nil
}

func realIndexPath(workspace string) (string, error) {
	gitPath := filepath.Join(workspace, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: .git must not be a symlink", ErrInvalidRequest)
	}
	if info.IsDir() {
		return validateIndexPath(filepath.Join(gitPath, "index"))
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: unsupported .git entry", ErrInvalidRequest)
	}
	contents, err := os.ReadFile(gitPath)
	if err != nil {
		return "", err
	}
	const prefix = "gitdir: "
	location := strings.TrimSpace(string(contents))
	if !strings.HasPrefix(location, prefix) {
		return "", fmt.Errorf("%w: invalid .git file", ErrInvalidRequest)
	}
	gitDirectory := strings.TrimPrefix(location, prefix)
	if !filepath.IsAbs(gitDirectory) {
		gitDirectory = filepath.Join(workspace, gitDirectory)
	}
	gitDirectory, err = filepath.EvalSymlinks(gitDirectory)
	if err != nil {
		return "", fmt.Errorf("%w: resolve linked gitdir: %v", ErrInvalidRequest, err)
	}
	info, err = os.Stat(gitDirectory)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: invalid linked gitdir", ErrInvalidRequest)
	}
	backlink, err := os.ReadFile(filepath.Join(gitDirectory, "gitdir"))
	if err != nil {
		return "", fmt.Errorf("%w: linked worktree backlink: %v", ErrInvalidRequest, err)
	}
	backlinkPath := strings.TrimSpace(string(backlink))
	if !filepath.IsAbs(backlinkPath) {
		backlinkPath = filepath.Join(gitDirectory, backlinkPath)
	}
	if filepath.Clean(backlinkPath) != filepath.Join(workspace, ".git") {
		return "", fmt.Errorf("%w: linked worktree backlink mismatch", ErrInvalidRequest)
	}
	return validateIndexPath(filepath.Join(gitDirectory, "index"))
}

func validateIndexPath(index string) (string, error) {
	info, err := os.Lstat(index)
	if errors.Is(err, os.ErrNotExist) {
		return index, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || !indexOwnerSafe(info) {
		return "", fmt.Errorf("%w: unsafe worktree index", ErrInvalidRequest)
	}
	return index, nil
}

func indexChecksum(index string) (string, error) {
	contents, err := os.ReadFile(index)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]), nil
}

func parseRawChanges(raw []byte) ([]artifact.Change, error) {
	fields := strings.Split(string(raw), "\x00")
	var changes []artifact.Change
	for position := 0; position < len(fields)-1; {
		header := fields[position]
		position++
		parts := strings.Fields(header)
		if len(parts) != 5 || !strings.HasPrefix(parts[0], ":") {
			return nil, fmt.Errorf("%w: malformed Git raw change", ErrInvalidRequest)
		}
		status := parts[4]
		if status == "" {
			return nil, fmt.Errorf("%w: missing Git change status", ErrInvalidRequest)
		}
		kind := status[:1]
		if position >= len(fields) {
			return nil, fmt.Errorf("%w: missing Git change path", ErrInvalidRequest)
		}
		first, err := normalizeGitPath(fields[position])
		if err != nil {
			return nil, err
		}
		position++
		change := artifact.Change{Status: kind, OldMode: parts[0][1:], NewMode: parts[1]}
		if !safeMode(change.OldMode) || !safeMode(change.NewMode) {
			return nil, fmt.Errorf("%w: unsupported Git file mode", ErrInvalidRequest)
		}
		if kind == "R" || kind == "C" {
			if position >= len(fields) {
				return nil, fmt.Errorf("%w: missing renamed Git path", ErrInvalidRequest)
			}
			second, err := normalizeGitPath(fields[position])
			if err != nil {
				return nil, err
			}
			position++
			change.OldPath, change.Path = first, second
		} else {
			change.Path = first
		}
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		if changes[i].OldPath != changes[j].OldPath {
			return changes[i].OldPath < changes[j].OldPath
		}
		return changes[i].Status < changes[j].Status
	})
	return changes, nil
}

func normalizeGitPath(value string) (string, error) {
	if value == "" || !utf8.ValidString(value) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("%w: unsafe Git path", ErrInvalidRequest)
	}
	normalized := path.Clean(value)
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", fmt.Errorf("%w: unsafe Git path", ErrInvalidRequest)
	}
	return normalized, nil
}

func safeMode(mode string) bool {
	switch mode {
	case "000000", "100644", "100755", "120000", "160000":
		return true
	default:
		return false
	}
}

type namedChange struct {
	status, oldPath, path string
}

func crossValidateChanges(changes []artifact.Change, nameStatus, stages []byte) error {
	names, err := parseNameStatus(nameStatus)
	if err != nil {
		return err
	}
	if len(names) != len(changes) {
		return fmt.Errorf("%w: name-status does not match raw changes", ErrInvalidRequest)
	}
	for index, change := range changes {
		name := names[index]
		if name.status != change.Status || name.path != change.Path || name.oldPath != change.OldPath {
			return fmt.Errorf("%w: name-status does not match raw changes", ErrInvalidRequest)
		}
	}
	indexed, err := parseStages(stages)
	if err != nil {
		return err
	}
	for _, change := range changes {
		if change.NewMode == "000000" {
			if _, exists := indexed[change.Path]; exists {
				return fmt.Errorf("%w: deleted path remains staged", ErrInvalidRequest)
			}
			continue
		}
		if indexed[change.Path] != change.NewMode {
			return fmt.Errorf("%w: staged mode does not match raw change", ErrInvalidRequest)
		}
	}
	return nil
}

func parseNameStatus(raw []byte) ([]namedChange, error) {
	fields := bytes.Split(raw, []byte{'\x00'})
	changes := make([]namedChange, 0)
	for position := 0; position < len(fields)-1; {
		status := string(fields[position])
		position++
		if status == "" || (status[0] != 'A' && status[0] != 'M' && status[0] != 'D' && status[0] != 'R' && status[0] != 'C' && status[0] != 'T') || position >= len(fields) {
			return nil, fmt.Errorf("%w: malformed name-status stream", ErrInvalidRequest)
		}
		first, err := normalizeGitPath(string(fields[position]))
		if err != nil {
			return nil, err
		}
		position++
		change := namedChange{status: status[:1]}
		if change.status == "R" || change.status == "C" {
			if position >= len(fields) {
				return nil, fmt.Errorf("%w: malformed rename name-status stream", ErrInvalidRequest)
			}
			second, err := normalizeGitPath(string(fields[position]))
			if err != nil {
				return nil, err
			}
			position++
			change.oldPath, change.path = first, second
		} else {
			change.path = first
		}
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].path != changes[j].path {
			return changes[i].path < changes[j].path
		}
		return changes[i].oldPath < changes[j].oldPath
	})
	return changes, nil
}

func parseStages(raw []byte) (map[string]string, error) {
	stages := make(map[string]string)
	for _, record := range bytes.Split(raw, []byte{'\x00'}) {
		if len(record) == 0 {
			continue
		}
		metadata, rawPath, found := bytes.Cut(record, []byte{'\t'})
		if !found {
			return nil, fmt.Errorf("%w: malformed ls-files stream", ErrInvalidRequest)
		}
		fields := strings.Fields(string(metadata))
		if len(fields) != 3 || fields[2] != "0" || !safeMode(fields[0]) {
			return nil, fmt.Errorf("%w: malformed ls-files stage", ErrInvalidRequest)
		}
		path, err := normalizeGitPath(string(rawPath))
		if err != nil {
			return nil, err
		}
		if _, exists := stages[path]; exists {
			return nil, fmt.Errorf("%w: duplicate staged path", ErrInvalidRequest)
		}
		stages[path] = fields[0]
	}
	return stages, nil
}

func validatePatchPaths(patch []byte) error {
	found := false
	for _, line := range bytes.Split(patch, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("diff --git ")) {
			continue
		}
		found = true
		paths, err := splitDiffHeader(string(line[len("diff --git "):]))
		if err != nil || len(paths) != 2 {
			return fmt.Errorf("%w: malformed patch path", ErrInvalidRequest)
		}
		for _, candidate := range paths {
			if len(candidate) < 3 || (candidate[:2] != "a/" && candidate[:2] != "b/") {
				return fmt.Errorf("%w: malformed patch path", ErrInvalidRequest)
			}
			if _, err := normalizeGitPath(candidate[2:]); err != nil {
				return err
			}
		}
	}
	if !found {
		return fmt.Errorf("%w: patch has no file header", ErrInvalidRequest)
	}
	return nil
}

func splitDiffHeader(value string) ([]string, error) {
	var paths []string
	for value != "" {
		value = strings.TrimLeft(value, " ")
		if value == "" {
			break
		}
		if value[0] != '"' {
			end := strings.IndexByte(value, ' ')
			if end < 0 {
				paths = append(paths, value)
				break
			}
			paths, value = append(paths, value[:end]), value[end+1:]
			continue
		}
		end, escaped := 1, false
		for ; end < len(value); end++ {
			if value[end] == '"' && !escaped {
				break
			}
			escaped = value[end] == '\\' && !escaped
			if value[end] != '\\' {
				escaped = false
			}
		}
		if end == len(value) {
			return nil, errors.New("unterminated quoted Git path")
		}
		decoded, err := strconv.Unquote(value[:end+1])
		if err != nil {
			return nil, err
		}
		paths, value = append(paths, decoded), value[end+1:]
	}
	return paths, nil
}

func pathContains(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
