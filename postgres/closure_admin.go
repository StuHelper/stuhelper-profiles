package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"strings"
	"time"
)

func (r *Repository) lookupClosureCursor(ctx context.Context, after string) (time.Time, error) {
	var createdAt time.Time
	e := r.DB.QueryRowContext(ctx, `SELECT created_at FROM profiles.closures WHERE id=$1`, after).Scan(&createdAt)
	if errors.Is(e, sql.ErrNoRows) {
		return time.Time{}, p.ErrInput
	}
	if e != nil {
		return time.Time{}, p.ErrUnavailable
	}
	return createdAt, nil
}

func (r *Repository) QueryClosures(ctx context.Context, filter p.ClosureFilter, now time.Time) (p.ClosurePage, error) {
	now = now.UTC()
	hasCursor := filter.After != ""
	var cursorAt time.Time
	cursorID := filter.After
	if hasCursor {
		var err error
		cursorAt, err = r.lookupClosureCursor(ctx, cursorID)
		if err != nil {
			return p.ClosurePage{}, err
		}
	}

	target := filter.Limit + 1
	items := make([]p.Closure, 0, target)
	for len(items) < target {
		conds := []string{}
		args := []any{}
		nextArg := func(v any) string {
			args = append(args, v)
			return fmt.Sprintf("$%d", len(args))
		}
		if hasCursor {
			pAt := nextArg(cursorAt)
			pID := nextArg(cursorID)
			conds = append(conds, fmt.Sprintf("(created_at,id)>(%s,%s)", pAt, pID))
		}
		if filter.State != "" {
			conds = append(conds, fmt.Sprintf("state=%s", nextArg(filter.State)))
		}
		if filter.Blocker != "" {
			conds = append(conds, fmt.Sprintf("COALESCE(result->>'blocker','')=%s", nextArg(filter.Blocker)))
		}
		if filter.Actionable {
			// Include 1ms slack on PostgreSQL microsecond created_at, then verify exact CoolingEndsAt in Go.
			cutoff := now.Add(-7*24*time.Hour + time.Millisecond)
			conds = append(conds, fmt.Sprintf("(state IN('awaiting_assets','processing') OR (state='cooling' AND created_at<=%s))", nextArg(cutoff)))
		}
		batchSize := target - len(items)
		query := `SELECT created_at,id,issuer,subject,result FROM profiles.closures`
		if len(conds) > 0 {
			query += ` WHERE ` + strings.Join(conds, ` AND `)
		}
		query += fmt.Sprintf(` ORDER BY created_at ASC,id ASC LIMIT %s`, nextArg(batchSize))

		rows, err := r.DB.QueryContext(ctx, query, args...)
		if err != nil {
			return p.ClosurePage{}, p.ErrUnavailable
		}
		fetched := 0
		for rows.Next() {
			fetched++
			var rowAt time.Time
			var rowID string
			var v p.ClosureRecord
			var raw []byte
			if err := rows.Scan(&rowAt, &rowID, &v.Owner.Issuer, &v.Owner.Subject, &raw); err != nil {
				rows.Close()
				return p.ClosurePage{}, p.ErrUnavailable
			}
			v, err = decodeClosureRow(v.Owner, raw)
			if err != nil {
				rows.Close()
				return p.ClosurePage{}, err
			}
			cursorAt = rowAt
			cursorID = rowID
			hasCursor = true
			if filter.Actionable && v.Closure.State == "cooling" && now.Before(v.Closure.CoolingEndsAt) {
				continue
			}
			items = append(items, v.Closure)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return p.ClosurePage{}, p.ErrUnavailable
		}
		rows.Close()
		if fetched < batchSize {
			break
		}
	}

	out := p.ClosurePage{Items: items}
	if len(items) > filter.Limit {
		out.Items = items[:filter.Limit]
		out.NextCursor = out.Items[len(out.Items)-1].ID
	}
	return out, nil
}

func parseStoredClosureAction(stored string) (action, blocker string) {
	idx := strings.IndexByte(stored, ':')
	if idx < 0 {
		return stored, ""
	}
	return stored[:idx], stored[idx+1:]
}

func (r *Repository) QueryClosureEvents(ctx context.Context, closureID string, hasCursor bool, cursorAt time.Time, cursorID int64, limit int) (p.ClosureEventPage, error) {
	conds := []string{}
	args := []any{}
	nextArg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if closureID != "" {
		conds = append(conds, fmt.Sprintf("e.closure_id=%s", nextArg(closureID)))
	}
	if hasCursor {
		pAt := nextArg(cursorAt)
		pID := nextArg(cursorID)
		conds = append(conds, fmt.Sprintf("(e.created_at,e.id)<(%s,%s)", pAt, pID))
	}
	query := `SELECT e.id,e.closure_id,COALESCE(c.result->>'profile_id',''),e.action,e.actor_issuer,e.actor_subject,e.created_at FROM profiles.closure_events e LEFT JOIN profiles.closures c ON c.id=e.closure_id`
	if len(conds) > 0 {
		query += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	query += fmt.Sprintf(` ORDER BY e.created_at DESC,e.id DESC LIMIT %s`, nextArg(limit+1))

	rows, err := r.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return p.ClosureEventPage{}, p.ErrUnavailable
	}
	defer rows.Close()
	items := make([]p.ClosureAuditEvent, 0, limit+1)
	for rows.Next() {
		var item p.ClosureAuditEvent
		var rawAction string
		if err := rows.Scan(&item.ID, &item.ClosureID, &item.ProfileID, &rawAction, &item.Actor.Issuer, &item.Actor.Subject, &item.CreatedAt); err != nil {
			return p.ClosureEventPage{}, p.ErrUnavailable
		}
		item.Action, item.Blocker = parseStoredClosureAction(rawAction)
		item.CreatedAt = item.CreatedAt.UTC()
		items = append(items, item)
	}
	if rows.Err() != nil {
		return p.ClosureEventPage{}, p.ErrUnavailable
	}
	out := p.ClosureEventPage{Items: items}
	if len(items) > limit {
		out.Items = items[:limit]
		last := out.Items[len(out.Items)-1]
		out.NextCursor = p.EncodeClosureEventCursor(last.CreatedAt, last.ID)
	}
	return out, nil
}
