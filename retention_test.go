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
	if _, e = owner.Exec(runtimeRoleSQL(role)); e != nil {
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

	wall := time.Now().UTC()
	cleanupTime := time.Date(wall.Year(), wall.Month(), 1, 12, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	closureBoundary := cleanupTime.AddDate(-1, 0, 0)
	now := closureBoundary.Add(-8 * 24 * time.Hour)
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
	// Input bounds validation.
	for _, badLimit := range []int{0, -1, 1001} {
		if _, e = s.CleanupExpired(ctx, now, badLimit); !errors.Is(e, p.ErrInput) {
			t.Fatalf("expected ErrInput for batchLimit=%d, got %v", badLimit, e)
		}
	}
	if _, e = s.CleanupExpired(ctx, time.Time{}, 10); !errors.Is(e, p.ErrInput) {
		t.Fatal("expected ErrInput for zero time", e)
	}

	// 1. The closed request expires one second before the cutoff.
	cClosed, e := s.RequestClosure(ctx, closedUser, "req-closed", true)
	if e != nil {
		t.Fatal(e)
	}
	now = closureBoundary.Add(-time.Second)
	cClosed, e = s.ProcessClosure(ctx, admin, "proc-closed", cClosed.ID)
	if e != nil || cClosed.State != "closed" {
		t.Fatal(cClosed, e)
	}

	// 2. The cancelled request expires one second before the cutoff.
	now = closureBoundary.Add(-3 * 24 * time.Hour)
	cCancelled, e := s.RequestClosure(ctx, cancelledUser, "req-cancel", true)
	if e != nil {
		t.Fatal(e)
	}
	now = closureBoundary.Add(-time.Second)
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
	now = closureBoundary.Add(-8 * 24 * time.Hour)
	cPending, e := s.RequestClosure(ctx, pendingUser, "req-pending", true)
	if e != nil {
		t.Fatal(e)
	}

	// 4. A cancellation at the exact 12-month boundary remains.
	now = closureBoundary.Add(-3 * 24 * time.Hour)
	cBoundary, e := s.RequestClosure(ctx, boundaryUser, "req-boundary", true)
	if e != nil {
		t.Fatal(e)
	}
	now = closureBoundary
	if _, e = s.CancelClosure(ctx, boundaryUser, "cancel-boundary", cBoundary.ID); e != nil {
		t.Fatal(e)
	}

	// Create profile update idempotency requests around the 24-hour boundary.
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

	t.Run("Ops runtime role", func(t *testing.T) {
		now = wall.Truncate(time.Millisecond)
		person := p.Principal{Issuer: issuer, Subject: "restricted_user"}
		profile, err := s.Ensure(ctx, person, "Restricted")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Save(ctx, person, "restricted-profile", profile.Revision, "NewName"); err != nil {
			t.Fatalf("save with Ops runtime privileges: %v", err)
		}
		closure, err := s.RequestClosure(ctx, person, "restricted-closure", true)
		if err != nil {
			t.Fatalf("request closure with Ops runtime privileges: %v", err)
		}
		cancelled, err := s.CancelClosure(ctx, person, "cancel-restricted", closure.ID)
		if err != nil || cancelled.State != "cancelled" {
			t.Fatal(cancelled, err)
		}

		for _, table := range []string{"requests", "events", "closure_requests", "closure_events", "closures"} {
			if _, err := runtime.Exec("DELETE FROM profiles." + table + " WHERE false"); err == nil {
				t.Fatalf("runtime may directly delete profiles.%s", table)
			}
		}
		future := now.AddDate(10, 0, 0)
		if _, err := runtime.Exec("SELECT profiles.erase_closed_account_requests($1,$2)", issuer, person.Subject); err == nil {
			t.Fatal("runtime erased an active account's receipts")
		}
		if _, err := runtime.Exec("SELECT profiles.expire_profile_request($1,$2,$3,$4)", issuer, person.Subject, "restricted-profile", future); err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Exec("SELECT profiles.expire_closure_request($1,$2,$3,$4)", issuer, person.Subject, "restricted-closure", future); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CleanupExpired(ctx, future, 100); err != nil {
			t.Fatal(err)
		}
		var requests, closureRequests, closureEvents int
		if err := runtime.QueryRow(`SELECT
		 (SELECT count(*) FROM profiles.requests WHERE issuer=$1 AND subject=$2),
		 (SELECT count(*) FROM profiles.closure_requests WHERE actor_issuer=$1 AND actor_subject=$2),
		 (SELECT count(*) FROM profiles.closure_events WHERE closure_id=$3)`, issuer, person.Subject, closure.ID).Scan(&requests, &closureRequests, &closureEvents); err != nil {
			t.Fatal(err)
		}
		if requests != 1 || closureRequests != 2 || closureEvents != 2 {
			t.Fatalf("future cutoff removed live records: %d %d %d", requests, closureRequests, closureEvents)
		}
		if _, e = repo.Closure(ctx, cancelled.ID); e != nil {
			t.Fatal("future cutoff removed recent cancellation", e)
		}
	})
}
