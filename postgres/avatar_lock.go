package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"time"

	p "github.com/StuHelper/stuhelper-profiles"
)

// Avatar operations run on the session that owns the PostgreSQL advisory lock.
// Reusing that connection also permits a runtime pool limited to one connection.
type avatarConnectionKey struct{ db *sql.DB }

type avatarExecutor interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (r *Repository) avatarDB(ctx context.Context) avatarExecutor {
	if conn, ok := ctx.Value(avatarConnectionKey{r.DB}).(*sql.Conn); ok {
		return conn
	}
	return r.DB
}

// WithAvatarOwner serializes activation and release across application instances.
// The session lock allows recovery intent to commit before the external call.
func (r *Repository) WithAvatarOwner(ctx context.Context, who p.Principal, work func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := r.DB.Conn(ctx)
	if err != nil {
		return p.ErrUnavailable
	}
	defer conn.Close()
	key, err := json.Marshal([]string{who.Issuer, who.Subject})
	if err != nil {
		return p.ErrInput
	}
	discard := func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }) }
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1,485973))`, string(key)); err != nil {
		discard()
		return p.ErrUnavailable
	}
	defer func() {
		release, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer stop()
		var unlocked bool
		if err := conn.QueryRowContext(release, `SELECT pg_advisory_unlock(hashtextextended($1,485973))`, string(key)).Scan(&unlocked); err != nil || !unlocked {
			discard()
		}
	}()
	return work(context.WithValue(ctx, avatarConnectionKey{r.DB}, conn))
}
