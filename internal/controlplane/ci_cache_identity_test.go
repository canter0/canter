package controlplane

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const ciCacheTestAudience = "urn:canter:ci:cache:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const ciCacheTestHeader = `{"alg":"RS256","typ":"JWT","kid":"test-key"}`

func ciCacheTestSigner(t *testing.T) (*rsa.PrivateKey, func(header, payload string) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(header, payload string) string {
		t.Helper()
		body := base64.RawURLEncoding.EncodeToString([]byte(header)) + "." +
			base64.RawURLEncoding.EncodeToString([]byte(payload))
		sum := sha256.Sum256([]byte(body))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return body + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	return key, sign
}

func ciCacheTestKeys(key *rsa.PrivateKey) oidc.KeySet {
	return &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}
}

func ciCacheValidClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss":                   ciCacheOIDCIssuer,
		"sub":                   "repo:octo/repo:ref:refs/heads/main",
		"aud":                   ciCacheTestAudience,
		"iat":                   now.Add(-time.Second).Unix(),
		"nbf":                   now.Add(-time.Second).Unix(),
		"exp":                   now.Add(299 * time.Second).Unix(),
		"jti":                   "token-1",
		"repository_id":         "101",
		"repository_owner_id":   "11",
		"repository":            "octo/repo",
		"repository_visibility": "private",
		"runner_environment":    "self-hosted",
		"run_id":                "202",
		"run_attempt":           "2",
		"check_run_id":          "303",
		"ref":                   "refs/heads/main",
		"event_name":            "push",
		"workflow_ref":          "octo/repo/.github/workflows/ci.yml@refs/heads/main",
		"job_workflow_ref":      "octo/repo/.github/workflows/ci.yml@refs/heads/main",
		"environment":           "ci",
		"sha":                   strings.Repeat("a", 40),
	}
}

func ciCacheSignClaims(t *testing.T, sign func(string, string) string, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return sign(ciCacheTestHeader, string(raw))
}

func ciCacheScope() ciCacheIdentityScope {
	return ciCacheIdentityScope{
		RepositoryID:      101,
		RunID:             202,
		RunAttempt:        2,
		CheckRunID:        303,
		Audience:          ciCacheTestAudience,
		ExecutionDeadline: time.Now().Add(10 * time.Minute),
	}
}

func ciCacheRequireUnauthorized(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

type ciCacheAdvancingKeys struct {
	inner   oidc.KeySet
	advance func()
}

func (k ciCacheAdvancingKeys) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	payload, err := k.inner.VerifySignature(ctx, jwt)
	if k.advance != nil {
		k.advance()
	}
	return payload, err
}

