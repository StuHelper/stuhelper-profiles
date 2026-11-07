package profiles_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/postgres"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPublicProfileDefaultsUnicodeCompletionAndVisibility(t *testing.T) {
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
	alice := p.Principal{Issuer: "issuer", Subject: "alice"}
	bob := p.Principal{Issuer: "issuer", Subject: "bob"}
	v, e := s.Ensure(ctx, alice, p.InitialNickname(strings.Repeat("长", 21), "username"))
	if e != nil || v.Nickname != "username" || v.NicknameRequired || v.Visibility != p.DefaultVisibility() {
		t.Fatal(v, e)
	}
	source := &publicFacts{}
	s.Facts = source
	visible, e := s.Public(ctx, v.ID)
	if e != nil || visible.FactsState != "available" || len(visible.Affiliations) != 1 || visible.Affiliations[0].Grade != "" || visible.Affiliations[0].SchoolID != "school" || source.owner != alice {
		t.Fatal(visible, e)
	}
	source.failed = true
	visible, e = s.Public(ctx, v.ID)
	if e != nil || visible.Nickname != "username" || visible.FactsState != "unavailable" || len(visible.Affiliations) != 0 {
		t.Fatal("basic page lost or stale facts leaked", visible, e)
	}
	source.failed = false
	originalID := v.ID
	pending, e := s.Ensure(ctx, bob, p.InitialNickname(strings.Repeat("长", 21), " invalid "))
	if e != nil || !pending.NicknameRequired || pending.Nickname != "" {
		t.Fatal(pending, e)
	}
	if _, e = s.Public(ctx, pending.ID); !errors.Is(e, p.ErrNotFound) {
		t.Fatal("unfinished nickname published", e)
	}
	// Emoji count as one code point, matching the API contract.
	bio := strings.Repeat("😀", 120)
	hidden := p.Visibility{}
	in := p.Update{Nickname: strings.Repeat("😀", 20), Revision: v.Revision, Bio: &bio, Visibility: &hidden}
	var wg sync.WaitGroup
	results := make(chan error, 6)
	for range 6 {
		wg.Go(func() {
			got, e := s.Update(ctx, alice, "same-public", in)
			if e == nil && (got.Revision != 2 || got.ID != originalID) {
				e = errors.New("replay changed result")
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
	calls := source.calls
	public, e := s.Public(ctx, originalID)
	if e != nil || public.Visibility != hidden || public.Bio != bio || public.Nickname != in.Nickname {
		t.Fatal(public, e)
	}
	if public.FactsState != "hidden" || len(public.Affiliations) != 0 || source.calls != calls {
		t.Fatal("hidden fields consulted provider", public)
	}
	b, _ := json.Marshal(public)
	for _, field := range []string{"issuer", "subject", "identity", "balance", "document", "revision"} {
		if strings.Contains(string(b), field) {
			t.Fatal("private public field", field)
		}
	}
	// Omitted optional fields survive nickname-only updates and later SSO logins.
	v, e = s.Save(ctx, alice, "nickname", 2, "nickname")
	if e != nil || v.Visibility != hidden || v.Bio != bio {
		t.Fatal(v, e)
	}
	if got, e := s.Ensure(ctx, alice, ""); e != nil || got != v {
		t.Fatal("login overwrote completed profile", got, e)
	}
	got, e := s.Update(ctx, alice, "stale", in)
	if !errors.Is(e, p.ErrConflict) || got.Revision != 3 {
		t.Fatal("missing current conflict data", got, e)
	}
	in.Revision = 3
	in.Nickname = strings.Repeat("😀", 21)
	if _, e = s.Update(ctx, alice, "too-long", in); !errors.Is(e, p.ErrInput) {
		t.Fatal(e)
	}
	in.Nickname = "okay"
	longBio := bio + "😀"
	in.Bio = &longBio
	if _, e = s.Update(ctx, alice, "long-bio", in); !errors.Is(e, p.ErrInput) {
		t.Fatal(e)
	}
	v, e = s.Save(ctx, bob, "complete", 1, "Bob")
	if e != nil || v.NicknameRequired {
		t.Fatal(v, e)
	}
	if _, e = s.Public(ctx, v.ID); e != nil {
		t.Fatal(e)
	}
	var flag p.Visibility
	if json.Unmarshal([]byte(`{"school":true}`), &flag) == nil {
		t.Fatal("partial visibility cleared preferences")
	}
	var count int
	if e = owner.QueryRow(`SELECT count(*) FROM profiles.events`).Scan(&count); e != nil || count != 5 {
		t.Fatal(count, e)
	}
	// An old nickname-only idempotency result gains only fields known at that revision.
	digestBytes := sha256.Sum256([]byte(`{"Revision":1,"Name":"Past"}`))
	if _, e = owner.Exec(`INSERT INTO profiles.requests VALUES($1,$2,'legacy',$3,'{"nickname":"Past","revision":2}')`, alice.Issuer, alice.Subject, hex.EncodeToString(digestBytes[:])); e != nil {
		t.Fatal(e)
	}
	// Repeated migrations preserve profile IDs, settings and idempotent responses.
	if e = postgres.Migrate(ctx, owner); e != nil {
		t.Fatal(e)
	}
	if e = postgres.Migrate(ctx, owner); e != nil {
		t.Fatal(e)
	}
	legacy, e := s.Save(ctx, alice, "legacy", 1, "Past")
	if e != nil || legacy.ID != originalID || legacy.Nickname != "Past" || legacy.Bio != "" || legacy.Visibility != p.DefaultVisibility() {
		t.Fatal("old replay", legacy, e)
	}
	again, e := s.Get(ctx, alice)
	if e != nil || again.ID != originalID || again.Bio != bio || again.Visibility != hidden {
		t.Fatal(again, e)
	}
}

type publicFacts struct {
	calls  int
	failed bool
	owner  p.Principal
}

func (f *publicFacts) ProfileFacts(_ context.Context, who p.Principal, _ p.Visibility) ([]p.Affiliation, error) {
	f.calls++
	f.owner = who
	if f.failed {
		return nil, p.ErrUnavailable
	}
	return []p.Affiliation{{SchoolID: "school", SchoolName: "School", Department: "Department", Grade: "2026", Badge: "student"}}, nil
}
