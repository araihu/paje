//go:build darwin || linux

package filesystem

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"golang.org/x/sys/unix"
)

func TestPublishTempRejectsReplacementInsteadOfPublishingAnotherInode(t *testing.T) {
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
	published, err := publishTemp(store.tmp, name, temp, prefix, "artifact.tar.gz")
	if !errors.Is(err, artifact.ErrDigestMismatch) {
		t.Fatalf("publishTemp() error = %v, want ErrDigestMismatch", err)
	}
	if published {
		t.Fatal("publishTemp reported a replacement inode as published")
	}
	if _, err := os.Lstat(filepath.Join(root, "sha256", "aa", "artifact.tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement inode remains published: %v", err)
	}
}

func TestPublishTempLeavesNoTwoLinkCrashState(t *testing.T) {
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
	defer temp.Close()
	if _, err := temp.Write([]byte("canonical gzip bytes")); err != nil {
		t.Fatal(err)
	}
	if err := temp.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := checkFile(temp); err != nil {
		t.Fatal(err)
	}
	prefix, _, err := store.prefix("aa")
	if err != nil {
		t.Fatal(err)
	}
	defer prefix.Close()

	published, err := publishTemp(store.tmp, name, temp, prefix, "artifact.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if !published {
		t.Fatal("publishTemp unexpectedly reported an existing winner")
	}
	if _, err := os.Lstat(filepath.Join(root, "tmp", name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary source still exists after publication: %v", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(prefix.Fd()), "artifact.tar.gz", &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	if stat.Nlink != 1 {
		t.Fatalf("published nlink = %d, want 1", stat.Nlink)
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

func TestLockNameReplacementCannotSplitSynchronization(t *testing.T) {
	root := t.TempDir()
	store, err := New(root, 1<<20)
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
	defer unlock()
	lockPath := filepath.Join(root, "sha256", "aa", ".lock")
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if release, err := lock(ctx, second); !errors.Is(err, context.DeadlineExceeded) {
		if err == nil {
			release()
		}
		t.Fatalf("lock after lock-name replacement error = %v, want DeadlineExceeded", err)
	}
}

func TestPrefixReachabilityRejectsRenamedDirectory(t *testing.T) {
	root := t.TempDir()
	store, err := New(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	prefix, _, err := store.prefix("aa")
	if err != nil {
		t.Fatal(err)
	}
	defer prefix.Close()
	path := filepath.Join(root, "sha256", "aa")
	if err := os.Rename(path, path+".detached"); err != nil {
		t.Fatal(err)
	}
	if err := checkDirReachable(store.sha, "aa", prefix); !errors.Is(err, artifact.ErrDigestMismatch) {
		t.Fatalf("checkDirReachable() error = %v, want ErrDigestMismatch", err)
	}
}

func TestOpenPrefixAlwaysSyncsParentBeforeUse(t *testing.T) {
	store, err := New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	syncs := 0
	syncParent := func() error {
		syncs++
		return nil
	}
	first, _, err := openPrefixAt(store.sha, "aa", syncParent)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, _, err := openPrefixAt(store.sha, "aa", syncParent)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	if syncs != 2 {
		t.Fatalf("parent sync calls = %d, want one for new and one for existing prefix", syncs)
	}
}

func TestRollbackPublicationSurfacesCauseUnlinkAndSyncErrors(t *testing.T) {
	cause := errors.New("verification failed")
	unlinkErr := errors.New("unlink failed")
	syncErr := errors.New("sync failed")
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	err = rollbackPublicationWith(
		directory,
		"artifact.tar.gz",
		cause,
		func(int, string, int) error { return unlinkErr },
		func(*os.File) error { return syncErr },
	)
	for _, want := range []error{cause, unlinkErr, syncErr} {
		if !errors.Is(err, want) {
			t.Fatalf("rollback error %v does not contain %v", err, want)
		}
	}
}

func TestReadAndDecompressPreservesCancellationAfterCompressedRead(t *testing.T) {
	compressed, err := gzipBytes(context.Background(), bytes.Repeat([]byte("evidence"), 1024), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelAfterRead{data: compressed, cancel: cancel}
	if _, err := readAndDecompress(ctx, reader, 1<<20, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("readAndDecompress() error = %v, want context.Canceled", err)
	}
}

func TestReadBoundedPreservesCancellationFromReadBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := cancelingErrorReader{cancel: cancel}
	if _, err := readBounded(ctx, reader, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("readBounded() error = %v, want context.Canceled", err)
	}
}

type cancelingErrorReader struct {
	cancel context.CancelFunc
}

func (reader cancelingErrorReader) Read([]byte) (int, error) {
	reader.cancel()
	return 0, errors.New("read interrupted")
}

type cancelAfterRead struct {
	data   []byte
	cancel context.CancelFunc
}

func (r *cancelAfterRead) Read(buffer []byte) (int, error) {
	count := copy(buffer, r.data)
	r.data = r.data[count:]
	if len(r.data) == 0 {
		r.cancel()
		return count, io.EOF
	}
	return count, nil
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