func TestCICacheTokenVerifiesAndBindsActualJob(t *testing.T) {
	key, sign := ciCacheTestSigner(t)
	keys := ciCacheTestKeys(key)
	now := time.Now().UTC().Truncate(time.Second)
	current := now
	nowFn := func() time.Time { return current }
	raw := ciCacheSignClaims(t, sign, ciCacheValidClaims(now))

	token, err := verifyCICacheToken(context.Background(), keys, nowFn, raw, ciCacheTestAudience)
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	if !token.verified || token.identity.bound {
		t.Fatalf("unexpected token state: %+v", token)
	}
	id := token.identity
	if id.RepositoryID != 101 || id.RepositoryOwnerID != 11 || id.RunID != 202 ||
		id.RunAttempt != 2 || id.CheckRunID != 303 || id.Repository != "octo/repo" ||
		id.RepositoryVisibility != "private" || id.Ref != "refs/heads/main" ||
		id.EventName != "push" || id.WorkflowRef == "" || id.SHA != strings.Repeat("a", 40) ||
		id.Audience != ciCacheTestAudience {
		t.Fatalf("unexpected identity: %+v", id)
	}
	if _, err := ciCacheNamespace("ws", "inst", 7, id); !errors.Is(err, ErrConflict) {
		t.Fatalf("unbound identity must not namespace, got %v", err)
	}

	scope := ciCacheScope()
	scope.ExecutionDeadline = now.Add(10 * time.Minute)
	bound, err := bindCICacheIdentity(token, scope, now)
	if err != nil {
		t.Fatalf("bind failed: %v", err)
	}
	if !bound.bound {
		t.Fatal("bound identity lost bound flag")
	}
	signedExp := now.Add(299 * time.Second)
	if !bound.ExpiresAt.Equal(signedExp) {
		t.Fatalf("expiry should be signed exp %v, got %v", signedExp, bound.ExpiresAt)
	}
	if ns1, err := ciCacheNamespace("ws", "inst", 7, bound); err != nil || ns1 == "" {
		t.Fatalf("bound namespace failed: %v", err)
	}

	scope.ExecutionDeadline = now.Add(60 * time.Second)
	bound, err = bindCICacheIdentity(token, scope, now)
	if err != nil {
		t.Fatalf("bind failed: %v", err)
	}
	if !bound.ExpiresAt.Equal(scope.ExecutionDeadline) {
		t.Fatalf("expiry should clamp to deadline %v, got %v", scope.ExecutionDeadline, bound.ExpiresAt)
	}

	for name, mutate := range map[string]func(*ciCacheIdentityScope){
		"wrong repo":      func(s *ciCacheIdentityScope) { s.RepositoryID = 999 },
		"wrong owner run": func(s *ciCacheIdentityScope) { s.RunID = 998 },
		"wrong attempt":   func(s *ciCacheIdentityScope) { s.RunAttempt = 9 },
		"wrong checkrun":  func(s *ciCacheIdentityScope) { s.CheckRunID = 998 },
		"wrong audience":  func(s *ciCacheIdentityScope) { s.Audience = ciCacheTestAudience[:len(ciCacheTestAudience)-1] + "b" },
		"zero repo":       func(s *ciCacheIdentityScope) { s.RepositoryID = 0 },
		"zero run":        func(s *ciCacheIdentityScope) { s.RunID = 0 },
		"zero attempt":    func(s *ciCacheIdentityScope) { s.RunAttempt = 0 },
		"zero checkrun":   func(s *ciCacheIdentityScope) { s.CheckRunID = 0 },
		"expired deadline": func(s *ciCacheIdentityScope) {
			s.ExecutionDeadline = now.Add(-time.Second)
		},
		"deadline now": func(s *ciCacheIdentityScope) {
			s.ExecutionDeadline = now
		},
	} {
		scope := ciCacheScope()
		scope.ExecutionDeadline = now.Add(10 * time.Minute)
		mutate(&scope)
		got, err := bindCICacheIdentity(token, scope, now)
		ciCacheRequireUnauthorized(t, err)
		if got != (ciCacheIdentity{}) {
			t.Fatalf("%s: expected zero identity, got %+v", name, got)
		}
	}

	if _, err := bindCICacheIdentity(ciCacheToken{}, ciCacheScope(), now); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unverified token must not bind, got %v", err)
	}
	if _, err := bindCICacheIdentity(boundTokenOf(t, token, scope), ciCacheScope(), now); err == nil {
		t.Fatal("already-bound token must not rebind")
	}
}

