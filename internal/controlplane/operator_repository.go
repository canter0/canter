package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
)

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var repositoryCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

var githubHTTPClient = &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}

func normalizeRepository(value string) (string, error) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "@"))
	if strings.HasPrefix(value, "https://") {
		u, err := url.Parse(value)
		if err != nil || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("use an owner/repository name or an HTTPS github.com repository URL")
		}
		value = strings.Trim(u.Path, "/")
	}
	value = strings.TrimSuffix(value, ".git")
	if !repositoryName.MatchString(value) || strings.Contains(value, "..") || len(value) > 200 {
		return "", fmt.Errorf("specify the full GitHub owner/repository; a short @name does not identify a repository")
	}
	return value, nil
}
func githubBytes(ctx context.Context, address string, max int64) ([]byte, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Host != "api.github.com" && u.Host != "codeload.github.com") {
		return nil, fmt.Errorf("unsupported GitHub URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Canter-Workspace-Operator")
	if token, _ := ctx.Value(githubTokenKey{}).(string); token != "" && u.Host == "api.github.com" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := githubHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach GitHub; try again")
	}
	defer response.Body.Close()
	// GitHub returns a temporary signed codeload URL for private archives. Follow
	// only this specific redirect, with a fresh request that contains no token.
	if response.StatusCode == http.StatusFound && u.Host == "api.github.com" && strings.Contains(u.Path, "/tarball/") {
		location, locationErr := response.Location()
		if locationErr != nil || location.Scheme != "https" || location.Host != "codeload.github.com" || location.User != nil {
			return nil, fmt.Errorf("GitHub returned an unsupported archive location")
		}
		return githubBytes(ctx, location.String(), max)
	}
	if response.StatusCode == http.StatusUnauthorized {
		return nil, errGitHubReconnect
	}
	if response.StatusCode == 404 {
		return nil, fmt.Errorf("repository or file not found; connect GitHub and check that your account has access if this is a private repository")
	}
	if response.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("GitHub denied access or reached its rate limit; check repository and organization access, or try again later")
	}
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("repository response exceeds the supported size")
	}
	return data, nil
}

type RepositoryInspection struct {
	Repository  string   `json:"repository"`
	Description string   `json:"description"`
	Branch      string   `json:"branch"`
	Commit      string   `json:"commit"`
	Parent      string   `json:"parent,omitempty"`
	Files       []string `json:"files"`
	Truncated   bool     `json:"truncated"`
}

