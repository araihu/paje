package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteJSONCreatesCanonicalPrivateFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "record.json")
	value := struct {
		Name string `json:"name"`
	}{Name: "submission"}
	if err := atomicWriteJSON(context.Background(), target, value); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{\"name\":\"submission\"}\n" {
		t.Fatalf("atomic JSON = %q", encoded)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("atomic JSON mode = %o, want 600", info.Mode().Perm())
	}
}

func TestAtomicWriteJSONDoesNotFollowTargetSymlink(t *testing.T) {
	directory := t.TempDir()
	realTarget := filepath.Join(directory, "real.json")
	if err := os.WriteFile(realTarget, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkTarget := filepath.Join(directory, "link.json")
	if err := os.Symlink(realTarget, linkTarget); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteJSON(context.Background(), linkTarget, struct{}{}); err == nil {
		t.Fatal("atomicWriteJSON() followed target symlink")
	}
	encoded, err := os.ReadFile(realTarget)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "original\n" {
		t.Fatalf("symlink target changed to %q", encoded)
	}
}

func TestAtomicWriteJSONHonorsCanceledContextBeforeReplace(t *testing.T) {
	target := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := atomicWriteJSON(ctx, target, struct{}{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("atomicWriteJSON() error = %v, want context canceled", err)
	}
	encoded, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "original\n" {
		t.Fatalf("canceled write changed target to %q", encoded)
	}
}
