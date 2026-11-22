package profiles_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/postgres"
	"os"
	"slices"
	"testing"
	"time"
)

type detailedClosureEffects struct {
	closureEffects
	assetErr     error
	assetReasons []string
}

func (f *detailedClosureEffects) ClearAssetsDetail(ctx context.Context, who p.Principal, id string) (p.AssetsClearance, error) {
	if f.assetErr != nil {
		return p.AssetsClearance{PendingReasons: f.assetReasons}, f.assetErr
	}
	return p.AssetsClearance{}, f.ClearAssets(ctx, who, id)
}

func TestClosureAssetsPendingReasonsAndDueClosuresScan(t *testing.T) {
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
	name := fmt.Sprintf("closure_due_%d", time.Now().UnixNano())
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
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	issuer := "https://issuer.invalid"
	admin := p.Principal{Issuer: issuer, Subject: "admin"}
	alice := p.Principal{Issuer: issuer, Subject: "alice"}
	bob := p.Principal{Issuer: issuer, Subject: "bob"}
	carol := p.Principal{Issuer: issuer, Subject: "carol"}
	for _, u := range []p.Principal{alice, bob, carol} {
		if _, e = s.Ensure(ctx, u, u.Subject); e != nil {
			t.Fatal(e)
		}
	}

	cAlice, e := s.RequestClosure(ctx, alice, "req-alice", true)
	if e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Hour)
	cBob, e := s.RequestClosure(ctx, bob, "req-bob", true)
	if e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Hour)
	cCarol, e := s.RequestClosure(ctx, carol, "req-carol", true)
	if e != nil {
		t.Fatal(e)
	}
	// Cancel carol's request while in cooling.
	if _, e = s.CancelClosure(ctx, carol, "cancel-carol", cCarol.ID); e != nil {
		t.Fatal(e)
	}

	// 1ns before cAlice's CoolingEndsAt: DueClosures must be empty!
	now = cAlice.CoolingEndsAt.Add(-time.Nanosecond)
	if _, e = s.DueClosures(ctx, alice, 10); !errors.Is(e, p.ErrForbidden) {
		t.Fatal("expected ErrForbidden for non-admin DueClosures", e)
	}
	if _, e = s.DueClosures(ctx, admin, 0); !errors.Is(e, p.ErrInput) {
		t.Fatal("expected ErrInput for invalid limit", e)
	}
	dueEarly, e := s.DueClosures(ctx, admin, 10)
	if e != nil || len(dueEarly) != 0 {
		t.Fatalf("expected 0 due closures before deadline, got %+v, err=%v", dueEarly, e)
	}

	// At cAlice.CoolingEndsAt: only cAlice is due (cBob is still 1h away, cCarol is cancelled).
	now = cAlice.CoolingEndsAt
	dueAtAlice, e := s.DueClosures(ctx, admin, 10)
	if e != nil || len(dueAtAlice) != 1 || dueAtAlice[0].ID != cAlice.ID {
		t.Fatalf("expected only cAlice due, got %+v, err=%v", dueAtAlice, e)
	}

	// Process cAlice with sanitized pending_reasons via AssetsPendingError + ClearAssetsDetail.
	fx := &detailedClosureEffects{
		closureEffects: closureEffects{seen: map[string]bool{}, effects: map[string]int{}},
		assetErr:       &p.AssetsPendingError{Reasons: []string{p.PendingReasonRefundInProgress}},
		assetReasons:   []string{p.PendingReasonIvoryBalanceRemaining, p.PendingReasonIvoryBalanceRemaining},
	}
	s.ClosureEffects = fx
	cAlice, e = s.ProcessClosure(ctx, admin, "proc-alice-1", cAlice.ID)
	if e != nil || cAlice.State != "awaiting_assets" || cAlice.Blocker != "assets_pending" {
		t.Fatalf("unexpected pending closure state: %+v, err=%v", cAlice, e)
	}
	wantReasons := []string{p.PendingReasonIvoryBalanceRemaining, p.PendingReasonRefundInProgress}
	if !slices.Equal(cAlice.PendingReasons, wantReasons) {
		t.Fatalf("unexpected pending_reasons: got %v want %v", cAlice.PendingReasons, wantReasons)
	}
	ownView, e := s.OwnClosure(ctx, alice)
	if e != nil || !slices.Equal(ownView.PendingReasons, wantReasons) {
		t.Fatalf("OwnClosure missing pending_reasons: %+v, err=%v", ownView, e)
	}

	// Unknown or un-sanitized pending reason string is rejected by AdvanceClosureWithReasons.
	if _, e = repo.AdvanceClosureWithReasons(ctx, cAlice.ID, 0, "assets_pending", []string{"raw_amount_100"}, nil, now); !errors.Is(e, p.ErrInput) {
		t.Fatal("expected ErrInput for unknown pending_reason", e)
	}

	// At cBob.CoolingEndsAt: both cAlice (awaiting_assets) and cBob (cooling ended) are returned by DueClosures in chronological order!
	now = cBob.CoolingEndsAt
	dueBoth, e := s.DueClosures(ctx, admin, 10)
	if e != nil || len(dueBoth) != 2 || dueBoth[0].ID != cAlice.ID || dueBoth[1].ID != cBob.ID {
		t.Fatalf("expected [cAlice, cBob] due, got %+v, err=%v", dueBoth, e)
	}

	// Once Alice settles assets and retries ProcessClosure, PendingReasons is cleared and account closes.
	fx.assetErr = nil
	fx.assetReasons = nil
	cAlice, e = s.ProcessClosure(ctx, admin, "proc-alice-2", cAlice.ID)
	if e != nil || cAlice.State != "closed" || len(cAlice.PendingReasons) != 0 {
		t.Fatalf("expected closed closure with cleared pending_reasons, got %+v, err=%v", cAlice, e)
	}
	dueAfterClose, e := s.DueClosures(ctx, admin, 10)
	if e != nil || len(dueAfterClose) != 1 || dueAfterClose[0].ID != cBob.ID {
		t.Fatalf("expected only cBob due after Alice closed, got %+v, err=%v", dueAfterClose, e)
	}
}