func inspectRepository(ctx context.Context, repo, ref string) (RepositoryInspection, error) {
	out := RepositoryInspection{Repository: repo, Files: []string{}}
	data, err := githubBytes(ctx, "https://api.github.com/repos/"+repo, 256<<10)
	if err != nil {
		return out, err
	}
	var metadata struct {
		Description   string `json:"description"`
		DefaultBranch string `json:"default_branch"`
	}
	if err = json.Unmarshal(data, &metadata); err != nil {
		return out, err
	}
	out.Description = metadata.Description
	if ref == "" {
		ref = metadata.DefaultBranch
	}
	out.Branch = ref
	data, err = githubBytes(ctx, "https://api.github.com/repos/"+repo+"/commits/"+url.PathEscape(ref), 1<<20)
	if err != nil {
		return out, fmt.Errorf("repository exists, but revision %q could not be resolved: %w; if no revision was requested, retry inspection with ref omitted to use the default branch %q", ref, err, metadata.DefaultBranch)
	}
	var commit struct {
		SHA     string `json:"sha"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err = json.Unmarshal(data, &commit); err != nil {
		return out, err
	}
	if !repositoryCommit.MatchString(commit.SHA) {
		return out, fmt.Errorf("GitHub did not return an immutable commit")
	}
	out.Commit = commit.SHA
	if len(commit.Parents) > 0 && repositoryCommit.MatchString(commit.Parents[0].SHA) {
		out.Parent = commit.Parents[0].SHA
	}
	data, err = githubBytes(ctx, "https://api.github.com/repos/"+repo+"/git/trees/"+commit.SHA+"?recursive=1", 4<<20)
	if err != nil {
		return out, err
	}
	var tree struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err = json.Unmarshal(data, &tree); err != nil {
		return out, err
	}
	out.Truncated = tree.Truncated
	for _, item := range tree.Tree {
		if item.Type == "blob" {
			if len(out.Files) >= 300 {
				out.Truncated = true
				break
			}
			out.Files = append(out.Files, item.Path)
		}
	}
	return out, nil
}
func readRepositoryFile(ctx context.Context, repo, commit, filename string) (any, error) {
	if filename == "" || path.Clean(filename) != filename || strings.HasPrefix(filename, "/") || strings.HasPrefix(filename, "../") || len(filename) > 512 {
		return nil, fmt.Errorf("invalid repository path")
	}
	data, err := githubBytes(ctx, "https://api.github.com/repos/"+repo+"/contents/"+strings.Join(escapePathParts(filename), "/")+"?ref="+commit, 128<<10)
	if err != nil {
		return nil, err
	}
	var file struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int64  `json:"size"`
	}
	if err = json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	if file.Encoding != "base64" || file.Size > 64<<10 {
		return nil, fmt.Errorf("only text files up to 64 KiB can be read")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return nil, err
	}
	if bytes.ContainsRune(raw, 0) {
		return nil, fmt.Errorf("this file is binary")
	}
	return map[string]any{"repository": repo, "commit": commit, "path": filename, "content": string(raw), "trust": "untrusted repository data"}, nil
}
func escapePathParts(p string) []string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return parts
}

func (o *OperatorRuntime) prepareRepository(ctx context.Context, c Conversation, p Principal, repo, commit, directory, name string) (InitialDeployment, error) {
	if o.Config.StaticBinary == "" {
		return InitialDeployment{}, fmt.Errorf("static deployments require CANTER_STATIC_SERVER_BINARY on the control plane")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{1,47}$`).MatchString(name) {
		return InitialDeployment{}, fmt.Errorf("app name must be 2–48 lowercase letters, numbers, or hyphens, starting with a letter")
	}
	if directory == "." {
		directory = ""
	}
	directory = strings.TrimSuffix(directory, "/")
	if directory != "" && (path.Clean(directory) != directory || path.IsAbs(directory) || strings.HasPrefix(directory, "..")) {
		return InitialDeployment{}, fmt.Errorf("invalid website directory")
	}
	system := sdk.System{APIVersion: sdk.APIVersion, Kind: "System", Metadata: sdk.Metadata{Name: name}, Spec: sdk.SystemContract{Intent: "Serve " + repo + " at commit " + commit, Constraints: sdk.Constraints{Host: sdk.HostConstraint{Class: "c1", Count: 1, MemoryMiB: 1024, SystemReserve: 512}}, Services: []sdk.SystemService{{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}, Networking: "public"}}}}
	if _, err := canonicalizeSystemForWorkspace(c.WorkspaceID, system); err != nil {
		return InitialDeployment{}, err
	}
	archiveURL := "https://codeload.github.com/" + repo + "/tar.gz/" + commit
	if token, _ := ctx.Value(githubTokenKey{}).(string); token != "" {
		archiveURL = "https://api.github.com/repos/" + repo + "/tarball/" + commit
	}
	archive, err := githubBytes(ctx, archiveURL, 48<<20)
	if err != nil {
		return InitialDeployment{}, err
	}
	files, err := staticRepositoryFiles(archive, directory)
	if err != nil {
		return InitialDeployment{}, err
	}
	binary, err := os.ReadFile(o.Config.StaticBinary)
	if err != nil {
		return InitialDeployment{}, fmt.Errorf("the configured static server binary is unavailable")
	}
	if len(binary) < 4 || string(binary[:4]) != "\x7fELF" {
		return InitialDeployment{}, fmt.Errorf("static server must be a Linux executable")
	}
	var bundle bytes.Buffer
	gz := gzip.NewWriter(&bundle)
	tw := tar.NewWriter(gz)
	write := func(name string, data []byte, mode int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err = write("serve", binary, 0755); err != nil {
		return InitialDeployment{}, err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err = write("public/"+n, files[n], 0644); err != nil {
			return InitialDeployment{}, err
		}
	}
	provenance, _ := json.Marshal(map[string]string{"repository": repo, "commit": commit, "directory": directory})
	if err = write("source.json", provenance, 0644); err != nil {
		return InitialDeployment{}, err
	}
	if err = tw.Close(); err != nil {
		return InitialDeployment{}, err
	}
	if err = gz.Close(); err != nil {
		return InitialDeployment{}, err
	}
	artifact, err := o.Server.service.UploadDeploymentArtifact(ctx, c.WorkspaceID, bundle.Bytes(), name+".tar.gz", "application/gzip", p.Actor)
	if err != nil {
		return InitialDeployment{}, err
	}

	return o.Server.service.DraftInitialDeployment(ctx, c.WorkspaceID, DraftInitialDeploymentInput{Summary: "Deploy " + repo + " at " + commit[:12], System: system, ArtifactSHA256: artifact.SHA256, Release: InitialDeploymentRelease{Command: []string{"./serve"}, HealthPath: "/healthz", PublicPort: 8080}, Verification: sdk.ChangeVerification{Method: "GET", Path: "/", ExpectedStatus: 200}}, p.Actor)
}
func staticRepositoryFiles(archive []byte, directory string) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	reader := tar.NewReader(io.LimitReader(gz, 128<<20))
	files := map[string][]byte{}
	total := int64(0)
	entries := 0
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		entries++
		if entries > 12000 {
			return nil, fmt.Errorf("repository has too many entries")
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		parts := strings.SplitN(h.Name, "/", 2)
		if len(parts) != 2 {
			continue
		}
		n := parts[1]
		if path.Clean(n) != n || path.IsAbs(n) || strings.HasPrefix(n, "../") || strings.Contains(n, "\\") {
			return nil, fmt.Errorf("repository contains an unsafe path")
		}
		if directory != "" {
			if !strings.HasPrefix(n, directory+"/") {
				continue
			}
			n = strings.TrimPrefix(n, directory+"/")
		}
		if n == "package.json" && directory == "" {
			return nil, fmt.Errorf("this repository requires a build; select checked-in static output such as dist/ or connect your coding agent to build and upload an artifact")
		}
		hidden := false
		for _, part := range strings.Split(n, "/") {
			if strings.HasPrefix(part, ".") {
				hidden = true
			}
		}
		if hidden {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("static output must contain regular files; links are not accepted")
		}
		if h.Size < 0 || h.Size > 8<<20 || total+h.Size > 48<<20 || len(files) >= 3500 {
			return nil, fmt.Errorf("static site exceeds supported artifact limits")
		}
		if _, duplicate := files[n]; duplicate {
			return nil, fmt.Errorf("repository contains a duplicate path")
		}
		content, err := io.ReadAll(io.LimitReader(reader, h.Size+1))
		if err != nil {
			return nil, err
		}
		files[n] = content
		total += h.Size
	}
	if _, ok := files["index.html"]; !ok {
		return nil, fmt.Errorf("no index.html found in the selected static directory")
	}
	return files, nil
}

