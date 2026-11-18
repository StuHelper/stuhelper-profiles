package profiles_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	p "github.com/StuHelper/stuhelper-profiles"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryRepo struct {
	mu          sync.Mutex
	readyErr    error
	profiles    map[p.Principal]p.Profile
	states      map[p.Principal]string
	closures    map[string]p.ClosureRecord
	changedAt   time.Time
	closureReqs map[string]p.Closure
}

func newMemoryRepo() *memoryRepo {
	return &memoryRepo{
		profiles:    map[p.Principal]p.Profile{},
		states:      map[p.Principal]string{},
		closures:    map[string]p.ClosureRecord{},
		changedAt:   time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
		closureReqs: map[string]p.Closure{},
	}
}

func (m *memoryRepo) Ready(context.Context) error { return m.readyErr }

func (m *memoryRepo) Get(_ context.Context, who p.Principal) (p.Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.profiles[who]
	if !ok {
		return p.Profile{}, p.ErrNotFound
	}
	return v, nil
}

func (m *memoryRepo) Public(_ context.Context, id string) (p.PublicRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for owner, v := range m.profiles {
		if v.ID == id && v.Nickname != "" && m.states[owner] == "active" {
			return p.PublicRecord{Owner: owner, Profile: v}, nil
		}
	}
	return p.PublicRecord{}, p.ErrNotFound
}

func (m *memoryRepo) Ensure(_ context.Context, who p.Principal, name string) (p.Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, exists := m.states[who]; exists {
		if st != "active" {
			return p.Profile{}, p.ErrClosed
		}
		return m.profiles[who], nil
	}
	v := p.Profile{
		ID:               fmt.Sprintf("00000000-0000-4000-8000-%012d", len(m.profiles)+1),
		Nickname:         name,
		NicknameRequired: name == "",
		Visibility:       p.DefaultVisibility(),
		Revision:         1,
	}
	m.profiles[who] = v
	m.states[who] = "active"
	return v, nil
}

func (m *memoryRepo) Update(_ context.Context, who p.Principal, _ string, in p.Update) (p.Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.profiles[who]
	if !ok {
		return p.Profile{}, p.ErrNotFound
	}
	if m.states[who] != "active" {
		return cur, p.ErrClosed
	}
	if cur.Revision != in.Revision {
		return cur, p.ErrConflict
	}
	cur.Nickname = in.Nickname
	cur.NicknameRequired = false
	cur.Revision++
	if in.Bio != nil {
		cur.Bio = *in.Bio
	}
	if in.Visibility != nil {
		cur.Visibility = *in.Visibility
	}
	if in.GroupCardConsent != nil {
		cur.GroupCardConsent = *in.GroupCardConsent
	}
	m.profiles[who] = cur
	return cur, nil
}

func (m *memoryRepo) InformationChangedAt(_ context.Context, _ p.Principal) (time.Time, error) {
	return m.changedAt, nil
}

func (m *memoryRepo) AccountState(_ context.Context, who p.Principal) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[who]
	if !ok {
		return "", p.ErrNotFound
	}
	return st, nil
}

func (m *memoryRepo) CurrentClosure(_ context.Context, who p.Principal) (p.Closure, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.closures {
		if rec.Owner == who {
			return rec.Closure, nil
		}
	}
	return p.Closure{}, p.ErrNotFound
}

func (m *memoryRepo) Closure(_ context.Context, id string) (p.ClosureRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.closures[id]
	if !ok {
		return p.ClosureRecord{}, p.ErrNotFound
	}
	return rec, nil
}

func (m *memoryRepo) Closures(_ context.Context, after string, limit int) ([]p.Closure, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id := range m.closures {
		if id > after {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]p.Closure, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.closures[id].Closure)
	}
	return out, nil
}

func (m *memoryRepo) ChangeClosure(_ context.Context, actor p.Principal, key, action, id string, now time.Time) (p.Closure, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rk := actor.Issuer + "|" + actor.Subject + "|" + key
	if cached, ok := m.closureReqs[rk]; ok {
		if action == "approve" {
			return m.closures[id].Closure, nil
		}
		return cached, nil
	}
	switch action {
	case "request":
		if m.states[actor] != "active" {
			return p.Closure{}, p.ErrClosed
		}
		cid := fmt.Sprintf("11111111-1111-4000-8000-%012d", len(m.closures)+1)
		cl := p.Closure{
			ID:             cid,
			ProfileID:      m.profiles[actor].ID,
			State:          "cooling",
			Revision:       1,
			RequestedAt:    now,
			CoolingEndsAt:  now.Add(7 * 24 * time.Hour),
			CompletedSteps: []string{},
			PointsPolicy:   p.PointsPolicy,
		}
		m.closures[cid] = p.ClosureRecord{Owner: actor, Closure: cl}
		m.closureReqs[rk] = cl
		return cl, nil
	case "cancel":
		rec, ok := m.closures[id]
		if !ok || rec.Owner != actor {
			return p.Closure{}, p.ErrNotFound
		}
		if rec.Closure.State != "cooling" || !now.Before(rec.Closure.CoolingEndsAt) {
			return p.Closure{}, p.ErrConflict
		}
		rec.Closure.State = "cancelled"
		rec.Closure.Revision++
		m.closures[id] = rec
		m.closureReqs[rk] = rec.Closure
		return rec.Closure, nil
	case "approve":
		rec, ok := m.closures[id]
		if !ok {
			return p.Closure{}, p.ErrNotFound
		}
		if rec.Closure.State == "cancelled" || now.Before(rec.Closure.CoolingEndsAt) {
			return p.Closure{}, p.ErrConflict
		}
		if rec.Closure.State == "cooling" {
			rec.Closure.State = "awaiting_assets"
			rec.Closure.Revision++
			m.closures[id] = rec
		}
		m.closureReqs[rk] = rec.Closure
		return rec.Closure, nil
	default:
		return p.Closure{}, p.ErrInput
	}
}

