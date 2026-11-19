package controlplane

import (
	"archive/tar"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const pluginMarketplaceRepository = "CognitionAI/devin-marketplace"

//
//go:embed plugin_marketplace.json
var pluginMarketplaceSnapshot []byte

type pluginInput struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Help      string `json:"help,omitempty"`
	Required  bool   `json:"required"`
	Sensitive bool   `json:"sensitive"`
	Default   string `json:"default,omitempty"`
}

type pluginServer struct {
	Name          string            `json:"name"`
	URL           string            `json:"url,omitempty"`
	Command       string            `json:"command,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	OAuthClientID string            `json:"oauthClientId,omitempty"`
	OAuthScopes   []string          `json:"oauthScopes,omitempty"`
}

type pluginCatalogEntry struct {
	Slug        string         `json:"slug"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Summary     string         `json:"summary"`
	Homepage    string         `json:"homepage"`
	Source      string         `json:"source"`
	Revision    string         `json:"revision"`
	Icon        string         `json:"icon,omitempty"`
	Servers     []pluginServer `json:"servers"`
	Inputs      []pluginInput  `json:"inputs"`
	Notice      string         `json:"notice,omitempty"`
	Steps       []string       `json:"steps,omitempty"`
}

type pluginCatalog struct {
	Revision  string               `json:"revision"`
	Source    string               `json:"source"`
	UpdatedAt time.Time            `json:"updatedAt"`
	Cached    bool                 `json:"cached"`
	Warning   string               `json:"warning,omitempty"`
	Plugins   []pluginCatalogEntry `json:"plugins"`
}

type pluginSnapshot struct {
	Revision  string                     `json:"revision"`
	Manifests map[string]json.RawMessage `json:"manifests"`
}

var pluginPlaceholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)
var pluginSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)
var pluginRevisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var marketplaceCache struct {
	sync.Mutex
	value pluginCatalog
	next  time.Time
}

func parsePluginSnapshot(snapshot pluginSnapshot) (pluginCatalog, error) {
	if !pluginRevisionPattern.MatchString(snapshot.Revision) {
		return pluginCatalog{}, fmt.Errorf("invalid marketplace revision")
	}
	catalog := pluginCatalog{Revision: snapshot.Revision, Source: "https://github.com/" + pluginMarketplaceRepository, Plugins: []pluginCatalogEntry{}}
	for location, raw := range snapshot.Manifests {
		slug := strings.TrimPrefix(location, "plugins/")
		if location != "plugins/"+slug || !pluginSlugPattern.MatchString(slug) {
			continue
		}
		var m struct {
			Name, DisplayName, Description, ShortDescription, Homepage, Logo string
			MCPServers                                                       map[string]pluginServer `json:"mcpServers"`
			UserConfig                                                       map[string]struct {
				Title, Description  string
				Required, Sensitive bool
				Default             json.RawMessage
			} `json:"user_config"`
			Setup struct {
				Notice string
				Steps  []string
			}
		}
		if len(raw) > 128<<10 || json.Unmarshal(raw, &m) != nil || m.Name == "" {
			continue
		}
		entry := pluginCatalogEntry{Slug: slug, Name: m.DisplayName, Description: m.Description, Summary: m.ShortDescription, Homepage: m.Homepage, Revision: snapshot.Revision,
			Source:  catalog.Source + "/tree/" + snapshot.Revision + "/" + location,
			Servers: []pluginServer{}, Inputs: []pluginInput{}, Notice: m.Setup.Notice, Steps: m.Setup.Steps}
		if entry.Name == "" {
			entry.Name = m.Name
		}
		if m.Logo != "" && path.Base(m.Logo) == m.Logo {
			entry.Icon = "/marketplace/" + slug + ".png?v=2"
		}
		fields := map[string]pluginInput{}
		for name, config := range m.UserConfig {
			var defaultValue string
			_ = json.Unmarshal(config.Default, &defaultValue)
			fields[name] = pluginInput{Name: name, Label: config.Title, Help: config.Description, Required: config.Required, Sensitive: config.Sensitive, Default: defaultValue}
		}
		addFields := func(template string, sensitive bool) {
			for _, match := range pluginPlaceholder.FindAllStringSubmatch(template, -1) {
				name, def := match[1], match[2]
				field, exists := fields[name]
				if !exists {
					field = pluginInput{Name: name, Label: strings.ReplaceAll(strings.ToLower(name), "_", " "), Required: !strings.Contains(match[0], ":-"), Default: def}
				}
				field.Sensitive = field.Sensitive || sensitive
				fields[name] = field
			}
		}
		for name, server := range m.MCPServers {
			server.Name = name
			addFields(server.URL, false)
			for _, template := range server.Headers {
				addFields(template, true)
			}
			// Executable metadata describes compatibility only; Canter never runs
			// package commands on the control-plane host.
			entry.Servers = append(entry.Servers, server)
		}
		for _, field := range fields {
			entry.Inputs = append(entry.Inputs, field)
		}
		sort.Slice(entry.Inputs, func(i, j int) bool { return entry.Inputs[i].Name < entry.Inputs[j].Name })
		sort.Slice(entry.Servers, func(i, j int) bool { return entry.Servers[i].Name < entry.Servers[j].Name })
		catalog.Plugins = append(catalog.Plugins, entry)
	}
	sort.Slice(catalog.Plugins, func(i, j int) bool {
		return strings.ToLower(catalog.Plugins[i].Name) < strings.ToLower(catalog.Plugins[j].Name)
	})
	if len(catalog.Plugins) == 0 {
		return pluginCatalog{}, fmt.Errorf("marketplace contains no readable plugins")
	}
	return catalog, nil
}

