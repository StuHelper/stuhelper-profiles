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
	WithAvatarOwner(ctx context.Context, who Principal, work func(context.Context) error) error
	QueueAvatarRelease(ctx context.Context, who Principal, profileID, versionID string) (bool, error)
	ReplayUpdate(ctx context.Context, who Principal, key string, in Update, now time.Time) (Profile, bool, error)
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
	ar, ok := s.repo.(AvatarRepository)
	if !ok {
		return Profile{}, ErrUnavailable
	}
	var out Profile
	err := ar.WithAvatarOwner(ctx, p, func(locked context.Context) error {
		var err error
		out, err = s.applyAvatarUpdate(locked, ar, p, key, in)
		return err
	})
	return out, err
}

func (s *Service) applyAvatarUpdate(ctx context.Context, ar AvatarRepository, p Principal, key string, in Update) (Profile, error) {
	now := s.now()
	if saved, found, err := ar.ReplayUpdate(ctx, p, key, in, now); err != nil || found {
		if found && err == nil {
			_ = s.drainAvatarReleases(ctx, ar, p, false)
		}
		return saved, err
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
	version := *in.AvatarVersionID
	if version != "" && version != current.AvatarVersionID {
		// A successful profile transaction consumes the intent. A failed write
		// or lost acknowledgement leaves any unused reference recoverable.
		inserted, err := ar.QueueAvatarRelease(ctx, p, current.ID, version)
		if err != nil {
			return Profile{}, err
		}
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = s.AvatarMedia.ActivateAvatar(call, p, current.ID, version)
		cancel()
		if err != nil {
			if inserted && avatarError(err) != ErrUnavailable {
				if cleanupErr := ar.ConfirmAvatarReleased(ctx, p, version); cleanupErr != nil {
					return Profile{}, cleanupErr
				}
			}
			return Profile{}, avatarError(err)
		}
	}
	out, err := s.saveUpdate(ctx, p, key, in)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			if saved, hit, replayErr := ar.ReplayUpdate(check, p, key, in, now); hit && replayErr == nil {
				return saved, nil
			}
		}
		return out, err
	}
	// Profile and release intents have committed. Transient release failures
	// remain in the durable queue and can be retried via FlushAvatarReleases.
	_ = s.drainAvatarReleases(ctx, ar, p, false)
	return out, nil
}

// FlushAvatarReleases drains superseded references under the same owner lock as activation.
func (s *Service) FlushAvatarReleases(ctx context.Context, p Principal) error {
	if !p.Valid() {
		return ErrInput
	}
	return s.releaseAvatars(ctx, p, false)
}

// FlushPendingAvatarReleases visits a page of account queues. A zero cursor
// starts a pass. Keep the returned cursor even on error so a failed account
// cannot prevent other queues from being drained.
func (s *Service) FlushPendingAvatarReleases(ctx context.Context, after Principal, limit int) (Principal, error) {
	if (after != (Principal{}) && !after.Valid()) || limit < 1 || limit > 100 {
		return after, ErrInput
	}
	repo, ok := s.repo.(interface {
		PendingAvatarReleaseOwners(context.Context, Principal, int) ([]Principal, error)
	})
	if !ok {
		return after, ErrUnavailable
	}
	scanCtx, cancelScan := context.WithTimeout(ctx, 3*time.Second)
	owners, err := repo.PendingAvatarReleaseOwners(scanCtx, after, limit)
	cancelScan()
	if err != nil {
		return after, err
	}
	next := after
	var workErr error
	for _, owner := range owners {
		if ctx.Err() != nil {
			return next, errors.Join(workErr, ctx.Err())
		}
		next = owner
		workErr = errors.Join(workErr, s.FlushAvatarReleases(ctx, owner))
	}
	if len(owners) < limit {
		next = Principal{}
	}
	return next, workErr
}

func (s *Service) releaseClosureAvatars(ctx context.Context, p Principal) error {
	return s.releaseAvatars(ctx, p, true)
}

func (s *Service) releaseAvatars(ctx context.Context, p Principal, closing bool) error {
	ar, ok := s.repo.(AvatarRepository)
	if !ok {
		return nil
	}
	return ar.WithAvatarOwner(ctx, p, func(locked context.Context) error { return s.drainAvatarReleases(locked, ar, p, closing) })
}

func (s *Service) drainAvatarReleases(ctx context.Context, ar AvatarRepository, p Principal, closing bool) error {
	var releases []AvatarRelease
	var err error
	if closing {
		releases, err = ar.ClosureAvatarReleases(ctx, p)
	} else {
		releases, err = ar.PendingAvatarReleases(ctx, p)
	}
	if err != nil || len(releases) == 0 {
		return err
	}
	if s.AvatarMedia == nil {
		return ErrUnavailable
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for i, rel := range releases {
		if i == 100 {
			return ErrUnavailable
		}
		if err := s.AvatarMedia.ReleaseAvatar(call, p, rel.ProfileID, rel.VersionID); err != nil && !errors.Is(err, ErrNotFound) {
			return avatarError(err)
		}
		if err := ar.ConfirmAvatarReleased(call, p, rel.VersionID); err != nil {
			return err
		}
	}
	return nil
}