func boundTokenOf(t *testing.T, token ciCacheToken, scope ciCacheIdentityScope) ciCacheToken {
	t.Helper()
	identity, err := bindCICacheIdentity(token, scope, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	token.identity = identity
	return token
}

func TestCICacheTokenRejectsInvalidClaims(t *testing.T) {
	key, sign := ciCacheTestSigner(t)
	keys := ciCacheTestKeys(key)
	now := time.Now().UTC().Truncate(time.Second)
	current := now
	nowFn := func() time.Time { return current }

	stringID := func(key string) func(map[string]any) {
		return func(c map[string]any) { c[key] = 101 }
	}
	cases := map[string]func(map[string]any){
		"bad issuer":              func(c map[string]any) { c["iss"] = "https://evil.test" },
		"aud other urn":           func(c map[string]any) { c["aud"] = "urn:canter:ci:cache:v1:" + strings.Repeat("b", 64) },
		"aud array":               func(c map[string]any) { c["aud"] = []any{ciCacheTestAudience} },
		"missing repository":      func(c map[string]any) { delete(c, "repository_id") },
		"missing owner":           func(c map[string]any) { delete(c, "repository_owner_id") },
		"missing run":             func(c map[string]any) { delete(c, "run_id") },
		"missing attempt":         func(c map[string]any) { delete(c, "run_attempt") },
		"missing checkrun":        func(c map[string]any) { delete(c, "check_run_id") },
		"missing repository name": func(c map[string]any) { delete(c, "repository") },
		"missing visibility":      func(c map[string]any) { delete(c, "repository_visibility") },
		"missing runner env":      func(c map[string]any) { delete(c, "runner_environment") },
		"missing ref":             func(c map[string]any) { delete(c, "ref") },
		"missing event":           func(c map[string]any) { delete(c, "event_name") },
		"missing workflow":        func(c map[string]any) { delete(c, "workflow_ref") },
		"missing sha":             func(c map[string]any) { delete(c, "sha") },
		"missing sub":             func(c map[string]any) { delete(c, "sub") },
		"missing iat":             func(c map[string]any) { delete(c, "iat") },
		"missing exp":             func(c map[string]any) { delete(c, "exp") },
		"repo id numeric":         stringID("repository_id"),
		"owner id float":          func(c map[string]any) { c["repository_owner_id"] = "11.0" },
		"run id leading zero":     func(c map[string]any) { c["run_id"] = "0202" },
		"run id zero":             func(c map[string]any) { c["run_id"] = "0" },
		"checkrun overflow":       func(c map[string]any) { c["check_run_id"] = "99999999999999999999" },
		"checkrun int64 edge":     func(c map[string]any) { c["check_run_id"] = "9223372036854775808" },
		"repo bad name":           func(c map[string]any) { c["repository"] = "not-a-repo" },
		"visibility bogus":        func(c map[string]any) { c["repository_visibility"] = "weird" },
		"hosted runner":           func(c map[string]any) { c["runner_environment"] = "github-hosted" },
		"ref bare heads":          func(c map[string]any) { c["ref"] = "refs/heads/" },
		"ref no prefix":           func(c map[string]any) { c["ref"] = "refs/heads" },
		"ref control":             func(c map[string]any) { c["ref"] = "refs/heads/ma\x01in" },
		"ref too long":            func(c map[string]any) { c["ref"] = "refs/heads/" + strings.Repeat("x", 2000) },
		"event uppercase":         func(c map[string]any) { c["event_name"] = "Push" },
		"event empty":             func(c map[string]any) { c["event_name"] = "" },
		"event bad char":          func(c map[string]any) { c["event_name"] = "pull-request" },
		"workflow empty":          func(c map[string]any) { c["workflow_ref"] = "" },
		"job workflow empty":      func(c map[string]any) { c["job_workflow_ref"] = "" },
		"job workflow numeric":    func(c map[string]any) { c["job_workflow_ref"] = 42 },
		"environment empty":       func(c map[string]any) { c["environment"] = "" },
		"sha uppercase":           func(c map[string]any) { c["sha"] = strings.Repeat("A", 40) },
		"sha short":               func(c map[string]any) { c["sha"] = strings.Repeat("a", 39) },
		"sha 64 ok length bad":    func(c map[string]any) { c["sha"] = strings.Repeat("g", 64) },
		"sub empty":               func(c map[string]any) { c["sub"] = "" },
		"sub whitespace":          func(c map[string]any) { c["sub"] = "   " },
		"iat string":              func(c map[string]any) { c["iat"] = "123" },
		"iat float":               func(c map[string]any) { c["iat"] = float64(now.Add(-time.Second).Unix()) + 0.5 },
		"iat exponent":            func(c map[string]any) { c["iat"] = "1e9" },
		"exp float":               func(c map[string]any) { c["exp"] = float64(now.Add(299*time.Second).Unix()) + 0.5 },
		"iat future 31s":          func(c map[string]any) { c["iat"] = now.Add(31 * time.Second).Unix() },
		"iat age 301s":            func(c map[string]any) { c["iat"] = now.Add(-301 * time.Second).Unix() },
		"lifetime 601s": func(c map[string]any) {
			c["iat"] = now.Add(-time.Second).Unix()
			c["exp"] = now.Add(600 * time.Second).Unix()
		},
		"exp before iat": func(c map[string]any) {
			c["iat"] = now.Add(-time.Second).Unix()
			c["exp"] = now.Add(-2 * time.Second).Unix()
		},
		"nbf future 31s": func(c map[string]any) { c["nbf"] = now.Add(31 * time.Second).Unix() },
		"nbf after exp":  func(c map[string]any) { c["nbf"] = now.Add(400 * time.Second).Unix() },
		"nbf float":      func(c map[string]any) { c["nbf"] = 1.5 },
	}
	for name, mutate := range cases {
		claims := ciCacheValidClaims(now)
		mutate(claims)
		raw := ciCacheSignClaims(t, sign, claims)
		token, err := verifyCICacheToken(context.Background(), keys, nowFn, raw, ciCacheTestAudience)
		ciCacheRequireUnauthorized(t, err)
		if token != (ciCacheToken{}) {
			t.Fatalf("%s: expected zero token, got %+v", name, token)
		}
	}

	// exp equal to now is not "after now".
	claims := ciCacheValidClaims(now)
	claims["iat"] = now.Add(-time.Minute).Unix()
	claims["exp"] = now.Unix()
	raw := ciCacheSignClaims(t, sign, claims)
	if _, err := verifyCICacheToken(context.Background(), keys, nowFn, raw, ciCacheTestAudience); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("exp==now must reject")
	}

	// Unknown extra claims and an absent optional nbf are tolerated.
	claims = ciCacheValidClaims(now)
	claims["actor_id"] = "7"
	claims["future_github_claim"] = map[string]any{"nested": true}
	delete(claims, "nbf")
	raw = ciCacheSignClaims(t, sign, claims)
	token, err := verifyCICacheToken(context.Background(), keys, nowFn, raw, ciCacheTestAudience)
	if err != nil || !token.verified {
		t.Fatalf("extra claims must be allowed: %v", err)
	}
}

