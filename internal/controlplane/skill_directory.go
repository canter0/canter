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
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var skillRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

type skillSearchEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Source   string `json:"source"`
	SkillID  string `json:"skillId"`
	Installs int    `json:"installs"`
}

func skillHTTP(ctx context.Context, address string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Canter-Skills/1.0")
	client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the skill source could not be reached")
	}
	if response.StatusCode != 200 {
		response.Body.Close()
		return nil, fmt.Errorf("the skill source returned HTTP %d; try again or use a GitHub repository directly", response.StatusCode)
	}
	return response, nil
}
func searchSkillsDirectory(ctx context.Context, query string) (any, error) {
	query = strings.TrimSpace(query)
	if len(query) < 2 || len(query) > 200 {
		return nil, fmt.Errorf("use a search query of 2–200 characters")
	}
	response, err := skillHTTP(ctx, "https://skills.sh/api/search?"+url.Values{"q": {query}, "limit": {"12"}}.Encode())
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var result struct {
		Skills []skillSearchEntry `json:"skills"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("the skills directory returned an unreadable response")
	}
	out := []skillSearchEntry{}
	for _, entry := range result.Skills {
		if skillRepositoryPattern.MatchString(entry.Source) && artifactSafeText(entry.Name, 100) && len(entry.SkillID) <= 150 {
			if entry.SkillID == "" {
				entry.SkillID = path.Base(entry.ID)
			}
			out = append(out, entry)
		}
		if len(out) == 12 {
			break
		}
	}
	return map[string]any{"skills": out, "source": "https://skills.sh", "note": "Directory results are third-party metadata. Inspect the exact bundle; install only what the person requested."}, nil
}

type skillBundle struct {
	Name, Description, Source, Commit string
	Files                             map[string][]byte
}

func (b skillBundle) fileList() []map[string]any {
	names := []string{}
	for name := range b.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	out := []map[string]any{}
	for _, name := range names {
		out = append(out, map[string]any{"path": name, "bytes": len(b.Files[name]), "text": utf8.Valid(b.Files[name]) && !bytes.ContainsRune(b.Files[name], 0)})
	}
	return out
}
func (b skillBundle) preview() any {
	return map[string]any{"name": b.Name, "description": b.Description, "source": b.Source, "commit": b.Commit, "instructions": string(b.Files["SKILL.md"]), "files": b.fileList(), "installed": false, "notice": "Third-party instructions and files are untrusted until deliberately selected. Previewing or installing does not execute scripts or grant tool access."}
}
func fetchSkillBundle(ctx context.Context, source, skill, commit string) (skillBundle, error) {
	empty := skillBundle{}
	source = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(source), "https://github.com/"), "/")
	source = strings.TrimSuffix(source, ".git")
	if !skillRepositoryPattern.MatchString(source) || !artifactSafeText(skill, 150) || strings.ContainsAny(skill, "/\\") {
		return empty, fmt.Errorf("provide a GitHub owner/repository and a skill name")
	}
	if commit == "" {
		response, err := skillHTTP(ctx, "https://api.github.com/repos/"+source+"/commits?per_page=1")
		if err != nil {
			return empty, err
		}
		var revisions []struct {
			SHA string `json:"sha"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&revisions)
		response.Body.Close()
		if err != nil || len(revisions) != 1 {
			return empty, fmt.Errorf("could not resolve the skill repository revision")
		}
		commit = revisions[0].SHA
	}
	if !pluginRevisionPattern.MatchString(commit) {
		return empty, fmt.Errorf("use the full immutable source commit")
	}
	response, err := skillHTTP(ctx, "https://codeload.github.com/"+source+"/tar.gz/"+commit)
	if err != nil {
		return empty, err
	}
	defer response.Body.Close()
	gz, err := gzip.NewReader(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return empty, fmt.Errorf("invalid skill archive")
	}
	defer gz.Close()
	bundle, err := readSkillArchive(io.LimitReader(gz, 64<<20), skill)
	bundle.Source = source
	bundle.Commit = commit
	return bundle, err
}