func (m *memoryRepo) AdvanceClosure(_ context.Context, id string, step int, blocker string, skipped []string, now time.Time) (p.Closure, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.closures[id]
	if !ok {
		return p.Closure{}, p.ErrNotFound
	}
	steps := []string{"assets", "sessions", "authorizations", "content"}
	if len(rec.Closure.CompletedSteps) != step || rec.Closure.State == "closed" {
		return rec.Closure, nil
	}
	for _, mod := range skipped {
		if !slices.Contains(rec.Closure.SkippedModules, mod) {
			rec.Closure.SkippedModules = append(rec.Closure.SkippedModules, mod)
		}
	}
	rec.Closure.Blocker = blocker
	rec.Closure.Revision++
	if blocker == "" {
		rec.Closure.CompletedSteps = append(rec.Closure.CompletedSteps, steps[step])
		rec.Closure.State = "processing"
		if len(rec.Closure.CompletedSteps) == 4 {
			rec.Closure.State = "closed"
			rec.Closure.CompletedAt = &now
		}
		m.states[rec.Owner] = rec.Closure.State
	}
	m.closures[id] = rec
	return rec.Closure, nil
}

type unitFacts struct {
	facts []p.Affiliation
	err   error
}

func (u unitFacts) ProfileFacts(context.Context, p.Principal, p.Visibility) ([]p.Affiliation, error) {
	return u.facts, u.err
}

type unitCardProvider struct {
	authErr error
	valErr  error
	values  map[string]p.CardField
}

func (u unitCardProvider) AuthorizeGroupCard(context.Context, p.Principal, string) error {
	return u.authErr
}
func (u unitCardProvider) GroupCardValues(context.Context, p.Principal, string, string, []string) (map[string]p.CardField, error) {
	return u.values, u.valErr
}

