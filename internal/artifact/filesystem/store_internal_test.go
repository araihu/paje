package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"golang.org/x/sys/unix"
)

func TestLinkTempRejectsReplacementInsteadOfPublishingAnotherInode(t *testing.T) {
	root := t.TempDir()
	store, err := New(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	name, temp, err := store.createTemp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = temp.Close() })
	if _, err := temp.Write([]byte("expected inode")); err != nil {
		t.Fatal(err)
	}
	if err := temp.Sync(); err != nil {
		t.Fatal(err)
	}

	original := filepath.Join(root, "tmp", name)
	moved := original + ".moved"
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("replacement inode"), 0o600); err != nil {
		t.Fatal(err)
	}

	prefix, _, err := store.prefix("aa")
	if err != nil {
		t.Fatal(err)
	}
	defer prefix.Close()
	published, err := linkTemp(store.tmp, name, temp, prefix, "artifact.tar.gz")
	if !errors.Is(err, artifact.ErrDigestMismatch) {
		t.Fatalf("linkTemp() error = %v, want ErrDigestMismatch", err)
	}
	if published {
		t.Fatal("linkTemp reported a replacement inode as published")
	}
	if _, err := os.Lstat(filepath.Join(root, "sha256", "aa", "artifact.tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement inode remains published: %v", err)
	}
}

func TestLockCancellationDoesNotLeakOrDeadlock(t *testing.T) {
	store, err := New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	first, _, err := store.prefix("aa")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, _, err := store.prefix("aa")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	unlock, err := lock(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := lock(ctx, second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended lock error = %v, want DeadlineExceeded", err)
	}
	unlock()

	release, err := lock(context.Background(), second)
	if err != nil {
		t.Fatalf("lock after canceled waiter: %v", err)
	}
	release()
}

func TestArtifactDescriptorsAreCloseOnExec(t *testing.T) {
	store, err := New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertCloseOnExec(t, store.root)
	assertCloseOnExec(t, store.tmp)
	assertCloseOnExec(t, store.sha)

	name, temp, err := store.createTemp()
	if err != nil {
		t.Fatal(err)
	}
	defer temp.Close()
	defer unix.Unlinkat(int(store.tmp.Fd()), name, 0)
	assertCloseOnExec(t, temp)

	prefix, _, err := store.prefix("aa")
	if err != nil {
		t.Fatal(err)
	}
	defer prefix.Close()
	assertCloseOnExec(t, prefix)
}

func assertCloseOnExec(t *testing.T, file *os.File) {
	t.Helper()
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("descriptor %q is not close-on-exec", file.Name())
	}
}
