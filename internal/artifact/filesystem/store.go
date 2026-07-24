// Package filesystem persists canonical bundles using descriptor-anchored Unix operations.
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
	"os"
	"path/filepath"
	"time"

	"github.com/araihu/paje/internal/artifact"
	"golang.org/x/sys/unix"
)

type Store struct {
	root, tmp, sha                 *os.File
	maxCompressed, maxUncompressed int64
}

var _ artifact.Store = (*Store)(nil)

func New(root string, max int64) (*Store, error) {
	if max <= 0 || max > int64(^uint64(0)>>1)/16 {
		return nil, fmt.Errorf("create artifact filesystem store: invalid compressed limit")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	rootFile, err := openRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open artifact root: %w", err)
	}
	tmp, _, err := openDirAt(int(rootFile.Fd()), "tmp", true)
	if err != nil {
		rootFile.Close()
		return nil, err
	}
	sha, _, err := openDirAt(int(rootFile.Fd()), "sha256", true)
	if err != nil {
		tmp.Close()
		rootFile.Close()
		return nil, err
	}
	if err := rootFile.Sync(); err != nil {
		sha.Close()
		tmp.Close()
		rootFile.Close()
		return nil, err
	}
	return &Store{root: rootFile, tmp: tmp, sha: sha, maxCompressed: max, maxUncompressed: max * 16}, nil
}

// Close releases all held directory descriptors.
func (s *Store) Close() error { return errors.Join(s.sha.Close(), s.tmp.Close(), s.root.Close()) }

func (s *Store) Save(ctx context.Context, bundle artifact.Bundle) (artifact.Reference, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	_, tarBytes, ref, err := artifact.CanonicalizeLimited(ctx, bundle, s.maxUncompressed)
	if err != nil {
		return artifact.Reference{}, err
	}
	name, temp, err := s.createTemp()
	if err != nil {
		return artifact.Reference{}, err
	}
	defer temp.Close()
	cleanup := true
	defer func() {
		if cleanup {
			_ = unix.Unlinkat(int(s.tmp.Fd()), name, 0)
			_ = s.tmp.Sync()
		}
	}()
	if err := writeGzip(ctx, temp, tarBytes, s.maxCompressed); err != nil {
		return artifact.Reference{}, err
	}
	if err := temp.Sync(); err != nil {
		return artifact.Reference{}, err
	}
	if err := checkFile(temp); err != nil {
		return artifact.Reference{}, err
	}
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	prefix, created, err := s.prefix(ref.Digest[:2])
	if err != nil {
		return artifact.Reference{}, err
	}
	defer prefix.Close()
	if created {
		if err := s.sha.Sync(); err != nil {
			return artifact.Reference{}, err
		}
	}
	unlock, err := lock(ctx, prefix)
	if err != nil {
		return artifact.Reference{}, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return artifact.Reference{}, err
	}
	final := ref.Digest + ".tar.gz"
	published, err := linkTemp(s.tmp, name, temp, prefix, final)
	if err != nil {
		return artifact.Reference{}, err
	}
	if published {
		if err := prefix.Sync(); err != nil {
			return artifact.Reference{}, err
		}
		if err := unix.Unlinkat(int(s.tmp.Fd()), name, 0); err != nil {
			return artifact.Reference{}, err
		}
		if err := s.tmp.Sync(); err != nil {
			return artifact.Reference{}, err
		}
		cleanup = false
	}
	if err := s.verifyFinal(ctx, prefix, final, tarBytes, ref); err != nil {
		if published {
			_ = unix.Unlinkat(int(prefix.Fd()), final, 0)
			_ = prefix.Sync()
		}
		return artifact.Reference{}, err
	}
	if cleanup {
		if err := unix.Unlinkat(int(s.tmp.Fd()), name, 0); err != nil {
			return artifact.Reference{}, fmt.Errorf("cleanup temp: %w", err)
		}
		if err := s.tmp.Sync(); err != nil {
			return artifact.Reference{}, err
		}
		cleanup = false
	}
	return ref, nil
}

func (s *Store) Load(ctx context.Context, ref artifact.Reference) (artifact.Bundle, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Bundle{}, err
	}
	if err := validRef(ref); err != nil {
		return artifact.Bundle{}, err
	}
	prefix, _, err := s.prefix(ref.Digest[:2])
	if err != nil {
		return artifact.Bundle{}, err
	}
	defer prefix.Close()
	unlock, err := lock(ctx, prefix)
	if err != nil {
		return artifact.Bundle{}, err
	}
	defer unlock()
	file, err := openFileAt(int(prefix.Fd()), ref.Digest+".tar.gz")
	if err != nil {
		return artifact.Bundle{}, err
	}
	defer file.Close()
	tarBytes, err := readAndDecompress(ctx, file, s.maxCompressed, s.maxUncompressed)
	if err != nil {
		return artifact.Bundle{}, err
	}
	if int64(len(tarBytes)) != ref.Size || artifact.DigestTar(tarBytes) != ref.Digest {
		return artifact.Bundle{}, artifact.ErrDigestMismatch
	}
	bundle, err := artifact.DecodeCanonicalTar(tarBytes)
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
		fd, err := unix.Openat(int(s.tmp.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return name, os.NewFile(uintptr(fd), name), nil
	}
	return "", nil, fmt.Errorf("create unique artifact temp")
}
func (s *Store) prefix(name string) (*os.File, bool, error) {
	return openDirAt(int(s.sha.Fd()), name, true)
}
func validRef(ref artifact.Reference) error {
	if ref.RunID == "" || ref.Size < 0 || !artifact.ValidDigest(ref.Digest) {
		return artifact.ErrInvalidReference
	}
	return nil
}

