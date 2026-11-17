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
	"sync"
	"testing"
	"time"
)

type fakeAvatarMedia struct {
	mu           sync.Mutex
	readyByOwner map[string]p.Principal
	failActivate error
	failRelease  error
	activated    map[string]int
	released     map[string]int
	active       map[string]bool
}

func (m *fakeAvatarMedia) ActivateAvatar(_ context.Context, owner p.Principal, profileID, versionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failActivate != nil {
		return m.failActivate
	}
	expected, ok := m.readyByOwner[versionID]
	if !ok {
		return p.ErrNotFound
	}
	if expected != owner {
		return p.ErrForbidden
	}
	key := profileID + ":" + versionID
	m.activated[key]++
	m.active[key] = true
	return nil
}

func (m *fakeAvatarMedia) ReleaseAvatar(_ context.Context, _ p.Principal, profileID, versionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRelease != nil {
		return m.failRelease
	}
	key := profileID + ":" + versionID
	m.released[key]++
	delete(m.active, key)
	return nil
}

func TestAvatarMediaLifecyclePublicInformationAndClosureRelease(t *testing.T) {
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
	name := fmt.Sprintf("profiles_avatar_%d", time.Now().UnixNano())
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
	bob := p.Principal{Issuer: alice.Issuer, Subject: "bob"}
	admin := p.Principal{Issuer: alice.Issuer, Subject: "admin"}
	profile, e := s.Ensure(ctx, alice, "Alice")
	if e != nil || profile.AvatarVersionID != "" {
		t.Fatal(profile, e)
	}

	v1, v2, foreign, bad := "ver_alice_1", "ver_alice_2", "ver_bob_1", "bad space"
	if _, e = s.Update(ctx, alice, "invalid-avatar", p.Update{Nickname: "Alice", Revision: profile.Revision, AvatarVersionID: &bad}); !errors.Is(e, p.ErrInput) {
		t.Fatal("expected invalid avatar input error", e)
	}
	if _, e = s.Update(ctx, alice, "missing-media", p.Update{Nickname: "Alice", Revision: profile.Revision, AvatarVersionID: &v1}); !errors.Is(e, p.ErrUnavailable) {
		t.Fatal("expected unavailable when AvatarMedia is nil", e)
	}

	media := &fakeAvatarMedia{
		readyByOwner: map[string]p.Principal{v1: alice, v2: alice, foreign: bob},
		activated:    map[string]int{},
		released:     map[string]int{},
		active:       map[string]bool{},
	}
	s.AvatarMedia = media
	s.Information = &informationAuthority{}
	s.ClosureAuthority = closureAuthority{}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	if _, e = s.Update(ctx, alice, "foreign-avatar", p.Update{Nickname: "Alice", Revision: profile.Revision, AvatarVersionID: &foreign}); !errors.Is(e, p.ErrForbidden) {
		t.Fatal("expected forbidden for non-owner avatar version", e)
	}
	profile, e = s.Update(ctx, alice, "set-v1", p.Update{Nickname: "Alice", Revision: profile.Revision, AvatarVersionID: &v1})
	if e != nil || profile.AvatarVersionID != v1 || profile.Revision != 2 {
		t.Fatal(profile, e)
	}
	// Idempotent replay with media temporarily down returns the saved profile without re-activating.
	media.failActivate = p.ErrUnavailable
	replay, e := s.Update(ctx, alice, "set-v1", p.Update{Nickname: "Alice", Revision: 1, AvatarVersionID: &v1})
	if e != nil || replay != profile {
		t.Fatal(replay, e)
	}
	media.failActivate = nil
	// Stale revision fails before touching AvatarMedia.
	if _, e = s.Update(ctx, alice, "stale-v2", p.Update{Nickname: "Alice", Revision: 1, AvatarVersionID: &v2}); !errors.Is(e, p.ErrConflict) {
		t.Fatal("expected conflict", e)
	}
	if media.activated[profile.ID+":"+v2] != 0 {
		t.Fatal("stale revision activated media version")
	}

	pub, e := s.Public(ctx, profile.ID)
	if e != nil || pub.AvatarVersionID != v1 {
		t.Fatal(pub, e)
	}
	acctKey := base64.RawURLEncoding.EncodeToString([]byte(alice.Issuer)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(alice.Subject))
	rows, e := s.Query(ctx, admin, p.Query{Kind: "account", Value: acctKey, Scope: p.Scope{School: "school"}, Purpose: "avatar-audit", Limit: 1, Fields: []string{"avatar_version_id"}})
	if e != nil || len(rows) != 1 || len(rows[0].Fields) != 1 || rows[0].Fields[0].Value != v1 {
		t.Fatal(rows, e)
	}

	// Omitting AvatarVersionID preserves current avatar; replacing with v2 releases v1.
	profile, e = s.Save(ctx, alice, "rename-keep-avatar", profile.Revision, "Alice2")
	if e != nil || profile.AvatarVersionID != v1 {
		t.Fatal(profile, e)
	}
	profile, e = s.Update(ctx, alice, "replace-v2", p.Update{Nickname: "Alice2", Revision: profile.Revision, AvatarVersionID: &v2})
	if e != nil || profile.AvatarVersionID != v2 {
		t.Fatal(profile, e)
	}
	if !media.active[profile.ID+":"+v2] || media.active[profile.ID+":"+v1] || media.released[profile.ID+":"+v1] != 1 {
		t.Fatalf("unexpected media reference state after replace: active=%v released=%v", media.active, media.released)
	}

	// Clearing avatar when ReleaseAvatar transiently fails keeps durable release intent, drained by FlushAvatarReleases.
	media.failRelease = p.ErrUnavailable
	empty := ""
	profile, e = s.Update(ctx, alice, "clear-transient", p.Update{Nickname: "Alice2", Revision: profile.Revision, AvatarVersionID: &empty})
	if e != nil || profile.AvatarVersionID != "" {
		t.Fatal(profile, e)
	}
	if !media.active[profile.ID+":"+v2] {
		t.Fatal("expected v2 still active while release failed")
	}
	media.failRelease = nil
	if e = s.FlushAvatarReleases(ctx, alice); e != nil || media.active[profile.ID+":"+v2] {
		t.Fatal("expected FlushAvatarReleases to release v2", e, media.active)
	}

	// Set v1 again, then verify closure releases avatar and clears DB column.
	profile, e = s.Update(ctx, alice, "restore-v1", p.Update{Nickname: "Alice2", Revision: profile.Revision, AvatarVersionID: &v1})
	if e != nil || profile.AvatarVersionID != v1 {
		t.Fatal(profile, e)
	}
	closure, e := s.RequestClosure(ctx, alice, "close-req", true)
	if e != nil {
		t.Fatal(e)
	}
	now = closure.CoolingEndsAt
	s.ClosureEffects = &closureEffects{seen: map[string]bool{}, effects: map[string]int{}}
	media.failRelease = p.ErrUnavailable
	closure, e = s.ProcessClosure(ctx, admin, "close-approve", closure.ID)
	if e != nil || closure.State != "processing" || closure.Blocker != "content_unavailable" {
		t.Fatal("closure should pause on avatar release failure", closure, e)
	}
	media.failRelease = nil
	closure, e = s.ProcessClosure(ctx, admin, "close-retry", closure.ID)
	if e != nil || closure.State != "closed" {
		t.Fatal(closure, e)
	}
	if media.active[profile.ID+":"+v1] {
		t.Fatal("closed account left active avatar reference")
	}
	var storedAvatar sql.NullString
	if e = owner.QueryRow(`SELECT avatar_version_id FROM profiles.profiles WHERE subject='alice'`).Scan(&storedAvatar); e != nil || storedAvatar.Valid {
		t.Fatal("expected NULL avatar_version_id after closure", storedAvatar, e)
	}
}
