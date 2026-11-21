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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type multiAdminClosureAuthority struct{}

func (multiAdminClosureAuthority) AuthorizeClosure(_ context.Context, who p.Principal) error {
	if !strings.HasPrefix(who.Subject, "admin") {
		return p.ErrForbidden
	}
	return nil
}

type instrumentedClosureEffects struct {
	onAssets      func()
	stepDelay     time.Duration
	inFlight      atomic.Int32
	maxConcurrent atomic.Int32
	assetsCalls   atomic.Int32
	sessionsCalls atomic.Int32
	authzCalls    atomic.Int32
	contentCalls  atomic.Int32
}

func (e *instrumentedClosureEffects) trackConcurrency() func() {
	cur := e.inFlight.Add(1)
	for {
		prev := e.maxConcurrent.Load()
		if cur <= prev || e.maxConcurrent.CompareAndSwap(prev, cur) {
			break
		}
	}
	if e.stepDelay > 0 {
		time.Sleep(e.stepDelay)
	}
	return func() { e.inFlight.Add(-1) }
}

func (e *instrumentedClosureEffects) ClearAssets(context.Context, p.Principal, string) error {
	defer e.trackConcurrency()()
	e.assetsCalls.Add(1)
	if e.onAssets != nil {
		e.onAssets()
	}
	return nil
}

func (e *instrumentedClosureEffects) ClearAssetsResult(ctx context.Context, who p.Principal, id string) ([]string, error) {
	return []string{"billing"}, e.ClearAssets(ctx, who, id)
}

func (e *instrumentedClosureEffects) RevokeSessions(context.Context, p.Principal, string) error {
	defer e.trackConcurrency()()
	e.sessionsCalls.Add(1)
	return nil
}

func (e *instrumentedClosureEffects) RevokeAuthorizations(context.Context, p.Principal, string) error {
	defer e.trackConcurrency()()
	e.authzCalls.Add(1)
	return nil
}

func (e *instrumentedClosureEffects) AnonymizeContent(context.Context, p.Principal, string) error {
	defer e.trackConcurrency()()
	e.contentCalls.Add(1)
	return nil
}

