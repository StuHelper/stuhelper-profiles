// Package profiles owns public profiles independently from SSO identity.
package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

func (p Principal) Valid() bool {
	return p.Issuer != "" && p.Subject != "" && len(p.Issuer) <= 512 && len(p.Subject) <= 256
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
	ID               string     `json:"id"`
	Nickname         string     `json:"nickname"`
	NicknameRequired bool       `json:"nickname_required"`
	Bio              string     `json:"bio"`
	Visibility       Visibility `json:"visibility"`
	Revision         int64      `json:"revision"`
}
type Update struct {
	Nickname   string      `json:"nickname"`
	Revision   int64       `json:"revision"`
	Bio        *string     `json:"bio,omitempty"`
	Visibility *Visibility `json:"visibility,omitempty"`
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
	repo             Repository
	Now              func() time.Time
	ClosureAuthority ClosureAuthority
	ClosureEffects   ClosureEffects
	Facts            PublicFacts
}

func New(repo Repository) (*Service, error) {
	if repo == nil {
		return nil, ErrUnavailable
	}
	return &Service{repo: repo}, nil
}
func ValidName(name string) bool {
	return name == strings.TrimSpace(name) && utf8.ValidString(name) && utf8.RuneCountInString(name) >= 1 && utf8.RuneCountInString(name) <= 20 && !strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs) })
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
		return Profile{}, ErrInput
	}
	return s.repo.Get(ctx, p)
}

// An empty initial name records the explicit completion state without failing login.
// Ensure never overwrites a profile created during an earlier login.
func (s *Service) Ensure(ctx context.Context, p Principal, name string) (Profile, error) {
	if !p.Valid() || (name != "" && !ValidName(name)) {
		return Profile{}, ErrInput
	}
	return s.repo.Ensure(ctx, p, name)
}
func (s *Service) Save(ctx context.Context, p Principal, key string, revision int64, name string) (Profile, error) {
	return s.Update(ctx, p, key, Update{Nickname: name, Revision: revision})
}
func (s *Service) Update(ctx context.Context, p Principal, key string, in Update) (Profile, error) {
	if !p.Valid() || len(key) < 1 || len(key) > 128 || in.Revision < 1 || !ValidName(in.Nickname) {
		return Profile{}, ErrInput
	}
	if in.Bio != nil && (!utf8.ValidString(*in.Bio) || utf8.RuneCountInString(*in.Bio) > 120 || strings.ContainsFunc(*in.Bio, func(r rune) bool { return (unicode.IsControl(r) && r != '\n') || unicode.In(r, unicode.Co, unicode.Cs) })) {
		return Profile{}, ErrInput
	}
	return s.repo.Update(ctx, p, key, in)
}
func (s *Service) Public(ctx context.Context, id string) (PublicProfile, error) {
	if len(id) != 36 || strings.ContainsAny(id, "/\\ \n") {
		return PublicProfile{}, ErrNotFound
	}
	record, e := s.repo.Public(ctx, id)
	if e != nil {
		return PublicProfile{}, e
	}
	v := record.Profile
	out := PublicProfile{ID: v.ID, Nickname: v.Nickname, Bio: v.Bio, Visibility: v.Visibility, Affiliations: []Affiliation{}, FactsState: "hidden"}
	if !v.Visibility.School && !v.Visibility.Badge && !v.Visibility.Department && !v.Visibility.Grade {
		return out, nil
	}
	out.FactsState = "unavailable"
	if s.Facts == nil {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	facts, e := s.Facts.ProfileFacts(ctx, record.Owner, v.Visibility)
	if e != nil || len(facts) > 100 {
		return out, nil
	}
	filtered := []Affiliation{}
	for _, fact := range facts {
		if !v.Visibility.School {
			fact.SchoolID = ""
			fact.SchoolName = ""
		}
		if !v.Visibility.Badge {
			fact.Badge = ""
		}
		if !v.Visibility.Department {
			fact.Department = ""
		}
		if !v.Visibility.Grade {
			fact.Grade = ""
		}
		for _, value := range []string{fact.SchoolID, fact.SchoolName, fact.Department, fact.Grade} {
			if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 200 || strings.ContainsFunc(value, unicode.IsControl) {
				return out, nil
			}
		}
		if fact.Badge != "" && fact.Badge != "student" && fact.Badge != "teacher" {
			return out, nil
		}
		if fact != (Affiliation{}) {
			filtered = append(filtered, fact)
		}
	}
	out.Affiliations = filtered
	out.FactsState = "available"
	return out, nil
}

// All six flags are required so an incomplete update cannot silently clear consent.
func (v *Visibility) UnmarshalJSON(b []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil || len(fields) != 6 {
		return ErrInput
	}
	values := map[string]*bool{"school": &v.School, "badge": &v.Badge, "department": &v.Department, "grade": &v.Grade, "reviews": &v.Reviews, "resources": &v.Resources}
	for key, target := range values {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" || json.Unmarshal(raw, target) != nil {
			return ErrInput
		}
	}
	return nil
}
