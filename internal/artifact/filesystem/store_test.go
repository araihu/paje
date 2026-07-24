package filesystem_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

func TestStoreRejectsUncompressedBundlesLoadWouldReject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newStore(t, root, 512) // Load permits only 8 KiB uncompressed.
	bundle := testBundle()
	bundle.AgentOutput = bytes.Repeat([]byte("compressible evidence\n"), 1024)
	if _, err := store.Save(context.Background(), bundle); !errors.Is(err, artifact.ErrTooLarge) {
		t.Fatalf("Save() error = %v, want uncompressed ErrTooLarge", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files remain: %#v", entries)
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
	left.ExecutionMetadata = json.RawMessage(`{"completed":true,"duration":2.5,"exit_code":0}`)
	right.ExecutionMetadata = json.RawMessage(` { "exit_code" : 0, "duration" : 2.5, "completed" : true } `)
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

func TestReferenceForNormalizesNilEmptyAndRejectsUnsafeMetadataAndNumbers(t *testing.T) {
	t.Parallel()
	nilBundle := testBundle()
	nilBundle.Manifest.Changes = nil
	nilBundle.Manifest.MemoryIDs = nil
	nilBundle.Manifest.MemoryCount = 0
	nilBundle.Verification = nil
	nilBundle.Warnings = nil
	emptyBundle := nilBundle
	emptyBundle.Manifest.Changes = []artifact.Change{}
	emptyBundle.Manifest.MemoryIDs = []string{}
	emptyBundle.Verification = []verification.Result{}
	emptyBundle.Warnings = []string{}
	nilRef, err := artifact.ReferenceFor(nilBundle)
	if err != nil {
		t.Fatal(err)
	}
	emptyRef, err := artifact.ReferenceFor(emptyBundle)
	if err != nil {
		t.Fatal(err)
	}
	if nilRef != emptyRef {
		t.Fatalf("nil and empty references differ: %#v %#v", nilRef, emptyRef)
	}

	unsafe := testBundle()
	unsafe.ExecutionMetadata = json.RawMessage(`{"completed":true,"environment":{"TOKEN":"secret"}}`)
	if _, err := artifact.ReferenceFor(unsafe); !errors.Is(err, artifact.ErrInvalidBundle) {
		t.Fatalf("unsafe metadata error = %v", err)
	}
	numeric := testBundle()
	numeric.ExecutionMetadata = json.RawMessage(`{"completed":true,"duration":1.0}`)
	if _, err := artifact.ReferenceFor(numeric); !errors.Is(err, artifact.ErrInvalidBundle) {
		t.Fatalf("noncanonical number error = %v", err)
	}
}

func TestStoreRejectsAppendedGzipMember(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newStore(t, root, 1<<20)
	ref, err := store.Save(context.Background(), testBundle())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sha256", ref.Digest[:2], ref.Digest+".tar.gz")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(file)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), ref); !errors.Is(err, artifact.ErrDigestMismatch) {
		t.Fatalf("Load() error = %v, want mismatch", err)
	}
}

func TestStoreRejectsSymlinkedComponentsAndHardLinkedArtifacts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := newStore(t, root, 1<<20)
	if err := os.RemoveAll(filepath.Join(root, "sha256")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "sha256")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), testBundle()); err == nil {
		t.Fatal("Save accepted a symlinked sha256 component")
	}

	root = t.TempDir()
	store = newStore(t, root, 1<<20)
	ref, err := store.Save(context.Background(), testBundle())
	if err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(root, "sha256", ref.Digest[:2], ref.Digest+".tar.gz")
	if err := os.Link(final, final+".extra"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), ref); !errors.Is(err, artifact.ErrDigestMismatch) {
		t.Fatalf("Load hard-linked artifact error = %v", err)
	}
}

func TestStoreConcurrentSavesDoNotOverwriteWinner(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := newStore(t, root, 1<<20)
	second := newStore(t, root, 1<<20)
	var refs [2]artifact.Reference
	var errs [2]error
	var wait sync.WaitGroup
	for index, store := range []*filesystem.Store{first, second} {
		wait.Add(1)
		go func(index int, store *filesystem.Store) {
			defer wait.Done()
			refs[index], errs[index] = store.Save(context.Background(), testBundle())
		}(index, store)
	}
	wait.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent Save errors: %v, %v", errs[0], errs[1])
	}
	if refs[0] != refs[1] {
		t.Fatalf("concurrent references differ: %#v %#v", refs[0], refs[1])
	}
	if _, err := first.Load(context.Background(), refs[0]); err != nil {
		t.Fatal(err)
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
		ExecutionMetadata: json.RawMessage(`{"completed":true,"duration":2.5,"exit_code":0,"started":true,"truncated":false}`),
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
	want := map[string][]byte{"changes.patch": loaded.ChangesPatch, "agent-output.txt": loaded.AgentOutput, "execution.json": loaded.ExecutionMetadata}
	for _, member := range loaded.Manifest.Members {
		data, ok := want[member.Name]
		if !ok {
			continue
		}
		sum := sha256.Sum256(data)
		if member.Size != int64(len(data)) || member.SHA256 != fmt.Sprintf("%x", sum) {
			t.Fatalf("member = %#v, want exact digest/size", member)
		}
	}
}
