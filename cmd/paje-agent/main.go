// Command paje-agent gives Codex a bounded client for Pajé leaf workflows.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/araihu/paje/internal/agentclient"
)

const (
	exitOK             = 0
	exitInvalidInput   = 2
	exitAuthentication = 3
	exitForbidden      = 4
	exitConflict       = 5
	exitUnavailable    = 6
	exitTimeout        = 7
	exitCanceled       = 8
	exitWorkflowFailed = 9
	exitInternal       = 10
)

func main() {
	os.Exit(runCommand(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

func runCommand(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return diagnostic(stderr, exitInvalidInput, "command is required")
	}
	if args[0] == "capabilities" {
		if len(args) != 1 {
			return diagnostic(stderr, exitInvalidInput, "capabilities accepts no arguments")
		}
		if err := json.NewEncoder(stdout).Encode(map[string]bool{"leaf_submission": true, "control_plane": false}); err != nil {
			return diagnostic(stderr, exitInternal, "encode result")
		}
		return exitOK
	}
	if args[0] != "submit" && args[0] != "status" && args[0] != "cancel" && args[0] != "wait" {
		return diagnostic(stderr, exitInvalidInput, "unsupported command")
	}
	client, err := configuredClient(getenv)
	if err != nil {
		return diagnostic(stderr, classify(err), err.Error())
	}
	ctx := context.Background()
	var result agentclient.View
	switch args[0] {
	case "submit":
		flags := flag.NewFlagSet("submit", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		path := flags.String("file", "-", "request JSON file or - for stdin")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
			return diagnostic(stderr, exitInvalidInput, "invalid submit arguments")
		}
		raw, readErr := readInput(*path, stdin)
		if readErr != nil {
			return diagnostic(stderr, exitInvalidInput, readErr.Error())
		}
		result, err = client.Submit(ctx, getenv("CODEX_THREAD_ID"), raw)
	case "status", "cancel", "wait":
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		runID := flags.String("run", "", "Pajé run ID")
		timeout := flags.Duration("timeout", 30*time.Minute, "wait timeout")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || strings.TrimSpace(*runID) == "" {
			return diagnostic(stderr, exitInvalidInput, "invalid "+args[0]+" arguments")
		}
		switch args[0] {
		case "status":
			result, err = client.Status(ctx, *runID)
		case "cancel":
			result, err = client.Cancel(ctx, *runID)
		case "wait":
			if *timeout <= 0 || *timeout > 24*time.Hour {
				return diagnostic(stderr, exitInvalidInput, "wait timeout must be positive and at most 24h")
			}
			waitCtx, cancel := context.WithTimeout(ctx, *timeout)
			defer cancel()
			result, err = client.Wait(waitCtx, *runID)
		}
	}
	if err != nil {
		return diagnostic(stderr, classify(err), err.Error())
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return diagnostic(stderr, exitInternal, "encode result")
	}
	if args[0] == "wait" && result.Status != "succeeded" {
		return exitWorkflowFailed
	}
	return exitOK
}

func configuredClient(getenv func(string) string) (*agentclient.Client, error) {
	baseURL := strings.TrimSpace(getenv("PAJE_AGENT_URL"))
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8787"
	}
	tokenPath := strings.TrimSpace(getenv("PAJE_AGENT_TOKEN_FILE"))
	if tokenPath == "" {
		home := strings.TrimSpace(getenv("HOME"))
		if !filepath.IsAbs(home) || filepath.Clean(home) != home {
			return nil, errors.New("PAJE_AGENT_TOKEN_FILE or an absolute HOME is required")
		}
		tokenPath = filepath.Join(home, ".local", "share", "paje", "agent", "token")
	}
	token, err := agentclient.ReadTokenFile(tokenPath)
	if err != nil {
		return nil, err
	}
	return agentclient.New(agentclient.Config{BaseURL: baseURL, Token: token})
}

func readInput(path string, stdin io.Reader) (json.RawMessage, error) {
	reader := stdin
	var file *os.File
	if path == "-" {
		// Keep the caller-provided reader.
	} else {
		if !strings.HasPrefix(path, "/") {
			return nil, errors.New("request file must be absolute or -")
		}
		var err error
		file, err = os.Open(path)
		if err != nil {
			return nil, errors.New("request JSON is unreadable or too large")
		}
		defer file.Close()
		reader = file
	}
	raw, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil || len(raw) == 0 || len(raw) > 1<<20 {
		return nil, errors.New("request JSON is unreadable or too large")
	}
	return json.RawMessage(raw), nil
}

func classify(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return exitTimeout
	}
	if errors.Is(err, context.Canceled) {
		return exitCanceled
	}
	var apiError *agentclient.APIError
	if errors.As(err, &apiError) {
		switch apiError.StatusCode {
		case 401:
			return exitAuthentication
		case 403:
			return exitForbidden
		case 409, 422:
			return exitConflict
		case 503:
			return exitUnavailable
		default:
			return exitInternal
		}
	}
	if strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "required") ||
		strings.Contains(err.Error(), "permits") || strings.Contains(err.Error(), "client-managed") {
		return exitInvalidInput
	}
	return exitInternal
}

func diagnostic(writer io.Writer, code int, message string) int {
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\n", " "), "\r", " ")
	if len(message) > 512 {
		message = message[:512]
	}
	_, _ = fmt.Fprintf(writer, "paje-agent: %s\n", message)
	return code
}
