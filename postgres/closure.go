package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	p "github.com/StuHelper/stuhelper-profiles"
	"slices"
	"time"
)

const closureSchema = `
CREATE TABLE IF NOT EXISTS profiles.closures(id text PRIMARY KEY,issuer text NOT NULL,subject text NOT NULL,state text NOT NULL CHECK(state IN('cooling','cancelled','awaiting_assets','processing','closed')),result jsonb NOT NULL,created_at timestamptz NOT NULL);
ALTER TABLE profiles.closures ADD COLUMN IF NOT EXISTS processing_until timestamptz;
CREATE UNIQUE INDEX IF NOT EXISTS profiles_active_closure ON profiles.closures(issuer,subject) WHERE state<>'cancelled';
CREATE INDEX IF NOT EXISTS profiles_closures_owner_created_idx ON profiles.closures(issuer,subject,created_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS profiles_closures_created_id_idx ON profiles.closures(created_at ASC,id ASC);
CREATE INDEX IF NOT EXISTS profiles_closures_state_created_id_idx ON profiles.closures(state,created_at ASC,id ASC);
CREATE INDEX IF NOT EXISTS profiles_closures_due_idx ON profiles.closures(created_at ASC,id ASC) WHERE state IN('cooling','awaiting_assets','processing');
CREATE TABLE IF NOT EXISTS profiles.closure_requests(actor_issuer text NOT NULL,actor_subject text NOT NULL,key text NOT NULL,action text NOT NULL,target text NOT NULL,result jsonb NOT NULL,PRIMARY KEY(actor_issuer,actor_subject,key));
CREATE TABLE IF NOT EXISTS profiles.closure_events(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,closure_id text NOT NULL,action text NOT NULL,actor_issuer text NOT NULL,actor_subject text NOT NULL,created_at timestamptz NOT NULL);
CREATE INDEX IF NOT EXISTS profiles_closure_events_closure_idx ON profiles.closure_events(closure_id,id);
CREATE INDEX IF NOT EXISTS profiles_closure_events_created_id_idx ON profiles.closure_events(created_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS profiles_closure_events_closure_id_idx ON profiles.closure_events(closure_id,created_at DESC,id DESC);
`

