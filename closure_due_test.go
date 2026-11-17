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
