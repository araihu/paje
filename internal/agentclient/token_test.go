package agentclient_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/araihu/paje/internal/agentclient"
)

func TestReadTokenFileRequiresAbsoluteOwnerOnlyRegularFile(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "token")
	if err := os.WriteFile(path, []byte(clientTestToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := agentclient.ReadTokenFile(path); err != nil || token != clientTestToken {
		t.Fatalf("token = %q error = %v", token, err)
	}

	if _, err := agentclient.ReadTokenFile("token"); err == nil {
		t.Fatal("relative token path accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := agentclient.ReadTokenFile(path); err == nil {
		t.Fatal("world-readable token accepted")
	}
}

func TestReadTokenFileRejectsSymlink(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "target")
	link := filepath.Join(directory, "token")
	if err := os.WriteFile(target, []byte(clientTestToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := agentclient.ReadTokenFile(link); err == nil {
		t.Fatal("symlink token accepted")
	}
}
