package sandboxinit

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractBootstrapMaterializesOnlyPrivateAllowlistedFiles(t *testing.T) {
	archive := bootstrapArchive(t,
		bootstrapTestEntry{name: "run/paje", mode: 0o700, typeflag: tar.TypeDir},
		bootstrapTestEntry{name: "run/paje/secrets", mode: 0o700, typeflag: tar.TypeDir},
		bootstrapTestEntry{name: "run/paje/command.json", mode: 0o400, contents: []byte(`{"command":true}`)},
		bootstrapTestEntry{name: "run/paje/secrets/token", mode: 0o600, contents: []byte("secret")},
	)
	root := t.TempDir()
	if err := ExtractBootstrap(bytes.NewReader(archive), root); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"run/paje/command.json":  `{"command":true}`,
		"run/paje/secrets/token": "secret",
	} {
		value, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || string(value) != want {
			t.Fatalf("%s = %q, %v", name, value, err)
		}
	}
	info, err := os.Stat(filepath.Join(root, "run", "paje", "secrets", "token"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode = %#o", info.Mode().Perm())
	}
}

func TestExtractBootstrapRejectsUnsafeOrUnboundedArchivesBeforeWriting(t *testing.T) {
	tests := map[string][]byte{
		"path traversal": bootstrapArchive(t,
			bootstrapTestEntry{name: "run/paje/../outside", mode: 0o400, contents: []byte("bad")}),
		"non-allowlisted path": bootstrapArchive(t,
			bootstrapTestEntry{name: "home/paje/token", mode: 0o400, contents: []byte("bad")}),
		"symlink": bootstrapArchive(t,
			bootstrapTestEntry{name: "run/paje/secrets/link", mode: 0o700, typeflag: tar.TypeSymlink, linkname: "/outside"}),
		"hardlink": bootstrapArchive(t,
			bootstrapTestEntry{name: "run/paje/secrets/link", mode: 0o400, typeflag: tar.TypeLink, linkname: "run/paje/command.json"}),
		"device": bootstrapArchive(t,
			bootstrapTestEntry{name: "run/paje/secrets/device", mode: 0o600, typeflag: tar.TypeChar}),
		"duplicate": bootstrapArchive(t,
			bootstrapTestEntry{name: "run/paje/command.json", mode: 0o400, contents: []byte("first")},
			bootstrapTestEntry{name: "run/paje/command.json", mode: 0o400, contents: []byte("second")}),
		"empty secret": bootstrapArchive(t,
			bootstrapTestEntry{name: "run/paje/command.json", mode: 0o400, contents: []byte("valid")},
			bootstrapTestEntry{name: "run/paje/secrets/empty", mode: 0o400}),
		"trailing bytes": append(bootstrapArchive(t,
			bootstrapTestEntry{name: "run/paje/command.json", mode: 0o400, contents: []byte("valid")}),
			[]byte("trailing")...),
		"oversized entry header": bootstrapHeaderOnly(t, "run/paje/secrets/huge", MaxBootstrapEntryBytes+1),
	}
	for name, archive := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := ExtractBootstrap(bytes.NewReader(archive), root); err == nil {
				t.Fatal("unsafe bootstrap archive succeeded")
			}
			if _, err := os.Stat(filepath.Join(root, "run")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid archive wrote material: %v", err)
			}
		})
	}
}

func TestExtractBootstrapAppliesHardTotalAndEntryCountCaps(t *testing.T) {
	t.Run("total", func(t *testing.T) {
		root := t.TempDir()
		reader := io.MultiReader(
			bytes.NewReader(bootstrapArchive(t,
				bootstrapTestEntry{name: "run/paje/command.json", mode: 0o400, contents: []byte("valid")})),
			strings.NewReader(strings.Repeat("x", MaxBootstrapArchiveBytes)),
		)
		if err := ExtractBootstrap(reader, root); err == nil {
			t.Fatal("oversized bootstrap archive succeeded")
		}
	})

	t.Run("entries", func(t *testing.T) {
		entries := make([]bootstrapTestEntry, 0, MaxBootstrapEntries+1)
		for index := 0; index < MaxBootstrapEntries+1; index++ {
			entries = append(entries, bootstrapTestEntry{
				name: fmt.Sprintf("run/paje/secrets/entry-%04d", index),
				mode: 0o400, contents: []byte("x"),
			})
		}
		root := t.TempDir()
		if err := ExtractBootstrap(bytes.NewReader(bootstrapArchive(t, entries...)), root); err == nil {
			t.Fatal("bootstrap archive with too many entries succeeded")
		}
	})
}

type bootstrapTestEntry struct {
	name     string
	mode     int64
	typeflag byte
	linkname string
	contents []byte
}

func bootstrapArchive(t *testing.T, entries ...bootstrapTestEntry) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := tar.NewWriter(&encoded)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name: entry.name, Mode: entry.mode, Size: int64(len(entry.contents)),
			Typeflag: typeflag, Linkname: entry.linkname, Uid: BootstrapUID, Gid: BootstrapGID,
		}
		if typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.contents) != 0 {
			if _, err := writer.Write(entry.contents); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func bootstrapHeaderOnly(t *testing.T, name string, size int64) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := tar.NewWriter(&encoded)
	if err := writer.WriteHeader(&tar.Header{
		Name: name, Mode: 0o400, Size: size, Typeflag: tar.TypeReg,
		Uid: BootstrapUID, Gid: BootstrapGID,
	}); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
