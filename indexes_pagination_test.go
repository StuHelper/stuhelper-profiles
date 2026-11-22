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
	"strings"
	"testing"
	"time"
)

func explainPlan(t *testing.T, tx *sql.Tx, query string, args ...any) string {
	t.Helper()
	rows, err := tx.Query("EXPLAIN (COSTS OFF) "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func TestIndexesChronologicalClosurePaginationAndErrorMapping(t *testing.T) {
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

	name := fmt.Sprintf("profiles_a03_a07_%d", time.Now().UnixNano())
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
	if _, err = owner.Exec(runtimeRoleSQL(role)); err != nil {
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
	svc.ClosureAuthority = closureAuthority{}
	svc.Information = &informationAuthority{}

	// A-03: Verify EXPLAIN plans use indexes (never Seq Scan) when enable_seqscan = off.
	tx, err := owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	for label, plan := range map[string]string{
		"events_changed_at": explainPlan(t, tx, `SELECT max(created_at) FROM profiles.events WHERE issuer=$1 AND subject=$2`, "iss", "sub"),
		"own_closure":       explainPlan(t, tx, `SELECT issuer,subject,result FROM profiles.closures WHERE issuer=$1 AND subject=$2 ORDER BY created_at DESC,id DESC LIMIT 1`, "iss", "sub"),
		"closures_list":     explainPlan(t, tx, `SELECT issuer,subject,result FROM profiles.closures WHERE (created_at,id)>($1,$2) ORDER BY created_at ASC,id ASC LIMIT 10`, time.Now().UTC(), "00000000-0000-4000-8000-000000000000"),
		"closure_events":    explainPlan(t, tx, `SELECT id FROM profiles.closure_events WHERE closure_id=$1 ORDER BY id`, "cid"),
	} {
		if strings.Contains(plan, "Seq Scan") {
			t.Fatalf("%s fell back to Seq Scan:\n%s", label, plan)
		}
	}
	_ = tx.Rollback()

	// A-07.1: Chronological pagination must not miss new arrivals whose random UUIDv4 sorts before an older cursor.
	issuer := "https://issuer.invalid"
	admin := p.Principal{Issuer: issuer, Subject: "admin"}
	t1 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	firstID := "f0000000-0000-4000-8000-000000000001"
	secondID := "10000000-0000-4000-8000-000000000002"
	insertClosure := func(subject, id string, at time.Time) {
		prof, e := svc.Ensure(ctx, p.Principal{Issuer: issuer, Subject: subject}, subject)
		if e != nil {
			t.Fatal(e)
		}
		c := p.Closure{
			ID:             id,
			ProfileID:      prof.ID,
			State:          "cooling",
			Revision:       1,
			RequestedAt:    at,
			CoolingEndsAt:  at.Add(7 * 24 * time.Hour),
			CompletedSteps: []string{},
			PointsPolicy:   p.PointsPolicy,
		}
		raw, _ := json.Marshal(c)
		if _, e = owner.Exec(`INSERT INTO profiles.closures(id,issuer,subject,state,result,created_at) VALUES($1,$2,$3,'cooling',$4,$5)`, id, issuer, subject, raw, at); e != nil {
			t.Fatal(e)
		}
	}
	insertClosure("user1", firstID, t1)
	page1, err := svc.ListClosures(ctx, admin, "", 1)
	if err != nil || len(page1) != 1 || page1[0].ID != firstID {
		t.Fatalf("expected first closure %s on page 1, got %v err=%v", firstID, page1, err)
	}
	// New arrival gets a smaller UUIDv4 after page 1 was read.
	insertClosure("user2", secondID, t2)
	page2, err := svc.ListClosures(ctx, admin, page1[0].ID, 1)
	if err != nil || len(page2) != 1 || page2[0].ID != secondID {
		t.Fatalf("new arrival %s missed after cursor %s: got %v err=%v", secondID, page1[0].ID, page2, err)
	}

	// A-07.2: Uppercase UUID cursors must be rejected with ErrInput in both ListClosures and Service.Query.
	upperCursor := "A0000000-0000-4000-8000-000000000000"
	if _, err = svc.ListClosures(ctx, admin, upperCursor, 10); !errors.Is(err, p.ErrInput) {
		t.Fatalf("expected ErrInput for uppercase ListClosures cursor, got %v", err)
	}
	u1Key := base64.RawURLEncoding.EncodeToString([]byte(issuer)) + ":" + base64.RawURLEncoding.EncodeToString([]byte("user1"))
	if _, err = svc.Query(ctx, admin, p.Query{
		Kind:    "account",
		Value:   u1Key,
		Scope:   p.Scope{School: "school"},
		Purpose: "support",
		Limit:   10,
		After:   upperCursor,
	}); !errors.Is(err, p.ErrInput) {
		t.Fatalf("expected ErrInput for uppercase Query cursor, got %v", err)
	}

	// A-07.3: AdvanceClosure on missing ID maps sql.ErrNoRows to ErrNotFound; cancelled query maps to ErrUnavailable.
	if _, err = repo.AdvanceClosure(ctx, "00000000-0000-4000-8000-000000000099", 0, "", nil, t2); !errors.Is(err, p.ErrNotFound) {
		t.Fatalf("expected ErrNotFound from AdvanceClosure on unknown ID, got %v", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if items, err := repo.Closures(canceledCtx, "", 10); !errors.Is(err, p.ErrUnavailable) || items != nil {
		t.Fatalf("expected (nil, ErrUnavailable) on failed Closures query, got items=%v err=%v", items, err)
	}
}
