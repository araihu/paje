package filesystem_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"github.com/araihu/paje/internal/artifact/filesystem"
	"github.com/araihu/paje/internal/runner"
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
	left.ExecutionMetadata = json.RawMessage(`{"exit_code":0,"duration":2.5,"started":true,"completed":true,"truncated":false}`)
	right.ExecutionMetadata = append(json.RawMessage(nil), left.ExecutionMetadata...)
	left.Manifest.Changes = []artifact.Change{{Path: "z.go", Status: "modified"}, {Path: "a.go", Status: "added"}}
	left.Manifest.MemoryIDs = []string{"memory-z", "memory-a"}
	left.Warnings = []string{"z warning", "a warning"}
	right.Manifest.Changes = []artifact.Change{{Path: "a.go", Status: "added"}, {Path: "z.go", Status: "modified"}}
	right.Manifest.MemoryIDs = []string{"memory-a", "memory-z"}
	right.Warnings = []string{"a warning", "z warning"}
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
	unsafe.ExecutionMetadata = json.RawMessage(`{"exit_code":0,"duration":2.5,"started":true,"completed":true,"truncated":false,"environment":{"TOKEN":"secret"}}`)
	if _, err := artifact.ReferenceFor(unsafe); !errors.Is(err, artifact.ErrInvalidBundle) {
		t.Fatalf("unsafe metadata error = %v", err)
	}
	numeric := testBundle()
	numeric.ExecutionMetadata = json.RawMessage(`{"exit_code":0,"duration":1.0,"started":true,"completed":true,"truncated":false}`)
	if _, err := artifact.ReferenceFor(numeric); !errors.Is(err, artifact.ErrInvalidBundle) {
		t.Fatalf("noncanonical number error = %v", err)
	}
}

func TestExecutionEvidenceFromTaskOneAndStrictWireForm(t *testing.T) {
	t.Parallel()
	evidence := artifact.ExecutionEvidenceFrom(runner.ExecutionResult{Transcript: "secret transcript", Output: "secret output", ExitCode: 7, Duration: 1.5, Started: true, Completed: true, Truncated: true})
	if evidence != (artifact.ExecutionEvidence{ExitCode: 7, Duration: 1.5, Started: true, Completed: true, Truncated: true}) {
		t.Fatalf("evidence = %#v", evidence)
	}
	bundle := testBundle()
	bundle.ExecutionMetadata = json.RawMessage(`{"exit_code":7,"duration":1.5,"started":true,"completed":true,"truncated":true}`)
	if _, err := artifact.ReferenceFor(bundle); err != nil {
		t.Fatal(err)
	}
	bundle.ExecutionMetadata = json.RawMessage(`{"exit_code":7,"duration":1.5,"started":true,"completed":true,"truncated":true,"output":"secret"}`)
	if _, err := artifact.ReferenceFor(bundle); !errors.Is(err, artifact.ErrInvalidBundle) {
		t.Fatalf("unknown metadata field error = %v", err)
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

func TestNewRejectsSymlinkedRootWithoutChangingOutsideDirectory(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	outside := t.TempDir()
	if err := os.Chmod(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "artifact-root")
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}

	if _, err := filesystem.New(root, 1<<20); err == nil {
		t.Fatal("New accepted a symlinked root")
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("outside directory mode = %o, want 755", got)
	}
}

func TestStoreDoesNotFollowReplacedTemporaryOrPrefixNames(t *testing.T) {
	t.Parallel()
	t.Run("temporary directory", func(t *testing.T) {
		root := t.TempDir()
		store := newStore(t, root, 1<<20)
		outside := t.TempDir()
		sentinel := filepath.Join(outside, "sentinel")
		if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "tmp")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "tmp")); err != nil {
			t.Fatal(err)
		}

		if _, err := store.Save(context.Background(), testBundle()); err == nil {
			t.Fatal("Save followed a replaced temporary-directory name")
		}
		assertOnlyOutsideSentinel(t, outside, sentinel)
	})

	t.Run("digest prefix", func(t *testing.T) {
		root := t.TempDir()
		store := newStore(t, root, 1<<20)
		bundle := testBundle()
		ref, err := store.Save(context.Background(), bundle)
		if err != nil {
			t.Fatal(err)
		}
		prefix := filepath.Join(root, "sha256", ref.Digest[:2])
		if err := os.Rename(prefix, prefix+".moved"); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		sentinel := filepath.Join(outside, "sentinel")
		if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, prefix); err != nil {
			t.Fatal(err)
		}

		if _, err := store.Load(context.Background(), ref); err == nil {
			t.Fatal("Load followed a replaced digest-prefix name")
		}
		assertOnlyOutsideSentinel(t, outside, sentinel)
	})
}

func assertOnlyOutsideSentinel(t *testing.T, outside, sentinel string) {
	t.Helper()
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(sentinel) {
		t.Fatalf("outside directory was modified: %#v", entries)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "outside" {
		t.Fatalf("outside sentinel = %q, want outside", data)
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
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func testBundle() artifact.Bundle {
	return artifact.Bundle{
		Manifest:          artifact.Manifest{RunID: "run-123", Template: template.ID{Name: "code-change", Version: 1}, Repository: "https://example.test/repo.git", BaseSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), Changes: []artifact.Change{{Path: "main.go", Status: "modified", OldMode: "100644", NewMode: "100755"}}, MemoryIDs: []string{"memory-z", "memory-a"}, MemoryCount: 2},
		ChangesPatch:      []byte("diff --git a/main.go b/main.go\n"),
		AgentOutput:       []byte("agent output"),
		ExecutionMetadata: json.RawMessage(`{"exit_code":0,"duration":2.5,"started":true,"completed":true,"truncated":false}`),
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
	want := []artifact.Member{
		{Name: "changes.patch", SHA256: "37ccc013b5e1b4412daf98cc7d0a2449d56ec4de799cd0408bb5b0429599e207", Size: 31},
		{Name: "agent-output.txt", SHA256: "39d8ba2ada2b71567f9e0c8e3cf54e857a990a4a83aa9a03a102b9c44aba1bf7", Size: 12},
		{Name: "execution.json", SHA256: "cd7500269fbe5eda26d71cc8a500fd0e560e210256f73bc303feb271291be2e2", Size: 80},
		{Name: "verification.json", SHA256: "fc510c02ee052e21115aee395c8487ff258ae791635313b3f5ad37a021d4a0fb", Size: 235},
		{Name: "preflight.json", SHA256: "dbe343b13cce473846c0c82448552f060fe6d6d3a404a52fe1a529d9adbafe46", Size: 72},
		{Name: "warnings.json", SHA256: "2cb92aff025886ffc3c3e9f5222a0bb23976430250379e5d0a11e2e035a5c807", Size: 25},
	}
	if !reflect.DeepEqual(loaded.Manifest.Members, want) {
		t.Fatalf("members = %#v, want %#v", loaded.Manifest.Members, want)
	}
}
