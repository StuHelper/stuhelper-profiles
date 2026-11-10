package profiles_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
	"os"
	"sync"
	"testing"
	"time"
)

func TestProfileOwnershipReplayConcurrentRevisionAndRuntimeRole(t *testing.T) {
	dsn := os.Getenv("STUHELPER_PROFILES_TEST_DSN")
	if dsn == "" {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	db, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	name := fmt.Sprintf("profiles_%d", time.Now().UnixNano())
	if _, e = db.Exec(`CREATE DATABASE ` + name); e != nil {
		t.Fatal(e)
	}
	defer db.Exec(`DROP DATABASE ` + name + ` WITH(FORCE)`)
	owner, e := sql.Open("pgx", dsn+" dbname="+name)
	if e != nil {
		t.Fatal(e)
	}
	defer owner.Close()
	if e = postgres.Migrate(ctx, owner); e != nil {
		t.Fatal(e)
	}
	role := name + "_runtime"
	if _, e = owner.Exec(`CREATE ROLE ` + role + ` LOGIN;REVOKE ALL ON SCHEMA public FROM PUBLIC;GRANT USAGE ON SCHEMA profiles TO ` + role + `;GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA profiles TO ` + role + `;GRANT USAGE ON ALL SEQUENCES IN SCHEMA profiles TO ` + role + `;CREATE SCHEMA foreign_data;CREATE TABLE foreign_data.secret(value text);`); e != nil {
		t.Fatal(e)
	}
	defer func() { owner.Close(); db.Exec(`DROP DATABASE ` + name + ` WITH(FORCE)`); db.Exec(`DROP ROLE ` + role) }()
	runtime, e := sql.Open("pgx", dsn+" dbname="+name+" user="+role)
	if e != nil {
		t.Fatal(e)
	}
	defer runtime.Close()
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
	bob := p.Principal{Issuer: alice.Issuer, Subject: "bob"}
	v, e := s.Ensure(ctx, alice, "Chosen nickname")
	if e != nil || v.Revision != 1 {
		t.Fatal(v, e)
	}
	if _, e = s.Get(ctx, bob); !errors.Is(e, p.ErrNotFound) {
		t.Fatal("another subject inherited profile", e)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			v, e := s.Save(ctx, alice, "same", 1, "New nickname")
			if e == nil && v.Revision != 2 {
				e = errors.New("bad revision")
			}
			results <- e
		})
	}
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.Save(ctx, alice, "same", 1, "Different"); !errors.Is(e, p.ErrConflict) {
		t.Fatal("changed replay accepted", e)
	}
	if _, e = s.Save(ctx, alice, "new", 1, "Stale"); !errors.Is(e, p.ErrConflict) {
		t.Fatal("stale revision accepted", e)
	}
	v, e = s.Ensure(ctx, alice, "Later SSO name")
	if e != nil || v.Nickname != "New nickname" {
		t.Fatal("login overwrote profile", v, e)
	}
	var count int
	if e = owner.QueryRow(`SELECT count(*) FROM profiles.events`).Scan(&count); e != nil || count != 2 {
		t.Fatal("incorrect business effects", count, e)
	}
}
