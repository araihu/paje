package dockerengine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/containerd/errdefs"
	mobyclient "github.com/moby/moby/client"
)

func TestReadPrivateReceiptExecOutputMapsMissingReceiptToNotFoundAfterBoundedStderr(t *testing.T) {
	const privateDiagnostic = "receipt does not exist yet: secret-private-detail"
	got, err := readPrivateReceiptExecOutput(
		bytes.NewReader(multiplexedOutput(nil, []byte(privateDiagnostic))),
		1024,
		func() (mobyclient.ExecInspectResult, error) {
			return mobyclient.ExecInspectResult{ExitCode: 1}, nil
		},
	)
	if len(got) != 0 || !errors.Is(err, errdefs.ErrNotFound) {
		t.Fatalf("readPrivateReceiptExecOutput() = %q, %v", got, err)
	}
	if strings.Contains(err.Error(), privateDiagnostic) {
		t.Fatalf("error exposed private diagnostic: %q", err)
	}
}

func TestReadPrivateReceiptExecOutputClassifiesSignalTerminationAsUncertainty(t *testing.T) {
	terminal := func(exitCode int) func() (mobyclient.ExecInspectResult, error) {
		return func() (mobyclient.ExecInspectResult, error) {
			return mobyclient.ExecInspectResult{ExitCode: exitCode}, nil
		}
	}

	for _, exitCode := range []int{128, 137, 255} {
		t.Run(fmt.Sprintf("signal class %d", exitCode), func(t *testing.T) {
			got, err := readPrivateReceiptExecOutput(bytes.NewReader(nil), 1024, terminal(exitCode))
			if len(got) != 0 || !errors.Is(err, errPrivateReceiptOutcomeUncertain) {
				t.Fatalf("readPrivateReceiptExecOutput() = %q, %v", got, err)
			}
		})
	}

	for _, exitCode := range []int{2, 127} {
		t.Run(fmt.Sprintf("logical exit %d", exitCode), func(t *testing.T) {
			got, err := readPrivateReceiptExecOutput(bytes.NewReader(nil), 1024, terminal(exitCode))
			if len(got) != 0 || err == nil {
				t.Fatalf("readPrivateReceiptExecOutput() = %q, %v", got, err)
			}
			if errors.Is(err, errPrivateReceiptOutcomeUncertain) || errors.Is(err, errdefs.ErrNotFound) {
				t.Fatalf("logical exit became retryable: %v", err)
			}
		})
	}
}

func TestReadPrivateReceiptExecOutputPreservesCandidateAcrossTerminalRaces(t *testing.T) {
	receipt := []byte(`{"receipt":"exact-candidate"}`)
	stream := multiplexedOutput(receipt, nil)

	for _, test := range []struct {
		name    string
		reader  io.Reader
		inspect func() (mobyclient.ExecInspectResult, error)
	}{
		{
			name:   "exec identity removed after output",
			reader: bytes.NewReader(stream),
			inspect: func() (mobyclient.ExecInspectResult, error) {
				return mobyclient.ExecInspectResult{}, errdefs.ErrNotFound
			},
		},
		{
			name:   "terminal stream closes after output",
			reader: &readerEndingWithError{data: stream, err: net.ErrClosed},
			inspect: func() (mobyclient.ExecInspectResult, error) {
				return mobyclient.ExecInspectResult{ExitCode: 0}, nil
			},
		},
		{
			name: "Docker 29 terminal stream closes after output",
			reader: &readerEndingWithError{
				data: stream,
				err:  errors.New("read unix @->/Users/test/.colima/default/docker.sock: use of closed network connection"),
			},
			inspect: func() (mobyclient.ExecInspectResult, error) {
				return mobyclient.ExecInspectResult{ExitCode: 0}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := readPrivateReceiptExecOutput(test.reader, int64(len(receipt)+1), test.inspect)
			if !bytes.Equal(got, receipt) || err == nil {
				t.Fatalf("readPrivateReceiptExecOutput() = %q, %v", got, err)
			}
		})
	}
}

