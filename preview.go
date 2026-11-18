package profiles

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Preview struct {
	Nickname   *string     `json:"nickname,omitempty"`
	Bio        *string     `json:"bio,omitempty"`
	Visibility *Visibility `json:"visibility,omitempty"`
}

func (s *Service) composePublicProfile(ctx context.Context, owner Principal, v Profile) PublicProfile {
	out := PublicProfile{
		ID:           v.ID,
		Nickname:     v.Nickname,
		Bio:          v.Bio,
		Visibility:   v.Visibility,
		Affiliations: []Affiliation{},
		FactsState:   "hidden",
	}
	if !v.Visibility.School && !v.Visibility.Badge && !v.Visibility.Department && !v.Visibility.Grade {
		return out
	}
	out.FactsState = "unavailable"
	if s.Facts == nil {
		return out
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	facts, e := s.Facts.ProfileFacts(callCtx, owner, v.Visibility)
	if e != nil || len(facts) > 100 {
		return out
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
			if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 200 || strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || isBidiControl(r) }) {
				return out
			}
		}
		if fact.Badge != "" && fact.Badge != "student" && fact.Badge != "teacher" {
			return out
		}
		if fact != (Affiliation{}) {
			filtered = append(filtered, fact)
		}
	}
	out.Affiliations = filtered
	out.FactsState = "available"
	return out
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
	return s.composePublicProfile(ctx, p, draft), nil
}
