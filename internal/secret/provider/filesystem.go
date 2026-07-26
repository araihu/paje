package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/araihu/paje/internal/secret"
	"golang.org/x/sys/unix"
)

type FilesystemConfig struct {
	AllowedRoots []string
	MaxBytes     int64
	MaxEntries   int
	OwnerUID     int
}

type Filesystem struct {
	roots      []string
	maxBytes   int64
	maxEntries int
	ownerUID   uint32
}

func NewFilesystem(config FilesystemConfig) (*Filesystem, error) {
	if len(config.AllowedRoots) == 0 || config.MaxBytes <= 0 || config.MaxEntries <= 0 || config.OwnerUID < 0 {
		return nil, errors.New("filesystem secret roots, bounds, and owner are required")
	}
	roots := make([]string, 0, len(config.AllowedRoots))
	seen := make(map[string]struct{}, len(config.AllowedRoots))
	for _, root := range config.AllowedRoots {
		if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return nil, errors.New("filesystem secret root is invalid")
		}
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("filesystem secret root is invalid")
		}
		if _, duplicate := seen[root]; duplicate {
			return nil, errors.New("filesystem secret root is duplicated")
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	return &Filesystem{
		roots: roots, maxBytes: config.MaxBytes, maxEntries: config.MaxEntries, ownerUID: uint32(config.OwnerUID),
	}, nil
}

func (provider *Filesystem) Read(ctx context.Context, reference string) (secret.Payload, error) {
	if err := ctx.Err(); err != nil {
		return secret.Payload{}, err
	}
	root, relative, ok := provider.anchor(reference)
	if !ok {
		return secret.Payload{}, secret.ErrSourceInvalid
	}
	file, stat, err := openAnchored(root, relative)
	if err != nil {
		return secret.Payload{}, fmt.Errorf("%w: open source", secret.ErrSourceInvalid)
	}
	defer file.Close()
	if err := provider.validateMetadata(stat); err != nil {
		return secret.Payload{}, err
	}

	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		contents, err := readBoundedFile(file, stat, provider.maxBytes)
		if err != nil {
			return secret.Payload{}, err
		}
		payload := secret.NewValuePayload(contents)
		zeroBytes(contents)
		return payload, nil
	case unix.S_IFDIR:
		state := &treeState{maxBytes: provider.maxBytes, maxEntries: provider.maxEntries}
		if err := provider.readDirectory(ctx, file, "", state); err != nil {
			zeroFileValues(state.files)
			return secret.Payload{}, err
		}
		payload, err := secret.NewDirectoryPayload(state.files)
		zeroFileValues(state.files)
		if err != nil {
			return secret.Payload{}, fmt.Errorf("%w: invalid directory source", secret.ErrSourceInvalid)
		}
		return payload, nil
	default:
		return secret.Payload{}, secret.ErrSourceInvalid
	}
}

func (provider *Filesystem) anchor(reference string) (root, relative string, ok bool) {
	if reference == "" || !filepath.IsAbs(reference) || filepath.Clean(reference) != reference {
		return "", "", false
	}
	for _, candidate := range provider.roots {
		relative, err := filepath.Rel(candidate, reference)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		return candidate, relative, true
	}
	return "", "", false
}

func openAnchored(root, relative string) (*os.File, *unix.Stat_t, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_DIRECTORY
	fd, err := unix.Open(root, flags, 0)
	if err != nil {
		return nil, nil, err
	}
	components := []string(nil)
	if relative != "." {
		components = strings.Split(relative, string(filepath.Separator))
	}
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			unix.Close(fd)
			return nil, nil, errors.New("invalid anchored path")
		}
		childFlags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if index < len(components)-1 {
			childFlags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, component, childFlags, 0)
		unix.Close(fd)
		if err != nil {
			return nil, nil, err
		}
		fd = next
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return nil, nil, err
	}
	return os.NewFile(uintptr(fd), "secret-source"), &stat, nil
}

