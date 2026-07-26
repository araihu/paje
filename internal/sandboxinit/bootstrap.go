package sandboxinit

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

const (
	BootstrapUID             = 65532
	BootstrapGID             = 65532
	MaxBootstrapArchiveBytes = 32 << 20
	MaxBootstrapEntryBytes   = 8 << 20
	MaxBootstrapEntries      = 1024
)

type bootstrapEntry struct {
	name     string
	mode     os.FileMode
	contents []byte
	dir      bool
}

// ExtractBootstrap validates a complete executor archive in memory before
// materializing its allowlisted files beneath root. The caller must provide a
// private, live tmpfs root when processing workload secrets.
func ExtractBootstrap(reader io.Reader, root string) error {
	if reader == nil || root == "" {
		return errors.New("sandbox bootstrap input is invalid")
	}
	entries, err := decodeBootstrap(reader)
	if err != nil {
		return err
	}
	defer destroyBootstrapEntries(entries)

	opened, err := os.OpenRoot(root)
	if err != nil {
		return errors.New("open sandbox bootstrap root")
	}
	defer opened.Close()

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].dir != entries[j].dir {
			return entries[i].dir
		}
		return strings.Count(entries[i].name, "/") < strings.Count(entries[j].name, "/")
	})
	created := make([]string, 0, len(entries))
	cleanup := func() {
		for index := len(created) - 1; index >= 0; index-- {
			_ = opened.Remove(created[index])
		}
	}
	for index := range entries {
		entry := &entries[index]
		if entry.dir {
			if err := opened.MkdirAll(entry.name, entry.mode); err != nil {
				cleanup()
				return errors.New("materialize sandbox bootstrap directory")
			}
			if err := opened.Chmod(entry.name, entry.mode); err != nil {
				cleanup()
				return errors.New("secure sandbox bootstrap directory")
			}
			created = append(created, entry.name)
			continue
		}
		parent := path.Dir(entry.name)
		if err := opened.MkdirAll(parent, 0o700); err != nil {
			cleanup()
			return errors.New("materialize sandbox bootstrap parent")
		}
		file, err := opened.OpenFile(entry.name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, entry.mode)
		if err != nil {
			cleanup()
			return errors.New("create sandbox bootstrap file")
		}
		written, writeErr := file.Write(entry.contents)
		closeErr := file.Close()
		if writeErr != nil || written != len(entry.contents) || closeErr != nil {
			_ = opened.Remove(entry.name)
			cleanup()
			return errors.New("write sandbox bootstrap file")
		}
		created = append(created, entry.name)
	}
	return nil
}

func decodeBootstrap(reader io.Reader) ([]bootstrapEntry, error) {
	limited := &io.LimitedReader{R: reader, N: MaxBootstrapArchiveBytes + 1}
	archive := tar.NewReader(limited)
	entries := make([]bootstrapEntry, 0, 16)
	seen := make(map[string]struct{})
	var materializedBytes int64
	documentSeen := false

	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			destroyBootstrapEntries(entries)
			return nil, errors.New("read sandbox bootstrap archive")
		}
		if len(entries) >= MaxBootstrapEntries {
			destroyBootstrapEntries(entries)
			return nil, errors.New("sandbox bootstrap archive has too many entries")
		}
		name, mode, dir, err := validateBootstrapHeader(header)
		if err != nil {
			destroyBootstrapEntries(entries)
			return nil, err
		}
		if _, duplicate := seen[name]; duplicate {
			destroyBootstrapEntries(entries)
			return nil, errors.New("sandbox bootstrap archive contains duplicate entries")
		}
		seen[name] = struct{}{}
		if name == strings.TrimPrefix(DocumentPath, "/") {
			documentSeen = true
		}
		entry := bootstrapEntry{name: name, mode: mode, dir: dir}
		if !dir {
			if header.Size > MaxBootstrapEntryBytes ||
				materializedBytes > MaxBootstrapArchiveBytes-header.Size {
				destroyBootstrapEntries(entries)
				return nil, errors.New("sandbox bootstrap archive entry exceeds limit")
			}
			materializedBytes += header.Size
			entry.contents = make([]byte, int(header.Size))
			if _, err := io.ReadFull(archive, entry.contents); err != nil {
				clear(entry.contents)
				destroyBootstrapEntries(entries)
				return nil, errors.New("read sandbox bootstrap entry")
			}
		}
		entries = append(entries, entry)
	}

	remaining, err := io.ReadAll(limited)
	consumed := int64(MaxBootstrapArchiveBytes+1) - limited.N
	if err != nil || consumed > MaxBootstrapArchiveBytes {
		clear(remaining)
		destroyBootstrapEntries(entries)
		return nil, errors.New("sandbox bootstrap archive exceeds limit")
	}
	if len(remaining) != 0 {
		clear(remaining)
		destroyBootstrapEntries(entries)
		return nil, errors.New("sandbox bootstrap archive has trailing content")
	}
	if !documentSeen {
		destroyBootstrapEntries(entries)
		return nil, errors.New("sandbox bootstrap command document is missing")
	}
	return entries, nil
}

func validateBootstrapHeader(header *tar.Header) (string, os.FileMode, bool, error) {
	if header == nil || header.Name == "" || strings.Contains(header.Name, "\\") ||
		strings.IndexByte(header.Name, 0) >= 0 || strings.HasPrefix(header.Name, "/") {
		return "", 0, false, errors.New("sandbox bootstrap archive path is invalid")
	}
	dir := header.Typeflag == tar.TypeDir
	name := header.Name
	if dir {
		name = strings.TrimSuffix(name, "/")
	}
	if path.Clean(name) != name || name == "." || strings.HasPrefix(name, "../") {
		return "", 0, false, errors.New("sandbox bootstrap archive path is invalid")
	}
	document := strings.TrimPrefix(DocumentPath, "/")
	secrets := strings.TrimPrefix(SecretRoot, "/")
	if dir {
		if name != "run/paje" && name != secrets && !strings.HasPrefix(name, secrets+"/") {
			return "", 0, false, errors.New("sandbox bootstrap archive path is not allowlisted")
		}
	} else if name != document && !strings.HasPrefix(name, secrets+"/") {
		return "", 0, false, errors.New("sandbox bootstrap archive path is not allowlisted")
	}
	if header.Uid != BootstrapUID || header.Gid != BootstrapGID ||
		header.Size < 0 || dir && header.Size != 0 {
		return "", 0, false, errors.New("sandbox bootstrap archive metadata is invalid")
	}
	switch header.Typeflag {
	case tar.TypeReg, tar.TypeRegA:
		if dir || header.Size == 0 {
			return "", 0, false, errors.New("sandbox bootstrap archive entry type is invalid")
		}
	case tar.TypeDir:
	default:
		return "", 0, false, errors.New("sandbox bootstrap archive entry type is invalid")
	}
	mode := os.FileMode(header.Mode)
	if mode&^os.FileMode(0o777) != 0 || mode&0o077 != 0 || mode&0o400 == 0 {
		return "", 0, false, errors.New("sandbox bootstrap archive mode is invalid")
	}
	if dir && mode.Perm() != 0o700 {
		return "", 0, false, errors.New("sandbox bootstrap directory mode is invalid")
	}
	return name, mode.Perm(), dir, nil
}

func destroyBootstrapEntries(entries []bootstrapEntry) {
	for index := range entries {
		clear(entries[index].contents)
		entries[index].contents = nil
	}
}
