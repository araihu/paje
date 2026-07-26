package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/araihu/paje/internal/workerprofile"
)

func TestLeaseAndMaterializationCannotBeEncoded(t *testing.T) {
	materialization, err := NewValueMaterialization(workerprofile.DeliveryEnvironment, "WORKLOAD_TOKEN", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := NewLease("lease-id", time.Unix(200, 0).UTC(), materialization)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{
		"lease": lease, "materialization": materialization,
	} {
		t.Run(name+" json", func(t *testing.T) {
			if _, err := json.Marshal(value); !errors.Is(err, ErrSecretSerialization) {
				t.Fatalf("json.Marshal() error = %v", err)
			}
		})
	}
	if _, err := lease.MarshalText(); !errors.Is(err, ErrSecretSerialization) {
		t.Fatalf("Lease.MarshalText() error = %v", err)
	}
	if _, err := materialization.MarshalText(); !errors.Is(err, ErrSecretSerialization) {
		t.Fatalf("Materialization.MarshalText() error = %v", err)
	}
	if got := lease.String(); got != "[secret lease]" {
		t.Fatalf("Lease.String() = %q", got)
	}
}

func TestTransientTypesRemainOpaqueUnderDiagnosticFormatting(t *testing.T) {
	materialization, err := NewValueMaterialization(workerprofile.DeliveryEnvironment, "WORKLOAD_TOKEN", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := NewLease("lease-id", time.Unix(200, 0).UTC(), materialization)
	if err != nil {
		t.Fatal(err)
	}
	binding := validBinding(t)
	payload := NewValuePayload([]byte("secret"))
	file := mustFile(t, "token", 0o600, []byte("secret"))
	for name, value := range map[string]any{
		"lease": lease, "materialization": materialization, "binding": binding,
		"payload": payload, "file": file,
	} {
		t.Run(name, func(t *testing.T) {
			for _, format := range []string{"%v", "%+v", "%#v", "%q", "%s"} {
				got := fmt.Sprintf(format, value)
				if !strings.HasPrefix(got, "[secret ") || strings.Contains(got, "secret\"") || strings.Contains(got, "WORKLOAD_TOKEN_SOURCE") {
					t.Fatalf("format %q exposed transient structure: %q", format, got)
				}
			}
		})
	}
}

func TestDetectorDestroyZeroesPatternsAndDisablesMatching(t *testing.T) {
	secretValue := []byte("secret-value-with-enough-entropy")
	materialization, err := NewValueMaterialization(
		workerprofile.DeliveryFile,
		"/run/paje/secrets/token",
		secretValue,
	)
	if err != nil {
		t.Fatal(err)
	}
	detector := NewDetector(materialization)
	materialization.Destroy()
	if !detector.Scan(secretValue) {
		t.Fatal("detector did not retain a defensive secret pattern")
	}

	exact := detector.(*exactDetector)
	retainedPatterns := append([][]byte(nil), exact.patterns...)
	detector.Destroy()
	detector.Destroy()

	for _, pattern := range retainedPatterns {
		for _, value := range pattern {
			if value != 0 {
				t.Fatal("Destroy did not zero a retained detector pattern")
			}
		}
	}
	if exact.patterns != nil || detector.Scan(secretValue) {
		t.Fatal("destroyed detector retained matchable patterns")
	}
	redacted, detected := detector.Redact(secretValue)
	if detected || !bytes.Equal(redacted, secretValue) {
		t.Fatalf("destroyed detector Redact() = %q, %v", redacted, detected)
	}
	redacted[0] = 'X'
	if secretValue[0] == 'X' {
		t.Fatal("destroyed detector returned an aliased input")
	}
}

func TestDetectorFormattingAndSerializationRemainOpaque(t *testing.T) {
	secretValue := []byte("opaque-detector-secret")
	materialization, err := NewValueMaterialization(
		workerprofile.DeliveryFile,
		"/run/paje/secrets/source-detail",
		secretValue,
	)
	if err != nil {
		t.Fatal(err)
	}
	detector := NewDetector(materialization)

	for _, format := range []string{"%v", "%+v", "%#v", "%q", "%s"} {
		got := fmt.Sprintf(format, detector)
		if got != "[secret detector]" {
			t.Fatalf("format %q exposed detector state: %q", format, got)
		}
	}
	goStringer, ok := detector.(fmt.GoStringer)
	if !ok || goStringer.GoString() != "[secret detector]" {
		t.Fatalf("detector GoString() = %q, supported=%v", fmt.Sprintf("%#v", detector), ok)
	}
	if _, err := json.Marshal(detector); !errors.Is(err, ErrSecretSerialization) {
		t.Fatalf("json.Marshal(Detector) error = %v", err)
	}
	if strings.Contains(fmt.Sprint(detector), string(secretValue)) ||
		strings.Contains(fmt.Sprint(detector), materialization.Target()) {
		t.Fatal("detector formatting exposed a secret or source detail")
	}
}

func TestCallerOwnedSecretCopiesCanBeDestroyed(t *testing.T) {
	t.Run("payload value", func(t *testing.T) {
		payload := NewValuePayload([]byte("payload-secret"))
		retainedValue := payload.value
		payload.Destroy()
		for _, value := range retainedValue {
			if value != 0 {
				t.Fatal("Payload.Destroy did not zero its value")
			}
		}
		if payload.Kind() != "" || payload.Value() != nil || payload.Files() != nil {
			t.Fatalf("destroyed value payload accessors are non-empty: %q %q %#v", payload.Kind(), payload.Value(), payload.Files())
		}
	})

	t.Run("payload files", func(t *testing.T) {
		payload, err := NewDirectoryPayload([]File{mustFile(t, "auth.json", 0o600, []byte("payload-file-secret"))})
		if err != nil {
			t.Fatal(err)
		}
		retainedValue := payload.files[0].bytes
		payload.Destroy()
		for _, value := range retainedValue {
			if value != 0 {
				t.Fatal("Payload.Destroy did not zero a file value")
			}
		}
		if payload.Kind() != "" || payload.Value() != nil || payload.Files() != nil {
			t.Fatalf("destroyed directory payload accessors are non-empty: %q %q %#v", payload.Kind(), payload.Value(), payload.Files())
		}
	})

	t.Run("materialization value", func(t *testing.T) {
		materialization, err := NewValueMaterialization(
			workerprofile.DeliveryEnvironment,
			"WORKLOAD_TOKEN",
			[]byte("secret-value"),
		)
		if err != nil {
			t.Fatal(err)
		}
		retainedValue := materialization.value
		materialization.Destroy()
		for _, value := range retainedValue {
			if value != 0 {
				t.Fatal("Materialization.Destroy did not zero its value")
			}
		}
		if materialization.Delivery() != "" || materialization.Target() != "" ||
			materialization.Value() != nil || materialization.Files() != nil {
			t.Fatalf("destroyed materialization accessors are non-empty: %s %q %q %#v",
				materialization.Delivery(), materialization.Target(), materialization.Value(), materialization.Files())
		}
	})

	t.Run("materialization files", func(t *testing.T) {
		materialization, err := NewDirectoryMaterialization(
			"/run/paje/secrets/codex",
			[]File{mustFile(t, "auth.json", 0o600, []byte("file-secret"))},
		)
		if err != nil {
			t.Fatal(err)
		}
		retainedValue := materialization.files[0].bytes
		materialization.Destroy()
		for _, value := range retainedValue {
			if value != 0 {
				t.Fatal("Materialization.Destroy did not zero a retained file")
			}
		}
		if materialization.Delivery() != "" || materialization.Target() != "" || materialization.Files() != nil {
			t.Fatalf("destroyed directory materialization accessors are non-empty: %s %q %#v",
				materialization.Delivery(), materialization.Target(), materialization.Files())
		}
	})

	t.Run("lease", func(t *testing.T) {
		materialization, err := NewValueMaterialization(
			workerprofile.DeliveryFile,
			"/run/paje/secrets/token",
			[]byte("lease-secret"),
		)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := NewLease("sensitive-lease-id", time.Unix(200, 0).UTC(), materialization)
		if err != nil {
			t.Fatal(err)
		}
		returned := lease.Materialization()
		returnedValue := returned.value
		returned.Destroy()
		for _, value := range returnedValue {
			if value != 0 {
				t.Fatal("returned Materialization.Destroy did not zero its value")
			}
		}
		if returned.Delivery() != "" || returned.Target() != "" || returned.Value() != nil || returned.Files() != nil {
			t.Fatal("returned materialization accessors remained populated after Destroy")
		}
		retainedValue := lease.materialization.value
		lease.Destroy()
		for _, value := range retainedValue {
			if value != 0 {
				t.Fatal("Lease.Destroy did not zero its materialization")
			}
		}
		got := lease.Materialization()
		if lease.ID() != "" || !lease.ExpiresAt().IsZero() || got.Delivery() != "" ||
			got.Target() != "" || got.Value() != nil || got.Files() != nil {
			t.Fatalf("destroyed lease accessors are non-empty: %q %v %s %q %q %#v",
				lease.ID(), lease.ExpiresAt(), got.Delivery(), got.Target(), got.Value(), got.Files())
		}
	})
}

func TestTransientConstructorsRejectUnsafeTargetsAndOverlappingTrees(t *testing.T) {
	if _, err := NewValueMaterialization(workerprofile.DeliveryFile, "/tmp/token", []byte("secret")); err == nil {
		t.Fatal("file target outside the secret root was accepted")
	}
	if _, err := NewValueMaterialization(workerprofile.DeliveryEnvironment, "PATH", []byte("secret")); err == nil {
		t.Fatal("reserved environment target was accepted")
	}
	files := []File{
		mustFile(t, "auth", 0o600, []byte("one")),
		mustFile(t, "auth/token", 0o600, []byte("two")),
	}
	if _, err := NewDirectoryPayload(files); err == nil {
		t.Fatal("file and descendant path were accepted in one directory payload")
	}
}

func TestEnvironmentDeliveryRejectsAllBaselineReservedKeys(t *testing.T) {
	for _, target := range []string{
		"HOME", "PATH", "TMP", "TEMP", "TMPDIR", "PWD", "OLDPWD",
		"SHELL", "USER", "LOGNAME", "LANG", "CODEX_HOME", "GIT_ASKPASS", "SSH_AUTH_SOCK",
	} {
		t.Run(target, func(t *testing.T) {
			if err := ValidateEnvironmentTarget(target); err == nil {
				t.Fatal("reserved environment target passed validation")
			}
			if _, err := NewValueMaterialization(workerprofile.DeliveryEnvironment, target, []byte("secret")); err == nil {
				t.Fatal("reserved environment materialization was accepted")
			}
			if _, err := NewBinding(
				BindingRef{Capability: "workload.api-token", Revision: 1},
				Authorization{
					ProfileID: workerprofile.ProfileID{Name: "codex-go", Revision: 1},
					Stage:     workerprofile.StageAgent,
					Delivery:  workerprofile.DeliveryEnvironment,
					Target:    target,
				},
				"environment",
				"WORKLOAD_TOKEN_SOURCE",
			); err == nil {
				t.Fatal("binding with reserved environment target was accepted")
			}
		})
	}
}

func TestDirectoryPayloadValidationZeroesOwnedFilesOnError(t *testing.T) {
	tests := map[string][]File{
		"invalid": {
			{path: "", mode: 0o600, bytes: []byte("invalid-secret")},
		},
		"duplicate": {
			{path: "token", mode: 0o600, bytes: []byte("first-secret")},
			{path: "token", mode: 0o600, bytes: []byte("second-secret")},
		},
		"overlap": {
			{path: "auth", mode: 0o600, bytes: []byte("parent-secret")},
			{path: "auth/token", mode: 0o600, bytes: []byte("child-secret")},
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			retained := make([][]byte, len(files))
			for index := range files {
				retained[index] = files[index].bytes
			}
			if _, err := newDirectoryPayloadFromOwnedFiles(files); err == nil {
				t.Fatal("invalid owned directory files were accepted")
			}
			for _, value := range retained {
				for _, character := range value {
					if character != 0 {
						t.Fatal("directory payload validation left cloned secret bytes resident")
					}
				}
			}
		})
	}
}

func TestDirectoryPayloadSuccessTransfersOwnedFilesWithoutZeroing(t *testing.T) {
	files := []File{{path: "token", mode: 0o600, bytes: []byte("success-secret")}}
	retained := files[0].bytes
	payload, err := newDirectoryPayloadFromOwnedFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	defer zeroPayload(&payload)
	if !bytes.Equal(payload.files[0].bytes, []byte("success-secret")) {
		t.Fatalf("payload file = %q", payload.files[0].bytes)
	}
	if &payload.files[0].bytes[0] != &retained[0] {
		t.Fatal("successful directory payload did not take ownership of validated files")
	}
}

func TestBrokerAcquiresExactBindingAndCapsExpiry(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	request := validAcquireRequest(now.Add(30 * time.Second))
	registry := &bindingRegistryStub{binding: validBinding(t)}
	provider := &providerStub{payload: NewValuePayload([]byte("top-secret"))}
	broker, err := NewBroker(registry, map[string]Provider{"environment": provider}, BrokerConfig{
		LeaseTTL: time.Minute,
		Now:      func() time.Time { return now },
		Random:   bytes.NewReader(bytes.Repeat([]byte{0x2a}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}

	lease, err := broker.Acquire(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID() == "" || lease.ExpiresAt() != request.Deadline {
		t.Fatalf("lease identity/expiry = %q %v", lease.ID(), lease.ExpiresAt())
	}
	if registry.got != (ResolveRequest{
		ProfileID:   request.ProfileID,
		Ref:         BindingRef{Capability: request.Capability, Revision: request.Binding},
		Requirement: request.Delivery,
	}) {
		t.Fatalf("registry request = %#v", registry.got)
	}
	if provider.reference != "WORKLOAD_TOKEN_SOURCE" {
		t.Fatalf("provider reference = %q", provider.reference)
	}
	materialization := lease.Materialization()
	if materialization.Delivery() != workerprofile.DeliveryEnvironment || materialization.Target() != "WORKLOAD_TOKEN" ||
		!bytes.Equal(materialization.Value(), []byte("top-secret")) {
		t.Fatalf("materialization = %s %q %q", materialization.Delivery(), materialization.Target(), materialization.Value())
	}

	value := materialization.Value()
	value[0] = 'X'
	if bytes.Equal(lease.Materialization().Value(), value) {
		t.Fatal("lease materialization aliases caller-owned bytes")
	}
	if err := broker.Revoke(context.Background(), lease.ID()); err != nil {
		t.Fatal(err)
	}
	if err := broker.Revoke(context.Background(), lease.ID()); err != nil {
		t.Fatalf("idempotent revoke failed: %v", err)
	}
	if broker.ActiveLeases() != 0 {
		t.Fatalf("active leases = %d", broker.ActiveLeases())
	}
}

func TestBrokerRejectsMismatchedPayloadShapeAndUnknownProvider(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	request := validAcquireRequest(now.Add(time.Minute))
	registry := &bindingRegistryStub{binding: validBinding(t)}
	broker, err := NewBroker(registry, map[string]Provider{}, BrokerConfig{
		LeaseTTL: time.Minute, Now: func() time.Time { return now }, Random: bytes.NewReader(make([]byte, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Acquire(context.Background(), request); err == nil {
		t.Fatal("unknown source provider was accepted")
	}

	directory, err := NewDirectoryPayload([]File{mustFile(t, "auth.json", 0o600, []byte("secret"))})
	if err != nil {
		t.Fatal(err)
	}
	broker, err = NewBroker(registry, map[string]Provider{"environment": &providerStub{payload: directory}}, BrokerConfig{
		LeaseTTL: time.Minute, Now: func() time.Time { return now }, Random: bytes.NewReader(make([]byte, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Acquire(context.Background(), request); err == nil {
		t.Fatal("directory payload was accepted for environment delivery")
	}
}

func TestBrokerBoundsProviderReadByAttemptDeadline(t *testing.T) {
	now := time.Now().UTC()
	request := validAcquireRequest(now.Add(25 * time.Millisecond))
	provider := &providerStub{
		read: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	broker, err := NewBroker(&bindingRegistryStub{binding: validBinding(t)}, map[string]Provider{
		"environment": provider,
	}, BrokerConfig{
		LeaseTTL: time.Minute,
		Now:      func() time.Time { return now },
		Random:   bytes.NewReader(make([]byte, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err = broker.Acquire(context.Background(), request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("provider read exceeded bounded deadline: %v", elapsed)
	}
}

func TestBrokerRejectsMaterializationWhenDeadlineExpiresDuringProviderRead(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	deadline := now.Add(time.Second)
	provider := &providerStub{payload: NewValuePayload([]byte("secret"))}
	provider.read = func(context.Context) error {
		now = deadline.Add(time.Nanosecond)
		return nil
	}
	broker, err := NewBroker(&bindingRegistryStub{binding: validBinding(t)}, map[string]Provider{"environment": provider}, BrokerConfig{
		LeaseTTL: time.Minute, Now: func() time.Time { return now }, Random: bytes.NewReader(make([]byte, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Acquire(context.Background(), validAcquireRequest(deadline)); err == nil {
		t.Fatal("lease was created after its attempt deadline")
	}
	if broker.ActiveLeases() != 0 {
		t.Fatalf("active leases = %d", broker.ActiveLeases())
	}
}

func TestAcquireRequestRejectsUnsafeRunIDsAndInvalidAttemptWindow(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	for name, mutate := range map[string]func(*AcquireRequest){
		"leading underscore": func(request *AcquireRequest) { request.RunID = "_run" },
		"leading hyphen":     func(request *AcquireRequest) { request.RunID = "-run" },
		"unsafe punctuation": func(request *AcquireRequest) { request.RunID = "run.bad" },
		"too long":           func(request *AcquireRequest) { request.RunID = strings.Repeat("r", 129) },
		"zero window": func(request *AcquireRequest) {
			request.StartedAt = request.Deadline
		},
		"negative window": func(request *AcquireRequest) {
			request.StartedAt = request.Deadline.Add(time.Nanosecond)
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := validAcquireRequest(now.Add(time.Minute))
			mutate(&request)
			if err := validateAcquireRequest(request, now); err == nil {
				t.Fatal("invalid acquisition request was accepted")
			}
		})
	}

	request := validAcquireRequest(now.Add(time.Minute))
	request.RunID = "R" + strings.Repeat("a", 127)
	if err := validateAcquireRequest(request, now); err != nil {
		t.Fatalf("boundary-valid acquisition request rejected: %v", err)
	}
}

func TestBrokerExpiresAndZeroesLeaseWithoutAnotherBrokerCall(t *testing.T) {
	now := time.Now().UTC()
	broker, err := NewBroker(
		&bindingRegistryStub{binding: validBinding(t)},
		map[string]Provider{"environment": &providerStub{payload: NewValuePayload([]byte("secret"))}},
		BrokerConfig{LeaseTTL: 10 * time.Millisecond, Random: bytes.NewReader(make([]byte, 64))},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Acquire(context.Background(), validAcquireRequest(now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		broker.mu.Lock()
		stored := len(broker.leases)
		broker.mu.Unlock()
		if stored == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("expired broker-owned lease remained resident without another broker call")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestDetectorFindsAndRedactsRawAndReversibleEncodings(t *testing.T) {
	value := append([]byte{0xfb, 0xff}, []byte("secret-value-with-enough-entropy")...)
	materialization, err := NewValueMaterialization(workerprofile.DeliveryFile, "/run/paje/secrets/token", value)
	if err != nil {
		t.Fatal(err)
	}
	detector := NewDetector(materialization)
	encodings := [][]byte{
		value,
		[]byte(base64.StdEncoding.EncodeToString(value)),
		[]byte(base64.RawStdEncoding.EncodeToString(value)),
		[]byte(base64.URLEncoding.EncodeToString(value)),
		[]byte(base64.RawURLEncoding.EncodeToString(value)),
	}
	for _, encoded := range encodings {
		if !detector.Scan(append([]byte("before "), encoded...)) {
			t.Fatalf("detector missed %q", encoded)
		}
	}
	input := append(append([]byte("before "), encodings[1]...), []byte(" after")...)
	redacted, detected := detector.Redact(input)
	if !detected || bytes.Contains(redacted, encodings[1]) || !bytes.Contains(redacted, []byte("[REDACTED]")) {
		t.Fatalf("Redact() = %q, %v", redacted, detected)
	}
	if bytes.Equal(redacted, input) || !bytes.Contains(input, encodings[1]) {
		t.Fatal("Redact mutated input or returned aliased bytes")
	}
}

func TestAcquireAllRevokesPartialLeasesInReverseOrder(t *testing.T) {
	broker := &recordingBroker{failAt: 3}
	requests := []AcquireRequest{{Capability: "workload.one"}, {Capability: "workload.two"}, {Capability: "workload.three"}}
	if _, err := AcquireAll(context.Background(), broker, requests); err == nil {
		t.Fatal("partial acquisition succeeded")
	}
	if !slices.Equal(broker.revocations, []string{"lease-workload.two", "lease-workload.one"}) {
		t.Fatalf("revocations = %#v", broker.revocations)
	}
}

func TestAcquireAllRejectsOverlappingTargetsBeforeAcquisition(t *testing.T) {
	broker := &recordingBroker{}
	requests := []AcquireRequest{
		{Capability: "workload.one", Delivery: workerprofile.SecretRequirement{Delivery: workerprofile.DeliveryDirectory, Target: "/run/paje/secrets/auth"}},
		{Capability: "workload.two", Delivery: workerprofile.SecretRequirement{Delivery: workerprofile.DeliveryFile, Target: "/run/paje/secrets/auth/token"}},
	}
	if _, err := AcquireAll(context.Background(), broker, requests); err == nil {
		t.Fatal("overlapping secret targets were accepted")
	}
	if broker.acquisitions != 0 {
		t.Fatalf("broker acquired %d leases before target validation", broker.acquisitions)
	}
}

func validAcquireRequest(deadline time.Time) AcquireRequest {
	return AcquireRequest{
		RunID: "run-1", Attempt: 2, StartedAt: time.Unix(100, 5).UTC(),
		ProfileID:  workerprofile.ProfileID{Name: "codex-go", Revision: 1},
		Capability: "workload.api-token", Binding: 3,
		Delivery: workerprofile.SecretRequirement{
			Capability: "workload.api-token", Stage: workerprofile.StageAgent,
			Delivery: workerprofile.DeliveryEnvironment, Target: "WORKLOAD_TOKEN", Required: true,
		},
		Deadline: deadline,
	}
}

func validBinding(t *testing.T) Binding {
	t.Helper()
	binding, err := NewBinding(
		BindingRef{Capability: "workload.api-token", Revision: 3},
		Authorization{
			ProfileID: workerprofile.ProfileID{Name: "codex-go", Revision: 1},
			Stage:     workerprofile.StageAgent, Delivery: workerprofile.DeliveryEnvironment, Target: "WORKLOAD_TOKEN",
		},
		"environment", "WORKLOAD_TOKEN_SOURCE",
	)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func mustFile(t *testing.T, name string, mode uint32, value []byte) File {
	t.Helper()
	file, err := NewFile(name, mode, value)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

type bindingRegistryStub struct {
	binding Binding
	err     error
	got     ResolveRequest
}

func (registry *bindingRegistryStub) Resolve(_ context.Context, request ResolveRequest) (Binding, error) {
	registry.got = request
	return registry.binding, registry.err
}

type providerStub struct {
	payload   Payload
	err       error
	reference string
	read      func(context.Context) error
}

func (provider *providerStub) Read(ctx context.Context, reference string) (Payload, error) {
	provider.reference = reference
	if provider.read != nil {
		if err := provider.read(ctx); err != nil {
			return Payload{}, err
		}
	}
	return provider.payload.Clone(), provider.err
}

type recordingBroker struct {
	acquisitions int
	failAt       int
	revocations  []string
}

func (broker *recordingBroker) Acquire(_ context.Context, request AcquireRequest) (Lease, error) {
	broker.acquisitions++
	if broker.acquisitions == broker.failAt {
		return Lease{}, errors.New("acquire failed")
	}
	materialization, _ := NewValueMaterialization(workerprofile.DeliveryFile, "/run/paje/secrets/token", []byte("secret"))
	return NewLease("lease-"+request.Capability, time.Unix(200, 0).UTC(), materialization)
}

func (broker *recordingBroker) Revoke(_ context.Context, id string) error {
	broker.revocations = append(broker.revocations, id)
	return nil
}
