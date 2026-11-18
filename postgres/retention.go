package postgres

import (
	"context"
	p "github.com/StuHelper/stuhelper-profiles"
	"time"
)

const retentionSchema = `
ALTER TABLE profiles.requests ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS profiles_requests_created_idx ON profiles.requests(created_at);
ALTER TABLE profiles.closure_requests ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS profiles_closure_requests_created_idx ON profiles.closure_requests(created_at);
ALTER TABLE profiles.closures ADD COLUMN IF NOT EXISTS updated_at timestamptz;
-- Preserve at least the promised retention when older history lacks a cancellation event.
UPDATE profiles.closures c SET updated_at=CASE WHEN c.state='cancelled' THEN
 COALESCE((SELECT max(e.created_at) FROM profiles.closure_events e WHERE e.closure_id=c.id AND e.action='cancel'),current_timestamp)
 ELSE COALESCE((c.result->>'completed_at')::timestamptz,c.created_at) END WHERE c.updated_at IS NULL;
ALTER TABLE profiles.closures ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE profiles.closures ALTER COLUMN updated_at SET DEFAULT now();
CREATE INDEX IF NOT EXISTS profiles_closures_retention_idx ON profiles.closures(updated_at,id) WHERE state IN('closed','cancelled');
`

func (r *Repository) CleanupExpired(ctx context.Context, now time.Time, batchLimit int) (p.CleanupResult, error) {
	if now.IsZero() || batchLimit < 1 || batchLimit > 1000 {
		return p.CleanupResult{}, p.ErrInput
	}
	now = now.UTC()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return p.CleanupResult{}, p.ErrUnavailable
	}
	defer tx.Rollback()

	var out p.CleanupResult
	reqCutoff := now.Add(-24 * time.Hour)

	res, err := tx.ExecContext(ctx, `DELETE FROM profiles.requests WHERE ctid IN (SELECT ctid FROM profiles.requests WHERE created_at<$1 ORDER BY created_at ASC LIMIT $2)`, reqCutoff, batchLimit)
	if err != nil {
		return out, p.ErrUnavailable
	}
	n, err := res.RowsAffected()
	if err != nil {
		return out, p.ErrUnavailable
	}
	out.RequestsDeleted = int(n)

	res, err = tx.ExecContext(ctx, `DELETE FROM profiles.closure_requests WHERE ctid IN (SELECT ctid FROM profiles.closure_requests WHERE created_at<$1 ORDER BY created_at ASC LIMIT $2)`, reqCutoff, batchLimit)
	if err != nil {
		return out, p.ErrUnavailable
	}
	n, err = res.RowsAffected()
	if err != nil {
		return out, p.ErrUnavailable
	}
	out.ClosureRequestsDeleted = int(n)

	rows, err := tx.QueryContext(ctx, `SELECT id FROM profiles.closures WHERE state IN('closed','cancelled') AND updated_at<((($1::timestamptz AT TIME ZONE 'UTC')-INTERVAL '12 months') AT TIME ZONE 'UTC') ORDER BY updated_at ASC,id ASC LIMIT $2 FOR UPDATE SKIP LOCKED`, now, batchLimit)
	if err != nil {
		return out, p.ErrUnavailable
	}
	expiredIDs := make([]string, 0, batchLimit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, p.ErrUnavailable
		}
		expiredIDs = append(expiredIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, p.ErrUnavailable
	}
	rows.Close()

	if len(expiredIDs) > 0 {
		res, err = tx.ExecContext(ctx, `DELETE FROM profiles.closure_events WHERE closure_id=ANY($1)`, expiredIDs)
		if err != nil {
			return out, p.ErrUnavailable
		}
		n, err = res.RowsAffected()
		if err != nil {
			return out, p.ErrUnavailable
		}
		out.ClosureEventsDeleted = int(n)

		res, err = tx.ExecContext(ctx, `DELETE FROM profiles.closures WHERE id=ANY($1)`, expiredIDs)
		if err != nil {
			return out, p.ErrUnavailable
		}
		n, err = res.RowsAffected()
		if err != nil {
			return out, p.ErrUnavailable
		}
		out.ClosuresDeleted = int(n)
	}

	if err := tx.Commit(); err != nil {
		return p.CleanupResult{}, p.ErrUnavailable
	}
	return out, nil
}
