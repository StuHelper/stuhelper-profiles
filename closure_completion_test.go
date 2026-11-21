package profiles_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/postgres"
)

func TestClosureCompletionSurvivesCleanupAndRestart(t *testing.T) {
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
	name := fmt.Sprintf("profiles_completion_%d", time.Now().UnixNano())
	if _, err = db.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	role := name + "_runtime"
	t.Cleanup(func() {
		db.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`)
		db.Exec(`DROP ROLE IF EXISTS ` + role)
	})
	owner, err := sql.Open("pgx", dsn+" dbname="+name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	if err = postgres.Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(runtimeRoleSQL(role)); err != nil {
		t.Fatal(err)
	}
	runtime, err := sql.Open("pgx", dsn+" dbname="+name+" user="+role)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	repo := &postgres.Repository{DB: runtime}
	service, err := p.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	service.ClosureAuthority = closureAuthority{}
	service.ClosureEffects = &closureEffects{seen: map[string]bool{}, effects: map[string]int{}}
	now := time.Now().UTC().Truncate(time.Microsecond)
	completedAt := now.AddDate(-1, -1, 0)
	businessTime := completedAt.Add(-8 * 24 * time.Hour)
	service.Now = func() time.Time { return businessTime }

	const issuer = "https://completion.invalid"
	admin := p.Principal{Issuer: issuer, Subject: "admin"}
	openClosure := func(subject string) (p.Principal, string) {
		t.Helper()
		businessTime = completedAt.Add(-8 * 24 * time.Hour)
		who := p.Principal{Issuer: issuer, Subject: subject}
		if _, err := service.Ensure(ctx, who, "User"); err != nil {
			t.Fatal(err)
		}
		closure, err := service.RequestClosure(ctx, who, "request", true)
		if err != nil {
			t.Fatal(err)
		}
		businessTime = completedAt
		return who, closure.ID
	}
	_, disabledID := openClosure("disabled")
	if value, err := service.ProcessClosure(ctx, admin, "disabled", disabledID); err != nil || value.State != "closed" {
		t.Fatal(value, err)
	}
	if _, err := owner.Exec(`REVOKE EXECUTE ON FUNCTION profiles.ack_closure_completion(text) FROM ` + role); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnableClosureCompletions(ctx); !errors.Is(err, p.ErrUnavailable) {
		t.Fatal("enabled without acknowledgment grant", err)
	}
	if _, err := owner.Exec(`GRANT EXECUTE ON FUNCTION profiles.ack_closure_completion(text) TO ` + role); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnableClosureCompletions(ctx); err != nil {
		t.Fatal(err)
	}
	if items, err := repo.PendingClosureCompletions(ctx, "", 100); err != nil || len(items) != 0 {
		t.Fatal("standalone closure captured an event", items, err)
	}

	// Force a failure at the enqueue step and verify that the completed progress
	// rolls back with the event, then retry using the original operation key.
	if _, err := owner.Exec(`ALTER TABLE profiles.closure_completions ADD CONSTRAINT fixture_reject CHECK (subject <> 'retry')`); err != nil {
		t.Fatal(err)
	}
	retryOwner, retryID := openClosure("retry")
	if _, err := service.ProcessClosure(ctx, admin, "retry", retryID); !errors.Is(err, p.ErrUnavailable) {
		t.Fatal("enqueue failure did not propagate", err)
	}
	first, err := service.OwnClosure(ctx, retryOwner)
	if err != nil || first.State != "processing" || len(first.CompletedSteps) != 3 || first.CompletedAt != nil {
		t.Fatal("partial closure commit", first, err)
	}
	if items, err := repo.PendingClosureCompletions(ctx, "", 1); err != nil || len(items) != 0 {
		t.Fatal("enqueue failure wrote a completion", items, err)
	}
	if _, err := owner.Exec(`ALTER TABLE profiles.closure_completions DROP CONSTRAINT fixture_reject`); err != nil {
		t.Fatal(err)
	}
	if result, err := service.ProcessClosure(ctx, admin, "retry", retryID); err != nil || result.State != "closed" {
		t.Fatal(result, err)
	}
	peerOwner, peerID := openClosure("peer")
	if result, err := service.ProcessClosure(ctx, admin, "peer", peerID); err != nil || result.State != "closed" {
		t.Fatal(result, err)
	}
	if _, err := runtime.Exec(`DELETE FROM profiles.closure_completions WHERE closure_id=$1`, retryID); err == nil {
		t.Fatal("runtime could directly delete a completion")
	}

	// Simulate old terminal histories whose independent 12-month retention
	// elapsed while the host was unavailable, without changing the DB clock.
	if _, err := owner.Exec(`
 ALTER TABLE profiles.closures DISABLE TRIGGER profiles_closures_guard_row;
 UPDATE profiles.closures SET retention_terminal_at=updated_at WHERE state='closed';
 ALTER TABLE profiles.closures ENABLE TRIGGER profiles_closures_guard_row;`); err != nil {
		t.Fatal(err)
	}
	if result, err := service.CleanupExpired(ctx, now, 100); err != nil || result.ClosuresDeleted != 3 {
		t.Fatal("pending completions affected cleanup", result, err)
	}

	restarted := &postgres.Repository{DB: runtime}
	if err := postgres.Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := restarted.EnableClosureCompletions(ctx); err != nil {
		t.Fatal(err)
	}
	left := map[string]p.Principal{retryID: retryOwner, peerID: peerOwner}
	after := ""
	for range 2 {
		items, err := restarted.PendingClosureCompletions(ctx, after, 1)
		if err != nil || len(items) != 1 {
			t.Fatal(items, err)
		}
		item := items[0]
		if item.ClosureID <= after || item.Owner != left[item.ClosureID] || !item.CompletedAt.Equal(completedAt) {
			t.Fatal("wrong archived fact or cursor", item)
		}
		after = item.ClosureID
		delete(left, after)
	}
	if items, err := restarted.PendingClosureCompletions(ctx, after, 1); err != nil || len(items) != 0 {
		t.Fatal("unexpected next page", items, err)
	}
	for _, id := range []string{retryID, retryID, peerID} {
		if err := restarted.AcknowledgeClosureCompletion(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if items, err := restarted.PendingClosureCompletions(ctx, "", 1); err != nil || len(items) != 0 {
		t.Fatal("acknowledgment was not durable", items, err)
	}
}
