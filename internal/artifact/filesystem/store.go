// Package filesystem persists canonical artifact bundles on a durable filesystem.
package filesystem

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/araihu/paje/internal/artifact"
)

// Store keeps descriptor-anchored roots so path replacement cannot redirect
// artifact operations outside the configured store.
type Store struct {
	root            *os.Root
	maxCompressed   int64
	maxUncompressed int64
}

var _ artifact.Store = (*Store)(nil)

// New creates a secure descriptor-anchored filesystem artifact store.
func New(root string, maxCompressedBytes int64) (*Store, error) {
	if maxCompressedBytes <= 0 || maxCompressedBytes > int64(^uint64(0)>>1)/16 {
		return nil, fmt.Errorf("create artifact filesystem store: invalid compressed limit")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("create artifact filesystem store: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact filesystem store: root: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("create artifact filesystem store: unsafe root")
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact filesystem store: secure root: %w", err)
	}
	anchored, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("create artifact filesystem store: open root: %w", err)
	}
	if err := checkDirectory(anchored, "."); err != nil {
		anchored.Close()
		return nil, err
	}
	tmpRoot, err := ensureDirectory(anchored, "tmp")
	if err != nil {
		anchored.Close()
		return nil, err
	}
	tmpRoot.Close()
	shaRoot, err := ensureDirectory(anchored, "sha256")
	if err != nil {
		anchored.Close()
		return nil, err
	}
	shaRoot.Close()
	if err := syncRoot(anchored); err != nil {
		anchored.Close()
		return nil, err
	}
	return &Store{root: anchored, maxCompressed: maxCompressedBytes, maxUncompressed: maxCompressedBytes * 16}, nil
}

