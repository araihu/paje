package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const commandTestToken = "paje_v1_codex01.ERERERERERERERERERERERERERERERERERERERERERE"

func TestCapabilitiesNeedsNoCredential(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := runCommand([]string{"capabilities"}, func(string) string { return "" }, bytes.NewReader(nil), &stdout, &stderr)
	if exit != 0 || stderr.Len() != 0 {
		t.Fatalf("exit/stderr = %d / %q", exit, stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result["leaf_submission"] != true || result["control_plane"] != false {
		t.Fatalf("capabilities = %s", stdout.String())
	}
}

func TestSubmitReadsStdinAndUsesTokenFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+commandTestToken {
			t.Errorf("missing narrow bearer token")
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"api_version":"v1","run_id":"paje_run","status":"accepted","reused":false,"depth":0,"root_run_id":"paje_run"}`))
	}))
	defer server.Close()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenPath, []byte(commandTestToken), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"PAJE_AGENT_URL": server.URL, "PAJE_AGENT_TOKEN_FILE": tokenPath, "CODEX_THREAD_ID": "thread-1",
	}
	input := []byte(`{"task_description":"update docs","repository_uri":"https://github.com/araihu/paje.git","base_ref":"main","tags":{"user_id":"guilhermecastro","app_id":"paje"},"worker_profile":"codex-go@1","profile":"go","publication":{"mode":"artifact"}}`)
	var stdout, stderr bytes.Buffer
	exit := runCommand([]string{"submit", "--file", "-"}, func(key string) string { return values[key] }, bytes.NewReader(input), &stdout, &stderr)
	if exit != 0 || stderr.Len() != 0 {
		t.Fatalf("exit/stderr = %d / %q", exit, stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result["run_id"] != "paje_run" {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestUnsupportedCommandDoesNotReadCredential(t *testing.T) {
	readCredential := false
	var stdout, stderr bytes.Buffer
	exit := runCommand([]string{"unknown"}, func(string) string {
		readCredential = true
		return ""
	}, bytes.NewReader(nil), &stdout, &stderr)
	if exit != exitInvalidInput || readCredential {
		t.Fatalf("exit/readCredential = %d / %t", exit, readCredential)
	}
}

func TestCapabilitiesReportsEncodingFailure(t *testing.T) {
	var stderr bytes.Buffer
	exit := runCommand([]string{"capabilities"}, func(string) string { return "" }, bytes.NewReader(nil), errorWriter{}, &stderr)
	if exit != exitInternal || !strings.Contains(stderr.String(), "encode result") {
		t.Fatalf("exit/stderr = %d / %q", exit, stderr.String())
	}
}

func TestReadInputRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readInput(path, bytes.NewReader(nil)); err == nil {
		t.Fatal("readInput accepted oversized file")
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestConfiguredClientDefaultsToOwnerLocalToken(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(home, ".local", "share", "paje", "agent", "token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(commandTestToken), 0o600); err != nil {
		t.Fatal(err)
	}

	client, err := configuredClient(func(key string) string {
		if key == "HOME" {
			return home
		}
		return ""
	})
	if err != nil || client == nil {
		t.Fatalf("configuredClient() = %v, %v", client, err)
	}
}
