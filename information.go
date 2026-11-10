package profiles

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// Scope is a provider-owned copy of the platform administrative scope.
type Scope struct{ School, Group, Module string }
type Field struct {
	Name, Category, Value string
	Masked                bool
}
type Record struct {
	ID, Subject string
	Scope       Scope
	Source      string
	Current     bool
	ChangedAt   time.Time
	Fields      []Field
}
type Query struct {
	Kind, Value    string
	Scope          Scope
	Purpose        string
	Fields, Expand []string
	Limit          int
	After          string
}
type Lead struct {
	Kind, Value string
	Scope       Scope
	Purpose     string
	Limit       int
	After       string
}
type Candidate struct {
	Account, Source string
	Current         bool
	ChangedAt       time.Time
	Scope           Scope
	Label           []Field
}

func (q Query) String() string   { return "information query [redacted]" }
func (q Query) GoString() string { return q.String() }
func (l Lead) String() string    { return "information lead [redacted]" }
func (l Lead) GoString() string  { return l.String() }

// InformationAuthority is supplied by the host. It checks current domain grants
// and target membership in the requested school/group, never treating a caller's
// scope as a grant. Empty target checks admission only. Denials must be audited
// by AuthorizeInformation using a sanitized reason code. Audit must durably record
// actor, request ID, purpose, internal targets, scope, categories, permission basis,
// channel, result and count. Request/channel metadata comes from trusted context.
type InformationAuthority interface {
	AuthorizeInformation(context.Context, Principal, string, Scope, string) error
	AuditInformation(context.Context, Principal, string, Scope, []string, []string) error
}

func informationAccount(value string) (Principal, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return Principal{}, false
	}
	decoded := make([]string, 2)
	for i, part := range parts {
		b, e := base64.RawURLEncoding.DecodeString(part)
		if e != nil || len(b) == 0 || len(b) > 512 || base64.RawURLEncoding.EncodeToString(b) != part {
			return Principal{}, false
		}
		decoded[i] = string(b)
	}
	return Principal{Issuer: decoded[0], Subject: decoded[1]}, true
}
func informationError(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, ErrForbidden) {
		return ErrForbidden
	}
	return ErrUnavailable
}
func (s *Service) informationAuthorize(ctx context.Context, p Principal, cap string, scope Scope, target string) error {
	if p.Issuer == "" || p.Subject == "" {
		return ErrForbidden
	}
	if scope.Module != "" && scope.Module != "profiles" {
		return ErrForbidden
	}
	if s.Information == nil {
		return ErrUnavailable
	}
	return informationError(s.Information.AuthorizeInformation(ctx, p, "profiles."+cap, scope, target))
}
func informationInput(q Query, max int) error {
	if q.Limit < 1 || q.Limit > max || len(q.After) > 256 || strings.TrimSpace(q.Purpose) == "" || len(q.Purpose) > 500 || len(q.Value) > 2048 || len(q.Fields) > 64 || len(q.Expand) > 3 || len(q.Scope.School) > 128 || len(q.Scope.Group) > 128 {
		return ErrInput
	}
	return nil
}
func (s *Service) informationFields(ctx context.Context, p Principal, q Query, records []Record) ([]Record, error) {
	selected := map[string]bool{}
	for _, f := range q.Fields {
		if selected[f] || f == "" {
			return nil, ErrInput
		}
		selected[f] = true
	}
	expanded := map[string]bool{}
	for _, cat := range q.Expand {
		if expanded[cat] || (cat != "identity" && cat != "document" && cat != "material") {
			return nil, ErrInput
		}
		expanded[cat] = true
		if e := s.informationAuthorize(ctx, p, "information-"+cat, q.Scope, ""); e != nil {
			return nil, e
		}
	}
	targets := []string{}
	for i := range records {
		r := &records[i]
		for cat := range expanded {
			if e := s.informationAuthorize(ctx, p, "information-"+cat, r.Scope, r.Subject); e != nil {
				return nil, e
			}
		}
		fields := []Field{}
		for _, f := range r.Fields {
			if len(selected) > 0 && !selected[f.Name] {
				continue
			}
			if f.Category != "business" && !expanded[f.Category] {
				f.Value = "***"
				f.Masked = true
			}
			fields = append(fields, f)
		}
		r.Fields = fields
		targets = append(targets, r.ID)
	}
	if len(expanded) > 0 {
		if e := s.Information.AuditInformation(ctx, p, q.Purpose, q.Scope, targets, q.Expand); e != nil {
			return nil, ErrUnavailable
		}
	}
	return records, nil
}