func TestCICacheTokenRejectsMalformedWire(t *testing.T) {
	key, sign := ciCacheTestSigner(t)
	keys := ciCacheTestKeys(key)
	now := time.Now().UTC().Truncate(time.Second)
	claims := ciCacheValidClaims(now)
	rawClaims, _ := json.Marshal(claims)
	valid := sign(ciCacheTestHeader, string(rawClaims))

	oversize := sign(ciCacheTestHeader, `{"pad":"`+strings.Repeat("x", 20000)+`"}`)
	cases := map[string]string{
		"empty":          "",
		"two segments":   strings.Join(strings.Split(valid, ".")[:2], "."),
		"four segments":  valid + ".extra",
		"bad b64 header": "!!!." + strings.Split(valid, ".")[1] + "." + strings.Split(valid, ".")[2],
		"bad b64 payload": func() string {
			p := strings.Split(valid, ".")
			return p[0] + ".!!!." + p[2]
		}(),
		"header array": func() string {
			p := strings.Split(valid, ".")
			return base64.RawURLEncoding.EncodeToString([]byte("[]")) + "." + p[1] + "." + p[2]
		}(),
		"payload array": func() string {
			p := strings.Split(valid, ".")
			return p[0] + "." + base64.RawURLEncoding.EncodeToString([]byte("[]")) + "." + p[2]
		}(),
		"header scalar": func() string {
			p := strings.Split(valid, ".")
			return base64.RawURLEncoding.EncodeToString([]byte(`"x"`)) + "." + p[1] + "." + p[2]
		}(),
		"alg none":      sign(`{"alg":"none","typ":"JWT"}`, string(rawClaims)),
		"alg rs512":     sign(`{"alg":"RS512","typ":"JWT"}`, string(rawClaims)),
		"alg missing":   sign(`{"typ":"JWT"}`, string(rawClaims)),
		"alg numeric":   sign(`{"alg":256}`, string(rawClaims)),
		"oversize":      oversize,
		"trailing data": valid + " ",
	}
	for name, raw := range cases {
		token, err := verifyCICacheToken(context.Background(), keys,
			func() time.Time { return now }, raw, ciCacheTestAudience)
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s: expected ErrUnauthorized, got %v", name, err)
		}
		if token != (ciCacheToken{}) {
			t.Fatalf("%s: expected zero token", name)
		}
	}
}

