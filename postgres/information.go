package postgres

import (
	"context"
	p "github.com/StuHelper/stuhelper-profiles"
	"time"
)

// InformationChangedAt uses the module's committed profile event timestamp.
func (r *Repository) InformationChangedAt(ctx context.Context, target p.Principal) (time.Time, error) {
	var at time.Time
	e := r.DB.QueryRowContext(ctx, `SELECT max(created_at) FROM profiles.events WHERE issuer=$1 AND subject=$2`, target.Issuer, target.Subject).Scan(&at)
	if e != nil {
		return time.Time{}, p.ErrUnavailable
	}
	return at, nil
}