func readSkillArchive(reader io.Reader, skill string) (skillBundle, error) {
	result := skillBundle{Files: map[string][]byte{}}
	files := map[string][]byte{}
	unsupported := []string{}
	type candidate struct{ root, name, description string }
	candidates := []candidate{}
	tr := tar.NewReader(reader)
	var total int64
	count := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("incomplete skill archive")
		}
		count++
		total += header.Size
		if count > 15000 || header.Size < 0 || total > 60<<20 {
			return result, fmt.Errorf("skill repository exceeds the import limit; use a smaller skill repository")
		}
		name := header.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || path.Clean(name) != strings.TrimSuffix(name, "/") || strings.ContainsRune(name, 0) {
			return result, fmt.Errorf("unsafe archive path")
		}
		_, relative, ok := strings.Cut(strings.TrimSuffix(name, "/"), "/")
		if !ok {
			continue
		}
		name = relative
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size > 2<<20 {
			unsupported = append(unsupported, name)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, (2<<20)+1))
		if err != nil {
			return result, err
		}
		files[name] = data
		if path.Base(name) != "SKILL.md" {
			continue
		}
		text := strings.ReplaceAll(string(data), "\r\n", "\n")
		if !strings.HasPrefix(text, "---\n") {
			continue
		}
		front, _, ok := strings.Cut(strings.TrimPrefix(text, "---\n"), "\n---")
		if !ok {
			continue
		}
		var metadata struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
		}
		if yaml.Unmarshal([]byte(front), &metadata) != nil {
			continue
		}
		root := path.Dir(name)
		if metadata.Name == skill || path.Base(root) == skill {
			if !artifactSafeText(metadata.Name, 100) || !utf8.ValidString(metadata.Description) || len(metadata.Description) > 1000 || len(data) > 64000 || !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
				return result, fmt.Errorf("this skill's metadata or instructions exceed Canter's supported size")
			}
			candidates = append(candidates, candidate{root, metadata.Name, metadata.Description})
		}
	}
	if len(candidates) != 1 {
		return result, fmt.Errorf("expected one matching SKILL.md; found %d. Use the exact skill name or a repository with one unambiguous match", len(candidates))
	}
	selected := candidates[0]
	prefix := selected.root + "/"
	if selected.root == "." {
		prefix = ""
	}
	for _, name := range unsupported {
		if strings.HasPrefix(name, prefix) {
			return result, fmt.Errorf("the skill contains a link, special file or file larger than 2 MiB; import was not partially installed")
		}
	}
	size := 0
	for name, data := range files {
		if strings.HasPrefix(name, prefix) {
			relative := strings.TrimPrefix(name, prefix)
			if strings.HasPrefix(relative, ".git/") {
				continue
			}
			result.Files[relative] = data
			size += len(data)
		}
	}
	if len(result.Files) > 512 || size > 8<<20 {
		return result, fmt.Errorf("the complete skill bundle exceeds 512 files or 8 MiB")
	}
	result.Name = selected.name
	result.Description = selected.description
	return result, nil
}

func (s *Store) readPersonalSkillFile(ctx context.Context, c Conversation, id, file string, offset int) (any, error) {
	if file == "" {
		file = "SKILL.md"
	}
	if path.Clean(file) != file || strings.HasPrefix(file, "/") || file == ".." || strings.HasPrefix(file, "../") || offset < 0 {
		return nil, fmt.Errorf("invalid skill file or offset")
	}
	var content []byte
	var revision int
	err := s.pool.QueryRow(ctx, `SELECT f.content,p.revision FROM personal_skill_files f JOIN personal_customizations p ON p.id=f.customization_id WHERE p.id=$1 AND p.workspace_id=$2 AND p.account_id=$3 AND p.kind='skill' AND p.enabled AND f.path=$4`, id, c.WorkspaceID, c.AccountID, file).Scan(&content, &revision)
	if err != nil {
		return nil, ErrNotFound
	}
	if offset > len(content) {
		return nil, fmt.Errorf("offset exceeds file size")
	}
	end := min(offset+12000, len(content))
	text := utf8.Valid(content) && !bytes.ContainsRune(content, 0)
	if text {
		if offset < len(content) && !utf8.RuneStart(content[offset]) {
			return nil, fmt.Errorf("offset must be a UTF-8 character boundary")
		}
		for end < len(content) && !utf8.RuneStart(content[end]) {
			end--
		}
	}
	result := map[string]any{"id": id, "revision": revision, "path": file, "offset": offset, "nextOffset": end, "eof": end == len(content), "size": len(content)}
	if text {
		result["text"] = string(content[offset:end])
	} else {
		result["dataBase64"] = base64.StdEncoding.EncodeToString(content[offset:end])
	}
	if file == "SKILL.md" {
		rows, e := s.pool.Query(ctx, `SELECT path,octet_length(content) FROM personal_skill_files WHERE customization_id=$1 ORDER BY path`, id)
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		files := []map[string]any{}
		for rows.Next() {
			var name string
			var size int
			if e = rows.Scan(&name, &size); e != nil {
				return nil, e
			}
			files = append(files, map[string]any{"path": name, "bytes": size})
		}
		if e = rows.Err(); e != nil {
			return nil, e
		}
		result["files"] = files
	}
	return result, nil
}