func TestCICacheTokenRejectsDuplicateNullAndLooseJSON(t *testing.T) {
	key, sign := ciCacheTestSigner(t)
	keys := ciCacheTestKeys(key)
	now := time.Now().UTC().Truncate(time.Second)
	claims := ciCacheValidClaims(now)
	rawClaims, _ := json.Marshal(claims)

	dupPayload := strings.Replace(string(rawClaims), `"aud":"`+ciCacheTestAudience+`"`,
		`"aud":"`+ciCacheTestAudience+`","aud":"`+ciCacheTestAudience+`"`, 1)
	nullPayload := strings.Replace(string(rawClaims), `"environment":"ci"`,
		`"environment":null`, 1)
	trailingPayload := string(rawClaims) + " {}"
	idNumericPayload := strings.Replace(string(rawClaims), `"run_id":"202"`,
		`"run_id":202`, 1)
	leadingZeroPayload := strings.Replace(string(rawClaims), `"run_id":"202"`,
		`"run_id":"0202"`, 1)
	expFloatPayload := strings.Replace(string(rawClaims),
		`"exp":`+jsonNumber(now.Add(299*time.Second).Unix()),
		`"exp":1.7e9`, 1)

	for name, payload := range map[string]string{
		"duplicate key":   dupPayload,
		"null value":      nullPayload,
		"trailing object": trailingPayload,
		"numeric id":      idNumericPayload,
		"leading zero id": leadingZeroPayload,
		"float exp":       expFloatPayload,
	} {
		raw := sign(ciCacheTestHeader, payload)
		token, err := verifyCICacheToken(context.Background(), keys,
			func() time.Time { return now }, raw, ciCacheTestAudience)
		ciCacheRequireUnauthorized(t, err)
		if token != (ciCacheToken{}) {
			t.Fatalf("%s: expected zero token", name)
		}
	}
}

func jsonNumber(v int64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func TestCICacheTokenRejectsClockAndContextFailures(t *testing.T) {
	key, sign := ciCacheTestSigner(t)
	keys := ciCacheTestKeys(key)
	base := time.Now().UTC().Truncate(time.Second)
	claims := ciCacheValidClaims(base)
	rawClaims, _ := json.Marshal(claims)
	raw := sign(ciCacheTestHeader, string(rawClaims))

	// Clock advances past token expiry inside the key-set callback.
	current := base
	advancing := ciCacheAdvancingKeys{
		inner:   keys,
		advance: func() { current = base.Add(10 * time.Minute) },
	}
	token, err := verifyCICacheToken(context.Background(), advancing,
		func() time.Time { return current }, raw, ciCacheTestAudience)
	ciCacheRequireUnauthorized(t, err)
	if token != (ciCacheToken{}) {
		t.Fatal("clock advance during verify must reject")
	}

	// Same token binds against an already-expired execution deadline.
	current = base
	token, err = verifyCICacheToken(context.Background(), keys,
		func() time.Time { return current }, raw, ciCacheTestAudience)
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	scope := ciCacheScope()
	scope.ExecutionDeadline = base.Add(4 * time.Minute)
	if _, err := bindCICacheIdentity(token, scope, base.Add(5*time.Minute)); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("bind after deadline must reject")
	}

	// Context cancelled before verification.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := verifyCICacheToken(cancelled, keys,
		func() time.Time { return base }, raw, ciCacheTestAudience); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("cancelled context must reject")
	}

	// Context cancelled during the key-set callback.
	ctx, cancelCtx := context.WithCancel(context.Background())
	cancelling := ciCacheAdvancingKeys{
		inner:   keys,
		advance: func() { cancelCtx() },
	}
	if _, err := verifyCICacheToken(ctx, cancelling,
		func() time.Time { return base }, raw, ciCacheTestAudience); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("context cancelled during verify must reject")
	}

	// Nil inputs fail closed.
	for name, call := range map[string]func() error{
		"nil keys": func() error {
			_, e := verifyCICacheToken(context.Background(), nil, func() time.Time { return base }, raw, ciCacheTestAudience)
			return e
		},
		"nil now": func() error {
			_, e := verifyCICacheToken(context.Background(), keys, nil, raw, ciCacheTestAudience)
			return e
		},
		"nil ctx": func() error {
			_, e := verifyCICacheToken(nil, keys, func() time.Time { return base }, raw, ciCacheTestAudience)
			return e
		},
		"bad audience": func() error {
			_, e := verifyCICacheToken(context.Background(), keys, func() time.Time { return base }, raw, "https://evil.test")
			return e
		},
	} {
		if err := call(); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s: expected ErrUnauthorized, got %v", name, err)
		}
	}
}