func TestBenignReceiptStreamCloseMatchesOnlyTerminalCompatibilityShape(t *testing.T) {
	dockerTerminal := errors.New("read unix @->/Users/test/.colima/default/docker.sock: use of closed network connection")
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "net closed sentinel", err: net.ErrClosed, want: true},
		{name: "pipe closed sentinel", err: io.ErrClosedPipe, want: true},
		{name: "connection reset sentinel", err: syscall.ECONNRESET, want: true},
		{name: "exact compatibility message", err: errors.New("use of closed network connection"), want: true},
		{name: "Docker 29 terminal suffix", err: dockerTerminal, want: true},
		{name: "wrapped Docker 29 terminal suffix", err: fmt.Errorf("read receipt: %w", dockerTerminal), want: true},
		{name: "substring without delimiter", err: errors.New("prefix use of closed network connection")},
		{name: "trailing text", err: errors.New("read unix socket: use of closed network connection after output")},
		{name: "case variant", err: errors.New("read unix socket: Use of closed network connection")},
		{name: "near miss", err: errors.New("read unix socket: use of a closed network connection")},
		{name: "arbitrary error", err: errors.New("terminal Docker stream failed")},
		{name: "nil error", err: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := benignReceiptStreamClose(test.err); got != test.want {
				t.Fatalf("benignReceiptStreamClose(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestReadPrivateReceiptExecOutputRejectsUnprovenOutput(t *testing.T) {
	receipt := []byte(`{"receipt":"exact-candidate"}`)
	terminal := func(result mobyclient.ExecInspectResult, err error) func() (mobyclient.ExecInspectResult, error) {
		return func() (mobyclient.ExecInspectResult, error) { return result, err }
	}

	for _, test := range []struct {
		name           string
		reader         io.Reader
		limit          int64
		inspect        func() (mobyclient.ExecInspectResult, error)
		rejectNotFound bool
	}{
		{
			name:    "stderr",
			reader:  bytes.NewReader(multiplexedOutput(receipt, []byte("private reader failed"))),
			limit:   int64(len(receipt) + 1),
			inspect: terminal(mobyclient.ExecInspectResult{ExitCode: 0}, nil),
		},
		{
			name:    "stdout truncated",
			reader:  bytes.NewReader(multiplexedOutput(receipt, nil)),
			limit:   int64(len(receipt) - 1),
			inspect: terminal(mobyclient.ExecInspectResult{ExitCode: 0}, nil),
		},
		{
			name:    "reader still running",
			reader:  bytes.NewReader(multiplexedOutput(receipt, nil)),
			limit:   int64(len(receipt) + 1),
			inspect: terminal(mobyclient.ExecInspectResult{Running: true}, nil),
		},
		{
			name:    "reader failed",
			reader:  bytes.NewReader(multiplexedOutput(receipt, nil)),
			limit:   int64(len(receipt) + 1),
			inspect: terminal(mobyclient.ExecInspectResult{ExitCode: 2}, nil),
		},
		{
			name:           "exit one with exact candidate",
			reader:         bytes.NewReader(multiplexedOutput(receipt, nil)),
			limit:          int64(len(receipt) + 1),
			inspect:        terminal(mobyclient.ExecInspectResult{ExitCode: 1}, nil),
			rejectNotFound: true,
		},
		{
			name:           "exit one with partial candidate",
			reader:         bytes.NewReader(multiplexedOutput(receipt[:8], nil)),
			limit:          int64(len(receipt) + 1),
			inspect:        terminal(mobyclient.ExecInspectResult{ExitCode: 1}, nil),
			rejectNotFound: true,
		},
		{
			name:           "exit one with truncated stderr",
			reader:         bytes.NewReader(multiplexedOutput(nil, bytes.Repeat([]byte("x"), 4097))),
			limit:          int64(len(receipt) + 1),
			inspect:        terminal(mobyclient.ExecInspectResult{ExitCode: 1}, nil),
			rejectNotFound: true,
		},
		{
			name:    "nonterminal stream error",
			reader:  &readerEndingWithError{data: multiplexedOutput(receipt, nil), err: errors.New("framing failure")},
			limit:   int64(len(receipt) + 1),
			inspect: terminal(mobyclient.ExecInspectResult{ExitCode: 0}, nil),
		},
		{
			name:           "inspect uncertainty without candidate",
			reader:         bytes.NewReader(nil),
			limit:          int64(len(receipt) + 1),
			inspect:        terminal(mobyclient.ExecInspectResult{}, errdefs.ErrNotFound),
			rejectNotFound: true,
		},
		{
			name:    "empty successful output",
			reader:  bytes.NewReader(nil),
			limit:   int64(len(receipt) + 1),
			inspect: terminal(mobyclient.ExecInspectResult{ExitCode: 0}, nil),
		},
		{
			name:           "exit one with malformed framing",
			reader:         bytes.NewReader(malformedMultiplexedOutput()),
			limit:          int64(len(receipt) + 1),
			inspect:        terminal(mobyclient.ExecInspectResult{ExitCode: 1}, nil),
			rejectNotFound: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := readPrivateReceiptExecOutput(test.reader, test.limit, test.inspect)
			if len(got) != 0 || err == nil {
				t.Fatalf("readPrivateReceiptExecOutput() = %q, %v", got, err)
			}
			if test.rejectNotFound && errors.Is(err, errdefs.ErrNotFound) {
				t.Fatalf("readPrivateReceiptExecOutput() returned retryable not-found: %v", err)
			}
		})
	}
}

func malformedMultiplexedOutput() []byte {
	header := make([]byte, 8)
	header[0] = 1
	binary.BigEndian.PutUint32(header[4:], 16)
	return append(header, []byte("partial")...)
}

type readerEndingWithError struct {
	data []byte
	err  error
}

func (reader *readerEndingWithError) Read(target []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, reader.err
	}
	count := copy(target, reader.data)
	reader.data = reader.data[count:]
	return count, nil
}
