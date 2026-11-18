package postgres

import (
	"context"
	"encoding/json"
	p "github.com/StuHelper/stuhelper-profiles"
	"time"
)

func (r *Repository) DueClosures(ctx context.Context, now time.Time, limit int) ([]p.Closure, error) {
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

	for len(out) < limit {
		batch := limit - len(out)
		var (
			rows interface {
				Next() bool
				Scan(...any) error
				Err() error
				Close() error
			}
			err error
		)
		if hasCursor {
			rows, err = r.DB.QueryContext(ctx, `SELECT created_at,id,result FROM profiles.closures WHERE (created_at,id)>($1,$2) AND (state IN('awaiting_assets','processing') OR (state='cooling' AND created_at<=$3)) ORDER BY created_at ASC,id ASC LIMIT $4`, cursorAt, cursorID, cutoff, batch)
		} else {
			rows, err = r.DB.QueryContext(ctx, `SELECT created_at,id,result FROM profiles.closures WHERE state IN('awaiting_assets','processing') OR (state='cooling' AND created_at<=$1) ORDER BY created_at ASC,id ASC LIMIT $2`, cutoff, batch)
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
