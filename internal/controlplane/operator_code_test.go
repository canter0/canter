package controlplane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRepositoryComparisonUsesPinnedCommitsAndReportsMissingPatches(t *testing.T) {
	old := githubHTTPClient
	t.Cleanup(func() { githubHTTPClient = old })
	calls := 0
	githubHTTPClient = &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer scoped-token" || !strings.HasSuffix(r.URL.Path, strings.Repeat("a", 40)+"..."+strings.Repeat("b", 40)) {
			t.Error("comparison lost pinned scope or authentication")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"files":[{"filename":"app.js","status":"modified","additions":1,"deletions":1,"patch":"@@ -1 +1 @@\n-old\n+new"},{"filename":"image.png","status":"added","additions":0,"deletions":0}]}`)), Header: http.Header{}}, nil
	})}
	ctx := context.WithValue(context.Background(), githubTokenKey{}, "scoped-token")
	if _, err := compareRepository(ctx, "o/r", "main", strings.Repeat("b", 40)); err == nil || calls != 0 {
		t.Fatal("mutable ref accepted")
	}
	diff, err := compareRepository(ctx, "o/r", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil || len(diff.Files) != 2 || diff.Files[0].PatchUnavailable || !diff.Files[1].PatchUnavailable {
		t.Fatalf("missing patch misrepresented: %+v %v", diff, err)
	}
}
func TestRepositoryCodeRoutesRequireWorkspaceMembership(t *testing.T) {
	s, c, human := operatorFixture(t)
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	old := githubHTTPClient
	t.Cleanup(func() { githubHTTPClient = old })
	githubHTTPClient = &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"files":[]}`)), Header: http.Header{}}, nil
	})}
	_, _, outsider, err := s.Signup(context.Background(), "code-outsider@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/workspaces/" + c.WorkspaceID + "/github/compare?repository=o/r&base=" + strings.Repeat("a", 40) + "&commit=" + strings.Repeat("b", 40)
	for _, item := range []struct {
		token   string
		allowed bool
	}{{human, true}, {outsider, false}, {"", false}} {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: "canter_session", Value: item.token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if (w.Code == 200) != item.allowed {
			t.Fatalf("access mismatch: %d allowed=%v", w.Code, item.allowed)
		}
	}
}

func TestRepositoryRevisionErrorKeepsValidRepositoryContext(t *testing.T) {
	old := githubHTTPClient
	t.Cleanup(func() { githubHTTPClient = old })
	githubHTTPClient = &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		status, body := 200, `{"default_branch":"main"}`
		if strings.Contains(r.URL.Path, "/commits/") {
			status, body = 422, `{"message":"invalid ref"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	_, err := inspectRepository(context.Background(), "o/r", "o/r")
	if err == nil || !strings.Contains(err.Error(), "repository exists") || !strings.Contains(err.Error(), "ref omitted") {
		t.Fatalf("invalid revision misidentified as missing repository: %v", err)
	}
}
