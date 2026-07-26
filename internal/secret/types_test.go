package secret

import (
	"testing"

	"github.com/araihu/paje/internal/workerprofile"
)

func TestMaterializationProvidesBoundedCopiesWithoutOversizeAllocation(t *testing.T) {
	t.Run("value", func(t *testing.T) {
		materialization, err := NewValueMaterialization(
			workerprofile.DeliveryFile, "/run/paje/secrets/token", []byte("secret"),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer materialization.Destroy()
		copier, ok := any(materialization).(interface {
			ValueBounded(int64) ([]byte, error)
		})
		if !ok {
			t.Fatal("materialization has no bounded value-copy API")
		}
		if value, err := copier.ValueBounded(5); err == nil || value != nil {
			t.Fatalf("oversized bounded value copy = %q, %v", value, err)
		}
		value, err := copier.ValueBounded(6)
		if err != nil || string(value) != "secret" {
			t.Fatalf("bounded value copy = %q, %v", value, err)
		}
		value[0] = 'X'
		if string(materialization.Value()) != "secret" {
			t.Fatal("bounded value copy aliases retained secret material")
		}
		clear(value)
	})

	t.Run("files", func(t *testing.T) {
		first, err := NewFile("first", 0o600, []byte("a"))
		if err != nil {
			t.Fatal(err)
		}
		second, err := NewFile("second", 0o600, []byte("b"))
		if err != nil {
			t.Fatal(err)
		}
		materialization, err := NewDirectoryMaterialization(
			"/run/paje/secrets/tree", []File{first, second},
		)
		first.Zero()
		second.Zero()
		if err != nil {
			t.Fatal(err)
		}
		defer materialization.Destroy()
		copier, ok := any(materialization).(interface {
			FilesBounded(int, int64, int64) ([]File, error)
		})
		if !ok {
			t.Fatal("materialization has no bounded file-copy API")
		}
		if files, err := copier.FilesBounded(1, 2, 1); err == nil || files != nil {
			t.Fatalf("excessive bounded file copy = %#v, %v", files, err)
		}
		if files, err := copier.FilesBounded(2, 1, 1); err == nil || files != nil {
			t.Fatalf("oversized bounded file copy = %#v, %v", files, err)
		}
		if files, err := copier.FilesBounded(2, 2, 0); err == nil || files != nil {
			t.Fatalf("zero per-file budget copy = %#v, %v", files, err)
		}
		files, err := copier.FilesBounded(2, 2, 1)
		if err != nil || len(files) != 2 {
			t.Fatalf("bounded file copy = %#v, %v", files, err)
		}
		zeroFiles(files)
	})
}