func TestClosureLeaseDisconnectPersistenceReplayAndAudit(t *testing.T) {
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

	name := fmt.Sprintf("profiles_a05_a06_%d", time.Now().UnixNano())
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
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	svc1, _ := p.New(repo)
	svc2, _ := p.New(repo)
	for _, s := range []*p.Service{svc1, svc2} {
		s.ClosureAuthority = multiAdminClosureAuthority{}
		s.Now = func() time.Time { return now }
	}

	issuer := "https://issuer.invalid"
	alice := p.Principal{Issuer: issuer, Subject: "alice"}
	admin1 := p.Principal{Issuer: issuer, Subject: "admin1"}
	admin2 := p.Principal{Issuer: issuer, Subject: "admin2"}
	if _, err = svc1.Ensure(ctx, alice, "Alice"); err != nil {
		t.Fatal(err)
	}

	// A-06.1: Replaying an old request key after cancellation must return current cancelled state, never stale cooling.
	c1, err := svc1.RequestClosure(ctx, alice, "req-k1", true)
	if err != nil || c1.State != "cooling" {
		t.Fatalf("expected cooling closure, got %+v err=%v", c1, err)
	}
	now = now.Add(time.Hour)
	cancelled, err := svc1.CancelClosure(ctx, alice, "cancel-k1", c1.ID)
	if err != nil || cancelled.State != "cancelled" {
		t.Fatalf("expected cancelled closure, got %+v err=%v", cancelled, err)
	}
	replayedAfterCancel, err := svc1.RequestClosure(ctx, alice, "req-k1", true)
	if err != nil || replayedAfterCancel.State != "cancelled" {
		t.Fatalf("expected current cancelled state on old key replay, got %+v err=%v", replayedAfterCancel, err)
	}

	// A-05.3: Client disconnect right after ClearAssets completes must still persist step 0 and skipped modules, then stop before step 1.
	now = now.Add(time.Hour)
	c2, err := svc1.RequestClosure(ctx, alice, "req-k2", true)
	if err != nil {
		t.Fatal(err)
	}
	now = c2.CoolingEndsAt
	disconnectCtx, cancelDisconnect := context.WithCancel(ctx)
	effects := &instrumentedClosureEffects{
		onAssets: func() { cancelDisconnect() },
	}
	svc1.ClosureEffects = effects
	svc2.ClosureEffects = effects

	afterDisconnect, err := svc1.ProcessClosure(disconnectCtx, admin1, "approve-k1", c2.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled after disconnect, got %v", err)
	}
	stored, err := svc1.AdminClosure(ctx, admin1, c2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != "processing" || !slices.Equal(stored.CompletedSteps, []string{"assets"}) || !slices.Equal(stored.SkippedModules, []string{"billing"}) {
		t.Fatalf("disconnect lost completed assets progress: got %+v (returned %+v)", stored, afterDisconnect)
	}
	if effects.sessionsCalls.Load() != 0 {
		t.Fatalf("expected sessions step not to start after disconnect, got %d calls", effects.sessionsCalls.Load())
	}

	// A-06.1: Replaying old request key once account entered processing must return ErrClosed.
	if _, err = svc1.RequestClosure(ctx, alice, "req-k2", true); !errors.Is(err, p.ErrClosed) {
		t.Fatalf("expected ErrClosed replaying request key during processing, got %v", err)
	}

	// A-05.1 & A-05.2: Concurrent ProcessClosure across two Service instances must serialize step execution without parallel external calls.
	effects.onAssets = nil
	effects.stepDelay = 40 * time.Millisecond
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, call := range []struct {
		svc   *p.Service
		admin p.Principal
		key   string
	}{
		{svc1, admin1, "recover-1"},
		{svc2, admin2, "recover-2"},
	} {
		wg.Add(1)
		go func(idx int, s *p.Service, who p.Principal, key string) {
			defer wg.Done()
			res, e := s.ProcessClosure(ctx, who, key, c2.ID)
			if e == nil && res.State != "closed" {
				e = fmt.Errorf("caller %d got non-closed state: %+v", idx, res)
			}
			errs <- e
		}(i, call.svc, call.admin, call.key)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if effects.maxConcurrent.Load() != 1 {
		t.Fatalf("external ClosureEffects ran concurrently: maxConcurrent=%d", effects.maxConcurrent.Load())
	}
	if effects.assetsCalls.Load() != 1 || effects.sessionsCalls.Load() != 1 || effects.authzCalls.Load() != 1 || effects.contentCalls.Load() != 1 {
		t.Fatalf("unexpected step invocation counts: assets=%d sessions=%d authz=%d content=%d",
			effects.assetsCalls.Load(), effects.sessionsCalls.Load(), effects.authzCalls.Load(), effects.contentCalls.Load())
	}

	// A-06.1: Replaying old request key after account is closed must return ErrClosed.
	if _, err = svc1.RequestClosure(ctx, alice, "req-k2", true); !errors.Is(err, p.ErrClosed) {
		t.Fatalf("expected ErrClosed replaying request key after closure, got %v", err)
	}

	// A-06.2 & A-06.3: Calling ProcessClosure with a new key on a closed request must not append fake approve events, and step events record admin actor.
	var eventCountBefore int
	if err = owner.QueryRow(`SELECT count(*) FROM profiles.closure_events WHERE closure_id=$1`, c2.ID).Scan(&eventCountBefore); err != nil {
		t.Fatal(err)
	}
	postClosed, err := svc1.ProcessClosure(ctx, admin1, "post-closed-key", c2.ID)
	if err != nil || postClosed.State != "closed" {
		t.Fatalf("expected closed result on post-closed ProcessClosure, got %+v err=%v", postClosed, err)
	}
	var eventCountAfter int
	if err = owner.QueryRow(`SELECT count(*) FROM profiles.closure_events WHERE closure_id=$1`, c2.ID).Scan(&eventCountAfter); err != nil {
		t.Fatal(err)
	}
	if eventCountAfter != eventCountBefore {
		t.Fatalf("ProcessClosure on closed request appended %d spurious events", eventCountAfter-eventCountBefore)
	}

	rows, err := owner.Query(`SELECT action,actor_subject FROM profiles.closure_events WHERE closure_id=$1 ORDER BY id ASC`, c2.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions, subjects []string
	for rows.Next() {
		var act, subj string
		if err = rows.Scan(&act, &subj); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, act)
		subjects = append(subjects, subj)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	approveCount := 0
	for i, act := range actions {
		if act == "approve" {
			approveCount++
		}
		if strings.Contains(act, ":") && !strings.HasPrefix(subjects[i], "admin") {
			t.Fatalf("step event %q recorded non-admin actor %q", act, subjects[i])
		}
	}
	if approveCount != 1 {
		t.Fatalf("expected exactly 1 'approve' event, got %d in %v", approveCount, actions)
	}
	if actions[len(actions)-1] != "content:" {
		t.Fatalf("expected final closure event to be 'content:', got %v", actions)
	}
}

type staleLocalBlockerRepo struct {
	owner p.Principal
	state p.Closure
}

func (r *staleLocalBlockerRepo) Ready(context.Context) error { return nil }
func (r *staleLocalBlockerRepo) Get(context.Context, p.Principal) (p.Profile, error) {
	return p.Profile{}, p.ErrNotFound
}
func (r *staleLocalBlockerRepo) Public(context.Context, string) (p.PublicRecord, error) {
	return p.PublicRecord{}, p.ErrNotFound
}
func (r *staleLocalBlockerRepo) Ensure(context.Context, p.Principal, string) (p.Profile, error) {
	return p.Profile{}, p.ErrUnavailable
}
func (r *staleLocalBlockerRepo) Update(context.Context, p.Principal, string, p.Update) (p.Profile, error) {
	return p.Profile{}, p.ErrUnavailable
}
func (r *staleLocalBlockerRepo) CurrentClosure(context.Context, p.Principal) (p.Closure, error) {
	return r.state, nil
}
func (r *staleLocalBlockerRepo) Closure(context.Context, string) (p.ClosureRecord, error) {
	return p.ClosureRecord{Owner: r.owner, Closure: r.state}, nil
}
func (r *staleLocalBlockerRepo) Closures(context.Context, string, int) ([]p.Closure, error) {
	return []p.Closure{r.state}, nil
}
func (r *staleLocalBlockerRepo) ChangeClosure(context.Context, p.Principal, string, string, string, time.Time) (p.Closure, error) {
	return r.state, nil
}
func (r *staleLocalBlockerRepo) AccountState(context.Context, p.Principal) (string, error) {
	return "active", nil
}
func (r *staleLocalBlockerRepo) AdvanceClosure(_ context.Context, _ string, step int, blocker string, _ []string, now time.Time) (p.Closure, error) {
	if step == 0 && blocker != "" {
		// Simulate another worker having already completed step 0 with Blocker == "".
		r.state.State = "processing"
		r.state.CompletedSteps = []string{"assets"}
		r.state.Blocker = ""
		r.state.Revision++
		return r.state, nil
	}
	steps := []string{"assets", "sessions", "authorizations", "content"}
	r.state.CompletedSteps = append(r.state.CompletedSteps, steps[step])
	r.state.Blocker = ""
	r.state.Revision++
	if len(r.state.CompletedSteps) == 4 {
		r.state.State = "closed"
		r.state.CompletedAt = &now
	}
	return r.state, nil
}

type failAssetsOnceEffects struct {
	failedAssets bool
}

func (f *failAssetsOnceEffects) ClearAssets(context.Context, p.Principal, string) error {
	if !f.failedAssets {
		f.failedAssets = true
		return p.ErrUnavailable
	}
	return nil
}
func (f *failAssetsOnceEffects) RevokeSessions(context.Context, p.Principal, string) error {
	return nil
}
func (f *failAssetsOnceEffects) RevokeAuthorizations(context.Context, p.Principal, string) error {
	return nil
}
func (f *failAssetsOnceEffects) AnonymizeContent(context.Context, p.Principal, string) error {
	return nil
}

func TestProcessClosureUsesPersistedBlockerOverStaleLocalError(t *testing.T) {
	reqAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	id := "11111111-1111-4111-8111-111111111111"
	owner := p.Principal{Issuer: "https://issuer.invalid", Subject: "alice"}
	admin := p.Principal{Issuer: owner.Issuer, Subject: "admin1"}
	repo := &staleLocalBlockerRepo{
		owner: owner,
		state: p.Closure{
			ID:             id,
			ProfileID:      "22222222-2222-4222-8222-222222222222",
			State:          "awaiting_assets",
			Revision:       2,
			RequestedAt:    reqAt,
			CoolingEndsAt:  reqAt.Add(7 * 24 * time.Hour),
			CompletedSteps: []string{},
			PointsPolicy:   p.PointsPolicy,
		},
	}
	svc, err := p.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	svc.ClosureAuthority = multiAdminClosureAuthority{}
	svc.ClosureEffects = &failAssetsOnceEffects{}
	svc.Now = func() time.Time { return reqAt.Add(8 * 24 * time.Hour) }

	got, err := svc.ProcessClosure(context.Background(), admin, "process-1", id)
	if err != nil || got.State != "closed" {
		t.Fatalf("expected ProcessClosure to continue using persisted state rather than exiting half-finished, got %+v err=%v", got, err)
	}
}

type gatedClosureAssets struct {
	*closureEffects
	entered chan struct{}
	release chan struct{}
}

func (f *gatedClosureAssets) ClearAssets(ctx context.Context, owner p.Principal, id string) error {
	close(f.entered)
	select {
	case <-f.release:
		return f.closureEffects.ClearAssets(ctx, owner, id)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestExpiredClosureWorkerCannotAdvanceOrReleaseReplacementLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo := avatarRepository(t)
	admin := p.Principal{Issuer: "https://issuer.invalid", Subject: "admin"}
	owner := p.Principal{Issuer: admin.Issuer, Subject: "account"}
	now := time.Now().UTC().Add(-9 * 24 * time.Hour).Truncate(time.Millisecond)
	mutator, err := p.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	mutator.ClosureAuthority = closureAuthority{}
	mutator.Now = func() time.Time { return now }
	if _, err := mutator.Ensure(ctx, owner, "Account"); err != nil {
		t.Fatal(err)
	}
	request, err := mutator.RequestClosure(ctx, owner, "apply", true)
	if err != nil {
		t.Fatal(err)
	}
	now = request.CoolingEndsAt

	effects := &closureEffects{seen: map[string]bool{}, effects: map[string]int{}}
	first := &gatedClosureAssets{closureEffects: effects, entered: make(chan struct{}), release: make(chan struct{})}
	mutator.ClosureEffects = first
	oldDone := make(chan error, 1)
	go func() {
		_, err := mutator.ProcessClosure(ctx, admin, "manual-first", request.ID)
		oldDone <- err
	}()
	select {
	case <-first.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Advance the test lease clock without sleeping or altering the application clock.
	if _, err := repo.DB.ExecContext(ctx, `UPDATE profiles.closures SET processing_until=clock_timestamp()-interval '1 second' WHERE id=$1`, request.ID); err != nil {
		t.Fatal(err)
	}

	second := &gatedClosureAssets{closureEffects: effects, entered: make(chan struct{}), release: make(chan struct{})}
	restarted, err := p.New(&postgres.Repository{DB: repo.DB})
	if err != nil {
		t.Fatal(err)
	}
	restarted.Now = mutator.Now
	restarted.ClosureEffects = second
	newDone := make(chan error, 1)
	go func() {
		results, err := restarted.ProcessDueClosures(ctx, 1)
		if err == nil && (len(results) != 1 || results[0].ID != request.ID || results[0].State != "closed") {
			err = fmt.Errorf("replacement did not close the original request: %+v", results)
		}
		newDone <- err
	}()
	select {
	case <-second.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	close(first.release)
	select {
	case err := <-oldDone:
		if !errors.Is(err, p.ErrConflict) {
			t.Fatalf("expired worker advanced state: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	record, token, err := repo.AcquireClosureStep(ctx, request.ID, time.Second)
	if err != nil || token != "" || len(record.Closure.CompletedSteps) != 0 {
		t.Fatalf("expired worker changed the step or released the new lease: %+v, lease=%t, %v", record.Closure, token != "", err)
	}

	close(second.release)
	select {
	case err := <-newDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if effects.effects["assets"] != 1 {
		t.Fatalf("retried external operation was applied %d times", effects.effects["assets"])
	}
}
