// Package secret holds crypto helpers: password
// generation/validation and API token hashing.
package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"unicode"
)

const (
	lower   = "abcdefghijklmnopqrstuvwxyz"
	upper   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digits  = "0123456789"
	symbols = "!@#$%^&*()-_=+[]{}"
)

// GeneratePassword returns a random password of length n (>=12) via crypto/rand
// guaranteed to contain all four character classes.
func GeneratePassword(n int) (string, error) {
	if n < 12 {
		return "", errors.New("password length must be >= 12")
	}
	all := lower + upper + digits + symbols
	pick := func(set string) (byte, error) {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return 0, err
		}
		return set[i.Int64()], nil
	}
	out := make([]byte, n)
	for i, set := range []string{lower, upper, digits, symbols} {
		c, err := pick(set)
		if err != nil {
			return "", err
		}
		out[i] = c
	}
	for i := 4; i < n; i++ {
		c, err := pick(all)
		if err != nil {
			return "", err
		}
		out[i] = c
	}
	// Fisher-Yates shuffle
	for i := n - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		out[i], out[j.Int64()] = out[j.Int64()], out[i]
	}
	return string(out), nil
}

// ValidatePassword enforces: >= 12 chars and at least 3 of 4 character classes.
func ValidatePassword(p string) error {
	if len([]rune(p)) < 12 {
		return errors.New("password must be at least 12 characters and mix lowercase, uppercase, digits and symbols (at least 3 of 4 classes)")
	}
	var l, u, d, s bool
	for _, r := range p {
		switch {
		case unicode.IsLower(r):
			l = true
		case unicode.IsUpper(r):
			u = true
		case unicode.IsDigit(r):
			d = true
		default:
			s = true
		}
	}
	n := 0
	for _, b := range []bool{l, u, d, s} {
		if b {
			n++
		}
	}
	if n < 3 {
		return errors.New("password must be at least 12 characters and mix lowercase, uppercase, digits and symbols (at least 3 of 4 classes)")
	}
	return nil
}

// NewToken returns a random API token and its SHA-256 hex hash.
func NewToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	token = "smk_" + hex.EncodeToString(b)
	return token, HashToken(token), nil
}

func HashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}
