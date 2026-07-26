package secret

import (
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/araihu/paje/internal/workerprofile"
)

var ErrSecretSerialization = errors.New("secret material cannot be serialized")

type BindingRef struct {
	Capability string `json:"capability"`
	Revision   uint64 `json:"revision"`
}

type AcquireRequest struct {
	RunID      string
	Attempt    int
	StartedAt  time.Time
	ProfileID  workerprofile.ProfileID
	Capability string
	Binding    uint64
	Delivery   workerprofile.SecretRequirement
	Deadline   time.Time
}

type Broker interface {
	Acquire(context.Context, AcquireRequest) (Lease, error)
	Revoke(context.Context, string) error
}

type File struct {
	path  string
	mode  uint32
	bytes []byte
}

func NewFile(relativePath string, mode uint32, contents []byte) (File, error) {
	if relativePath == "" || strings.HasPrefix(relativePath, "/") || path.Clean(relativePath) != relativePath ||
		relativePath == "." || strings.HasPrefix(relativePath, "../") || mode == 0 || mode&^uint32(0o777) != 0 ||
		mode&0o400 == 0 || mode&0o077 != 0 || len(contents) == 0 {
		return File{}, errors.New("invalid secret file")
	}
	return File{path: relativePath, mode: mode, bytes: slices.Clone(contents)}, nil
}

func (file File) Path() string   { return file.path }
func (file File) Mode() uint32   { return file.mode }
func (file File) Bytes() []byte  { return slices.Clone(file.bytes) }
func (file File) String() string { return "[secret file]" }
func (File) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[secret file]"))
}

func (file *File) Zero() {
	if file == nil {
		return
	}
	zeroBytes(file.bytes)
	file.bytes = nil
	file.path = ""
	file.mode = 0
}

func (File) MarshalJSON() ([]byte, error) { return nil, ErrSecretSerialization }
func (File) MarshalText() ([]byte, error) { return nil, ErrSecretSerialization }

const (
	payloadValue     = "value"
	payloadDirectory = "directory"
)

type Payload struct {
	kind  string
	value []byte
	files []File
}

func NewValuePayload(value []byte) Payload {
	return Payload{kind: payloadValue, value: slices.Clone(value)}
}

func NewDirectoryPayload(files []File) (Payload, error) {
	return newDirectoryPayloadFromOwnedFiles(cloneFiles(files))
}

func newDirectoryPayloadFromOwnedFiles(files []File) (_ Payload, err error) {
	defer func() {
		if err != nil {
			zeroFiles(files)
		}
	}()
	if len(files) == 0 {
		return Payload{}, errors.New("secret directory is empty")
	}
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.path, b.path) })
	for index, file := range files {
		if file.path == "" || len(file.bytes) == 0 {
			return Payload{}, errors.New("invalid secret directory file")
		}
		if index > 0 && files[index-1].path == file.path {
			return Payload{}, errors.New("duplicate secret directory file")
		}
		for prior := range index {
			if strings.HasPrefix(file.path, files[prior].path+"/") || strings.HasPrefix(files[prior].path, file.path+"/") {
				return Payload{}, errors.New("overlapping secret directory paths")
			}
		}
	}
	return Payload{kind: payloadDirectory, files: files}, nil
}

func (payload Payload) Kind() string   { return payload.kind }
func (payload Payload) Value() []byte  { return slices.Clone(payload.value) }
func (payload Payload) Files() []File  { return cloneFiles(payload.files) }
func (payload Payload) String() string { return "[secret payload]" }
func (Payload) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[secret payload]"))
}
func (payload Payload) Clone() Payload {
	return Payload{kind: payload.kind, value: slices.Clone(payload.value), files: cloneFiles(payload.files)}
}

// Destroy zeroes caller-owned provider payload material. It is safe to call
// repeatedly.
func (payload *Payload) Destroy() {
	if payload == nil {
		return
	}
	zeroBytes(payload.value)
	zeroFiles(payload.files)
	payload.kind = ""
	payload.value = nil
	payload.files = nil
}

func (Payload) MarshalJSON() ([]byte, error) { return nil, ErrSecretSerialization }
func (Payload) MarshalText() ([]byte, error) { return nil, ErrSecretSerialization }

type Materialization struct {
	delivery string
	target   string
	value    []byte
	files    []File
}

