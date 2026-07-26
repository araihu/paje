package dockerengine

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/araihu/paje/internal/executor"
	"github.com/araihu/paje/internal/sandboxinit"
	"github.com/araihu/paje/internal/secret"
	"github.com/araihu/paje/internal/workerprofile"
)

const (
	sandboxUID = sandboxinit.BootstrapUID
	sandboxGID = sandboxinit.BootstrapGID
)

type archiveEntry struct {
	name string
	mode int64
	body []byte
	dir  bool
}

type privateArchive struct {
	bytes []byte
}

func (archive *privateArchive) Reader() io.Reader {
	return bytes.NewReader(archive.bytes)
}

func (archive *privateArchive) Destroy() {
	if archive == nil {
		return
	}
	clear(archive.bytes)
	archive.bytes = nil
}

func buildArchive(request executor.Request) (_ *privateArchive, returnedErr error) {
	document := sandboxinit.Document{
		WorkspaceRoot: executor.SandboxWorkspaceRoot,
		Command:       request.Command.Clone(),
		Environment:   cloneStrings(request.Environment),
	}
	if len(request.Secrets) > sandboxinit.MaxBootstrapEntries-1 {
		return nil, errors.New("private Docker archive has too many materializations")
	}
	order := make([]int, len(request.Secrets))
	for index := range order {
		order[index] = index
	}
	sort.Slice(order, func(i, j int) bool {
		left, right := request.Secrets[order[i]], request.Secrets[order[j]]
		if left.Target() != right.Target() {
			return left.Target() < right.Target()
		}
		return left.Delivery() < right.Delivery()
	})

	entries := make([]archiveEntry, 0, len(request.Secrets)+1)
	defer func() {
		for index := range entries {
			clear(entries[index].body)
			entries[index].body = nil
		}
	}()
	var materializedBytes int64
	remainingEntries := sandboxinit.MaxBootstrapEntries - 1
	remainingBytes := int64(sandboxinit.MaxBootstrapArchiveBytes - sandboxinit.MaxDocumentBytes)
	addFile := func(name string, mode int64, body []byte) error {
		size := int64(len(body))
		if size <= 0 || size > sandboxinit.MaxBootstrapEntryBytes ||
			materializedBytes > sandboxinit.MaxBootstrapArchiveBytes-size {
			clear(body)
			return errors.New("private Docker archive entry exceeds limit")
		}
		materializedBytes += size
		entries = append(entries, archiveEntry{name: name, mode: mode, body: body})
		remainingEntries--
		return nil
	}
	for _, index := range order {
		materialization := request.Secrets[index]
		switch materialization.Delivery() {
		case workerprofile.DeliveryFile:
			if remainingEntries <= 0 {
				return nil, errors.New("private Docker archive has too many entries")
			}
			value, err := materialization.ValueBounded(min(remainingBytes, int64(sandboxinit.MaxBootstrapEntryBytes)))
			if err != nil {
				return nil, err
			}
			if err := addFile(archiveName(materialization.Target()), 0o400, value); err != nil {
				return nil, err
			}
			remainingBytes -= int64(len(value))
		case workerprofile.DeliveryEnvironment:
			if document.EnvironmentFiles == nil {
				document.EnvironmentFiles = make(map[string]string)
			}
			file := environmentMaterializationPath(materialization.Target())
			document.EnvironmentFiles[materialization.Target()] = file
			if remainingEntries <= 0 {
				return nil, errors.New("private Docker archive has too many entries")
			}
			value, err := materialization.ValueBounded(min(remainingBytes, int64(sandboxinit.MaxBootstrapEntryBytes)))
			if err != nil {
				return nil, err
			}
			if err := addFile(archiveName(file), 0o400, value); err != nil {
				return nil, err
			}
			remainingBytes -= int64(len(value))
		case workerprofile.DeliveryDirectory:
			if remainingEntries <= 1 {
				return nil, errors.New("private Docker archive has too many entries")
			}
			target := archiveName(materialization.Target())
			entries = append(entries, archiveEntry{name: target, mode: 0o700, dir: true})
			remainingEntries--
			files, err := materialization.FilesBounded(
				remainingEntries, remainingBytes, sandboxinit.MaxBootstrapEntryBytes,
			)
			if err != nil {
				return nil, err
			}
			for _, file := range files {
				name := path.Join(target, file.Path())
				mode := int64(file.Mode())
				contents := file.Bytes()
				file.Zero()
				if err := addFile(name, mode, contents); err != nil {
					zeroSecretFiles(files)
					return nil, err
				}
				remainingBytes -= int64(len(contents))
			}
			zeroSecretFiles(files)
		default:
			return nil, errors.New("unsupported Docker secret materialization")
		}
	}
	if err := document.Validate(); err != nil {
		return nil, err
	}
	encodedDocument, err := json.Marshal(document)
	if err != nil {
		return nil, errors.New("encode sandbox command document")
	}
	if err := addFile(archiveName(sandboxinit.DocumentPath), 0o400, encodedDocument); err != nil {
		return nil, err
	}

	entries, err = addParentDirectories(entries, sandboxinit.MaxBootstrapEntries)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].name == entries[j].name {
			return entries[i].dir && !entries[j].dir
		}
		return entries[i].name < entries[j].name
	})
	buffer := &hardCapBuffer{remaining: sandboxinit.MaxBootstrapArchiveBytes}
	writer := tar.NewWriter(buffer)
	for index := range entries {
		entry := &entries[index]
		header := &tar.Header{
			Name: entry.name, Mode: entry.mode, Uid: sandboxUID, Gid: sandboxGID,
			Size: int64(len(entry.body)), Format: tar.FormatPAX,
		}
		if entry.dir {
			header.Typeflag = tar.TypeDir
			header.Size = 0
			if !strings.HasSuffix(header.Name, "/") {
				header.Name += "/"
			}
		} else {
			header.Typeflag = tar.TypeReg
		}
		if err := writer.WriteHeader(header); err != nil {
			buffer.Destroy()
			return nil, errors.New("build private Docker archive")
		}
		if len(entry.body) > 0 {
			if _, err := writer.Write(entry.body); err != nil {
				buffer.Destroy()
				return nil, errors.New("build private Docker archive")
			}
		}
		clear(entry.body)
		entry.body = nil
	}
	if err := writer.Close(); err != nil {
		buffer.Destroy()
		return nil, errors.New("finish private Docker archive")
	}
	return &privateArchive{bytes: buffer.Take()}, nil
}

