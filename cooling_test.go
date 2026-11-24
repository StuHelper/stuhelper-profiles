package profiles_test

import (
	"context"
	"errors"
	p "github.com/StuHelper/stuhelper-profiles"
	"testing"
	"time"
)

type coolingStorage struct {
	fail  bool
	holds map[string]bool
}

func (f *coolingStorage) SetCooling(_ context.Context, _ p.Principal, id string, cancel bool) error {
	if f.fail {
		return p.ErrUnavailable
	}
	f.holds[id] = !cancel
	return nil
}

func TestCoolingWaitsForStorageAndRestoresAfterCancellationRestart(t *testing.T) {
	ctx := context.Background()
	repo := avatarRepository(t)
	svc, _ := p.New(repo)
	owner := p.Principal{Issuer: "urn:synthetic", Subject: "cooling"}
	clock := time.Now().UTC().Truncate(time.Microsecond)
	svc.Now = func() time.Time { return clock }
	if _, err := svc.Ensure(ctx, owner, "Fixture"); err != nil {
		t.Fatal(err)
	}
	f := &coolingStorage{fail: true, holds: map[string]bool{}}
	svc.CoolingControl = f
	if _, err := svc.RequestClosure(ctx, owner, "start", true); !errors.Is(err, p.ErrUnavailable) {
		t.Fatalf("unguarded acceptance: %v", err)
	}
	if _, err := svc.OwnClosure(ctx, owner); !errors.Is(err, p.ErrNotFound) {
		t.Fatalf("cooling visible before barrier: %v", err)
	}
	f.fail = false
	clock = clock.Add(time.Hour)
	if err := svc.ReconcileCooling(ctx, 10); err != nil {
		t.Fatal(err)
	}
	row, err := svc.RequestClosure(ctx, owner, "start", true)
	if err != nil || !f.holds[row.ID] || !row.CoolingEndsAt.Equal(clock.Add(7*24*time.Hour)) {
		t.Fatalf("guard/deadline mismatch: %+v %v", row, err)
	}
	f.fail = true
	if _, err = svc.CancelClosure(ctx, owner, "cancel", row.ID); !errors.Is(err, p.ErrUnavailable) {
		t.Fatalf("unconfirmed cancel: %v", err)
	}
	restarted, _ := p.New(repo)
	restarted.CoolingControl = f
	restarted.Now = svc.Now
	f.fail = false
	if err := restarted.ReconcileCooling(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if row, err = restarted.CancelClosure(ctx, owner, "cancel", row.ID); err != nil || row.State != "cancelled" || f.holds[row.ID] {
		t.Fatal("cancel did not restore state", row.State, err)
	}
}
