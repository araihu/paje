package scheduler

import (
	"context"
	"sort"
	"time"

	"github.com/araihu/paje/internal/controlplane/admission"
)

func rankReady(items []ReadyItem, now time.Time, policy Policy) ([]RankedItem, error) {
	if err := policy.validate(); err != nil {
		return nil, err
	}
	ranked := make([]RankedItem, 0, len(items))
	identities := make(map[string]struct{}, len(items))
	for _, item := range items {
		if err := validateReadyItem(item); err != nil {
			return nil, err
		}
		identity := item.ControlRunID + "\x00" + item.ID
		if _, exists := identities[identity]; exists {
			return nil, typedError(CodeInvalidRequest, "rank", ErrInvalidRecord)
		}
		identities[identity] = struct{}{}
		quantum, err := admission.CeilQuantum(item.Weight)
		if err != nil {
			return nil, err
		}
		finish := admission.SaturatingAdd(item.VirtualStart, quantum)
		credit := ageCredit(item.EnqueuedAt, now, policy)
		ranked = append(ranked, RankedItem{
			Item: item, VirtualFinish: finish, AgeCredit: credit,
			EffectiveVirtualFinish: admission.SaturatingSub(finish, credit),
		})
	}
	sort.Slice(ranked, func(left, right int) bool {
		one, two := ranked[left], ranked[right]
		if one.EffectiveVirtualFinish != two.EffectiveVirtualFinish {
			return one.EffectiveVirtualFinish < two.EffectiveVirtualFinish
		}
		if one.Item.EnqueueSequence != two.Item.EnqueueSequence {
			return one.Item.EnqueueSequence < two.Item.EnqueueSequence
		}
		if one.Item.ControlRunID != two.Item.ControlRunID {
			return one.Item.ControlRunID < two.Item.ControlRunID
		}
		return one.Item.ID < two.Item.ID
	})
	return ranked, nil
}

func selectReady(
	items []ReadyItem,
	state FairnessState,
	now time.Time,
	policy Policy,
) (RankedItem, FairnessState, error) {
	if len(items) == 0 {
		return RankedItem{}, state, typedError(CodeNoEligible, "select", ErrNoEligible)
	}
	if (state.LastRunID == "") != (state.ConsecutiveAdmissions == 0) || len(state.LastRunID) > 128 {
		return RankedItem{}, state, typedError(CodeInvalidRequest, "select", ErrInvalidRecord)
	}
	ranked, err := rankReady(items, now, policy)
	if err != nil {
		return RankedItem{}, state, err
	}
	selected := ranked[0]
	if state.LastRunID != "" && state.ConsecutiveAdmissions >= policy.ConsecutiveLimit {
		for _, candidate := range ranked {
			if candidate.Item.ControlRunID != state.LastRunID {
				selected = candidate
				break
			}
		}
	}
	return selected, advanceFairness(state, selected.Item.ControlRunID), nil
}

func (s *Service) AcquireResource(
	ctx context.Context,
	request admission.LeaseRequest,
) (admission.LeaseReceipt, error) {
	receipt, err := s.authority.AcquireLease(ctx, request)
	if err != nil {
		return admission.LeaseReceipt{}, typedError(CodeAuthority, "acquire_resource", err)
	}
	return receipt, nil
}

func (s *Service) ReleaseResource(
	ctx context.Context,
	request admission.LeaseRequest,
) (admission.LeaseReceipt, error) {
	receipt, err := s.authority.ReleaseLease(ctx, request)
	if err != nil {
		return admission.LeaseReceipt{}, typedError(CodeAuthority, "release_resource", err)
	}
	return receipt, nil
}

func (s *Service) ExpireResource(
	ctx context.Context,
	request admission.LeaseRequest,
) (admission.LeaseReceipt, error) {
	receipt, err := s.authority.ExpireLease(ctx, request)
	if err != nil {
		return admission.LeaseReceipt{}, typedError(CodeAuthority, "expire_resource", err)
	}
	return receipt, nil
}

func validateReadyItem(item ReadyItem) error {
	if !bounded(item.ID, 128) || !bounded(item.ControlRunID, 128) || item.EnqueueSequence == 0 ||
		item.EnqueuedAt.IsZero() || item.Admission.ControlRunID != item.ControlRunID ||
		item.Admission.Subject.ID != item.ID || item.Admission.Subject.WorkID != item.ID {
		return typedError(CodeInvalidRequest, "rank", ErrInvalidRecord)
	}
	return nil
}

func ageCredit(enqueuedAt, now time.Time, policy Policy) uint64 {
	if now.Before(enqueuedAt) {
		return 0
	}
	periods := uint64(now.Sub(enqueuedAt) / policy.AgingInterval)
	return admission.SaturatingCap(periods, policy.MaxAgeCredit)
}

func advanceFairness(state FairnessState, runID string) FairnessState {
	if state.LastRunID == runID {
		state.ConsecutiveAdmissions = admission.SaturatingAdd(state.ConsecutiveAdmissions, 1)
		return state
	}
	return FairnessState{LastRunID: runID, ConsecutiveAdmissions: 1}
}