type interruptedDueEffects struct {
	*detailedClosureEffects
	cancel          context.CancelFunc
	sessionAttempts int
}

func (f *interruptedDueEffects) RevokeSessions(ctx context.Context, who p.Principal, id string) error {
	f.sessionAttempts++
	if err := f.closureEffects.RevokeSessions(ctx, who, id); err != nil {
		return err
	}
	if f.cancel != nil {
		f.cancel()
		return ctx.Err()
	}
	return nil
}

func TestProcessDueClosuresAtDeadlineResumesAfterRestartAndLeavesBlockedCasesForAdmin(t *testing.T) {
	ctx := context.Background()
	repo := avatarRepository(t)
	s, err := p.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	assetEffects := &detailedClosureEffects{
		closureEffects: closureEffects{seen: map[string]bool{}, effects: map[string]int{}},
		assetErr:       &p.AssetsPendingError{Reasons: []string{p.PendingReasonRefundInProgress}},
	}
	fx := &interruptedDueEffects{detailedClosureEffects: assetEffects}
	s.ClosureEffects = fx
	now := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Millisecond)
	s.Now = func() time.Time { return now }
	issuer := "https://issuer.invalid"
	oldest := p.Principal{Issuer: issuer, Subject: "blocked"}
	recovering := p.Principal{Issuer: issuer, Subject: "recovering"}
	for _, person := range []p.Principal{oldest, recovering} {
		if _, err := s.Ensure(ctx, person, "Account"); err != nil {
			t.Fatal(err)
		}
	}
	blocked, err := s.RequestClosure(ctx, oldest, "blocked-request", true)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	recovery, err := s.RequestClosure(ctx, recovering, "recovery-request", true)
	if err != nil {
		t.Fatal(err)
	}

	now = blocked.CoolingEndsAt.Add(-time.Nanosecond)
	if early, err := s.ProcessDueClosures(ctx, 1); err != nil || len(early) != 0 {
		t.Fatalf("processed before deadline: %+v, %v", early, err)
	}
	now = blocked.CoolingEndsAt
	items, err := s.ProcessDueClosures(ctx, 1)
	if err != nil || len(items) != 1 || items[0].ID != blocked.ID || items[0].Blocker != "assets_pending" {
		t.Fatalf("expected one blocked application after its deadline: %+v, %v", items, err)
	}
	blockedRev := items[0].Revision

	assetEffects.assetErr = nil
	now = recovery.CoolingEndsAt
	inFlight, cancel := context.WithCancel(ctx)
	defer cancel()
	fx.cancel = cancel
	if _, err := s.ProcessDueClosures(inFlight, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("worker interruption was not reported: %v", err)
	}
	progress, err := s.OwnClosure(ctx, recovering)
	if err != nil || progress.State != "processing" || progress.Blocker != "" || !slices.Equal(progress.CompletedSteps, []string{"assets"}) {
		t.Fatalf("durable progress was lost or blocked on shutdown: %+v, %v", progress, err)
	}

	fx.cancel = nil
	restarted, err := p.New(&postgres.Repository{DB: repo.DB})
	if err != nil {
		t.Fatal(err)
	}
	restarted.ClosureEffects = fx
	restarted.Now = s.Now
	items, err = restarted.ProcessDueClosures(ctx, 1)
	if err != nil || len(items) != 1 || items[0].ID != recovery.ID || items[0].State != "closed" {
		t.Fatalf("restart did not finish the unblocked request: %+v, %v", items, err)
	}
	if again, err := restarted.ProcessDueClosures(ctx, 1); err != nil || len(again) != 0 {
		t.Fatalf("completed or manually blocked work ran again: %+v, %v", again, err)
	}
	if fx.sessionAttempts != 2 {
		t.Fatalf("session effect was not retried after an unknown result: %d", fx.sessionAttempts)
	}
	for _, step := range []string{"assets", "sessions", "authorizations", "content"} {
		if fx.effects[step] != 1 {
			t.Fatalf("durable effect for %s occurred %d times", step, fx.effects[step])
		}
	}
	if err = restarted.AccountAccess(ctx, recovering); !errors.Is(err, p.ErrClosed) {
		t.Fatalf("closed account still accessible: %v", err)
	}
	progress, err = restarted.OwnClosure(ctx, oldest)
	if err != nil || progress.Blocker != "assets_pending" || progress.Revision != blockedRev {
		t.Fatalf("automated retries changed a blocked request: %+v, %v", progress, err)
	}
	restarted.ClosureAuthority = closureAuthority{}
	admin := p.Principal{Issuer: issuer, Subject: "admin"}
	result, err := restarted.ProcessClosure(ctx, admin, "manual-retry", blocked.ID)
	if err != nil || result.State != "closed" {
		t.Fatalf("administrator could not resume an unblocked account: %+v, %v", result, err)
	}
}

