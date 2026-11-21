package profiles_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	p "github.com/StuHelper/stuhelper-profiles"
)

type perOwnerAvatarRelease struct {
	*fakeAvatarMedia
	unavailable p.Principal
}

func (m *perOwnerAvatarRelease) ReleaseAvatar(ctx context.Context, owner p.Principal, profileID, versionID string) error {
	if owner == m.unavailable {
		return p.ErrUnavailable
	}
	return m.fakeAvatarMedia.ReleaseAvatar(ctx, owner, profileID, versionID)
}

func TestFlushPendingAvatarReleasesAfterRestartSkipsActiveVersionsAndAdvancesPastFailures(t *testing.T) {
	ctx := context.Background()
	repo := avatarRepository(t)
	service, err := p.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	alice := p.Principal{Issuer: "https://issuer.invalid", Subject: "alice"}
	bob := p.Principal{Issuer: alice.Issuer, Subject: "bob"}
	port := &perOwnerAvatarRelease{
		fakeAvatarMedia: &fakeAvatarMedia{
			readyByOwner: map[string]p.Principal{"active": bob},
			activated:    map[string]int{},
			released:     map[string]int{},
			active:       map[string]bool{},
		},
		unavailable: alice,
	}
	service.AvatarMedia = port
	for _, owner := range []p.Principal{alice, bob} {
		profile, err := service.Ensure(ctx, owner, "Profile")
		if err != nil {
			t.Fatal(err)
		}
		if owner == bob {
			active := "active"
			if _, err := service.Update(ctx, bob, "set-avatar", p.Update{Nickname: profile.Nickname, Revision: profile.Revision, AvatarVersionID: &active}); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.QueueAvatarRelease(ctx, bob, profile.ID, active); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := repo.QueueAvatarRelease(ctx, owner, profile.ID, "unused"); err != nil {
			t.Fatal(err)
		}
	}

	restarted, err := p.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	restarted.AvatarMedia = port
	next, err := restarted.FlushPendingAvatarReleases(ctx, p.Principal{}, 1)
	if next != alice || !errors.Is(err, p.ErrUnavailable) {
		t.Fatalf("failed owner did not advance the cursor: %v, %v", next, err)
	}
	next, err = restarted.FlushPendingAvatarReleases(ctx, next, 1)
	if next != bob || err != nil {
		t.Fatalf("next owner was starved by a failed release: %v, %v", next, err)
	}
	if releases, err := repo.PendingAvatarReleases(ctx, bob); err != nil || len(releases) != 0 {
		t.Fatalf("second owner's queue was not drained: %+v, %v", releases, err)
	}
	if next, err = restarted.FlushPendingAvatarReleases(ctx, next, 1); next != (p.Principal{}) || err != nil {
		t.Fatalf("last page did not reset cursor: %v, %v", next, err)
	}
	if len(port.released) != 1 {
		t.Fatalf("unexpected media releases: %+v", port.released)
	}
	port.unavailable = p.Principal{}
	if _, err := restarted.FlushPendingAvatarReleases(ctx, next, 2); err != nil {
		t.Fatal(err)
	}
	if next, err := restarted.FlushPendingAvatarReleases(ctx, p.Principal{}, 2); next != (p.Principal{}) || err != nil {
		t.Fatalf("unexpected work after both queues were drained: %v, %v", next, err)
	}
	if len(port.released) != 2 {
		t.Fatalf("unexpected total releases: %+v", port.released)
	}
	for key, count := range port.released {
		if count != 1 || !strings.HasSuffix(key, ":unused") {
			t.Fatalf("active avatar or a duplicate was released: %+v", port.released)
		}
	}
}
