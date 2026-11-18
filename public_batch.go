package profiles

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxPublicBatchSize = 100

type PublicFactsRequest struct {
	Owner      Principal
	Visibility Visibility
}

type BatchPublicFacts interface {
	ProfileFactsBatch(ctx context.Context, requests []PublicFactsRequest) (map[Principal][]Affiliation, error)
}

type PublicBatchRepository interface {
	PublicByPrincipals(ctx context.Context, owners []Principal) ([]PublicRecord, error)
	PublicByIDs(ctx context.Context, ids []string) ([]PublicRecord, error)
}

func needsPublicFacts(v Visibility) bool {
	return v.School || v.Badge || v.Department || v.Grade
}

func filterPublicFacts(vis Visibility, facts []Affiliation) ([]Affiliation, bool) {
	if len(facts) > 100 {
		return nil, false
	}
	filtered := []Affiliation{}
	for _, fact := range facts {
		if !vis.School {
			fact.SchoolID = ""
			fact.SchoolName = ""
		}
		if !vis.Badge {
			fact.Badge = ""
		}
		if !vis.Department {
			fact.Department = ""
		}
		if !vis.Grade {
			fact.Grade = ""
		}
		for _, value := range []string{fact.SchoolID, fact.SchoolName, fact.Department, fact.Grade} {
			if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 200 || strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || isBidiControl(r) }) {
				return nil, false
			}
		}
		if fact.Badge != "" && fact.Badge != "student" && fact.Badge != "teacher" {
			return nil, false
		}
		if fact != (Affiliation{}) {
			filtered = append(filtered, fact)
		}
	}
	return filtered, true
}

func basePublicProfile(v Profile) PublicProfile {
	state := "unavailable"
	if !needsPublicFacts(v.Visibility) {
		state = "hidden"
	}
	return PublicProfile{
		ID:              v.ID,
		Nickname:        v.Nickname,
		AvatarVersionID: v.AvatarVersionID,
		Bio:             v.Bio,
		Visibility:      v.Visibility,
		Affiliations:    []Affiliation{},
		FactsState:      state,
	}
}

func (s *Service) resolvePublicRecords(ctx context.Context, records []PublicRecord) []PublicProfile {
	out := make([]PublicProfile, len(records))
	var requests []PublicFactsRequest
	reqIdx := make([]int, 0, len(records))
	for i, r := range records {
		out[i] = basePublicProfile(r.Profile)
		if out[i].FactsState == "unavailable" {
			requests = append(requests, PublicFactsRequest{Owner: r.Owner, Visibility: r.Profile.Visibility})
			reqIdx = append(reqIdx, i)
		}
	}
	if len(requests) == 0 || s.Facts == nil {
		return out
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if batchProvider, ok := s.Facts.(BatchPublicFacts); ok {
		byOwner, err := batchProvider.ProfileFactsBatch(callCtx, requests)
		if err != nil {
			return out
		}
		for _, i := range reqIdx {
			facts, exists := byOwner[records[i].Owner]
			if !exists {
				facts = nil
			}
			if filtered, valid := filterPublicFacts(records[i].Profile.Visibility, facts); valid {
				out[i].Affiliations = filtered
				out[i].FactsState = "available"
			}
		}
		return out
	}

	for _, i := range reqIdx {
		facts, err := s.Facts.ProfileFacts(callCtx, records[i].Owner, records[i].Profile.Visibility)
		if err != nil {
			continue
		}
		if filtered, valid := filterPublicFacts(records[i].Profile.Visibility, facts); valid {
			out[i].Affiliations = filtered
			out[i].FactsState = "available"
		}
	}
	return out
}

// PublicBatch reads up to 100 deduplicated active published profiles by Principal in one query.
func (s *Service) PublicBatch(ctx context.Context, principals []Principal) (map[Principal]PublicProfile, error) {
	if len(principals) == 0 {
		return map[Principal]PublicProfile{}, nil
	}
	seen := make(map[Principal]struct{}, len(principals))
	unique := make([]Principal, 0, len(principals))
	for _, p := range principals {
		if !p.Valid() {
			return nil, ErrInput
		}
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			unique = append(unique, p)
		}
	}
	if len(unique) > maxPublicBatchSize {
		return nil, ErrInput
	}
	r, ok := s.repo.(PublicBatchRepository)
	if !ok {
		return nil, ErrUnavailable
	}
	dbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	records, err := r.PublicByPrincipals(dbCtx, unique)
	cancel()
	if err != nil {
		return nil, err
	}
	resolved := s.resolvePublicRecords(ctx, records)
	out := make(map[Principal]PublicProfile, len(records))
	for i, rec := range records {
		out[rec.Owner] = resolved[i]
	}
	return out, nil
}

// PublicByIDs reads up to 100 deduplicated active published profiles by public UUID in one query.
func (s *Service) PublicByIDs(ctx context.Context, ids []string) (map[string]PublicProfile, error) {
	if len(ids) == 0 {
		return map[string]PublicProfile{}, nil
	}
	seen := make(map[string]struct{}, len(ids))
	validIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if validUUID(id) {
			validIDs = append(validIDs, id)
		}
	}
	if len(seen) > maxPublicBatchSize {
		return nil, ErrInput
	}
	if len(validIDs) == 0 {
		return map[string]PublicProfile{}, nil
	}
	r, ok := s.repo.(PublicBatchRepository)
	if !ok {
		return nil, ErrUnavailable
	}
	dbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	records, err := r.PublicByIDs(dbCtx, validIDs)
	cancel()
	if err != nil {
		return nil, err
	}
	resolved := s.resolvePublicRecords(ctx, records)
	out := make(map[string]PublicProfile, len(records))
	for i, rec := range records {
		out[rec.Profile.ID] = resolved[i]
	}
	return out, nil
}
