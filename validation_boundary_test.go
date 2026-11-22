package profiles_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"github.com/StuHelper/stuhelper-profiles/postgres"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

type recordingCardProvider struct {
	lastFields []string
	values     map[string]p.CardField
}

func (r *recordingCardProvider) AuthorizeGroupCard(_ context.Context, who p.Principal, school string) error {
	if who.Subject != "service" || school != "school" {
		return p.ErrForbidden
	}
	return nil
}

func (r *recordingCardProvider) GroupCardValues(_ context.Context, _ p.Principal, _, _ string, fields []string) (map[string]p.CardField, error) {
	r.lastFields = slices.Clone(fields)
	return r.values, nil
}

func TestInputEncodingUUIDIdempotencyGroupCardAndBioSafety(t *testing.T) {
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

	name := fmt.Sprintf("profiles_a04_a10_%d", time.Now().UnixNano())
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
	cards := &recordingCardProvider{
		values: map[string]p.CardField{"name": {Available: true, Value: "Alice"}},
	}
	svc.GroupCards = cards

	issuer := "https://issuer.invalid"
	alice := p.Principal{Issuer: issuer, Subject: "alice"}
	admin := p.Principal{Issuer: issuer, Subject: "admin"}
	servicePrincipal := p.Principal{Issuer: issuer, Subject: "service"}
	prof, err := svc.Ensure(ctx, alice, "Alice")
	if err != nil {
		t.Fatal(err)
	}

	// A-04.1: Malformed 36-byte public IDs with invalid UTF-8 or NUL must return ErrNotFound (404), never 503.
	for _, badID := range []string{
		"\xff1234567-89ab-cdef-0123-456789abcdef",
		"\x001234567-89ab-cdef-0123-456789abcdef",
		"A0000000-0000-4000-8000-000000000000",
	} {
		if _, err = svc.Public(ctx, badID); !errors.Is(err, p.ErrNotFound) {
			t.Fatalf("Public(%q) expected ErrNotFound, got %v", badID, err)
		}
	}

	// A-04.4: AdminClosure must reject non-UUID with ErrInput (400) after checking admin permission.
	if _, err = svc.AdminClosure(ctx, alice, "not-a-uuid"); !errors.Is(err, p.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for non-admin AdminClosure, got %v", err)
	}
	for _, badID := range []string{"not-a-uuid", "\xff1234567-89ab-cdef-0123-456789abcdef", "A0000000-0000-4000-8000-000000000000"} {
		if _, err = svc.AdminClosure(ctx, admin, badID); !errors.Is(err, p.ErrInput) {
			t.Fatalf("AdminClosure(%q) expected ErrInput, got %v", badID, err)
		}
	}

	// A-04.2: Idempotency keys must be 1..128 printable ASCII (0x21..0x7E).
	for _, badKey := range []string{"", "key\x00nul", "key with space", "中文幂等键", "bad\xffkey"} {
		if _, err = svc.Save(ctx, alice, badKey, prof.Revision, "Alice2"); !errors.Is(err, p.ErrInput) {
			t.Fatalf("Save with key %q expected ErrInput, got %v", badKey, err)
		}
		if _, err = svc.RequestClosure(ctx, alice, badKey, true); !errors.Is(err, p.ErrInput) {
			t.Fatalf("RequestClosure with key %q expected ErrInput, got %v", badKey, err)
		}
	}

	// A-04.3: Base64URL account keys decoding to NUL, invalid UTF-8, or >256-byte Subject must return ErrInput, never 503.
	overlongSubject := base64.RawURLEncoding.EncodeToString([]byte(issuer)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("a", 257)))
	for _, badOwner := range []string{"AA:AA", "_w:_w", overlongSubject} {
		if _, err = svc.GroupCardFields(ctx, servicePrincipal, badOwner, "school", p.GroupCardPurpose); !errors.Is(err, p.ErrInput) {
			t.Fatalf("GroupCardFields(%q) expected ErrInput, got %v", badOwner, err)
		}
		if _, err = svc.Query(ctx, admin, p.Query{
			Kind:    "account",
			Value:   badOwner,
			Scope:   p.Scope{School: "school"},
			Purpose: "support",
			Limit:   10,
		}); !errors.Is(err, p.ErrInput) {
			t.Fatalf("Query(%q) expected ErrInput, got %v", badOwner, err)
		}
	}

	// A-10.2: Bio rejects Bidi overrides while preserving Arabic RTL script, newline, and ZWJ Emoji.
	for _, spoof := range []string{"hello\u202eadmin", "spoof\u2066text\u2069"} {
		if _, err = svc.Update(ctx, alice, "bidi-bio", p.Update{Nickname: "Alice", Revision: prof.Revision, Bio: &spoof}); !errors.Is(err, p.ErrInput) {
			t.Fatalf("Update Bio %q expected ErrInput, got %v", spoof, err)
		}
	}
	validBio := "طالب علوم الحاسوب\n👨‍👩‍👧"
	allConsent := p.GroupCardConsent{School: true, Grade: true, Department: true, Name: true, StudentID: true, Gender: true, Nickname: true}
	prof, err = svc.Update(ctx, alice, "valid-bio-consent", p.Update{
		Nickname:         "Alice",
		Revision:         prof.Revision,
		Bio:              &validBio,
		GroupCardConsent: &allConsent,
	})
	if err != nil || prof.Bio != validBio {
		t.Fatalf("expected Arabic + ZWJ bio to succeed, got prof=%+v err=%v", prof, err)
	}

	// A-10.1: GroupCardFields rejects blank/control school, keeps deterministic field order, and rejects unsafe upstream values.
	aliceKey := base64.RawURLEncoding.EncodeToString([]byte(alice.Issuer)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(alice.Subject))
	for _, badSchool := range []string{"   ", "school\n", "school\x00"} {
		if _, err = svc.GroupCardFields(ctx, servicePrincipal, aliceKey, badSchool, p.GroupCardPurpose); !errors.Is(err, p.ErrInput) {
			t.Fatalf("GroupCardFields school %q expected ErrInput, got %v", badSchool, err)
		}
	}
	expectedOrder := []string{"school", "grade", "department", "name", "student_id", "gender"}
	for range 20 {
		if _, err = svc.GroupCardFields(ctx, servicePrincipal, aliceKey, "school", p.GroupCardPurpose); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cards.lastFields, expectedOrder) {
			t.Fatalf("non-deterministic GroupCardFields request order: got %v, want %v", cards.lastFields, expectedOrder)
		}
	}
	for _, unsafeVal := range []string{"", "张三\n管理员", "bad\x00val", "spoof\u202eval", strings.Repeat("院", 201)} {
		cards.values = map[string]p.CardField{"name": {Available: true, Value: unsafeVal}}
		if _, err = svc.GroupCardFields(ctx, servicePrincipal, aliceKey, "school", p.GroupCardPurpose); !errors.Is(err, p.ErrUnavailable) {
			t.Fatalf("GroupCardFields upstream value %q expected ErrUnavailable, got %v", unsafeVal, err)
		}
	}
}
