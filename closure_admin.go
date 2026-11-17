package profiles

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	validClosureStates   = []string{"cooling", "cancelled", "awaiting_assets", "processing", "closed"}
	validClosureBlockers = []string{"assets_unavailable", "assets_pending", "sessions_unavailable", "authorizations_unavailable", "content_unavailable"}
)

type ClosureFilter struct {
	After      string
	State      string
	Blocker    string
	Actionable bool
	Limit      int
}

type ClosurePage struct {
	Items      []Closure `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type ClosureAuditEvent struct {
	ID        int64     `json:"id"`
	ClosureID string    `json:"closure_id"`
	ProfileID string    `json:"profile_id,omitempty"`
	Actor     Principal `json:"-"`
	Action    string    `json:"action"`
	Blocker   string    `json:"blocker,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type ClosureEventFilter struct {
	ClosureID string
	After     string
	Limit     int
}

type ClosureEventPage struct {
	Items      []ClosureAuditEvent `json:"items"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

type ClosureAuditAuthority interface {
	AuthorizeClosureAudit(context.Context, Principal) error
}

type ClosureAdminRepository interface {
	QueryClosures(ctx context.Context, filter ClosureFilter, now time.Time) (ClosurePage, error)
	QueryClosureEvents(ctx context.Context, closureID string, hasCursor bool, cursorAt time.Time, cursorID int64, limit int) (ClosureEventPage, error)
}

func validClosureUUID(s string) bool {
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

func EncodeClosureEventCursor(createdAt time.Time, id int64) string {
	raw := fmt.Sprintf("%d:%d", createdAt.UTC().UnixNano(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeClosureEventCursor(cursor string) (time.Time, int64, bool) {
	if len(cursor) == 0 || len(cursor) > 128 {
		return time.Time{}, 0, false
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != cursor {
		return time.Time{}, 0, false
	}
	parts := strings.Split(string(b), ":")
	if len(parts) != 2 {
		return time.Time{}, 0, false
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, 0, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id < 1 {
		return time.Time{}, 0, false
	}
	return time.Unix(0, nanos).UTC(), id, true
}

func (s *Service) authorizeClosureAudit(ctx context.Context, p Principal) error {
	if !p.Valid() || s.ClosureAuthority == nil {
		return ErrForbidden
	}
	if auditAuth, ok := s.ClosureAuthority.(ClosureAuditAuthority); ok {
		return auditAuth.AuthorizeClosureAudit(ctx, p)
	}
	return s.ClosureAuthority.AuthorizeClosure(ctx, p)
}

func (s *Service) QueryClosures(ctx context.Context, p Principal, filter ClosureFilter) (ClosurePage, error) {
	if e := s.authorizeClosure(ctx, p); e != nil {
		return ClosurePage{}, e
	}
	if filter.Limit < 1 || filter.Limit > 100 {
		return ClosurePage{}, ErrInput
	}
	if filter.After != "" && !validClosureUUID(filter.After) {
		return ClosurePage{}, ErrInput
	}
	if filter.State != "" && !slices.Contains(validClosureStates, filter.State) {
		return ClosurePage{}, ErrInput
	}
	if filter.Blocker != "" && !slices.Contains(validClosureBlockers, filter.Blocker) {
		return ClosurePage{}, ErrInput
	}
	r, ok := s.repo.(ClosureAdminRepository)
	if !ok {
		return ClosurePage{}, ErrUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return r.QueryClosures(callCtx, filter, s.now())
}

// ListClosureEvents returns read-only closure audit records ordered newest-first
// for platform operation log aggregation without exposing target SSO identities.
func (s *Service) ListClosureEvents(ctx context.Context, p Principal, filter ClosureEventFilter) (ClosureEventPage, error) {
	if e := s.authorizeClosureAudit(ctx, p); e != nil {
		return ClosureEventPage{}, e
	}
	if filter.Limit < 1 || filter.Limit > 100 {
		return ClosureEventPage{}, ErrInput
	}
	if filter.ClosureID != "" && !validClosureUUID(filter.ClosureID) {
		return ClosureEventPage{}, ErrInput
	}
	var (
		hasCursor bool
		cursorAt  time.Time
		cursorID  int64
	)
	if filter.After != "" {
		var ok bool
		cursorAt, cursorID, ok = decodeClosureEventCursor(filter.After)
		if !ok {
			return ClosureEventPage{}, ErrInput
		}
		hasCursor = true
	}
	r, ok := s.repo.(ClosureAdminRepository)
	if !ok {
		return ClosureEventPage{}, ErrUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return r.QueryClosureEvents(callCtx, filter.ClosureID, hasCursor, cursorAt, cursorID, filter.Limit)
}
