package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// errCIJobLogUnavailable is the only error this pipeline reports: transport
// details, signed URLs, tokens, and raw responses never leave the downloader.
var errCIJobLogUnavailable = errors.New("CI job logs are unavailable")

// downloadCIJobLogs fetches the plaintext log for one claimed scope. The
// repository and job identity, run binding, and GitHub-side completion are
// all verified with a repository-scoped read-only token before any log bytes
// are requested. The whole exchange shares one bounded context.
func (c *ciGitHubClient) downloadCIJobLogs(ctx context.Context, scope ciJobLogScope, allowLoopbackHTTP bool) (string, bool, error) {
	fail := func() (string, bool, error) { return "", false, errCIJobLogUnavailable }
	if c == nil || c.hc == nil || scope.GitHubInstallationID <= 0 || scope.GitHubRepositoryID <= 0 ||
		scope.GitHubJobID <= 0 || scope.GitHubRunID <= 0 {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, ciJobLogDownloadBound)
	defer cancel()
	token, err := c.runnerSourceToken(ctx, scope.GitHubInstallationID, scope.GitHubRepositoryID)
	if err != nil {
		return fail()
	}
	var repo ciSourceRepository
	if err := c.ciSourceJSON(ctx, http.MethodGet,
		fmt.Sprintf("/repositories/%d", scope.GitHubRepositoryID), token, nil, &repo); err != nil {
		return fail()
	}
	owner, name, ok := ciCanonicalRepoName(repo.FullName)
	if !ok || repo.ID != scope.GitHubRepositoryID {
		return fail()
	}
	var job ciSourceJob
	jobPath := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d",
		url.PathEscape(owner), url.PathEscape(name), scope.GitHubJobID)
	if err := c.ciSourceJSON(ctx, http.MethodGet, jobPath, token, nil, &job); err != nil {
		return fail()
	}
	if job.ID != scope.GitHubJobID || job.RunID != scope.GitHubRunID || job.Status != "completed" {
		return fail()
	}
	raw, err := c.fetchCIJobLogBody(ctx, jobPath+"/logs", token, allowLoopbackHTTP)
	if err != nil {
		return fail()
	}
	text, truncated := normalizeCIJobLog(raw)
	return text, truncated, nil
}

// fetchCIJobLogBody performs the authenticated logs request. GitHub answers
// 302 with a short-lived plaintext URL or, rarely, 200 with the body inline;
// every other status is unavailable and the body is ignored.
func (c *ciGitHubClient) fetchCIJobLogBody(ctx context.Context, path, token string, allowLoopbackHTTP bool) ([]byte, error) {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "://") {
		return nil, errCIJobLogUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, errCIJobLogUnavailable
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, errCIJobLogUnavailable
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return readCIJobLogBody(resp.Body)
	case http.StatusFound:
		return c.followCIJobLogRedirect(ctx, resp, allowLoopbackHTTP)
	default:
		return nil, errCIJobLogUnavailable
	}
}

// followCIJobLogRedirect accepts exactly one absolute Location on an
// allowlisted blob host, then issues a fresh unauthenticated request. The
// installation token is never replayed to the blob endpoint, and no second
// redirect is followed.
func (c *ciGitHubClient) followCIJobLogRedirect(ctx context.Context, resp *http.Response, allowLoopbackHTTP bool) ([]byte, error) {
	locations := resp.Header.Values("Location")
	if len(locations) != 1 {
		return nil, errCIJobLogUnavailable
	}
	target, err := url.Parse(locations[0])
	if err != nil || !target.IsAbs() || target.User != nil || target.Fragment != "" ||
		!ciJobLogBlobURLAllowed(target, allowLoopbackHTTP) {
		return nil, errCIJobLogUnavailable
	}
	hc := *c.hc
	hc.Jar = nil
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errCIJobLogUnavailable
	}
	blob, err := hc.Do(req)
	if err != nil {
		return nil, errCIJobLogUnavailable
	}
	defer blob.Body.Close()
	if blob.StatusCode != http.StatusOK {
		return nil, errCIJobLogUnavailable
	}
	return readCIJobLogBody(blob.Body)
}

// ciJobLogBlobURLAllowed restricts redirect targets to strict subdomains of
// the GitHub blob hosts over HTTPS on the default port, with no userinfo or
// fragment. The private loopback seam exists only for local tests and admits
// plain http(s) loopback URLs on any port.
func ciJobLogBlobURLAllowed(u *url.URL, allowLoopbackHTTP bool) bool {
	host := u.Hostname()
	if host == "" {
		return false
	}
	if allowLoopbackHTTP {
		if host == "localhost" {
			return u.Scheme == "http" || u.Scheme == "https"
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return u.Scheme == "http" || u.Scheme == "https"
		}
	}
	if u.Scheme != "https" {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	return strings.HasSuffix(host, ".blob.core.windows.net") ||
		strings.HasSuffix(host, ".actions.githubusercontent.com")
}

func readCIJobLogBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, ciJobLogMaxBytes+1))
	if err != nil {
		return nil, errCIJobLogUnavailable
	}
	return raw, nil
}
