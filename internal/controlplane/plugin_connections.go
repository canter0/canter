package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type pluginConnection struct {
	ID       string             `json:"id"`
	Catalog  pluginCatalogEntry `json:"catalog"`
	State    string             `json:"state"`
	Failure  string             `json:"failure,omitempty"`
	Revision int                `json:"revision"`
	Enabled  bool               `json:"enabled"`
}
type pluginSecret struct {
	Revision  int                `json:"-"`
	Endpoint  string             `json:"endpoint"`
	Headers   map[string]string  `json:"headers"`
	Discovery wrenMCPDiscovery   `json:"discovery"`
	OAuth     *pluginOAuthSecret `json:"oauth,omitempty"`
}
type pluginHeaderContext struct{}
type pluginEndpointHeaders struct {
	Endpoint string
	Headers  map[string]string
}

func pluginSecretBinding(workspace, account, id string) []byte {
	return []byte("canter:plugin:v1\x00" + workspace + "\x00" + account + "\x00" + id)
}

func scanPluginConnection(row interface{ Scan(...any) error }) (pluginConnection, error) {
	var v pluginConnection
	err := row.Scan(&v.ID, &v.Catalog, &v.State, &v.Failure, &v.Revision, &v.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}

const pluginConnectionColumns = `id,catalog,state,failure,revision,enabled`

func (h *HTTPServer) getPluginConnection(ctx context.Context, workspace, account, id string) (pluginConnection, error) {
	return scanPluginConnection(h.service.Store.pool.QueryRow(ctx, `SELECT `+pluginConnectionColumns+` FROM plugin_connections WHERE id=$1 AND workspace_id=$2 AND account_id=$3`, id, workspace, account))
}
func (h *HTTPServer) readPluginSecret(ctx context.Context, q artifactQueryer, workspace, account, id string) (pluginSecret, error) {
	var secret pluginSecret
	var key string
	var sealed []byte
	if err := q.QueryRow(ctx, `SELECT key_id,ciphertext FROM plugin_connections WHERE id=$1 AND workspace_id=$2 AND account_id=$3`, id, workspace, account).Scan(&key, &sealed); err != nil {
		return secret, err
	}
	if len(sealed) == 0 {
		return secret, nil
	}
	if h.config.Secrets == nil {
		return secret, fmt.Errorf("plugin credential storage is unavailable")
	}
	raw, err := h.config.Secrets.open(key, sealed, pluginSecretBinding(workspace, account, id))
	if err != nil {
		return secret, err
	}
	defer clear(raw)
	err = json.Unmarshal(raw, &secret)
	return secret, err
}
func (h *HTTPServer) savePluginSecret(ctx context.Context, workspace, account string, v pluginConnection, secret pluginSecret, state, failure string) (pluginConnection, error) {
	if h.config.Secrets == nil {
		return v, fmt.Errorf("plugin credential storage is unavailable")
	}
	raw, _ := json.Marshal(secret)
	defer clear(raw)
	key, sealed, err := h.config.Secrets.seal(raw, pluginSecretBinding(workspace, account, v.ID))
	if err != nil {
		return v, err
	}
	tag, err := h.service.Store.pool.Exec(ctx, `UPDATE plugin_connections SET key_id=$5,ciphertext=$6,state=$7,failure=$8,revision=revision+1,updated_at=now() WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND revision=$4 AND enabled`, v.ID, workspace, account, v.Revision, key, sealed, state, failure)
	if err != nil {
		return v, err
	}
	if tag.RowsAffected() != 1 {
		return v, ErrConflict
	}
	return h.getPluginConnection(ctx, workspace, account, v.ID)
}

func resolvePluginServer(entry pluginCatalogEntry, serverName string, values map[string]string, token string, config WrenExtensionConfig) (pluginSecret, error) {
	out := pluginSecret{Headers: map[string]string{}}
	var server *pluginServer
	for i := range entry.Servers {
		if entry.Servers[i].Name == serverName || serverName == "" && len(entry.Servers) == 1 {
			server = &entry.Servers[i]
			break
		}
	}
	if server == nil || server.URL == "" {
		return out, fmt.Errorf("this plugin requires a local runtime; choose a hosted connection")
	}
	fields := map[string]pluginInput{}
	for _, field := range entry.Inputs {
		fields[field.Name] = field
	}
	for key := range values {
		if _, ok := fields[key]; !ok {
			return out, fmt.Errorf("unknown setup field")
		}
	}
	expand := func(template string, endpoint bool) (string, error) {
		var failure error
		value := pluginPlaceholder.ReplaceAllStringFunc(template, func(match string) string {
			parts := pluginPlaceholder.FindStringSubmatch(match)
			field := fields[parts[1]]
			v := values[parts[1]]
			if v == "" {
				v = field.Default
			}
			if v == "" && field.Required {
				failure = fmt.Errorf("enter %s", field.Label)
			}
			if len(v) > 4096 || strings.ContainsAny(v, "\r\n\x00") {
				failure = fmt.Errorf("invalid setup value")
			}
			if endpoint && (field.Sensitive || strings.ContainsAny(v, "/?#@:&\\")) {
				failure = fmt.Errorf("URL fields must be a hostname or path segment, without credentials")
			}
			return v
		})
		if strings.Contains(value, "${") {
			failure = fmt.Errorf("unsupported configuration variable")
		}
		return value, failure
	}
	endpoint, err := expand(server.URL, true)
	if err != nil {
		return out, err
	}
	u, _, err := extensionURL(endpoint, config)
	if err != nil {
		return out, err
	}
	out.Endpoint = u.String()
	for key, template := range server.Headers {
		canonical := http.CanonicalHeaderKey(key)
		// Keep hop-by-hop, session, cookie and routing headers under Canter's control.
		if canonical != "Authorization" && !strings.HasPrefix(canonical, "X-") && canonical != "Api-Key" {
			return out, fmt.Errorf("this connector declares an unsupported HTTP header")
		}
		value, err := expand(template, false)
		if err != nil {
			return out, err
		}
		out.Headers[canonical] = value
	}
	if token != "" {
		if len(token) < 8 || len(token) > 4096 || strings.ContainsAny(token, "\r\n\x00") {
			return out, fmt.Errorf("enter a valid API token")
		}
		out.Headers["Authorization"] = "Bearer " + token
	}
	return out, nil
}

func pluginContext(ctx context.Context, secret pluginSecret) context.Context {
	return context.WithValue(ctx, pluginHeaderContext{}, pluginEndpointHeaders{Endpoint: secret.Endpoint, Headers: secret.Headers})
}
func redactPluginDiscovery(discovery wrenMCPDiscovery, headers map[string]string) wrenMCPDiscovery {
	for _, value := range headers {
		discovery = redactMCPDiscovery(discovery, value)
		if strings.HasPrefix(value, "Bearer ") {
			discovery = redactMCPDiscovery(discovery, strings.TrimPrefix(value, "Bearer "))
		}
	}
	return discovery
}
func (h *HTTPServer) discoverPlugin(ctx context.Context, workspace, account string, v pluginConnection, secret pluginSecret) (pluginConnection, error) {
	discovery, err := discoverExtensionMCP(pluginContext(ctx, secret), h.config.Extensions, secret.Endpoint, "")
	if errors.Is(err, errMCPUnauthorized) {
		return h.savePluginSecret(ctx, workspace, account, v, secret, "needs_auth", "Sign in or enter an API token to connect.")
	}
	if err != nil {
		return h.savePluginSecret(ctx, workspace, account, v, secret, "failed", "The provider could not be reached or did not return compatible tools. You can retry.")
	}
	secret.Discovery = redactPluginDiscovery(discovery, secret.Headers)
	if len(secret.Discovery.Tools) == 0 {
		return h.savePluginSecret(ctx, workspace, account, v, secret, "failed", "This server returned no compatible tools.")
	}
	// DeepWiki's three public tools are reviewed in Canter's native catalog.
	if v.Catalog.Slug == "deepwiki" && secret.Endpoint == "https://mcp.deepwiki.com/mcp" {
		for i := range secret.Discovery.Tools {
			switch secret.Discovery.Tools[i].RemoteName {
			case "read_wiki_structure", "read_wiki_contents", "ask_question":
				secret.Discovery.Tools[i].Impact = "read"
			}
		}
	}
	return h.savePluginSecret(ctx, workspace, account, v, secret, "review", "")
}

func (h *HTTPServer) workspacePlugins(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	account := p.Account.ID
	ctx := r.Context()
	if err := h.service.Store.RequireWorkspaceWriter(ctx, workspace, account); err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(parts) == 0 && r.Method == "GET" {
		rows, err := h.service.Store.pool.Query(ctx, `SELECT `+pluginConnectionColumns+` FROM plugin_connections WHERE workspace_id=$1 AND account_id=$2 ORDER BY created_at,id`, workspace, account)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer rows.Close()
		out := []pluginConnection{}
		for rows.Next() {
			v, e := scanPluginConnection(rows)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			out = append(out, v)
		}
		if err = rows.Err(); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"connections": out})
		return
	}
	if len(parts) == 0 && r.Method == "POST" {
		var input struct {
			Slug     string `json:"slug"`
			Name     string `json:"name"`
			Endpoint string `json:"endpoint"`
		}
		if !decodeLimit(w, r, &input, 2048) {
			return
		}
		var entry *pluginCatalogEntry
		if input.Endpoint != "" {
			u, _, err := extensionURL(input.Endpoint, h.config.Extensions)
			if err != nil || u.RawQuery != "" || !artifactSafeText(input.Name, 100) {
				writeError(w, 400, fmt.Errorf("enter a name and public HTTPS server URL without credentials or query parameters"))
				return
			}
			digest := sha256.Sum256([]byte(u.String()))
			entry = &pluginCatalogEntry{Slug: fmt.Sprintf("custom-%x", digest[:12]), Name: input.Name, Description: "Your custom connection.", Summary: u.Hostname(), Source: u.String(), Homepage: pluginOrigin(u.String()), Revision: "custom", Servers: []pluginServer{{Name: "custom", URL: u.String()}}, Inputs: []pluginInput{}}
		} else {
			catalog, err := currentPluginMarketplace(ctx)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			for i := range catalog.Plugins {
				if catalog.Plugins[i].Slug == input.Slug {
					entry = &catalog.Plugins[i]
					break
				}
			}
		}
		if entry == nil {
			writeStoreError(w, ErrNotFound)
			return
		}
		id, err := newID("plugin_")
		if err != nil {
			writeStoreError(w, err)
			return
		}
		tx, err := h.service.Store.pool.Begin(ctx)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer tx.Rollback(ctx)
		if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
			writeStoreError(w, err)
			return
		}
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,92))`, workspace+":"+account); err != nil {
			writeStoreError(w, err)
			return
		}
		var count int
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(bool_or(slug=$3),false) FROM plugin_connections WHERE workspace_id=$1 AND account_id=$2`, workspace, account, entry.Slug).Scan(&count, &exists); err != nil {
			writeStoreError(w, err)
			return
		}
		if count >= 100 && !exists {
			writeError(w, 409, fmt.Errorf("up to 100 personal plugin connections are supported"))
			return
		}
		v, err := scanPluginConnection(tx.QueryRow(ctx, `INSERT INTO plugin_connections(id,workspace_id,account_id,slug,catalog) VALUES($1,$2,$3,$4,$5) ON CONFLICT(workspace_id,account_id,slug) DO UPDATE SET slug=EXCLUDED.slug RETURNING `+pluginConnectionColumns, id, workspace, account, entry.Slug, entry))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if err = tx.Commit(ctx); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	if len(parts) < 1 {
		writeError(w, 405, fmt.Errorf("method not allowed"))
		return
	}
	v, err := h.getPluginConnection(ctx, workspace, account, parts[0])
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(parts) == 1 && r.Method == "GET" {
		secret, err := h.readPluginSecret(ctx, h.service.Store.pool, workspace, account, v.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"connection": v, "tools": secret.Discovery.Tools})
		return
	}
	var input struct {
		Revision int               `json:"revision"`
		Enabled  bool              `json:"enabled"`
		Server   string            `json:"server"`
		Values   map[string]string `json:"values"`
		Token    string            `json:"token"`
		Tools    []string          `json:"tools"`
		ClientID string            `json:"clientId"`
	}
	if !decodeLimit(w, r, &input, 32<<10) {
		return
	}
	if input.Revision != v.Revision {
		writeStoreError(w, ErrConflict)
		return
	}
	if len(parts) == 1 && (r.Method == "PATCH" || r.Method == "DELETE") {
		query := `UPDATE plugin_connections SET enabled=$5,revision=revision+1,updated_at=now() WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND revision=$4`
		arguments := []any{v.ID, workspace, account, v.Revision}
		if r.Method == "DELETE" {
			query = `UPDATE plugin_connections SET enabled=false,ciphertext=NULL,key_id='',state='installed',failure='',revision=revision+1,updated_at=now() WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND revision=$4`
		} else {
			arguments = append(arguments, input.Enabled)
		}
		tag, err := h.service.Store.pool.Exec(ctx, query, arguments...)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if tag.RowsAffected() != 1 {
			writeStoreError(w, ErrConflict)
			return
		}
		v, err = h.getPluginConnection(ctx, workspace, account, v.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	if len(parts) != 2 || r.Method != "POST" || !v.Enabled {
		writeStoreError(w, ErrForbidden)
		return
	}
	switch parts[1] {
	case "configure":
		var secret pluginSecret
		secret, err = resolvePluginServer(v.Catalog, input.Server, input.Values, input.Token, h.config.Extensions)
		if err == nil {
			// Invalidate existing grants before performing network work. A failed
			// reconnect never silently retains an earlier "ready" connection.
			v, err = h.savePluginSecret(ctx, workspace, account, v, secret, "checking", "")
			if err == nil {
				v, err = h.discoverPlugin(ctx, workspace, account, v, secret)
			}
		}
	case "retry":
		var secret pluginSecret
		secret, err = h.readPluginSecret(ctx, h.service.Store.pool, workspace, account, v.ID)
		if err == nil && secret.Endpoint == "" {
			err = fmt.Errorf("complete setup first")
		}
		if err == nil {
			v, err = h.discoverPlugin(ctx, workspace, account, v, secret)
		}
	case "credential":
		if len(input.Token) < 8 || len(input.Token) > 4096 || strings.IndexFunc(input.Token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
			writeError(w, 400, fmt.Errorf("enter a valid API token"))
			return
		}
		var secret pluginSecret
		secret, err = h.readPluginSecret(ctx, h.service.Store.pool, workspace, account, v.ID)
		if err == nil && secret.Endpoint == "" {
			err = fmt.Errorf("complete setup first")
		}
		if err == nil {
			if secret.Headers == nil {
				secret.Headers = map[string]string{}
			}
			secret.Headers["Authorization"] = "Bearer " + input.Token
			secret.OAuth = nil
			v, err = h.savePluginSecret(ctx, workspace, account, v, secret, "checking", "")
			if err == nil {
				v, err = h.discoverPlugin(ctx, workspace, account, v, secret)
			}
		}
	case "authorize":
		h.authorizePlugin(w, r, workspace, account, v, input.ClientID)
		return
	case "activate":
		v, err = h.activatePlugin(ctx, workspace, account, v, input.Tools)
	default:
		err = ErrNotFound
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (h *HTTPServer) activatePlugin(ctx context.Context, workspace, account string, v pluginConnection, selected []string) (pluginConnection, error) {
	if v.State != "review" || len(selected) == 0 || len(selected) > 8 {
		return v, fmt.Errorf("select one to eight tools")
	}
	secret, err := h.readPluginSecret(ctx, h.service.Store.pool, workspace, account, v.ID)
	if err != nil {
		return v, err
	}
	tools := []WrenExtensionTool{}
	seen := map[string]bool{}
	for _, name := range selected {
		if seen[name] {
			return v, fmt.Errorf("duplicate tool")
		}
		seen[name] = true
		found := false
		for _, tool := range secret.Discovery.Tools {
			if tool.Name == name {
				tools = append(tools, tool)
				found = true
				break
			}
		}
		if !found {
			return v, fmt.Errorf("refresh the provider's available tools")
		}
	}
	// Versioned, immutable manifests keep in-flight review arguments stable.
	extensionID := "extension_" + v.ID + "_" + strconv.Itoa(v.Revision)
	version := 0
	if current, e := h.service.Store.WrenExtension(ctx, workspace, account, extensionID, 0); e == nil {
		version = current.Version
	} else if !errors.Is(e, ErrNotFound) {
		return v, e
	}
	manifest := WrenExtensionManifest{Name: v.Catalog.Name, Description: operatorExcerpt(v.Catalog.Description, 1500), Origin: secret.Discovery.Origin, Auth: "bearer", Tools: tools, Events: []string{}}
	extension, err := h.service.Store.saveWrenExtension(ctx, workspace, account, WrenExtensionInput{ID: extensionID, ExpectedVersion: version, Manifest: manifest}, h.config.Extensions)
	if err != nil {
		return v, err
	}
	tag, err := h.service.Store.pool.Exec(ctx, `UPDATE plugin_connections SET extension_id=$5,state='ready',failure='',revision=revision+1,updated_at=now() WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND revision=$4 AND enabled`, v.ID, workspace, account, v.Revision, extension.ID)
	if err != nil {
		return v, err
	}
	if tag.RowsAffected() != 1 {
		return v, ErrConflict
	}
	return h.getPluginConnection(ctx, workspace, account, v.ID)
}

// Materialize the existing audited execution scope only for a human's private
// request. Parent state is checked again under lock at the execution boundary.
func (h *HTTPServer) ensurePersonalPlugins(ctx context.Context, c Conversation, scope WrenKnowledgeScope, human bool) error {
	if !human || scope.Scope == "room" {
		return nil
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = extensionScopeAccess(ctx, tx, c.WorkspaceID, c.AccountID, scope, true); err != nil {
		return err
	}
	var wren any
	if scope.WrenID != "" {
		wren = scope.WrenID
	}
	_, err = tx.Exec(ctx, `INSERT INTO wren_extension_installations(id,workspace_id,conversation_id,scope_kind,owner_id,wren_id,extension_id,version,enabled,allow_write,credential_mode,plugin_connection_id)
	SELECT 'extinstall_'||md5(p.id||':'||$2),p.workspace_id,$2,$3,p.account_id,$4,p.extension_id,e.current_version,true,true,'personal',p.id
	FROM plugin_connections p JOIN wren_extensions e ON e.id=p.extension_id WHERE p.workspace_id=$1 AND p.account_id=$5 AND p.enabled AND p.state='ready'
	ON CONFLICT(plugin_connection_id,conversation_id) WHERE plugin_connection_id IS NOT NULL
	DO UPDATE SET extension_id=EXCLUDED.extension_id,version=EXCLUDED.version,revision=wren_extension_installations.revision+CASE WHEN wren_extension_installations.extension_id<>EXCLUDED.extension_id OR wren_extension_installations.version<>EXCLUDED.version THEN 1 ELSE 0 END`,
		c.WorkspaceID, c.ID, scope.Scope, wren, c.AccountID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (h *HTTPServer) pluginCredentialsTx(ctx context.Context, tx pgx.Tx, v WrenExtensionInstallation, account string) (pluginSecret, error) {
	var state string
	var enabled bool
	var revision int
	err := tx.QueryRow(ctx, `SELECT state,enabled,revision FROM plugin_connections WHERE id=$1 AND workspace_id=$2 AND account_id=$3 FOR UPDATE`, v.PluginConnectionID, v.WorkspaceID, account).Scan(&state, &enabled, &revision)
	if err != nil || !enabled || state != "ready" {
		return pluginSecret{}, ErrForbidden
	}
	secret, err := h.readPluginSecret(ctx, tx, v.WorkspaceID, account, v.PluginConnectionID)
	if err != nil {
		return secret, err
	}
	secret.Revision = revision
	if secret.OAuth != nil && time.Now().After(secret.OAuth.ExpiresAt.Add(-time.Minute)) {
		if err = h.refreshPluginOAuth(ctx, &secret); err != nil {
			if errors.Is(err, errAppReconnect) {
				if _, updateErr := tx.Exec(ctx, `UPDATE plugin_connections SET state='needs_auth',failure='Sign in again to continue.',revision=revision+1,updated_at=now() WHERE id=$1`, v.PluginConnectionID); updateErr != nil {
					return secret, updateErr
				}
			}
			return secret, err
		}
		raw, _ := json.Marshal(secret)
		defer clear(raw)
		key, sealed, e := h.config.Secrets.seal(raw, pluginSecretBinding(v.WorkspaceID, account, v.PluginConnectionID))
		if e != nil {
			return secret, e
		}
		if _, err = tx.Exec(ctx, `UPDATE plugin_connections SET key_id=$2,ciphertext=$3 WHERE id=$1`, v.PluginConnectionID, key, sealed); err != nil {
			return secret, err
		}
	}
	for _, tool := range v.Extension.Manifest.Tools {
		if tool.Endpoint != secret.Endpoint {
			return secret, ErrForbidden
		}
	}
	return secret, nil
}

func pluginOrigin(endpoint string) string {
	u, _ := url.Parse(endpoint)
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
