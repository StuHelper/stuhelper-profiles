package profiles

import (
	"context"
	"time"
)

type CleanupResult struct {
	RequestsDeleted        int `json:"requests_deleted"`
	ClosureRequestsDeleted int `json:"closure_requests_deleted"`
	ClosuresDeleted        int `json:"closures_deleted"`
	ClosureEventsDeleted   int `json:"closure_events_deleted"`
}

type RetentionRepository interface {
	CleanupExpired(ctx context.Context, now time.Time, batchLimit int) (CleanupResult, error)
}

type updateAtRepository interface {
	UpdateAt(ctx context.Context, who Principal, key string, in Update, now time.Time) (Profile, error)
}

// CleanupExpired boundedly removes 24-hour expired idempotency responses and
// 12-month completed or cancelled closure histories while keeping closed account markers.
func (s *Service) CleanupExpired(ctx context.Context, now time.Time, batchLimit int) (CleanupResult, error) {
	if now.IsZero() || batchLimit < 1 || batchLimit > 1000 {
		return CleanupResult{}, ErrInput
	}
	r, ok := s.repo.(RetentionRepository)
	if !ok {
		return CleanupResult{}, ErrUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return r.CleanupExpired(callCtx, now.UTC(), batchLimit)
}
