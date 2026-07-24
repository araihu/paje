// Package filesystem persists canonical artifact bundles on a durable filesystem.
package filesystem

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/araihu/paje/internal/artifact"
)

var rootLocks sync.Map // map[string]*sync.Mutex, coordinating stores in-process.

// Store is a content-addressed, filesystem-backed artifact store.
type Store struct {
	root            string
	tmp             string
	maxCompressed   int64
	maxUncompressed int64
	lock            *sync.Mutex
}

var _ artifact.Store = (*Store)(nil)

// New creates a filesystem artifact store rooted at root.
func New(root string, maxCompressedBytes int64) (*Store, error) {
	if maxCompressedBytes <= 0 || maxCompressedBytes > int64(^uint64(0)>>1)/16 {
		return nil, fmt.Errorf("create artifact filesystem store: invalid compressed limit")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("create artifact filesystem store: resolve root: %w", err)
	}
	if err := mkdirSecure(absRoot); err != nil {
		return nil, fmt.Errorf("create artifact filesystem store: root: %w", err)
	}
	tmp := filepath.Join(absRoot, "tmp")
	shaRoot := filepath.Join(absRoot, "sha256")
	if err := mkdirSecure(tmp); err != nil {
		return nil, fmt.Errorf("create artifact filesystem store: tmp: %w", err)
	}
	if err := mkdirSecure(shaRoot); err != nil {
		return nil, fmt.Errorf("create artifact filesystem store: sha256: %w", err)
	}
	if err := syncDir(absRoot); err != nil {
		return nil, fmt.Errorf("create artifact filesystem store: sync root: %w", err)
	}
	lock, _ := rootLocks.LoadOrStore(absRoot, &sync.Mutex{})
	return &Store{root: absRoot, tmp: tmp, maxCompressed: maxCompressedBytes, maxUncompressed: maxCompressedBytes * 16, lock: lock.(*sync.Mutex)}, nil
}

// Save canonically encodes bundle before atomically persisting it.
func (s *Store) Save(ctx context.Context, bundle artifact.Bundle) (artifact.Reference, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	_, canonicalTar, reference, err := artifact.Canonicalize(bundle)
	if err != nil {
		return artifact.Reference{}, err
	}
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	temp, err := os.CreateTemp(s.tmp, ".artifact-*")
	if err != nil {
		return artifact.Reference{}, fmt.Errorf("save artifact: create temp: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := writeGzip(temp, canonicalTar); err != nil {
		_ = temp.Close()
		return artifact.Reference{}, err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return artifact.Reference{}, fmt.Errorf("save artifact: sync temp: %w", err)
	}
	info, err := temp.Stat()
	if err != nil {
		_ = temp.Close()
		return artifact.Reference{}, fmt.Errorf("save artifact: stat temp: %w", err)
	}
	if err := temp.Close(); err != nil {
		return artifact.Reference{}, fmt.Errorf("save artifact: close temp: %w", err)
	}
	if info.Size() > s.maxCompressed {
		return artifact.Reference{}, artifact.ErrTooLarge
	}
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}

	s.lock.Lock()
	defer s.lock.Unlock()
	destination, err := s.pathFor(reference)
	if err != nil {
		return artifact.Reference{}, err
	}
	if _, err := os.Lstat(destination); err == nil {
		if err := s.verifyExisting(ctx, destination, canonicalTar, reference); err != nil {
			return artifact.Reference{}, err
		}
		return reference, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return artifact.Reference{}, fmt.Errorf("save artifact: inspect destination: %w", err)
	}
	if err := os.Rename(tempName, destination); err != nil {
		return artifact.Reference{}, fmt.Errorf("save artifact: rename: %w", err)
	}
	if err := syncDir(filepath.Dir(destination)); err != nil {
		return artifact.Reference{}, err
	}
	if err := syncDir(s.tmp); err != nil {
		return artifact.Reference{}, err
	}
	return reference, nil
}

