// Package profiles owns public profiles independently from SSO identity.
package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
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

func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validIdempotencyKey(k string) bool {
	if len(k) < 1 || len(k) > 128 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7E {
			return false
		}
	}
	return true
}

func (p Principal) Valid() bool {
	return p.Issuer != "" && p.Subject != "" && len(p.Issuer) <= 512 && len(p.Subject) <= 256 &&
		utf8.ValidString(p.Issuer) && utf8.ValidString(p.Subject) &&
		!strings.ContainsFunc(p.Issuer, unicode.IsControl) && !strings.ContainsFunc(p.Subject, unicode.IsControl)
}

type Visibility struct {
	School     bool `json:"school"`
	Badge      bool `json:"badge"`
	Department bool `json:"department"`
	Grade      bool `json:"grade"`
	Reviews    bool `json:"reviews"`
	Resources  bool `json:"resources"`
}

func DefaultVisibility() Visibility {
	return Visibility{School: true, Badge: true, Department: true, Reviews: true, Resources: true}
}

type Profile struct {
	GroupCardConsent GroupCardConsent `json:"group_card_consent"`
	ID               string           `json:"id"`
	Nickname         string           `json:"nickname"`
	NicknameRequired bool             `json:"nickname_required"`
	Bio              string           `json:"bio"`
	Visibility       Visibility       `json:"visibility"`
	Revision         int64            `json:"revision"`
}
type Update struct {
	GroupCardConsent *GroupCardConsent `json:"group_card_consent,omitempty"`
	Nickname         string            `json:"nickname"`
	Revision         int64             `json:"revision"`
	Bio              *string           `json:"bio,omitempty"`
	Visibility       *Visibility       `json:"visibility,omitempty"`
}
type Affiliation struct {
	SchoolID   string `json:"school_id,omitempty"`
	SchoolName string `json:"school_name,omitempty"`
	Department string `json:"department,omitempty"`
	Grade      string `json:"grade,omitempty"`
	Badge      string `json:"badge,omitempty"`
}
type PublicRecord struct {
	Profile Profile
	Owner   Principal
}
type PublicFacts interface {
	ProfileFacts(context.Context, Principal, Visibility) ([]Affiliation, error)
}
type PublicProfile struct {
	Affiliations []Affiliation `json:"affiliations"`
	FactsState   string        `json:"facts_state"`
	ID           string        `json:"id"`
	Nickname     string        `json:"nickname"`
	Bio          string        `json:"bio"`
	Visibility   Visibility    `json:"visibility"`
}
type Repository interface {
	Ready(context.Context) error
	Get(context.Context, Principal) (Profile, error)
	Public(context.Context, string) (PublicRecord, error)
	Ensure(context.Context, Principal, string) (Profile, error)
	Update(context.Context, Principal, string, Update) (Profile, error)
}
type Service struct {
	Information      InformationAuthority
	GroupCards       GroupCardProvider
	repo             Repository
	Now              func() time.Time
	ClosureAuthority ClosureAuthority
	ClosureEffects   ClosureEffects
	Facts            PublicFacts
	closureMu        sync.Mutex
	closureTurns     map[string]chan struct{}
}

func New(repo Repository) (*Service, error) {
	if repo == nil {
		return nil, ErrUnavailable
	}
	return &Service{repo: repo}, nil
}
func ValidName(name string) bool {
	return ValidateNickname(name) == nil
}

// InitialNickname validates each source independently and never changes SSO.
func InitialNickname(displayName, username string) string {
	for _, v := range []string{displayName, username} {
		if ValidName(v) {
			return v
		}
	}
	return ""
}
func (s *Service) Ready(ctx context.Context) error { return s.repo.Ready(ctx) }
func (s *Service) Get(ctx context.Context, p Principal) (Profile, error) {
	if !p.Valid() {
		return Profile{}, inputError("principal", "principal_invalid")
	}
	return s.repo.Get(ctx, p)
}

// An empty initial name records the explicit completion state without failing login.
// Ensure never overwrites a profile created during an earlier login.
func (s *Service) Ensure(ctx context.Context, p Principal, name string) (Profile, error) {
	if !p.Valid() {
		return Profile{}, inputError("principal", "principal_invalid")
	}
	if name != "" {
		if err := ValidateNickname(name); err != nil {
			return Profile{}, err
		}
	}
	return s.repo.Ensure(ctx, p, name)
}
func (s *Service) Save(ctx context.Context, p Principal, key string, revision int64, name string) (Profile, error) {
	return s.Update(ctx, p, key, Update{Nickname: name, Revision: revision})
}
func (s *Service) Update(ctx context.Context, p Principal, key string, in Update) (Profile, error) {
	if !p.Valid() {
		return Profile{}, inputError("principal", "principal_invalid")
	}
	if !validIdempotencyKey(key) {
		return Profile{}, inputError("idempotency_key", "idempotency_key_invalid")
	}
	if in.Revision < 1 {
		return Profile{}, inputError("revision", "revision_invalid")
	}
	if err := ValidateNickname(in.Nickname); err != nil {
		return Profile{}, err
	}
	if in.Bio != nil {
		normalized := NormalizeBio(*in.Bio)
		if err := ValidateBio(normalized); err != nil {
			return Profile{}, err
		}
		in.Bio = &normalized
	}
	return s.repo.Update(ctx, p, key, in)
}
func (s *Service) Public(ctx context.Context, id string) (PublicProfile, error) {
	if !validUUID(id) {
		return PublicProfile{}, ErrNotFound
	}
	record, e := s.repo.Public(ctx, id)
	if e != nil {
		return PublicProfile{}, e
	}
	return s.composePublicProfile(ctx, record.Owner, record.Profile), nil
}

// All six flags are required so an incomplete update cannot silently clear consent.
func (v *Visibility) UnmarshalJSON(b []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil || len(fields) != 6 {
		return inputError("visibility", "visibility_invalid")
	}
	values := map[string]*bool{"school": &v.School, "badge": &v.Badge, "department": &v.Department, "grade": &v.Grade, "reviews": &v.Reviews, "resources": &v.Resources}
	for key, target := range values {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" || json.Unmarshal(raw, target) != nil {
			return inputError("visibility", "visibility_invalid")
		}
	}
	return nil
}
