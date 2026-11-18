package profiles_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/postgres"
	"os"
	"testing"
	"time"
)

type countingInformationAuthority struct {
	calls int
}

func (a *countingInformationAuthority) AuthorizeInformation(_ context.Context, principal p.Principal, _ string, scope p.Scope, target string) error {
	a.calls++
	if principal.Subject != "admin" || (target != "" && scope.School != "school") {
		return p.ErrForbidden
	}
	return nil
}

func (a *countingInformationAuthority) AuditInformation(context.Context, p.Principal, string, p.Scope, []string, []string) error {
	return nil
}

func TestAccountStateIsolationClosureErasureAndInformationQuery(t *testing.T) {
	dsn := os.Getenv("STUHELPER_PROFILES_TEST_DSN")
	if dsn == "" {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	name := fmt.Sprintf("profiles_a01_a08_%d", time.Now().UnixNano())
	if _, err = db.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	owner, err := sql.Open("pgx", dsn+" dbname="+name)
	if err != nil {
		t.Fatal(err)
	}
	if err = postgres.Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	role := name + "_runtime"
	if _, err = owner.Exec(`CREATE ROLE ` + role + ` LOGIN;REVOKE ALL ON SCHEMA public FROM PUBLIC;GRANT USAGE ON SCHEMA profiles TO ` + role + `;GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA profiles TO ` + role + `;GRANT USAGE ON ALL SEQUENCES IN SCHEMA profiles TO ` + role + `;`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		owner.Close()
		db.Exec(`DROP DATABASE ` + name + ` WITH(FORCE)`)
		db.Exec(`DROP ROLE ` + role)
	})
	runtime, err := sql.Open("pgx", dsn+" dbname="+name+" user="+role)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })

	repo := &postgres.Repository{DB: runtime}
	if err = repo.CheckRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	svc, err := p.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	auth := &countingInformationAuthority{}
	svc.Information = auth
	svc.ClosureAuthority = closureAuthority{}
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }

	alice := p.Principal{Issuer: "https://issuer.invalid", Subject: "alice"}
	bob := p.Principal{Issuer: alice.Issuer, Subject: "bob"}
	admin := p.Principal{Issuer: alice.Issuer, Subject: "admin"}
	aliceKey := base64.RawURLEncoding.EncodeToString([]byte(alice.Issuer)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(alice.Subject))

	prof, err := svc.Ensure(ctx, alice, "AliceInitial")
	if err != nil {
		t.Fatal(err)
	}
	bobProf, err := svc.Ensure(ctx, bob, "BobInitial")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Save(ctx, bob, "bob-edit-1", bobProf.Revision, "BobSaved"); err != nil {
		t.Fatal(err)
	}

	// A-08.3: Field validation must happen before authorization and reject unknown/empty/duplicate fields.
	auth.calls = 0
	for _, fields := range [][]string{{""}, {"nickname", "nickname"}, {"unknown_field"}} {
		_, err = svc.Query(ctx, admin, p.Query{
			Kind:    "account",
			Value:   aliceKey,
			Scope:   p.Scope{School: "school"},
			Purpose: "support",
			Limit:   10,
			Fields:  fields,
		})
		if !errors.Is(err, p.ErrInput) {
			t.Fatalf("expected ErrInput for fields %v, got %v", fields, err)
		}
		if auth.calls != 0 {
			t.Fatalf("AuthorizeInformation called %d times before invalid fields %v were rejected", auth.calls, fields)
		}
	}

	// A-08.2: Missing event rows must not trigger a NULL scan 503 failure.
	if _, err = owner.Exec(`DELETE FROM profiles.events WHERE subject='alice'`); err != nil {
		t.Fatal(err)
	}
	records, err := svc.Query(ctx, admin, p.Query{
		Kind:    "account",
		Value:   aliceKey,
		Scope:   p.Scope{School: "school"},
		Purpose: "support",
		Limit:   10,
	})
	if err != nil || len(records) != 1 || !records[0].Current || !records[0].ChangedAt.IsZero() {
		t.Fatalf("expected valid query record with zero ChangedAt when events empty, got %v err=%v", records, err)
	}

	// Save sensitive bio, custom visibility, and group card consent (storing plaintext snapshot in profiles.requests).
	bio := "Historical secret bio"
	vis := p.Visibility{School: false, Badge: false, Department: false, Grade: true, Reviews: false, Resources: false}
	consent := p.GroupCardConsent{School: true, Grade: true, Department: true, Name: true, StudentID: true, Gender: true, Nickname: true}
	prof, err = svc.Update(ctx, alice, "alice-edit-1", p.Update{
		Nickname:         "AliceHistorical",
		Revision:         prof.Revision,
		Bio:              &bio,
		Visibility:       &vis,
		GroupCardConsent: &consent,
	})
	if err != nil {
		t.Fatal(err)
	}

	closureReq, err := svc.RequestClosure(ctx, alice, "alice-close-req", true)
	if err != nil {
		t.Fatal(err)
	}
	now = closureReq.CoolingEndsAt
	effects := &closureEffects{blocked: "authorizations", seen: map[string]bool{}, effects: map[string]int{}}
	svc.ClosureEffects = effects
	inProgress, err := svc.ProcessClosure(ctx, admin, "admin-process-1", closureReq.ID)
	if err != nil || inProgress.State != "processing" {
		t.Fatalf("expected processing state, got %v err=%v", inProgress, err)
	}

	// A-01.1 & A-08.1: Processing account must be rejected by Service.Get and marked Current=false in Query.
	if _, err = svc.Get(ctx, alice); !errors.Is(err, p.ErrClosed) {
		t.Fatalf("expected ErrClosed from Service.Get during processing, got %v", err)
	}
	records, err = svc.Query(ctx, admin, p.Query{
		Kind:    "account",
		Value:   aliceKey,
		Scope:   p.Scope{School: "school"},
		Purpose: "support",
		Limit:   10,
	})
	if err != nil || len(records) != 1 || records[0].Current {
		t.Fatalf("expected Current=false for processing account in Query, got %v err=%v", records, err)
	}

	// Finish closure at a distinct timestamp.
	closedAt := now.Add(2 * time.Hour)
	now = closedAt
	effects.blocked = ""
	closed, err := svc.ProcessClosure(ctx, admin, "admin-process-2", closureReq.ID)
	if err != nil || closed.State != "closed" {
		t.Fatalf("expected closed state, got %v err=%v", closed, err)
	}

	// A-01.1: Closed account must be rejected by Service.Get.
	if _, err = svc.Get(ctx, alice); !errors.Is(err, p.ErrClosed) {
		t.Fatalf("expected ErrClosed from Service.Get after closure, got %v", err)
	}

	// A-01.2: Verify database erasure of historical request snapshots, closure_requests, consent, and visibility.
	var aliceReqCount, bobReqCount, aliceClosureReqCount int
	if err = owner.QueryRow(`SELECT count(*) FROM profiles.requests WHERE subject='alice'`).Scan(&aliceReqCount); err != nil {
		t.Fatal(err)
	}
	if err = owner.QueryRow(`SELECT count(*) FROM profiles.requests WHERE subject='bob'`).Scan(&bobReqCount); err != nil {
		t.Fatal(err)
	}
	if err = owner.QueryRow(`SELECT count(*) FROM profiles.closure_requests WHERE actor_subject='alice'`).Scan(&aliceClosureReqCount); err != nil {
		t.Fatal(err)
	}
	if aliceReqCount != 0 || bobReqCount != 1 || aliceClosureReqCount != 0 {
		t.Fatalf("unexpected request rows after closure: aliceReq=%d bobReq=%d aliceClosureReq=%d", aliceReqCount, bobReqCount, aliceClosureReqCount)
	}

	var rawVis, rawConsent []byte
	var storedNick, storedBio, storedState string
	if err = owner.QueryRow(`SELECT nickname,bio,account_state,visibility,group_card_consent FROM profiles.profiles WHERE subject='alice'`).Scan(&storedNick, &storedBio, &storedState, &rawVis, &rawConsent); err != nil {
		t.Fatal(err)
	}
	var gotVis p.Visibility
	var gotConsent p.GroupCardConsent
	if err = json.Unmarshal(rawVis, &gotVis); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(rawConsent, &gotConsent); err != nil {
		t.Fatal(err)
	}
	if storedNick != "" || storedBio != "" || storedState != "closed" || gotVis != p.DefaultVisibility() || gotConsent != (p.GroupCardConsent{}) {
		t.Fatalf("profile state not fully reset on closure: nick=%q bio=%q state=%q vis=%+v consent=%+v", storedNick, storedBio, storedState, gotVis, gotConsent)
	}

	// A-08.1: Closed account in Service.Query must have Current=false and ChangedAt equal to closedAt.
	records, err = svc.Query(ctx, admin, p.Query{
		Kind:    "account",
		Value:   aliceKey,
		Scope:   p.Scope{School: "school"},
		Purpose: "support",
		Limit:   10,
	})
	if err != nil || len(records) != 1 {
		t.Fatalf("expected 1 query record for closed account, got %v err=%v", records, err)
	}
	if records[0].Current || !records[0].ChangedAt.Equal(closedAt) {
		t.Fatalf("expected Current=false and ChangedAt=%v, got Current=%v ChangedAt=%v", closedAt, records[0].Current, records[0].ChangedAt)
	}
}
