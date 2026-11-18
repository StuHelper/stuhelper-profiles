package profiles

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const GroupCardPurpose = "group_card"

var ErrUnauthenticated = errors.New("profile_unauthenticated")

type GroupCardConsent struct {
	School     bool `json:"school"`
	Grade      bool `json:"grade"`
	Department bool `json:"department"`
	Name       bool `json:"name"`
	StudentID  bool `json:"student_id"`
	Gender     bool `json:"gender"`
	Nickname   bool `json:"nickname"`
}

func (v *GroupCardConsent) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || len(m) != 7 {
		return ErrInput
	}
	for k, p := range map[string]*bool{"school": &v.School, "grade": &v.Grade, "department": &v.Department, "name": &v.Name, "student_id": &v.StudentID, "gender": &v.Gender, "nickname": &v.Nickname} {
		raw, ok := m[k]
		if !ok || string(raw) == "null" || json.Unmarshal(raw, p) != nil {
			return ErrInput
		}
	}
	return nil
}

type CardField struct {
	Allowed   bool
	Available bool
	Value     string
}

// GroupCardProvider must validate the current service identity and the explicit
// profiles.group-card.read capability for the school on every call. Values only
// reads the requested permitted fields through the owning module's public API.
type GroupCardProvider interface {
	AuthorizeGroupCard(context.Context, Principal, string) error
	GroupCardValues(context.Context, Principal, string, string, []string) (map[string]CardField, error)
}

func (s *Service) GroupCardFields(ctx context.Context, principal Principal, owner, school, purpose string) (map[string]CardField, error) {
	if !principal.Valid() {
		return nil, ErrUnauthenticated
	}
	parts := strings.Split(owner, ":")
	if len(parts) != 2 || strings.TrimSpace(school) == "" || len(school) > 64 || !utf8.ValidString(school) || strings.ContainsFunc(school, unicode.IsControl) || purpose != GroupCardPurpose {
		return nil, ErrInput
	}
	values := make([]string, 2)
	for i, p := range parts {
		b, e := base64.RawURLEncoding.DecodeString(p)
		if e != nil || len(b) == 0 || base64.RawURLEncoding.EncodeToString(b) != p {
			return nil, ErrInput
		}
		values[i] = string(b)
	}
	target := Principal{values[0], values[1]}
	if !target.Valid() {
		return nil, ErrInput
	}
	if s.GroupCards == nil {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if e := s.GroupCards.AuthorizeGroupCard(ctx, principal, school); e != nil {
		return nil, e
	}
	if e := s.AccountAccess(ctx, target); e != nil {
		return nil, e
	}
	profile, e := s.repo.Get(ctx, target)
	if e != nil {
		return nil, e
	}
	c := profile.GroupCardConsent
	ordered := []struct {
		name    string
		allowed bool
	}{
		{"school", c.School},
		{"grade", c.Grade},
		{"department", c.Department},
		{"name", c.Name},
		{"student_id", c.StudentID},
		{"gender", c.Gender},
		{"nickname", c.Nickname},
	}
	out := make(map[string]CardField, len(ordered))
	requested := make([]string, 0, len(ordered)-1)
	for _, entry := range ordered {
		out[entry.name] = CardField{Allowed: entry.allowed}
		if entry.allowed && entry.name != "nickname" {
			requested = append(requested, entry.name)
		}
	}
	if c.Nickname {
		out["nickname"] = CardField{true, !profile.NicknameRequired, profile.Nickname}
	}
	if len(requested) > 0 {
		facts, e := s.GroupCards.GroupCardValues(ctx, principal, owner, school, requested)
		if e != nil {
			return nil, e
		}
		for _, k := range requested {
			f := facts[k]
			if !f.Available {
				continue
			}
			if f.Value == "" || !utf8.ValidString(f.Value) || utf8.RuneCountInString(f.Value) > 200 || strings.ContainsFunc(f.Value, func(r rune) bool { return unicode.IsControl(r) || isBidiControl(r) }) || (k == "gender" && f.Value != "male" && f.Value != "female" && f.Value != "unknown") {
				return nil, ErrUnavailable
			}
			out[k] = CardField{true, true, f.Value}
		}
	}
	return out, nil
}
