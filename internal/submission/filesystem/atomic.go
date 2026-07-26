package filesystem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

func atomicWriteJSON(ctx context.Context, target string, value any) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	directory := filepath.Dir(target)
	if err := requireSafeDirectory(directory); err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("atomic JSON target is a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(target)+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	open := true
	defer func() {
		if open {
			returnErr = errors.Join(returnErr, temporary.Close())
		}
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, removeErr)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	open = false
	if err := ctx.Err(); err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("atomic JSON target became a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func readCanonicalJSON(path string, destination any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return errors.New("JSON file is not a private regular file")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON file contains trailing data")
		}
		return err
	}
	canonical, err := json.Marshal(destination)
	if err != nil {
		return err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(encoded, canonical) {
		return errors.New("JSON file is not canonical")
	}
	return nil
}

func requireSafeDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("directory is unsafe")
	}
	return nil
}

func requirePrivateDirectory(path string) error {
	if err := requireSafeDirectory(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("directory is not private")
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