// The source is fixed. Only public metadata is read, bounded in memory; no
// archive extraction, packages, git hooks, credential requests or scripts.
func fetchPluginMarketplace(ctx context.Context) (pluginCatalog, error) {
	client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(endpoint string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Canter-Plugin-Catalog/1.0")
		res, err := client.Do(req)
		if err == nil && res.StatusCode != 200 {
			res.Body.Close()
			return nil, fmt.Errorf("marketplace returned HTTP %d", res.StatusCode)
		}
		return res, err
	}
	res, err := get("https://api.github.com/repos/" + pluginMarketplaceRepository + "/commits/main")
	if err != nil {
		return pluginCatalog{}, err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&commit)
	res.Body.Close()
	if err != nil || !pluginRevisionPattern.MatchString(commit.SHA) {
		return pluginCatalog{}, fmt.Errorf("marketplace revision unavailable")
	}
	res, err = get("https://codeload.github.com/" + pluginMarketplaceRepository + "/tar.gz/" + commit.SHA)
	if err != nil {
		return pluginCatalog{}, err
	}
	defer res.Body.Close()
	gz, err := gzip.NewReader(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return pluginCatalog{}, err
	}
	defer gz.Close()
	reader := tar.NewReader(io.LimitReader(gz, 64<<20))
	files := map[string]json.RawMessage{}
	for {
		header, e := reader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return pluginCatalog{}, fmt.Errorf("marketplace archive is invalid")
		}
		if header.Typeflag != tar.TypeReg || header.Size > 128<<10 {
			continue
		}
		_, name, found := strings.Cut(header.Name, "/")
		if !found || (name != ".devin-plugin/plugin.json" && !strings.HasSuffix(name, "/.devin-plugin/plugin.json")) {
			continue
		}
		body, e := io.ReadAll(io.LimitReader(reader, 128<<10))
		if e != nil {
			return pluginCatalog{}, e
		}
		files[name] = body
	}
	var root struct {
		OptionalPlugins []json.RawMessage `json:"optionalPlugins"`
	}
	if json.Unmarshal(files[".devin-plugin/plugin.json"], &root) != nil {
		return pluginCatalog{}, fmt.Errorf("marketplace index is missing")
	}
	snapshot := pluginSnapshot{Revision: commit.SHA, Manifests: map[string]json.RawMessage{}}
	for _, raw := range root.OptionalPlugins {
		var location string
		if json.Unmarshal(raw, &location) != nil || !strings.HasPrefix(location, "./plugins/") {
			continue
		}
		location = strings.TrimPrefix(location, "./")
		if body := files[location+"/.devin-plugin/plugin.json"]; body != nil {
			snapshot.Manifests[location] = body
		}
	}
	catalog, err := parsePluginSnapshot(snapshot)
	catalog.UpdatedAt = time.Now().UTC()
	return catalog, err
}

func currentPluginMarketplace(ctx context.Context) (pluginCatalog, error) {
	marketplaceCache.Lock()
	defer marketplaceCache.Unlock()
	if time.Now().Before(marketplaceCache.next) {
		return marketplaceCache.value, nil
	}
	catalog, err := fetchPluginMarketplace(ctx)
	if err != nil {
		// A navigation or Strict Mode abort must not poison the process-wide
		// source cache for everyone else.
		if ctx.Err() != nil {
			return pluginCatalog{}, ctx.Err()
		}
		catalog = marketplaceCache.value
		if len(catalog.Plugins) == 0 {
			var snapshot pluginSnapshot
			if json.Unmarshal(pluginMarketplaceSnapshot, &snapshot) != nil {
				return pluginCatalog{}, err
			}
			catalog, err = parsePluginSnapshot(snapshot)
			if err != nil {
				return pluginCatalog{}, err
			}
		}
		catalog.Cached = true
		catalog.Warning = "The marketplace could not be refreshed. Showing the last saved catalog."
		marketplaceCache.next = time.Now().Add(5 * time.Minute)
	} else {
		marketplaceCache.next = time.Now().Add(time.Hour)
	}
	marketplaceCache.value = catalog
	return catalog, nil
}
