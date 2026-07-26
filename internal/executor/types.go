// Package executor defines provider-neutral one-shot sandbox execution.
package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
)

type Purpose string

const (
	PurposeProbe        Purpose = "probe"
	PurposeAgent        Purpose = "agent"
	PurposeVerification Purpose = "verification"
)

// AttemptID identifies exactly one one-shot sandbox. Every field participates
// in Key so adapters can rediscover only resources owned by this attempt.
type AttemptID struct {
	RunID     string    `json:"run_id"`
	Stage     string    `json:"stage"`
	Attempt   int       `json:"attempt"`
	StartedAt time.Time `json:"started_at"`
	Purpose   Purpose   `json:"purpose"`
	Sequence  int       `json:"sequence"`
}

// Key returns a provider-neutral collision-resistant identity suitable for
// deterministic adapter labels. Invalid identities have no key.
func (id AttemptID) Key() string {
	if err := id.Validate(); err != nil {
		return ""
	}
	identity := id.RunID + "\x00" + id.Stage + "\x00" + strconv.Itoa(id.Attempt) + "\x00" +
		id.StartedAt.UTC().Format(time.RFC3339Nano) + "\x00" + string(id.Purpose) + "\x00" + strconv.Itoa(id.Sequence)
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

// Command is one exact shell-free command in the sandbox filesystem view.
type Command struct {
	Executable  string            `json:"executable"`
	Args        []string          `json:"args,omitempty"`
	Directory   string            `json:"directory"`
	Environment map[string]string `json:"environment,omitempty"`
}

func (command Command) Clone() Command {
	command.Args = slices.Clone(command.Args)
	command.Environment = cloneMap(command.Environment)
	return command
}

// Workspace maps a transient host directory to an absolute sandbox path.
// HostPath is never safe durable evidence.
type Workspace struct {
	HostPath    string `json:"-"`
	SandboxPath string `json:"sandbox_path"`
	Writable    bool   `json:"writable"`
}

// Request contains validated domain values and transient secret bytes. It
// must never be serialized or persisted.
type Request struct {
	Attempt     AttemptID
	Profile     workerprofile.Snapshot
	Command     Command
	Workspace   Workspace
	Environment map[string]string
	Secrets     []secret.Materialization
	Timeout     time.Duration
	OutputLimit int64
}

func (request Request) Clone() Request {
	clone := request
	clone.Profile = request.Profile.Clone()
	clone.Command = request.Command.Clone()
	clone.Environment = cloneMap(request.Environment)
	if request.Secrets != nil {
		clone.Secrets = make([]secret.Materialization, len(request.Secrets))
		for index := range request.Secrets {
			clone.Secrets[index] = request.Secrets[index].Clone()
		}
	}
	return clone
}

// Destroy zeroes and releases the caller-owned transient secret copies.
func (request *Request) Destroy() {
	if request == nil {
		return
	}
	for index := range request.Secrets {
		request.Secrets[index].Destroy()
	}
	request.Secrets = nil
}

func (Request) MarshalJSON() ([]byte, error) { return nil, secret.ErrSecretSerialization }
func (Request) MarshalText() ([]byte, error) { return nil, secret.ErrSecretSerialization }
func (Request) String() string               { return "[executor request]" }
func (Request) GoString() string             { return "[executor request]" }
func (Request) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[executor request]"))
}

// Result contains bounded, provider-neutral execution evidence.
type Result struct {
	Created         bool              `json:"created"`
	Started         bool              `json:"started"`
	Completed       bool              `json:"completed"`
	ExitCode        int               `json:"exit_code"`
	Stdout          []byte            `json:"stdout,omitempty"`
	Stderr          []byte            `json:"stderr,omitempty"`
	Duration        time.Duration     `json:"duration"`
	StdoutTruncated bool              `json:"stdout_truncated"`
	StderrTruncated bool              `json:"stderr_truncated"`
	SecretDetected  bool              `json:"secret_detected"`
	SafeFacts       map[string]string `json:"safe_facts,omitempty"`
}

func (result Result) MarshalJSON() ([]byte, error) {
	type resultJSON Result
	safe := resultJSON(result)
	if result.SecretDetected {
		safe.Stdout = nil
		safe.Stderr = nil
	}
	return json.Marshal(safe)
}

func (result Result) String() string {
	encoded, err := result.MarshalJSON()
	if err != nil {
		return "[executor result]"
	}
	return string(encoded)
}

func (result Result) GoString() string { return result.String() }

func (result Result) Format(state fmt.State, verb rune) {
	formatted := result.String()
	if verb == 'q' {
		formatted = strconv.Quote(formatted)
	}
	_, _ = state.Write([]byte(formatted))
}

func (result Result) Clone() Result {
	result.Stdout = slices.Clone(result.Stdout)
	result.Stderr = slices.Clone(result.Stderr)
	result.SafeFacts = cloneMap(result.SafeFacts)
	return result
}

// Destroy zeroes and releases the caller-owned transient command output.
func (result *Result) Destroy() {
	if result == nil {
		return
	}
	clear(result.Stdout)
	clear(result.Stderr)
	result.Stdout = nil
	result.Stderr = nil
}

type State string

const (
	StateAbsent    State = "absent"
	StateCreated   State = "created"
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateDestroyed State = "destroyed"
	StateUnknown   State = "unknown"
)

func StableStates() []State {
	return []State{StateAbsent, StateCreated, StateRunning, StateCompleted, StateDestroyed, StateUnknown}
}

func (state State) Validate() error {
	switch state {
	case StateAbsent, StateCreated, StateRunning, StateCompleted, StateDestroyed, StateUnknown:
		return nil
	default:
		return errors.New("invalid executor lifecycle state")
	}
}

type Executor interface {
	Execute(context.Context, Request) (Result, error)
	Inspect(context.Context, AttemptID) (State, error)
	Cancel(context.Context, AttemptID) error
	Destroy(context.Context, AttemptID) error
}

var ErrAttemptExists = errors.New("executor attempt identity already exists")

// ProviderError preserves the trusted local cause through Unwrap while its
// error text and JSON projection expose only stable diagnostics.
type ProviderError struct {
	Class     string `json:"class"`
	CauseCode string `json:"cause_code"`
	err       error
}

func WrapError(class, causeCode string, err error) error {
	if err == nil {
		return nil
	}
	if !validErrorClass(class) || !safeIdentifier(causeCode, 63) {
		class = "internal"
		causeCode = "provider_error"
	}
	return &ProviderError{Class: class, CauseCode: causeCode, err: err}
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "executor failure"
	}
	return fmt.Sprintf("executor %s failure (%s)", e.Class, e.CauseCode)
}

func (e *ProviderError) String() string   { return e.Error() }
func (e *ProviderError) GoString() string { return e.Error() }
func (e *ProviderError) Format(state fmt.State, verb rune) {
	formatted := e.Error()
	if verb == 'q' {
		formatted = strconv.Quote(formatted)
	}
	_, _ = state.Write([]byte(formatted))
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *ProviderError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Class     string `json:"class"`
		CauseCode string `json:"cause_code"`
	}{Class: e.Class, CauseCode: e.CauseCode})
}

func cloneMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}
