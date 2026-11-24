package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	p "github.com/StuHelper/stuhelper-profiles"
	"time"
)

func (r *Repository) DueClosures(ctx context.Context, now time.Time, limit int) ([]p.Closure, error) {
	rows, err := r.dueClosures(ctx, now, p.ClosureWorkCursor{}, limit, false)
	if err != nil {
		return nil, err
	}
	closures := make([]p.Closure, 0, len(rows))
	for _, row := range rows {
		closures = append(closures, row.closure)
	}
	return closures, nil
}

// RunnableClosures excludes cases already assigned to the administrator queue
// and requests with a live processing lease on another process.
func (r *Repository) RunnableClosures(ctx context.Context, now time.Time, after p.ClosureWorkCursor, limit int) ([]p.ClosureWorkCursor, error) {
	rows, err := r.dueClosures(ctx, now, after, limit, true)
	if err != nil {
		return nil, err
	}
	candidates := make([]p.ClosureWorkCursor, 0, len(rows))
	for _, row := range rows {
		candidates = append(candidates, row.cursor)
	}
	return candidates, nil
}

type dueClosureRow struct {
	cursor  p.ClosureWorkCursor
	closure p.Closure
}

func (r *Repository) dueClosures(ctx context.Context, now time.Time, after p.ClosureWorkCursor, limit int, automatic bool) ([]dueClosureRow, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return nil, p.ErrInput
	}
	now = now.UTC()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out := make([]dueClosureRow, 0, limit)
	cursor := after
	cutoff := now.Add(-7*24*time.Hour + time.Millisecond)
	query := `SELECT created_at,id,result FROM profiles.closures
 WHERE (state IN('awaiting_assets','processing') OR (state='cooling' AND created_at<=$1))`
	if automatic {
		query += ` AND COALESCE(result->>'blocker','') IN ('','background_processing') AND (processing_until IS NULL OR processing_until<=clock_timestamp())`
	}

	for len(out) < limit {
		batch := limit - len(out)
		var rows *sql.Rows
		var err error
		if cursor != (p.ClosureWorkCursor{}) {
			rows, err = r.DB.QueryContext(ctx, query+` AND (created_at,id)>($3,$4) ORDER BY created_at,id LIMIT $2`, cutoff, batch, cursor.CreatedAt, cursor.ID)
		} else {
			rows, err = r.DB.QueryContext(ctx, query+` ORDER BY created_at,id LIMIT $2`, cutoff, batch)
		}
		if err != nil {
			return nil, p.ErrUnavailable
		}
		fetched := 0
		for rows.Next() {
			fetched++
			var raw []byte
			if err := rows.Scan(&cursor.CreatedAt, &cursor.ID, &raw); err != nil {
				rows.Close()
				return nil, p.ErrUnavailable
			}
			var item p.Closure
			if json.Unmarshal(raw, &item) != nil || item.ID != cursor.ID || !p.ValidClosureProgress(item) {
				if automatic {
					out = append(out, dueClosureRow{cursor: cursor})
					continue
				}
				rows.Close()
				return nil, p.ErrUnavailable
			}
			if item.State == "cooling" && now.Before(item.CoolingEndsAt) {
				continue
			}
			out = append(out, dueClosureRow{cursor: cursor, closure: item})
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
