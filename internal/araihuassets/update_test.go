package araihuassets

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRejectsTraversalCollisionAndIncompleteRoles(t *testing.T) {
	base := Manifest{SchemaVersion: 1, AssetsRepository: "araihu/assets", AssetsRevision: strings.Repeat("a", 40), Release: "v1.2.3", ReleaseURL: "https://github.com/araihu/assets/releases/download/v1.2.3/araihu-assets-v1.2.3.tar.gz", ReleaseSHA256: strings.Repeat("b", 64), ReleaseJSONSHA256: strings.Repeat("c", 64), Mappings: []Mapping{{Source: "themes/araihu.css", Destination: "site/generator/araihu.css"}}}
	if err := validate(base); err != nil {
		t.Fatal(err)
	}
	bad := base
	bad.Mappings = []Mapping{{Source: "../x", Destination: "x"}}
	if err := validate(bad); err == nil {
		t.Fatal("traversal accepted")
	}
	bad = base
	bad.Mappings = []Mapping{{Source: "a", Destination: "A"}, {Source: "b", Destination: "a"}}
	if err := validate(bad); err == nil {
		t.Fatal("collision accepted")
	}
	bad = base
	bad.Mappings = []Mapping{{Source: "a", Destination: "b", CanonicalName: "x"}}
	if err := validate(bad); err == nil {
		t.Fatal("incomplete role accepted")
	}
}

func TestUpdateRejectsSymlinkRoot(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(Options{RepoRoot: link, ReleaseRoot: target}); err == nil {
		t.Fatal("symlink root accepted")
	}
}

func TestRollbackRestoresAppliedFallbacksInReverseOrder(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"site/a", "site/b"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(name+"-old"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writes := []write{{path: "site/a", next: []byte("new-a"), old: []byte("site/a-old"), existed: true}, {path: "site/b", next: []byte("new-b"), old: []byte("site/b-old"), existed: true}, {path: "araihu-assets.json", next: []byte("manifest")}}
	for i := range writes[:2] {
		if err := stage(root, &writes[i]); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(writes[i].staged, filepath.Join(root, writes[i].path)); err != nil {
			t.Fatal(err)
		}
	}
	err := rollback(root, writes, 2, []string{"site/a", "site/b"}, os.ErrPermission)
	apply, ok := err.(*ApplyError)
	if !ok || !apply.RollbackComplete() {
		t.Fatalf("rollback = %#v", err)
	}
	for _, name := range []string{"site/a", "site/b"} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != name+"-old" {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
}

func TestUpdateCommitsNewIdentityAfterFallbacks(t *testing.T) {
	repo := t.TempDir()
	releaseRoot := t.TempDir()
	write := func(root, name string, contents []byte) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0644); err != nil {
			t.Fatal(err)
		}
	}
	encode := func(value any) []byte {
		t.Helper()
		contents, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(contents, '\n')
	}

	const destination = "site/generator/araihu.css"
	oldManifest := Manifest{
		SchemaVersion: 1, AssetsRepository: "araihu/assets", AssetsRevision: strings.Repeat("a", 40),
		Release: "v1.0.0", ReleaseURL: "https://github.com/araihu/assets/releases/download/v1.0.0/araihu-assets-v1.0.0.tar.gz",
		ReleaseSHA256: strings.Repeat("b", 64), ReleaseJSONSHA256: strings.Repeat("c", 64),
		Mappings: []Mapping{{Source: "themes/araihu.css", Destination: destination}},
	}
	write(repo, DefaultManifestPath, encode(oldManifest))
	write(repo, destination, []byte("old theme\n"))

	theme := []byte("new theme\n")
	catalogBytes := []byte("{\n  \"release\": \"v2.0.0\",\n  \"assets\": []\n}\n")
	write(releaseRoot, "catalog.json", catalogBytes)
	write(releaseRoot, "themes/araihu.css", theme)
	releaseBytes := encode(releaseDocument{
		SchemaVersion: 1,
		Release:       "v2.0.0",
		CatalogSHA256: hash(catalogBytes),
		Files:         []releaseFile{{Path: "themes/araihu.css", SHA256: hash(theme), Size: int64(len(theme))}},
	})
	write(releaseRoot, "release.json", releaseBytes)

	identity := &ReleaseIdentity{
		AssetsRepository: "araihu/assets", AssetsRevision: strings.Repeat("d", 40), Release: "v2.0.0",
		ReleaseURL:    "https://github.com/araihu/assets/releases/download/v2.0.0/araihu-assets-v2.0.0.tar.gz",
		ReleaseSHA256: strings.Repeat("e", 64), ReleaseJSONSHA256: hash(releaseBytes),
	}
	result, err := Update(Options{RepoRoot: repo, ReleaseRoot: releaseRoot, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(result.Changed, ","), destination+","+DefaultManifestPath; got != want {
		t.Fatalf("changed order = %q, want %q", got, want)
	}
	if got, err := os.ReadFile(filepath.Join(repo, destination)); err != nil || string(got) != string(theme) {
		t.Fatalf("fallback = %q, %v", got, err)
	}
	var current Manifest
	manifestBytes, err := os.ReadFile(filepath.Join(repo, DefaultManifestPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifestBytes, &current); err != nil {
		t.Fatal(err)
	}
	if current.AssetsRevision != identity.AssetsRevision || current.Release != identity.Release || current.ReleaseJSONSHA256 != identity.ReleaseJSONSHA256 {
		t.Fatalf("manifest identity = %#v, want %#v", current, identity)
	}
}
