package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	p "github.com/StuHelper/stuhelper-profiles"
)

const avatarSchema = `
ALTER TABLE profiles.profiles ADD COLUMN IF NOT EXISTS avatar_version_id text CHECK(avatar_version_id IS NULL OR (char_length(avatar_version_id)>=1 AND char_length(avatar_version_id)<=128));
CREATE TABLE IF NOT EXISTS profiles.avatar_releases(issuer text NOT NULL,subject text NOT NULL,profile_id text NOT NULL,version_id text NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),PRIMARY KEY(issuer,subject,version_id));
`

func (r *Repository) ReplayUpdate(ctx context.Context, who p.Principal, key string, in p.Update) (p.Profile, bool, error) {
	var previous string
	var saved []byte
	e := r.DB.QueryRowContext(ctx, `SELECT digest,result FROM profiles.requests WHERE issuer=$1 AND subject=$2 AND key=$3`, who.Issuer, who.Subject, key).Scan(&previous, &saved)
	if errors.Is(e, sql.ErrNoRows) {
		return p.Profile{}, false, nil
	}
	if e != nil {
		return p.Profile{}, false, p.ErrUnavailable
	}
	var state string
	current, ce := scan(r.DB.QueryRowContext(ctx, `SELECT `+columns+` FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject))
	if ce != nil {
		return current, true, ce
	}
	if e = r.DB.QueryRowContext(ctx, `SELECT account_state FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject).Scan(&state); e != nil {
		return current, true, p.ErrUnavailable
	}
	if state != "active" {
		return current, true, p.ErrClosed
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
	rows, e := r.DB.QueryContext(ctx, `SELECT profile_id,version_id FROM profiles.avatar_releases WHERE issuer=$1 AND subject=$2 ORDER BY created_at ASC,version_id ASC`, who.Issuer, who.Subject)
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
	e = r.DB.QueryRowContext(ctx, `SELECT public_id,COALESCE(avatar_version_id,'') FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject).Scan(&profileID, &active)
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
	if _, e := r.DB.ExecContext(ctx, `DELETE FROM profiles.avatar_releases WHERE issuer=$1 AND subject=$2 AND version_id=$3`, who.Issuer, who.Subject, versionID); e != nil {
		return p.ErrUnavailable
	}
	return nil
}
