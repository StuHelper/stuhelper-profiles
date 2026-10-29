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
CREATE TABLE IF NOT EXISTS profiles.events(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,issuer text NOT NULL,subject text NOT NULL,revision bigint NOT NULL,kind text NOT NULL,created_at timestamptz NOT NULL DEFAULT now());`

type Repository struct{ DB *sql.DB }

func Migrate(ctx context.Context, db *sql.DB) error   { _, e := db.ExecContext(ctx, Schema); return e }
func (r *Repository) Ready(ctx context.Context) error { return r.DB.PingContext(ctx) }
func (r *Repository) CheckRuntime(ctx context.Context) error {
	var unsafe bool
	e := r.DB.QueryRowContext(ctx, `SELECT rolsuper OR rolcreaterole OR rolcreatedb OR has_schema_privilege(current_user,'profiles','CREATE') FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe)
	if e != nil || unsafe {
		return p.ErrUnavailable
	}
	return r.Ready(ctx)
}
func (r *Repository) Get(ctx context.Context, who p.Principal) (p.Profile, error) {
	var out p.Profile
	e := r.DB.QueryRowContext(ctx, `SELECT nickname,revision FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject).Scan(&out.Nickname, &out.Revision)
	if errors.Is(e, sql.ErrNoRows) {
		return out, p.ErrNotFound
	}
	return out, e
}
func (r *Repository) Ensure(ctx context.Context, who p.Principal, name string) (p.Profile, error) {
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return p.Profile{}, e
	}
	defer tx.Rollback()
	result, e := tx.ExecContext(ctx, `INSERT INTO profiles.profiles VALUES($1,$2,$3,1) ON CONFLICT DO NOTHING`, who.Issuer, who.Subject, name)
	if e != nil {
		return p.Profile{}, e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return p.Profile{}, e
	}
	if n > 0 {
		if _, e = tx.ExecContext(ctx, `INSERT INTO profiles.events(issuer,subject,revision,kind) VALUES($1,$2,1,'created')`, who.Issuer, who.Subject); e != nil {
			return p.Profile{}, e
		}
	}
	var out p.Profile
	e = tx.QueryRowContext(ctx, `SELECT nickname,revision FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject).Scan(&out.Nickname, &out.Revision)
	if e != nil {
		return out, e
	}
	return out, tx.Commit()
}
func (r *Repository) Save(ctx context.Context, who p.Principal, key string, revision int64, name string) (p.Profile, error) {
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return p.Profile{}, e
	}
	defer tx.Rollback()
	var current p.Profile
	e = tx.QueryRowContext(ctx, `SELECT nickname,revision FROM profiles.profiles WHERE issuer=$1 AND subject=$2 FOR UPDATE`, who.Issuer, who.Subject).Scan(&current.Nickname, &current.Revision)
	if errors.Is(e, sql.ErrNoRows) {
		return current, p.ErrNotFound
	}
	if e != nil {
		return current, e
	}
	b, _ := json.Marshal(struct {
		Revision int64
		Name     string
	}{revision, name})
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
		return current, e
	}
	if current.Revision != revision {
		return current, p.ErrConflict
	}
	out := p.Profile{Nickname: name, Revision: revision + 1}
	saved, _ = json.Marshal(out)
	if _, e = tx.ExecContext(ctx, `UPDATE profiles.profiles SET nickname=$3,revision=$4 WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject, name, out.Revision); e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO profiles.requests VALUES($1,$2,$3,$4,$5)`, who.Issuer, who.Subject, key, digest, saved); e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO profiles.events(issuer,subject,revision,kind) VALUES($1,$2,$3,'updated')`, who.Issuer, who.Subject, out.Revision); e != nil {
		return out, e
	}
	return out, tx.Commit()
}
