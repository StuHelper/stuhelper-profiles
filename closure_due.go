package profiles

import (
	"context"
	"errors"
	"slices"
	"time"
)

const (
	PendingReasonIvoryBalanceRemaining        = "ivory_balance_remaining"
	PendingReasonRefundInProgress             = "refund_in_progress"
	PendingReasonPaymentPendingReconciliation = "payment_pending_reconciliation"
)

var allowedPendingReasons = []string{
	PendingReasonIvoryBalanceRemaining,
	PendingReasonRefundInProgress,
	PendingReasonPaymentPendingReconciliation,
}

// AssetsPendingError conveys sanitized asset settlement blocker reasons while
// unwrapping to ErrAssetsPending.
type AssetsPendingError struct {
	Reasons []string
}

func (e *AssetsPendingError) Error() string { return ErrAssetsPending.Error() }
func (e *AssetsPendingError) Unwrap() error { return ErrAssetsPending }

type AssetsClearance struct {
	SkippedModules []string
	PendingReasons []string
}

type AssetsClearanceEffects interface {
	ClearAssetsDetail(ctx context.Context, owner Principal, closureID string) (AssetsClearance, error)
}

type closureReasonsRepository interface {
	AdvanceClosureByWithReasons(ctx context.Context, actor Principal, id string, step int, blocker string, pendingReasons, skipped []string, now time.Time) (Closure, error)
}

func NormalizePendingReasons(reasons []string) ([]string, bool) {
	if len(reasons) == 0 {
		return nil, true
	}
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		if !slices.Contains(allowedPendingReasons, r) {
			return nil, false
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out, true
}

func extractPendingReasons(reasons []string, err error) []string {
	merged := append([]string(nil), reasons...)
	var ape *AssetsPendingError
	if errors.As(err, &ape) && ape != nil {
		merged = append(merged, ape.Reasons...)
	}
	return merged
}

// DueClosures returns up to limit closures whose cooling period has elapsed or
// that are waiting for retry, ordered oldest-first for admin badges and scheduled runners.
func (s *Service) DueClosures(ctx context.Context, p Principal, limit int) ([]Closure, error) {
	if e := s.authorizeClosure(ctx, p); e != nil {
		return nil, e
	}
	if limit < 1 || limit > 100 {
		return nil, ErrInput
	}
	r, ok := s.repo.(interface {
		DueClosures(context.Context, time.Time, int) ([]Closure, error)
	})
	if !ok {
		return nil, ErrUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return r.DueClosures(callCtx, s.now(), limit)
}
