package controlplane

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// A software authenticator exercises the real WebAuthn verifier, including
// signed challenges, RP/origin binding, user verification, and counter updates.
type testAuthenticator struct {
	key     *ecdsa.PrivateKey
	id      []byte
	handle  []byte
	counter uint32
}

func newTestAuthenticator(t *testing.T) *testAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	if _, err = rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &testAuthenticator{key: key, id: id}
}
func b64auth(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func (tk *testAuthenticator) registration(t *testing.T, challenge, origin, rp string, uv bool) map[string]any {
	t.Helper()
	client, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": challenge, "origin": origin})
	hash := sha256.Sum256([]byte(rp))
	auth := append([]byte{}, hash[:]...)
	flags := byte(0x41)
	if uv {
		flags |= 0x04
	}
	auth = append(auth, flags)
	auth = append(auth, make([]byte, 4+16)...)
	auth = binary.BigEndian.AppendUint16(auth, uint16(len(tk.id)))
	auth = append(auth, tk.id...)
	pub, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: tk.key.X.FillBytes(make([]byte, 32)), -3: tk.key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	auth = append(auth, pub...)
	att, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": auth})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"id": b64auth(tk.id), "rawId": b64auth(tk.id), "type": "public-key", "clientExtensionResults": map[string]any{}, "response": map[string]any{"clientDataJSON": b64auth(client), "attestationObject": b64auth(att), "transports": []string{"internal"}}}
}
func (tk *testAuthenticator) assertion(t *testing.T, challenge, origin, rp string, uv bool) map[string]any {
	t.Helper()
	client, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": challenge, "origin": origin})
	hash := sha256.Sum256([]byte(rp))
	auth := append([]byte{}, hash[:]...)
	flags := byte(1)
	if uv {
		flags |= 4
	}
	auth = append(auth, flags)
	tk.counter++
	auth = binary.BigEndian.AppendUint32(auth, tk.counter)
	cd := sha256.Sum256(client)
	signed := append(append([]byte{}, auth...), cd[:]...)
	digest := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, tk.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"id": b64auth(tk.id), "rawId": b64auth(tk.id), "type": "public-key", "clientExtensionResults": map[string]any{}, "response": map[string]any{"clientDataJSON": b64auth(client), "authenticatorData": b64auth(auth), "signature": b64auth(sig), "userHandle": b64auth(tk.handle)}}
}
func passkeyOptions(t *testing.T, h *HTTPServer, route string, cookie *http.Cookie) (string, []byte, *http.Cookie) {
	t.Helper()
	w := authRequest(t, h, "passkeys/"+route+"/begin", map[string]string{"name": "Test key"}, cookie)
	requireStatus(t, w, 200)
	var out struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	handle, _ := base64.RawURLEncoding.DecodeString(out.PublicKey.User.ID)
	return out.PublicKey.Challenge, handle, authCookie(t, h, w, "passkey")
}
func registerTestPasskey(t *testing.T, h *HTTPServer, cookie *http.Cookie) *testAuthenticator {
	t.Helper()
	challenge, handle, ceremony := passkeyOptions(t, h, "register", cookie)
	key := newTestAuthenticator(t)
	key.handle = handle
	w := authRequest(t, h, "passkeys/register/finish", key.registration(t, challenge, h.config.PublicURL, "canter.test", true), cookie, ceremony)
	requireStatus(t, w, 200)
	return key
}
func TestPasskeyRegistrationLoginReplayAndReauth(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	w := signupHTTP(t, h, "passkey@example.com")
	requireStatus(t, w, 201)
	cookie := authCookie(t, h, w, "session")
	key := registerTestPasskey(t, h, cookie)
	challenge, _, ceremony := passkeyOptions(t, h, "login", nil)
	assertion := key.assertion(t, challenge, h.config.PublicURL, "canter.test", true)
	w = authRequest(t, h, "passkeys/login/finish", assertion)
	requireStatus(t, w, 400)
	w = authRequest(t, h, "passkeys/login/finish", assertion, ceremony)
	requireStatus(t, w, 200)
	session := authCookie(t, h, w, "session")
	if _, err := s.ResolveHuman(context.Background(), session.Value); err != nil {
		t.Fatal(err)
	}
	w = authRequest(t, h, "passkeys/login/finish", assertion, ceremony)
	requireStatus(t, w, 400)
	challenge, _, ceremony = passkeyOptions(t, h, "reauth", session)
	w = authRequest(t, h, "passkeys/reauth/finish", key.assertion(t, challenge, h.config.PublicURL, "canter.test", true), session, ceremony)
	requireStatus(t, w, 200)
	if _, err := s.ResolveHuman(context.Background(), session.Value); err == nil {
		t.Fatal("pre-reauth passkey session token survived")
	}
	if _, err := s.ResolveHuman(context.Background(), authCookie(t, h, w, "session").Value); err != nil {
		t.Fatal(err)
	}
}
func TestPasskeyRejectsWrongOriginRPUserAndMissingUV(t *testing.T) {
	_, h, _ := newAuthTestServer(t)
	w := signupHTTP(t, h, "passkey-negative@example.com")
	requireStatus(t, w, 201)
	cookie := authCookie(t, h, w, "session")
	key := registerTestPasskey(t, h, cookie)
	cases := []struct {
		name, origin, rp string
		uv               bool
		handle           []byte
	}{{"origin", "https://evil.test", "canter.test", true, key.handle}, {"rp", h.config.PublicURL, "evil.test", true, key.handle}, {"uv", h.config.PublicURL, "canter.test", false, key.handle}, {"handle", h.config.PublicURL, "canter.test", true, []byte("another-account")}}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			challenge, _, ceremony := passkeyOptions(t, h, "login", nil)
			before := key.handle
			key.handle = test.handle
			body := key.assertion(t, challenge, test.origin, test.rp, test.uv)
			key.handle = before
			w := authRequest(t, h, "passkeys/login/finish", body, ceremony)
			requireStatus(t, w, 400)
		})
	}
}
func TestPasskeyRequiresVerifiedEnrollmentAndBinding(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	w := signupHTTP(t, h, "pk-owner@example.com")
	requireStatus(t, w, 201)
	cookie := authCookie(t, h, w, "session")
	challenge, handle, ceremony := passkeyOptions(t, h, "register", cookie)
	key := newTestAuthenticator(t)
	key.handle = handle
	body := key.registration(t, challenge, h.config.PublicURL, "canter.test", false)
	w = authRequest(t, h, "passkeys/register/finish", body, cookie, ceremony)
	requireStatus(t, w, 400)
	_, _, other, err := s.Signup(context.Background(), "pk-other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	otherCookie := &http.Cookie{Name: h.humanCookieName(), Value: other}
	body = key.registration(t, challenge, h.config.PublicURL, "canter.test", true)
	w = authRequest(t, h, "passkeys/register/finish", body, otherCookie, ceremony)
	requireStatus(t, w, 401)
	w = authRequest(t, h, "passkeys/register/finish", body, cookie, ceremony)
	requireStatus(t, w, 200)
}
func TestPasskeySatisfiesMFAAndRevokedCredentialCannotLogin(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	w := signupHTTP(t, h, "pk-mfa@example.com")
	requireStatus(t, w, 201)
	cookie := authCookie(t, h, w, "session")
	key := registerTestPasskey(t, h, cookie)
	enrollTOTP(t, h, cookie)
	challenge, _, ceremony := passkeyOptions(t, h, "login", nil)
	w = authRequest(t, h, "passkeys/login/finish", key.assertion(t, challenge, h.config.PublicURL, "canter.test", true), ceremony)
	requireStatus(t, w, 200)
	session := authCookie(t, h, w, "session")
	if _, err := s.ResolveHuman(context.Background(), session.Value); err != nil {
		t.Fatal("UV passkey did not satisfy MFA", err)
	}
	var id string
	if err := s.pool.QueryRow(context.Background(), `SELECT id FROM account_passkeys`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	challenge, _, ceremony = passkeyOptions(t, h, "login", nil)
	w = authMethod(t, h, http.MethodDelete, "security/passkeys/"+id, map[string]string{}, session)
	requireStatus(t, w, 200)
	w = authRequest(t, h, "passkeys/login/finish", key.assertion(t, challenge, h.config.PublicURL, "canter.test", true), ceremony)
	requireStatus(t, w, 400)
}
