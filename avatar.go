package profiles

import (
	"context"
	"errors"
	"time"
)

const AvatarUsage = "profile_avatar"

// AvatarMedia validates and activates ready owner-uploaded profile_avatar media
// versions and releases superseded or closed references. Implementations must be
// idempotent for the same (owner, profileID, versionID) binding.
type AvatarMedia interface {
	ActivateAvatar(ctx context.Context, owner Principal, profileID, versionID string) error
	ReleaseAvatar(ctx context.Context, owner Principal, profileID, versionID string) error
}

type AvatarRelease struct {
	ProfileID string
	VersionID string
}

type AvatarRepository interface {
	ReplayUpdate(ctx context.Context, who Principal, key string, in Update) (Profile, bool, error)
	PendingAvatarReleases(ctx context.Context, who Principal) ([]AvatarRelease, error)
	ClosureAvatarReleases(ctx context.Context, who Principal) ([]AvatarRelease, error)
	ConfirmAvatarReleased(ctx context.Context, who Principal, versionID string) error
}

func ValidAvatarVersionID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func avatarError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrInput):
		return ErrInput
	case errors.Is(err, ErrForbidden):
		return ErrForbidden
	case errors.Is(err, ErrNotFound):
		return ErrNotFound
	case errors.Is(err, ErrConflict):
		return ErrConflict
	case errors.Is(err, ErrClosed):
		return ErrClosed
	default:
		return ErrUnavailable
	}
}

func (s *Service) updateWithAvatar(ctx context.Context, p Principal, key string, in Update) (Profile, error) {
	if *in.AvatarVersionID != "" && !ValidAvatarVersionID(*in.AvatarVersionID) {
		return Profile{}, ErrInput
	}
	ar, hasAR := s.repo.(AvatarRepository)
	if hasAR {
		replayed, hit, err := ar.ReplayUpdate(ctx, p, key, in)
		if err != nil {
			return replayed, err
		}
		if hit {
			_ = s.FlushAvatarReleases(ctx, p)
			return replayed, nil
		}
	}
	if s.AvatarMedia == nil {
		return Profile{}, ErrUnavailable
	}
	if err := s.AccountAccess(ctx, p); err != nil {
		return Profile{}, err
	}
	current, err := s.repo.Get(ctx, p)
	if err != nil {
		return current, err
	}
	if current.Revision != in.Revision {
		return current, ErrConflict
	}
	activated := false
	if *in.AvatarVersionID != "" && *in.AvatarVersionID != current.AvatarVersionID {
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = s.AvatarMedia.ActivateAvatar(callCtx, p, current.ID, *in.AvatarVersionID)
		cancel()
		if err != nil {
			return Profile{}, avatarError(err)
		}
		activated = true
	}
	out, err := s.repo.Update(ctx, p, key, in)
	if err != nil {
		if activated && out.AvatarVersionID != *in.AvatarVersionID {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			_ = s.AvatarMedia.ReleaseAvatar(cleanupCtx, p, current.ID, *in.AvatarVersionID)
			cancel()
		}
		return out, err
	}
	if hasAR {
		_ = s.FlushAvatarReleases(ctx, p)
	} else if current.AvatarVersionID != "" && current.AvatarVersionID != out.AvatarVersionID {
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		_ = s.AvatarMedia.ReleaseAvatar(callCtx, p, current.ID, current.AvatarVersionID)
		cancel()
	}
	return out, nil
}

// FlushAvatarReleases drains durable superseded avatar references for an owner.
func (s *Service) FlushAvatarReleases(ctx context.Context, p Principal) error {
	if !p.Valid() {
		return ErrInput
	}
	ar, ok := s.repo.(AvatarRepository)
	if !ok {
		return nil
	}
	releases, err := ar.PendingAvatarReleases(ctx, p)
	if err != nil || len(releases) == 0 {
		return err
	}
	if s.AvatarMedia == nil {
		return ErrUnavailable
	}
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	for _, rel := range releases {
		if err := s.AvatarMedia.ReleaseAvatar(callCtx, p, rel.ProfileID, rel.VersionID); err != nil {
			return avatarError(err)
		}
		if err := ar.ConfirmAvatarReleased(callCtx, p, rel.VersionID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) releaseClosureAvatars(ctx context.Context, owner Principal) error {
	ar, ok := s.repo.(AvatarRepository)
	if !ok {
		return nil
	}
	releases, err := ar.ClosureAvatarReleases(ctx, owner)
	if err != nil || len(releases) == 0 {
		return err
	}
	if s.AvatarMedia == nil {
		return ErrUnavailable
	}
	for _, rel := range releases {
		if err := s.AvatarMedia.ReleaseAvatar(ctx, owner, rel.ProfileID, rel.VersionID); err != nil {
			return avatarError(err)
		}
		if err := ar.ConfirmAvatarReleased(ctx, owner, rel.VersionID); err != nil {
			return err
		}
	}
	return nil
}
