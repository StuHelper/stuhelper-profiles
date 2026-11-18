package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	p "github.com/StuHelper/stuhelper-profiles"
	"time"
)

const avatarSchema = `
ALTER TABLE profiles.profiles ADD COLUMN IF NOT EXISTS avatar_version_id text CHECK(avatar_version_id IS NULL OR (char_length(avatar_version_id)>=1 AND char_length(avatar_version_id)<=128));
CREATE TABLE IF NOT EXISTS profiles.avatar_releases(issuer text NOT NULL,subject text NOT NULL,profile_id text NOT NULL,version_id text NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(issuer,subject,version_id));
`

func (r *Repository) ReplayUpdate(ctx context.Context, who p.Principal, key string, in p.Update, now time.Time) (p.Profile, bool, error) {
	var previous string
	var saved []byte
	e := r.avatarDB(ctx).QueryRowContext(ctx, `SELECT digest,result FROM profiles.requests WHERE issuer=$1 AND subject=$2 AND key=$3 AND created_at >= $4`, who.Issuer, who.Subject, key, now.Add(-24*time.Hour)).Scan(&previous, &saved)
	if errors.Is(e, sql.ErrNoRows) {
		return p.Profile{}, false, nil
	}
	if e != nil {
		return p.Profile{}, false, p.ErrUnavailable
	}
	current, ce := r.Get(ctx, who)
	if ce != nil {
		return current, true, ce
	}
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	if previous != hex.EncodeToString(sum[:]) {
		return current, true, p.ErrConflict
	}
	var out p.Profile
	if json.Unmarshal(saved, &out) != nil {
		return out, true, p.ErrUnavailable
	}
	return out, true, nil
}

func (r *Repository) PendingAvatarReleases(ctx context.Context, who p.Principal) ([]p.AvatarRelease, error) {
	rows, e := r.avatarDB(ctx).QueryContext(ctx, `SELECT a.profile_id,a.version_id FROM profiles.avatar_releases a
 WHERE a.issuer=$1 AND a.subject=$2 AND NOT EXISTS (
  SELECT 1 FROM profiles.profiles v WHERE v.issuer=a.issuer AND v.subject=a.subject AND v.avatar_version_id=a.version_id
 ) ORDER BY a.created_at,a.version_id LIMIT 101`, who.Issuer, who.Subject)
	if e != nil {
		return nil, p.ErrUnavailable
	}
	defer rows.Close()
	var out []p.AvatarRelease
	for rows.Next() {
		var item p.AvatarRelease
		if e := rows.Scan(&item.ProfileID, &item.VersionID); e != nil {
			return nil, p.ErrUnavailable
		}
		out = append(out, item)
	}
	if rows.Err() != nil {
		return nil, p.ErrUnavailable
	}
	return out, nil
}

func (r *Repository) ClosureAvatarReleases(ctx context.Context, who p.Principal) ([]p.AvatarRelease, error) {
	pending, e := r.PendingAvatarReleases(ctx, who)
	if e != nil {
		return nil, e
	}
	var profileID, active string
	e = r.avatarDB(ctx).QueryRowContext(ctx, `SELECT public_id,COALESCE(avatar_version_id,'') FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject).Scan(&profileID, &active)
	if errors.Is(e, sql.ErrNoRows) {
		return pending, nil
	}
	if e != nil {
		return nil, p.ErrUnavailable
	}
	if active != "" {
		seen := false
		for _, item := range pending {
			if item.VersionID == active {
				seen = true
				break
			}
		}
		if !seen {
			pending = append(pending, p.AvatarRelease{ProfileID: profileID, VersionID: active})
		}
	}
	return pending, nil
}

func (r *Repository) ConfirmAvatarReleased(ctx context.Context, who p.Principal, versionID string) error {
	if _, e := r.avatarDB(ctx).ExecContext(ctx, `DELETE FROM profiles.avatar_releases WHERE issuer=$1 AND subject=$2 AND version_id=$3`, who.Issuer, who.Subject, versionID); e != nil {
		return p.ErrUnavailable
	}
	return nil
}

func (r *Repository) QueueAvatarRelease(ctx context.Context, who p.Principal, profileID, versionID string) (bool, error) {
	result, err := r.avatarDB(ctx).ExecContext(ctx, `INSERT INTO profiles.avatar_releases(issuer,subject,profile_id,version_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, who.Issuer, who.Subject, profileID, versionID)
	if err != nil {
		return false, p.ErrUnavailable
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, p.ErrUnavailable
	}
	return n != 0, nil
}
