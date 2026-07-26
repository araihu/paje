package secret

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/araihu/paje/internal/workerprofile"
)

var acquisitionRunIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type BrokerConfig struct {
	LeaseTTL time.Duration
	Now      func() time.Time
	Random   io.Reader
}

type MemoryBroker struct {
	registry  Registry
	providers map[string]Provider
	leaseTTL  time.Duration
	now       func() time.Time
	random    io.Reader
	randomMu  sync.Mutex

	mu     sync.Mutex
	leases map[string]*Lease
	timers map[string]*time.Timer
}

func NewBroker(registry Registry, providers map[string]Provider, config BrokerConfig) (*MemoryBroker, error) {
	if registry == nil || config.LeaseTTL <= 0 {
		return nil, errors.New("secret broker registry and positive lease TTL are required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	providerCopy := make(map[string]Provider, len(providers))
	for name, provider := range providers {
		if !providerNamePattern.MatchString(name) || provider == nil {
			return nil, errors.New("invalid secret provider registration")
		}
		providerCopy[name] = provider
	}
	return &MemoryBroker{
		registry: registry, providers: providerCopy, leaseTTL: config.LeaseTTL,
		now: config.Now, random: config.Random, leases: make(map[string]*Lease), timers: make(map[string]*time.Timer),
	}, nil
}

func (broker *MemoryBroker) Acquire(ctx context.Context, request AcquireRequest) (Lease, error) {
	now := broker.now().UTC()
	if err := validateAcquireRequest(request, now); err != nil {
		return Lease{}, err
	}
	binding, err := broker.registry.Resolve(ctx, ResolveRequest{
		ProfileID:   request.ProfileID,
		Ref:         BindingRef{Capability: request.Capability, Revision: request.Binding},
		Requirement: request.Delivery,
	})
	if err != nil {
		return Lease{}, fmt.Errorf("resolve secret binding: %w", err)
	}
	if !binding.Authorizes(ResolveRequest{
		ProfileID:   request.ProfileID,
		Ref:         BindingRef{Capability: request.Capability, Revision: request.Binding},
		Requirement: request.Delivery,
	}) {
		return Lease{}, ErrBindingUnauthorized
	}
	providerName, reference := binding.Source()
	provider, ok := broker.providers[providerName]
	if !ok {
		return Lease{}, ErrSourceUnavailable
	}
	readCtx, cancelRead := context.WithTimeout(ctx, request.Deadline.Sub(now))
	payload, err := provider.Read(readCtx, reference)
	readContextErr := readCtx.Err()
	cancelRead()
	if err != nil {
		return Lease{}, fmt.Errorf("read secret source: %w", err)
	}
	if readContextErr != nil {
		payload.Destroy()
		return Lease{}, readContextErr
	}
	now = broker.now().UTC()
	if err := ctx.Err(); err != nil {
		zeroPayload(&payload)
		return Lease{}, err
	}
	if !request.Deadline.After(now) {
		zeroPayload(&payload)
		return Lease{}, errors.New("secret acquisition deadline expired")
	}
	materialization, err := materialize(payload, request.Delivery)
	if err != nil {
		zeroPayload(&payload)
		return Lease{}, err
	}
	zeroPayload(&payload)

	expiresAt := now.Add(broker.leaseTTL)
	if request.Deadline.Before(expiresAt) {
		expiresAt = request.Deadline.UTC()
	}
	idBytes := make([]byte, 32)
	broker.randomMu.Lock()
	_, randomErr := io.ReadFull(broker.random, idBytes)
	broker.randomMu.Unlock()
	if randomErr != nil {
		materialization.Destroy()
		return Lease{}, fmt.Errorf("generate secret lease identity: %w", randomErr)
	}
	id := hex.EncodeToString(idBytes)
	zeroBytes(idBytes)
	lease, err := NewLease(id, expiresAt, materialization)
	materialization.Destroy()
	if err != nil {
		return Lease{}, err
	}

	broker.mu.Lock()
	broker.reapExpiredLocked(now)
	if _, collision := broker.leases[id]; collision {
		broker.mu.Unlock()
		lease.Destroy()
		return Lease{}, errors.New("secret lease identity collision")
	}
	stored := cloneLease(lease)
	result := cloneLease(lease)
	broker.leases[id] = &stored
	broker.timers[id] = time.AfterFunc(expiresAt.Sub(now), func() {
		broker.expire(id, expiresAt)
	})
	broker.mu.Unlock()
	lease.Destroy()
	return result, nil
}

func (broker *MemoryBroker) Revoke(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	lease, ok := broker.leases[id]
	if !ok {
		return nil
	}
	lease.Destroy()
	delete(broker.leases, id)
	if timer := broker.timers[id]; timer != nil {
		timer.Stop()
	}
	delete(broker.timers, id)
	return nil
}

func (broker *MemoryBroker) ActiveLeases() int {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.reapExpiredLocked(broker.now().UTC())
	return len(broker.leases)
}

func (broker *MemoryBroker) Detector() Detector {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.reapExpiredLocked(broker.now().UTC())
	materializations := make([]Materialization, 0, len(broker.leases))
	for _, lease := range broker.leases {
		materializations = append(materializations, lease.materialization.Clone())
	}
	detector := NewDetector(materializations...)
	for index := range materializations {
		materializations[index].Destroy()
	}
	return detector
}

func (broker *MemoryBroker) reapExpiredLocked(now time.Time) {
	for id, lease := range broker.leases {
		if !lease.expiresAt.After(now) {
			lease.Destroy()
			delete(broker.leases, id)
			if timer := broker.timers[id]; timer != nil {
				timer.Stop()
			}
			delete(broker.timers, id)
		}
	}
}

func (broker *MemoryBroker) expire(id string, expiresAt time.Time) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	lease, ok := broker.leases[id]
	if !ok || !lease.expiresAt.Equal(expiresAt) {
		return
	}
	lease.Destroy()
	delete(broker.leases, id)
	delete(broker.timers, id)
}

func validateAcquireRequest(request AcquireRequest, now time.Time) error {
	if !acquisitionRunIDPattern.MatchString(request.RunID) || request.Attempt <= 0 || request.StartedAt.IsZero() ||
		request.Binding == 0 || !validCapability(request.Capability) ||
		request.Delivery.Capability != request.Capability || !request.Delivery.Required ||
		request.Delivery.Stage != workerprofile.StageAgent ||
		!validDeliveryTarget(request.Delivery.Delivery, request.Delivery.Target) ||
		!request.Deadline.After(now) || !request.StartedAt.Before(request.Deadline) {
		return errors.New("invalid secret acquisition request")
	}
	parsed, err := workerprofile.ParseProfileID(request.ProfileID.String())
	if err != nil || parsed != request.ProfileID {
		return errors.New("invalid secret acquisition profile")
	}
	return nil
}

func materialize(payload Payload, requirement workerprofile.SecretRequirement) (Materialization, error) {
	switch payload.kind {
	case payloadValue:
		return NewValueMaterialization(requirement.Delivery, requirement.Target, payload.value)
	case payloadDirectory:
		if requirement.Delivery != workerprofile.DeliveryDirectory {
			return Materialization{}, errors.New("secret source shape does not match delivery")
		}
		return NewDirectoryMaterialization(requirement.Target, payload.files)
	default:
		return Materialization{}, ErrSourceInvalid
	}
}

func zeroPayload(payload *Payload) {
	payload.Destroy()
}

func AcquireAll(ctx context.Context, broker Broker, requests []AcquireRequest) ([]Lease, error) {
	for index, request := range requests {
		for prior := range index {
			if acquireTargetsOverlap(request.Delivery, requests[prior].Delivery) {
				return nil, errors.New("secret acquisition targets overlap")
			}
		}
	}
	leases := make([]Lease, 0, len(requests))
	for _, request := range requests {
		lease, err := broker.Acquire(ctx, request)
		if err == nil {
			leases = append(leases, lease)
			continue
		}
		compensationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		var compensationErrors []error
		for index := len(leases) - 1; index >= 0; index-- {
			if revokeErr := broker.Revoke(compensationCtx, leases[index].ID()); revokeErr != nil {
				compensationErrors = append(compensationErrors, revokeErr)
			}
			leases[index].Destroy()
		}
		cancel()
		if len(compensationErrors) > 0 {
			return nil, errors.Join(err, errors.Join(compensationErrors...))
		}
		return nil, err
	}
	return leases, nil
}

func acquireTargetsOverlap(first, second workerprofile.SecretRequirement) bool {
	if first.Target == "" || second.Target == "" {
		return false
	}
	if first.Delivery == workerprofile.DeliveryEnvironment || second.Delivery == workerprofile.DeliveryEnvironment {
		return first.Delivery == workerprofile.DeliveryEnvironment && second.Delivery == workerprofile.DeliveryEnvironment &&
			first.Target == second.Target
	}
	return first.Target == second.Target || strings.HasPrefix(first.Target, second.Target+"/") ||
		strings.HasPrefix(second.Target, first.Target+"/")
}

type Detector interface {
	Scan([]byte) bool
	Redact([]byte) (redacted []byte, detected bool)
	Destroy()
}

type exactDetector struct {
	mu       sync.RWMutex
	patterns [][]byte
}

func NewDetector(materializations ...Materialization) Detector {
	patterns := make([][]byte, 0)
	for _, materialization := range materializations {
		if len(materialization.value) > 0 {
			addDetectorPatterns(&patterns, materialization.value)
		}
		for _, file := range materialization.files {
			if len(file.bytes) > 0 {
				addDetectorPatterns(&patterns, file.bytes)
			}
		}
	}
	sort.Slice(patterns, func(i, j int) bool {
		if len(patterns[i]) != len(patterns[j]) {
			return len(patterns[i]) > len(patterns[j])
		}
		return bytes.Compare(patterns[i], patterns[j]) < 0
	})
	return &exactDetector{patterns: patterns}
}

func addDetectorPatterns(patterns *[][]byte, value []byte) {
	addDetectorPattern(patterns, value)
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		encoded := encodeDetectorPattern(encoding, value)
		addDetectorPattern(patterns, encoded)
		zeroBytes(encoded)
	}
}

