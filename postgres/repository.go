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

const Schema = `CREATE SCHEMA IF NOT EXISTS profiles;
CREATE TABLE IF NOT EXISTS profiles.profiles(issuer text NOT NULL,subject text NOT NULL,nickname text NOT NULL,revision bigint NOT NULL CHECK(revision>0),PRIMARY KEY(issuer,subject));
CREATE TABLE IF NOT EXISTS profiles.requests(issuer text NOT NULL,subject text NOT NULL,key text NOT NULL,digest text NOT NULL,result jsonb NOT NULL,PRIMARY KEY(issuer,subject,key));
CREATE TABLE IF NOT EXISTS profiles.events(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,issuer text NOT NULL,subject text NOT NULL,revision bigint NOT NULL,kind text NOT NULL,created_at timestamptz NOT NULL DEFAULT now());
ALTER TABLE profiles.profiles ADD COLUMN IF NOT EXISTS public_id text NOT NULL DEFAULT gen_random_uuid()::text;
ALTER TABLE profiles.profiles ADD COLUMN IF NOT EXISTS bio text NOT NULL DEFAULT '';
ALTER TABLE profiles.profiles ADD COLUMN IF NOT EXISTS visibility jsonb NOT NULL DEFAULT '{"school":true,"badge":true,"department":true,"grade":false,"reviews":true,"resources":true}';
CREATE UNIQUE INDEX IF NOT EXISTS profiles_public_id ON profiles.profiles(public_id);
UPDATE profiles.requests q SET result=q.result||jsonb_build_object('id',p.public_id,'bio','','nickname_required',false,'visibility','{"school":true,"badge":true,"department":true,"grade":false,"reviews":true,"resources":true}'::jsonb)
FROM profiles.profiles p WHERE q.issuer=p.issuer AND q.subject=p.subject AND NOT(q.result ? 'id');
DO $migration$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='profiles_nickname_length' AND conrelid='profiles.profiles'::regclass) THEN
 ALTER TABLE profiles.profiles ADD CONSTRAINT profiles_nickname_length CHECK(char_length(nickname)<=20);
 END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='profiles_bio_length' AND conrelid='profiles.profiles'::regclass) THEN
 ALTER TABLE profiles.profiles ADD CONSTRAINT profiles_bio_length CHECK(char_length(bio)<=120);
 END IF;
END $migration$;`

type Repository struct{ DB *sql.DB }

func Migrate(ctx context.Context, db *sql.DB) error {
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(726391840107)`); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, Schema+closureSchema); e != nil {
		return e
	}
	return tx.Commit()
}
func (r *Repository) Ready(ctx context.Context) error { return r.DB.PingContext(ctx) }
func (r *Repository) CheckRuntime(ctx context.Context) error {
	var unsafe bool
	e := r.DB.QueryRowContext(ctx, `SELECT rolsuper OR rolcreaterole OR rolcreatedb OR has_schema_privilege(current_user,'profiles','CREATE') FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe)
	if e != nil || unsafe {
		return p.ErrUnavailable
	}
	return r.Ready(ctx)
}

type scanner interface{ Scan(...any) error }

func scan(row scanner) (p.Profile, error) {
	var v p.Profile
	var raw []byte
	e := row.Scan(&v.ID, &v.Nickname, &v.Revision, &v.Bio, &raw)
	if errors.Is(e, sql.ErrNoRows) {
		return v, p.ErrNotFound
	}
	if e != nil {
		return v, p.ErrUnavailable
	}
	if json.Unmarshal(raw, &v.Visibility) != nil {
		return v, p.ErrUnavailable
	}
	v.NicknameRequired = v.Nickname == ""
	return v, nil
}

const columns = "public_id,nickname,revision,bio,visibility"

