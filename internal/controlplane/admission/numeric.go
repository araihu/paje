package admission

import "math"

func SaturatingAdd(left, right uint64) uint64 {
	if math.MaxUint64-left < right {
		return math.MaxUint64
	}
	return left + right
}

func SaturatingSub(left, right uint64) uint64 {
	if right > left {
		return 0
	}
	return left - right
}

func CeilQuantum(weight uint64) (uint64, error) {
	if weight == 0 {
		return 0, typedError(CodeInvalidRequest, "", ErrInvalidPolicy)
	}
	return 1 + (1024-1)/weight, nil
}

func SaturatingCap(value, cap uint64) uint64 {
	if value > cap {
		return cap
	}
	return value
}