func openRoot(name string) (*os.File, error) {
	fd, err := unix.Openat(unix.AT_FDCWD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || int(stat.Uid) != os.Geteuid() {
		file.Close()
		return nil, fmt.Errorf("unsafe artifact root")
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		file.Close()
		return nil, err
	}
	if err := checkDir(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func openDirAt(parent int, name string, create bool) (*os.File, bool, error) {
	created := false
	if create {
		if err := unix.Mkdirat(parent, name, 0o700); err == nil {
			created = true
		} else if !errors.Is(err, unix.EEXIST) {
			return nil, false, err
		}
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), name)
	if err := checkDir(file); err != nil {
		file.Close()
		return nil, false, err
	}
	return file, created, nil
}
func openFileAt(parent int, name string) (*os.File, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if err := checkFile(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
func checkDir(file *os.File) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o077 != 0 || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("unsafe artifact directory")
	}
	return nil
}
func checkFile(file *os.File) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0o777 != 0o600 || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
		return artifact.ErrDigestMismatch
	}
	return nil
}
func lock(ctx context.Context, prefix *os.File) (func(), error) {
	fd, err := unix.Openat(int(prefix.Fd()), ".lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), ".lock")
	if err := checkFile(file); err != nil {
		file.Close()
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func linkTemp(tmp *os.File, name string, temp *os.File, prefix *os.File, final string) (bool, error) {
	if err := unix.Linkat(int(tmp.Fd()), name, int(prefix.Fd()), final, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return false, nil
		}
		return false, fmt.Errorf("publish artifact: %w", err)
	}

	var expected unix.Stat_t
	if err := unix.Fstat(int(temp.Fd()), &expected); err != nil {
		return false, rollbackPublication(prefix, final, err)
	}
	fd, err := unix.Openat(int(prefix.Fd()), final, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, rollbackPublication(prefix, final, err)
	}
	var actual unix.Stat_t
	statErr := unix.Fstat(fd, &actual)
	closeErr := unix.Close(fd)
	if statErr != nil {
		return false, rollbackPublication(prefix, final, statErr)
	}
	if closeErr != nil {
		return false, rollbackPublication(prefix, final, closeErr)
	}
	if expected.Dev != actual.Dev || expected.Ino != actual.Ino {
		return false, rollbackPublication(prefix, final, artifact.ErrDigestMismatch)
	}
	return true, nil
}

func rollbackPublication(prefix *os.File, final string, cause error) error {
	unlinkErr := unix.Unlinkat(int(prefix.Fd()), final, 0)
	syncErr := prefix.Sync()
	return errors.Join(cause, unlinkErr, syncErr)
}

func (s *Store) verifyFinal(ctx context.Context, prefix *os.File, name string, expected []byte, ref artifact.Reference) error {
	file, err := openFileAt(int(prefix.Fd()), name)
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
func writeGzip(ctx context.Context, file io.Writer, data []byte, max int64) error {
	writer, err := gzip.NewWriterLevel(&limitedWriter{writer: file, limit: max}, 6)
	if err != nil {
		return err
	}
	writer.Header.ModTime = time.Unix(0, 0).UTC()
	writer.Header.OS = 255
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			writer.Close()
			return err
		}
		n := len(data)
		if n > 32*1024 {
			n = 32 * 1024
		}
		if _, err := writer.Write(data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return writer.Close()
}
func readAndDecompress(ctx context.Context, file *os.File, maxC, maxU int64) ([]byte, error) {
	compressed, err := readBounded(ctx, file, maxC)
	if err != nil {
		return nil, err
	}
	source := bytes.NewReader(compressed)
	r, err := gzip.NewReader(source)
	if err != nil {
		return nil, artifact.ErrDigestMismatch
	}
	if r.Name != "" || r.Comment != "" || len(r.Extra) != 0 || r.OS != 255 {
		r.Close()
		return nil, artifact.ErrDigestMismatch
	}
	r.Multistream(false)
	plain, err := readBounded(ctx, r, maxU)
	closeErr := r.Close()
	if err != nil || closeErr != nil || source.Len() != 0 {
		return nil, artifact.ErrDigestMismatch
	}
	re, err := gzipBytes(ctx, plain, maxC)
	if err != nil || !bytes.Equal(re, compressed) {
		return nil, artifact.ErrDigestMismatch
	}
	return plain, nil
}
func gzipBytes(ctx context.Context, data []byte, max int64) ([]byte, error) {
	var b bytes.Buffer
	err := writeGzip(ctx, &b, data, max)
	return b.Bytes(), err
}
func readBounded(ctx context.Context, r io.Reader, limit int64) ([]byte, error) {
	var b bytes.Buffer
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if int64(n) > limit-int64(b.Len()) {
				return nil, artifact.ErrTooLarge
			}
			b.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			return b.Bytes(), nil
		}
		if err != nil {
			return nil, artifact.ErrDigestMismatch
		}
	}
}
func randomName() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return ".artifact-" + hex.EncodeToString(b), nil
}

type limitedWriter struct {
	writer         io.Writer
	limit, written int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.written {
		return 0, artifact.ErrTooLarge
	}
	n, e := w.writer.Write(p)
	w.written += int64(n)
	return n, e
}
