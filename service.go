// Package profiles owns the public nickname used by signed contributions.
package profiles

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInput       = errors.New("invalid_profile")
	ErrConflict    = errors.New("profile_revision_conflict")
	ErrUnavailable = errors.New("profile_unavailable")
	ErrNotFound    = errors.New("profile_not_found")
)

type Principal struct{ Issuer, Subject string }

func (p Principal) Valid() bool {
	return p.Issuer != "" && p.Subject != "" && len(p.Issuer) <= 512 && len(p.Subject) <= 256
}

type Profile struct {
	Nickname string `json:"nickname"`
	Revision int64  `json:"revision"`
}
type Repository interface {
	Ready(context.Context) error
	Get(context.Context, Principal) (Profile, error)
	Ensure(context.Context, Principal, string) (Profile, error)
	Save(context.Context, Principal, string, int64, string) (Profile, error)
}
type Service struct{ repo Repository }

func New(repo Repository) (*Service, error) {
	if repo == nil {
		return nil, ErrUnavailable
	}
	return &Service{repo}, nil
}
func validName(name string) bool {
	return name == strings.TrimSpace(name) && utf8.ValidString(name) && utf8.RuneCountInString(name) >= 1 && utf8.RuneCountInString(name) <= 64 && !strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) })
}
func (s *Service) Ready(ctx context.Context) error { return s.repo.Ready(ctx) }
func (s *Service) Get(ctx context.Context, p Principal) (Profile, error) {
	if !p.Valid() {
		return Profile{}, ErrInput
	}
	return s.repo.Get(ctx, p)
}

// Ensure imports only the user's chosen SSO display name on the first login.
// Later SSO changes never overwrite the business nickname or its revision.
func (s *Service) Ensure(ctx context.Context, p Principal, name string) (Profile, error) {
	if !p.Valid() || !validName(name) {
		return Profile{}, ErrInput
	}
	return s.repo.Ensure(ctx, p, name)
}
func (s *Service) Save(ctx context.Context, p Principal, key string, revision int64, name string) (Profile, error) {
	if !p.Valid() || len(key) < 1 || len(key) > 128 || revision < 1 || !validName(name) {
		return Profile{}, ErrInput
	}
	return s.repo.Save(ctx, p, key, revision, name)
}
