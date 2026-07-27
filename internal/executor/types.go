// Package executor defines provider-neutral one-shot sandbox execution.
package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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

// CanonicalCommand is the immutable, environment-free command identity bound
// into a child-start receipt. Environment declarations have their own digest
// so neither set can be silently inferred from the other.
type CanonicalCommand struct {
	Executable string   `json:"executable"`
	Args       []string `json:"args,omitempty"`
	Directory  string   `json:"directory"`
}

func (command Command) Clone() Command {
	command.Args = slices.Clone(command.Args)
	command.Environment = cloneMap(command.Environment)
	return command
}

// ChildStartReceipt is private executor evidence that the exact declared
// child passed the operating-system exec boundary. It is never inferred from
// resource creation, bootstrap start, or terminal provider state.
type ChildStartReceipt struct {
	Version           string           `json:"version"`
	Attempt           AttemptID        `json:"attempt"`
	Command           CanonicalCommand `json:"command"`
	CommandDigest     string           `json:"command_digest"`
	EnvironmentDigest string           `json:"environment_digest"`
	Challenge         string           `json:"challenge"`
}

const childStartReceiptVersion = "paje.araihu.com/child-start/v1"

// NewRandomChildStartReceipt creates a receipt with a cryptographically random
// private challenge. Adapters must expose it only after their declared child
// has crossed the operating-system exec boundary.
func NewRandomChildStartReceipt(
	attempt AttemptID,
	command Command,
	environment map[string]string,
	environmentFiles map[string]string,
) (ChildStartReceipt, error) {
	challengeBytes := make([]byte, sha256.Size)
	if _, err := rand.Read(challengeBytes); err != nil {
		return ChildStartReceipt{}, errors.New("generate child-start challenge")
	}
	challenge := hex.EncodeToString(challengeBytes)
	clear(challengeBytes)
	return NewChildStartReceipt(attempt, command, environment, environmentFiles, challenge)
}

// NewChildStartReceipt binds the full attempt, canonical command, complete
// non-secret environment declaration, and one exact private challenge.
func NewChildStartReceipt(
	attempt AttemptID,
	command Command,
	environment map[string]string,
	environmentFiles map[string]string,
	challenge string,
) (ChildStartReceipt, error) {
	if err := attempt.Validate(); err != nil {
		return ChildStartReceipt{}, err
	}
	if err := validateCommand(command, SandboxWorkspaceRoot); err != nil {
		return ChildStartReceipt{}, err
	}
	if err := validateEnvironment(environment); err != nil {
		return ChildStartReceipt{}, err
	}
	if err := validateEnvironment(command.Environment); err != nil {
		return ChildStartReceipt{}, err
	}
	for key := range command.Environment {
		if _, collision := environment[key]; collision {
			return ChildStartReceipt{}, errors.New("child-start environment declaration collides")
		}
	}
	if err := validateEnvironmentFileDeclarations(environment, command.Environment, environmentFiles); err != nil {
		return ChildStartReceipt{}, err
	}
	if !validDigest(challenge) {
		return ChildStartReceipt{}, errors.New("child-start challenge is invalid")
	}
	identity := CanonicalCommand{
		Executable: command.Executable,
		Args:       slices.Clone(command.Args),
		Directory:  command.Directory,
	}
	receipt := ChildStartReceipt{
		Version:       childStartReceiptVersion,
		Attempt:       attempt,
		Command:       identity,
		CommandDigest: digestCanonical(identity),
		EnvironmentDigest: digestCanonical(struct {
			Baseline map[string]string `json:"baseline"`
			Command  map[string]string `json:"command"`
			Files    map[string]string `json:"files"`
		}{
			Baseline: cloneMap(environment),
			Command:  cloneMap(command.Environment),
			Files:    cloneMap(environmentFiles),
		}),
		Challenge: challenge,
	}
	if err := receipt.Validate(); err != nil {
		return ChildStartReceipt{}, err
	}
	return receipt, nil
}

func (receipt ChildStartReceipt) Clone() ChildStartReceipt {
	receipt.Command.Args = slices.Clone(receipt.Command.Args)
	return receipt
}

func (receipt ChildStartReceipt) Validate() error {
	if receipt.Version != childStartReceiptVersion {
		return errors.New("child-start receipt version is invalid")
	}
	if err := receipt.Attempt.Validate(); err != nil {
		return err
	}
	command := Command{
		Executable: receipt.Command.Executable,
		Args:       slices.Clone(receipt.Command.Args),
		Directory:  receipt.Command.Directory,
	}
	if err := validateCommand(command, SandboxWorkspaceRoot); err != nil {
		return err
	}
	if receipt.CommandDigest != digestCanonical(receipt.Command) ||
		!validDigest(receipt.EnvironmentDigest) || !validDigest(receipt.Challenge) {
		return errors.New("child-start receipt binding is invalid")
	}
	return nil
}

// BindingDigest is a non-secret stable projection suitable for provider
// labels. Invalid receipts have no digest.
func (receipt ChildStartReceipt) BindingDigest() string {
	if err := receipt.Validate(); err != nil {
		return ""
	}
	return digestCanonical(receipt)
}

func (receipt ChildStartReceipt) Matches(expected ChildStartReceipt) bool {
	left, right := receipt.BindingDigest(), expected.BindingDigest()
	return left != "" && right != "" && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func digestCanonical(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
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
	Created           bool               `json:"created"`
	BootstrapStarted  bool               `json:"bootstrap_started,omitempty"`
	Started           bool               `json:"started"`
	Completed         bool               `json:"completed"`
	ChildStartReceipt *ChildStartReceipt `json:"-"`
	ExitCode          int                `json:"exit_code"`
	Stdout            []byte             `json:"stdout,omitempty"`
	Stderr            []byte             `json:"stderr,omitempty"`
	Duration          time.Duration      `json:"duration"`
	StdoutTruncated   bool               `json:"stdout_truncated"`
	StderrTruncated   bool               `json:"stderr_truncated"`
	SecretDetected    bool               `json:"secret_detected"`
	SafeFacts         map[string]string  `json:"safe_facts,omitempty"`
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
	if result.ChildStartReceipt != nil {
		receipt := result.ChildStartReceipt.Clone()
		result.ChildStartReceipt = &receipt
	}
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
	if result.ChildStartReceipt != nil {
		*result.ChildStartReceipt = ChildStartReceipt{}
		result.ChildStartReceipt = nil
	}
}

type State string

const (
	StateAbsent           State = "absent"
	StateResourceCreated  State = "resource_created"
	StateBootstrapStarted State = "bootstrap_started"
	StateChildStarted     State = "child_started"
	StateTerminalComplete State = "terminal_complete"
	StateDestroyed        State = "destroyed"
	StateUnknown          State = "unknown"

	// Compatibility names retain source compatibility while mapping to the
	// corrected provider-neutral lifecycle vocabulary.
	StateCreated   = StateResourceCreated
	StateRunning   = StateChildStarted
	StateCompleted = StateTerminalComplete
)

func StableStates() []State {
	return []State{
		StateAbsent,
		StateResourceCreated,
		StateBootstrapStarted,
		StateChildStarted,
		StateTerminalComplete,
		StateDestroyed,
		StateUnknown,
	}
}

func (state State) Validate() error {
	switch state {
	case StateAbsent, StateResourceCreated, StateBootstrapStarted, StateChildStarted,
		StateTerminalComplete, StateDestroyed, StateUnknown:
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
