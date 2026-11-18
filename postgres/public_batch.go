package postgres

import (
	"context"
	"encoding/json"
	p "github.com/StuHelper/stuhelper-profiles"
)

func scanPublicRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]p.PublicRecord, error) {
	var out []p.PublicRecord
	for rows.Next() {
		var rec p.PublicRecord
		var raw, consent []byte
		v := &rec.Profile
		if e := rows.Scan(&rec.Owner.Issuer, &rec.Owner.Subject, &v.ID, &v.Nickname, &v.Revision, &v.Bio, &raw, &consent, &v.AvatarVersionID); e != nil {
			return nil, p.ErrUnavailable
		}
		if json.Unmarshal(raw, &v.Visibility) != nil || json.Unmarshal(consent, &v.GroupCardConsent) != nil {
			return nil, p.ErrUnavailable
		}
		v.NicknameRequired = v.Nickname == ""
		out = append(out, rec)
	}
	if rows.Err() != nil {
		return nil, p.ErrUnavailable
	}
	return out, nil
}

func (r *Repository) PublicByPrincipals(ctx context.Context, owners []p.Principal) ([]p.PublicRecord, error) {
	if len(owners) == 0 {
		return []p.PublicRecord{}, nil
	}
	issuers := make([]string, len(owners))
	subjects := make([]string, len(owners))
	for i, o := range owners {
		issuers[i] = o.Issuer
		subjects[i] = o.Subject
	}
	rows, e := r.DB.QueryContext(ctx, `SELECT p.issuer,p.subject,`+columns+` FROM unnest($1::text[],$2::text[]) AS u(issuer,subject) JOIN profiles.profiles p USING(issuer,subject) WHERE p.nickname<>'' AND p.account_state='active'`, issuers, subjects)
	if e != nil {
		return nil, p.ErrUnavailable
	}
	defer rows.Close()
	return scanPublicRows(rows)
}

func (r *Repository) PublicByIDs(ctx context.Context, ids []string) ([]p.PublicRecord, error) {
	if len(ids) == 0 {
		return []p.PublicRecord{}, nil
	}
	rows, e := r.DB.QueryContext(ctx, `SELECT issuer,subject,`+columns+` FROM profiles.profiles WHERE public_id=ANY($1::text[]) AND nickname<>'' AND account_state='active'`, ids)
	if e != nil {
		return nil, p.ErrUnavailable
	}
	defer rows.Close()
	return scanPublicRows(rows)
}
