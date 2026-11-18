package profiles

import "context"

type Preview struct {
	Nickname   *string     `json:"nickname,omitempty"`
	Bio        *string     `json:"bio,omitempty"`
	Visibility *Visibility `json:"visibility,omitempty"`
}

// Preview synthesizes the caller's PublicProfile from unsaved draft fields using
// the exact PublicFacts filtering rules without mutating database state.
func (s *Service) Preview(ctx context.Context, p Principal, in Preview) (PublicProfile, error) {
	if !p.Valid() {
		return PublicProfile{}, ErrInput
	}
	if err := s.AccountAccess(ctx, p); err != nil {
		return PublicProfile{}, err
	}
	current, err := s.repo.Get(ctx, p)
	if err != nil {
		return PublicProfile{}, err
	}
	draft := current
	if in.Nickname != nil {
		if *in.Nickname == "" {
			if !current.NicknameRequired {
				return PublicProfile{}, ErrInput
			}
		} else if !ValidName(*in.Nickname) {
			return PublicProfile{}, ErrInput
		}
		draft.Nickname = *in.Nickname
	}
	if in.Bio != nil {
		bio := NormalizeBio(*in.Bio)
		if err := ValidateBio(bio); err != nil {
			return PublicProfile{}, err
		}
		draft.Bio = bio
	}
	if in.Visibility != nil {
		draft.Visibility = *in.Visibility
	}
	return s.resolvePublicRecords(ctx, []PublicRecord{{Owner: p, Profile: draft}})[0], nil
}
