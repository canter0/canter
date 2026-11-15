package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

var errCICacheOrigin = errors.New("CI cache public URL must be an HTTPS origin")

// The audience identifies the service realm, not the user-supplied Host
// header. Normalize only origin aliases; refuse paths and URL credentials.
func ciCacheAudienceForURL(raw string, allowLoopbackHTTP bool) (string, string, error) {
	for _, b := range []byte(raw) {
		if b <= 0x20 || b >= 0x7f {
			return "", "", errCICacheOrigin
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") ||
		u.RawPath != "" || (u.Path != "" && u.Path != "/") ||
		strings.HasSuffix(u.Host, ":") {
		return "", "", errCICacheOrigin
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if scheme != "https" &&
		!(scheme == "http" && allowLoopbackHTTP && ip != nil && ip.IsLoopback()) {
		return "", "", errCICacheOrigin
	}
	if ip != nil {
		host = ip.String()
	} else {
		if len(host) == 0 || len(host) > 253 {
			return "", "", errCICacheOrigin
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 ||
				label[0] == '-' || label[len(label)-1] == '-' {
				return "", "", errCICacheOrigin
			}
			for _, b := range []byte(label) {
				if !((b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-') {
					return "", "", errCICacheOrigin
				}
			}
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return "", "", errCICacheOrigin
		}
		port = strconv.FormatUint(n, 10)
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	origin := scheme + "://" + host
	sum := sha256.Sum256([]byte("canter-ci-cache-audience-v1\n" + origin + "\n"))
	return origin, ciCacheAudiencePrefix + hex.EncodeToString(sum[:]), nil
}