func TestCICacheTokenErrorDoesNotLeakClaims(t *testing.T) {
	key, sign := ciCacheTestSigner(t)
	keys := ciCacheTestKeys(key)
	now := time.Now().UTC().Truncate(time.Second)
	secret := "planted-secret-value-9f8e7d"
	claims := ciCacheValidClaims(now)
	claims["sub"] = "repo:octo/repo:ref:refs/heads/" + secret
	claims["exp"] = now.Add(-time.Hour).Unix()
	raw := ciCacheSignClaims(t, sign, claims)
	_, err := verifyCICacheToken(context.Background(), keys,
		func() time.Time { return now }, raw, ciCacheTestAudience)
	ciCacheRequireUnauthorized(t, err)
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error leaked claim material")
	}
}

func TestCICacheNamespaceRelationalScope(t *testing.T) {
	key, sign := ciCacheTestSigner(t)
	keys := ciCacheTestKeys(key)
	now := time.Now().UTC().Truncate(time.Second)
	raw := ciCacheSignClaims(t, sign, ciCacheValidClaims(now))
	token, err := verifyCICacheToken(context.Background(), keys,
		func() time.Time { return now }, raw, ciCacheTestAudience)
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	scope := ciCacheScope()
	scope.ExecutionDeadline = now.Add(10 * time.Minute)
	bound, err := bindCICacheIdentity(token, scope, now)
	if err != nil {
		t.Fatalf("bind failed: %v", err)
	}

	ns, err := ciCacheNamespace("ws", "inst", 7, bound)
	if err != nil {
		t.Fatalf("namespace failed: %v", err)
	}

	// Run/attempt/check-run/SHA are excluded: repeated jobs share the namespace.
	for name, mutate := range map[string]func(*ciCacheIdentity){
		"new run":      func(i *ciCacheIdentity) { i.RunID = 999 },
		"new attempt":  func(i *ciCacheIdentity) { i.RunAttempt = 9 },
		"new checkrun": func(i *ciCacheIdentity) { i.CheckRunID = 999 },
		"new sha":      func(i *ciCacheIdentity) { i.SHA = strings.Repeat("b", 40) },
	} {
		other := bound
		mutate(&other)
		got, err := ciCacheNamespace("ws", "inst", 7, other)
		if err != nil || got != ns {
			t.Fatalf("%s: namespace must be stable, got %q err %v", name, got, err)
		}
	}

	// Every trust-scope field changes the namespace.
	for name, mutate := range map[string]func(*ciCacheIdentity){
		"audience":     func(i *ciCacheIdentity) { i.Audience = "urn:canter:ci:cache:v1:" + strings.Repeat("b", 64) },
		"visibility":   func(i *ciCacheIdentity) { i.RepositoryVisibility = "public" },
		"repo id":      func(i *ciCacheIdentity) { i.RepositoryID = 102 },
		"owner id":     func(i *ciCacheIdentity) { i.RepositoryOwnerID = 12 },
		"event":        func(i *ciCacheIdentity) { i.EventName = "schedule" },
		"ref":          func(i *ciCacheIdentity) { i.Ref = "refs/heads/dev" },
		"tag ref":      func(i *ciCacheIdentity) { i.Ref = "refs/tags/v1" },
		"workflow":     func(i *ciCacheIdentity) { i.WorkflowRef = "octo/repo/.github/workflows/other.yml@refs/heads/main" },
		"job workflow": func(i *ciCacheIdentity) { i.JobWorkflowRef = "octo/repo/.github/workflows/job.yml@refs/heads/main" },
		"environment":  func(i *ciCacheIdentity) { i.Environment = "prod" },
	} {
		other := bound
		mutate(&other)
		got, err := ciCacheNamespace("ws", "inst", 7, other)
		if err != nil {
			t.Fatalf("%s: namespace failed: %v", name, err)
		}
		if got == ns {
			t.Fatalf("%s: namespace must differ", name)
		}
	}

	// Workspace/installation/epoch are part of the scope.
	for name, call := range map[string]func() (string, error){
		"workspace":    func() (string, error) { return ciCacheNamespace("ws2", "inst", 7, bound) },
		"installation": func() (string, error) { return ciCacheNamespace("ws", "inst2", 7, bound) },
		"epoch":        func() (string, error) { return ciCacheNamespace("ws", "inst", 8, bound) },
		"epoch zero":   func() (string, error) { return ciCacheNamespace("ws", "inst", 0, bound) },
		"unbound":      func() (string, error) { return ciCacheNamespace("ws", "inst", 7, token.identity) },
		"delimiter ab/c": func() (string, error) {
			return ciCacheNamespace("ab", "c", 7, bound)
		},
		"delimiter a/bc": func() (string, error) {
			return ciCacheNamespace("a", "bc", 7, bound)
		},
	} {
		_, err := call()
		if name == "epoch zero" || name == "unbound" {
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("%s: expected ErrConflict, got %v", name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: namespace failed: %v", name, err)
		}
	}
	wsAlt, _ := ciCacheNamespace("ws2", "inst", 7, bound)
	if wsAlt == ns {
		t.Fatal("workspace change must alter namespace")
	}
	instAlt, _ := ciCacheNamespace("ws", "inst2", 7, bound)
	if instAlt == ns {
		t.Fatal("installation change must alter namespace")
	}
	epochAlt, _ := ciCacheNamespace("ws", "inst", 8, bound)
	if epochAlt == ns {
		t.Fatal("epoch change must alter namespace")
	}
	delimAB, _ := ciCacheNamespace("ab", "c", 7, bound)
	delimA, _ := ciCacheNamespace("a", "bc", 7, bound)
	if delimAB == delimA {
		t.Fatal("workspace/installation delimiter ambiguity must differ")
	}

	// Pull-request refs share a namespace across runs; other events isolate.
	prClaims := ciCacheValidClaims(now)
	prClaims["event_name"] = "pull_request"
	prClaims["ref"] = "refs/pull/7/merge"
	prToken, err := verifyCICacheToken(context.Background(), keys,
		func() time.Time { return now }, ciCacheSignClaims(t, sign, prClaims), ciCacheTestAudience)
	if err != nil {
		t.Fatalf("pr verify failed: %v", err)
	}
	prScope := ciCacheScope()
	prScope.ExecutionDeadline = now.Add(10 * time.Minute)
	prBound, err := bindCICacheIdentity(prToken, prScope, now)
	if err != nil {
		t.Fatalf("pr bind failed: %v", err)
	}
	prNS, err := ciCacheNamespace("ws", "inst", 7, prBound)
	if err != nil {
		t.Fatalf("pr namespace failed: %v", err)
	}
	prOther := prBound
	prOther.RunID = 555
	prOther.RunAttempt = 8
	prOther.CheckRunID = 777
	prOther.SHA = strings.Repeat("c", 40)
	prNS2, err := ciCacheNamespace("ws", "inst", 7, prOther)
	if err != nil || prNS2 != prNS {
		t.Fatalf("same PR ref across runs must share namespace: %v", err)
	}
	prOther.Ref = "refs/pull/8/merge"
	prNS3, _ := ciCacheNamespace("ws", "inst", 7, prOther)
	if prNS3 == prNS {
		t.Fatal("different PR merge refs must differ")
	}

	// Non-shared events isolate by run+attempt even on identical refs.
	for _, event := range []string{"workflow_run", "workflow_dispatch", "pull_request_target"} {
		claims := ciCacheValidClaims(now)
		claims["event_name"] = event
		claims["ref"] = "refs/heads/main"
		tok, err := verifyCICacheToken(context.Background(), keys,
			func() time.Time { return now }, ciCacheSignClaims(t, sign, claims), ciCacheTestAudience)
		if err != nil {
			t.Fatalf("%s verify failed: %v", event, err)
		}
		b, err := bindCICacheIdentity(tok, ciCacheScope(), now)
		if err != nil {
			t.Fatalf("%s bind failed: %v", event, err)
		}
		nsA, _ := ciCacheNamespace("ws", "inst", 7, b)
		b.RunID = 556
		b.RunAttempt = 3
		nsB, _ := ciCacheNamespace("ws", "inst", 7, b)
		if nsA == nsB {
			t.Fatalf("%s must isolate per run", event)
		}
	}
}

func TestCICacheTokenRejectsNonCanonicalSegments(t *testing.T) {
	key, sign := ciCacheTestSigner(t)
	keys := ciCacheTestKeys(key)
	now := time.Now().UTC().Truncate(time.Second)
	raw := ciCacheSignClaims(t, sign, ciCacheValidClaims(now))
	parts := strings.Split(raw, ".")

	// CR/LF inside any segment is not valid canonical base64url.
	for i, segment := range []string{parts[0] + "\r\n", parts[1] + "\n", parts[2] + "\r"} {
		mutated := strings.Join([]string{parts[0], parts[1], parts[2]}, ".")
		all := []string{parts[0], parts[1], parts[2]}
		all[i] = segment
		mutated = strings.Join(all, ".")
		if _, err := verifyCICacheToken(context.Background(), keys,
			func() time.Time { return now }, mutated, ciCacheTestAudience); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("segment %d with line break must reject", i)
		}
	}

	// A signature segment that decodes to the same bytes but uses non-canonical
	// trailing bits must reject even though the signature itself is valid.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	canonical := base64.RawURLEncoding.EncodeToString(sigBytes)
	if canonical != parts[2] {
		t.Fatal("fixture signature segment must already be canonical")
	}
	alias := ""
	for _, c := range alphabet {
		candidate := parts[2][:len(parts[2])-1] + string(c)
		decoded, err := base64.RawURLEncoding.DecodeString(candidate)
		if err == nil && string(decoded) == string(sigBytes) && candidate != parts[2] {
			alias = candidate
			break
		}
	}
	if alias == "" {
		t.Fatal("no non-canonical signature alias found")
	}
	mutated := parts[0] + "." + parts[1] + "." + alias
	if _, err := verifyCICacheToken(context.Background(), keys,
		func() time.Time { return now }, mutated, ciCacheTestAudience); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("non-canonical signature encoding must reject")
	}
}

