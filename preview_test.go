package profiles_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/postgres"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOwnProfileDraftPreviewWithoutDatabaseMutation(t *testing.T) {
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
	name := fmt.Sprintf("profiles_preview_%d", time.Now().UnixNano())
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
	facts := &publicFacts{}
	s.Facts = facts
	s.ClosureAuthority = closureAuthority{}
	s.ClosureEffects = &closureEffects{seen: map[string]bool{}, effects: map[string]int{}}
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	issuer := "https://issuer.invalid"
	pendingUser := p.Principal{Issuer: issuer, Subject: "pending_user"}
	alice := p.Principal{Issuer: issuer, Subject: "alice"}
	missing := p.Principal{Issuer: issuer, Subject: "missing"}
	admin := p.Principal{Issuer: issuer, Subject: "admin"}

	if _, e = s.Preview(ctx, missing, p.Preview{}); !errors.Is(e, p.ErrNotFound) {
		t.Fatal("expected ErrNotFound for missing profile", e)
	}

	// 1. User with nickname_required=true can preview both empty-nickname privacy toggles and draft nickname,
	// while public endpoint GET /api/v1/profiles/{id} still returns ErrNotFound and DB remains untouched!
	pendingProf, e := s.Ensure(ctx, pendingUser, "")
	if e != nil || !pendingProf.NicknameRequired {
		t.Fatal(pendingProf, e)
	}
	var eventsBefore int
	if e = owner.QueryRow(`SELECT count(*) FROM profiles.events`).Scan(&eventsBefore); e != nil {
		t.Fatal(e)
	}

	draftName := "DraftNick"
	draftBio := "Line1\r\nLine2"
	withGrade := p.Visibility{School: true, Badge: true, Department: false, Grade: true, Reviews: true, Resources: false}
	prev, e := s.Preview(ctx, pendingUser, p.Preview{Nickname: &draftName, Bio: &draftBio, Visibility: &withGrade})
	if e != nil {
		t.Fatal(e)
	}
	if prev.ID != pendingProf.ID || prev.Nickname != "DraftNick" || prev.Bio != "Line1\nLine2" || prev.FactsState != "available" || len(prev.Affiliations) != 1 || prev.Affiliations[0].Grade != "2026" || prev.Affiliations[0].Department != "" {
		t.Fatalf("unexpected draft preview: %+v", prev)
	}
	// Previewing without typing nickname yet also works while nickname_required=true.
	prevNoNick, e := s.Preview(ctx, pendingUser, p.Preview{Visibility: &withGrade})
	if e != nil || prevNoNick.Nickname != "" || prevNoNick.FactsState != "available" {
		t.Fatalf("expected preview before nickname entry to succeed: %+v, err=%v", prevNoNick, e)
	}
	// Public page is still unpublished and database revision/events did not change!
	if _, e = s.Public(ctx, pendingProf.ID); !errors.Is(e, p.ErrNotFound) {
		t.Fatal("preview leaked unpublished profile to public endpoint", e)
	}
	storedPending, e := s.Get(ctx, pendingUser)
	if e != nil || storedPending != pendingProf {
		t.Fatalf("preview mutated stored profile: got %+v want %+v", storedPending, pendingProf)
	}
	var eventsAfter int
	if e = owner.QueryRow(`SELECT count(*) FROM profiles.events`).Scan(&eventsAfter); e != nil || eventsAfter != eventsBefore {
		t.Fatalf("preview wrote profile events: before=%d after=%d", eventsBefore, eventsAfter)
	}

	// 2. Completed user omitting fields falls back to saved values; passing empty nickname or invalid bio is rejected.
	aliceProf, e := s.Ensure(ctx, alice, "SavedAlice")
	if e != nil {
		t.Fatal(e)
	}
	hidden := p.Visibility{}
	prevHidden, e := s.Preview(ctx, alice, p.Preview{Visibility: &hidden})
	if e != nil || prevHidden.ID != aliceProf.ID || prevHidden.Nickname != "SavedAlice" || prevHidden.FactsState != "hidden" || len(prevHidden.Affiliations) != 0 {
		t.Fatalf("unexpected hidden preview: %+v, err=%v", prevHidden, e)
	}
	empty := ""
	if _, e = s.Preview(ctx, alice, p.Preview{Nickname: &empty}); !errors.Is(e, p.ErrInput) {
		t.Fatal("expected ErrInput when completed user previews empty nickname", e)
	}
	tooLong := strings.Repeat("长", 21)
	if _, e = s.Preview(ctx, alice, p.Preview{Nickname: &tooLong}); !errors.Is(e, p.ErrInput) {
		t.Fatal("expected ErrInput for overlong draft nickname", e)
	}

	invalidBio := "before\u202eafter"
	if _, err := s.Preview(ctx, pendingUser, p.Preview{Bio: &invalidBio}); !errors.Is(err, p.ErrInput) {
		t.Fatalf("invalid draft: %v", err)
	}
	if _, err := s.Update(ctx, pendingUser, "invalid-bio", p.Update{Nickname: draftName, Revision: pendingProf.Revision, Bio: &invalidBio}); !errors.Is(err, p.ErrInput) {
		t.Fatalf("invalid update: %v", err)
	}
	saved, err := s.Update(ctx, pendingUser, "save-bio", p.Update{Nickname: draftName, Revision: pendingProf.Revision, Bio: &draftBio})
	if err != nil || saved.Bio != prev.Bio {
		t.Fatalf("saved bio %q differs from preview %q: %v", saved.Bio, prev.Bio, err)
	}

	// 3. Closed account cannot preview.
	cl, e := s.RequestClosure(ctx, alice, "close-alice", true)
	if e != nil {
		t.Fatal(e)
	}
	now = cl.CoolingEndsAt
	if _, e = s.ProcessClosure(ctx, admin, "proc-alice", cl.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Preview(ctx, alice, p.Preview{}); !errors.Is(e, p.ErrClosed) {
		t.Fatal("expected ErrClosed on closed account preview", e)
	}
}
