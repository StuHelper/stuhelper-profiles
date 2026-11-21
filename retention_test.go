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

func TestRetentionCleanupIdempotencyAndCompletedClosures(t *testing.T) {
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
	name := fmt.Sprintf("profiles_retention_%d", time.Now().UnixNano())
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
	s.ClosureAuthority = closureAuthority{}
	s.ClosureEffects = &closureEffects{seen: map[string]bool{}, effects: map[string]int{}}

	now := time.Date(2026, 2, 20, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	issuer := "https://issuer.invalid"
	admin := p.Principal{Issuer: issuer, Subject: "admin"}
	closedUser := p.Principal{Issuer: issuer, Subject: "closed_user"}
	cancelledUser := p.Principal{Issuer: issuer, Subject: "cancelled_user"}
	pendingUser := p.Principal{Issuer: issuer, Subject: "pending_user"}
	boundaryUser := p.Principal{Issuer: issuer, Subject: "boundary_user"}

	for i, u := range []p.Principal{closedUser, cancelledUser, pendingUser, boundaryUser} {
		if _, e = s.Ensure(ctx, u, fmt.Sprintf("User%d", i)); e != nil {
			t.Fatal(e)
		}
	}
	t.Run("Ops runtime role", func(t *testing.T) {
		if _, err := owner.Exec(`REVOKE UPDATE,DELETE ON profiles.requests,profiles.events,profiles.closure_requests,profiles.closure_events FROM ` + role); err != nil {
			t.Fatal(err)
		}
		person := p.Principal{Issuer: issuer, Subject: "restricted_user"}
		profile, err := s.Ensure(ctx, person, "Restricted")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Save(ctx, person, "restricted-profile", profile.Revision, "NewName"); err != nil {
			t.Errorf("save with Ops runtime privileges: %v", err)
		}
		closure, err := s.RequestClosure(ctx, person, "restricted-closure", true)
		if err != nil {
			t.Errorf("request closure with Ops runtime privileges: %v", err)
		} else if _, err := s.CancelClosure(ctx, person, "cancel-restricted", closure.ID); err != nil {
			t.Errorf("cancel closure with Ops runtime privileges: %v", err)
		}
		if _, err := s.CleanupExpired(ctx, now, 1); err != nil {
			t.Errorf("retention with Ops runtime privileges: %v", err)
		}
	})
	if t.Failed() {
		return
	}

	// Input bounds validation.
	for _, badLimit := range []int{0, -1, 1001} {
		if _, e = s.CleanupExpired(ctx, now, badLimit); !errors.Is(e, p.ErrInput) {
			t.Fatalf("expected ErrInput for batchLimit=%d, got %v", badLimit, e)
		}
	}
	if _, e = s.CleanupExpired(ctx, time.Time{}, 10); !errors.Is(e, p.ErrInput) {
		t.Fatal("expected ErrInput for zero time", e)
	}

	// 1. closedUser completes closure on 2026-02-28T11:59:59Z (strictly before 2027-02-28T12:00:00Z).
	cClosed, e := s.RequestClosure(ctx, closedUser, "req-closed", true)
	if e != nil {
		t.Fatal(e)
	}
	now = time.Date(2026, 2, 28, 11, 59, 59, 0, time.UTC)
	cClosed, e = s.ProcessClosure(ctx, admin, "proc-closed", cClosed.ID)
	if e != nil || cClosed.State != "closed" {
		t.Fatal(cClosed, e)
	}

	// 2. cancelledUser cancels on 2026-02-28T11:59:59Z.
	now = time.Date(2026, 2, 25, 12, 0, 0, 0, time.UTC)
	cCancelled, e := s.RequestClosure(ctx, cancelledUser, "req-cancel", true)
	if e != nil {
		t.Fatal(e)
	}
	now = time.Date(2026, 2, 28, 11, 59, 59, 0, time.UTC)
	if _, e = s.CancelClosure(ctx, cancelledUser, "do-cancel", cCancelled.ID); e != nil {
		t.Fatal(e)
	}

	// Older schemas lacked updated_at. Cancellation history starts at the
	// recorded cancellation, including when it happened days after the request.
	if _, e = owner.Exec("ALTER TABLE profiles.closures ALTER COLUMN updated_at DROP NOT NULL"); e != nil {
		t.Fatal(e)
	}
	if _, e = owner.Exec("UPDATE profiles.closures SET updated_at=NULL WHERE id=$1", cCancelled.ID); e != nil {
		t.Fatal(e)
	}
	for range 2 {
		if e = postgres.Migrate(ctx, owner); e != nil {
			t.Fatal(e)
		}
	}
	var restored time.Time
	if e = owner.QueryRow("SELECT updated_at FROM profiles.closures WHERE id=$1", cCancelled.ID).Scan(&restored); e != nil || !restored.Equal(now) {
		t.Fatalf("cancelled history timestamp %v, want %v: %v", restored, now, e)
	}

	// 3. pendingUser remains in cooling/incomplete state older than 12 months: must NEVER be deleted!
	now = time.Date(2026, 2, 20, 12, 0, 0, 0, time.UTC)
	cPending, e := s.RequestClosure(ctx, pendingUser, "req-pending", true)
	if e != nil {
		t.Fatal(e)
	}

	// 4. boundaryUser cancels at exact 12-month boundary 2026-02-28T12:00:00Z (for leap day 2028-02-29 - 12m = 2027-02-28; here test 2027-02-28T12:00:00Z - 12m = 2026-02-28T12:00:00Z).
	now = time.Date(2026, 2, 25, 12, 0, 0, 0, time.UTC)
	cBoundary, e := s.RequestClosure(ctx, boundaryUser, "req-boundary", true)
	if e != nil {
		t.Fatal(e)
	}
	now = time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
	if _, e = s.CancelClosure(ctx, boundaryUser, "cancel-boundary", cBoundary.ID); e != nil {
		t.Fatal(e)
	}

	// Create profile update idempotency requests around 24h boundary before cleanupTime = 2027-02-28T12:00:00Z.
	cleanupTime := time.Date(2027, 2, 28, 12, 0, 0, 0, time.UTC)
	prof, e := s.Get(ctx, cancelledUser)
	if e != nil {
		t.Fatal(e)
	}
	now = cleanupTime.Add(-24*time.Hour - time.Microsecond)
	prof, e = s.Save(ctx, cancelledUser, "expired-req-1", prof.Revision, "Nick1")
	if e != nil {
		t.Fatal(e)
	}
	prof, e = s.Save(ctx, cancelledUser, "expired-req-2", prof.Revision, "Nick2")
	if e != nil {
		t.Fatal(e)
	}
	now = cleanupTime.Add(-24 * time.Hour)
	if _, e = s.Save(ctx, cancelledUser, "boundary-req", prof.Revision, "Nick3"); e != nil {
		t.Fatal(e)
	}

	// Bounded batchLimit=1 deletes 1 request, 1 closure_request, and 1 expired closure (+ its events) per call.
	r1, e := s.CleanupExpired(ctx, cleanupTime, 1)
	if e != nil || r1.RequestsDeleted != 1 || r1.ClosureRequestsDeleted != 1 || r1.ClosuresDeleted != 1 || r1.ClosureEventsDeleted == 0 {
		t.Fatalf("unexpected batch 1 cleanup result: %+v, err=%v", r1, e)
	}
	r2, e := s.CleanupExpired(ctx, cleanupTime, 100)
	if e != nil || r2.RequestsDeleted != 1 || r2.ClosuresDeleted != 1 || r2.ClosureEventsDeleted == 0 {
		t.Fatalf("unexpected batch 2 cleanup result: %+v, err=%v", r2, e)
	}
	r3, e := s.CleanupExpired(ctx, cleanupTime, 100)
	if e != nil || r3 != (p.CleanupResult{}) {
		t.Fatalf("expected idempotent zero cleanup on third pass: %+v, err=%v", r3, e)
	}

	// Verify boundary request, boundary closure, and incomplete closure survived!
	var reqCount int
	if e = owner.QueryRow(`SELECT count(*) FROM profiles.requests`).Scan(&reqCount); e != nil || reqCount != 1 {
		t.Fatalf("expected 1 boundary request retained, got %d (err=%v)", reqCount, e)
	}
	if _, e = repo.Closure(ctx, cPending.ID); e != nil {
		t.Fatal("incomplete closure was erroneously deleted", e)
	}
	if _, e = repo.Closure(ctx, cBoundary.ID); e != nil {
		t.Fatal("exact 12-month boundary closure was erroneously deleted", e)
	}
	if _, e = repo.Closure(ctx, cClosed.ID); !errors.Is(e, p.ErrNotFound) {
		t.Fatal("expired closed closure was not deleted", e)
	}
	// Closed account marker in profiles.profiles MUST remain to block reopening!
	if _, e = s.Ensure(ctx, closedUser, "ReopenAttempt"); !errors.Is(e, p.ErrClosed) {
		t.Fatal("closed account marker lost after history cleanup", e)
	}
	if e = postgres.Migrate(ctx, owner); e != nil {
		t.Fatal("repeat migration failed", e)
	}
}
