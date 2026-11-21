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

var closureWorker = Principal{Issuer: "urn:stuhelper:profiles-service", Subject: "closure"}

type ClosureWorkCursor struct {
	CreatedAt time.Time
	ID        string
}

type closureWorkRepository interface {
	RunnableClosures(context.Context, time.Time, ClosureWorkCursor, int) ([]ClosureWorkCursor, error)
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

// ProcessDueClosures is a trusted host maintenance port. It starts expired
// requests and resumes interrupted work in order, using the existing closure
// ID for durable effects. Blocked cases remain in the administrator queue.
func (s *Service) ProcessDueClosures(ctx context.Context, limit int) ([]Closure, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInput
	}
	if s.ClosureEffects == nil {
		return nil, ErrUnavailable
	}
	s.dueClosureMu.Lock()
	defer s.dueClosureMu.Unlock()
	requests, err := s.dueClosureWork(ctx, s.dueClosureAfter, limit)
	if err != nil {
		return nil, err
	}
	results := make([]Closure, 0, len(requests))
	var workErr error
	for _, item := range requests {
		if ctx.Err() != nil {
			return results, errors.Join(workErr, ctx.Err())
		}
		// Advance even on a malformed or unavailable record. The next pass
		// revisits failures without blocking later candidates in this pass.
		s.dueClosureAfter = item
		result, err := s.processClosure(ctx, closureWorker, "due-"+item.ID, item.ID, true)
		switch {
		case err == nil:
			results = append(results, result)
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrConflict):
			// Another actor may have changed the candidate since the scan.
		default:
			workErr = errors.Join(workErr, err)
		}
	}
	if len(requests) < limit {
		s.dueClosureAfter = ClosureWorkCursor{}
	}
	return results, workErr
}

// HasDueClosureWork checks for automatic work when the host cannot run effects.
func (s *Service) HasDueClosureWork(ctx context.Context) (bool, error) {
	requests, err := s.dueClosureWork(ctx, ClosureWorkCursor{}, 1)
	return len(requests) != 0, err
}

func (s *Service) dueClosureWork(ctx context.Context, after ClosureWorkCursor, limit int) ([]ClosureWorkCursor, error) {
	repo, ok := s.repo.(closureWorkRepository)
	if !ok {
		return nil, ErrUnavailable
	}
	scanCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return repo.RunnableClosures(scanCtx, s.now(), after, limit)
}
