package profiles

import "context"

// PublicAvatar returns the avatar version currently bound to a published
// profile. The caller must check the media reference before serving bytes.
func (s *Service) PublicAvatar(ctx context.Context, id string) (string, error) {
	if !validUUID(id) {
		return "", ErrNotFound
	}
	record, err := s.repo.Public(ctx, id)
	if err != nil {
		return "", err
	}
	if record.Profile.AvatarVersionID == "" {
		return "", ErrNotFound
	}
	return record.Profile.AvatarVersionID, nil
}