func NewValueMaterialization(delivery, target string, value []byte) (Materialization, error) {
	if delivery != workerprofile.DeliveryFile && delivery != workerprofile.DeliveryEnvironment {
		return Materialization{}, errors.New("value secret requires file or environment delivery")
	}
	if !validDeliveryTarget(delivery, target) || len(value) == 0 {
		return Materialization{}, errors.New("invalid secret value materialization")
	}
	return Materialization{delivery: delivery, target: target, value: slices.Clone(value)}, nil
}

func NewDirectoryMaterialization(target string, files []File) (Materialization, error) {
	if !validDeliveryTarget(workerprofile.DeliveryDirectory, target) {
		return Materialization{}, errors.New("secret directory target is required")
	}
	payload, err := NewDirectoryPayload(files)
	if err != nil {
		return Materialization{}, err
	}
	return Materialization{delivery: workerprofile.DeliveryDirectory, target: target, files: payload.files}, nil
}

func (materialization Materialization) Delivery() string { return materialization.delivery }
func (materialization Materialization) Target() string   { return materialization.target }
func (materialization Materialization) Value() []byte    { return slices.Clone(materialization.value) }
func (materialization Materialization) Files() []File    { return cloneFiles(materialization.files) }
func (materialization Materialization) String() string   { return "[secret materialization]" }
func (Materialization) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[secret materialization]"))
}

func (materialization Materialization) Clone() Materialization {
	return Materialization{
		delivery: materialization.delivery,
		target:   materialization.target,
		value:    slices.Clone(materialization.value),
		files:    cloneFiles(materialization.files),
	}
}

// Destroy zeroes caller-owned secret material and clears its delivery
// metadata. It is safe to call repeatedly.
func (materialization *Materialization) Destroy() {
	if materialization == nil {
		return
	}
	zeroBytes(materialization.value)
	zeroFiles(materialization.files)
	materialization.value = nil
	materialization.files = nil
	materialization.delivery = ""
	materialization.target = ""
}

func (Materialization) MarshalJSON() ([]byte, error) { return nil, ErrSecretSerialization }
func (Materialization) MarshalText() ([]byte, error) { return nil, ErrSecretSerialization }

type Lease struct {
	id              string
	expiresAt       time.Time
	materialization Materialization
}

func NewLease(id string, expiresAt time.Time, materialization Materialization) (Lease, error) {
	if id == "" || expiresAt.IsZero() || materialization.delivery == "" {
		return Lease{}, errors.New("invalid secret lease")
	}
	return Lease{id: id, expiresAt: expiresAt.UTC(), materialization: materialization.Clone()}, nil
}

func (lease Lease) ID() string                       { return lease.id }
func (lease Lease) ExpiresAt() time.Time             { return lease.expiresAt }
func (lease Lease) Materialization() Materialization { return lease.materialization.Clone() }
func (Lease) String() string                         { return "[secret lease]" }
func (Lease) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[secret lease]"))
}
func (Lease) MarshalJSON() ([]byte, error) { return nil, ErrSecretSerialization }
func (Lease) MarshalText() ([]byte, error) { return nil, ErrSecretSerialization }

// Destroy zeroes a caller-owned lease and clears its identity and expiry. It
// does not revoke the broker-owned lease; callers must still invoke Revoke.
func (lease *Lease) Destroy() {
	if lease == nil {
		return
	}
	lease.materialization.Destroy()
	lease.id = ""
	lease.expiresAt = time.Time{}
}

func cloneLease(lease Lease) Lease {
	return Lease{id: lease.id, expiresAt: lease.expiresAt, materialization: lease.materialization.Clone()}
}

func cloneFiles(files []File) []File {
	if files == nil {
		return nil
	}
	clone := make([]File, len(files))
	for index, file := range files {
		clone[index] = File{path: file.path, mode: file.mode, bytes: slices.Clone(file.bytes)}
	}
	return clone
}

func zeroFiles(files []File) {
	for index := range files {
		files[index].Zero()
	}
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

// Compile-time guards keep accidental removal of the serialization denial
// visible during refactors.
var (
	_ json.Marshaler         = Lease{}
	_ encoding.TextMarshaler = Lease{}
	_ fmt.Stringer           = Lease{}
	_ fmt.Formatter          = Lease{}
)
