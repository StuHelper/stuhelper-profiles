package profiles_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/postgres"
	"os"
	"testing"
	"time"
)

type informationAuthority struct{ denied bool }

func (a *informationAuthority) AuthorizeInformation(_ context.Context, principal p.Principal, cap string, scope p.Scope, target string) error {
	if principal.Subject != "admin" || a.denied || (target != "" && scope.School != "school") {
		return p.ErrForbidden
	}
	return nil
}
func (a *informationAuthority) AuditInformation(context.Context, p.Principal, string, p.Scope, []string, []string) error {
	return nil
}
func TestInformationProfileScopeAndSelection(t *testing.T) {
	dsn := os.Getenv("STUHELPER_PROFILES_TEST_DSN")
	if dsn == "" {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	db, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	name := fmt.Sprintf("profiles_%d", time.Now().UnixNano())
	if _, e = db.Exec(`CREATE DATABASE ` + name); e != nil {
		t.Fatal(e)
	}

	owner, e := sql.Open("pgx", dsn+" dbname="+name)
	if e != nil {
		t.Fatal(e)
	}

	if e = postgres.Migrate(ctx, owner); e != nil {
		t.Fatal(e)
	}
	role := name + "_runtime"
	if _, e = owner.Exec(`CREATE ROLE ` + role + ` LOGIN;REVOKE ALL ON SCHEMA public FROM PUBLIC;GRANT USAGE ON SCHEMA profiles TO ` + role + `;GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA profiles TO ` + role + `;GRANT USAGE ON ALL SEQUENCES IN SCHEMA profiles TO ` + role + `;CREATE SCHEMA foreign_data;CREATE TABLE foreign_data.secret(value text);`); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { owner.Close(); db.Exec(`DROP DATABASE ` + name + ` WITH(FORCE)`); db.Exec(`DROP ROLE ` + role) })
	runtime, e := sql.Open("pgx", dsn+" dbname="+name+" user="+role)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { runtime.Close() })
	repo := &postgres.Repository{DB: runtime}
	if e = repo.CheckRuntime(ctx); e != nil {
		t.Fatal(e)
	}
	if e = (&postgres.Repository{DB: owner}).CheckRuntime(ctx); e == nil {
		t.Fatal("migration identity accepted")
	}
	if _, e = runtime.Exec(`SELECT * FROM foreign_data.secret`); e == nil {
		t.Fatal("foreign schema readable")
	}

	s, _ := p.New(repo)
	alice := p.Principal{Issuer: "https://issuer.invalid", Subject: "alice"}
	admin := p.Principal{Issuer: alice.Issuer, Subject: "admin"}
	profile, e := s.Ensure(ctx, alice, "Nickname")
	if e != nil {
		t.Fatal(e)
	}
	key := base64.RawURLEncoding.EncodeToString([]byte(alice.Issuer)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(alice.Subject))
	q := p.Query{Kind: "account", Value: key, Scope: p.Scope{School: "school"}, Purpose: "support", Limit: 1, Fields: []string{"nickname", "visibility", "group_card_consent"}}
	if _, e = s.Query(ctx, admin, q); !errors.Is(e, p.ErrUnavailable) {
		t.Fatal(e)
	}
	auth := &informationAuthority{}
	s.Information = auth
	rows, e := s.Query(ctx, admin, q)
	if e != nil || len(rows) != 1 || rows[0].ID != profile.ID || len(rows[0].Fields) != 3 || rows[0].Fields[0].Value != "Nickname" {
		t.Fatal(rows, e)
	}
	q.After = profile.ID
	rows, e = s.Query(ctx, admin, q)
	if e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
	q.After = ""
	q.Scope.School = "outside"
	rows, e = s.Query(ctx, admin, q)
	if e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
	if _, e = s.Query(ctx, alice, q); !errors.Is(e, p.ErrForbidden) {
		t.Fatal(e)
	}
	q.Kind = "name"
	if _, e = s.Query(ctx, admin, q); !errors.Is(e, p.ErrInput) {
		t.Fatal(e)
	}
}
