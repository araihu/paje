package filesystem_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/artifact/filesystem"
	"github.com/araihu/paje/internal/template"
	"github.com/araihu/paje/internal/verification"
)

func TestStoreSaveLoadIsDeterministicAcrossRestart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newStore(t, root, 1<<20)
	bundle := testBundle()
	first, err := store.Save(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Save(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest || first.Size != second.Size {
		t.Fatalf("references differ: %#v %#v", first, second)
	}

	restarted := newStore(t, root, 1<<20)
	loaded, err := restarted.Load(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	want, _, _, err := artifact.Canonicalize(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("loaded bundle mismatch\n got: %#v\nwant: %#v", loaded, want)
	}
	if loaded.Verification[0].Command.Environment != nil {
		t.Fatal("verification environment values were serialized")
	}
}

func TestStoreDetectsCorruptionAndRejectsUnsafeReferences(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newStore(t, root, 1<<20)
	ref, err := store.Save(context.Background(), testBundle())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sha256", ref.Digest[:2], ref.Digest+".tar.gz")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0x01
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), ref); !errors.Is(err, artifact.ErrDigestMismatch) {
		t.Fatalf("Load() error = %v, want ErrDigestMismatch", err)
	}
	if _, err := store.Load(context.Background(), artifact.Reference{RunID: ref.RunID, Digest: "../" + ref.Digest, Size: ref.Size}); !errors.Is(err, artifact.ErrInvalidReference) {
		t.Fatalf("unsafe ref error = %v", err)
	}
}

func TestStoreLimitsAndCleansTemporaryFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newStore(t, root, 64)
	bundle := testBundle()
	bundle.AgentOutput = []byte(strings.Repeat("uncompressible-ish-artifact-content-", 40))
	if _, err := store.Save(context.Background(), bundle); !errors.Is(err, artifact.ErrTooLarge) {
		t.Fatalf("Save() error = %v, want ErrTooLarge", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files remain: %#v", entries)
	}
	shaEntries, err := os.ReadDir(filepath.Join(root, "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if len(shaEntries) != 0 {
		t.Fatalf("final bundle directories remain: %#v", shaEntries)
	}
	if _, err := filesystem.New(root, (int64(^uint64(0)>>1)/16)+1); err == nil {
		t.Fatal("New accepted overflowing compressed limit")
	}
}

func TestStoreReturnsDefensiveCopiesAndHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	store := newStore(t, t.TempDir(), 1<<20)
	ref, err := store.Save(context.Background(), testBundle())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	loaded.AgentOutput[0] = 'X'
	loaded.Preflight["base_sha"] = "changed"
	loaded.Verification[0].Command.Args[0] = "changed"
	again, err := store.Load(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.AgentOutput) != "agent output" || again.Preflight["base_sha"] != "abc" || again.Verification[0].Command.Args[0] != "test" {
		t.Fatalf("store leaked internal mutable values: %#v", again)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load canceled context error = %v", err)
	}
}

func TestReferenceForCanonicalizesRawJSONAndSemanticallyUnorderedValues(t *testing.T) {
	t.Parallel()
	left := testBundle()
	right := testBundle()
	left.ExecutionMetadata = json.RawMessage(`{"z": [3, 2], "a": {"beta": true, "alpha": false}}`)
	right.ExecutionMetadata = json.RawMessage(` { "a" : { "alpha" : false, "beta" : true }, "z" : [3,2] } `)
	right.Manifest.MemoryIDs = []string{"memory-z", "memory-a"}
	right.Warnings = []string{"z warning", "a warning"}
	leftRef, err := artifact.ReferenceFor(left)
	if err != nil {
		t.Fatal(err)
	}
	rightRef, err := artifact.ReferenceFor(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftRef.Digest != rightRef.Digest {
		t.Fatalf("canonical references differ: %s != %s", leftRef.Digest, rightRef.Digest)
	}
}

func newStore(t *testing.T, root string, limit int64) *filesystem.Store {
	t.Helper()
	store, err := filesystem.New(root, limit)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testBundle() artifact.Bundle {
	return artifact.Bundle{
		Manifest:          artifact.Manifest{RunID: "run-123", Template: template.ID{Name: "code-change", Version: 1}, Repository: "https://example.test/repo.git", BaseSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), Changes: []artifact.Change{{Path: "main.go", Status: "modified", OldMode: "100644", NewMode: "100755"}}, MemoryIDs: []string{"memory-z", "memory-a"}, MemoryCount: 2},
		ChangesPatch:      []byte("diff --git a/main.go b/main.go\n"),
		AgentOutput:       []byte("agent output"),
		ExecutionMetadata: json.RawMessage(`{"run":{"status":"completed"},"environment":{"PATH":"[redacted]","TOKEN":"[redacted]"}}`),
		Verification:      []verification.Result{{Command: verification.Command{Name: "go test", Directory: "/workspace", Executable: "go", Args: []string{"test", "./..."}, Environment: map[string]string{"GOWORK": "off"}, Timeout: time.Minute, Required: true}, ExitCode: 0, Duration: 2 * time.Second, Output: "ok", Passed: true}},
		Preflight:         map[string]string{"tool:go": "available", "base_sha": "abc"},
		Warnings:          []string{"z warning", "a warning"},
	}
}

func TestMemberDigestsAreVerified(t *testing.T) {
	t.Parallel()
	store := newStore(t, t.TempDir(), 1<<20)
	bundle := testBundle()
	ref, err := store.Save(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Manifest.Members) != 6 {
		t.Fatalf("members = %#v", loaded.Manifest.Members)
	}
	for _, member := range loaded.Manifest.Members {
		if member.Size < 0 || len(member.SHA256) != 64 {
			t.Fatalf("invalid member: %#v", member)
		}
	}
	got := sha256.Sum256(loaded.AgentOutput)
	if loaded.Manifest.Members[1].SHA256 == "" || got == ([32]byte{}) {
		t.Fatal("missing member digest")
	}
}
