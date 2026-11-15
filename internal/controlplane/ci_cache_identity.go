package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
)

var (
	errCICacheUnauthorized = fmt.Errorf(
		"%w: CI cache identity could not be verified", ErrUnauthorized)
	errCICacheUnbound = fmt.Errorf(
		"%w: CI cache identity could not be bound", ErrUnauthorized)
	ciCacheEventName   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	ciCacheSHA         = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	ciCacheDecimalID   = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	ciCacheUnixLiteral = regexp.MustCompile(`^(0|[1-9][0-9]{0,18})$`)
)

func ciCacheStringClaim(claims map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := claims[key]
	if !ok {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func ciCacheIDClaim(claims map[string]json.RawMessage, key string) (int64, bool) {
	value, ok := ciCacheStringClaim(claims, key)
	if !ok || !ciCacheDecimalID.MatchString(value) {
		return 0, false
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func ciCacheUnixClaim(claims map[string]json.RawMessage, key string) (int64, bool, bool) {
	raw, ok := claims[key]
	if !ok {
		return 0, false, false
	}
	if !ciCacheUnixLiteral.MatchString(string(raw)) {
		return 0, false, true
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, false, true
	}
	return value, true, true
}

func ciCacheOptionalText(claims map[string]json.RawMessage, key string, limit int) (string, bool) {
	if _, present := claims[key]; !present {
		return "", true
	}
	value, ok := ciCacheStringClaim(claims, key)
	if !ok || !ciCacheIdentityText(value, limit, false) {
		return "", false
	}
	return value, true
}

func ciCacheRefAllowed(ref string) bool {
	if !ciCacheIdentityText(ref, ciCacheMaxRefBytes, false) {
		return false
	}
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if strings.HasPrefix(ref, prefix) {
			return len(ref) > len(prefix)
		}
	}
	return ciCachePullRef.MatchString(ref)
}

func ciCacheClockOK(issuedAt, expiresAt time.Time, notBefore *time.Time, now time.Time) bool {
	if !expiresAt.After(issuedAt) ||
		expiresAt.Sub(issuedAt) > ciCacheMaxTokenLifetime ||
		issuedAt.After(now.Add(ciCacheClockSkew)) ||
		now.Sub(issuedAt) > ciCacheMaxTokenAge ||
		!expiresAt.After(now) {
		return false
	}
	if notBefore != nil &&
		(notBefore.After(now.Add(ciCacheClockSkew)) || notBefore.After(expiresAt)) {
		return false
	}
	return true
}

func ciCacheB64Segment(segment string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != segment {
		return nil, false
	}
	return raw, true
}

func ciCacheJWTParts(raw string) (header, payload []byte, ok bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, nil, false
	}
	header, ok = ciCacheB64Segment(parts[0])
	if !ok || !utf8.Valid(header) {
		return nil, nil, false
	}
	payload, ok = ciCacheB64Segment(parts[1])
	if !ok || !utf8.Valid(payload) {
		return nil, nil, false
	}
	if _, ok = ciCacheB64Segment(parts[2]); !ok {
		return nil, nil, false
	}
	return header, payload, true
}

// verifyCICacheToken authenticates a GitHub Actions OIDC job token. A verified
// token is not yet authorized: bindCICacheIdentity must tie its signed claims
// to the admitted Canter job resolved by the server.
func verifyCICacheToken(ctx context.Context, keys oidc.KeySet, now func() time.Time,
	raw, audience string) (ciCacheToken, error) {
	if ctx == nil || ctx.Err() != nil || keys == nil || now == nil ||
		len(raw) == 0 || len(raw) > ciCacheMaxTokenBytes ||
		!ciCacheAudience.MatchString(audience) {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	headerJSON, payloadJSON, ok := ciCacheJWTParts(raw)
	if !ok {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	var header, claims map[string]json.RawMessage
	if ciEnforcerStrictJSON(headerJSON, &header) != nil ||
		ciEnforcerStrictJSON(payloadJSON, &claims) != nil {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	var alg string
	if rawAlg, present := header["alg"]; !present ||
		json.Unmarshal(rawAlg, &alg) != nil || alg != "RS256" {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	aud, ok := ciCacheStringClaim(claims, "aud")
	if !ok || aud != audience {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	repositoryID, ok := ciCacheIDClaim(claims, "repository_id")
	if !ok {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	ownerID, ok := ciCacheIDClaim(claims, "repository_owner_id")
	if !ok {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	runID, ok := ciCacheIDClaim(claims, "run_id")
	if !ok {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	attempt, ok := ciCacheIDClaim(claims, "run_attempt")
	if !ok {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	checkRunID, ok := ciCacheIDClaim(claims, "check_run_id")
	if !ok {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	repository, ok := ciCacheStringClaim(claims, "repository")
	if !ok {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	if _, _, valid := ciCanonicalRepoName(repository); !valid {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	visibility, ok := ciCacheStringClaim(claims, "repository_visibility")
	if !ok || (visibility != "public" && visibility != "private" &&
		visibility != "internal") {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	runnerEnv, ok := ciCacheStringClaim(claims, "runner_environment")
	if !ok || runnerEnv != "self-hosted" {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	ref, ok := ciCacheStringClaim(claims, "ref")
	if !ok || !ciCacheRefAllowed(ref) {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	eventName, ok := ciCacheStringClaim(claims, "event_name")
	if !ok || !ciCacheEventName.MatchString(eventName) {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	workflowRef, ok := ciCacheStringClaim(claims, "workflow_ref")
	if !ok || !ciCacheIdentityText(workflowRef, ciCacheMaxWorkflowBytes, false) {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	jobWorkflowRef, ok := ciCacheOptionalText(claims, "job_workflow_ref", ciCacheMaxWorkflowBytes)
	if !ok {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	environment, ok := ciCacheOptionalText(claims, "environment", ciCacheMaxEnvBytes)
	if !ok {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	sha, ok := ciCacheStringClaim(claims, "sha")
	if !ok || !ciCacheSHA.MatchString(sha) {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	sub, ok := ciCacheStringClaim(claims, "sub")
	if !ok || !ciCacheIdentityText(sub, 2048, false) {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	iatRaw, iatOK, iatPresent := ciCacheUnixClaim(claims, "iat")
	expRaw, expOK, expPresent := ciCacheUnixClaim(claims, "exp")
	if !iatPresent || !expPresent || !iatOK || !expOK {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	nbfRaw, nbfOK, nbfPresent := ciCacheUnixClaim(claims, "nbf")
	if nbfPresent && !nbfOK {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	issuedAt := time.Unix(iatRaw, 0).UTC()
	expiresAt := time.Unix(expRaw, 0).UTC()
	var notBefore *time.Time
	if nbfPresent {
		value := time.Unix(nbfRaw, 0).UTC()
		notBefore = &value
	}
	if !ciCacheClockOK(issuedAt, expiresAt, notBefore, now()) {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	verifier := oidc.NewVerifier(ciCacheOIDCIssuer, keys, &oidc.Config{
		ClientID:             audience,
		SupportedSigningAlgs: []string{oidc.RS256},
		Now:                  now,
	})
	if _, err := verifier.Verify(ctx, raw); err != nil {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	if ctx.Err() != nil || !ciCacheClockOK(issuedAt, expiresAt, notBefore, now()) {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	return ciCacheToken{identity: ciCacheIdentity{
		RepositoryID:         repositoryID,
		RepositoryOwnerID:    ownerID,
		RunID:                runID,
		RunAttempt:           attempt,
		CheckRunID:           checkRunID,
		Repository:           repository,
		RepositoryVisibility: visibility,
		Ref:                  ref,
		EventName:            eventName,
		WorkflowRef:          workflowRef,
		JobWorkflowRef:       jobWorkflowRef,
		Environment:          environment,
		SHA:                  sha,
		Audience:             audience,
		IssuedAt:             issuedAt,
		ExpiresAt:            expiresAt,
	}, verified: true, notBefore: notBefore}, nil
}

// bindCICacheIdentity ties a signature-verified token to the actual admitted
// Canter job the server resolved. All scope values are server-sourced; the
// guest never supplies expected claims.
func bindCICacheIdentity(token ciCacheToken, scope ciCacheIdentityScope,
	now time.Time) (ciCacheIdentity, error) {
	identity := token.identity
	if !token.verified || identity.bound ||
		scope.RepositoryID <= 0 || scope.RunID <= 0 || scope.RunAttempt <= 0 ||
		scope.CheckRunID <= 0 || !ciCacheAudience.MatchString(scope.Audience) ||
		scope.RepositoryID != identity.RepositoryID ||
		scope.RunID != identity.RunID ||
		scope.RunAttempt != identity.RunAttempt ||
		scope.CheckRunID != identity.CheckRunID ||
		scope.Audience != identity.Audience ||
		!scope.ExecutionDeadline.After(now) ||
		!ciCacheClockOK(identity.IssuedAt, identity.ExpiresAt, token.notBefore, now) {
		return ciCacheIdentity{}, errCICacheUnbound
	}
	identity.bound = true
	if scope.ExecutionDeadline.Before(identity.ExpiresAt) {
		identity.ExpiresAt = scope.ExecutionDeadline
	}
	return identity, nil
}