// Load validates and returns an independent copy of the referenced bundle.
func (s *Store) Load(ctx context.Context, reference artifact.Reference) (artifact.Bundle, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Bundle{}, err
	}
	path, err := s.pathFor(reference)
	if err != nil {
		return artifact.Bundle{}, err
	}
	canonicalTar, err := readAndDecompress(ctx, path, s.maxCompressed, s.maxUncompressed)
	if err != nil {
		return artifact.Bundle{}, err
	}
	got, err := artifact.ReferenceForTar(canonicalTar)
	if err != nil {
		return artifact.Bundle{}, err
	}
	if got.Digest != reference.Digest || got.Size != reference.Size {
		return artifact.Bundle{}, artifact.ErrDigestMismatch
	}
	bundle, err := artifact.DecodeCanonicalTar(canonicalTar)
	if err != nil {
		return artifact.Bundle{}, err
	}
	if bundle.Manifest.RunID != reference.RunID {
		return artifact.Bundle{}, artifact.ErrDigestMismatch
	}
	return artifact.CloneBundle(bundle), nil
}

func (s *Store) verifyExisting(ctx context.Context, path string, canonicalTar []byte, reference artifact.Reference) error {
	existing, err := readAndDecompress(ctx, path, s.maxCompressed, s.maxUncompressed)
	if err != nil {
		return err
	}
	if !bytes.Equal(existing, canonicalTar) {
		return artifact.ErrDigestMismatch
	}
	got, err := artifact.ReferenceForTar(existing)
	if err != nil || got != reference {
		return artifact.ErrDigestMismatch
	}
	_, err = artifact.DecodeCanonicalTar(existing)
	return err
}

func (s *Store) pathFor(reference artifact.Reference) (string, error) {
	if reference.RunID == "" || reference.Size < 0 || !artifact.ValidDigest(reference.Digest) {
		return "", artifact.ErrInvalidReference
	}
	directory := filepath.Join(s.root, "sha256", reference.Digest[:2])
	if err := mkdirSecure(directory); err != nil {
		return "", fmt.Errorf("artifact path: %w", err)
	}
	if err := syncDir(filepath.Dir(directory)); err != nil {
		return "", err
	}
	return filepath.Join(directory, reference.Digest+".tar.gz"), nil
}

func writeGzip(file *os.File, data []byte) error {
	writer, err := gzip.NewWriterLevel(file, 6)
	if err != nil {
		return fmt.Errorf("save artifact: create gzip: %w", err)
	}
	writer.Header.ModTime = time.Unix(0, 0).UTC()
	writer.Header.Name = ""
	writer.Header.Comment = ""
	writer.Header.OS = 255
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return fmt.Errorf("save artifact: write gzip: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("save artifact: close gzip: %w", err)
	}
	return nil
}

func readAndDecompress(ctx context.Context, path string, maxCompressed, maxUncompressed int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("load artifact: open: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, artifact.ErrDigestMismatch
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("load artifact: open: %w", err)
	}
	defer file.Close()
	compressed, err := readBounded(ctx, file, maxCompressed)
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, artifact.ErrDigestMismatch
	}
	defer reader.Close()
	uncompressed, err := readBounded(ctx, reader, maxUncompressed)
	if err != nil {
		return nil, err
	}
	if err := reader.Close(); err != nil {
		return nil, artifact.ErrDigestMismatch
	}
	return uncompressed, nil
}

func readBounded(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	var out bytes.Buffer
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		read, err := reader.Read(buffer)
		if read > 0 {
			if int64(out.Len()+read) > limit {
				return nil, artifact.ErrTooLarge
			}
			_, _ = out.Write(buffer[:read])
		}
		if errors.Is(err, io.EOF) {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, artifact.ErrDigestMismatch
		}
	}
}

func mkdirSecure(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("unsafe directory")
	}
	return os.Chmod(path, 0o700)
}

func syncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