// Save produces a bounded canonical stream and atomically publishes it only if
// no process has already published the same verified content.
func (s *Store) Save(ctx context.Context, bundle artifact.Bundle) (artifact.Reference, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	_, canonicalTar, ref, err := artifact.CanonicalizeLimited(ctx, bundle, s.maxUncompressed)
	if err != nil {
		return artifact.Reference{}, err
	}
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	tempName, file, err := s.createTemp()
	if err != nil {
		return artifact.Reference{}, err
	}
	defer func() {
		if tempName != "" {
			_ = s.root.Remove(path.Join("tmp", tempName))
			_ = syncChild(s.root, "tmp")
		}
	}()
	if err := writeGzip(ctx, file, canonicalTar, s.maxCompressed); err != nil {
		file.Close()
		return artifact.Reference{}, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return artifact.Reference{}, fmt.Errorf("save artifact: sync temp: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return artifact.Reference{}, fmt.Errorf("save artifact: stat temp: %w", err)
	}
	if err := checkRegular(info); err != nil {
		file.Close()
		return artifact.Reference{}, err
	}
	if err := file.Close(); err != nil {
		return artifact.Reference{}, fmt.Errorf("save artifact: close temp: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	prefix, destination, err := s.destination(ref)
	if err != nil {
		return artifact.Reference{}, err
	}
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	// Root.Link is an atomic no-replace publish primitive across processes.
	err = s.root.Link(path.Join("tmp", tempName), path.Join("sha256", prefix, destination))
	if err == nil {
		if err := syncChild(s.root, path.Join("sha256", prefix)); err != nil {
			return artifact.Reference{}, err
		}
		if err := s.root.Remove(path.Join("tmp", tempName)); err != nil {
			return artifact.Reference{}, fmt.Errorf("save artifact: remove published temp: %w", err)
		}
		if err := syncChild(s.root, "tmp"); err != nil {
			return artifact.Reference{}, err
		}
		tempName = ""
		return ref, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return artifact.Reference{}, fmt.Errorf("save artifact: publish: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	if err := s.verifyExistingAfterPublish(ctx, prefix, destination, canonicalTar, ref); err != nil {
		return artifact.Reference{}, err
	}
	return ref, nil
}

func (s *Store) verifyExistingAfterPublish(ctx context.Context, prefix, name string, expected []byte, ref artifact.Reference) error {
	for attempt := 0; attempt < 50; attempt++ {
		err := s.verifyExisting(ctx, prefix, name, expected, ref)
		if !errors.Is(err, artifact.ErrDigestMismatch) {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return artifact.ErrDigestMismatch
}

// Load opens only descriptor-anchored, digest-derived names and verifies every
// storage and bundle invariant before returning an independent copy.
func (s *Store) Load(ctx context.Context, ref artifact.Reference) (artifact.Bundle, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Bundle{}, err
	}
	prefix, destination, err := s.destination(ref)
	if err != nil {
		return artifact.Bundle{}, err
	}
	file, err := s.openFinal(prefix, destination)
	if err != nil {
		return artifact.Bundle{}, err
	}
	defer file.Close()
	canonicalTar, err := readAndDecompress(ctx, file, s.maxCompressed, s.maxUncompressed)
	if err != nil {
		return artifact.Bundle{}, err
	}
	if int64(len(canonicalTar)) != ref.Size || artifact.DigestTar(canonicalTar) != ref.Digest {
		return artifact.Bundle{}, artifact.ErrDigestMismatch
	}
	bundle, err := artifact.DecodeCanonicalTar(canonicalTar)
	if err != nil {
		return artifact.Bundle{}, err
	}
	if bundle.Manifest.RunID != ref.RunID {
		return artifact.Bundle{}, artifact.ErrDigestMismatch
	}
	return artifact.CloneBundle(bundle), nil
}

func (s *Store) createTemp() (string, *os.File, error) {
	for range 32 {
		name, err := randomName()
		if err != nil {
			return "", nil, err
		}
		file, err := s.root.OpenFile(path.Join("tmp", name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("save artifact: create temp: %w", err)
		}
		return name, file, nil
	}
	return "", nil, fmt.Errorf("save artifact: create unique temp")
}

func (s *Store) destination(ref artifact.Reference) (string, string, error) {
	if ref.RunID == "" || ref.Size < 0 || !artifact.ValidDigest(ref.Digest) {
		return "", "", artifact.ErrInvalidReference
	}
	prefix := ref.Digest[:2]
	shaRoot, err := ensureDirectory(s.root, "sha256")
	if err != nil {
		return "", "", err
	}
	shaRoot.Close()
	prefixRoot, err := ensureDirectory(s.root, path.Join("sha256", prefix))
	if err != nil {
		return "", "", err
	}
	prefixRoot.Close()
	return prefix, ref.Digest + ".tar.gz", nil
}

func (s *Store) openFinal(prefix, name string) (*os.File, error) {
	rel := path.Join("sha256", prefix, name)
	info, err := s.root.Lstat(rel)
	if err != nil {
		return nil, fmt.Errorf("load artifact: open: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, artifact.ErrDigestMismatch
	}
	file, err := s.root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("load artifact: open: %w", err)
	}
	if info, err := file.Stat(); err != nil || checkRegular(info) != nil {
		file.Close()
		return nil, artifact.ErrDigestMismatch
	}
	return file, nil
}

func (s *Store) verifyExisting(ctx context.Context, prefix, name string, expected []byte, ref artifact.Reference) error {
	file, err := s.openFinal(prefix, name)
	if err != nil {
		return err
	}
	defer file.Close()
	actual, err := readAndDecompress(ctx, file, s.maxCompressed, s.maxUncompressed)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) || int64(len(actual)) != ref.Size || artifact.DigestTar(actual) != ref.Digest {
		return artifact.ErrDigestMismatch
	}
	return nil
}

func readAndDecompress(ctx context.Context, file *os.File, maxCompressed, maxUncompressed int64) ([]byte, error) {
	compressed, err := readBounded(ctx, file, maxCompressed)
	if err != nil {
		return nil, err
	}
	source := bytes.NewReader(compressed)
	reader, err := gzip.NewReader(source)
	if err != nil {
		return nil, artifact.ErrDigestMismatch
	}
	if reader.Name != "" || reader.Comment != "" || len(reader.Extra) != 0 || (!reader.ModTime.IsZero() && !reader.ModTime.Equal(time.Unix(0, 0).UTC())) || reader.OS != 255 {
		reader.Close()
		return nil, artifact.ErrDigestMismatch
	}
	reader.Multistream(false)
	uncompressed, err := readBounded(ctx, reader, maxUncompressed)
	if closeErr := reader.Close(); err != nil || closeErr != nil || source.Len() != 0 {
		return nil, artifact.ErrDigestMismatch
	}
	reencoded, err := gzipBytes(ctx, uncompressed, maxCompressed)
	if err != nil || !bytes.Equal(compressed, reencoded) {
		return nil, artifact.ErrDigestMismatch
	}
	return uncompressed, nil
}

func writeGzip(ctx context.Context, file io.Writer, data []byte, max int64) error {
	writer, err := gzip.NewWriterLevel(&limitedWriter{writer: file, limit: max}, 6)
	if err != nil {
		return fmt.Errorf("save artifact: create gzip: %w", err)
	}
	writer.Header.ModTime = time.Unix(0, 0).UTC()
	writer.Header.OS = 255
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			writer.Close()
			return err
		}
		size := len(data)
		if size > 32*1024 {
			size = 32 * 1024
		}
		if _, err := writer.Write(data[:size]); err != nil {
			writer.Close()
			if errors.Is(err, artifact.ErrTooLarge) {
				return artifact.ErrTooLarge
			}
			return fmt.Errorf("save artifact: write gzip: %w", err)
		}
		data = data[size:]
	}
	if err := writer.Close(); err != nil {
		if errors.Is(err, artifact.ErrTooLarge) {
			return artifact.ErrTooLarge
		}
		return fmt.Errorf("save artifact: close gzip: %w", err)
	}
	return nil
}

func gzipBytes(ctx context.Context, data []byte, max int64) ([]byte, error) {
	var out bytes.Buffer
	if err := writeGzip(ctx, nopSyncFile{Writer: &out}, data, max); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func readBounded(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	var out bytes.Buffer
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, err := reader.Read(buffer)
		if count > 0 {
			if int64(out.Len()+count) > limit {
				return nil, artifact.ErrTooLarge
			}
			_, _ = out.Write(buffer[:count])
		}
		if errors.Is(err, io.EOF) {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, artifact.ErrDigestMismatch
		}
	}
}

func ensureDirectory(root *os.Root, name string) (*os.Root, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		info, err = root.Lstat(name)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("unsafe artifact directory %q", name)
	}
	child, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	if err := checkDirectory(child, "."); err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}
func checkDirectory(root *os.Root, name string) error {
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("unsafe artifact directory %q: %w", name, err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("unsafe artifact directory %q: %v", name, info.Mode())
	}
	return nil
}
func checkRegular(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return artifact.ErrDigestMismatch
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && (stat.Nlink != 1 || int(stat.Uid) != os.Geteuid()) {
		return artifact.ErrDigestMismatch
	}
	return nil
}
func syncRoot(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
func syncChild(root *os.Root, name string) error {
	child, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer child.Close()
	return syncRoot(child)
}
func randomName() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return ".artifact-" + hex.EncodeToString(bytes), nil
}

type limitedWriter struct {
	writer  io.Writer
	limit   int64
	written int64
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	remaining := w.limit - w.written
	if remaining <= 0 {
		return 0, artifact.ErrTooLarge
	}
	if int64(len(data)) > remaining {
		count, err := w.writer.Write(data[:remaining])
		w.written += int64(count)
		if err != nil {
			return count, err
		}
		return count, artifact.ErrTooLarge
	}
	count, err := w.writer.Write(data)
	w.written += int64(count)
	return count, err
}

type nopSyncFile struct{ io.Writer }

func (nopSyncFile) Sync() error { return nil }
