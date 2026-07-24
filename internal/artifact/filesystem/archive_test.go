//go:build darwin || linux

package filesystem_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/artifact"
)

type archiveEntry struct {
	header tar.Header
	data   []byte
}

func TestStoreRejectsMalformedCanonicalTar(t *testing.T) {
	t.Parallel()
	_, canonical, _, err := artifact.Canonicalize(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	entries := readArchiveEntries(t, canonical)
	if reencoded := writeArchiveEntries(t, entries); !bytes.Equal(reencoded, canonical) {
		t.Fatal("test fixture re-encoder did not preserve the canonical archive")
	}

	tests := []struct {
		name   string
		mutate func(*testing.T, []archiveEntry, []byte) []byte
	}{
		{
			name: "duplicate member",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				return writeArchiveEntries(t, append(entries, cloneArchiveEntry(entries[1])))
			},
		},
		{
			name: "unknown member",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				entries[0].header.Name = "unknown.json"
				return writeArchiveEntries(t, entries)
			},
		},
		{
			name: "missing member",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				return writeArchiveEntries(t, entries[:len(entries)-1])
			},
		},
		{
			name: "reordered member",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				entries[0], entries[1] = entries[1], entries[0]
				return writeArchiveEntries(t, entries)
			},
		},
		{
			name: "traversal name",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				entries[0].header.Name = "../manifest.json"
				return writeArchiveEntries(t, entries)
			},
		},
		{
			name: "absolute name",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				entries[0].header.Name = "/manifest.json"
				return writeArchiveEntries(t, entries)
			},
		},
		{
			name: "unsupported type",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				entries[1].header.Typeflag = tar.TypeDir
				entries[1].header.Size = 0
				entries[1].data = nil
				return writeArchiveEntries(t, entries)
			},
		},
		{
			name: "noncanonical header mode",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				entries[0].header.Mode = 0o644
				return writeArchiveEntries(t, entries)
			},
		},
		{
			name: "noncanonical header uid",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				entries[0].header.Uid = 1
				return writeArchiveEntries(t, entries)
			},
		},
		{
			name: "noncanonical header gid",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				entries[0].header.Gid = 1
				return writeArchiveEntries(t, entries)
			},
		},
		{
			name: "noncanonical header user name",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				entries[0].header.Uname = "owner"
				return writeArchiveEntries(t, entries)
			},
		},
		{
			name: "noncanonical header group name",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				entries[0].header.Gname = "group"
				return writeArchiveEntries(t, entries)
			},
		},
		{
			name: "noncanonical header modification time",
			mutate: func(t *testing.T, entries []archiveEntry, _ []byte) []byte {
				entries[0].header.ModTime = time.Unix(1, 0).UTC()
				return writeArchiveEntries(t, entries)
			},
		},
		{
			name: "nonzero padding",
			mutate: func(t *testing.T, entries []archiveEntry, canonical []byte) []byte {
				t.Helper()
				mutated := append([]byte(nil), canonical...)
				offset := 0
				for _, entry := range entries {
					dataEnd := offset + 512 + len(entry.data)
					nextHeader := (dataEnd + 511) / 512 * 512
					if dataEnd < nextHeader {
						mutated[dataEnd] = 1
						return mutated
					}
					offset = nextHeader
				}
				t.Fatal("canonical fixture has no tar padding to tamper")
				return nil
			},
		},
		{
			name: "bytes after tar terminator",
			mutate: func(_ *testing.T, _ []archiveEntry, canonical []byte) []byte {
				return append(append([]byte(nil), canonical...), []byte("after-tar-terminator")...)
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			malformed := test.mutate(t, cloneArchiveEntries(entries), canonical)
			if _, err := artifact.DecodeCanonicalTar(malformed); !errors.Is(err, artifact.ErrDigestMismatch) {
				t.Fatalf("DecodeCanonicalTar() error = %v, want ErrDigestMismatch", err)
			}

			root := t.TempDir()
			store := newStore(t, root, 1<<20)
			ref := writeStoredArchive(t, root, malformed, testBundle().Manifest.RunID)
			if _, err := store.Load(context.Background(), ref); !errors.Is(err, artifact.ErrDigestMismatch) {
				t.Fatalf("Load() error = %v, want ErrDigestMismatch", err)
			}
		})
	}
}