func (r *Repository) Get(ctx context.Context, who p.Principal) (p.Profile, error) {
	return scan(r.DB.QueryRowContext(ctx, `SELECT `+columns+` FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject))
}
func (r *Repository) Public(ctx context.Context, id string) (p.PublicRecord, error) {
	var out p.PublicRecord
	var raw []byte
	v := &out.Profile
	e := r.DB.QueryRowContext(ctx, `SELECT issuer,subject,`+columns+` FROM profiles.profiles WHERE public_id=$1 AND nickname<>'' AND account_state='active'`, id).Scan(&out.Owner.Issuer, &out.Owner.Subject, &v.ID, &v.Nickname, &v.Revision, &v.Bio, &raw)
	if errors.Is(e, sql.ErrNoRows) {
		return out, p.ErrNotFound
	}
	if e != nil || json.Unmarshal(raw, &v.Visibility) != nil {
		return out, p.ErrUnavailable
	}
	return out, nil
}
func (r *Repository) Ensure(ctx context.Context, who p.Principal, name string) (p.Profile, error) {
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return p.Profile{}, p.ErrUnavailable
	}
	defer tx.Rollback()
	result, e := tx.ExecContext(ctx, `INSERT INTO profiles.profiles(issuer,subject,nickname,revision) VALUES($1,$2,$3,1) ON CONFLICT(issuer,subject) DO NOTHING`, who.Issuer, who.Subject, name)
	if e != nil {
		return p.Profile{}, p.ErrUnavailable
	}
	n, e := result.RowsAffected()
	if e != nil {
		return p.Profile{}, p.ErrUnavailable
	}
	if n > 0 {
		if _, e = tx.ExecContext(ctx, `INSERT INTO profiles.events(issuer,subject,revision,kind) VALUES($1,$2,1,'created')`, who.Issuer, who.Subject); e != nil {
			return p.Profile{}, p.ErrUnavailable
		}
	}
	var state string
	if e = tx.QueryRowContext(ctx, `SELECT account_state FROM profiles.profiles WHERE issuer=$1 AND subject=$2 FOR UPDATE`, who.Issuer, who.Subject).Scan(&state); e != nil {
		return p.Profile{}, p.ErrUnavailable
	}
	if state != "active" {
		return p.Profile{}, p.ErrClosed
	}
	out, e := scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject))
	if e != nil {
		return out, e
	}
	if e = tx.Commit(); e != nil {
		return p.Profile{}, p.ErrUnavailable
	}
	return out, nil
}
func (r *Repository) Update(ctx context.Context, who p.Principal, key string, in p.Update) (p.Profile, error) {
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return p.Profile{}, p.ErrUnavailable
	}
	defer tx.Rollback()
	current, e := scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM profiles.profiles WHERE issuer=$1 AND subject=$2 FOR UPDATE`, who.Issuer, who.Subject))
	if e != nil {
		return current, e
	}
	var state string
	if e = tx.QueryRowContext(ctx, `SELECT account_state FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject).Scan(&state); e != nil {
		return current, p.ErrUnavailable
	}
	if state != "active" {
		return current, p.ErrClosed
	}
	// Preserve the original nickname-only digest for unknown-result retries from old clients.
	var b []byte
	if in.Bio == nil && in.Visibility == nil {
		b, _ = json.Marshal(struct {
			Revision int64
			Name     string
		}{in.Revision, in.Nickname})
	} else {
		b, _ = json.Marshal(in)
	}
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	var previous string
	var saved []byte
	e = tx.QueryRowContext(ctx, `SELECT digest,result FROM profiles.requests WHERE issuer=$1 AND subject=$2 AND key=$3`, who.Issuer, who.Subject, key).Scan(&previous, &saved)
	if e == nil {
		if previous != digest {
			return current, p.ErrConflict
		}
		var out p.Profile
		if json.Unmarshal(saved, &out) != nil {
			return out, p.ErrUnavailable
		}
		return out, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return current, p.ErrUnavailable
	}
	if current.Revision != in.Revision {
		return current, p.ErrConflict
	}
	out := current
	out.Nickname = in.Nickname
	out.NicknameRequired = false
	out.Revision++
	if in.Bio != nil {
		out.Bio = *in.Bio
	}
	if in.Visibility != nil {
		out.Visibility = *in.Visibility
	}
	visibility, _ := json.Marshal(out.Visibility)
	saved, _ = json.Marshal(out)
	if _, e = tx.ExecContext(ctx, `UPDATE profiles.profiles SET nickname=$3,revision=$4,bio=$5,visibility=$6 WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject, out.Nickname, out.Revision, out.Bio, visibility); e != nil {
		return out, p.ErrUnavailable
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO profiles.requests VALUES($1,$2,$3,$4,$5)`, who.Issuer, who.Subject, key, digest, saved); e != nil {
		return out, p.ErrUnavailable
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO profiles.events(issuer,subject,revision,kind) VALUES($1,$2,$3,'updated')`, who.Issuer, who.Subject, out.Revision); e != nil {
		return out, p.ErrUnavailable
	}
	if e = tx.Commit(); e != nil {
		return p.Profile{}, p.ErrUnavailable
	}
	return out, nil
}
