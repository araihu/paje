// Package executil contains shared process-execution helpers.
package executil

import (
	"bytes"
	"fmt"
	"sync"
)

// LimitedBuffer retains a bounded prefix of data written to it.
type LimitedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int64
	truncated bool
}

// NewLimitedBuffer constructs a buffer that retains at most limit bytes.
func NewLimitedBuffer(limit int64) (*LimitedBuffer, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("create limited buffer: limit must be positive")
	}
	return &LimitedBuffer{limit: limit}, nil
}

// Write accepts every byte while retaining only the first limit bytes.
func (b *LimitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	original := len(p)
	remaining := b.limit - int64(b.buffer.Len())
	if remaining <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(p)
	return original, nil
}

// Bytes returns a copy of the retained output.
func (b *LimitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

// Truncated reports whether output exceeded the configured limit.
func (b *LimitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
