package controlplane

import (
	"bufio"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

// Source: SecLists Passwords/Common-Credentials/10k-most-common.txt, retrieved
// 2026-09-28. License: authdata/LICENSE.SecLists. This is a bounded denylist,
// not a claim to contain every breached password. No runtime network requests.
//
//go:embed authdata/common-password-sha256.txt
var commonPasswordHashes string
var commonPasswordDenylist = func() map[[32]byte]struct{} {
	v, err := ReadPasswordDenylist(strings.NewReader(commonPasswordHashes))
	if err != nil {
		panic(err)
	}
	return v
}()

// ReadPasswordDenylist loads one hexadecimal SHA-256 digest per line. Operators
// can supply a curated offline corpus without sending passwords to a provider.
func ReadPasswordDenylist(r io.Reader) (map[[32]byte]struct{}, error) {
	out := map[[32]byte]struct{}{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		raw, err := hex.DecodeString(line)
		if err != nil || len(raw) != 32 {
			return nil, errors.New("password denylist must contain SHA-256 hashes")
		}
		var key [32]byte
		copy(key[:], raw)
		out[key] = struct{}{}
	}
	return out, scanner.Err()
}
func passwordDenied(password string, list map[[32]byte]struct{}) bool {
	_, found := list[sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(password))))]
	return found
}
func (h *HTTPServer) checkPassword(password string) error {
	if err := validateNewPassword(password); err != nil {
		return err
	}
	if passwordDenied(password, h.config.Auth.PasswordDenylist) {
		return errors.New("this password is known to be unsafe; choose another")
	}
	return nil
}
