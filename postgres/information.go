package postgres

import (
	"context"
	"database/sql"
	"errors"
	p "github.com/StuHelper/stuhelper-profiles"
	"time"
)

// InformationChangedAt returns the timestamp of the latest committed profile event, or zero when none exist.
func (r *Repository) InformationChangedAt(ctx context.Context, target p.Principal) (time.Time, error) {
	var at time.Time
	e := r.DB.QueryRowContext(ctx, `SELECT created_at FROM profiles.events WHERE issuer=$1 AND subject=$2 ORDER BY id DESC LIMIT 1`, target.Issuer, target.Subject).Scan(&at)
	if errors.Is(e, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if e != nil {
		return time.Time{}, p.ErrUnavailable
	}
	return at.UTC(), nil
}

// InformationProfile reads a profile and its current account_state for authorized administrative queries, including closed accounts.
func (r *Repository) InformationProfile(ctx context.Context, target p.Principal) (p.Profile, string, time.Time, error) {
	v, state, e := scanWithState(r.DB.QueryRowContext(ctx, `SELECT account_state,`+columns+` FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, target.Issuer, target.Subject))
	if e != nil {
		return v, "", time.Time{}, e
	}
	changedAt, e := r.InformationChangedAt(ctx, target)
	if e != nil {
		return v, "", time.Time{}, e
	}
	return v, state, changedAt, nil
}