func TestCICacheTokenRejectsInvalidUTF8Payload(t *testing.T) {
	key, sign := ciCacheTestSigner(t)
	keys := ciCacheTestKeys(key)
	now := time.Now().UTC().Truncate(time.Second)
	// A payload carrying a raw invalid UTF-8 byte inside a claim value: the
	// signature is valid over these exact bytes, so only the UTF-8 preflight
	// can reject it before the JSON parser would silently replace the byte.
	broken := []byte(`{"iss":"` + ciCacheOIDCIssuer + `","sub":"s","aud":"` + ciCacheTestAudience +
		`","iat":` + jsonNumber(now.Add(-time.Second).Unix()) + `,"exp":` +
		jsonNumber(now.Add(299*time.Second).Unix()) + `,"repository_id":"101",` +
		`"repository_owner_id":"11","repository":"octo/repo","repository_visibility":"private",` +
		`"runner_environment":"self-hosted","run_id":"202","run_attempt":"2","check_run_id":"303",` +
		`"ref":"refs/heads/main","event_name":"push","workflow_ref":"octo/` +
		string([]byte{0xff}) + `","sha":"` + strings.Repeat("a", 40) + `"}`)
	token := sign(ciCacheTestHeader, string(broken))
	if _, err := verifyCICacheToken(context.Background(), keys,
		func() time.Time { return now }, token, ciCacheTestAudience); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("invalid UTF-8 payload bytes must reject before JSON parsing")
	}
}

func TestCICacheBindRechecksPreservedNotBefore(t *testing.T) {
	key, sign := ciCacheTestSigner(t)
	keys := ciCacheTestKeys(key)
	base := time.Now().UTC().Truncate(time.Second)
	claims := ciCacheValidClaims(base)
	claims["nbf"] = base.Add(30 * time.Second).Unix()
	raw := ciCacheSignClaims(t, sign, claims)
	token, err := verifyCICacheToken(context.Background(), keys,
		func() time.Time { return base }, raw, ciCacheTestAudience)
	if err != nil {
		t.Fatalf("verify at base must accept skewed nbf: %v", err)
	}
	scope := ciCacheScope()
	scope.ExecutionDeadline = base.Add(10 * time.Minute)
	if _, err := bindCICacheIdentity(token, scope, base); err != nil {
		t.Fatalf("bind at verify time must succeed: %v", err)
	}
	// Wall-clock rollback: iat is only 19s in the future (inside skew) but the
	// preserved nbf is 50s ahead and must still reject.
	if _, err := bindCICacheIdentity(token, scope, base.Add(-20*time.Second)); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("bind must recheck signed nbf on clock rollback")
	}
}
