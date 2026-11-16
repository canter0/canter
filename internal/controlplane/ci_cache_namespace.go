package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ciCacheOIDCIssuer       = "https://token.actions.githubusercontent.com"
	ciCacheAudiencePrefix   = "urn:canter:ci:cache:v1:"
	ciCacheMaxTokenBytes    = 16 << 10
	ciCacheMaxTokenLifetime = 10 * time.Minute
	ciCacheMaxTokenAge      = 5 * time.Minute
	ciCacheClockSkew        = 30 * time.Second
	ciCacheMaxRefBytes      = 1024
	ciCacheMaxWorkflowBytes = 2048
	ciCacheMaxEnvBytes      = 255
)

var (
	ciCachePullRef  = regexp.MustCompile(`^refs/pull/[1-9][0-9]*/(?:head|merge)$`)
	ciCacheAudience = regexp.MustCompile(`^urn:canter:ci:cache:v1:[0-9a-f]{64}$`)
)

// A verified token is not an authorization: its check run must still be bound
// to the active, admitted Canter job loaded by the server.
type ciCacheToken struct {
	identity  ciCacheIdentity
	notBefore *time.Time
	verified  bool
}

type ciCacheIdentityScope struct {
	RepositoryID      int64
	RunID             int64
	RunAttempt        int64
	CheckRunID        int64
	Audience          string
	ExecutionDeadline time.Time
}

type ciCacheIdentity struct {
	RepositoryID         int64
	RepositoryOwnerID    int64
	RunID                int64
	RunAttempt           int64
	CheckRunID           int64
	Repository           string
	RepositoryVisibility string
	Ref                  string
	EventName            string
	WorkflowRef          string
	JobWorkflowRef       string
	Environment          string
	SHA                  string
	Audience             string
	IssuedAt             time.Time
	ExpiresAt            time.Time
	bound                bool
}

func ciCacheIdentityText(value string, limit int, optional bool) bool {
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	if value == "" {
		return optional
	}
	if strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Default to run isolation. Privileged workflow_run/pull_request_target and
// arbitrary dispatch inputs must never populate a trusted push namespace.
func ciCacheRefPartition(identity ciCacheIdentity) string {
	switch identity.EventName {
	case "push":
		if strings.HasPrefix(identity.Ref, "refs/heads/") ||
			strings.HasPrefix(identity.Ref, "refs/tags/") {
			return "shared-ref"
		}
	case "schedule":
		if strings.HasPrefix(identity.Ref, "refs/heads/") {
			return "shared-ref"
		}
	case "pull_request":
		if ciCachePullRef.MatchString(identity.Ref) {
			return "shared-ref"
		}
	}
	return "run:" + strconv.FormatInt(identity.RunID, 10) + ":" +
		strconv.FormatInt(identity.RunAttempt, 10)
}

func ciCacheNamespace(workspace, installation string, epoch int64, identity ciCacheIdentity) (string, error) {
	if !identity.bound || epoch <= 0 || identity.RepositoryID <= 0 ||
		identity.RepositoryOwnerID <= 0 || identity.RunID <= 0 ||
		identity.RunAttempt <= 0 || identity.CheckRunID <= 0 ||
		!ciCacheAudience.MatchString(identity.Audience) ||
		!ciCacheIdentityText(workspace, 128, false) ||
		!ciCacheIdentityText(installation, 128, false) ||
		!ciCacheIdentityText(identity.EventName, 64, false) ||
		!ciCacheIdentityText(identity.Ref, ciCacheMaxRefBytes, false) ||
		!ciCacheIdentityText(identity.WorkflowRef, ciCacheMaxWorkflowBytes, false) ||
		!ciCacheIdentityText(identity.JobWorkflowRef, ciCacheMaxWorkflowBytes, true) ||
		!ciCacheIdentityText(identity.Environment, ciCacheMaxEnvBytes, true) ||
		(identity.RepositoryVisibility != "public" &&
			identity.RepositoryVisibility != "private" &&
			identity.RepositoryVisibility != "internal") {
		return "", fmt.Errorf("%w: invalid CI cache namespace scope", ErrConflict)
	}
	// A typed, ordered JSON tuple avoids delimiter and case-normalization
	// aliases. Visibility and environment changes cannot expose older data.
	raw, err := json.Marshal([]string{
		"canter-ci-cache-namespace-v1",
		identity.Audience,
		workspace,
		installation,
		strconv.FormatInt(epoch, 10),
		strconv.FormatInt(identity.RepositoryID, 10),
		strconv.FormatInt(identity.RepositoryOwnerID, 10),
		identity.RepositoryVisibility,
		identity.EventName,
		identity.Ref,
		identity.WorkflowRef,
		identity.JobWorkflowRef,
		identity.Environment,
		ciCacheRefPartition(identity),
	})
	if err != nil {
		return "", fmt.Errorf("%w: invalid CI cache namespace scope", ErrConflict)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
