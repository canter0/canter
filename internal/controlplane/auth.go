package controlplane

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

func newSecret(prefix string, bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func secretHash(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func hashPassword(password string) (string, error) {
	if err := validateNewPassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	var memory uint32
	var iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false
	}
	if memory < 8*uint32(threads) || memory > argonMemory || iterations == 0 || iterations > 10 || threads == 0 || threads > 16 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) != argonKeyLen {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(actual, want) == 1
}

func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || len(email) > 254 || !strings.Contains(email, "@") {
		return "", fmt.Errorf("valid email is required")
	}
	return email, nil
}

func newID(prefix string) (string, error) {
	value, err := newSecret("", 12)
	if err != nil {
		return "", err
	}
	return prefix + value, nil
}

func newUserCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	for i := range raw {
		raw[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(raw[:4]) + "-" + string(raw[4:]), nil
}

func validateNewPassword(password string) error {
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < 15 || utf8.RuneCountInString(password) > 1024 {
		return fmt.Errorf("use a password between 15 and 1024 characters")
	}
	if passwordDenied(password, commonPasswordDenylist) {
		return fmt.Errorf("this password is too common; choose another")
	}
	lower := strings.ToLower(strings.TrimSpace(password))
	for _, word := range []string{"password", "1234567890", "qwerty", "letmein", "iloveyou", "123456789", "abcdefgh"} {
		if strings.ReplaceAll(lower, word, "") == "" {
			return fmt.Errorf("choose a less common password or a longer passphrase")
		}
	}
	return nil
}

var dummyPasswordHash = func() string { h, _ := hashPassword("Canter dummy comparison 6a4e7c2b"); return h }()
