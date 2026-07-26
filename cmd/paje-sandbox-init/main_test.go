package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/sandboxinit"
)

func TestRunBootstrapExtractsLiveArchiveBeforeExecutingDocument(t *testing.T) {
	document := sandboxinit.Document{
		WorkspaceRoot: "/workspace",
		Command:       executor.Command{Executable: "codex", Directory: "/workspace"},
		Environment:   map[string]string{"PATH": "/usr/bin:/bin"},
		EnvironmentFiles: map[string]string{
			"CODEX_TOKEN": "/run/paje/secrets/token",
		},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for name, value := range map[string][]byte{
		"run/paje/command.json":  encoded,
		"run/paje/secrets/token": []byte("secret"),
	} {
		if err := writer.WriteHeader(&tar.Header{
			Name: name, Mode: 0o400, Size: int64(len(value)), Typeflag: tar.TypeReg,
			Uid: sandboxinit.BootstrapUID, Gid: sandboxinit.BootstrapGID,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	execReturned := errors.New("exec returned")
	ops := realOperations()
	ops.resolve = func(string, string, map[string]string) (string, error) {
		return "/usr/bin/codex", nil
	}
	ops.chdir = func(string) error { return nil }
	ops.exec = func(string, []string, []string) error { return execReturned }
	ops.readFile = func(name string, limit int64) ([]byte, error) {
		if name == sandboxinit.DocumentPath {
			name = filepath.Join(root, "run", "paje", "command.json")
		} else if name == sandboxinit.SecretRoot+"/token" {
			name = filepath.Join(root, "run", "paje", "secrets", "token")
		}
		return readBoundedRegularFile(name, limit)
	}
	ops.remove = func(name string) error {
		if name == sandboxinit.DocumentPath {
			name = filepath.Join(root, "run", "paje", "command.json")
		} else if name == sandboxinit.SecretRoot+"/token" {
			name = filepath.Join(root, "run", "paje", "secrets", "token")
		}
		return os.Remove(name)
	}
	if err := runBootstrap(bytes.NewReader(archive.Bytes()), root, ops); !errors.Is(err, execReturned) {
		t.Fatalf("runBootstrap() = %v", err)
	}
	for _, name := range []string{
		filepath.Join(root, "run", "paje", "command.json"),
		filepath.Join(root, "run", "paje", "secrets", "token"),
	} {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private bootstrap material remains at %s: %v", name, err)
		}
	}
}

func TestRunRejectsUnknownSandboxInitMode(t *testing.T) {
	if err := run([]string{"--unknown"}, io.NopCloser(bytes.NewReader(nil)), realOperations()); err == nil {
		t.Fatal("unknown sandbox-init mode succeeded")
	}
}

func TestRunDocumentExecutesExactArgvAndEnvironmentThenRemovesMaterial(t *testing.T) {
	document := sandboxinit.Document{
		WorkspaceRoot: "/workspace",
		Command: executor.Command{
			Executable: "codex", Args: []string{"exec", "$(touch /tmp/pwn)"}, Directory: "/workspace/site",
			Environment: map[string]string{"GOWORK": "off"},
		},
		Environment:      map[string]string{"PATH": "/usr/bin:/bin", "BASE": "present"},
		EnvironmentFiles: map[string]string{"CODEX_TOKEN": "/run/paje/secrets/codex-token"},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	execReturned := errors.New("exec returned")
	files := map[string][]byte{
		sandboxinit.DocumentPath:        encoded,
		"/run/paje/secrets/codex-token": []byte("secret-value"),
	}
	var removed []string
	var directory, executable string
	var argv, environment []string
	ops := operations{
		readFile: func(path string, _ int64) ([]byte, error) { return slices.Clone(files[path]), nil },
		remove:   func(path string) error { removed = append(removed, path); return nil },
		chdir:    func(path string) error { directory = path; return nil },
		resolve: func(name, dir string, values map[string]string) (string, error) {
			if name != "codex" || dir != "/workspace/site" || values["PATH"] != "/usr/bin:/bin" {
				t.Fatalf("resolve = %q %q %#v", name, dir, values)
			}
			return "/usr/bin/codex", nil
		},
		exec: func(path string, args, env []string) error {
			executable, argv, environment = path, slices.Clone(args), slices.Clone(env)
			return execReturned
		},
	}
	if err := runDocument(sandboxinit.DocumentPath, ops); !errors.Is(err, execReturned) {
		t.Fatalf("runDocument() error = %v", err)
	}
	if directory != "/workspace/site" || executable != "/usr/bin/codex" {
		t.Fatalf("directory/executable = %q %q", directory, executable)
	}
	if !reflect.DeepEqual(argv, []string{"codex", "exec", "$(touch /tmp/pwn)"}) {
		t.Fatalf("argv = %#v", argv)
	}
	wantEnvironment := []string{"BASE=present", "CODEX_TOKEN=secret-value", "GOWORK=off", "PATH=/usr/bin:/bin"}
	if !reflect.DeepEqual(environment, wantEnvironment) {
		t.Fatalf("environment = %#v", environment)
	}
	if !reflect.DeepEqual(removed, []string{sandboxinit.DocumentPath, "/run/paje/secrets/codex-token"}) {
		t.Fatalf("removed = %#v", removed)
	}
}

func TestRunDocumentRemovesMalformedDocumentWithoutExec(t *testing.T) {
	removed := false
	executed := false
	ops := operations{
		readFile: func(string, int64) ([]byte, error) { return []byte(`{"unknown":true}`), nil },
		remove:   func(string) error { removed = true; return nil },
		exec:     func(string, []string, []string) error { executed = true; return nil },
	}
	if err := runDocument(sandboxinit.DocumentPath, ops); err == nil {
		t.Fatal("malformed document succeeded")
	}
	if !removed || executed {
		t.Fatalf("removed/executed = %v/%v", removed, executed)
	}
}

func TestRunDocumentRejectsNULPathsBeforeMaterialAccessOrChdir(t *testing.T) {
	for name, mutate := range map[string]func(*sandboxinit.Document){
		"directory": func(document *sandboxinit.Document) { document.Command.Directory = "/workspace/bad\x00directory" },
		"environment file": func(document *sandboxinit.Document) {
			document.EnvironmentFiles["CODEX_TOKEN"] = "/run/paje/secrets/bad\x00path"
		},
	} {
		t.Run(name, func(t *testing.T) {
			document := sandboxinit.Document{
				WorkspaceRoot: "/workspace",
				Command:       executor.Command{Executable: "codex", Directory: "/workspace"},
				Environment:   map[string]string{"PATH": "/usr/bin:/bin"},
				EnvironmentFiles: map[string]string{
					"CODEX_TOKEN": "/run/paje/secrets/codex-token",
				},
			}
			mutate(&document)
			encoded, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			changedDirectory := false
			ops := operations{
				readFile: func(string, int64) ([]byte, error) {
					reads++
					return slices.Clone(encoded), nil
				},
				remove: func(string) error { return nil },
				chdir:  func(string) error { changedDirectory = true; return nil },
			}
			if err := runDocument(sandboxinit.DocumentPath, ops); err == nil {
				t.Fatal("NUL path document succeeded")
			}
			if reads != 1 || changedDirectory {
				t.Fatalf("material reads/chdir = %d/%v, want 1/false", reads, changedDirectory)
			}
		})
	}
}

func TestReadBoundedRegularFileRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedRegularFile(link, 1024); err == nil {
		t.Fatal("symlink material file accepted")
	}
}