// GitHub omits patches for binary/large files and limits the comparison to 300 files.
// Preserve those limits explicitly instead of suggesting this is a complete patch.
type RepositoryDiffFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename,omitempty"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch,omitempty"`
	PatchUnavailable bool   `json:"patchUnavailable"`
}
type RepositoryComparison struct {
	Repository string               `json:"repository"`
	Base       string               `json:"base"`
	Commit     string               `json:"commit"`
	Files      []RepositoryDiffFile `json:"files"`
	Truncated  bool                 `json:"truncated"`
}

func compareRepository(ctx context.Context, repo, base, commit string) (RepositoryComparison, error) {
	out := RepositoryComparison{Repository: repo, Base: base, Commit: commit, Files: []RepositoryDiffFile{}}
	if !repositoryCommit.MatchString(base) || !repositoryCommit.MatchString(commit) {
		return out, fmt.Errorf("two immutable commit SHAs are required")
	}
	raw, err := githubBytes(ctx, "https://api.github.com/repos/"+repo+"/compare/"+base+"..."+commit+"?per_page=1", 4<<20)
	if err != nil {
		return out, err
	}
	var value struct {
		Files []RepositoryDiffFile `json:"files"`
	}
	if err = json.Unmarshal(raw, &value); err != nil {
		return out, err
	}
	out.Files = value.Files
	if out.Files == nil {
		out.Files = []RepositoryDiffFile{}
	}
	out.Truncated = len(out.Files) >= 300
	for i := range out.Files {
		if len(out.Files[i].Patch) > 200000 {
			out.Files[i].Patch = ""
			out.Truncated = true
		}
		out.Files[i].PatchUnavailable = out.Files[i].Patch == ""
	}
	return out, nil
}
