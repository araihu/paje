package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/araihu/paje/internal/secret"
	"golang.org/x/sys/unix"
)

func TestFilesystemReadsPrivateRegularFileAndDirectoryTree(t *testing.T) {
	root := privateRoot(t)
	file := filepath.Join(root, "token")
	writePrivateFile(t, file, []byte("secret"))
	directory := filepath.Join(root, "codex")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(directory, "auth.json"), []byte(`{"token":"secret"}`))

	provider := newFilesystem(t, root, 1024, 8)
	payload, err := provider.Read(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Kind() != "value" || !bytes.Equal(payload.Value(), []byte("secret")) {
		t.Fatalf("file payload = %s %q", payload.Kind(), payload.Value())
	}
	payload, err = provider.Read(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	files := payload.Files()
	if payload.Kind() != "directory" || len(files) != 1 || files[0].Path() != "auth.json" ||
		!bytes.Equal(files[0].Bytes(), []byte(`{"token":"secret"}`)) {
		t.Fatalf("directory payload = %s %#v", payload.Kind(), files)
	}
	contents := files[0].Bytes()
	contents[0] = 'X'
	if bytes.Equal(payload.Files()[0].Bytes(), contents) {
		t.Fatal("directory payload aliases caller bytes")
	}
}

func TestFilesystemRejectsEscapesSymlinksUnsafeModesAndSpecialFiles(t *testing.T) {
	root := privateRoot(t)
	outside := filepath.Join(t.TempDir(), "outside")
	writePrivateFile(t, outside, []byte("outside"))
	symlink := filepath.Join(root, "link")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(root, "public")
	if err := os.WriteFile(public, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "tree")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "nested-link")); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	provider := newFilesystem(t, root, 1024, 8)
	for name, target := range map[string]string{
		"outside root": outside,
		"symlink":      symlink,
		"public mode":  public,
		"nested link":  directory,
		"fifo":         fifo,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := provider.Read(context.Background(), target); err == nil {
				t.Fatal("unsafe filesystem source accepted")
			}
		})
	}
}

func TestFilesystemEnforcesByteAndEntryLimits(t *testing.T) {
	root := privateRoot(t)
	large := filepath.Join(root, "large")
	writePrivateFile(t, large, []byte("12345"))
	provider := newFilesystem(t, root, 4, 8)
	if _, err := provider.Read(context.Background(), large); err == nil {
		t.Fatal("oversized file accepted")
	}

	directory := filepath.Join(root, "tree")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(directory, "one"), []byte("1"))
	writePrivateFile(t, filepath.Join(directory, "two"), []byte("2"))
	provider = newFilesystem(t, root, 32, 1)
	if _, err := provider.Read(context.Background(), directory); err == nil {
		t.Fatal("oversized directory tree accepted")
	}

	opened, err := os.Open(large)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(opened.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(opened, &stat, 0); !errors.Is(err, secret.ErrSourceLimit) {
		t.Fatalf("exhausted aggregate budget error = %v", err)
	}
}

func TestBoundedDirectoryReadStopsAtOneEntryOverLimit(t *testing.T) {
	directory := t.TempDir()
	for index := range 66 {
		writePrivateFile(t, filepath.Join(directory, fmt.Sprintf("entry-%03d", index)), []byte("secret"))
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(entries)
	reader := &recordingDirectoryReader{entries: entries}

	if _, err := readBoundedDirectoryEntries(reader, 65); !errors.Is(err, secret.ErrSourceLimit) {
		t.Fatalf("readBoundedDirectoryEntries() error = %v", err)
	}
	if reader.offset != 66 {
		t.Fatalf("directory entries consumed = %d, want MaxEntries+1", reader.offset)
	}
	if len(reader.requests) < 2 {
		t.Fatalf("directory was not read in bounded chunks: requests=%v", reader.requests)
	}
	for _, request := range reader.requests {
		if request <= 0 || request > 65 {
			t.Fatalf("unbounded directory read request: %v", reader.requests)
		}
	}
}

func TestBoundedDirectoryReadSortsAcceptedEntries(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		writePrivateFile(t, filepath.Join(directory, name), []byte("secret"))
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(entries)
	reader := &recordingDirectoryReader{entries: entries}

	got, err := readBoundedDirectoryEntries(reader, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "bravo", "charlie"}
	for index, entry := range got {
		if entry.Name() != want[index] {
			t.Fatalf("entry %d = %q, want %q", index, entry.Name(), want[index])
		}
	}
}

type recordingDirectoryReader struct {
	entries  []os.DirEntry
	offset   int
	requests []int
}

func (reader *recordingDirectoryReader) ReadDir(count int) ([]os.DirEntry, error) {
	reader.requests = append(reader.requests, count)
	if count <= 0 {
		return nil, errors.New("unbounded directory read")
	}
	if reader.offset == len(reader.entries) {
		return nil, io.EOF
	}
	end := min(reader.offset+count, len(reader.entries))
	entries := reader.entries[reader.offset:end]
	reader.offset = end
	return entries, nil
}

func privateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func writePrivateFile(t *testing.T, filename string, value []byte) {
	t.Helper()
	if err := os.WriteFile(filename, value, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newFilesystem(t *testing.T, root string, maxBytes int64, maxEntries int) *Filesystem {
	t.Helper()
	provider, err := NewFilesystem(FilesystemConfig{
		AllowedRoots: []string{root}, MaxBytes: maxBytes, MaxEntries: maxEntries, OwnerUID: os.Geteuid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
