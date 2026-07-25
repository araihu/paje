package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const askpassScript = `#!/bin/sh
case "$1" in
  *Username*|*username*) printf '%s\n' "$PAJE_GIT_USERNAME" ;;
  *) printf '%s\n' "$PAJE_GIT_PASSWORD" ;;
esac
`

// Credentials creates independent, private askpass sessions.
type Credentials struct {
	root  string
	token string
}

// NewCredentials constructs a temporary askpass credential provider.
func NewCredentials(runtimeRoot, token string) (*Credentials, error) {
	runtimeRoot = strings.TrimSpace(runtimeRoot)
	if runtimeRoot == "" {
		return nil, fmt.Errorf("create GitHub credentials: runtime root is required")
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\x00\r\n") {
		return nil, fmt.Errorf("create GitHub credentials: token is invalid")
	}
	absolute, err := filepath.Abs(runtimeRoot)
	if err != nil {
		return nil, fmt.Errorf("create GitHub credentials: resolve runtime root: %w", err)
	}
	if info, err := os.Lstat(absolute); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("create GitHub credentials: runtime root must be a non-symlink directory")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("create GitHub credentials: runtime root must be private")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("create GitHub credentials: inspect runtime root: %w", err)
	} else if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create GitHub credentials: create runtime root: %w", err)
	}
	if err := validateCredentialDirectory(absolute); err != nil {
		return nil, fmt.Errorf("create GitHub credentials: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("create GitHub credentials: canonicalize runtime root: %w", err)
	}
	return &Credentials{root: canonical, token: token}, nil
}

// Prepare creates one askpass helper and returns its exact Git environment.
func (c *Credentials) Prepare(ctx context.Context) (map[string]string, func(context.Context) error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := validateCredentialDirectory(c.root); err != nil {
		return nil, nil, fmt.Errorf("prepare GitHub credentials: %w", err)
	}
	session, err := os.MkdirTemp(c.root, "askpass-")
	if err != nil {
		return nil, nil, fmt.Errorf("prepare GitHub credentials: create session: %w", err)
	}
	if err := os.Chmod(session, 0o700); err != nil {
		_ = os.Remove(session)
		return nil, nil, fmt.Errorf("prepare GitHub credentials: secure session: %w", err)
	}
	helper := filepath.Join(session, "git-askpass")
	file, err := os.OpenFile(helper, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		_ = os.Remove(session)
		return nil, nil, fmt.Errorf("prepare GitHub credentials: create helper: %w", err)
	}
	writeErr := writeAndSync(file, []byte(askpassScript))
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(helper)
		_ = os.Remove(session)
		return nil, nil, fmt.Errorf("prepare GitHub credentials: write helper: %w", err)
	}
	if err := validateCredentialHelper(helper); err != nil {
		_ = os.Remove(helper)
		_ = os.Remove(session)
		return nil, nil, fmt.Errorf("prepare GitHub credentials: %w", err)
	}

	environment := map[string]string{
		"GIT_ASKPASS":         helper,
		"GIT_TERMINAL_PROMPT": "0",
		"PAJE_GIT_USERNAME":   "x-access-token",
		"PAJE_GIT_PASSWORD":   c.token,
	}
	var once sync.Once
	var cleanupErr error
	cleanup := func(_ context.Context) error {
		once.Do(func() {
			cleanupErr = cleanupCredentialSession(c.root, session, helper)
			environment["PAJE_GIT_PASSWORD"] = ""
		})
		return cleanupErr
	}
	return environment, cleanup, nil
}

func writeAndSync(file *os.File, contents []byte) error {
	if _, err := file.Write(contents); err != nil {
		return err
	}
	return file.Sync()
}

func cleanupCredentialSession(root, session, helper string) error {
	relative, err := filepath.Rel(root, session)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.Dir(relative) != "." {
		return fmt.Errorf("cleanup GitHub credentials: session escapes runtime root")
	}
	info, err := os.Lstat(session)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cleanup GitHub credentials: inspect session: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("cleanup GitHub credentials: unsafe session path")
	}
	var failures []error
	if err := os.Remove(helper); err != nil && !errors.Is(err, os.ErrNotExist) {
		failures = append(failures, fmt.Errorf("remove helper: %w", err))
	}
	if err := os.Remove(session); err != nil && !errors.Is(err, os.ErrNotExist) {
		failures = append(failures, fmt.Errorf("remove session: %w", err))
	}
	return errors.Join(failures...)
}

func validateCredentialDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("runtime root must be a non-symlink directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("runtime root must be private")
	}
	return nil
}

func validateCredentialHelper(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o700 || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("askpass helper must be a regular 0700 file")
	}
	return nil
}