func addDetectorPattern(patterns *[][]byte, pattern []byte) {
	if len(pattern) == 0 {
		return
	}
	for _, existing := range *patterns {
		if bytes.Equal(existing, pattern) {
			return
		}
	}
	*patterns = append(*patterns, slices.Clone(pattern))
}

func encodeDetectorPattern(encoding *base64.Encoding, value []byte) []byte {
	encoded := make([]byte, encoding.EncodedLen(len(value)))
	encoding.Encode(encoded, value)
	return encoded
}

func (detector *exactDetector) Scan(input []byte) bool {
	detector.mu.RLock()
	defer detector.mu.RUnlock()
	for _, pattern := range detector.patterns {
		if bytes.Contains(input, pattern) {
			return true
		}
	}
	return false
}

func (detector *exactDetector) Redact(input []byte) ([]byte, bool) {
	detector.mu.RLock()
	defer detector.mu.RUnlock()
	redacted := slices.Clone(input)
	detected := false
	for _, pattern := range detector.patterns {
		if bytes.Contains(redacted, pattern) {
			detected = true
			redacted = bytes.ReplaceAll(redacted, pattern, []byte("[REDACTED]"))
		}
	}
	return redacted, detected
}

func (detector *exactDetector) Destroy() {
	if detector == nil {
		return
	}
	detector.mu.Lock()
	defer detector.mu.Unlock()
	for _, pattern := range detector.patterns {
		zeroBytes(pattern)
	}
	detector.patterns = nil
}

func (*exactDetector) String() string   { return "[secret detector]" }
func (*exactDetector) GoString() string { return "[secret detector]" }
func (*exactDetector) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[secret detector]"))
}
func (*exactDetector) MarshalJSON() ([]byte, error) { return nil, ErrSecretSerialization }
func (*exactDetector) MarshalText() ([]byte, error) { return nil, ErrSecretSerialization }
