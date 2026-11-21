package postgres

import (
	"context"
	"time"

	p "github.com/StuHelper/stuhelper-profiles"
)

const closureCompletionSchema = `
CREATE TABLE IF NOT EXISTS profiles.closure_completions(
 closure_id text COLLATE "C" PRIMARY KEY CHECK (closure_id ~ '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$'),
 issuer text NOT NULL,
 subject text NOT NULL,
 completed_at timestamptz NOT NULL CHECK (isfinite(completed_at))
);
REVOKE ALL ON TABLE profiles.closure_completions FROM PUBLIC;

CREATE OR REPLACE FUNCTION profiles.ack_closure_completion(p_closure_id text)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 DELETE FROM profiles.closure_completions WHERE closure_id=p_closure_id;
$$;
REVOKE ALL ON FUNCTION profiles.ack_closure_completion(text) FROM PUBLIC;
`

// EnableClosureCompletions validates the host's grants and activates durable
// completion capture. Call during assembly, before processing any closures.
func (r *Repository) EnableClosureCompletions(ctx context.Context) error {
	if err := r.CheckRuntime(ctx); err != nil {
		return err
	}
	var permitted bool
	err := r.DB.QueryRowContext(ctx, `
 SELECT has_table_privilege(current_user,'profiles.closure_completions','SELECT')
    AND has_table_privilege(current_user,'profiles.closure_completions','INSERT')
    AND NOT has_table_privilege(current_user,'profiles.closure_completions','UPDATE,DELETE,TRUNCATE')
    AND has_function_privilege(current_user,'profiles.ack_closure_completion(text)','EXECUTE')
    AND NOT (SELECT rolbypassrls FROM pg_roles WHERE rolname=current_user)
`).Scan(&permitted)
	if err != nil || !permitted {
		return p.ErrUnavailable
	}
	r.captureClosureCompletions = true
	return nil
}

// PendingClosureCompletions returns at most 100 unacknowledged facts ordered by
// closure ID, strictly after the optional cursor. Restart from the first page
// after each pass to pick up new or previously failed events.
func (r *Repository) PendingClosureCompletions(ctx context.Context, after string, limit int) ([]p.ClosureCompletion, error) {
	if limit < 1 || limit > 100 || (after != "" && len(after) != 36) {
		return nil, p.ErrInput
	}
	if !r.captureClosureCompletions {
		return nil, p.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	rows, err := r.DB.QueryContext(ctx, `
 SELECT closure_id,issuer,subject,completed_at FROM profiles.closure_completions
 WHERE closure_id>$1 ORDER BY closure_id LIMIT $2`, after, limit)
	if err != nil {
		return nil, p.ErrUnavailable
	}
	defer rows.Close()

	out := make([]p.ClosureCompletion, 0, limit)
	for rows.Next() {
		var item p.ClosureCompletion
		if err := rows.Scan(&item.ClosureID, &item.Owner.Issuer, &item.Owner.Subject, &item.CompletedAt); err != nil {
			return nil, p.ErrUnavailable
		}
		item.CompletedAt = item.CompletedAt.UTC()
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, p.ErrUnavailable
	}
	return out, nil
}

// AcknowledgeClosureCompletion removes a fact after the consumer has durably
// handled it. Repeating a successful acknowledgement has the same outcome.
func (r *Repository) AcknowledgeClosureCompletion(ctx context.Context, id string) error {
	if len(id) != 36 {
		return p.ErrInput
	}
	if !r.captureClosureCompletions {
		return p.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := r.DB.ExecContext(ctx, `SELECT profiles.ack_closure_completion($1)`, id); err != nil {
		return p.ErrUnavailable
	}
	return nil
}