func (provider *Filesystem) validateMetadata(stat *unix.Stat_t) error {
	if stat.Uid != provider.ownerUID || stat.Mode&0o077 != 0 || stat.Mode&0o400 == 0 ||
		stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 {
		return secret.ErrSourceInvalid
	}
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return nil
	case unix.S_IFDIR:
		if stat.Mode&0o100 == 0 {
			return secret.ErrSourceInvalid
		}
		return nil
	default:
		return secret.ErrSourceInvalid
	}
}

type treeState struct {
	maxBytes   int64
	maxEntries int
	bytes      int64
	entries    int
	files      []secret.File
}

const directoryReadChunkSize = 64

type directoryEntryReader interface {
	ReadDir(int) ([]os.DirEntry, error)
}

func (provider *Filesystem) readDirectory(ctx context.Context, directory *os.File, prefix string, state *treeState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	remainingEntries := state.maxEntries - state.entries
	if remainingEntries < 0 {
		return secret.ErrSourceLimit
	}
	entries, err := readBoundedDirectoryEntries(directory, remainingEntries)
	if err != nil {
		return err
	}
	state.entries += len(entries)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		fd, err := unix.Openat(int(directory.Fd()), entry.Name(), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return fmt.Errorf("%w: open directory entry", secret.ErrSourceInvalid)
		}
		child := os.NewFile(uintptr(fd), "secret-entry")
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			child.Close()
			return fmt.Errorf("%w: inspect directory entry", secret.ErrSourceInvalid)
		}
		if err := provider.validateMetadata(&stat); err != nil {
			child.Close()
			return err
		}
		relativePath := path.Join(prefix, entry.Name())
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFREG:
			remaining := state.maxBytes - state.bytes
			contents, err := readBoundedFile(child, &stat, remaining)
			child.Close()
			if err != nil {
				return err
			}
			contentLength := len(contents)
			file, err := secret.NewFile(relativePath, uint32(stat.Mode&0o777), contents)
			zeroBytes(contents)
			if err != nil {
				return fmt.Errorf("%w: invalid directory file", secret.ErrSourceInvalid)
			}
			state.bytes += int64(contentLength)
			state.files = append(state.files, file)
		case unix.S_IFDIR:
			err := provider.readDirectory(ctx, child, relativePath, state)
			child.Close()
			if err != nil {
				return err
			}
		default:
			child.Close()
			return secret.ErrSourceInvalid
		}
	}
	return nil
}

func readBoundedDirectoryEntries(reader directoryEntryReader, maxEntries int) ([]os.DirEntry, error) {
	if maxEntries < 0 {
		return nil, secret.ErrSourceLimit
	}
	entries := make([]os.DirEntry, 0, min(maxEntries, directoryReadChunkSize)+1)
	for len(entries) <= maxEntries {
		remaining := maxEntries - len(entries) + 1
		batch, err := reader.ReadDir(min(remaining, directoryReadChunkSize))
		if len(batch) > remaining {
			batch = batch[:remaining]
		}
		entries = append(entries, batch...)
		if len(entries) > maxEntries {
			return nil, secret.ErrSourceLimit
		}
		if errors.Is(err, io.EOF) {
			slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
			return entries, nil
		}
		if err != nil || len(batch) == 0 {
			return nil, fmt.Errorf("%w: read directory", secret.ErrSourceInvalid)
		}
	}
	return nil, secret.ErrSourceLimit
}

func readBoundedFile(file *os.File, stat *unix.Stat_t, maxBytes int64) ([]byte, error) {
	if stat.Size <= 0 {
		return nil, secret.ErrSourceInvalid
	}
	if maxBytes <= 0 || stat.Size > maxBytes {
		return nil, secret.ErrSourceLimit
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read source", secret.ErrSourceUnavailable)
	}
	if len(contents) == 0 {
		return nil, secret.ErrSourceInvalid
	}
	if int64(len(contents)) > maxBytes {
		zeroBytes(contents)
		return nil, secret.ErrSourceLimit
	}
	return contents, nil
}

func zeroFileValues(files []secret.File) {
	for index := range files {
		files[index].Zero()
	}
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ secret.Provider = (*Filesystem)(nil)
