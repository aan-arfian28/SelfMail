package service

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/net/idna"

	"selfmail/internal/cloudflare"
	"selfmail/internal/stalwart"
	"selfmail/internal/store"
)

// Error is an explicit, structured API error (FR-23).
type Error struct {
	Status     int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Details    any    `json:"details,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func Bad(msg string, details any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: msg, Details: details}
}
func NotFound(what string) *Error {
	return &Error{Status: http.StatusNotFound, Code: "not_found", Message: what + " not found"}
}
func Conflict(code, msg string, details any) *Error {
	return &Error{Status: http.StatusConflict, Code: code, Message: msg, Details: details}
}

// Unavailable builds a 503 with a positive retry_after.
func Unavailable(what string) *Error {
	return &Error{Status: http.StatusServiceUnavailable, Code: "dependency_unavailable",
		Message: what + " is unavailable; the operation was not completed, retry later", RetryAfter: 30}
}

// wrap converts dependency errors into explicit API errors.
func wrap(err error, what string) error {
	if err == nil {
		return nil
	}
	var se *Error
	switch {
	case errors.As(err, &se):
		return se
	case errors.Is(err, stalwart.ErrUnavailable):
		return Unavailable("Stalwart mail server")
	case errors.Is(err, cloudflare.ErrUnavailable):
		return Unavailable("Cloudflare API")
	case errors.Is(err, store.ErrNotFound):
		return NotFound(what)
	case errors.Is(err, store.ErrConflict):
		return Conflict("conflict", what+" already exists", nil)
	}
	return fmt.Errorf("%s: %w", what, err)
}

var localRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}$`)

func validLocalPart(l string) error {
	if !localRe.MatchString(l) || strings.Contains(l, "..") {
		return Bad("invalid local_part: use 1-64 characters from a-z, 0-9, '.', '_', '+', '-' and start with a letter or digit", nil)
	}
	return nil
}

// NormalizeDomain returns the unicode and punycode (ASCII) forms (IDN support).
func NormalizeDomain(in string) (uni, ascii string, err error) {
	in = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(in), ".")))
	if in == "" {
		return "", "", Bad("domain name is required", nil)
	}
	ascii, err = idna.Lookup.ToASCII(in)
	if err != nil || len(ascii) > 253 || !strings.Contains(ascii, ".") {
		return "", "", Bad("invalid domain name", nil)
	}
	for _, l := range strings.Split(ascii, ".") {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return "", "", Bad("invalid domain name", nil)
		}
	}
	uni, err = idna.Lookup.ToUnicode(ascii)
	if err != nil {
		uni = ascii
	}
	return uni, ascii, nil
}

// SplitAddress lower-cases and splits an email address, normalising the domain to punycode.
func SplitAddress(addr string) (local, domain string, err error) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	i := strings.LastIndex(addr, "@")
	if i <= 0 || i == len(addr)-1 {
		return "", "", Bad("invalid email address", nil)
	}
	_, ascii, err := NormalizeDomain(addr[i+1:])
	if err != nil {
		return "", "", err
	}
	return addr[:i], ascii, nil
}