type hardCapBuffer struct {
	buffer    bytes.Buffer
	remaining int64
}

func (buffer *hardCapBuffer) Write(value []byte) (int, error) {
	if int64(len(value)) > buffer.remaining {
		return 0, errors.New("private Docker archive exceeds limit")
	}
	written, err := buffer.buffer.Write(value)
	buffer.remaining -= int64(written)
	return written, err
}

func (buffer *hardCapBuffer) Take() []byte {
	value := buffer.buffer.Bytes()
	buffer.buffer = bytes.Buffer{}
	buffer.remaining = 0
	return value
}

func (buffer *hardCapBuffer) Destroy() {
	clear(buffer.buffer.Bytes())
	buffer.buffer.Reset()
	buffer.remaining = 0
}

func addParentDirectories(entries []archiveEntry, maxEntries int) ([]archiveEntry, error) {
	if len(entries) > maxEntries {
		return nil, errors.New("private Docker archive has too many entries")
	}
	existing := make(map[string]struct{})
	for _, entry := range entries {
		if entry.dir {
			existing[entry.name] = struct{}{}
		}
	}
	directories := make(map[string]struct{})
	for _, entry := range entries {
		current := entry.name
		if !entry.dir {
			current = path.Dir(current)
		}
		for current != "." && current != "/" && current != "" && current != "run" {
			if _, exists := existing[current]; exists {
				current = path.Dir(current)
				continue
			}
			if _, exists := directories[current]; !exists {
				if len(entries)+len(directories) >= maxEntries {
					return nil, errors.New("private Docker archive has too many entries")
				}
				directories[current] = struct{}{}
			}
			current = path.Dir(current)
		}
	}
	for directory := range directories {
		entries = append(entries, archiveEntry{name: directory, mode: 0o700, dir: true})
	}
	return entries, nil
}

func zeroSecretFiles(files []secret.File) {
	for index := range files {
		files[index].Zero()
	}
}

func archiveName(absolute string) string {
	return strings.TrimPrefix(path.Clean(absolute), "/")
}

func environmentMaterializationPath(key string) string {
	digest := sha256.Sum256([]byte(key))
	return path.Join(sandboxinit.SecretRoot, "environment", hex.EncodeToString(digest[:16]))
}
