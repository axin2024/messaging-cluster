package credential

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"regexp"

	"golang.org/x/crypto/bcrypt"
)

var slug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func ValidKey(key string) bool {
	return slug.MatchString(key)
}

func ValidPassword(password string) bool {
	return len(password) >= 12 && len(password) <= 72
}

func HashPassword(password string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
}

func VerifyPassword(hash []byte, password string) bool {
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}

func IssueToken() (string, [32]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [32]byte{}, err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), sha256.Sum256(raw[:]), nil
}

func TokenDigest(token string) ([32]byte, bool) {
	if len(token) != base64.RawURLEncoding.EncodedLen(32) {
		return [32]byte{}, false
	}
	var raw [32]byte
	size, err := base64.RawURLEncoding.Strict().Decode(raw[:], []byte(token))
	if err != nil || size != len(raw) {
		return [32]byte{}, false
	}
	return sha256.Sum256(raw[:]), true
}
