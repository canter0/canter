package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// errCIEvidenceCapabilityInvalid is the only token failure: it never echoes
// the token, claims, key material, or transport detail.
var errCIEvidenceCapabilityInvalid = errors.New("invalid CI evidence capability")

var ciEvidenceCapabilityKeys = map[string]bool{
	"version":              true,
	"workspace":            true,
	"runner":               true,
	"admissionId":          true,
	"githubInstallationId": true,
	"githubRepositoryId":   true,
	"githubRunnerId":       true,
	"githubJobId":          true,
	"deadlineUnixMs":       true,
}

func (c ciEvidenceCapability) claimsValid() bool {
	return c.Version == 1 &&
		ciAdmissionScopeIDValid(c.Workspace) && ciAdmissionScopeIDValid(c.Runner) &&
		ciAdmissionID.MatchString(c.AdmissionID) &&
		c.GitHubInstallationID > 0 && c.GitHubRepositoryID > 0 &&
		c.GitHubRunnerID > 0 && c.GitHubJobID > 0 &&
		c.DeadlineUnixMs > 0
}

// mintCIEvidenceCapability signs the nine evidence claims into the
// ce1.<payload>.<mac> wire token under the evidence MAC domain. The deadline
// is the original execution deadline and may never exceed the lifecycle hard
// window; the private verified marker is never serialized.
func mintCIEvidenceCapability(key []byte, c ciEvidenceCapability, now time.Time) (string, error) {
	if len(key) < 32 || !c.claimsValid() ||
		c.DeadlineUnixMs > now.Add(ciLifecycleHardWindow).UnixMilli() {
		return "", errCIEvidenceCapabilityInvalid
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", errCIEvidenceCapabilityInvalid
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ciEvidenceMACDomain + payload))
	tag := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	token := ciEvidenceTokenPrefix + "." + payload + "." + tag
	if len(token) > ciAdmissionCapabilityMaxBytes {
		return "", errCIEvidenceCapabilityInvalid
	}
	return token, nil
}

// parseCIEvidenceCapability verifies the MAC before any claim parsing, then
// enforces the strict nine-key schema, claim bounds, the hard-window cap,
// and the deadline+grace expiry. Only a fully validated token gets verified.
func parseCIEvidenceCapability(key []byte, token string, now time.Time) (ciEvidenceCapability, error) {
	var c ciEvidenceCapability
	if len(key) < 32 || token == "" || len(token) > ciAdmissionCapabilityMaxBytes {
		return c, errCIEvidenceCapabilityInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != ciEvidenceTokenPrefix || parts[1] == "" || parts[2] == "" {
		return c, errCIEvidenceCapabilityInvalid
	}
	macRaw, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(macRaw) != sha256.Size {
		return c, errCIEvidenceCapabilityInvalid
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ciEvidenceMACDomain + parts[1]))
	if !hmac.Equal(macRaw, mac.Sum(nil)) {
		return c, errCIEvidenceCapabilityInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) == 0 || len(payload) > ciAdmissionCapabilityMaxBytes ||
		!utf8.Valid(payload) {
		return c, errCIEvidenceCapabilityInvalid
	}
	c, err = decodeCIEvidenceCapabilityJSON(payload)
	if err != nil || !c.claimsValid() {
		return ciEvidenceCapability{}, errCIEvidenceCapabilityInvalid
	}
	if c.DeadlineUnixMs > now.Add(ciLifecycleHardWindow).UnixMilli() {
		return ciEvidenceCapability{}, errCIEvidenceCapabilityInvalid
	}
	graceMs := ciEvidenceUploadGrace.Milliseconds()
	if c.DeadlineUnixMs > math.MaxInt64-graceMs ||
		now.UnixMilli() >= c.DeadlineUnixMs+graceMs {
		return ciEvidenceCapability{}, errCIEvidenceCapabilityInvalid
	}
	c.verified = true
	return c, nil
}

// decodeCIEvidenceCapabilityJSON enforces the exact nine-key object: every
// key mandatory and non-null, duplicates, unknown keys, and case aliases
// rejected before typed decoding.
func decodeCIEvidenceCapabilityJSON(raw []byte) (ciEvidenceCapability, error) {
	var c ciEvidenceCapability
	fields, err := ciEvidenceJSONObject(raw, ciEvidenceCapabilityKeys, ciEvidenceCapabilityKeys)
	if err != nil {
		return c, errCIEvidenceCapabilityInvalid
	}
	var version int64
	if err := ciEvidenceJSONInt(fields, "version", &version); err != nil || version != 1 {
		return c, errCIEvidenceCapabilityInvalid
	}
	c.Version = 1
	if err := ciEvidenceJSONString(fields, "workspace", &c.Workspace); err != nil {
		return c, err
	}
	if err := ciEvidenceJSONString(fields, "runner", &c.Runner); err != nil {
		return c, err
	}
	if err := ciEvidenceJSONString(fields, "admissionId", &c.AdmissionID); err != nil {
		return c, err
	}
	if err := ciEvidenceJSONInt(fields, "githubInstallationId", &c.GitHubInstallationID); err != nil {
		return c, err
	}
	if err := ciEvidenceJSONInt(fields, "githubRepositoryId", &c.GitHubRepositoryID); err != nil {
		return c, err
	}
	if err := ciEvidenceJSONInt(fields, "githubRunnerId", &c.GitHubRunnerID); err != nil {
		return c, err
	}
	if err := ciEvidenceJSONInt(fields, "githubJobId", &c.GitHubJobID); err != nil {
		return c, err
	}
	if err := ciEvidenceJSONInt(fields, "deadlineUnixMs", &c.DeadlineUnixMs); err != nil {
		return c, err
	}
	return c, nil
}
