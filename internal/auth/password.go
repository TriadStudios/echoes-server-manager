package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// PasswordAlgo names the only supported password hash.
const PasswordAlgo = "pbkdf2-sha256"

const (
	saltBytes = 16
	keyBytes  = 32
	// maxIterations bounds the work a tampered users.json can demand.
	maxIterations = 10_000_000
)

// PasswordHash is a stored password hash.
type PasswordHash struct {
	Algo       string `json:"algo"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"` // base64, 16 bytes
	Hash       string `json:"hash"` // base64, 32 bytes
}

// HashPassword hashes pw with PBKDF2-SHA256, a fresh 16-byte salt and the
// given iteration count (DefaultIterations when iter <= 0).
func HashPassword(pw string, iter int) (PasswordHash, error) {
	if iter <= 0 {
		iter = DefaultIterations
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return PasswordHash{}, err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, iter, keyBytes)
	if err != nil {
		return PasswordHash{}, fmt.Errorf("auth: hash password: %w", err)
	}
	return PasswordHash{
		Algo:       PasswordAlgo,
		Iterations: iter,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Hash:       base64.StdEncoding.EncodeToString(key),
	}, nil
}

// VerifyPassword reports whether pw matches h, comparing in constant time.
func VerifyPassword(h PasswordHash, pw string) bool {
	if h.Algo != PasswordAlgo || h.Iterations <= 0 || h.Iterations > maxIterations {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(h.Salt)
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(h.Hash)
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, h.Iterations, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}
