package profiles_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/postgres"
	"os"
	"testing"
	"time"
)

type cardProvider struct {
	denied bool
	calls  int
}

func (a *cardProvider) AuthorizeGroupCard(_ context.Context, principal p.Principal, school string) error {
	if a.denied || principal.Subject != "service" || school != "school" {
		return p.ErrForbidden
	}
	return nil
}
func (a *cardProvider) GroupCardValues(_ context.Context, _ p.Principal, _, _ string, fields []string) (map[string]p.CardField, error) {
	a.calls++
	return map[string]p.CardField{"name": {Available: true, Value: "Synthetic name"}, "student_id": {Available: true, Value: "secret"}}, nil
}
func TestGroupCardConsentAuthorizationAndTransaction(t *testing.T) {
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
	provider := &cardProvider{}
	s.GroupCards = provider
	who := p.Principal{Issuer: "issuer", Subject: "alice"}
	principal := p.Principal{Issuer: "issuer", Subject: "service"}
	key := "aXNzdWVy:YWxpY2U"
	profile, e := s.Ensure(ctx, who, "Nickname")
	if e != nil {
		t.Fatal(e)
	}
	fields, e := s.GroupCardFields(ctx, principal, key, "school", p.GroupCardPurpose)
	if e != nil || len(fields) != 7 || provider.calls != 0 {
		t.Fatal(fields, e)
	}
	consent := p.GroupCardConsent{Name: true, Grade: true, Nickname: true}
	profile, e = s.Update(ctx, who, "consent", p.Update{Nickname: profile.Nickname, Revision: profile.Revision, GroupCardConsent: &consent})
	if e != nil {
		t.Fatal(e)
	}
	fields, e = s.GroupCardFields(ctx, principal, key, "school", p.GroupCardPurpose)
	if e != nil || fields["name"].Value != "Synthetic name" || fields["nickname"].Value != "Nickname" || fields["student_id"].Value != "" || fields["grade"].Available || !fields["grade"].Allowed {
		t.Fatal(fields, e)
	}
	for _, school := range []string{"other"} {
		if _, e = s.GroupCardFields(ctx, principal, key, school, p.GroupCardPurpose); !errors.Is(e, p.ErrForbidden) {
			t.Fatal(e)
		}
	}
	if _, e = s.GroupCardFields(ctx, who, key, "school", p.GroupCardPurpose); !errors.Is(e, p.ErrForbidden) {
		t.Fatal(e)
	}
	provider.denied = true
	if _, e = s.GroupCardFields(ctx, principal, key, "school", p.GroupCardPurpose); !errors.Is(e, p.ErrForbidden) {
		t.Fatal(e)
	}
	provider.denied = false
	if _, e = s.GroupCardFields(ctx, principal, key, "school", "public"); !errors.Is(e, p.ErrInput) {
		t.Fatal(e)
	}
	var before int
	if e = owner.QueryRow(`SELECT count(*) FROM profiles.events`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	_, e = owner.Exec(`CREATE FUNCTION profiles.reject_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic'; END $$; CREATE TRIGGER reject_event BEFORE INSERT ON profiles.events FOR EACH ROW EXECUTE FUNCTION profiles.reject_event()`)
	if e != nil {
		t.Fatal(e)
	}
	denied := p.GroupCardConsent{}
	_, e = s.Update(ctx, who, "rollback", p.Update{Nickname: profile.Nickname, Revision: profile.Revision, GroupCardConsent: &denied})
	if e == nil {
		t.Fatal("event failure ignored")
	}
	current, e := s.Get(ctx, who)
	if e != nil || current.GroupCardConsent != consent || current.Revision != profile.Revision {
		t.Fatal(current, e)
	}
	var after int
	if e = owner.QueryRow(`SELECT count(*) FROM profiles.events`).Scan(&after); e != nil || after != before {
		t.Fatal(after, e)
	}
	_, e = owner.Exec(`DROP TRIGGER reject_event ON profiles.events`)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.Update(ctx, who, "revoke", p.Update{Nickname: profile.Nickname, Revision: profile.Revision, GroupCardConsent: &denied})
	if e != nil {
		t.Fatal(e)
	}
	fields, e = s.GroupCardFields(ctx, principal, key, "school", p.GroupCardPurpose)
	if e != nil || fields["name"].Allowed || fields["name"].Value != "" {
		t.Fatal(fields, e)
	}
	if e = postgres.Migrate(ctx, owner); e != nil {
		t.Fatal(e)
	}
}
