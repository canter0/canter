package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Catalog entries describe integrations built by Canter, not partnerships or
// credentials. Preparing a package never grants a conversation access to it.
type WrenConnectorCatalogEntry struct {
	Slug          string                `json:"slug"`
	Category      string                `json:"category"`
	Website       string                `json:"website"`
	Documentation string                `json:"documentation"`
	Limitation    string                `json:"limitation"`
	Example       map[string]any        `json:"example"`
	Manifest      WrenExtensionManifest `json:"manifest"`
}

func wrenConnectorCatalog() []WrenConnectorCatalogEntry {
	return []WrenConnectorCatalogEntry{
		{Slug: "github-public", Category: "Development", Website: "https://github.com", Documentation: "https://docs.github.com/en/rest/search/search", Limitation: "Public repositories and issues only. No private repositories, code search, or write access. GitHub applies shared anonymous rate limits.", Example: map[string]any{"q": "go user:golang", "per_page": 3}, Manifest: WrenExtensionManifest{Name: "GitHub public search", Description: "Find public repositories and investigate public issues with your Wren.", Origin: "https://api.github.com", Auth: "none", Events: []string{}, Tools: []WrenExtensionTool{
			{Name: "search_repositories", Description: "Search public GitHub repositories. q accepts GitHub search syntax; keep per_page small and cite returned html_url links. Check incomplete_results before claiming completeness.", Endpoint: "https://api.github.com/search/repositories", Method: "GET", Impact: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string","minLength":1,"maxLength":256},"per_page":{"type":"integer","minimum":1,"maximum":5},"page":{"type":"integer","minimum":1,"maximum":20}},"required":["q","per_page"],"additionalProperties":false}`)},
			{Name: "search_issues", Description: "Search public GitHub issues and pull requests using q, for example repo:golang/go is:issue is:open. Cite html_url and check incomplete_results.", Endpoint: "https://api.github.com/search/issues", Method: "GET", Impact: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string","minLength":1,"maxLength":256},"per_page":{"type":"integer","minimum":1,"maximum":3},"page":{"type":"integer","minimum":1,"maximum":20}},"required":["q","per_page"],"additionalProperties":false}`)},
		}}},
		{Slug: "crossref", Category: "Research", Website: "https://www.crossref.org", Documentation: "https://www.crossref.org/documentation/retrieve-metadata/rest-api/", Limitation: "Searches publication metadata, not full papers. Records may be incomplete; verify publication details and cite the DOI. Crossref's public API rate limits apply.", Example: map[string]any{"query": "attention is all you need", "rows": 3}, Manifest: WrenExtensionManifest{Name: "Crossref", Description: "Find research papers, authors, publication dates, and DOI references.", Origin: "https://api.crossref.org", Auth: "none", Events: []string{}, Tools: []WrenExtensionTool{
			{Name: "search_publications", Description: "Find scholarly publication metadata. Use query for keywords or a paper title, rows up to 5. Cite returned DOI URLs; metadata is not evidence that you read the full paper.", Endpoint: "https://api.crossref.org/works", Method: "GET", Impact: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":256},"rows":{"type":"integer","minimum":1,"maximum":5},"offset":{"type":"integer","minimum":0,"maximum":100}},"required":["query","rows"],"additionalProperties":false}`)},
		}}},
		{Slug: "deepwiki", Category: "Development", Website: "https://deepwiki.com", Documentation: "https://docs.devin.ai/work-with-devin/deepwiki-mcp", Limitation: "Public, indexed GitHub repositories only. Answers are AI-generated repository documentation; verify important details against source code. Repository names and questions go to DeepWiki.", Example: map[string]any{"repoName": "modelcontextprotocol/go-sdk"}, Manifest: WrenExtensionManifest{Name: "DeepWiki", Description: "Explore a public repository’s architecture and ask questions about its code.", Origin: "https://mcp.deepwiki.com", Auth: "none", Events: []string{}, Tools: []WrenExtensionTool{
			{Name: "read_wiki_structure", Method: "MCP", Endpoint: "https://mcp.deepwiki.com/mcp", Impact: "read", Description: "Read the documentation structure for a public GitHub repository. Use owner/repo and cite https://deepwiki.com/owner/repo. Documentation is AI-generated, not a verified source-code inspection.", InputSchema: json.RawMessage(`{"type":"object","properties":{"repoName":{"type":"string","minLength":3,"maxLength":200}},"required":["repoName"],"additionalProperties":false}`)},
			{Name: "ask_wiki_question", Method: "MCP", Endpoint: "https://mcp.deepwiki.com/mcp", Impact: "read", Description: "Ask a question about one public GitHub repository’s code and architecture. repoName uses owner/repo. Cite returned source links; distinguish generated documentation from independently verified source code.", InputSchema: json.RawMessage(`{"type":"object","properties":{"repoName":{"type":"string","minLength":3,"maxLength":200},"question":{"type":"string","minLength":1,"maxLength":2000}},"required":["repoName","question"],"additionalProperties":false}`)},
		}}},
	}
}
func extensionQueryArguments(raw json.RawMessage) (string, error) {
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return "", fmt.Errorf("invalid query arguments")
	}
	query := url.Values{}
	for key, value := range values {
		switch v := value.(type) {
		case string:
			query.Set(key, v)
		case float64:
			query.Set(key, strconv.FormatFloat(v, 'f', -1, 64))
		case bool:
			query.Set(key, strconv.FormatBool(v))
		default:
			return "", fmt.Errorf("GET tool arguments must be strings, numbers or booleans")
		}
	}
	encoded := query.Encode()
	if len(encoded) > 8192 {
		return "", fmt.Errorf("query arguments exceed 8 KiB")
	}
	return encoded, nil
}
func (s *Store) prepareWrenCatalogConnector(ctx context.Context, workspace, account, slug string) (WrenExtension, error) {
	if err := s.RequireWorkspaceWriter(ctx, workspace, account); err != nil {
		return WrenExtension{}, err
	}
	for _, entry := range wrenConnectorCatalog() {
		if entry.Slug != slug {
			continue
		}
		raw, _ := json.Marshal(entry.Manifest)
		digest := sha256.Sum256(append([]byte(workspace+":"), raw...))
		id := "extension_catalog_" + hex.EncodeToString(digest[:16])
		existing, err := s.WrenExtension(ctx, workspace, account, id, 0)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return WrenExtension{}, err
		}
		result, err := s.SaveWrenExtension(ctx, workspace, account, WrenExtensionInput{ID: id, Manifest: entry.Manifest})
		if errors.Is(err, ErrForbidden) || errors.Is(err, ErrConflict) {
			return s.WrenExtension(ctx, workspace, account, id, 0)
		}
		return result, err
	}
	return WrenExtension{}, ErrNotFound
}
func (h *HTTPServer) wrenConnectorCatalog(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if len(parts) == 0 && r.Method == http.MethodGet {
		if _, err := workspaceRole(r.Context(), h.service.Store.pool, workspace, p.Account.ID, false); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"connectors": wrenConnectorCatalog()})
		return
	}
	if len(parts) == 2 && parts[1] == "prepare" && r.Method == http.MethodPost {
		result, err := h.service.Store.prepareWrenCatalogConnector(r.Context(), workspace, p.Account.ID, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, result)
		return
	}
	writeStoreError(w, ErrNotFound)
}