func (r *Repository) AccountState(ctx context.Context, who p.Principal) (string, error) {
	var state string
	e := r.DB.QueryRowContext(ctx, `SELECT account_state FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject).Scan(&state)
	if errors.Is(e, sql.ErrNoRows) {
		return "", p.ErrNotFound
	}
	if e != nil {
		return "", p.ErrUnavailable
	}
	return state, nil
}
func decodeClosureRow(owner p.Principal, raw []byte) (p.ClosureRecord, error) {
	v := p.ClosureRecord{Owner: owner}
	if json.Unmarshal(raw, &v.Closure) != nil || !p.ValidClosureProgress(v.Closure) {
		return v, p.ErrUnavailable
	}
	return v, nil
}
func closureScan(row scanner) (p.ClosureRecord, error) {
	var owner p.Principal
	var raw []byte
	e := row.Scan(&owner.Issuer, &owner.Subject, &raw)
	if errors.Is(e, sql.ErrNoRows) {
		return p.ClosureRecord{}, p.ErrNotFound
	}
	if e != nil {
		return p.ClosureRecord{}, p.ErrUnavailable
	}
	return decodeClosureRow(owner, raw)
}
func (r *Repository) Closure(ctx context.Context, id string) (p.ClosureRecord, error) {
	return closureScan(r.DB.QueryRowContext(ctx, `SELECT issuer,subject,result FROM profiles.closures WHERE id=$1`, id))
}
func (r *Repository) CurrentClosure(ctx context.Context, who p.Principal) (p.Closure, error) {
	v, e := closureScan(r.DB.QueryRowContext(ctx, `SELECT issuer,subject,result FROM profiles.closures WHERE issuer=$1 AND subject=$2 ORDER BY created_at DESC,id DESC LIMIT 1`, who.Issuer, who.Subject))
	return v.Closure, e
}
func (r *Repository) Closures(ctx context.Context, after string, limit int) ([]p.Closure, error) {
	page, e := r.QueryClosures(ctx, p.ClosureFilter{After: after, Limit: limit}, time.Now().UTC())
	return page.Items, e
}

// AcquireClosureStep claims a short wall-clock lease on an active closure so concurrent callers serialize step RPCs without holding DB connections.
func (r *Repository) AcquireClosureStep(ctx context.Context, id string, ttl time.Duration) (p.ClosureRecord, bool, error) {
	ms := ttl.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	rec, err := closureScan(r.DB.QueryRowContext(ctx, `UPDATE profiles.closures SET processing_until=clock_timestamp()+($2*interval '1 millisecond') WHERE id=$1 AND state IN('awaiting_assets','processing') AND (processing_until IS NULL OR processing_until<=clock_timestamp()) RETURNING issuer,subject,result`, id, ms))
	if err == nil {
		return rec, true, nil
	}
	if !errors.Is(err, p.ErrNotFound) {
		return rec, false, err
	}
	rec, err = r.Closure(ctx, id)
	return rec, false, err
}

func (r *Repository) ReleaseClosureStep(ctx context.Context, id string) error {
	if _, err := r.DB.ExecContext(ctx, `UPDATE profiles.closures SET processing_until=NULL WHERE id=$1`, id); err != nil {
		return p.ErrUnavailable
	}
	return nil
}

func (r *Repository) ChangeClosure(ctx context.Context, actor p.Principal, key, action, id string, now time.Time) (p.Closure, error) {
	var out p.Closure
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return out, p.ErrUnavailable
	}
	defer tx.Rollback()
	// Serialize each actor's key namespace, independently of the target account.
	lockKey, _ := json.Marshal(actor)
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,734125))`, string(lockKey)); e != nil {
		return out, p.ErrUnavailable
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM profiles.closure_requests WHERE actor_issuer=$1 AND actor_subject=$2 AND key=$3 AND created_at<$4`, actor.Issuer, actor.Subject, key, now.Add(-24*time.Hour)); e != nil {
		return out, p.ErrUnavailable
	}
	var savedAction, target string
	var raw []byte
	e = tx.QueryRowContext(ctx, `SELECT action,target,result FROM profiles.closure_requests WHERE actor_issuer=$1 AND actor_subject=$2 AND key=$3`, actor.Issuer, actor.Subject, key).Scan(&savedAction, &target, &raw)
	if e == nil {
		if savedAction != action || target != id {
			return out, p.ErrConflict
		}
		if json.Unmarshal(raw, &out) != nil {
			return out, p.ErrUnavailable
		}
		v, e := closureScan(tx.QueryRowContext(ctx, `SELECT issuer,subject,result FROM profiles.closures WHERE id=$1`, out.ID))
		if e != nil {
			return out, e
		}
		if action != "approve" {
			var state string
			if e = tx.QueryRowContext(ctx, `SELECT account_state FROM profiles.profiles WHERE issuer=$1 AND subject=$2`, v.Owner.Issuer, v.Owner.Subject).Scan(&state); errors.Is(e, sql.ErrNoRows) {
				return out, p.ErrNotFound
			} else if e != nil {
				return out, p.ErrUnavailable
			}
			if state != "active" {
				return out, p.ErrClosed
			}
		}
		return v.Closure, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return out, p.ErrUnavailable
	}
	var who p.Principal
	if action == "request" {
		who = actor
	} else {
		if e = tx.QueryRowContext(ctx, `SELECT issuer,subject FROM profiles.closures WHERE id=$1`, id).Scan(&who.Issuer, &who.Subject); errors.Is(e, sql.ErrNoRows) {
			return out, p.ErrNotFound
		} else if e != nil {
			return out, p.ErrUnavailable
		}
		if action == "cancel" && who != actor {
			return out, p.ErrNotFound
		}
	}
	var state, profileID string
	e = tx.QueryRowContext(ctx, `SELECT account_state,public_id FROM profiles.profiles WHERE issuer=$1 AND subject=$2 FOR UPDATE`, who.Issuer, who.Subject).Scan(&state, &profileID)
	if errors.Is(e, sql.ErrNoRows) {
		return out, p.ErrNotFound
	}
	if e != nil {
		return out, p.ErrUnavailable
	}
	eventAction := action
	updateClosure := true
	if action == "request" {
		if state != "active" {
			return out, p.ErrClosed
		}
		var exists bool
		if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM profiles.closures WHERE issuer=$1 AND subject=$2 AND state<>'cancelled')`, who.Issuer, who.Subject).Scan(&exists); e != nil {
			return out, p.ErrUnavailable
		}
		if exists {
			return out, p.ErrConflict
		}
		if e = tx.QueryRowContext(ctx, `SELECT gen_random_uuid()::text`).Scan(&out.ID); e != nil {
			return out, p.ErrUnavailable
		}
		out.ProfileID = profileID
		out.State = "cooling"
		out.Revision = 1
		out.RequestedAt = now
		out.CoolingEndsAt = now.Add(7 * 24 * time.Hour)
		out.CompletedSteps = []string{}
		out.PointsPolicy = p.PointsPolicy
	} else {
		record, e := closureScan(tx.QueryRowContext(ctx, `SELECT issuer,subject,result FROM profiles.closures WHERE id=$1 FOR UPDATE`, id))
		if e != nil {
			return out, e
		}
		out = record.Closure
		switch action {
		case "cancel":
			if out.State != "cooling" || !now.Before(out.CoolingEndsAt) {
				return out, p.ErrConflict
			}
			out.State = "cancelled"
			out.Revision++
		case "approve":
			if out.State == "cancelled" || now.Before(out.CoolingEndsAt) {
				return out, p.ErrConflict
			}
			if out.State == "closed" {
				raw, _ = json.Marshal(out)
				if _, e = tx.ExecContext(ctx, `INSERT INTO profiles.closure_requests(actor_issuer,actor_subject,key,action,target,result,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, actor.Issuer, actor.Subject, key, action, id, raw, now); e != nil {
					return out, p.ErrUnavailable
				}
				if e = tx.Commit(); e != nil {
					return out, p.ErrUnavailable
				}
				return out, nil
			}
			if out.State == "cooling" {
				out.State = "awaiting_assets"
				out.Revision++
			} else {
				eventAction = "retry"
				updateClosure = false
			}
		default:
			return out, p.ErrInput
		}
	}
	raw, _ = json.Marshal(out)
	if action == "request" {
		_, e = tx.ExecContext(ctx, `INSERT INTO profiles.closures(id,issuer,subject,state,result,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, out.ID, who.Issuer, who.Subject, out.State, raw, now)
	} else if updateClosure {
		_, e = tx.ExecContext(ctx, `UPDATE profiles.closures SET state=$2,result=$3,updated_at=$4 WHERE id=$1`, id, out.State, raw, now)
	}
	if e != nil {
		return out, p.ErrUnavailable
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO profiles.closure_requests(actor_issuer,actor_subject,key,action,target,result,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, actor.Issuer, actor.Subject, key, action, id, raw, now); e != nil {
		return out, p.ErrUnavailable
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO profiles.closure_events(closure_id,action,actor_issuer,actor_subject,created_at) VALUES($1,$2,$3,$4,$5)`, out.ID, eventAction, actor.Issuer, actor.Subject, now); e != nil {
		return out, p.ErrUnavailable
	}
	if e = tx.Commit(); e != nil {
		return out, p.ErrUnavailable
	}
	return out, nil
}

func (r *Repository) AdvanceClosure(ctx context.Context, id string, step int, blocker string, skipped []string, now time.Time) (p.Closure, error) {
	return r.AdvanceClosureBy(ctx, p.Principal{Issuer: "urn:stuhelper:profiles-service", Subject: "closure"}, id, step, blocker, skipped, now)
}

func (r *Repository) AdvanceClosureBy(ctx context.Context, actor p.Principal, id string, step int, blocker string, skipped []string, now time.Time) (p.Closure, error) {
	return r.AdvanceClosureByWithReasons(ctx, actor, id, step, blocker, nil, skipped, now)
}

func (r *Repository) AdvanceClosureWithReasons(ctx context.Context, id string, step int, blocker string, pendingReasons, skipped []string, now time.Time) (p.Closure, error) {
	return r.AdvanceClosureByWithReasons(ctx, p.Principal{}, id, step, blocker, pendingReasons, skipped, now)
}

func (r *Repository) AdvanceClosureByWithReasons(ctx context.Context, actor p.Principal, id string, step int, blocker string, pendingReasons, skipped []string, now time.Time) (p.Closure, error) {
	if actor.Issuer == "" || actor.Subject == "" {
		actor = p.Principal{Issuer: "urn:stuhelper:profiles-service", Subject: "closure"}
	}
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return p.Closure{}, p.ErrUnavailable
	}
	defer tx.Rollback()
	// Same lock order as application mutations: account row, then closure row.
	var who p.Principal
	if e = tx.QueryRowContext(ctx, `SELECT issuer,subject FROM profiles.closures WHERE id=$1`, id).Scan(&who.Issuer, &who.Subject); errors.Is(e, sql.ErrNoRows) {
		return p.Closure{}, p.ErrNotFound
	} else if e != nil {
		return p.Closure{}, p.ErrUnavailable
	}
	if _, e = tx.ExecContext(ctx, `SELECT 1 FROM profiles.profiles WHERE issuer=$1 AND subject=$2 FOR UPDATE`, who.Issuer, who.Subject); e != nil {
		return p.Closure{}, p.ErrUnavailable
	}
	record, e := closureScan(tx.QueryRowContext(ctx, `SELECT issuer,subject,result FROM profiles.closures WHERE id=$1 FOR UPDATE`, id))
	if e != nil {
		return p.Closure{}, e
	}
	out := record.Closure
	if len(out.CompletedSteps) != step || out.State == "closed" {
		if _, e = tx.ExecContext(ctx, `UPDATE profiles.closures SET processing_until=NULL WHERE id=$1`, id); e != nil {
			return out, p.ErrUnavailable
		}
		if e = tx.Commit(); e != nil {
			return out, p.ErrUnavailable
		}
		return out, nil
	}
	if out.State != "awaiting_assets" && out.State != "processing" {
		return out, p.ErrConflict
	}
	steps := []string{"assets", "sessions", "authorizations", "content"}
	if step < 0 || step >= len(steps) {
		return out, p.ErrInput
	}
	allowed := [][]string{{"billing"}, {}, {"open-platform"}, {"reviews", "resources", "favorites", "qq-binding"}}
	for _, name := range skipped {
		if !slices.Contains(allowed[step], name) {
			return out, p.ErrInput
		}
	}
	normalizedReasons, ok := p.NormalizePendingReasons(pendingReasons)
	if !ok || (len(normalizedReasons) > 0 && (step != 0 || blocker != "assets_pending")) {
		return out, p.ErrInput
	}
	for _, name := range skipped {
		if !slices.Contains(out.SkippedModules, name) {
			out.SkippedModules = append(out.SkippedModules, name)
		}
	}
	out.Blocker = blocker
	if blocker == "assets_pending" {
		out.PendingReasons = normalizedReasons
	} else {
		out.PendingReasons = nil
	}
	out.Revision++
	if blocker == "" {
		out.CompletedSteps = append(out.CompletedSteps, steps[step])
		out.State = "processing"
		if len(out.CompletedSteps) == 4 {
			out.State = "closed"
			out.CompletedAt = &now
			visibility, _ := json.Marshal(p.DefaultVisibility())
			consent, _ := json.Marshal(p.GroupCardConsent{})
			var rev int64
			if e = tx.QueryRowContext(ctx, `UPDATE profiles.profiles SET account_state='closed',nickname='',bio='',visibility=$3,group_card_consent=$4,revision=revision+1 WHERE issuer=$1 AND subject=$2 RETURNING revision`, who.Issuer, who.Subject, visibility, consent).Scan(&rev); e != nil {
				return out, p.ErrUnavailable
			}
			if _, e = tx.ExecContext(ctx, `DELETE FROM profiles.requests WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject); e != nil {
				return out, p.ErrUnavailable
			}
			if _, e = tx.ExecContext(ctx, `DELETE FROM profiles.closure_requests WHERE actor_issuer=$1 AND actor_subject=$2`, who.Issuer, who.Subject); e != nil {
				return out, p.ErrUnavailable
			}
			if _, e = tx.ExecContext(ctx, `INSERT INTO profiles.events(issuer,subject,revision,kind,created_at) VALUES($1,$2,$3,'closed',$4)`, who.Issuer, who.Subject, rev, now); e != nil {
				return out, p.ErrUnavailable
			}
		} else {
			if _, e = tx.ExecContext(ctx, `UPDATE profiles.profiles SET account_state=$3 WHERE issuer=$1 AND subject=$2`, who.Issuer, who.Subject, out.State); e != nil {
				return out, p.ErrUnavailable
			}
		}
	}
	raw, _ := json.Marshal(out)
	if _, e = tx.ExecContext(ctx, `UPDATE profiles.closures SET state=$2,result=$3,updated_at=$4,processing_until=NULL WHERE id=$1`, id, out.State, raw, now); e != nil {
		return out, p.ErrUnavailable
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO profiles.closure_events(closure_id,action,actor_issuer,actor_subject,created_at) VALUES($1,$2,$3,$4,$5)`, id, steps[step]+":"+blocker, actor.Issuer, actor.Subject, now); e != nil {
		return out, p.ErrUnavailable
	}
	if e = tx.Commit(); e != nil {
		return out, p.ErrUnavailable
	}
	return out, nil
}
