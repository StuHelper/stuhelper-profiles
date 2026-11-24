package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	p "github.com/StuHelper/stuhelper-profiles"
)

const coolingSchema = `
CREATE TABLE IF NOT EXISTS profiles.cooling_intents(
 closure_id text PRIMARY KEY, issuer text NOT NULL, subject text NOT NULL,
 request_key text NOT NULL, cancelled boolean NOT NULL DEFAULT false,
 retry_at timestamptz NOT NULL DEFAULT statement_timestamp()
);
CREATE UNIQUE INDEX IF NOT EXISTS cooling_preparation_owner ON profiles.cooling_intents(issuer,subject) WHERE NOT cancelled;
CREATE INDEX IF NOT EXISTS cooling_intent_due ON profiles.cooling_intents(retry_at,closure_id);
`

func (r *Repository) PrepareCooling(ctx context.Context, owner p.Principal, key string, now time.Time) (p.CoolingIntent, error) {
	item := p.CoolingIntent{Owner: owner, Key: key}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return item, p.ErrUnavailable
	}
	defer tx.Rollback()
	actor, _ := json.Marshal(owner)
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,734125))`, string(actor)); err != nil {
		return item, p.ErrUnavailable
	}
	if _, err = tx.ExecContext(ctx, `SELECT profiles.expire_closure_request($1,$2,$3,$4)`, owner.Issuer, owner.Subject, key, now); err != nil {
		return item, p.ErrUnavailable
	}
	var prior bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM profiles.closure_requests WHERE actor_issuer=$1 AND actor_subject=$2 AND key=$3)`, owner.Issuer, owner.Subject, key).Scan(&prior)
	if err != nil {
		return item, p.ErrUnavailable
	}
	if prior {
		return item, nil
	}
	var state string
	err = tx.QueryRowContext(ctx, `SELECT account_state FROM profiles.profiles WHERE issuer=$1 AND subject=$2 FOR UPDATE`, owner.Issuer, owner.Subject).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return item, p.ErrNotFound
	}
	if err != nil {
		return item, p.ErrUnavailable
	}
	if state != "active" {
		return item, p.ErrClosed
	}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM profiles.closures WHERE issuer=$1 AND subject=$2 AND state<>'cancelled')`, owner.Issuer, owner.Subject).Scan(&prior); err != nil {
		return item, p.ErrUnavailable
	}
	if prior {
		return item, p.ErrConflict
	}
	var stored string
	err = tx.QueryRowContext(ctx, `INSERT INTO profiles.cooling_intents(closure_id,issuer,subject,request_key) VALUES(gen_random_uuid()::text,$1,$2,$3)
      ON CONFLICT(issuer,subject) WHERE NOT cancelled DO UPDATE SET issuer=excluded.issuer RETURNING closure_id,request_key`, owner.Issuer, owner.Subject, key).Scan(&item.ClosureID, &stored)
	if err != nil {
		return item, p.ErrUnavailable
	}
	if stored != key {
		return item, p.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return item, p.ErrUnavailable
	}
	return item, nil
}

func (r *Repository) PendingCooling(ctx context.Context, limit int) ([]p.CoolingIntent, error) {
	rows, err := r.DB.QueryContext(ctx, `WITH due AS (SELECT closure_id FROM profiles.cooling_intents WHERE retry_at<=statement_timestamp() ORDER BY retry_at,closure_id LIMIT $1 FOR UPDATE SKIP LOCKED)
 UPDATE profiles.cooling_intents i SET retry_at=statement_timestamp()+interval '1 minute' FROM due WHERE i.closure_id=due.closure_id RETURNING i.issuer,i.subject,i.closure_id,i.request_key,i.cancelled`, limit)
	if err != nil {
		return nil, p.ErrUnavailable
	}
	defer rows.Close()
	var items []p.CoolingIntent
	for rows.Next() {
		var item p.CoolingIntent
		if err := rows.Scan(&item.Owner.Issuer, &item.Owner.Subject, &item.ClosureID, &item.Key, &item.Cancelled); err != nil {
			return nil, p.ErrUnavailable
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, p.ErrUnavailable
	}
	return items, nil
}
func (r *Repository) AcknowledgeCooling(ctx context.Context, item p.CoolingIntent) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM profiles.cooling_intents WHERE closure_id=$1 AND issuer=$2 AND subject=$3 AND cancelled`, item.ClosureID, item.Owner.Issuer, item.Owner.Subject)
	if err != nil {
		return p.ErrUnavailable
	}
	return nil
}
