package admission

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/araihu/paje/internal/controlplane/journal"
)

func TestBoundedAdmissionPayloadAcrossReleasedLifetimeHistory(t *testing.T) {
	base, err := journal.NewMemoryStore("installation-cache-history")
	if err != nil {
		t.Fatal(err)
	}
	store := &payloadCountingStore{AuthoritativeStore: base}
	service := newCacheTestService(t, store)
	ctx := context.Background()

	var latest AdmissionRequest
	for index := range 48 {
		latest = cacheTestLargeAdmissionRequest(index)
		if _, err := service.Reserve(ctx, latest); err != nil {
			t.Fatalf("Reserve(%d) error = %v", index, err)
		}
		latest.Identity = cacheTestNextIdentity(latest.Identity, "enqueue")
		if _, err := service.Enqueue(ctx, latest); err != nil {
			t.Fatalf("Enqueue(%d) error = %v", index, err)
		}
		latest.Identity = cacheTestNextIdentity(latest.Identity, "admit")
		if _, err := service.Admit(ctx, latest); err != nil {
			t.Fatalf("Admit(%d) error = %v", index, err)
		}
		latest.Identity = cacheTestNextIdentity(latest.Identity, "release")
		if _, err := service.Release(ctx, latest); err != nil {
			t.Fatalf("Release(%d) error = %v", index, err)
		}
	}
	cumulativeBytes := authoritativePayloadBytes(t, base)
	if cumulativeBytes <= journal.MaxPayloadBytes {
		t.Fatalf("released lifetime payload bytes = %d, want > %d", cumulativeBytes, journal.MaxPayloadBytes)
	}
	t.Logf("released lifetime authoritative payload bytes = %d", cumulativeBytes)

	store.payloadCalls.Store(0)
	got, err := service.Admission(ctx, latest.ControlRunID, latest.Subject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != AdmissionReleased {
		t.Fatalf("latest admission state = %q, want %q", got.State, AdmissionReleased)
	}
	if calls := store.payloadCalls.Load(); calls > 2 {
		t.Fatalf("Payload calls after released lifetime history = %d, want at most 2 new authoritative payloads", calls)
	}
}

func TestAdmissionProjectionCacheConcurrentCachedReadsAreRaceFree(t *testing.T) {
	base, err := journal.NewMemoryStore("installation-cache-concurrent-read")
	if err != nil {
		t.Fatal(err)
	}
	const readers = 8
	store := &concurrentCachedReadStore{
		AuthoritativeStore: base,
		target:             readers,
		ready:              make(chan struct{}),
		release:            make(chan struct{}),
	}
	service := newCacheTestService(t, store)
	ctx := context.Background()
	request := cacheTestAdmissionRequest(1)
	if _, err := service.Reserve(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Admission(ctx, request.ControlRunID, request.Subject.ID); err != nil {
		t.Fatal(err)
	}

	store.enabled.Store(true)
	results := make(chan error, readers)
	for range readers {
		go func() {
			got, err := service.Admission(ctx, request.ControlRunID, request.Subject.ID)
			if err == nil && got.State != AdmissionReserved {
				err = fmt.Errorf("cached admission state = %q, want %q", got.State, AdmissionReserved)
			}
			results <- err
		}()
	}
	select {
	case <-store.ready:
		close(store.release)
	case <-time.After(5 * time.Second):
		close(store.release)
		t.Fatal("concurrent cached reads did not reach the empty-feed barrier")
	}
	for range readers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func authoritativePayloadBytes(t *testing.T, store journal.AuthoritativeStore) int {
	t.Helper()
	ctx := context.Background()
	cursor := journal.GlobalCursor{}
	total := 0
	for {
		events, next, err := store.Feed(ctx, cursor, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if !journal.IsOutcome(event.Kind) {
				continue
			}
			action, err := store.Reservation(ctx, event.ControlRunID, event.ActionID)
			if err != nil {
				t.Fatal(err)
			}
			requestPayload, err := store.Payload(ctx, action.CanonicalRequestDigest)
			if err != nil {
				t.Fatal(err)
			}
			outcomePayload, err := store.Payload(ctx, event.PayloadDigest)
			if err != nil {
				t.Fatal(err)
			}
			if len(requestPayload) > journal.MaxPayloadBytes || len(outcomePayload) > journal.MaxPayloadBytes {
				t.Fatalf("unbounded admission delta at position %d: request=%d outcome=%d", event.JournalPosition, len(requestPayload), len(outcomePayload))
			}
			total += len(requestPayload) + len(outcomePayload)
		}
		cursor = next
		if len(events) == 0 {
			return total
		}
	}
}

func TestAdmissionProjectionCacheRebuildsAfterRestartAndReplaysFromAuthority(t *testing.T) {
	base, err := journal.NewMemoryStore("installation-cache-restart")
	if err != nil {
		t.Fatal(err)
	}
	store := &payloadCountingStore{AuthoritativeStore: base}
	service := newCacheTestService(t, store)
	ctx := context.Background()
	request := cacheTestAdmissionRequest(1)
	reserved, err := service.Reserve(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Identity = cacheTestNextIdentity(request.Identity, "enqueue")
	if _, err := service.Enqueue(ctx, request); err != nil {
		t.Fatal(err)
	}

	restarted := newCacheTestService(t, store)
	store.payloadCalls.Store(0)
	got, err := restarted.Admission(ctx, request.ControlRunID, request.Subject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != AdmissionQueued || store.payloadCalls.Load() == 0 {
		t.Fatalf("restart rebuild = %#v with %d Payload calls, want queued journal rebuild", got, store.payloadCalls.Load())
	}
	store.payloadCalls.Store(0)
	got, err = restarted.Admission(ctx, request.ControlRunID, request.Subject.ID)
	if err != nil || got.State != AdmissionQueued {
		t.Fatalf("cached restart read = %#v, %v", got, err)
	}
	if calls := store.payloadCalls.Load(); calls != 0 {
		t.Fatalf("cached restart read Payload calls = %d, want 0", calls)
	}

	replay := cacheTestAdmissionRequest(1)
	store.payloadCalls.Store(0)
	replayed, err := restarted.Reserve(ctx, replay)
	if err != nil {
		t.Fatal(err)
	}
	reserved.Created = false
	reserved.Commit.Created = false
	if replayed.Created || !reflect.DeepEqual(replayed, reserved) {
		t.Fatalf("restart replay = %#v, want immutable %#v", replayed, reserved)
	}
	if calls := store.payloadCalls.Load(); calls != 0 {
		t.Fatalf("restart replay Payload calls = %d, want 0", calls)
	}
}

func TestAdmissionProjectionCacheDiscardsInvalidStateAndRebuildsAuthority(t *testing.T) {
	tests := []struct {
		name          string
		refreshDigest bool
		mutate        func(*projectionCache)
	}{
		{
			name: "corrupt projection",
			mutate: func(cache *projectionCache) {
				for key, value := range cache.projection.admissions {
					value.State = AdmissionAdmitted
					cache.projection.admissions[key] = value
					return
				}
			},
		},
		{
			name:          "foreign installation",
			refreshDigest: true,
			mutate: func(cache *projectionCache) {
				cache.cursor.InstallationID = "installation-foreign"
				cache.projection.installationID = "installation-foreign"
				cache.projection.global.InstallationID = "installation-foreign"
			},
		},
		{
			name:          "future cursor",
			refreshDigest: true,
			mutate: func(cache *projectionCache) {
				cache.cursor.JournalPosition += 2
				cache.projection.global.JournalPosition += 2
			},
		},
		{
			name:          "regressive cursor",
			refreshDigest: true,
			mutate: func(cache *projectionCache) {
				cache.cursor.JournalPosition = 0
				cache.projection.global.JournalPosition = 0
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, err := journal.NewMemoryStore("installation-cache-invalid")
			if err != nil {
				t.Fatal(err)
			}
			store := &payloadCountingStore{AuthoritativeStore: base}
			service := newCacheTestService(t, store)
			ctx := context.Background()
			request := cacheTestAdmissionRequest(1)
			if _, err := service.Reserve(ctx, request); err != nil {
				t.Fatal(err)
			}
			request.Identity = cacheTestNextIdentity(request.Identity, "enqueue")
			if _, err := service.Enqueue(ctx, request); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Admission(ctx, request.ControlRunID, request.Subject.ID); err != nil {
				t.Fatal(err)
			}

			service.cacheMu.Lock()
			test.mutate(service.cache)
			if test.refreshDigest {
				service.cache.digest, err = projectionCacheDigest(service.cache.projection)
				if err != nil {
					service.cacheMu.Unlock()
					t.Fatal(err)
				}
			}
			service.cacheMu.Unlock()
			store.payloadCalls.Store(0)
			got, err := service.Admission(ctx, request.ControlRunID, request.Subject.ID)
			if err != nil {
				t.Fatalf("Admission(after invalid cache) error = %v", err)
			}
			if got.State != AdmissionQueued {
				t.Fatalf("Admission(after invalid cache) = %#v, want queued authority", got)
			}
			if calls := store.payloadCalls.Load(); calls == 0 {
				t.Fatal("invalid cache was trusted without an authoritative Payload rebuild")
			}
		})
	}
}

func TestAdmissionProjectionCacheDiscardsCorruptReplayRecord(t *testing.T) {
	base, err := journal.NewMemoryStore("installation-cache-replay-corrupt")
	if err != nil {
		t.Fatal(err)
	}
	store := &payloadCountingStore{AuthoritativeStore: base}
	service := newCacheTestService(t, store)
	ctx := context.Background()
	request := cacheTestAdmissionRequest(1)
	want, err := service.Reserve(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Admission(ctx, request.ControlRunID, request.Subject.ID); err != nil {
		t.Fatal(err)
	}

	service.cacheMu.Lock()
	key := semanticIndex(request.ControlRunID, request.Identity.ActionID, request.Identity.Generation)
	record, exists := service.cache.projection.transitions[key]
	if !exists || record.outcome.Admission == nil {
		service.cacheMu.Unlock()
		t.Fatalf("cached replay transition = %#v, want admission outcome", record)
	}
	forged := *record.outcome.Admission
	forged.State = AdmissionAdmitted
	record.outcome.Admission = &forged
	service.cache.projection.transitions[key] = record
	service.cacheMu.Unlock()

	store.payloadCalls.Store(0)
	got, err := service.Reserve(ctx, request)
	if err != nil {
		t.Fatalf("Reserve(after corrupt replay cache) error = %v", err)
	}
	want.Created = false
	want.Commit.Created = false
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Reserve(after corrupt replay cache) = %#v, want authoritative %#v", got, want)
	}
	if calls := store.payloadCalls.Load(); calls == 0 {
		t.Fatal("corrupt replay cache was trusted without an authoritative Payload rebuild")
	}
}

func TestAdmissionProjectionCacheAdvancesStaleStateAndRejectsRegressiveInstall(t *testing.T) {
	base, err := journal.NewMemoryStore("installation-cache-stale")
	if err != nil {
		t.Fatal(err)
	}
	service := newCacheTestService(t, base)
	ctx := context.Background()
	request := cacheTestAdmissionRequest(1)
	if _, err := service.Reserve(ctx, request); err != nil {
		t.Fatal(err)
	}
	service.cacheMu.Lock()
	stale := cloneProjection(service.cache.projection)
	service.cacheMu.Unlock()
	request.Identity = cacheTestNextIdentity(request.Identity, "enqueue")
	if _, err := service.Enqueue(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Admission(ctx, request.ControlRunID, request.Subject.ID); err != nil {
		t.Fatal(err)
	}
	service.cacheMu.Lock()
	currentPosition := service.cache.cursor.JournalPosition
	service.cacheMu.Unlock()
	service.installProjectionCache(stale)
	service.cacheMu.Lock()
	installedPosition := service.cache.cursor.JournalPosition
	service.cacheMu.Unlock()
	if installedPosition != currentPosition {
		t.Fatalf("regressive cache install position = %d, want retained %d", installedPosition, currentPosition)
	}

	staleCache, err := newProjectionCache(stale)
	if err != nil {
		t.Fatal(err)
	}
	service.cacheMu.Lock()
	service.cache = staleCache
	service.cacheMu.Unlock()
	got, err := service.Admission(ctx, request.ControlRunID, request.Subject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != AdmissionQueued {
		t.Fatalf("Admission(from stale cache) = %#v, want queued authoritative suffix", got)
	}
	service.cacheMu.Lock()
	advancedPosition := service.cache.cursor.JournalPosition
	service.cacheMu.Unlock()
	if advancedPosition != currentPosition {
		t.Fatalf("advanced cache position = %d, want %d", advancedPosition, currentPosition)
	}
}

func TestAdmissionProjectionCacheDoesNotMaskJournalCorruption(t *testing.T) {
	base, err := journal.NewMemoryStore("installation-cache-journal-corrupt")
	if err != nil {
		t.Fatal(err)
	}
	service := newCacheTestService(t, base)
	if _, err := service.Admission(context.Background(), "run-empty", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Admission(empty authority) error = %v, want ErrNotFound", err)
	}
	action := journal.Action{
		ID: "admission_action_orphan", ControlRunID: "run-orphan", Kind: journal.KindAllocateResource,
		GraphRevision: 1, CanonicalRequestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		IdempotencyKey: "orphan-key",
	}
	if _, _, err := base.Reserve(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Admission(context.Background(), "run-orphan", "missing"); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Admission(orphaned authority) error = %v, want ErrInvalidRecord", err)
	}
}

type payloadCountingStore struct {
	journal.AuthoritativeStore
	payloadCalls atomic.Uint64
}

func (s *payloadCountingStore) Payload(ctx context.Context, digest string) ([]byte, error) {
	s.payloadCalls.Add(1)
	return s.AuthoritativeStore.Payload(ctx, digest)
}

type concurrentCachedReadStore struct {
	journal.AuthoritativeStore
	enabled  atomic.Bool
	arrivals atomic.Uint64
	target   uint64
	ready    chan struct{}
	release  chan struct{}
}

func (s *concurrentCachedReadStore) Feed(
	ctx context.Context,
	cursor journal.GlobalCursor,
	limit int,
) ([]journal.Event, journal.GlobalCursor, error) {
	events, next, err := s.AuthoritativeStore.Feed(ctx, cursor, limit)
	if err == nil && len(events) == 0 && s.enabled.Load() {
		if s.arrivals.Add(1) == s.target {
			close(s.ready)
		}
		<-s.release
	}
	return events, next, err
}

func newCacheTestService(t *testing.T, store journal.AuthoritativeStore) *Service {
	t.Helper()
	service, err := New(Dependencies{
		Store: store,
		Policy: Policy{
			Version: 1, InstallationLimit: 4096, PrincipalLimit: 4096,
			RunLimit: 4096, ProjectLimit: 4096, PrimitiveLimit: 4096,
		},
		Clock: func() time.Time { return time.Unix(100, 0).UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func cacheTestAdmissionRequest(index int) AdmissionRequest {
	suffix := fmt.Sprintf("%03d", index)
	return AdmissionRequest{
		ControlRunID: "run-" + suffix,
		Identity: TransitionIdentity{
			ActionID: "action-" + suffix, IdempotencyKey: "key-" + suffix,
			OutcomeEventID: "outcome-" + suffix, OutcomeKind: journal.EventActionResult,
			GraphRevision: 1, Generation: 1, TaskID: "task-" + suffix,
			AttemptID: "attempt-" + suffix,
		},
		Subject: AdmissionSubject{
			ID: "admission-" + suffix, PrincipalID: "principal-" + suffix,
			ProjectID: "project-" + suffix, Primitive: "persistent_session",
			WorkID: "work-" + suffix,
		},
	}
}

func cacheTestLargeAdmissionRequest(index int) AdmissionRequest {
	suffix := fmt.Sprintf("%03d", index)
	request := cacheTestAdmissionRequest(index)
	request.ControlRunID = cacheTestBoundedValue("run", suffix, 128)
	request.Identity.ActionID = cacheTestBoundedValue("action", suffix, 88)
	request.Identity.IdempotencyKey = cacheTestBoundedValue("key", suffix, 196)
	request.Identity.OutcomeEventID = cacheTestBoundedValue("outcome", suffix, 88)
	request.Identity.TaskID = cacheTestBoundedValue("task", suffix, 128)
	request.Identity.AttemptID = cacheTestBoundedValue("attempt", suffix, 128)
	request.Subject.ID = cacheTestBoundedValue("admission", suffix, 128)
	request.Subject.PrincipalID = cacheTestBoundedValue("principal", suffix, 128)
	request.Subject.ProjectID = cacheTestBoundedValue("project", suffix, 256)
	request.Subject.Primitive = cacheTestBoundedValue("primitive", suffix, 64)
	request.Subject.WorkID = cacheTestBoundedValue("work", suffix, 128)
	return request
}

func cacheTestBoundedValue(prefix, suffix string, limit int) string {
	value := prefix + "-" + suffix + "-"
	return value + strings.Repeat("x", limit-len(value))
}

func cacheTestNextIdentity(prior TransitionIdentity, suffix string) TransitionIdentity {
	prior.ActionID += "-" + suffix
	prior.IdempotencyKey += "-" + suffix
	prior.OutcomeEventID += "-" + suffix
	return prior
}