func TestStoreRejectsMemberAndManifestTampering(t *testing.T) {
	t.Parallel()
	_, canonical, _, err := artifact.Canonicalize(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	entries := readArchiveEntries(t, canonical)

	tests := []struct {
		name   string
		mutate func(*testing.T, []archiveEntry)
	}{
		{
			name: "member bytes",
			mutate: func(t *testing.T, entries []archiveEntry) {
				t.Helper()
				entries[1].data[0] ^= 1
			},
		},
		{
			name: "manifest digest",
			mutate: func(t *testing.T, entries []archiveEntry) {
				t.Helper()
				manifest := decodeManifest(t, entries[0].data)
				manifest.Members[0].SHA256 = strings.Repeat("0", 64)
				entries[0].data = encodeManifest(t, manifest)
				entries[0].header.Size = int64(len(entries[0].data))
			},
		},
		{
			name: "manifest size",
			mutate: func(t *testing.T, entries []archiveEntry) {
				t.Helper()
				manifest := decodeManifest(t, entries[0].data)
				manifest.Members[0].Size++
				entries[0].data = encodeManifest(t, manifest)
				entries[0].header.Size = int64(len(entries[0].data))
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			tamperedEntries := cloneArchiveEntries(entries)
			test.mutate(t, tamperedEntries)
			tampered := writeArchiveEntries(t, tamperedEntries)

			root := t.TempDir()
			store := newStore(t, root, 1<<20)
			ref := writeStoredArchive(t, root, tampered, testBundle().Manifest.RunID)
			if _, err := store.Load(context.Background(), ref); !errors.Is(err, artifact.ErrDigestMismatch) {
				t.Fatalf("Load() error = %v, want ErrDigestMismatch", err)
			}
		})
	}
}

func TestStoreLoadPreservesTooLargeFromUncompressedRead(t *testing.T) {
	t.Parallel()
	bundle := testBundle()
	bundle.AgentOutput = bytes.Repeat([]byte("highly compressible artifact evidence\n"), 8192)
	_, tarBytes, _, err := artifact.Canonicalize(bundle)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzipArchive(t, tarBytes, 6)
	if int64(len(tarBytes)) <= 16*int64(len(compressed)) {
		t.Fatalf("fixture is not compressible enough: tar=%d gzip=%d", len(tarBytes), len(compressed))
	}

	root := t.TempDir()
	store := newStore(t, root, int64(len(compressed)))
	ref := writeStoredCompressedArchive(t, root, tarBytes, compressed, bundle.Manifest.RunID)
	if _, err := store.Load(context.Background(), ref); !errors.Is(err, artifact.ErrTooLarge) {
		t.Fatalf("Load() error = %v, want ErrTooLarge", err)
	}
}

func TestStoreLoadPreservesTooLargeFromCanonicalReencoding(t *testing.T) {
	t.Parallel()
	var plain bytes.Buffer
	for index := range 4096 {
		var seed [8]byte
		binary.LittleEndian.PutUint64(seed[:], uint64(index))
		sum := sha256.Sum256(seed[:])
		plain.Write(sum[:16])
		plain.WriteString(" repeated artifact record framing ")
	}
	levelSix := gzipArchive(t, plain.Bytes(), 6)
	levelNine := gzipArchive(t, plain.Bytes(), 9)
	if len(levelNine) >= len(levelSix) {
		t.Fatalf("fixture does not distinguish gzip levels: level6=%d level9=%d", len(levelSix), len(levelNine))
	}
	if int64(plain.Len()) > 16*int64(len(levelNine)) {
		t.Fatalf("fixture exceeds uncompressed bound before re-encoding: plain=%d gzip=%d", plain.Len(), len(levelNine))
	}

	root := t.TempDir()
	store := newStore(t, root, int64(len(levelNine)))
	ref := writeStoredCompressedArchive(t, root, plain.Bytes(), levelNine, "run-123")
	if _, err := store.Load(context.Background(), ref); !errors.Is(err, artifact.ErrTooLarge) {
		t.Fatalf("Load() error = %v, want ErrTooLarge", err)
	}
}

func readArchiveEntries(t *testing.T, data []byte) []archiveEntry {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(data))
	var entries []archiveEntry
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		payload, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, archiveEntry{header: *header, data: payload})
	}
}

func writeArchiveEntries(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, entry := range entries {
		header := entry.header
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func cloneArchiveEntries(entries []archiveEntry) []archiveEntry {
	cloned := make([]archiveEntry, len(entries))
	for index, entry := range entries {
		cloned[index] = cloneArchiveEntry(entry)
	}
	return cloned
}

func cloneArchiveEntry(entry archiveEntry) archiveEntry {
	entry.data = append([]byte(nil), entry.data...)
	return entry
}

func decodeManifest(t *testing.T, data []byte) artifact.Manifest {
	t.Helper()
	var manifest artifact.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func encodeManifest(t *testing.T, manifest artifact.Manifest) []byte {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeStoredArchive(t *testing.T, root string, tarBytes []byte, runID string) artifact.Reference {
	t.Helper()
	return writeStoredCompressedArchive(t, root, tarBytes, gzipArchive(t, tarBytes, 6), runID)
}

func writeStoredCompressedArchive(t *testing.T, root string, tarBytes, compressed []byte, runID string) artifact.Reference {
	t.Helper()
	digest := artifact.DigestTar(tarBytes)
	prefix := filepath.Join(root, "sha256", digest[:2])
	if err := os.MkdirAll(prefix, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(prefix, digest+".tar.gz")
	if err := os.WriteFile(path, compressed, 0o600); err != nil {
		t.Fatal(err)
	}
	return artifact.Reference{RunID: runID, Digest: digest, Size: int64(len(tarBytes))}
}

func gzipArchive(t *testing.T, data []byte, level int) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, level)
	if err != nil {
		t.Fatal(err)
	}
	writer.Header.ModTime = time.Unix(0, 0).UTC()
	writer.Header.OS = 255
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}
