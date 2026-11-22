package postgres

import (
	"context"
	_ "embed"
	"time"

	p "github.com/StuHelper/stuhelper-profiles"
)

//go:embed retention.sql
var retentionSchema string

func (r *Repository) CleanupExpired(ctx context.Context, now time.Time, batchLimit int) (p.CleanupResult, error) {
	if now.IsZero() || batchLimit < 1 || batchLimit > 1000 {
		return p.CleanupResult{}, p.ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var out p.CleanupResult
	err := r.DB.QueryRowContext(ctx, `SELECT requests_deleted,closure_requests_deleted,closures_deleted,closure_events_deleted FROM profiles.cleanup_expired($1,$2)`, now.UTC(), batchLimit).
		Scan(&out.RequestsDeleted, &out.ClosureRequestsDeleted, &out.ClosuresDeleted, &out.ClosureEventsDeleted)
	if err != nil {
		return p.CleanupResult{}, p.ErrUnavailable
	}
	return out, nil
}
