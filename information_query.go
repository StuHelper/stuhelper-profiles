package profiles

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

func (s *Service) Query(ctx context.Context, p Principal, q Query) ([]Record, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if e := informationInput(q, 100); e != nil {
		return nil, e
	}
	target, ok := informationAccount(q.Value)
	if q.Kind != "account" || !ok {
		return nil, ErrInput
	}
	if len(q.Expand) > 0 {
		return nil, ErrInput
	}
	if e := s.informationAuthorize(ctx, p, "information-query", q.Scope, ""); e != nil {
		return nil, e
	}
	if e := s.informationAuthorize(ctx, p, "information-query", q.Scope, q.Value); errors.Is(e, ErrForbidden) {
		return []Record{}, nil
	} else if e != nil {
		return nil, e
	}
	profile, e := s.repo.Get(ctx, target)
	if errors.Is(e, ErrNotFound) {
		return []Record{}, nil
	}
	if e != nil {
		return nil, ErrUnavailable
	}
	if profile.ID <= q.After {
		return []Record{}, nil
	}
	visibility, _ := json.Marshal(profile.Visibility)
	consent, _ := json.Marshal(profile.GroupCardConsent)
	reader, ok := s.repo.(interface {
		InformationChangedAt(context.Context, Principal) (time.Time, error)
	})
	if !ok {
		return nil, ErrUnavailable
	}
	changedAt, e := reader.InformationChangedAt(ctx, target)
	if e != nil {
		return nil, ErrUnavailable
	}
	scope := q.Scope
	scope.Module = "profiles"
	out := []Record{{ID: profile.ID, Subject: q.Value, Scope: scope, Source: "profiles", Current: true, ChangedAt: changedAt, Fields: []Field{{Name: "nickname", Category: "business", Value: profile.Nickname}, {Name: "bio", Category: "business", Value: profile.Bio}, {Name: "visibility", Category: "business", Value: string(visibility)}, {Name: "group_card_consent", Category: "business", Value: string(consent)}}}}
	return s.informationFields(ctx, p, q, out)
}
