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

func TestBioCRLFNormalizationAndFieldValidationReasons(t *testing.T) {
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
	name := fmt.Sprintf("profiles_validation_%d", time.Now().UnixNano())
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
	alice := p.Principal{Issuer: "https://issuer.invalid", Subject: "alice"}
	prof, e := s.Ensure(ctx, alice, "Alice")
	if e != nil {
		t.Fatal(e)
	}

	// CRLF normalization: 119 chars + "\r\n" -> 120 Unicode code points after CRLF->LF normalization!
	originalCRLF := strings.Repeat("好", 119) + "\r\n"
	callerCopy := originalCRLF
	updated, e := s.Update(ctx, alice, "crlf-ok", p.Update{Nickname: "Alice", Revision: prof.Revision, Bio: &callerCopy})
	if e != nil {
		t.Fatalf("expected CRLF bio to normalize and succeed, got %v", e)
	}
	if callerCopy != originalCRLF {
		t.Fatal("Update mutated caller input string target")
	}
	expectedLF := strings.Repeat("好", 119) + "\n"
	if updated.Bio != expectedLF {
		t.Fatalf("expected normalized LF bio, got %q", updated.Bio)
	}
	// Replay with equivalent LF bio under same idempotency key succeeds without conflict!
	replay, e := s.Update(ctx, alice, "crlf-ok", p.Update{Nickname: "Alice", Revision: prof.Revision, Bio: &expectedLF})
	if e != nil || replay != updated {
		t.Fatalf("expected equivalent LF replay to match normalized digest: %+v, err=%v", replay, e)
	}

	// Verify fine-grained field + reason codes while preserving errors.Is(err, ErrInput) and Error() == "invalid_profile".
	tooLongBio := strings.Repeat("a", 121)
	loneCRBio := "line1\rline2"
	cases := []struct {
		name   string
		who    p.Principal
		key    string
		in     p.Update
		field  string
		reason string
	}{
		{"bad-principal", p.Principal{}, "k", p.Update{Nickname: "Alice", Revision: 2}, "principal", "principal_invalid"},
		{"bad-key", alice, "", p.Update{Nickname: "Alice", Revision: 2}, "idempotency_key", "idempotency_key_invalid"},
		{"bad-revision", alice, "k", p.Update{Nickname: "Alice", Revision: 0}, "revision", "revision_invalid"},
		{"empty-nickname", alice, "k", p.Update{Nickname: "", Revision: 2}, "nickname", "nickname_empty"},
		{"space-nickname", alice, "k", p.Update{Nickname: " Alice ", Revision: 2}, "nickname", "nickname_whitespace"},
		{"long-nickname", alice, "k", p.Update{Nickname: strings.Repeat("名", 21), Revision: 2}, "nickname", "nickname_too_long"},
		{"zwj-nickname", alice, "k", p.Update{Nickname: "A\u200dB", Revision: 2}, "nickname", "nickname_invalid_char"},
		{"long-bio", alice, "k", p.Update{Nickname: "Alice", Revision: 2, Bio: &tooLongBio}, "bio", "bio_too_long"},
		{"cr-bio", alice, "k", p.Update{Nickname: "Alice", Revision: 2, Bio: &loneCRBio}, "bio", "bio_invalid_char"},
	}
	for _, tc := range cases {
		_, err := s.Update(ctx, tc.who, tc.key, tc.in)
		if !errors.Is(err, p.ErrInput) || err.Error() != "invalid_profile" {
			t.Fatalf("%s: expected ErrInput with code invalid_profile, got %v", tc.name, err)
		}
		gotField, gotReason, ok := p.ValidationDetail(err)
		if !ok || gotField != tc.field || gotReason != tc.reason {
			t.Fatalf("%s: expected (%q, %q), got (%q, %q, %v)", tc.name, tc.field, tc.reason, gotField, gotReason, ok)
		}
	}

	var vis p.Visibility
	if err := json.Unmarshal([]byte(`{"school":true}`), &vis); !errors.Is(err, p.ErrInput) {
		t.Fatal("expected visibility unmarshal error", err)
	} else if f, r, ok := p.ValidationDetail(err); !ok || f != "visibility" || r != "visibility_invalid" {
		t.Fatalf("unexpected visibility validation detail: %q %q %v", f, r, ok)
	}

	var consent p.GroupCardConsent
	if err := json.Unmarshal([]byte(`{"school":true}`), &consent); !errors.Is(err, p.ErrInput) {
		t.Fatal("expected group_card_consent unmarshal error", err)
	} else if f, r, ok := p.ValidationDetail(err); !ok || f != "group_card_consent" || r != "group_card_consent_invalid" {
		t.Fatalf("unexpected group_card_consent validation detail: %q %q %v", f, r, ok)
	}
}
