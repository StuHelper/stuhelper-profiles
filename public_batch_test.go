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

type batchPublicFactsProvider struct {
	batchCalls  int
	singleCalls int
	failed      bool
	byOwner     map[p.Principal][]p.Affiliation
}

func (f *batchPublicFactsProvider) ProfileFacts(_ context.Context, who p.Principal, _ p.Visibility) ([]p.Affiliation, error) {
	f.singleCalls++
	if f.failed {
		return nil, p.ErrUnavailable
	}
	return f.byOwner[who], nil
}

func (f *batchPublicFactsProvider) ProfileFactsBatch(_ context.Context, requests []p.PublicFactsRequest) (map[p.Principal][]p.Affiliation, error) {
	f.batchCalls++
	if f.failed {
		return nil, p.ErrUnavailable
	}
	out := make(map[p.Principal][]p.Affiliation, len(requests))
	for _, r := range requests {
		out[r.Owner] = f.byOwner[r.Owner]
	}
	return out, nil
}

func TestPublicBatchAndIDsEliminateNPlusOneAndEnforceVisibility(t *testing.T) {
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
	name := fmt.Sprintf("profiles_batch_%d", time.Now().UnixNano())
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
	if _, e = owner.Exec(`CREATE ROLE ` + role + ` LOGIN;REVOKE ALL ON SCHEMA public FROM PUBLIC;GRANT USAGE ON SCHEMA profiles TO ` + role + `;GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA profiles TO ` + role + `;GRANT USAGE ON ALL SEQUENCES IN SCHEMA profiles TO ` + role + `;`); e != nil {
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

	s, _ := p.New(repo)
	issuer := "https://issuer.invalid"
	alice := p.Principal{Issuer: issuer, Subject: "alice"}
	bob := p.Principal{Issuer: issuer, Subject: "bob"}
	pending := p.Principal{Issuer: issuer, Subject: "pending"}
	closed := p.Principal{Issuer: issuer, Subject: "closed"}
	missing := p.Principal{Issuer: issuer, Subject: "missing"}
	admin := p.Principal{Issuer: issuer, Subject: "admin"}

	pAlice, e := s.Ensure(ctx, alice, "Alice")
	if e != nil {
		t.Fatal(e)
	}
	pBob, e := s.Ensure(ctx, bob, "Bob")
	if e != nil {
		t.Fatal(e)
	}
	hidden := p.Visibility{}
	pBob, e = s.Update(ctx, bob, "hide-bob", p.Update{Nickname: "Bob", Revision: pBob.Revision, Visibility: &hidden})
	if e != nil {
		t.Fatal(e)
	}
	pPending, e := s.Ensure(ctx, pending, "")
	if e != nil {
		t.Fatal(e)
	}
	pClosed, e := s.Ensure(ctx, closed, "ClosedAuthor")
	if e != nil {
		t.Fatal(e)
	}

	// Close closed user's account to verify immediate hiding from batch reads.
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	s.ClosureAuthority = closureAuthority{}
	s.ClosureEffects = &closureEffects{seen: map[string]bool{}, effects: map[string]int{}}
	cl, e := s.RequestClosure(ctx, closed, "close-req", true)
	if e != nil {
		t.Fatal(e)
	}
	now = cl.CoolingEndsAt
	if _, e = s.ProcessClosure(ctx, admin, "close-proc", cl.ID); e != nil {
		t.Fatal(e)
	}

	facts := &batchPublicFactsProvider{
		byOwner: map[p.Principal][]p.Affiliation{
			alice: {{SchoolID: "buaa", SchoolName: "BUAA", Department: "CS", Grade: "2024", Badge: "student"}},
			bob:   {{SchoolID: "buaa", SchoolName: "BUAA", Badge: "teacher"}},
		},
	}
	s.Facts = facts

	// Batch read with duplicates, pending nickname, closed account, and missing account:
	// only Alice and Bob are returned, and BatchPublicFacts is called ONCE (only for Alice since Bob hid academic fields)!
	got, e := s.PublicBatch(ctx, []p.Principal{alice, bob, alice, pending, closed, missing})
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 2 || facts.batchCalls != 1 || facts.singleCalls != 0 {
		t.Fatalf("unexpected batch result or call counts: len=%d batchCalls=%d singleCalls=%d", len(got), facts.batchCalls, facts.singleCalls)
	}
	if got[alice].Nickname != "Alice" || got[alice].FactsState != "available" || len(got[alice].Affiliations) != 1 || got[alice].Affiliations[0].Grade != "" || got[alice].Affiliations[0].Badge != "student" {
		t.Fatalf("unexpected Alice profile: %+v", got[alice])
	}
	if got[bob].Nickname != "Bob" || got[bob].FactsState != "hidden" || len(got[bob].Affiliations) != 0 {
		t.Fatalf("unexpected Bob profile: %+v", got[bob])
	}

	// PublicByIDs with duplicates, pending, closed, and non-UUID strings.
	byID, e := s.PublicByIDs(ctx, []string{pAlice.ID, pBob.ID, pAlice.ID, pPending.ID, pClosed.ID, "not-a-uuid\x00"})
	if e != nil || len(byID) != 2 || byID[pAlice.ID].Nickname != "Alice" || byID[pBob.ID].FactsState != "hidden" {
		t.Fatalf("unexpected PublicByIDs: %+v, err=%v", byID, e)
	}

	// Facts outage preserves basic profiles with FactsState == "unavailable".
	facts.failed = true
	degraded, e := s.PublicBatch(ctx, []p.Principal{alice, bob})
	if e != nil || degraded[alice].FactsState != "unavailable" || degraded[alice].Nickname != "Alice" || degraded[bob].FactsState != "hidden" {
		t.Fatalf("unexpected degraded batch result: %+v, err=%v", degraded, e)
	}

	// Reject > 100 unique targets and invalid principals.
	many := make([]p.Principal, 101)
	manyIDs := make([]string, 101)
	for i := range 101 {
		many[i] = p.Principal{Issuer: issuer, Subject: fmt.Sprintf("u-%d", i)}
		manyIDs[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	if _, e = s.PublicBatch(ctx, many); !errors.Is(e, p.ErrInput) {
		t.Fatal("expected ErrInput when unique principals > 100", e)
	}
	if _, e = s.PublicByIDs(ctx, manyIDs); !errors.Is(e, p.ErrInput) {
		t.Fatal("expected ErrInput when unique IDs > 100", e)
	}
	if _, e = s.PublicBatch(ctx, []p.Principal{{Issuer: issuer, Subject: "bad\x00"}}); !errors.Is(e, p.ErrInput) {
		t.Fatal("expected ErrInput for NUL byte in principal", e)
	}
}
