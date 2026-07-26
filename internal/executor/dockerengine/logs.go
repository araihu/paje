package dockerengine

import (
	"io"
	"time"

	"github.com/araihu/paje/internal/executil"
	"github.com/moby/moby/api/pkg/stdcopy"
)

const logDrainTimeout = 2 * time.Second

type logCapture struct {
	reader io.ReadCloser
	stdout *executil.LimitedBuffer
	stderr *executil.LimitedBuffer
	done   chan error
}

func startLogCapture(reader io.ReadCloser, limit int64) (*logCapture, error) {
	stdout, err := executil.NewLimitedBuffer(limit)
	if err != nil {
		return nil, err
	}
	stderr, err := executil.NewLimitedBuffer(limit)
	if err != nil {
		return nil, err
	}
	capture := &logCapture{
		reader: reader, stdout: stdout, stderr: stderr, done: make(chan error, 1),
	}
	go func() {
		_, copyErr := stdcopy.StdCopy(stdout, stderr, reader)
		capture.done <- copyErr
	}()
	return capture, nil
}

func (capture *logCapture) finish(timeout time.Duration) ([]byte, []byte, bool, bool, error) {
	if capture == nil {
		return nil, nil, false, false, nil
	}
	if timeout <= 0 {
		timeout = logDrainTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-capture.done:
		_ = capture.reader.Close()
		return capture.stdout.Bytes(), capture.stderr.Bytes(),
			capture.stdout.Truncated(), capture.stderr.Truncated(), err
	case <-timer.C:
		_ = capture.reader.Close()
		second := time.NewTimer(timeout)
		defer second.Stop()
		select {
		case <-capture.done:
			return capture.stdout.Bytes(), capture.stderr.Bytes(),
				capture.stdout.Truncated(), capture.stderr.Truncated(), io.ErrNoProgress
		case <-second.C:
			return nil, nil, false, false, io.ErrNoProgress
		}
	}
}
