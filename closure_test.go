package profiles_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/postgres"
	"os"
	"sync"
	"testing"
	"time"
)

type closureAuthority struct{}

func (closureAuthority) AuthorizeClosure(_ context.Context, who p.Principal) error {
	if who.Subject != "admin" {
		return p.ErrForbidden
	}
	return nil
}

type closureEffects struct {
	mu      sync.Mutex
	blocked string
	skipped []string
	seen    map[string]bool
	effects map[string]int
}

func (f *closureEffects) step(name, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blocked == name {
		return p.ErrUnavailable
	}
	key := name + id
	if !f.seen[key] {
		f.seen[key] = true
		f.effects[name]++
	}
	return nil
}
func (f *closureEffects) ClearAssets(_ context.Context, _ p.Principal, id string) error {
	return f.step("assets", id)
}
func (f *closureEffects) RevokeSessions(_ context.Context, _ p.Principal, id string) error {
	return f.step("sessions", id)
}
func (f *closureEffects) RevokeAuthorizations(_ context.Context, _ p.Principal, id string) error {
	return f.step("authorizations", id)
}
func (f *closureEffects) AnonymizeContent(_ context.Context, _ p.Principal, id string) error {
	return f.step("content", id)
}
func (f *closureEffects) AnonymizeContentResult(ctx context.Context, who p.Principal, id string) ([]string, error) {
	return f.skipped, f.AnonymizeContent(ctx, who, id)
}
func TestClosureCoolingCancellationSettlementFailureAndConcurrentRecovery(t *testing.T) {
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
	name := fmt.Sprintf("closures_%d", time.Now().UnixNano())
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
	s.ClosureAuthority = closureAuthority{}
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	alice := p.Principal{Issuer: "https://issuer.invalid", Subject: "alice"}
	bob := p.Principal{Issuer: alice.Issuer, Subject: "bob"}
	admin := p.Principal{Issuer: alice.Issuer, Subject: "admin"}
	profile, e := s.Ensure(ctx, alice, "Alice")
	if e != nil {
		t.Fatal(e)
	}
	s.Ensure(ctx, bob, "Bob")
	if _, e = s.RequestClosure(ctx, alice, "no-consent", false); !errors.Is(e, p.ErrInput) {
		t.Fatal("consent", e)
	}
	v, e := s.RequestClosure(ctx, alice, "request", true)
	if e != nil || v.State != "cooling" || !v.CoolingEndsAt.Equal(now.Add(7*24*time.Hour)) {
		t.Fatal(v, e)
	}
	replay, e := s.RequestClosure(ctx, alice, "request", true)
	if e != nil || replay.ID != v.ID {
		t.Fatal(replay, e)
	}
	if _, e = s.CancelClosure(ctx, bob, "cancel", v.ID); !errors.Is(e, p.ErrNotFound) {
		t.Fatal("foreign cancellation", e)
	}
	if _, e = s.ProcessClosure(ctx, bob, "process", v.ID); !errors.Is(e, p.ErrForbidden) {
		t.Fatal("foreign approval", e)
	}
	if _, e = s.ProcessClosure(ctx, admin, "process", v.ID); !errors.Is(e, p.ErrConflict) {
		t.Fatal("early processing", e)
	}
	now = v.CoolingEndsAt.Add(-time.Nanosecond)
	if _, e = s.CancelClosure(ctx, alice, "cancel", v.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.AccountAccess(ctx, alice); e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Second)
	v, e = s.RequestClosure(ctx, alice, "second", true)
	if e != nil {
		t.Fatal(e)
	}
	now = v.CoolingEndsAt
	if _, e = s.CancelClosure(ctx, alice, "late-cancel", v.ID); !errors.Is(e, p.ErrConflict) {
		t.Fatal("deadline cancellation", e)
	}
	v, e = s.ProcessClosure(ctx, admin, "process", v.ID)
	if e != nil || v.State != "awaiting_assets" || v.Blocker != "assets_unavailable" || len(v.CompletedSteps) != 0 {
		t.Fatal(v, e)
	}
	if e = s.AccountAccess(ctx, alice); e != nil {
		t.Fatal("unknown assets locked owner", e)
	}
	f := &closureEffects{blocked: "authorizations", seen: map[string]bool{}, effects: map[string]int{}}
	s.ClosureEffects = f
	v, e = s.ProcessClosure(ctx, admin, "process", v.ID)
	if e != nil || v.State != "processing" || len(v.CompletedSteps) != 2 || v.Blocker != "authorizations_unavailable" {
		t.Fatal(v, e)
	}
	if e = s.AccountAccess(ctx, alice); !errors.Is(e, p.ErrClosed) {
		t.Fatal(e)
	}
	if _, e = s.Ensure(ctx, alice, "Reopened"); !errors.Is(e, p.ErrClosed) {
		t.Fatal("automatic reopen", e)
	}
	if _, e = s.Save(ctx, alice, "edit", profile.Revision, "Reopened"); !errors.Is(e, p.ErrClosed) {
		t.Fatal("mutation while processing", e)
	}
	if _, e = s.Public(ctx, profile.ID); !errors.Is(e, p.ErrNotFound) {
		t.Fatal("closed public profile", e)
	}
	f.blocked = "content"
	f.skipped = []string{"reviews"}
	v, e = s.ProcessClosure(ctx, admin, "process", v.ID)
	if e != nil || v.Blocker != "content_unavailable" || len(v.SkippedModules) != 1 || v.SkippedModules[0] != "reviews" {
		t.Fatal("skipped module progress not persisted", v, e)
	}
	if _, e = s.ProcessClosure(ctx, bob, "unauthorized-recovery", v.ID); !errors.Is(e, p.ErrForbidden) {
		t.Fatal("manual recovery bypassed current administrator permission", e)
	}
	// The retry no longer reports the earlier skip, for example because the content
	// dependency cannot read installation history now; the recorded skip must survive.
	f.blocked = ""
	f.skipped = nil
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			result, e := s.ProcessClosure(ctx, admin, "process", v.ID)
			if e == nil && result.State != "closed" {
				e = fmt.Errorf("incomplete: %s", result.State)
			}
			errs <- e
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	for _, step := range []string{"assets", "sessions", "authorizations", "content"} {
		if f.effects[step] != 1 {
			t.Fatalf("duplicate %s: %v", step, f.effects)
		}
	}
	result, e := s.OwnClosure(ctx, alice)
	if e != nil || result.State != "closed" || len(result.CompletedSteps) != 4 || result.CompletedAt == nil || len(result.SkippedModules) != 1 || result.SkippedModules[0] != "reviews" {
		t.Fatal(result, e)
	}
	if _, e = s.Ensure(ctx, alice, "Reopened"); !errors.Is(e, p.ErrClosed) {
		t.Fatal(e)
	}
	if e = s.AccountAccess(ctx, bob); e != nil {
		t.Fatal("unrelated account", e)
	}
	var nickname, bio, state string
	if e = owner.QueryRow(`SELECT nickname,bio,account_state FROM profiles.profiles WHERE subject='alice'`).Scan(&nickname, &bio, &state); e != nil || nickname != "" || bio != "" || state != "closed" {
		t.Fatal(nickname, bio, state, e)
	}
	if e = postgres.Migrate(ctx, owner); e != nil {
		t.Fatal("repeat migration", e)
	}
	page, e := s.ListClosures(ctx, admin, "", 1)
	if e != nil || len(page) != 1 {
		t.Fatal(page, e)
	}
	next, e := s.ListClosures(ctx, admin, page[0].ID, 1)
	if e != nil || len(next) != 1 || next[0].ID == page[0].ID {
		t.Fatal(next, e)
	}
}