func TestServiceUnitInputBoundariesPortsAndConcurrencyWithoutDatabase(t *testing.T) {
	if _, err := p.New(nil); !errors.Is(err, p.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable for nil repo, got %v", err)
	}
	repo := newMemoryRepo()
	s, err := p.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Ready(ctx); err != nil {
		t.Fatal(err)
	}

	// Nickname and strict JSON preference boundaries.
	if got := p.InitialNickname(" bad ", "good_name"); got != "good_name" {
		t.Fatalf("unexpected InitialNickname: %q", got)
	}
	if p.ValidName("has\x00nul") || p.ValidName("a\u200db") || p.ValidName(strings.Repeat("字", 21)) {
		t.Fatal("ValidName accepted invalid input")
	}
	var vis p.Visibility
	if json.Unmarshal([]byte(`{"school":null,"badge":true,"department":true,"grade":false,"reviews":true,"resources":true}`), &vis) == nil {
		t.Fatal("Visibility accepted null boolean")
	}
	var consent p.GroupCardConsent
	if json.Unmarshal([]byte(`{"school":false}`), &consent) == nil {
		t.Fatal("GroupCardConsent accepted incomplete object")
	}

	issuer := "https://issuer.invalid"
	alice := p.Principal{Issuer: issuer, Subject: "alice"}
	servicePrincipal := p.Principal{Issuer: issuer, Subject: "group-service"}
	admin := p.Principal{Issuer: issuer, Subject: "admin"}

	prof, err := s.Ensure(ctx, alice, "Alice")
	if err != nil {
		t.Fatal(err)
	}

	// Public profile edge cases: invalid path ID, unsafe facts values, unknown badge.
	for _, badID := range []string{"short", "00000000-0000-4000-8000-00000000000/", "00000000-0000-4000-8000-0000000000\n "} {
		if _, err := s.Public(ctx, badID); !errors.Is(err, p.ErrNotFound) {
			t.Fatalf("expected ErrNotFound for %q, got %v", badID, err)
		}
	}
	s.Facts = unitFacts{facts: []p.Affiliation{{SchoolID: "buaa", Badge: "invalid_role"}}}
	pub, err := s.Public(ctx, prof.ID)
	if err != nil || pub.FactsState != "unavailable" || len(pub.Affiliations) != 0 {
		t.Fatalf("expected invalid badge to degrade FactsState to unavailable: %+v, err=%v", pub, err)
	}
	s.Facts = unitFacts{facts: []p.Affiliation{{SchoolID: "buaa\ncontrol", Badge: "student"}}}
	pub, err = s.Public(ctx, prof.ID)
	if err != nil || pub.FactsState != "unavailable" {
		t.Fatalf("expected control char in fact to degrade FactsState: %+v, err=%v", pub, err)
	}

	// GroupCardFields Base64URL and upstream validation boundaries.
	ownerKey := base64.RawURLEncoding.EncodeToString([]byte(alice.Issuer)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(alice.Subject))
	if _, err := s.GroupCardFields(ctx, p.Principal{}, ownerKey, "buaa", p.GroupCardPurpose); !errors.Is(err, p.ErrUnauthenticated) {
		t.Fatal("expected ErrUnauthenticated", err)
	}
	for _, badOwner := range []string{"not-colon", "===:===", ownerKey + "=:"} {
		if _, err := s.GroupCardFields(ctx, servicePrincipal, badOwner, "buaa", p.GroupCardPurpose); !errors.Is(err, p.ErrInput) {
			t.Fatalf("expected ErrInput for owner=%q, got %v", badOwner, err)
		}
	}
	if _, err := s.GroupCardFields(ctx, servicePrincipal, ownerKey, "buaa", p.GroupCardPurpose); !errors.Is(err, p.ErrUnavailable) {
		t.Fatal("expected ErrUnavailable when GroupCards is nil", err)
	}
	allowGender := p.GroupCardConsent{Gender: true, Nickname: true}
	prof, err = s.Update(ctx, alice, "enable-gender", p.Update{Nickname: "Alice", Revision: prof.Revision, GroupCardConsent: &allowGender})
	if err != nil {
		t.Fatal(err)
	}
	s.GroupCards = unitCardProvider{values: map[string]p.CardField{"gender": {Available: true, Value: "invalid_gender"}}}
	if _, err := s.GroupCardFields(ctx, servicePrincipal, ownerKey, "buaa", p.GroupCardPurpose); !errors.Is(err, p.ErrUnavailable) {
		t.Fatal("expected ErrUnavailable for invalid upstream gender enum", err)
	}

	// InformationQuery redaction, expand rejection, and duplicate field rejection.
	q := p.Query{Kind: "account", Value: ownerKey, Scope: p.Scope{School: "school"}, Purpose: "audit", Limit: 10}
	if strings.Contains(fmt.Sprintf("%s %#v", q, q), ownerKey) {
		t.Fatal("Query String/GoString leaked raw account value")
	}
	s.Information = &informationAuthority{}
	qExpand := q
	qExpand.Expand = []string{"identity"}
	if _, err := s.Query(ctx, admin, qExpand); !errors.Is(err, p.ErrInput) {
		t.Fatal("expected Expand to be rejected with ErrInput", err)
	}
	qDup := q
	qDup.Fields = []string{"nickname", "nickname"}
	if _, err := s.Query(ctx, admin, qDup); !errors.Is(err, p.ErrInput) {
		t.Fatal("expected duplicate Fields to be rejected with ErrInput", err)
	}
	recs, err := s.Query(ctx, admin, p.Query{Kind: "account", Value: ownerKey, Scope: p.Scope{School: "school"}, Purpose: "audit", Limit: 10, Fields: []string{"nickname", "bio"}})
	if err != nil || len(recs) != 1 || len(recs[0].Fields) != 2 {
		t.Fatalf("unexpected Query result: %+v, err=%v", recs, err)
	}

	// Closure AdminClosure and concurrent ProcessClosure without PostgreSQL.
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	s.ClosureAuthority = closureAuthority{}
	cl, err := s.RequestClosure(ctx, alice, "req-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminClosure(ctx, alice, cl.ID); !errors.Is(err, p.ErrForbidden) {
		t.Fatal("expected ErrForbidden for non-admin AdminClosure", err)
	}
	gotAdmin, err := s.AdminClosure(ctx, admin, cl.ID)
	if err != nil || gotAdmin.ID != cl.ID {
		t.Fatal(gotAdmin, err)
	}
	now = cl.CoolingEndsAt
	fx := &closureEffects{seen: map[string]bool{}, effects: map[string]int{}, skipped: []string{"favorites"}}
	s.ClosureEffects = fx
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res, err := s.ProcessClosure(ctx, admin, fmt.Sprintf("k-%d", idx), cl.ID)
			if err != nil || res.State != "closed" {
				t.Errorf("concurrent ProcessClosure %d failed: %+v, err=%v", idx, res, err)
			}
		}(i)
	}
	wg.Wait()
	if !p.ValidClosureProgress(p.Closure{State: "closed", CompletedSteps: []string{"assets", "sessions", "authorizations", "content"}, RequestedAt: cl.RequestedAt, CoolingEndsAt: cl.CoolingEndsAt, PointsPolicy: p.PointsPolicy}) {
		t.Fatal("ValidClosureProgress rejected valid closed record")
	}
}
