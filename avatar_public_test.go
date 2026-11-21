package profiles_test

import (
	"context"
	"errors"
	"testing"

	p "github.com/StuHelper/stuhelper-profiles"
)

func TestPublicAvatarRequiresPublishedProfileAndActiveVersion(t *testing.T) {
	ctx := context.Background()
	repo := avatarRepository(t)
	s, err := p.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	who := p.Principal{Issuer: "issuer", Subject: "alice"}
	version := "avatar-1"
	profile, err := s.Ensure(ctx, who, "Alice")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.PublicAvatar(ctx, profile.ID); !errors.Is(err, p.ErrNotFound) {
		t.Fatalf("profile without avatar returned %v", err)
	}
	s.AvatarMedia = &fakeAvatarMedia{
		readyByOwner: map[string]p.Principal{version: who},
		activated:    map[string]int{},
		released:     map[string]int{},
		active:       map[string]bool{},
	}
	current, err := s.Update(ctx, who, "bind-avatar", p.Update{
		Nickname: "Alice", Revision: profile.Revision, AvatarVersionID: &version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.PublicAvatar(ctx, profile.ID); err != nil || got != version {
		t.Fatalf("published avatar = %q, %v", got, err)
	}
	empty := ""
	if _, err := s.Update(ctx, who, "clear-avatar", p.Update{
		Nickname: "Alice", Revision: current.Revision, AvatarVersionID: &empty,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublicAvatar(ctx, profile.ID); !errors.Is(err, p.ErrNotFound) {
		t.Fatalf("cleared avatar returned %v", err)
	}
}
