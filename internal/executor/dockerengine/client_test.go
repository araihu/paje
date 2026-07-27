package dockerengine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
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
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := readPrivateReceiptExecOutput(test.reader, int64(len(receipt)+1), test.inspect)
			if !bytes.Equal(got, receipt) || err == nil {
				t.Fatalf("readPrivateReceiptExecOutput() = %q, %v", got, err)
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
