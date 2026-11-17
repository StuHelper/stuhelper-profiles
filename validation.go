package profiles

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// InputError carries field-level validation metadata while unwrapping to ErrInput
// and keeping Error() equal to "invalid_profile" for wire code compatibility.
type InputError struct {
	Field  string `json:"field,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (e *InputError) Error() string { return ErrInput.Error() }
func (e *InputError) Unwrap() error { return ErrInput }

func inputError(field, reason string) error {
	return &InputError{Field: field, Reason: reason}
}

func ValidationDetail(err error) (field, reason string, ok bool) {
	var ie *InputError
	if errors.As(err, &ie) && ie != nil {
		return ie.Field, ie.Reason, true
	}
	return "", "", false
}

// NormalizeBio canonicalizes Windows CRLF line breaks to LF prior to length and character checks.
func NormalizeBio(bio string) string {
	return strings.ReplaceAll(bio, "\r\n", "\n")
}

func ValidateNickname(name string) error {
	if !utf8.ValidString(name) {
		return inputError("nickname", "nickname_invalid_char")
	}
	if name == "" {
		return inputError("nickname", "nickname_empty")
	}
	if name != strings.TrimSpace(name) {
		return inputError("nickname", "nickname_whitespace")
	}
	if utf8.RuneCountInString(name) > 20 {
		return inputError("nickname", "nickname_too_long")
	}
	if strings.ContainsFunc(name, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs)
	}) {
		return inputError("nickname", "nickname_invalid_char")
	}
	return nil
}

func ValidateBio(bio string) error {
	if !utf8.ValidString(bio) {
		return inputError("bio", "bio_invalid_char")
	}
	if utf8.RuneCountInString(bio) > 120 {
		return inputError("bio", "bio_too_long")
	}
	if strings.ContainsFunc(bio, func(r rune) bool {
		return (unicode.IsControl(r) && r != '\n') || unicode.In(r, unicode.Co, unicode.Cs)
	}) {
		return inputError("bio", "bio_invalid_char")
	}
	return nil
}
