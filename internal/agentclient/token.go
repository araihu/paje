package agentclient

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxTokenFileBytes = 512

func ReadTokenFile(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("read Pajé agent token: path must be a clean absolute path")
	}
	parent := filepath.Dir(path)
	canonicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil || canonicalParent != parent {
		return "", errors.New("read Pajé agent token: parent path must not contain symlinks")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 {
		return "", errors.New("read Pajé agent token: file must be regular and mode 0600")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("read Pajé agent token: open failed")
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm() != 0o600 {
		return "", errors.New("read Pajé agent token: file identity changed")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxTokenFileBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maxTokenFileBytes || strings.Contains(string(raw), "\r") {
		return "", errors.New("read Pajé agent token: contents are invalid")
	}
	value := string(raw)
	if strings.HasSuffix(value, "\n") {
		value = strings.TrimSuffix(value, "\n")
	}
	if strings.Contains(value, "\n") || !validToken(value) {
		return "", errors.New("read Pajé agent token: contents are invalid")
	}
	return value, nil
}
