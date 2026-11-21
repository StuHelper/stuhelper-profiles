package profiles_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/postgres"
	"os"
	"strings"
	"testing"
	"time"
)

type separatedClosureAuthority struct{}

func (separatedClosureAuthority) AuthorizeClosure(_ context.Context, who p.Principal) error {
	if who.Subject != "admin" {
		return p.ErrForbidden
	}
	return nil
}

func (separatedClosureAuthority) AuthorizeClosureAudit(_ context.Context, who p.Principal) error {
	if who.Subject != "admin" && who.Subject != "auditor" {
		return p.ErrForbidden
	}
	return nil
}

func TestClosureAdminFilteringChronologicalCursorAndAuditEvents(t *testing.T) {
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
	name := fmt.Sprintf("closure_admin_%d", time.Now().UnixNano())
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
	s.ClosureAuthority = separatedClosureAuthority{}
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	issuer := "https://issuer.invalid"
	admin := p.Principal{Issuer: issuer, Subject: "admin"}
	auditor := p.Principal{Issuer: issuer, Subject: "auditor"}
	u1 := p.Principal{Issuer: issuer, Subject: "secret_subject_u1"}
	u2 := p.Principal{Issuer: issuer, Subject: "secret_subject_u2"}
	u3 := p.Principal{Issuer: issuer, Subject: "secret_subject_u3"}
	u4 := p.Principal{Issuer: issuer, Subject: "secret_subject_u4"}
	for i, u := range []p.Principal{u1, u2, u3, u4} {
		if _, e = s.Ensure(ctx, u, fmt.Sprintf("User%d", i+1)); e != nil {
			t.Fatal(e)
		}
	}

	// u1: requested at t0, cancelled 1h later.
	c1, e := s.RequestClosure(ctx, u1, "req-u1", true)
	if e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Hour)
	c1, e = s.CancelClosure(ctx, u1, "cancel-u1", c1.ID)
	if e != nil || c1.State != "cancelled" {
		t.Fatal(c1, e)
	}

	// u2: requested at t0+2h, later blocked at authorizations_unavailable.
	now = now.Add(time.Hour)
	c2, e := s.RequestClosure(ctx, u2, "req-u2", true)
	if e != nil {
		t.Fatal(e)
	}

	// u3: requested at t0+3h, cooling deadline reached cleanly.
	now = now.Add(time.Hour)
	c3, e := s.RequestClosure(ctx, u3, "req-u3", true)
	if e != nil {
		t.Fatal(e)
	}

	// Advance time to c2's deadline and process c2 so it blocks on authorizations_unavailable.
	now = c2.CoolingEndsAt
	effects := &closureEffects{blocked: "authorizations", seen: map[string]bool{}, effects: map[string]int{}}
	s.ClosureEffects = effects
	c2, e = s.ProcessClosure(ctx, admin, "proc-u2", c2.ID)
	if e != nil || c2.State != "processing" || c2.Blocker != "authorizations_unavailable" {
		t.Fatal(c2, e)
	}

	// u4: requested later, still inside cooling window even when c3 becomes due.
	now = c3.CoolingEndsAt.Add(-time.Nanosecond)
	c4, e := s.RequestClosure(ctx, u4, "req-u4", true)
	if e != nil {
		t.Fatal(e)
	}

	// At 1ns before c3's CoolingEndsAt, actionable returns only c2 (processing), not c3 or c4!
	actEarly, e := s.QueryClosures(ctx, admin, p.ClosureFilter{Actionable: true, Limit: 10})
	if e != nil || len(actEarly.Items) != 1 || actEarly.Items[0].ID != c2.ID || actEarly.NextCursor != "" {
		t.Fatalf("unexpected actionable closures before c3 deadline: %+v, err=%v", actEarly, e)
	}

	// Advance 1ns to c3.CoolingEndsAt: actionable now includes [c2, c3] in chronological order!
	now = c3.CoolingEndsAt
	p1, e := s.QueryClosures(ctx, admin, p.ClosureFilter{Actionable: true, Limit: 1})
	if e != nil || len(p1.Items) != 1 || p1.Items[0].ID != c2.ID || p1.NextCursor != c2.ID {
		t.Fatalf("unexpected page 1: %+v, err=%v", p1, e)
	}
	p2, e := s.QueryClosures(ctx, admin, p.ClosureFilter{Actionable: true, After: p1.NextCursor, Limit: 1})
	if e != nil || len(p2.Items) != 1 || p2.Items[0].ID != c3.ID || p2.NextCursor != "" {
		t.Fatalf("unexpected page 2 (should have empty next_cursor on exact end): %+v, err=%v", p2, e)
	}

	// Filter by state and blocker.
	byBlocker, e := s.QueryClosures(ctx, admin, p.ClosureFilter{Blocker: "authorizations_unavailable", Limit: 10})
	if e != nil || len(byBlocker.Items) != 1 || byBlocker.Items[0].ID != c2.ID {
		t.Fatal(byBlocker, e)
	}
	byState, e := s.QueryClosures(ctx, admin, p.ClosureFilter{State: "cancelled", Limit: 10})
	if e != nil || len(byState.Items) != 1 || byState.Items[0].ID != c1.ID {
		t.Fatal(byState, e)
	}

	// Reject uppercase UUID, unknown UUID cursor, and invalid state/blocker.
	for _, bad := range []p.ClosureFilter{
		{After: strings.ToUpper(c1.ID), Limit: 10},
		{After: "00000000-0000-4000-8000-000000000000", Limit: 10},
		{State: "unknown", Limit: 10},
		{Blocker: "unknown", Limit: 10},
	} {
		if _, e = s.QueryClosures(ctx, admin, bad); !errors.Is(e, p.ErrInput) {
			t.Fatalf("expected ErrInput for %+v, got %v", bad, e)
		}
	}

	// Auditor can read closure audit events via AuthorizeClosureAudit, but cannot mutate via ProcessClosure!
	if _, e = s.ProcessClosure(ctx, auditor, "forbid-mutate", c3.ID); !errors.Is(e, p.ErrForbidden) {
		t.Fatal("auditor should not be allowed to process closures", e)
	}
	if _, e = s.ListClosureEvents(ctx, u1, p.ClosureEventFilter{Limit: 10}); !errors.Is(e, p.ErrForbidden) {
		t.Fatal("ordinary user should not read admin closure events", e)
	}

	var allEvents []p.ClosureAuditEvent
	cursor := ""
	for {
		evPage, e := s.ListClosureEvents(ctx, auditor, p.ClosureEventFilter{After: cursor, Limit: 3})
		if e != nil {
			t.Fatal(e)
		}
		allEvents = append(allEvents, evPage.Items...)
		if evPage.NextCursor == "" {
			break
		}
		cursor = evPage.NextCursor
	}
	if len(allEvents) != 9 {
		t.Fatalf("expected 9 closure events across requests/cancel/approve/steps, got %d: %+v", len(allEvents), allEvents)
	}
	// Verify descending chronological order and no leaked target SSO subject.
	for i := 1; i < len(allEvents); i++ {
		prev, cur := allEvents[i-1], allEvents[i]
		if prev.CreatedAt.Before(cur.CreatedAt) || (prev.CreatedAt.Equal(cur.CreatedAt) && prev.ID <= cur.ID) {
			t.Fatalf("audit events out of descending order at %d: %+v vs %+v", i, prev, cur)
		}
	}
	c2Events, e := s.ListClosureEvents(ctx, auditor, p.ClosureEventFilter{ClosureID: c2.ID, Limit: 10})
	if e != nil || len(c2Events.Items) != 5 {
		t.Fatal(c2Events, e)
	}
	if c2Events.Items[0].Action != "authorizations" || c2Events.Items[0].Blocker != "authorizations_unavailable" || c2Events.Items[0].ProfileID != c2.ProfileID {
		t.Fatalf("unexpected latest c2 event: %+v", c2Events.Items[0])
	}
	if c2Events.Items[3].Action != "approve" || c2Events.Items[3].Actor != admin || c2Events.Items[4].Action != "request" || c2Events.Items[4].Actor != u2 {
		t.Fatalf("unexpected actor attribution on c2 events: %+v", c2Events.Items)
	}
	serialized, _ := json.Marshal(allEvents)
	if strings.Contains(string(serialized), "secret_subject_u2") {
		t.Fatalf("audit event leaked target SSO subject for admin-initiated step: %s", string(serialized))
	}
	_ = c4
}
