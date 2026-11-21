package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	p "github.com/StuHelper/stuhelper-profiles"
	"time"
)

func (r *Repository) DueClosures(ctx context.Context, now time.Time, limit int) ([]p.Closure, error) {
	return r.dueClosures(ctx, now, limit, false)
}

// RunnableClosures excludes cases already assigned to the administrator queue
// and requests with a live processing lease on another process.
func (r *Repository) RunnableClosures(ctx context.Context, now time.Time, limit int) ([]p.Closure, error) {
	return r.dueClosures(ctx, now, limit, true)
}

func (r *Repository) dueClosures(ctx context.Context, now time.Time, limit int, automatic bool) ([]p.Closure, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return nil, p.ErrInput
	}
	now = now.UTC()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out := make([]p.Closure, 0, limit)
	hasCursor := false
	var cursorAt time.Time
	var cursorID string
	cutoff := now.Add(-7*24*time.Hour + time.Millisecond)
	query := `SELECT created_at,id,result FROM profiles.closures
 WHERE (state IN('awaiting_assets','processing') OR (state='cooling' AND created_at<=$1))`
	if automatic {
		query += ` AND COALESCE(result->>'blocker','')='' AND (processing_until IS NULL OR processing_until<=clock_timestamp())`
	}

	for len(out) < limit {
		batch := limit - len(out)
		var rows *sql.Rows
		var err error
		if hasCursor {
			rows, err = r.DB.QueryContext(ctx, query+` AND (created_at,id)>($3,$4) ORDER BY created_at,id LIMIT $2`, cutoff, batch, cursorAt, cursorID)
		} else {
			rows, err = r.DB.QueryContext(ctx, query+` ORDER BY created_at,id LIMIT $2`, cutoff, batch)
		}
		if err != nil {
			return nil, p.ErrUnavailable
		}
		fetched := 0
		for rows.Next() {
			fetched++
			var rowAt time.Time
			var rowID string
			var raw []byte
			if err := rows.Scan(&rowAt, &rowID, &raw); err != nil {
				rows.Close()
				return nil, p.ErrUnavailable
			}
			var item p.Closure
			if json.Unmarshal(raw, &item) != nil || !p.ValidClosureProgress(item) {
				rows.Close()
				return nil, p.ErrUnavailable
			}
			cursorAt = rowAt
			cursorID = rowID
			hasCursor = true
			if item.State == "cooling" && now.Before(item.CoolingEndsAt) {
				continue
			}
			out = append(out, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, p.ErrUnavailable
		}
		rows.Close()
		if fetched < batch {
			break
		}
	}
	return out, nil
}
