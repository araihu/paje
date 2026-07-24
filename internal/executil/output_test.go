package executil_test

import (
	"sync"
	"testing"

	"github.com/araihu/paje/internal/executil"
)

func TestNewLimitedBufferRejectsNonPositiveLimit(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		if _, err := executil.NewLimitedBuffer(limit); err == nil {
			t.Errorf("NewLimitedBuffer(%d) error = nil, want validation error", limit)
		}
	}
}

func TestLimitedBufferRetainsWritesAtLimit(t *testing.T) {
	buffer, err := executil.NewLimitedBuffer(4)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"ab", "cd"} {
		if count, err := buffer.Write([]byte(input)); err != nil || count != len(input) {
			t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", input, count, err, len(input))
		}
	}
	if got := string(buffer.Bytes()); got != "abcd" {
		t.Errorf("Bytes() = %q, want %q", got, "abcd")
	}
	if buffer.Truncated() {
		t.Error("Truncated() = true, want false")
	}
}

func TestLimitedBufferTruncatesAndAcceptsEntireWrite(t *testing.T) {
	buffer, err := executil.NewLimitedBuffer(4)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := buffer.Write([]byte("abcdef")); err != nil || count != 6 {
		t.Fatalf("Write() = (%d, %v), want (6, nil)", count, err)
	}
	if got := string(buffer.Bytes()); got != "abcd" {
		t.Errorf("Bytes() = %q, want %q", got, "abcd")
	}
	if !buffer.Truncated() {
		t.Error("Truncated() = false, want true")
	}
}

func TestLimitedBufferBytesReturnsDefensiveCopy(t *testing.T) {
	buffer, err := executil.NewLimitedBuffer(4)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = buffer.Write([]byte("test"))
	copy := buffer.Bytes()
	copy[0] = 'b'
	if got := string(buffer.Bytes()); got != "test" {
		t.Errorf("Bytes() after mutation = %q, want %q", got, "test")
	}
}

func TestLimitedBufferSupportsConcurrentWrites(t *testing.T) {
	buffer, err := executil.NewLimitedBuffer(10_000)
	if err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	for range 2 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for range 1_000 {
				if _, err := buffer.Write([]byte("x")); err != nil {
					t.Errorf("Write() error = %v", err)
				}
			}
		}()
	}
	writers.Wait()
	if got := len(buffer.Bytes()); got != 2_000 {
		t.Errorf("len(Bytes()) = %d, want 2000", got)
	}
	if buffer.Truncated() {
		t.Error("Truncated() = true, want false")
	}
}
