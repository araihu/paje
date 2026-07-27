package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/sandboxinit"
)

func TestRunDocumentUsesSupervisorReceiptAndPreservesChildExitStatus(t *testing.T) {
	document := sandboxinit.Document{
		WorkspaceRoot: executor.SandboxWorkspaceRoot,
		Command: executor.Command{
			Executable: "codex", Args: []string{"exec"}, Directory: executor.SandboxWorkspaceRoot,
		},
		Environment: map[string]string{"PATH": executor.CanonicalSandboxPATH},
	}
	attempt := executor.AttemptID{
		RunID: "run-main-supervisor", Stage: "execute", Attempt: 1,
		StartedAt: time.Unix(100, 1).UTC(), Purpose: executor.PurposeAgent,
	}
	if err := document.BindChildStartReceipt(attempt, strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var supervised bool
	ops := operations{
		readFile: func(string, int64) ([]byte, error) { return slices.Clone(encoded), nil },
		remove:   func(string) error { return nil },
		chdir:    func(string) error { return nil },
		resolve:  func(string, string, map[string]string) (string, error) { return "/usr/bin/codex", nil },
		supervise: func(config sandboxinit.SuperviseConfig) (int, error) {
			supervised = true
			if config.Executable != "/usr/bin/codex" || !config.Receipt.Matches(document.ChildStartReceipt) {
				t.Fatalf("supervisor config = %#v", config)
			}
			return 37, nil
		},
	}
	err = runDocument(sandboxinit.DocumentPath, ops)
	var exitErr *childExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 37 || !supervised {
		t.Fatalf("runDocument() = %v, supervised=%v", err, supervised)
	}
}

func TestConfirmChildExecAcceptsSuccessfullyExecedFastChild(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestConfirmChildExecFastChildHelper$")
	command.Env = append(os.Environ(), "GO_WANT_CONFIRM_EXEC_FAST_CHILD=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := confirmChildExec(pid); err != nil {
		t.Fatalf("successfully execed fast child was rejected: %v", err)
	}
}

func TestConfirmChildExecFastChildHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CONFIRM_EXEC_FAST_CHILD") == "1" {
		os.Exit(0)
	}
}

func TestRunEmitsPrivateChildStartReceiptWithoutExternalUtility(t *testing.T) {
	receipt := []byte(`{"version":"receipt"}`)
	var emitted []byte
	ops := operations{
		readFile: func(path string, limit int64) ([]byte, error) {
			if path != sandboxinit.ChildStartReceiptPath || limit != sandboxinit.MaxDocumentBytes {
				t.Fatalf("receipt read = %q limit %d", path, limit)
			}
			return slices.Clone(receipt), nil
		},
		writeAll: func(value []byte) error {
			emitted = slices.Clone(value)
			return nil
		},
	}
	if err := run([]string{"--emit-child-start-receipt"}, bytes.NewReader(nil), ops); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(emitted, receipt) {
		t.Fatalf("emitted receipt = %q, want %q", emitted, receipt)
	}
}

func TestRunBootstrapExtractsLiveArchiveBeforeExecutingDocument(t *testing.T) {
	document := sandboxinit.Document{
		WorkspaceRoot: "/workspace",
		Command:       executor.Command{Executable: "codex", Directory: "/workspace"},
		Environment:   map[string]string{"PATH": executor.CanonicalSandboxPATH},
		EnvironmentFiles: map[string]string{
			"CODEX_TOKEN": "/run/paje/secrets/token",
		},
	}
	bindTestDocument(t, &document)
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
	ops.supervise = func(sandboxinit.SuperviseConfig) (int, error) { return 0, execReturned }
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
		Environment:      map[string]string{"PATH": executor.CanonicalSandboxPATH, "BASE": "present"},
		EnvironmentFiles: map[string]string{"CODEX_TOKEN": "/run/paje/secrets/codex-token"},
	}
	bindTestDocument(t, &document)
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
			if name != "codex" || dir != "/workspace/site" || values["PATH"] != executor.CanonicalSandboxPATH {
				t.Fatalf("resolve = %q %q %#v", name, dir, values)
			}
			return "/usr/bin/codex", nil
		},
		supervise: func(config sandboxinit.SuperviseConfig) (int, error) {
			executable, argv, environment = config.Executable, slices.Clone(config.Arguments), slices.Clone(config.Environment)
			if !config.Receipt.Matches(document.ChildStartReceipt) {
				t.Fatalf("receipt = %#v", config.Receipt)
			}
			return 0, execReturned
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
	wantEnvironment := []string{"BASE=present", "CODEX_TOKEN=secret-value", "GOWORK=off", "PATH=" + executor.CanonicalSandboxPATH}
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
		readFile:  func(string, int64) ([]byte, error) { return []byte(`{"unknown":true}`), nil },
		remove:    func(string) error { removed = true; return nil },
		supervise: func(sandboxinit.SuperviseConfig) (int, error) { executed = true; return 0, nil },
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
				Environment:   map[string]string{"PATH": executor.CanonicalSandboxPATH},
				EnvironmentFiles: map[string]string{
					"CODEX_TOKEN": "/run/paje/secrets/codex-token",
				},
			}
			bindTestDocument(t, &document)
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

func bindTestDocument(t *testing.T, document *sandboxinit.Document) {
	t.Helper()
	attempt := executor.AttemptID{
		RunID: "run-test-document", Stage: "execute", Attempt: 1,
		StartedAt: time.Unix(100, 1).UTC(), Purpose: executor.PurposeAgent,
	}
	if err := document.BindChildStartReceipt(attempt, strings.Repeat("e", 64)); err != nil {
		t.Fatal(err)
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