func TestDueClosureWorkerContinuesPastInvalidRecords(t *testing.T) {
	ctx := context.Background()
	repo := avatarRepository(t)
	service, err := p.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-9 * 24 * time.Hour).Truncate(time.Millisecond)
	service.Now = func() time.Time { return now }
	service.ClosureEffects = &closureEffects{seen: map[string]bool{}, effects: map[string]int{}}
	var healthy p.Closure
	for _, subject := range []string{"invalid-first", "invalid-second", "healthy"} {
		owner := p.Principal{Issuer: "https://issuer.invalid", Subject: subject}
		if _, err := service.Ensure(ctx, owner, "Account"); err != nil {
			t.Fatal(err)
		}
		item, err := service.RequestClosure(ctx, owner, "apply-for-closure", true)
		if err != nil {
			t.Fatal(err)
		}
		if subject == "healthy" {
			healthy = item
		} else if _, err := repo.DB.ExecContext(ctx, `UPDATE profiles.closures SET result='{}' WHERE id=$1`, item.ID); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
	}
	now = healthy.CoolingEndsAt
	if due, err := service.HasDueClosureWork(ctx); err != nil || !due {
		t.Fatalf("unprocessed backlog is hidden: %v, %v", due, err)
	}
	for range 2 {
		if results, err := service.ProcessDueClosures(ctx, 1); !errors.Is(err, p.ErrUnavailable) || len(results) != 0 {
			t.Fatalf("bad records should be reported independently: %+v, %v", results, err)
		}
	}
	if results, err := service.ProcessDueClosures(ctx, 1); err != nil || len(results) != 1 || results[0].ID != healthy.ID || results[0].State != "closed" {
		t.Fatalf("invalid rows starved the healthy record: %+v, %v", results, err)
	}
}
