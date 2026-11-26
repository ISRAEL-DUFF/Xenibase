// Package provision implements the shared-tier project flows (spec §6):
// create, rotate password, and delete, each run as an operation with
// idempotent steps and compensating rollback.
package provision

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxNameLen = 64
	maxSlugLen = 40 // the slug lives only in Xenibase's metadata
)

// ErrInvalid wraps validation failures that are the caller's fault.
var ErrInvalid = errors.New("invalid request")

// ValidateName checks a project's display name.
func ValidateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return "", fmt.Errorf("%w: name must be at most %d characters", ErrInvalid, maxNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: name must not contain control characters", ErrInvalid)
		}
	}
	return name, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a display name into a lowercase identifier made of
// [a-z0-9_] that starts with a letter, e.g. "My Blog!" -> "my_blog".
func Slugify(name string) (string, error) {
	s := nonSlug.ReplaceAllString(strings.ToLower(name), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "", fmt.Errorf("%w: name must contain at least one ASCII letter or digit", ErrInvalid)
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "p_" + s
	}
	if len(s) > maxSlugLen {
		s = strings.TrimRight(s[:maxSlugLen], "_")
	}
	return s, nil
}

const suffixAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// randomSuffix returns n random characters from [a-z0-9].
func randomSuffix(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		// 256 % 36 != 0, but the slight bias is irrelevant for a uniqueness suffix.
		b[i] = suffixAlphabet[int(b[i])%len(suffixAlphabet)]
	}
	return string(b), nil
}

// opaqueAlphabet is lowercase base32: 5 bits per character, no bias.
const opaqueAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

// opaqueLen is the random part of an opaque database name (50 bits).
const opaqueLen = 10

var opaqueRe = regexp.MustCompile(`^p_[a-z2-7]{10}$`)

// IsOpaque reports whether dbName is an opaque database name (V2 §10.2).
func IsOpaque(dbName string) bool { return opaqueRe.MatchString(dbName) }

// OpaqueDBName returns a new database name like p_7f3k9x2m4q. Nothing about
// the project is in it, so other tenants reading pg_database learn nothing
// (V2 §10.2).
func OpaqueDBName() (string, error) {
	b := make([]byte, opaqueLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = opaqueAlphabet[b[i]&31]
	}
	return "p_" + string(b), nil
}

// The suffixes are the owner, read-only, console and personal roles, and
// backend services' request roles and edge login (V4 §2).
var opaqueRoleRe = regexp.MustCompile(`^p_[a-z2-7]{10}(_owner|_ro|_console|_console_ro|_u_[a-z0-9]{6}|_anon|_user|_service|_edge|_auth_hook)$`)

// IsOpaqueRole reports whether role is one of an opaque project's roles.
func IsOpaqueRole(role string) bool { return opaqueRoleRe.MatchString(role) }

// OwnerRoleName is a project's owner (login) role for dbName.
func OwnerRoleName(dbName string) string { return dbName + "_owner" }

// Names returns a new project's opaque database and owner role names, e.g.
// p_7f3k9x2m4q and p_7f3k9x2m4q_owner.
func Names() (dbName, ownerRole string, err error) {
	if dbName, err = OpaqueDBName(); err != nil {
		return "", "", err
	}
	return dbName, OwnerRoleName(dbName), nil
}

// GeneratePassword returns a password from 32 random bytes, URL-safe so it
// can sit in a connection string without escaping.
func GeneratePassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
