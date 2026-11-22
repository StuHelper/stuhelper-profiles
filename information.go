package profiles

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
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

func (q Query) String() string   { return "information query [redacted]" }
func (q Query) GoString() string { return q.String() }

// InformationAuthority is supplied by the host. It checks current domain grants
// and target membership in the requested school/group, never treating a caller's
// scope as a grant. Empty target checks admission only. Denials must be audited
// by AuthorizeInformation using a sanitized reason code. AuditInformation keeps
// the shared cross-module authority adapter signature, while profiles owns only
// business-category fields and rejects sensitive category expansion.
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
	target := Principal{Issuer: decoded[0], Subject: decoded[1]}
	return target, target.Valid()
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
	if !p.Valid() {
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
func validInformationField(name string) bool {
	switch name {
	case "nickname", "avatar_version_id", "bio", "visibility", "group_card_consent":
		return true
	}
	return false
}

func informationInput(q Query, max int) error {
	if q.Limit < 1 || q.Limit > max || (q.After != "" && !validUUID(q.After)) || strings.TrimSpace(q.Purpose) == "" || len(q.Purpose) > 500 || !utf8.ValidString(q.Purpose) || strings.ContainsRune(q.Purpose, 0) || len(q.Value) > 2048 || len(q.Fields) > 64 || len(q.Expand) > 3 || len(q.Scope.School) > 128 || len(q.Scope.Group) > 128 {
		return ErrInput
	}
	for _, s := range []string{q.Scope.School, q.Scope.Group, q.Scope.Module} {
		if !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
			return ErrInput
		}
	}
	selected := make(map[string]bool, len(q.Fields))
	for _, f := range q.Fields {
		if !validInformationField(f) || selected[f] {
			return ErrInput
		}
		selected[f] = true
	}
	return nil
}
func filterInformationFields(q Query, records []Record) ([]Record, error) {
	selected := make(map[string]bool, len(q.Fields))
	for _, f := range q.Fields {
		if !validInformationField(f) || selected[f] {
			return nil, ErrInput
		}
		selected[f] = true
	}
	if len(selected) == 0 {
		return records, nil
	}
	for i := range records {
		r := &records[i]
		fields := make([]Field, 0, len(r.Fields))
		for _, f := range r.Fields {
			if selected[f.Name] {
				fields = append(fields, f)
			}
		}
		r.Fields = fields
	}
	return records, nil
}
