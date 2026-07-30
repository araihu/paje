package araihuassets

import (
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
